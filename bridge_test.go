package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"bridge/config"
	"bridge/installer"
	"bridge/printer"
	"bridge/server"
	"bridge/sync"
)

// buildMockPrinter compiles a lightweight executable to simulate SumatraPDF.
func buildMockPrinter(t *testing.T, dir string) string {
	mockSrc := filepath.Join(dir, "mock_printer.go")
	mockExe := filepath.Join(dir, "mock_printer.exe")

	srcCode := `package main

import (
	"os"
	"strings"
	"time"
)

func main() {
	for _, arg := range os.Args {
		if strings.Contains(arg, "hang") {
			time.Sleep(10 * time.Second)
			os.Exit(0)
		}
	}
	// Normal fast exit
	os.Exit(0)
}
`
	if err := os.WriteFile(mockSrc, []byte(srcCode), 0644); err != nil {
		t.Fatalf("Failed to write mock printer src: %v", err)
	}

	cmd := exec.Command("go", "build", "-o", mockExe, mockSrc)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Failed to compile mock printer: %v\nOutput: %s", err, string(out))
	}

	return mockExe
}

func waitForCondition(timeout time.Duration, interval time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(interval)
	}
	return false
}

func TestTwoWaySyncAndLoopPrevention(t *testing.T) {
	tempRoot := t.TempDir()

	d1Shared := filepath.Join(tempRoot, "d1_shared")
	d1State := filepath.Join(tempRoot, "d1_state.json")
	d2Shared := filepath.Join(tempRoot, "d2_shared")
	d2State := filepath.Join(tempRoot, "d2_state.json")

	_ = os.MkdirAll(d1Shared, 0755)
	_ = os.MkdirAll(d2Shared, 0755)

	token := "secret123"

	cfg1 := &config.Config{
		Role:       "desktop1",
		ListenPort: 18765,
		PeerURL:    "http://127.0.0.1:18766",
		AuthToken:  token,
		SharedDir:  d1Shared,
		StateFile:  d1State,
	}

	cfg2 := &config.Config{
		Role:       "desktop2",
		ListenPort: 18766,
		PeerURL:    "http://127.0.0.1:18765",
		AuthToken:  token,
		SharedDir:  d2Shared,
		StateFile:  d2State,
	}

	sm1, err := sync.NewStateManager(d1State)
	if err != nil {
		t.Fatalf("Failed to init sm1: %v", err)
	}
	sm2, err := sync.NewStateManager(d2State)
	if err != nil {
		t.Fatalf("Failed to init sm2: %v", err)
	}

	srv1 := server.NewServer(cfg1, sm1, nil)
	srv2 := server.NewServer(cfg2, sm2, nil)

	go func() { _ = srv1.Start() }()
	defer srv1.Stop()

	go func() { _ = srv2.Start() }()
	defer srv2.Stop()

	se1 := sync.NewSyncEngine(d1Shared, cfg1.PeerURL, token, sm1)
	if err := se1.Start(); err != nil {
		t.Fatalf("Failed to start se1: %v", err)
	}
	defer se1.Stop()

	se2 := sync.NewSyncEngine(d2Shared, cfg2.PeerURL, token, sm2)
	if err := se2.Start(); err != nil {
		t.Fatalf("Failed to start se2: %v", err)
	}
	defer se2.Stop()

	// Wait for servers to be live
	time.Sleep(500 * time.Millisecond)

	// Step 1: Create a file on Desktop 1 -> should sync to Desktop 2
	t.Log("Testing Desktop 1 -> Desktop 2 file sync...")
	file1Name := "doc_from_d1.txt"
	file1Content := []byte("Hello from Desktop 1! Timestamp: " + time.Now().String())
	if err := os.WriteFile(filepath.Join(d1Shared, file1Name), file1Content, 0644); err != nil {
		t.Fatalf("Failed to write file1: %v", err)
	}

	targetOnD2 := filepath.Join(d2Shared, file1Name)
	synced := waitForCondition(8*time.Second, 200*time.Millisecond, func() bool {
		data, err := os.ReadFile(targetOnD2)
		return err == nil && bytes.Equal(data, file1Content)
	})
	if !synced {
		t.Fatalf("File did not sync from Desktop 1 to Desktop 2 within timeout")
	}

	// Step 2: Loop Prevention Verification
	// Verify that Desktop 2's StateManager has marked the file as synced with the same hash
	hashD1, _, _ := sync.ComputeHash(filepath.Join(d1Shared, file1Name))
	if !sm2.IsSynced(file1Name, hashD1) {
		t.Fatalf("Desktop 2 did not mark file %s as synced in state.json", file1Name)
	}

	// Step 3: Create a file on Desktop 2 -> should sync to Desktop 1
	t.Log("Testing Desktop 2 -> Desktop 1 file sync...")
	file2Name := "doc_from_d2.txt"
	file2Content := []byte("Greetings from Desktop 2! Counterpart sync.")
	if err := os.WriteFile(filepath.Join(d2Shared, file2Name), file2Content, 0644); err != nil {
		t.Fatalf("Failed to write file2: %v", err)
	}

	targetOnD1 := filepath.Join(d1Shared, file2Name)
	syncedBack := waitForCondition(8*time.Second, 200*time.Millisecond, func() bool {
		data, err := os.ReadFile(targetOnD1)
		return err == nil && bytes.Equal(data, file2Content)
	})
	if !syncedBack {
		t.Fatalf("File did not sync from Desktop 2 to Desktop 1 within timeout")
	}

	// Step 4: Health Check Endpoint verification
	resp, err := http.Get("http://127.0.0.1:18765/api/health")
	if err != nil {
		t.Fatalf("Health check request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Health check returned status %d, expected 200", resp.StatusCode)
	}
}

func TestSilentPrintQueueExecution(t *testing.T) {
	tempRoot := t.TempDir()
	mockExe := buildMockPrinter(t, tempRoot)

	queueDir := filepath.Join(tempRoot, "PrintQueue")
	failedDir := filepath.Join(tempRoot, "PrintFailed")

	ps := printer.NewPrintService(mockExe, 5, queueDir, failedDir)
	if err := ps.Start(); err != nil {
		t.Fatalf("Failed to start PrintService: %v", err)
	}
	defer ps.Stop()

	// Stage and enqueue a print job
	jobContent := []byte("Test invoice payload")
	job, err := ps.SaveIncomingJob("invoice.pdf", bytes.NewReader(jobContent))
	if err != nil {
		t.Fatalf("Failed to save incoming job: %v", err)
	}

	// Job should be processed by the worker and cleaned up upon success
	processed := waitForCondition(5*time.Second, 100*time.Millisecond, func() bool {
		_, err := os.Stat(job.FilePath)
		return os.IsNotExist(err) && ps.QueueDepth() == 0
	})

	if !processed {
		t.Fatalf("Print job %s was not processed and cleaned up within timeout", job.ID)
	}

	// Verify nothing was moved to PrintFailed
	failedEntries, _ := os.ReadDir(failedDir)
	if len(failedEntries) != 0 {
		t.Fatalf("Expected 0 failed jobs, found %d", len(failedEntries))
	}
}

func TestPrintTimeoutWatchdog(t *testing.T) {
	tempRoot := t.TempDir()
	mockExe := buildMockPrinter(t, tempRoot)

	queueDir := filepath.Join(tempRoot, "PrintQueue")
	failedDir := filepath.Join(tempRoot, "PrintFailed")

	// Set timeout to 1 second
	ps := printer.NewPrintService(mockExe, 1, queueDir, failedDir)
	if err := ps.Start(); err != nil {
		t.Fatalf("Failed to start PrintService: %v", err)
	}
	defer ps.Stop()

	// Enqueue a hung job (name contains "hang", triggering 10s sleep in mock)
	t.Log("Submitting hung print job...")
	hungJob, err := ps.SaveIncomingJob("hang_document.pdf", strings.NewReader("hang data"))
	if err != nil {
		t.Fatalf("Failed to enqueue hung job: %v", err)
	}

	// Watchdog should kill it after ~1s and move to PrintFailed
	hungHandled := waitForCondition(5*time.Second, 100*time.Millisecond, func() bool {
		failedPath := filepath.Join(failedDir, hungJob.ID)
		_, err := os.Stat(failedPath)
		return err == nil
	})

	if !hungHandled {
		t.Fatalf("Hung print job was not killed and moved to PrintFailed within timeout")
	}

	// Ensure temp file was removed from PrintQueue
	if _, err := os.Stat(hungJob.FilePath); !os.IsNotExist(err) {
		t.Fatalf("Expected hung job to be moved out of PrintQueue")
	}

	// Immediately submit a normal print job to verify worker is alive and unblocked
	t.Log("Submitting normal print job to verify worker recovery...")
	normalJob, err := ps.SaveIncomingJob("normal_doc.pdf", strings.NewReader("normal print content"))
	if err != nil {
		t.Fatalf("Failed to enqueue normal job: %v", err)
	}

	normalHandled := waitForCondition(5*time.Second, 100*time.Millisecond, func() bool {
		_, err := os.Stat(normalJob.FilePath)
		return os.IsNotExist(err) && ps.QueueDepth() == 0
	})

	if !normalHandled {
		t.Fatalf("Normal job was blocked by previous hung job or did not finish cleanly")
	}
}

func TestPrintWatcherIntegration(t *testing.T) {
	tempRoot := t.TempDir()
	mockExe := buildMockPrinter(t, tempRoot)

	printDir := filepath.Join(tempRoot, "Print")
	sentDir := filepath.Join(printDir, "Sent")
	queueDir := filepath.Join(tempRoot, "PrintQueue")
	failedDir := filepath.Join(tempRoot, "PrintFailed")

	ps := printer.NewPrintService(mockExe, 5, queueDir, failedDir)
	if err := ps.Start(); err != nil {
		t.Fatalf("Failed to start PrintService: %v", err)
	}
	defer ps.Stop()

	cfg := &config.Config{
		Role:       "desktop1",
		ListenPort: 18767,
		AuthToken:  "print-secret",
	}
	srv := server.NewServer(cfg, nil, ps)
	go func() { _ = srv.Start() }()
	defer srv.Stop()

	time.Sleep(300 * time.Millisecond)

	pw := printer.NewPrintWatcher(printDir, sentDir, "http://127.0.0.1:18767", "print-secret")
	if err := pw.Start(); err != nil {
		t.Fatalf("Failed to start PrintWatcher: %v", err)
	}
	defer pw.Stop()

	// Drop a file into printDir
	testDoc := filepath.Join(printDir, "report.pdf")
	if err := os.WriteFile(testDoc, []byte("Monthly Report"), 0644); err != nil {
		t.Fatalf("Failed to write report.pdf: %v", err)
	}

	// PrintWatcher should detect, upload to Desktop 1, and move to Sent
	movedToSent := waitForCondition(8*time.Second, 200*time.Millisecond, func() bool {
		sentDoc := filepath.Join(sentDir, "report.pdf")
		_, err := os.Stat(sentDoc)
		return err == nil
	})

	if !movedToSent {
		t.Fatalf("File in print_dir was not moved to Sent after printing")
	}

	// Also wait for the print service to finish processing the job cleanly
	processed := waitForCondition(5*time.Second, 100*time.Millisecond, func() bool {
		return ps.QueueDepth() == 0
	})
	if !processed {
		t.Fatalf("PrintService did not finish processing the queued job")
	}
}

func TestDynamicAutoPairing(t *testing.T) {
	tempRoot := t.TempDir()

	d1Shared := filepath.Join(tempRoot, "d1_shared")
	d1State := filepath.Join(tempRoot, "d1_state.json")
	d2Shared := filepath.Join(tempRoot, "d2_shared")
	d2State := filepath.Join(tempRoot, "d2_state.json")

	_ = os.MkdirAll(d1Shared, 0755)
	_ = os.MkdirAll(d2Shared, 0755)

	token := "pair-token-xyz"

	// Desktop 1 starts with NO PeerURL!
	cfg1 := &config.Config{
		Role:       "desktop1",
		ListenPort: 18768,
		PeerURL:    "",
		AuthToken:  token,
		SharedDir:  d1Shared,
		StateFile:  d1State,
	}

	cfg2 := &config.Config{
		Role:       "desktop2",
		ListenPort: 18769,
		PeerURL:    "http://127.0.0.1:18768",
		AuthToken:  token,
		SharedDir:  d2Shared,
		StateFile:  d2State,
	}

	sm1, _ := sync.NewStateManager(d1State)
	sm2, _ := sync.NewStateManager(d2State)

	srv1 := server.NewServer(cfg1, sm1, nil)
	srv2 := server.NewServer(cfg2, sm2, nil)

	se1 := sync.NewSyncEngine(d1Shared, "", token, sm1)
	srv1.SetPeerUpdater(func(peer config.PeerDevice, isNew bool) {
		se1.UpdatePeerURL(peer.URL)
	})

	se2 := sync.NewSyncEngine(d2Shared, cfg2.PeerURL, token, sm2)

	go func() { _ = srv1.Start() }()
	defer srv1.Stop()

	go func() { _ = srv2.Start() }()
	defer srv2.Stop()

	_ = se1.Start()
	defer se1.Stop()

	_ = se2.Start()
	defer se2.Stop()

	time.Sleep(300 * time.Millisecond)

	// Verify Desktop 1 currently has no PeerURL
	if cfg1.GetPeerURL() != "" {
		t.Fatalf("Expected Desktop 1 PeerURL to be empty initially")
	}

	// Desktop 2 pings Desktop 1 health check with X-Bridge-Peer-URL
	req, _ := http.NewRequest("GET", "http://127.0.0.1:18768/api/health", nil)
	req.Header.Set("X-Bridge-Token", token)
	req.Header.Set("X-Bridge-Peer-URL", "http://127.0.0.1:18769")

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Health check with peer URL header failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Expected 200 OK, got %d", resp.StatusCode)
	}

	// Verify Desktop 1 auto-paired and updated its PeerURL!
	paired := waitForCondition(3*time.Second, 100*time.Millisecond, func() bool {
		return cfg1.GetPeerURL() == "http://127.0.0.1:18769"
	})
	if !paired {
		t.Fatalf("Desktop 1 did not dynamically auto-pair with Desktop 2")
	}

	// Test that sync now works from Desktop 1 -> Desktop 2!
	fileName := "auto_paired_sync.txt"
	content := []byte("Auto pairing test data")
	_ = os.WriteFile(filepath.Join(d1Shared, fileName), content, 0644)

	synced := waitForCondition(6*time.Second, 200*time.Millisecond, func() bool {
		data, err := os.ReadFile(filepath.Join(d2Shared, fileName))
		return err == nil && bytes.Equal(data, content)
	})
	if !synced {
		t.Fatalf("File failed to sync from Desktop 1 to Desktop 2 after auto-pairing")
	}
}

func TestInstallerShortcutCreation(t *testing.T) {
	tempDir := t.TempDir()
	targetPath := filepath.Join(tempDir, "TargetFolder")
	_ = os.MkdirAll(targetPath, 0755)

	lnkPath := filepath.Join(tempDir, "TestShortcut.lnk")

	err := installer.CreateDesktopShortcut(targetPath, lnkPath, "Test Description", "")
	if err != nil {
		t.Fatalf("Failed to create shortcut: %v", err)
	}

	if _, err := os.Stat(lnkPath); os.IsNotExist(err) {
		t.Fatalf("Shortcut file was not created at %s", lnkPath)
	}
}

func TestSymmetricalTwoWayPrinting(t *testing.T) {
	tempRoot := t.TempDir()
	mockExe := buildMockPrinter(t, tempRoot)

	// Node 1 setup
	n1Queue := filepath.Join(tempRoot, "n1_queue")
	n1Failed := filepath.Join(tempRoot, "n1_failed")
	ps1 := printer.NewPrintService(mockExe, 5, n1Queue, n1Failed)
	_ = ps1.Start()
	defer ps1.Stop()

	cfg1 := &config.Config{
		NodeName:   "Office-PC",
		PeerName:   "Laptop",
		ListenPort: 18770,
		PeerURL:    "http://127.0.0.1:18771",
		AuthToken:  "sym-secret",
	}
	srv1 := server.NewServer(cfg1, nil, ps1)
	go func() { _ = srv1.Start() }()
	defer srv1.Stop()

	// Node 2 setup
	n2Queue := filepath.Join(tempRoot, "n2_queue")
	n2Failed := filepath.Join(tempRoot, "n2_failed")
	ps2 := printer.NewPrintService(mockExe, 5, n2Queue, n2Failed)
	_ = ps2.Start()
	defer ps2.Stop()

	cfg2 := &config.Config{
		NodeName:   "Laptop",
		PeerName:   "Office-PC",
		ListenPort: 18771,
		PeerURL:    "http://127.0.0.1:18770",
		AuthToken:  "sym-secret",
	}
	srv2 := server.NewServer(cfg2, nil, ps2)
	go func() { _ = srv2.Start() }()
	defer srv2.Stop()

	time.Sleep(300 * time.Millisecond)

	// Direction 1: Node 1 prints to Node 2
	t.Log("Testing Direction 1: Node 1 prints to Node 2...")
	docFrom1 := filepath.Join(tempRoot, "doc_from_node1.pdf")
	_ = os.WriteFile(docFrom1, []byte("Content from Node 1"), 0644)

	ctx1, cancel1 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel1()
	err := printer.SendDirectPrint(ctx1, cfg1.PeerURL, cfg1.AuthToken, docFrom1)
	if err != nil {
		t.Fatalf("Failed to print from Node 1 to Node 2: %v", err)
	}

	// Verify Node 2 received and processed the job
	processedOn2 := waitForCondition(5*time.Second, 100*time.Millisecond, func() bool {
		return ps2.QueueDepth() == 0
	})
	if !processedOn2 {
		t.Fatalf("Node 2 did not process print job from Node 1")
	}

	// Direction 2: Node 2 prints to Node 1
	t.Log("Testing Direction 2: Node 2 prints to Node 1...")
	docFrom2 := filepath.Join(tempRoot, "doc_from_node2.pdf")
	_ = os.WriteFile(docFrom2, []byte("Content from Node 2"), 0644)

	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	err = printer.SendDirectPrint(ctx2, cfg2.PeerURL, cfg2.AuthToken, docFrom2)
	if err != nil {
		t.Fatalf("Failed to print from Node 2 to Node 1: %v", err)
	}

	// Verify Node 1 received and processed the job
	processedOn1 := waitForCondition(5*time.Second, 100*time.Millisecond, func() bool {
		return ps1.QueueDepth() == 0
	})
	if !processedOn1 {
		t.Fatalf("Node 1 did not process print job from Node 2")
	}
}

func TestLegacyConfigAutoMigration(t *testing.T) {
	tempRoot := t.TempDir()
	legacyConfigPath := filepath.Join(tempRoot, "legacy_config.json")

	legacyJSON := `{
  "role": "desktop1",
  "listen_port": 8765,
  "peer_url": "http://100.1.2.3:8765",
  "auth_token": "legacy-secret",
  "shared_dir": "C:\\Bridge\\Shared",
  "print_dir": "C:\\Bridge\\Print",
  "sumatra_path": "C:\\SumatraPDF.exe",
  "print_timeout_sec": 45
}`
	if err := os.WriteFile(legacyConfigPath, []byte(legacyJSON), 0644); err != nil {
		t.Fatalf("Failed to write legacy config: %v", err)
	}

	cfg, err := config.LoadConfig(legacyConfigPath)
	if err != nil {
		t.Fatalf("Failed to load legacy config: %v", err)
	}

	if cfg.NodeName != "Desktop 1" {
		t.Fatalf("Expected NodeName 'Desktop 1', got %q", cfg.NodeName)
	}
	if cfg.PeerName != "Desktop 2" {
		t.Fatalf("Expected PeerName 'Desktop 2', got %q", cfg.PeerName)
	}
	if cfg.AuthToken != "legacy-secret" {
		t.Fatalf("Expected AuthToken 'legacy-secret', got %q", cfg.AuthToken)
	}
}

func TestSpoolerListenerPDFDetectionAndDispatch(t *testing.T) {
	var dispatchedFile string
	var dispatchedContent []byte
	dispatched := make(chan bool, 1)

	dispatchFunc := func(filePath string) error {
		data, err := os.ReadFile(filePath)
		if err != nil {
			return err
		}
		dispatchedFile = filePath
		dispatchedContent = data
		dispatched <- true
		return nil
	}

	listener := printer.NewSpoolerListener("127.0.0.1:19100", dispatchFunc)
	if err := listener.Start(); err != nil {
		t.Fatalf("Failed to start spooler listener: %v", err)
	}
	defer listener.Stop()

	// Connect as client and send PDF stream
	conn, err := net.Dial("tcp", "127.0.0.1:19100")
	if err != nil {
		t.Fatalf("Failed to dial spooler listener: %v", err)
	}

	pdfPayload := []byte("%PDF-1.4\n1 0 obj\n<< /Type /Catalog >>\nendobj\n%%EOF")
	_, err = conn.Write(pdfPayload)
	if err != nil {
		t.Fatalf("Failed to write PDF payload: %v", err)
	}
	_ = conn.Close() // Spooler closes write / connection on completion

	select {
	case <-dispatched:
		if !strings.HasSuffix(dispatchedFile, ".pdf") {
			t.Errorf("Expected dispatched file to have .pdf extension, got %s", dispatchedFile)
		}
		if !bytes.Equal(dispatchedContent, pdfPayload) {
			t.Errorf("Dispatched content does not match original payload")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Timeout waiting for spooler listener dispatch")
	}
}

func TestSpoolerListenerXPSDetectionAndDispatch(t *testing.T) {
	var dispatchedFile string
	var dispatchedContent []byte
	dispatched := make(chan bool, 1)

	dispatchFunc := func(filePath string) error {
		data, err := os.ReadFile(filePath)
		if err != nil {
			return err
		}
		dispatchedFile = filePath
		dispatchedContent = data
		dispatched <- true
		return nil
	}

	listener := printer.NewSpoolerListener("127.0.0.1:19101", dispatchFunc)
	if err := listener.Start(); err != nil {
		t.Fatalf("Failed to start spooler listener: %v", err)
	}
	defer listener.Stop()

	conn, err := net.Dial("tcp", "127.0.0.1:19101")
	if err != nil {
		t.Fatalf("Failed to dial spooler listener: %v", err)
	}

	xpsPayload := []byte("PK\x03\x04\x14\x00\x08\x00[Content_Types].xml...")
	_, err = conn.Write(xpsPayload)
	if err != nil {
		t.Fatalf("Failed to write XPS payload: %v", err)
	}
	_ = conn.Close()

	select {
	case <-dispatched:
		if !strings.HasSuffix(dispatchedFile, ".xps") {
			t.Errorf("Expected dispatched file to have .xps extension, got %s", dispatchedFile)
		}
		if !bytes.Equal(dispatchedContent, xpsPayload) {
			t.Errorf("Dispatched content does not match original payload")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Timeout waiting for spooler listener dispatch")
	}
}

func TestSpoolerListenerEndToEndWithPrintService(t *testing.T) {
	tempRoot := t.TempDir()
	mockExe := buildMockPrinter(t, tempRoot)

	queueDir := filepath.Join(tempRoot, "queue")
	failedDir := filepath.Join(tempRoot, "failed")

	ps := printer.NewPrintService(mockExe, 5, queueDir, failedDir)
	_ = ps.Start()
	defer ps.Stop()

	cfg := &config.Config{
		NodeName:   "Desktop 1",
		PeerName:   "Desktop 2",
		ListenPort: 18772,
		AuthToken:  "test-spool-token",
	}
	srv := server.NewServer(cfg, nil, ps)
	go func() { _ = srv.Start() }()
	defer srv.Stop()

	time.Sleep(200 * time.Millisecond)

	peerURL := "http://127.0.0.1:18772"
	spooler := printer.NewSpoolerListener("127.0.0.1:19102", func(filePath string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return printer.SendDirectPrint(ctx, peerURL, cfg.AuthToken, filePath)
	})
	if err := spooler.Start(); err != nil {
		t.Fatalf("Failed to start spooler: %v", err)
	}
	defer spooler.Stop()

	completedChan := make(chan struct{}, 1)
	ps.OnJobCompleted = func(job printer.PrintJob) {
		select {
		case completedChan <- struct{}{}:
		default:
		}
	}

	// Connect and send PDF stream over RAW TCP
	conn, err := net.Dial("tcp", "127.0.0.1:19102")
	if err != nil {
		t.Fatalf("Failed to dial spooler: %v", err)
	}
	pdfPayload := []byte("%PDF-1.5\nTest RAW print stream from spooler\n%%EOF")
	_, _ = conn.Write(pdfPayload)
	_ = conn.Close()

	// Wait for PrintService to finish processing the job
	select {
	case <-completedChan:
		// Success!
	case <-time.After(6 * time.Second):
		t.Fatal("Print job was not completed successfully by PrintService within timeout")
	}
}

func TestDynamicShortcutsAndVirtualPrinter(t *testing.T) {
	// Test creating shortcuts and virtual printer for "Desktop 2"
	err := installer.CreateBridgeShortcuts("Desktop 2")
	if err != nil {
		t.Fatalf("CreateBridgeShortcuts failed: %v", err)
	}

	dirs := installer.GetAllDesktopDirs()
	if len(dirs) == 0 {
		t.Fatal("No desktop directories found")
	}
	userDesktop := dirs[0]
	sharedLnk := filepath.Join(userDesktop, "Bridge Shared Folder.lnk")
	printLnk := filepath.Join(userDesktop, "Desktop 2 - Drop to Print.lnk")

	// Shared folder shortcut must exist
	if _, err := os.Stat(sharedLnk); os.IsNotExist(err) {
		t.Errorf("Expected shortcut not found: %s", sharedLnk)
	}

	// Drop to Print shortcut must strictly NOT exist (print folder decommissioned)
	if _, err := os.Stat(printLnk); !os.IsNotExist(err) {
		t.Errorf("Expected Drop to Print shortcut to NOT exist, but found: %s", printLnk)
	}

	// Dynamic rename to "Desktop 1"
	err = installer.CreateBridgeShortcuts("Desktop 1")
	if err != nil {
		t.Fatalf("CreateBridgeShortcuts for Desktop 1 failed: %v", err)
	}

	if _, err := os.Stat(sharedLnk); os.IsNotExist(err) {
		t.Errorf("Expected shared shortcut not found: %s", sharedLnk)
	}

	// Clean up test shortcut
	_ = os.Remove(sharedLnk)
	if cfg, err := config.LoadConfig(installer.TargetConfig); err == nil && cfg.GetPeerName() != "" {
		_ = installer.CreateBridgeShortcuts(cfg.GetPeerName())
	}
}

func TestTargetPhysicalPrinterDetection(t *testing.T) {
	// Verify virtual printer filter identifies virtual vs physical printers
	if !printer.IsVirtualPrinterName("Print on Sulobha Unit 1", "Bridge_TCP_Port") {
		t.Error("Expected 'Print on Sulobha Unit 1' on Bridge_TCP_Port to be identified as virtual")
	}
	if !printer.IsVirtualPrinterName("Microsoft Print to PDF", "PORTPROMPT:") {
		t.Error("Expected 'Microsoft Print to PDF' to be identified as virtual")
	}
	if !printer.IsVirtualPrinterName("AnyDesk Printer", "AD_Port") {
		t.Error("Expected 'AnyDesk Printer' to be identified as virtual")
	}
	if !printer.IsVirtualPrinterName("Fax", "SHRFAX:") {
		t.Error("Expected 'Fax' to be identified as virtual")
	}
	if printer.IsVirtualPrinterName("HP Smart Tank 580-590 series", "USB001") {
		t.Error("Expected 'HP Smart Tank 580-590 series' on USB001 to NOT be identified as virtual")
	}

	if !printer.IsVirtualPrinterName("Microsoft XPS Document Writer", "PORTPROMPT:") {
		t.Error("Expected 'Microsoft XPS Document Writer' to be identified as virtual")
	}
	if !printer.IsVirtualPrinterName("OneNote for Windows 10", "nul:") {
		t.Error("Expected 'OneNote for Windows 10' to be identified as virtual")
	}
	if !printer.IsVirtualPrinterName("CutePDF Writer", "CPW2:") {
		t.Error("Expected 'CutePDF Writer' to be identified as virtual")
	}
	if printer.IsVirtualPrinterName("Epson L3210 Series", "USB002") {
		t.Error("Expected 'Epson L3210 Series' on USB002 to NOT be identified as virtual")
	}
	if printer.IsVirtualPrinterName("Canon G3000 series", "WSD-1234") {
		t.Error("Expected 'Canon G3000 series' on WSD-1234 to NOT be identified as virtual")
	}

	target, err := printer.GetTargetPhysicalPrinter()
	if err == nil {
		t.Logf("Detected physical printer: %s", target)
		if printer.IsVirtualPrinterName(target, "") {
			t.Errorf("Target physical printer %q should not be virtual", target)
		}
	} else {
		t.Logf("No physical printer on this test runner: %v", err)
	}
}

func TestLoopbackEchoPrevention(t *testing.T) {
	tempRoot := t.TempDir()
	mockExe := buildMockPrinter(t, tempRoot)

	queueDir := filepath.Join(tempRoot, "queue")
	failedDir := filepath.Join(tempRoot, "failed")
	ps := printer.NewPrintService(mockExe, 5, queueDir, failedDir)
	_ = ps.Start()
	defer ps.Stop()

	cfg := &config.Config{
		NodeName:   "Sulobha Unit 1",
		PeerName:   "Sulobha Unit 2",
		ListenPort: 18774,
		AuthToken:  "test-echo-token",
	}
	srv := server.NewServer(cfg, nil, ps)
	go func() { _ = srv.Start() }()
	defer srv.Stop()

	time.Sleep(200 * time.Millisecond)

	client := &http.Client{Timeout: 3 * time.Second}

	// 1. Send print job with X-Bridge-Origin matching local node name ("Sulobha Unit 1")
	// MUST be rejected with HTTP 400 Bad Request to prevent echo loop!
	reqLoop, _ := http.NewRequest("POST", "http://127.0.0.1:18774/api/print", strings.NewReader("sample print data"))
	reqLoop.Header.Set("X-Bridge-Token", cfg.AuthToken)
	reqLoop.Header.Set("X-Bridge-FileName", "echo_doc.pdf")
	reqLoop.Header.Set("X-Bridge-Origin", "Sulobha Unit 1")

	respLoop, err := client.Do(reqLoop)
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	defer respLoop.Body.Close()

	if respLoop.StatusCode != http.StatusBadRequest {
		t.Errorf("Expected loopback print job to be rejected with 400 Bad Request, got %d", respLoop.StatusCode)
	}

	// 2. Send print job from valid peer origin ("Sulobha Unit 2")
	// MUST be accepted with HTTP 200 OK
	reqValid, _ := http.NewRequest("POST", "http://127.0.0.1:18774/api/print", strings.NewReader("sample print data"))
	reqValid.Header.Set("X-Bridge-Token", cfg.AuthToken)
	reqValid.Header.Set("X-Bridge-FileName", "peer_doc.pdf")
	reqValid.Header.Set("X-Bridge-Origin", "Sulobha Unit 2")

	respValid, err := client.Do(reqValid)
	if err != nil {
		t.Fatalf("Valid request failed: %v", err)
	}
	defer respValid.Body.Close()

	if respValid.StatusCode != http.StatusOK {
		t.Errorf("Expected valid print job to be accepted with 200 OK, got %d", respValid.StatusCode)
	}
}

func TestTailscaleAutoDiscovery(t *testing.T) {
	peers, err := installer.DiscoverTailscalePeers()
	if err != nil {
		t.Logf("DiscoverTailscalePeers returned: %v (Tailscale may not be active)", err)
		return
	}

	t.Logf("Discovered %d Tailscale peers", len(peers))
	for _, p := range peers {
		t.Logf("Peer: HostName=%s, IP=%s, Online=%v, BridgeName=%s, MyName=%s", p.HostName, p.IP, p.Online, p.BridgeName, p.MyName)
		if !strings.HasPrefix(p.IP, "100.") {
			t.Errorf("Expected peer IP to be a 100.x.x.x Tailscale IP, got %s", p.IP)
		}
	}
}

func TestKillOtherBridgeProcessesDoesNotSuicide(t *testing.T) {
	// Verify that KillOtherBridgeProcesses does not kill the current running test process!
	survived := make(chan bool, 1)
	go func() {
		installer.KillOtherBridgeProcesses()
		survived <- true
	}()

	select {
	case ok := <-survived:
		if !ok {
			t.Fatal("KillOtherBridgeProcesses failed")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("KillOtherBridgeProcesses hung or caused deadlock")
	}
}

func TestGetAllDesktopDirs(t *testing.T) {
	dirs := installer.GetAllDesktopDirs()
	if len(dirs) == 0 {
		t.Fatal("Expected at least one desktop directory, got 0")
	}

	if runtime.GOOS == "windows" {
		foundPublic := false
		for _, d := range dirs {
			if strings.EqualFold(d, `C:\Users\Public\Desktop`) {
				foundPublic = true
			}
		}
		if !foundPublic {
			t.Errorf("Expected C:\\Users\\Public\\Desktop in results, got %v", dirs)
		}
	}
}

func TestPeerHealthBidirectionalNameDiscovery(t *testing.T) {
	cfg := &config.Config{
		NodeName:   "Desktop 2",
		ListenPort: 18773,
		AuthToken:  "test-health-token",
	}
	srv := server.NewServer(cfg, nil, nil)
	go func() { _ = srv.Start() }()
	defer srv.Stop()

	time.Sleep(200 * time.Millisecond)

	healthy, learnedName, _ := checkPeerHealthy("http://127.0.0.1:18773", "test-health-token", "Desktop 1")
	if !healthy {
		t.Fatal("Expected checkPeerHealthy to return true")
	}
	if learnedName != "Desktop 2" {
		t.Fatalf("Expected learned peer name 'Desktop 2', got '%s'", learnedName)
	}
}

func TestMultiDeviceConfigAllocation(t *testing.T) {
	tempRoot := t.TempDir()
	cfgPath := filepath.Join(tempRoot, "multi_config.json")

	cfg := config.DefaultConfig()
	cfg.ConfigPath = cfgPath
	cfg.NodeName = "Unit 1"

	// Add Peer 1 (Unit 2)
	dev1, isNew1 := cfg.AddOrUpdatePeer("Unit 2", "100.64.1.2", "http://100.64.1.2:8765")
	if !isNew1 {
		t.Errorf("Expected dev1 to be new")
	}
	if dev1.SpoolPort != 9101 {
		t.Errorf("Expected dev1 SpoolPort to be 9101, got %d", dev1.SpoolPort)
	}

	// Add Peer 2 (Unit 3)
	dev2, isNew2 := cfg.AddOrUpdatePeer("Unit 3", "100.64.1.3", "http://100.64.1.3:8765")
	if !isNew2 {
		t.Errorf("Expected dev2 to be new")
	}
	if dev2.SpoolPort != 9102 {
		t.Errorf("Expected dev2 SpoolPort to be 9102, got %d", dev2.SpoolPort)
	}

	// Add Peer 3 (Unit 4)
	dev3, isNew3 := cfg.AddOrUpdatePeer("Unit 4", "100.64.1.4", "http://100.64.1.4:8765")
	if !isNew3 {
		t.Errorf("Expected dev3 to be new")
	}
	if dev3.SpoolPort != 9103 {
		t.Errorf("Expected dev3 SpoolPort to be 9103, got %d", dev3.SpoolPort)
	}

	// Re-add existing Peer 2 with updated name
	dev2Up, isNew2Up := cfg.AddOrUpdatePeer("Unit 3 Pro", "100.64.1.3", "")
	if isNew2Up {
		t.Errorf("Expected dev2Up to NOT be new")
	}
	if dev2Up.SpoolPort != 9102 {
		t.Errorf("Expected dev2Up to retain SpoolPort 9102, got %d", dev2Up.SpoolPort)
	}
	if dev2Up.Name != "Unit 3 Pro" {
		t.Errorf("Expected updated name 'Unit 3 Pro', got %s", dev2Up.Name)
	}

	// Lookup by port
	found := cfg.FindPeerBySpoolPort(9102)
	if found == nil || found.Name != "Unit 3 Pro" {
		t.Errorf("Expected to find peer with port 9102, got %v", found)
	}

	// Save and reload
	_ = config.SaveConfig(cfgPath, cfg)
	loaded, err := config.LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("Failed to reload multi-device config: %v", err)
	}
	if len(loaded.GetPeers()) != 3 {
		t.Fatalf("Expected 3 peers, got %d", len(loaded.GetPeers()))
	}
}

func TestMultiDeviceMeshFileSharing(t *testing.T) {
	tempRoot := t.TempDir()

	// 3 Nodes: Unit 1, Unit 2, Unit 3
	d1Shared := filepath.Join(tempRoot, "unit1_shared")
	d1State := filepath.Join(tempRoot, "unit1_state.json")
	d2Shared := filepath.Join(tempRoot, "unit2_shared")
	d2State := filepath.Join(tempRoot, "unit2_state.json")
	d3Shared := filepath.Join(tempRoot, "unit3_shared")
	d3State := filepath.Join(tempRoot, "unit3_state.json")

	_ = os.MkdirAll(d1Shared, 0755)
	_ = os.MkdirAll(d2Shared, 0755)
	_ = os.MkdirAll(d3Shared, 0755)

	token := "mesh-token-123"

	cfg1 := &config.Config{
		NodeName:   "Unit 1",
		ListenPort: 18801,
		AuthToken:  token,
		SharedDir:  d1Shared,
		StateFile:  d1State,
	}
	cfg1.AddOrUpdatePeer("Unit 2", "127.0.0.1", "http://127.0.0.1:18802")
	cfg1.AddOrUpdatePeer("Unit 3", "127.0.0.1", "http://127.0.0.1:18803")

	cfg2 := &config.Config{
		NodeName:   "Unit 2",
		ListenPort: 18802,
		AuthToken:  token,
		SharedDir:  d2Shared,
		StateFile:  d2State,
	}
	cfg2.AddOrUpdatePeer("Unit 1", "127.0.0.1", "http://127.0.0.1:18801")
	cfg2.AddOrUpdatePeer("Unit 3", "127.0.0.1", "http://127.0.0.1:18803")

	cfg3 := &config.Config{
		NodeName:   "Unit 3",
		ListenPort: 18803,
		AuthToken:  token,
		SharedDir:  d3Shared,
		StateFile:  d3State,
	}
	cfg3.AddOrUpdatePeer("Unit 1", "127.0.0.1", "http://127.0.0.1:18801")
	cfg3.AddOrUpdatePeer("Unit 2", "127.0.0.1", "http://127.0.0.1:18802")

	sm1, _ := sync.NewStateManager(d1State)
	sm2, _ := sync.NewStateManager(d2State)
	sm3, _ := sync.NewStateManager(d3State)

	srv1 := server.NewServer(cfg1, sm1, nil)
	srv2 := server.NewServer(cfg2, sm2, nil)
	srv3 := server.NewServer(cfg3, sm3, nil)

	go func() { _ = srv1.Start() }()
	defer srv1.Stop()
	go func() { _ = srv2.Start() }()
	defer srv2.Stop()
	go func() { _ = srv3.Start() }()
	defer srv3.Stop()

	se1 := sync.NewSyncEngine(d1Shared, "", token, sm1)
	se1.UpdatePeerURLs(cfg1.GetPeerURLs())
	_ = se1.Start()
	defer se1.Stop()

	se2 := sync.NewSyncEngine(d2Shared, "", token, sm2)
	se2.UpdatePeerURLs(cfg2.GetPeerURLs())
	_ = se2.Start()
	defer se2.Stop()

	se3 := sync.NewSyncEngine(d3Shared, "", token, sm3)
	se3.UpdatePeerURLs(cfg3.GetPeerURLs())
	_ = se3.Start()
	defer se3.Stop()

	time.Sleep(400 * time.Millisecond)

	// Step 1: Unit 1 creates a global file -> should sync to Unit 2 AND Unit 3!
	globalFile := "global_document.txt"
	content1 := []byte("Initial mesh document created by Unit 1")
	_ = os.WriteFile(filepath.Join(d1Shared, globalFile), content1, 0644)

	syncedD2 := waitForCondition(8*time.Second, 200*time.Millisecond, func() bool {
		data, err := os.ReadFile(filepath.Join(d2Shared, globalFile))
		return err == nil && bytes.Equal(data, content1)
	})
	if !syncedD2 {
		t.Fatalf("File did not sync from Unit 1 to Unit 2")
	}

	syncedD3 := waitForCondition(8*time.Second, 200*time.Millisecond, func() bool {
		data, err := os.ReadFile(filepath.Join(d3Shared, globalFile))
		return err == nil && bytes.Equal(data, content1)
	})
	if !syncedD3 {
		t.Fatalf("File did not sync from Unit 1 to Unit 3")
	}

	// Step 2: Unit 3 edits the global file -> should update on Unit 1 AND Unit 2!
	content2 := []byte("Updated mesh document edited by Unit 3!")
	_ = os.WriteFile(filepath.Join(d3Shared, globalFile), content2, 0644)

	updatedD1 := waitForCondition(8*time.Second, 200*time.Millisecond, func() bool {
		data, err := os.ReadFile(filepath.Join(d1Shared, globalFile))
		return err == nil && bytes.Equal(data, content2)
	})
	if !updatedD1 {
		t.Fatalf("Updated file from Unit 3 did not replicate to Unit 1")
	}

	updatedD2 := waitForCondition(8*time.Second, 200*time.Millisecond, func() bool {
		data, err := os.ReadFile(filepath.Join(d2Shared, globalFile))
		return err == nil && bytes.Equal(data, content2)
	})
	if !updatedD2 {
		t.Fatalf("Updated file from Unit 3 did not replicate to Unit 2")
	}
}

func TestMultiDevicePrintingRouting(t *testing.T) {
	tempRoot := t.TempDir()
	mockExe := buildMockPrinter(t, tempRoot)

	// Unit 2 Print Service
	q2 := filepath.Join(tempRoot, "q2")
	f2 := filepath.Join(tempRoot, "f2")
	ps2 := printer.NewPrintService(mockExe, 5, q2, f2)
	_ = ps2.Start()
	defer ps2.Stop()

	// Unit 3 Print Service
	q3 := filepath.Join(tempRoot, "q3")
	f3 := filepath.Join(tempRoot, "f3")
	ps3 := printer.NewPrintService(mockExe, 5, q3, f3)
	_ = ps3.Start()
	defer ps3.Stop()

	token := "multi-print-token"

	cfg1 := &config.Config{
		NodeName:   "Unit 1",
		ListenPort: 18811,
		AuthToken:  token,
	}
	cfg1.AddOrUpdatePeer("Unit 2", "127.0.0.1", "http://127.0.0.1:18812")
	cfg1.AddOrUpdatePeer("Unit 3", "127.0.0.1", "http://127.0.0.1:18813")

	cfg2 := &config.Config{
		NodeName:   "Unit 2",
		ListenPort: 18812,
		AuthToken:  token,
	}
	cfg3 := &config.Config{
		NodeName:   "Unit 3",
		ListenPort: 18813,
		AuthToken:  token,
	}

	srv1 := server.NewServer(cfg1, nil, nil)
	srv2 := server.NewServer(cfg2, nil, ps2)
	srv3 := server.NewServer(cfg3, nil, ps3)

	go func() { _ = srv1.Start() }()
	defer srv1.Stop()
	go func() { _ = srv2.Start() }()
	defer srv2.Stop()
	go func() { _ = srv3.Start() }()
	defer srv3.Stop()

	time.Sleep(300 * time.Millisecond)

	// Direction A: Unit 1 prints specifically to Unit 2
	docFor2 := filepath.Join(tempRoot, "invoice_unit2.pdf")
	_ = os.WriteFile(docFor2, []byte("Invoice for Unit 2"), 0644)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	p2 := cfg1.FindPeer("Unit 2")
	if p2 == nil {
		t.Fatal("Unit 2 not found in cfg1")
	}
	err := printer.SendDirectPrint(ctx, p2.URL, token, docFor2,
		"X-Bridge-Node-Name", cfg1.GetNodeName(),
		"X-Bridge-Origin", cfg1.GetNodeName(),
	)
	if err != nil {
		t.Fatalf("Failed to print to Unit 2: %v", err)
	}

	// Verify Unit 2 processed the job
	waitForCondition(5*time.Second, 100*time.Millisecond, func() bool {
		return ps2.QueueDepth() == 0
	})

	// Direction B: Unit 1 prints specifically to Unit 3
	docFor3 := filepath.Join(tempRoot, "blueprint_unit3.pdf")
	_ = os.WriteFile(docFor3, []byte("Blueprint for Unit 3"), 0644)

	p3 := cfg1.FindPeer("Unit 3")
	if p3 == nil {
		t.Fatal("Unit 3 not found in cfg1")
	}
	err = printer.SendDirectPrint(ctx, p3.URL, token, docFor3,
		"X-Bridge-Node-Name", cfg1.GetNodeName(),
		"X-Bridge-Origin", cfg1.GetNodeName(),
	)
	if err != nil {
		t.Fatalf("Failed to print to Unit 3: %v", err)
	}

	// Verify Unit 3 processed the job
	waitForCondition(5*time.Second, 100*time.Millisecond, func() bool {
		return ps3.QueueDepth() == 0
	})
}

func TestMultiSpoolerListenerRouting(t *testing.T) {
	var routedPeer string
	var routedFile string
	dispatched := make(chan bool, 2)

	multiSpooler := printer.NewMultiSpoolerListener(func(peer config.PeerDevice, filePath string) error {
		routedPeer = peer.Name
		routedFile = filePath
		dispatched <- true
		return nil
	})

	p2 := config.PeerDevice{Name: "Unit 2", SpoolPort: 19130}
	p3 := config.PeerDevice{Name: "Unit 3", SpoolPort: 19131}

	if err := multiSpooler.EnsurePeerListener(p2); err != nil {
		t.Fatalf("Failed to start listener for Unit 2 on 19130: %v", err)
	}
	if err := multiSpooler.EnsurePeerListener(p3); err != nil {
		t.Fatalf("Failed to start listener for Unit 3 on 19131: %v", err)
	}
	defer multiSpooler.StopAll()

	// 1. Send print stream to port 19130 -> must route to "Unit 2"!
	conn2, err := net.Dial("tcp", "127.0.0.1:19130")
	if err != nil {
		t.Fatalf("Failed to connect to port 19130: %v", err)
	}
	_, _ = conn2.Write([]byte("%PDF-1.4\nJob for Unit 2\n%%EOF"))
	_ = conn2.Close()

	select {
	case <-dispatched:
		if routedPeer != "Unit 2" {
			t.Errorf("Expected routedPeer 'Unit 2', got %s", routedPeer)
		}
		if routedFile == "" {
			t.Errorf("Expected routedFile to be non-empty")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Timeout waiting for Unit 2 dispatch")
	}

	// 2. Send print stream to port 19131 -> must route to "Unit 3"!
	conn3, err := net.Dial("tcp", "127.0.0.1:19131")
	if err != nil {
		t.Fatalf("Failed to connect to port 19131: %v", err)
	}
	_, _ = conn3.Write([]byte("%PDF-1.4\nJob for Unit 3\n%%EOF"))
	_ = conn3.Close()

	select {
	case <-dispatched:
		if routedPeer != "Unit 3" {
			t.Errorf("Expected routedPeer 'Unit 3', got %s", routedPeer)
		}
		if routedFile == "" {
			t.Errorf("Expected routedFile to be non-empty")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Timeout waiting for Unit 3 dispatch")
	}
}

func TestMultiDevicePeerDiscoveryMesh(t *testing.T) {
	token := "mesh-discovery-token"

	cfg1 := &config.Config{
		NodeName:   "Unit 1",
		ListenPort: 18821,
		AuthToken:  token,
	}
	cfg1.AddOrUpdatePeer("Unit 2", "127.0.0.1", "http://127.0.0.1:18822")

	srv1 := server.NewServer(cfg1, nil, nil)
	go func() { _ = srv1.Start() }()
	defer srv1.Stop()

	time.Sleep(200 * time.Millisecond)

	// Unit 3 queries Unit 1 /api/health with probe headers
	client := http.Client{Timeout: 3 * time.Second}
	req, _ := http.NewRequest("GET", "http://127.0.0.1:18821/api/health", nil)
	req.Header.Set("X-Bridge-Token", token)
	req.Header.Set("X-Bridge-Node-Name", "Unit 3")
	req.Header.Set("X-Bridge-Peer-URL", "http://127.0.0.1:18823")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Health check request failed: %v", err)
	}
	defer resp.Body.Close()

	var hr server.HealthResponse
	_ = json.NewDecoder(resp.Body).Decode(&hr)

	if hr.NodeName != "Unit 1" {
		t.Errorf("Expected node_name 'Unit 1', got %s", hr.NodeName)
	}

	// Verify Unit 1 automatically registered Unit 3!
	u3 := cfg1.FindPeer("Unit 3")
	if u3 == nil {
		t.Fatal("Unit 1 did not dynamically register Unit 3 from probe header")
	}
	if u3.URL != "http://127.0.0.1:18823" {
		t.Errorf("Expected Unit 3 URL 'http://127.0.0.1:18823', got %s", u3.URL)
	}
}

func TestTransitiveMeshGossipDiscovery(t *testing.T) {
	token := "transitive-token-123"

	// 1. Setup Node 1 and Node 2
	cfg1 := &config.Config{
		NodeName:   "Unit 1",
		ListenPort: 18831,
		AuthToken:  token,
	}
	cfg1.AddOrUpdatePeer("Unit 2", "127.0.0.1", "http://127.0.0.1:18832")

	cfg2 := &config.Config{
		NodeName:   "Unit 2",
		ListenPort: 18832,
		AuthToken:  token,
	}
	cfg2.AddOrUpdatePeer("Unit 1", "127.0.0.1", "http://127.0.0.1:18831")

	// Setup Node 3 starting with only knowledge of Node 1
	cfg3 := &config.Config{
		NodeName:   "Unit 3",
		ListenPort: 18833,
		AuthToken:  token,
	}
	cfg3.AddOrUpdatePeer("Unit 1", "127.0.0.1", "http://127.0.0.1:18831")

	srv1 := server.NewServer(cfg1, nil, nil)
	srv2 := server.NewServer(cfg2, nil, nil)
	srv3 := server.NewServer(cfg3, nil, nil)

	// Gossip wiring: when a node discovers a peer, announce to others
	srv1.SetPeerUpdater(func(peer config.PeerDevice, isNew bool) {
		if isNew {
			for _, other := range cfg1.GetPeers() {
				if other.URL != "" && other.URL != peer.URL && other.Name != peer.Name {
					go installer.IntroduceToRemote(other.URL, peer.Name, peer.IP, token)
				}
			}
		}
	})

	srv2.SetPeerUpdater(func(peer config.PeerDevice, isNew bool) {
		if isNew {
			for _, other := range cfg2.GetPeers() {
				if other.URL != "" && other.URL != peer.URL && other.Name != peer.Name {
					go installer.IntroduceToRemote(other.URL, peer.Name, peer.IP, token)
				}
			}
		}
	})

	go func() { _ = srv1.Start() }()
	defer srv1.Stop()
	go func() { _ = srv2.Start() }()
	defer srv2.Stop()
	go func() { _ = srv3.Start() }()
	defer srv3.Stop()

	time.Sleep(300 * time.Millisecond)

	// Unit 3 queries Unit 1 /api/health to join
	client := http.Client{Timeout: 3 * time.Second}
	req, _ := http.NewRequest("GET", "http://127.0.0.1:18831/api/health", nil)
	req.Header.Set("X-Bridge-Token", token)
	req.Header.Set("X-Bridge-Node-Name", "Unit 3")
	req.Header.Set("X-Bridge-Peer-URL", "http://127.0.0.1:18833")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Unit 3 health query to Unit 1 failed: %v", err)
	}
	defer resp.Body.Close()

	var hr server.HealthResponse
	_ = json.NewDecoder(resp.Body).Decode(&hr)

	// Unit 3 inherits mesh peers and introduces itself
	for _, mp := range hr.Peers {
		if mp.Name != "Unit 3" {
			cfg3.AddOrUpdatePeer(mp.Name, mp.IP, mp.URL)
			go installer.IntroduceToRemote(mp.URL, "Unit 3", "127.0.0.1", token)
		}
	}

	// Verify transitive convergence: Unit 2 must now have Unit 3!
	converged := waitForCondition(5*time.Second, 100*time.Millisecond, func() bool {
		u3On2 := cfg2.FindPeer("Unit 3")
		u2On3 := cfg3.FindPeer("Unit 2")
		return u3On2 != nil && u2On3 != nil
	})

	if !converged {
		t.Fatalf("Transitive mesh convergence failed: Unit 2 peers = %v, Unit 3 peers = %v", cfg2.GetPeers(), cfg3.GetPeers())
	}

	// Verify unique spool ports were allocated for all peers
	pOn2 := cfg2.FindPeer("Unit 3")
	if pOn2.SpoolPort < 9101 {
		t.Errorf("Invalid spool port for Unit 3 on Unit 2: %d", pOn2.SpoolPort)
	}
}

func TestOfflinePeerSyncDoesNotHangOrBlock(t *testing.T) {
	tempRoot := t.TempDir()

	d1Shared := filepath.Join(tempRoot, "d1_shared")
	d1State := filepath.Join(tempRoot, "d1_state.json")
	d2Shared := filepath.Join(tempRoot, "d2_shared")
	d2State := filepath.Join(tempRoot, "d2_state.json")

	_ = os.MkdirAll(d1Shared, 0755)
	_ = os.MkdirAll(d2Shared, 0755)

	token := "offline-test-token"

	cfg1 := &config.Config{
		NodeName:   "Unit 1",
		ListenPort: 18841,
		AuthToken:  token,
		SharedDir:  d1Shared,
		StateFile:  d1State,
	}
	// Unit 2 is online on 18842; Unit 3 is on offline/unbound port 18849
	cfg1.AddOrUpdatePeer("Unit 2", "127.0.0.1", "http://127.0.0.1:18842")
	cfg1.AddOrUpdatePeer("Unit 3", "127.0.0.1", "http://127.0.0.1:18849") // OFFLINE PORT

	cfg2 := &config.Config{
		NodeName:   "Unit 2",
		ListenPort: 18842,
		AuthToken:  token,
		SharedDir:  d2Shared,
		StateFile:  d2State,
	}
	cfg2.AddOrUpdatePeer("Unit 1", "127.0.0.1", "http://127.0.0.1:18841")

	sm1, _ := sync.NewStateManager(d1State)
	sm2, _ := sync.NewStateManager(d2State)

	srv1 := server.NewServer(cfg1, sm1, nil)
	srv2 := server.NewServer(cfg2, sm2, nil)

	go func() { _ = srv1.Start() }()
	defer srv1.Stop()
	go func() { _ = srv2.Start() }()
	defer srv2.Stop()

	se1 := sync.NewSyncEngine(d1Shared, "", token, sm1)
	se1.UpdatePeerURLs(cfg1.GetPeerURLs())
	_ = se1.Start()
	defer se1.Stop()

	se2 := sync.NewSyncEngine(d2Shared, "", token, sm2)
	se2.UpdatePeerURLs(cfg2.GetPeerURLs())
	_ = se2.Start()
	defer se2.Stop()

	time.Sleep(300 * time.Millisecond)

	// Write file on Unit 1: must sync to Unit 2 without hanging on offline Unit 3!
	fileName := "resilient_test.txt"
	content := []byte("Testing offline peer resilience in mesh sync")
	_ = os.WriteFile(filepath.Join(d1Shared, fileName), content, 0644)

	// Verify Unit 2 received the file within 5 seconds
	targetOn2 := filepath.Join(d2Shared, fileName)
	syncedOn2 := waitForCondition(5*time.Second, 100*time.Millisecond, func() bool {
		data, err := os.ReadFile(targetOn2)
		return err == nil && bytes.Equal(data, content)
	})
	if !syncedOn2 {
		t.Fatalf("File failed to sync to online Unit 2 when Unit 3 was offline")
	}

	// Verify Unit 1 recorded the file in stateMgr (did not hang on offline peer)
	rec, exists := sm1.Get(fileName)
	if !exists {
		t.Fatalf("Unit 1 state manager did not record file state due to offline peer stall")
	}
	if rec.Size != int64(len(content)) {
		t.Errorf("Expected recorded size %d, got %d", len(content), rec.Size)
	}
}

func TestMeshReconciliationWhenPeerComesOnline(t *testing.T) {
	tempRoot := t.TempDir()

	d1Shared := filepath.Join(tempRoot, "d1_shared")
	d1State := filepath.Join(tempRoot, "d1_state.json")
	d3Shared := filepath.Join(tempRoot, "d3_shared")
	d3State := filepath.Join(tempRoot, "d3_state.json")

	_ = os.MkdirAll(d1Shared, 0755)
	_ = os.MkdirAll(d3Shared, 0755)

	token := "reconcile-test-token"

	cfg1 := &config.Config{
		NodeName:   "Unit 1",
		ListenPort: 18851,
		AuthToken:  token,
		SharedDir:  d1Shared,
		StateFile:  d1State,
	}
	cfg1.AddOrUpdatePeer("Unit 3", "127.0.0.1", "http://127.0.0.1:18853")

	cfg3 := &config.Config{
		NodeName:   "Unit 3",
		ListenPort: 18853,
		AuthToken:  token,
		SharedDir:  d3Shared,
		StateFile:  d3State,
	}
	cfg3.AddOrUpdatePeer("Unit 1", "127.0.0.1", "http://127.0.0.1:18851")

	sm1, _ := sync.NewStateManager(d1State)
	sm3, _ := sync.NewStateManager(d3State)

	srv1 := server.NewServer(cfg1, sm1, nil)
	go func() { _ = srv1.Start() }()
	defer srv1.Stop()

	se1 := sync.NewSyncEngine(d1Shared, "", token, sm1)
	se1.UpdatePeerURLs(cfg1.GetPeerURLs())
	_ = se1.Start()
	defer se1.Stop()

	// Unit 1 creates a document while Unit 3 is offline
	missedFile := "missed_document.txt"
	missedContent := []byte("Created while Unit 3 was offline")
	fullPath := filepath.Join(d1Shared, missedFile)
	_ = os.WriteFile(fullPath, missedContent, 0644)
	h, sz, _ := sync.ComputeHash(fullPath)
	_ = sm1.Set(missedFile, sync.FileRecord{
		RelPath: missedFile,
		Hash:    h,
		Size:    sz,
		ModTime: time.Now().UnixNano(),
	})

	// Now Unit 3 comes online!
	srv3 := server.NewServer(cfg3, sm3, nil)
	go func() { _ = srv3.Start() }()
	defer srv3.Stop()

	se3 := sync.NewSyncEngine(d3Shared, "", token, sm3)
	se3.UpdatePeerURLs(cfg3.GetPeerURLs())
	_ = se3.Start()
	defer se3.Stop()

	time.Sleep(200 * time.Millisecond)

	// Unit 1 performs reconciliation with the newly online Unit 3
	se1.ReconcileWithPeer("http://127.0.0.1:18853")

	// Verify Unit 3 catches up and receives the missed file
	targetOn3 := filepath.Join(d3Shared, missedFile)
	received := waitForCondition(5*time.Second, 100*time.Millisecond, func() bool {
		data, err := os.ReadFile(targetOn3)
		return err == nil && bytes.Equal(data, missedContent)
	})

	if !received {
		t.Fatalf("Catch-up reconciliation failed: Unit 3 did not receive missed file")
	}
}

func TestLegacyPrototypeConfigAutoMigrationAndConfiguredFlag(t *testing.T) {
	tempRoot := t.TempDir()
	cfgPath := filepath.Join(tempRoot, "legacy_config.json")

	// Legacy prototype 1 config with role: desktop1
	legacyJSON := `{
		"role": "desktop1",
		"peer_name": "desktop2",
		"peer_url": "http://100.64.1.2:8765",
		"listen_port": 8765,
		"auth_token": "legacy-secret"
	}`
	_ = os.WriteFile(cfgPath, []byte(legacyJSON), 0644)

	cfg, err := config.LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("Failed to load legacy config: %v", err)
	}

	// Must be marked unconfigured so setup wizard prompts for friendly name!
	if cfg.Configured {
		t.Errorf("Expected legacy config to have Configured=false, got true")
	}
	if cfg.NodeName != "Desktop 1" {
		t.Errorf("Expected migrated node_name 'Desktop 1', got '%s'", cfg.NodeName)
	}

	// Now simulate user completing setup wizard with name "Unit 1"
	cfg.NodeName = "Unit 1"
	cfg.Configured = true
	if err := config.SaveConfig(cfgPath, cfg); err != nil {
		t.Fatalf("Failed to save updated config: %v", err)
	}

	// Reload config and verify Configured is now true
	reloaded, err := config.LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("Failed to reload config: %v", err)
	}
	if !reloaded.Configured {
		t.Errorf("Expected reloaded config to have Configured=true")
	}
	if reloaded.NodeName != "Unit 1" {
		t.Errorf("Expected reloaded node_name 'Unit 1', got '%s'", reloaded.NodeName)
	}
}

func TestBidirectionalReconciliationPullAndPush(t *testing.T) {
	tempRoot := t.TempDir()
	d1Dir := filepath.Join(tempRoot, "d1_shared")
	d2Dir := filepath.Join(tempRoot, "d2_shared")
	_ = os.MkdirAll(d1Dir, 0755)
	_ = os.MkdirAll(d2Dir, 0755)

	stateFile1 := filepath.Join(tempRoot, "state1.json")
	stateFile2 := filepath.Join(tempRoot, "state2.json")

	sm1, err := sync.NewStateManager(stateFile1)
	if err != nil {
		t.Fatalf("Failed to create sm1: %v", err)
	}
	sm2, err := sync.NewStateManager(stateFile2)
	if err != nil {
		t.Fatalf("Failed to create sm2: %v", err)
	}

	cfg1 := config.DefaultConfig()
	cfg1.SharedDir = d1Dir
	cfg1.ListenPort = 18861
	cfg1.NodeName = "Unit 1"
	cfg1.AuthToken = "test-secret"
	srv1 := server.NewServer(cfg1, sm1, nil)
	go func() { _ = srv1.Start() }()
	defer srv1.Stop()

	cfg2 := config.DefaultConfig()
	cfg2.SharedDir = d2Dir
	cfg2.ListenPort = 18862
	cfg2.NodeName = "Unit 2"
	cfg2.AuthToken = "test-secret"
	srv2 := server.NewServer(cfg2, sm2, nil)
	go func() { _ = srv2.Start() }()
	defer srv2.Stop()

	time.Sleep(100 * time.Millisecond)

	sync1 := sync.NewSyncEngine(d1Dir, "http://127.0.0.1:18862", cfg1.AuthToken, sm1)
	if err := sync1.Start(); err != nil {
		t.Fatalf("Failed to start sync1: %v", err)
	}
	defer sync1.Stop()

	sync2 := sync.NewSyncEngine(d2Dir, "http://127.0.0.1:18861", cfg2.AuthToken, sm2)
	if err := sync2.Start(); err != nil {
		t.Fatalf("Failed to start sync2: %v", err)
	}
	defer sync2.Stop()

	// 1. File exists on Unit 1: 'file_from_unit1.txt'
	f1Path := filepath.Join(d1Dir, "file_from_unit1.txt")
	_ = os.WriteFile(f1Path, []byte("Content created on Unit 1 while Unit 2 was offline"), 0644)
	h1, s1, _ := sync.ComputeHash(f1Path)
	_ = sm1.Set("file_from_unit1.txt", sync.FileRecord{RelPath: "file_from_unit1.txt", Hash: h1, Size: s1, ModTime: time.Now().UnixNano()})

	// 2. File exists on Unit 2: 'file_from_unit2.txt'
	f2Path := filepath.Join(d2Dir, "file_from_unit2.txt")
	_ = os.WriteFile(f2Path, []byte("Content created on Unit 2 while offline"), 0644)
	h2, s2, _ := sync.ComputeHash(f2Path)
	_ = sm2.Set("file_from_unit2.txt", sync.FileRecord{RelPath: "file_from_unit2.txt", Hash: h2, Size: s2, ModTime: time.Now().UnixNano()})

	// Trigger bidirectional catch-up reconciliation from Unit 2 towards Unit 1
	sync2.ReconcileWithPeer("http://127.0.0.1:18861")

	// Verify both nodes receive each other's files
	f2AtUnit1 := filepath.Join(d1Dir, "file_from_unit2.txt")
	f1AtUnit2 := filepath.Join(d2Dir, "file_from_unit1.txt")

	success := false
	for i := 0; i < 40; i++ {
		time.Sleep(100 * time.Millisecond)
		_, errA := os.Stat(f2AtUnit1)
		_, errB := os.Stat(f1AtUnit2)
		if errA == nil && errB == nil {
			success = true
			break
		}
	}

	if !success {
		_, errA := os.Stat(f2AtUnit1)
		_, errB := os.Stat(f1AtUnit2)
		t.Fatalf("Bidirectional reconciliation failed: Unit 1 has f2: %v, Unit 2 has f1: %v", errA == nil, errB == nil)
	}
}

func TestConfigOnlineStatusTrackingAndStability(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.NodeName = "Unit 1"
	cfg.AddOrUpdatePeer("Unit 2", "100.64.0.2", "http://100.64.0.2:8765")
	cfg.AddOrUpdatePeer("Unit 3", "100.64.0.3", "http://100.64.0.3:8765")

	// Initially Online is true because AddOrUpdatePeer sets it
	peers := cfg.GetPeers()
	if len(peers) != 2 {
		t.Fatalf("Expected 2 peers, got %d", len(peers))
	}

	// Change Unit 2 to offline
	cfg.SetPeerOnlineStatus("http://100.64.0.2:8765", false)
	peers = cfg.GetPeers()
	var p2, p3 *config.PeerDevice
	for i := range peers {
		if peers[i].Name == "Unit 2" {
			p2 = &peers[i]
		}
		if peers[i].Name == "Unit 3" {
			p3 = &peers[i]
		}
	}
	if p2 == nil || p2.Online != false {
		t.Errorf("Expected Unit 2 Online=false, got %v", p2.Online)
	}
	if p3 == nil || p3.Online != true {
		t.Errorf("Expected Unit 3 Online=true, got %v", p3.Online)
	}

	// Update learned name for Unit 3
	cfg.SetPeerName("http://100.64.0.3:8765", "Office Desktop")
	pLearned := cfg.FindPeer("http://100.64.0.3:8765")
	if pLearned == nil || pLearned.Name != "Office Desktop" {
		t.Errorf("Expected learned name 'Office Desktop', got '%v'", pLearned)
	}
}

func TestStrictConfiguredFlagMigrationWithArbitraryNames(t *testing.T) {
	tempRoot := t.TempDir()

	testCases := []struct {
		name     string
		jsonText string
		expected bool
	}{
		{
			name:     "Legacy prototype with role desktop1",
			jsonText: `{"role":"desktop1","node_name":"Desktop 1","peer_url":"http://100.64.1.2:8765"}`,
			expected: false,
		},
		{
			name:     "Legacy prototype with arbitrary computer name",
			jsonText: `{"node_name":"DESKTOP-ABC123","peer_url":"http://100.64.1.2:8765"}`,
			expected: false,
		},
		{
			name:     "Legacy prototype with server name",
			jsonText: `{"node_name":"WIN-TESTSERVER","listen_port":8765}`,
			expected: false,
		},
		{
			name:     "Legacy prototype with Bridge-PC name",
			jsonText: `{"node_name":"Bridge-PC","listen_port":8765}`,
			expected: false,
		},
		{
			name:     "Modern config with explicit configured flag",
			jsonText: `{"configured":true,"node_name":"Unit 1","listen_port":8765,"peers":[]}`,
			expected: true,
		},
		{
			name:     "Modern config configured true but stale Desktop 1 name",
			jsonText: `{"configured":true,"node_name":"Desktop 1","listen_port":8765}`,
			expected: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(tempRoot, fmt.Sprintf("%d.json", time.Now().UnixNano()))
			_ = os.WriteFile(p, []byte(tc.jsonText), 0644)
			c, err := config.LoadConfig(p)
			if err != nil {
				t.Fatalf("LoadConfig failed: %v", err)
			}
			if c.Configured != tc.expected {
				t.Errorf("Test '%s': expected Configured=%v, got %v", tc.name, tc.expected, c.Configured)
			}
		})
	}
}

func TestFastTailscaleIPDetection(t *testing.T) {
	// Should execute instantaneously without hanging or crashing
	ip := installer.FindLocalTailscaleIP()
	t.Logf("FindLocalTailscaleIP result: '%s'", ip)

	cached := installer.GetTailscaleIP()
	t.Logf("GetTailscaleIP result: '%s'", cached)
}


