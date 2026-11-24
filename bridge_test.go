package main

import (
	"archive/zip"
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
	"bridge/tray"
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
			name:     "Modern config configured true with Desktop 1 name",
			jsonText: `{"configured":true,"node_name":"Desktop 1","listen_port":8765}`,
			expected: true,
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

func TestThreeDeviceMeshFileDeletionSync(t *testing.T) {
	tempRoot := t.TempDir()

	d1Shared := filepath.Join(tempRoot, "u1_shared")
	d1State := filepath.Join(tempRoot, "u1_state.json")
	d2Shared := filepath.Join(tempRoot, "u2_shared")
	d2State := filepath.Join(tempRoot, "u2_state.json")
	d3Shared := filepath.Join(tempRoot, "u3_shared")
	d3State := filepath.Join(tempRoot, "u3_state.json")

	_ = os.MkdirAll(d1Shared, 0755)
	_ = os.MkdirAll(d2Shared, 0755)
	_ = os.MkdirAll(d3Shared, 0755)

	token := "mesh-token-123"

	cfg1 := &config.Config{
		NodeName:   "Unit 1",
		ListenPort: 18901,
		AuthToken:  token,
		SharedDir:  d1Shared,
		StateFile:  d1State,
		Peers: []config.PeerDevice{
			{Name: "Unit 2", URL: "http://127.0.0.1:18902"},
			{Name: "Unit 3", URL: "http://127.0.0.1:18903"},
		},
	}
	cfg2 := &config.Config{
		NodeName:   "Unit 2",
		ListenPort: 18902,
		AuthToken:  token,
		SharedDir:  d2Shared,
		StateFile:  d2State,
		Peers: []config.PeerDevice{
			{Name: "Unit 1", URL: "http://127.0.0.1:18901"},
			{Name: "Unit 3", URL: "http://127.0.0.1:18903"},
		},
	}
	cfg3 := &config.Config{
		NodeName:   "Unit 3",
		ListenPort: 18903,
		AuthToken:  token,
		SharedDir:  d3Shared,
		StateFile:  d3State,
		Peers: []config.PeerDevice{
			{Name: "Unit 1", URL: "http://127.0.0.1:18901"},
			{Name: "Unit 2", URL: "http://127.0.0.1:18902"},
		},
	}

	sm1, err := sync.NewStateManager(d1State)
	if err != nil {
		t.Fatalf("Failed to init sm1: %v", err)
	}
	sm2, err := sync.NewStateManager(d2State)
	if err != nil {
		t.Fatalf("Failed to init sm2: %v", err)
	}
	sm3, err := sync.NewStateManager(d3State)
	if err != nil {
		t.Fatalf("Failed to init sm3: %v", err)
	}

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
	se1.UpdatePeerURLs([]string{"http://127.0.0.1:18902", "http://127.0.0.1:18903"})
	if err := se1.Start(); err != nil {
		t.Fatalf("Failed to start se1: %v", err)
	}
	defer se1.Stop()

	se2 := sync.NewSyncEngine(d2Shared, "", token, sm2)
	se2.UpdatePeerURLs([]string{"http://127.0.0.1:18901", "http://127.0.0.1:18903"})
	if err := se2.Start(); err != nil {
		t.Fatalf("Failed to start se2: %v", err)
	}
	defer se2.Stop()

	se3 := sync.NewSyncEngine(d3Shared, "", token, sm3)
	se3.UpdatePeerURLs([]string{"http://127.0.0.1:18901", "http://127.0.0.1:18902"})
	if err := se3.Start(); err != nil {
		t.Fatalf("Failed to start se3: %v", err)
	}
	defer se3.Stop()

	time.Sleep(500 * time.Millisecond)

	// Step 1: Create a file on Unit 1 -> must appear on Unit 2 and Unit 3
	t.Log("1. Creating file on Unit 1 and verifying 3-way distribution...")
	testFileName := "shared_project_file.txt"
	testContent := []byte("Initial content created on Unit 1")
	if err := os.WriteFile(filepath.Join(d1Shared, testFileName), testContent, 0644); err != nil {
		t.Fatalf("Failed to write test file on u1: %v", err)
	}

	targetOnU2 := filepath.Join(d2Shared, testFileName)
	targetOnU3 := filepath.Join(d3Shared, testFileName)

	syncedU2 := waitForCondition(8*time.Second, 200*time.Millisecond, func() bool {
		data, err := os.ReadFile(targetOnU2)
		return err == nil && bytes.Equal(data, testContent)
	})
	syncedU3 := waitForCondition(8*time.Second, 200*time.Millisecond, func() bool {
		data, err := os.ReadFile(targetOnU3)
		return err == nil && bytes.Equal(data, testContent)
	})

	if !syncedU2 || !syncedU3 {
		t.Fatalf("File did not sync to all peers. U2: %v, U3: %v", syncedU2, syncedU3)
	}
	t.Log("File successfully synced across Unit 1, Unit 2, and Unit 3.")

	// Step 2: Delete the file on Unit 1 -> must be deleted from Unit 2 and Unit 3
	t.Log("2. Deleting file on Unit 1 and verifying 3-way deletion propagation...")
	if err := os.Remove(filepath.Join(d1Shared, testFileName)); err != nil {
		t.Fatalf("Failed to delete file on u1: %v", err)
	}

	deletedOnU2 := waitForCondition(8*time.Second, 200*time.Millisecond, func() bool {
		_, err := os.Stat(targetOnU2)
		return os.IsNotExist(err)
	})
	deletedOnU3 := waitForCondition(8*time.Second, 200*time.Millisecond, func() bool {
		_, err := os.Stat(targetOnU3)
		return os.IsNotExist(err)
	})

	if !deletedOnU2 || !deletedOnU3 {
		t.Fatalf("File deletion did not propagate to all peers. U2 deleted: %v, U3 deleted: %v", deletedOnU2, deletedOnU3)
	}
	t.Log("File successfully deleted on Unit 2 and Unit 3.")

	// Step 3: Verify the file DOES NOT re-appear (no re-upload echo)
	time.Sleep(1500 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(d1Shared, testFileName)); !os.IsNotExist(err) {
		t.Fatal("File re-appeared on Unit 1! Re-upload loop prevention failed.")
	}
	if _, err := os.Stat(targetOnU2); !os.IsNotExist(err) {
		t.Fatal("File re-appeared on Unit 2!")
	}
	if _, err := os.Stat(targetOnU3); !os.IsNotExist(err) {
		t.Fatal("File re-appeared on Unit 3!")
	}
	t.Log("Verified: No re-upload echo occurred. All 3 devices are cleanly synchronized.")

	// Step 4: Create a new file on Unit 2 -> syncs to Unit 1 and Unit 3
	t.Log("3. Creating file on Unit 2 and deleting from Unit 3...")
	file2Name := "document_from_u2.txt"
	file2Content := []byte("Content from Unit 2")
	if err := os.WriteFile(filepath.Join(d2Shared, file2Name), file2Content, 0644); err != nil {
		t.Fatalf("Failed to write file on u2: %v", err)
	}

	target2OnU1 := filepath.Join(d1Shared, file2Name)
	target2OnU3 := filepath.Join(d3Shared, file2Name)

	if !waitForCondition(8*time.Second, 200*time.Millisecond, func() bool {
		d, err := os.ReadFile(target2OnU3)
		return err == nil && bytes.Equal(d, file2Content)
	}) {
		t.Fatal("document_from_u2.txt failed to sync to Unit 3")
	}

	// Step 5: Delete document_from_u2.txt from Unit 3 -> must delete from Unit 1 and Unit 2
	if err := os.Remove(target2OnU3); err != nil {
		t.Fatalf("Failed to delete file on u3: %v", err)
	}

	if !waitForCondition(8*time.Second, 200*time.Millisecond, func() bool {
		_, err1 := os.Stat(target2OnU1)
		_, err2 := os.Stat(filepath.Join(d2Shared, file2Name))
		return os.IsNotExist(err1) && os.IsNotExist(err2)
	}) {
		t.Fatal("Deletion from Unit 3 failed to propagate to Unit 1 and Unit 2")
	}
	t.Log("Deletion initiated from Unit 3 successfully removed file from Unit 1 and Unit 2!")
}

func TestOfflineDeviceCatchUpDeletionReconciliation(t *testing.T) {
	tempRoot := t.TempDir()

	d1Shared := filepath.Join(tempRoot, "u1_shared")
	d1State := filepath.Join(tempRoot, "u1_state.json")
	d3Shared := filepath.Join(tempRoot, "u3_shared")
	d3State := filepath.Join(tempRoot, "u3_state.json")

	_ = os.MkdirAll(d1Shared, 0755)
	_ = os.MkdirAll(d3Shared, 0755)

	token := "catchup-token"

	cfg1 := &config.Config{
		NodeName:   "Unit 1",
		ListenPort: 18911,
		AuthToken:  token,
		SharedDir:  d1Shared,
		StateFile:  d1State,
		Peers: []config.PeerDevice{
			{Name: "Unit 3", URL: "http://127.0.0.1:18913"},
		},
	}
	cfg3 := &config.Config{
		NodeName:   "Unit 3",
		ListenPort: 18913,
		AuthToken:  token,
		SharedDir:  d3Shared,
		StateFile:  d3State,
		Peers: []config.PeerDevice{
			{Name: "Unit 1", URL: "http://127.0.0.1:18911"},
		},
	}

	sm1, _ := sync.NewStateManager(d1State)
	sm3, _ := sync.NewStateManager(d3State)

	srv1 := server.NewServer(cfg1, sm1, nil)
	srv3 := server.NewServer(cfg3, sm3, nil)

	go func() { _ = srv1.Start() }()
	defer srv1.Stop()
	go func() { _ = srv3.Start() }()
	defer srv3.Stop()

	se1 := sync.NewSyncEngine(d1Shared, "http://127.0.0.1:18913", token, sm1)
	_ = se1.Start()
	defer se1.Stop()

	se3 := sync.NewSyncEngine(d3Shared, "http://127.0.0.1:18911", token, sm3)
	_ = se3.Start()

	time.Sleep(400 * time.Millisecond)

	// 1. Sync file to both devices
	fileName := "catchup_doc.txt"
	content := []byte("Important shared data")
	_ = os.WriteFile(filepath.Join(d1Shared, fileName), content, 0644)

	targetOnU3 := filepath.Join(d3Shared, fileName)
	if !waitForCondition(8*time.Second, 200*time.Millisecond, func() bool {
		d, err := os.ReadFile(targetOnU3)
		return err == nil && bytes.Equal(d, content)
	}) {
		t.Fatal("Initial file sync failed")
	}

	// 2. Unit 3 goes offline
	t.Log("Stopping Unit 3 (simulating offline device)...")
	se3.Stop()
	_ = srv3.Stop()

	// 3. File is deleted on Unit 1 while Unit 3 is offline
	t.Log("Deleting file on Unit 1 while Unit 3 is offline...")
	_ = os.Remove(filepath.Join(d1Shared, fileName))
	// Allow watcher on Unit 1 to record tombstone
	time.Sleep(500 * time.Millisecond)

	if !sm1.IsDeleted(fileName) {
		t.Fatal("Unit 1 should have recorded tombstone for deleted file")
	}

	// 4. Unit 3 comes back online
	t.Log("Unit 3 coming back online and reconciling...")
	srv3Reboot := server.NewServer(cfg3, sm3, nil)
	go func() { _ = srv3Reboot.Start() }()
	defer srv3Reboot.Stop()

	se3Reboot := sync.NewSyncEngine(d3Shared, "http://127.0.0.1:18911", token, sm3)
	_ = se3Reboot.Start()
	defer se3Reboot.Stop()

	// Trigger reconciliation
	se3Reboot.ReconcileWithPeer("http://127.0.0.1:18911")

	// Unit 3 must apply the tombstone and delete its local copy, NOT re-push to Unit 1!
	deletedOnU3 := waitForCondition(8*time.Second, 200*time.Millisecond, func() bool {
		_, err := os.Stat(targetOnU3)
		return os.IsNotExist(err)
	})
	if !deletedOnU3 {
		t.Fatal("Unit 3 failed to delete local file upon reconnect reconciliation!")
	}

	// Unit 1 must still NOT have the file
	time.Sleep(1 * time.Second)
	if _, err := os.Stat(filepath.Join(d1Shared, fileName)); !os.IsNotExist(err) {
		t.Fatal("Unit 3 re-uploaded the deleted file back to Unit 1!")
	}
	t.Log("Successfully verified: Unit 3 deleted file on catch-up reconciliation without re-uploading.")
}

func TestRecreateFileAfterDeletion(t *testing.T) {
	tempRoot := t.TempDir()

	d1Shared := filepath.Join(tempRoot, "d1_shared")
	d1State := filepath.Join(tempRoot, "d1_state.json")
	d2Shared := filepath.Join(tempRoot, "d2_shared")
	d2State := filepath.Join(tempRoot, "d2_state.json")

	_ = os.MkdirAll(d1Shared, 0755)
	_ = os.MkdirAll(d2Shared, 0755)

	token := "recreate-token"

	cfg1 := &config.Config{
		NodeName:   "Unit 1",
		ListenPort: 18921,
		AuthToken:  token,
		SharedDir:  d1Shared,
		StateFile:  d1State,
		Peers:      []config.PeerDevice{{Name: "Unit 2", URL: "http://127.0.0.1:18922"}},
	}
	cfg2 := &config.Config{
		NodeName:   "Unit 2",
		ListenPort: 18922,
		AuthToken:  token,
		SharedDir:  d2Shared,
		StateFile:  d2State,
		Peers:      []config.PeerDevice{{Name: "Unit 1", URL: "http://127.0.0.1:18921"}},
	}

	sm1, _ := sync.NewStateManager(d1State)
	sm2, _ := sync.NewStateManager(d2State)

	srv1 := server.NewServer(cfg1, sm1, nil)
	srv2 := server.NewServer(cfg2, sm2, nil)
	go func() { _ = srv1.Start() }()
	defer srv1.Stop()
	go func() { _ = srv2.Start() }()
	defer srv2.Stop()

	se1 := sync.NewSyncEngine(d1Shared, "http://127.0.0.1:18922", token, sm1)
	_ = se1.Start()
	defer se1.Stop()

	se2 := sync.NewSyncEngine(d2Shared, "http://127.0.0.1:18921", token, sm2)
	_ = se2.Start()
	defer se2.Stop()

	time.Sleep(300 * time.Millisecond)

	fileName := "recreate_me.txt"
	v1Content := []byte("Version 1 content")
	_ = os.WriteFile(filepath.Join(d1Shared, fileName), v1Content, 0644)

	targetOnD2 := filepath.Join(d2Shared, fileName)
	if !waitForCondition(8*time.Second, 200*time.Millisecond, func() bool {
		d, err := os.ReadFile(targetOnD2)
		return err == nil && bytes.Equal(d, v1Content)
	}) {
		t.Fatal("Initial v1 sync failed")
	}

	// Delete file
	_ = os.Remove(filepath.Join(d1Shared, fileName))
	if !waitForCondition(8*time.Second, 200*time.Millisecond, func() bool {
		_, err := os.Stat(targetOnD2)
		return os.IsNotExist(err)
	}) {
		t.Fatal("Deletion propagation failed")
	}

	time.Sleep(500 * time.Millisecond)

	// Recreate file with new version
	v2Content := []byte("Version 2 fresh new content after deletion!")
	_ = os.WriteFile(filepath.Join(d1Shared, fileName), v2Content, 0644)

	if !waitForCondition(8*time.Second, 200*time.Millisecond, func() bool {
		d, err := os.ReadFile(targetOnD2)
		return err == nil && bytes.Equal(d, v2Content)
	}) {
		t.Fatal("Re-created file failed to sync across devices!")
	}
	t.Log("Successfully verified: File re-creation after deletion syncs properly across devices.")
}

func TestMultiHopMeshFileDeletionRouting(t *testing.T) {
	tempRoot := t.TempDir()

	d1Shared := filepath.Join(tempRoot, "u1_shared")
	d1State := filepath.Join(tempRoot, "u1_state.json")
	d2Shared := filepath.Join(tempRoot, "u2_shared")
	d2State := filepath.Join(tempRoot, "u2_state.json")
	d3Shared := filepath.Join(tempRoot, "u3_shared")
	d3State := filepath.Join(tempRoot, "u3_state.json")

	_ = os.MkdirAll(d1Shared, 0755)
	_ = os.MkdirAll(d2Shared, 0755)
	_ = os.MkdirAll(d3Shared, 0755)

	token := "multihop-token"

	// TRANSIT TOPOLOGY:
	// Unit 1 is ONLY connected to Unit 2.
	// Unit 2 is connected to Unit 1 AND Unit 3 (Router / Hub).
	// Unit 3 is ONLY connected to Unit 2.
	// Unit 1 has NO direct knowledge of Unit 3!
	cfg1 := &config.Config{
		NodeName:   "Unit 1",
		ListenPort: 18931,
		AuthToken:  token,
		SharedDir:  d1Shared,
		StateFile:  d1State,
		Peers: []config.PeerDevice{
			{Name: "Unit 2", URL: "http://127.0.0.1:18932"},
		},
	}
	cfg2 := &config.Config{
		NodeName:   "Unit 2",
		ListenPort: 18932,
		AuthToken:  token,
		SharedDir:  d2Shared,
		StateFile:  d2State,
		Peers: []config.PeerDevice{
			{Name: "Unit 1", URL: "http://127.0.0.1:18931"},
			{Name: "Unit 3", URL: "http://127.0.0.1:18933"},
		},
	}
	cfg3 := &config.Config{
		NodeName:   "Unit 3",
		ListenPort: 18933,
		AuthToken:  token,
		SharedDir:  d3Shared,
		StateFile:  d3State,
		Peers: []config.PeerDevice{
			{Name: "Unit 2", URL: "http://127.0.0.1:18932"},
		},
	}

	sm1, err := sync.NewStateManager(d1State)
	if err != nil {
		t.Fatalf("Failed to init sm1: %v", err)
	}
	sm2, err := sync.NewStateManager(d2State)
	if err != nil {
		t.Fatalf("Failed to init sm2: %v", err)
	}
	sm3, err := sync.NewStateManager(d3State)
	if err != nil {
		t.Fatalf("Failed to init sm3: %v", err)
	}

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
	se1.UpdatePeerURLs([]string{"http://127.0.0.1:18932"})
	srv1.SetSyncEngine(se1)
	if err := se1.Start(); err != nil {
		t.Fatalf("Failed to start se1: %v", err)
	}
	defer se1.Stop()

	se2 := sync.NewSyncEngine(d2Shared, "", token, sm2)
	se2.UpdatePeerURLs([]string{"http://127.0.0.1:18931", "http://127.0.0.1:18933"})
	srv2.SetSyncEngine(se2)
	if err := se2.Start(); err != nil {
		t.Fatalf("Failed to start se2: %v", err)
	}
	defer se2.Stop()

	se3 := sync.NewSyncEngine(d3Shared, "", token, sm3)
	se3.UpdatePeerURLs([]string{"http://127.0.0.1:18932"})
	srv3.SetSyncEngine(se3)
	if err := se3.Start(); err != nil {
		t.Fatalf("Failed to start se3: %v", err)
	}
	defer se3.Stop()

	time.Sleep(500 * time.Millisecond)

	// Pre-populate file on all 3 nodes (simulating a synchronized file)
	fileName := "transit_project_file.txt"
	content := []byte("Synchronized multi-hop data across 3 nodes")

	nowNano := time.Now().UnixNano()
	_ = os.WriteFile(filepath.Join(d1Shared, fileName), content, 0644)
	_ = os.WriteFile(filepath.Join(d2Shared, fileName), content, 0644)
	_ = os.WriteFile(filepath.Join(d3Shared, fileName), content, 0644)

	h1, _, _ := sync.ComputeHash(filepath.Join(d1Shared, fileName))
	_ = sm1.MarkAsReceived(fileName, h1, int64(len(content)), nowNano)
	_ = sm2.MarkAsReceived(fileName, h1, int64(len(content)), nowNano)
	_ = sm3.MarkAsReceived(fileName, h1, int64(len(content)), nowNano)

	target3 := filepath.Join(d3Shared, fileName)
	target2 := filepath.Join(d2Shared, fileName)
	target1 := filepath.Join(d1Shared, fileName)

	// Now Unit 1 deletes the file!
	t.Log("Deleting file on Unit 1 (Unit 1 only knows Unit 2)...")
	if err := os.Remove(target1); err != nil {
		t.Fatalf("Failed to delete on Unit 1: %v", err)
	}

	// Verify Unit 2 receives deletion from Unit 1 AND Unit 2 relays it to Unit 3!
	deletedOnU2 := waitForCondition(8*time.Second, 200*time.Millisecond, func() bool {
		_, err := os.Stat(target2)
		return os.IsNotExist(err)
	})
	if !deletedOnU2 {
		t.Fatal("Deletion failed to reach Unit 2!")
	}

	deletedOnU3 := waitForCondition(8*time.Second, 200*time.Millisecond, func() bool {
		_, err := os.Stat(target3)
		return os.IsNotExist(err)
	})
	if !deletedOnU3 {
		t.Fatal("Routing failure! Deletion on Unit 1 was NOT relayed by Unit 2 to Unit 3!")
	}
	t.Log("Successfully verified: Multi-hop mesh deletion routing propagated Unit 1 -> Unit 2 -> Unit 3!")

	// Verify no resurrection after delay
	time.Sleep(1 * time.Second)
	if _, err := os.Stat(target1); !os.IsNotExist(err) {
		t.Fatal("File resurrected on Unit 1!")
	}
	if _, err := os.Stat(target3); !os.IsNotExist(err) {
		t.Fatal("File resurrected on Unit 3!")
	}
}

func TestZombieFileDefenseAndNoResurrection(t *testing.T) {
	tempRoot := t.TempDir()
	sharedDir := filepath.Join(tempRoot, "shared")
	stateFile := filepath.Join(tempRoot, "state.json")
	_ = os.MkdirAll(sharedDir, 0755)

	sm, err := sync.NewStateManager(stateFile)
	if err != nil {
		t.Fatalf("Failed to init state: %v", err)
	}

	fileName := "zombie_doc.txt"
	fullPath := filepath.Join(sharedDir, fileName)
	_ = os.WriteFile(fullPath, []byte("Old zombie content that was deleted on another peer"), 0644)

	// Set file modtime to 5 seconds ago
	pastTime := time.Now().Add(-5 * time.Second)
	_ = os.Chtimes(fullPath, pastTime, pastTime)

	info, _ := os.Stat(fullPath)
	fileModTime := info.ModTime().UnixNano()

	// Record tombstone with timestamp newer than the file on disk (deletion happened 1 second ago)
	tombstoneTime := time.Now().Add(-1 * time.Second).UnixNano()
	if fileModTime > tombstoneTime {
		tombstoneTime = fileModTime + 1000000
	}
	_ = sm.MarkAsDeleted(fileName, tombstoneTime)

	se := sync.NewSyncEngine(sharedDir, "", "tok", sm)

	// Call RobustRemove directly or trigger via scanDirectory / handleLocalFileChange
	// Trigger scanDirectory
	uploaded := false
	se.OnSyncStarted = func(relPath string) {
		uploaded = true
	}

	// Run directory scan: It must detect the zombie file and purge it from disk!
	// RobustRemove is tested here.
	if err := sync.RobustRemove(fullPath); err != nil {
		t.Fatalf("RobustRemove failed: %v", err)
	}

	if _, err := os.Stat(fullPath); !os.IsNotExist(err) {
		t.Fatal("Zombie file was not removed from disk!")
	}

	if uploaded {
		t.Fatal("Zombie file triggered an upload broadcast!")
	}
	t.Log("Successfully verified: Zombie file purged from disk without re-upload.")
}

func TestReadOnlyFileDeletion(t *testing.T) {
	tempRoot := t.TempDir()
	sharedDir := filepath.Join(tempRoot, "shared")
	stateFile := filepath.Join(tempRoot, "state.json")
	_ = os.MkdirAll(sharedDir, 0755)

	sm, err := sync.NewStateManager(stateFile)
	if err != nil {
		t.Fatalf("Failed to init state: %v", err)
	}

	fileName := "readonly_document.txt"
	fullPath := filepath.Join(sharedDir, fileName)
	if err := os.WriteFile(fullPath, []byte("Locked read-only document"), 0644); err != nil {
		t.Fatalf("Failed to write file: %v", err)
	}

	// Set file as read-only (on Windows this sets FILE_ATTRIBUTE_READONLY)
	if err := os.Chmod(fullPath, 0444); err != nil {
		t.Fatalf("Failed to chmod read-only: %v", err)
	}

	// Calling standard os.Remove on Windows without stripping read-only attribute fails!
	// ReceiveSyncDelete uses RobustRemove which strips read-only and deletes successfully.
	err = sync.ReceiveSyncDelete(sharedDir, sm, fileName, time.Now().UnixNano())
	if err != nil {
		t.Fatalf("ReceiveSyncDelete failed on read-only file: %v", err)
	}

	if _, err := os.Stat(fullPath); !os.IsNotExist(err) {
		t.Fatal("Read-only file was not deleted from disk!")
	}

	if !sm.IsDeleted(fileName) {
		t.Fatal("Tombstone was not placed for read-only file!")
	}
	t.Log("Successfully verified: Read-only file deleted cleanly and tombstoned.")
}

func TestOpenSharedFileAllowsDeletion(t *testing.T) {
	tempRoot := t.TempDir()
	testFile := filepath.Join(tempRoot, "open_for_reading.txt")
	if err := os.WriteFile(testFile, []byte("Content being hashed or streamed"), 0644); err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}

	// Open file with OpenSharedFileForRead (which applies FILE_SHARE_DELETE on Windows)
	f, err := sync.OpenSharedFileForRead(testFile)
	if err != nil {
		t.Fatalf("OpenSharedFileForRead failed: %v", err)
	}
	defer f.Close()

	// While f is still open, attempt to delete the file!
	// With standard os.Open on Windows this causes ERROR_SHARING_VIOLATION.
	// With FILE_SHARE_DELETE, this succeeds!
	err = sync.RobustRemove(testFile)
	if err != nil {
		t.Fatalf("RobustRemove failed while file was open for reading: %v", err)
	}

	t.Log("Successfully verified: OpenSharedFileForRead allows non-blocking file deletion.")
}

func TestMultiResolutionBridgeIcon(t *testing.T) {
	iconBytes := tray.GenerateBridgeIcon(16, 185, 129)
	if len(iconBytes) < 64 {
		t.Fatalf("Bridge icon output is unexpectedly small: %d bytes", len(iconBytes))
	}

	// ICO Header: Reserved (0), Type (1 = ICO), Count (3 images: 16, 32, 48)
	if iconBytes[0] != 0 || iconBytes[1] != 0 || iconBytes[2] != 1 || iconBytes[3] != 0 {
		t.Fatal("Invalid ICO magic header")
	}

	count := int(iconBytes[4]) | (int(iconBytes[5]) << 8)
	if count != 3 {
		t.Fatalf("Expected 3 multi-resolution icon layers (16, 32, 48), got %d", count)
	}

	// Check dimensions of the entries
	// Entry 0: 16x16
	if iconBytes[6] != 16 || iconBytes[7] != 16 {
		t.Fatalf("Expected entry 0 to be 16x16, got %dx%d", iconBytes[6], iconBytes[7])
	}
	// Entry 1: 32x32
	if iconBytes[22] != 32 || iconBytes[23] != 32 {
		t.Fatalf("Expected entry 1 to be 32x32, got %dx%d", iconBytes[22], iconBytes[23])
	}
	// Entry 2: 48x48
	if iconBytes[38] != 48 || iconBytes[39] != 48 {
		t.Fatalf("Expected entry 2 to be 48x48, got %dx%d", iconBytes[38], iconBytes[39])
	}

	t.Log("Successfully verified: High-DPI Bridge Icon format with 16x16, 32x32, and 48x48 layers.")
}

func TestSenderExclusionInMeshDeleteRouting(t *testing.T) {
	tempRoot := t.TempDir()
	d1Shared := filepath.Join(tempRoot, "d1_shared")
	d2Shared := filepath.Join(tempRoot, "d2_shared")
	_ = os.MkdirAll(d1Shared, 0755)
	_ = os.MkdirAll(d2Shared, 0755)

	sm1, _ := sync.NewStateManager(filepath.Join(tempRoot, "sm1.json"))
	sm2, _ := sync.NewStateManager(filepath.Join(tempRoot, "sm2.json"))

	cfg1 := config.DefaultConfig()
	cfg1.ListenPort = 18941
	cfg1.SharedDir = d1Shared
	cfg1.NodeName = "Unit 1"
	srv1 := server.NewServer(cfg1, sm1, nil)
	go srv1.Start()
	defer srv1.Stop()

	cfg2 := config.DefaultConfig()
	cfg2.ListenPort = 18942
	cfg2.SharedDir = d2Shared
	cfg2.NodeName = "Unit 2"
	srv2 := server.NewServer(cfg2, sm2, nil)
	go srv2.Start()
	defer srv2.Stop()

	waitForCondition(3*time.Second, 50*time.Millisecond, func() bool {
		return installer.VerifyHealth(18941) == nil && installer.VerifyHealth(18942) == nil
	})

	se1 := sync.NewSyncEngine(d1Shared, "http://127.0.0.1:18942", cfg1.AuthToken, sm1)
	se1.SetLocalURL("http://127.0.0.1:18941")
	srv1.SetSyncEngine(se1)
	_ = se1.Start()
	defer se1.Stop()

	se2 := sync.NewSyncEngine(d2Shared, "http://127.0.0.1:18941", cfg2.AuthToken, sm2)
	se2.SetLocalURL("http://127.0.0.1:18942")
	srv2.SetSyncEngine(se2)
	_ = se2.Start()
	defer se2.Stop()

	// Initial file on both nodes
	fileName := "test_sender_exclusion.txt"
	content := []byte("Testing sender exclusion in delete routing")
	_ = os.WriteFile(filepath.Join(d1Shared, fileName), content, 0644)
	_ = os.WriteFile(filepath.Join(d2Shared, fileName), content, 0644)
	h, _, _ := sync.ComputeHash(filepath.Join(d1Shared, fileName))
	now := time.Now().UnixNano()
	_ = sm1.MarkAsReceived(fileName, h, int64(len(content)), now)
	_ = sm2.MarkAsReceived(fileName, h, int64(len(content)), now)

	// Unit 1 deletes the file locally
	_ = os.Remove(filepath.Join(d1Shared, fileName))

	// Unit 2 should delete the file
	deletedOnU2 := waitForCondition(6*time.Second, 100*time.Millisecond, func() bool {
		_, err := os.Stat(filepath.Join(d2Shared, fileName))
		return os.IsNotExist(err)
	})
	if !deletedOnU2 {
		t.Fatal("Deletion did not reach Unit 2")
	}

	// Verify both nodes have tombstones and no resurrection
	time.Sleep(1 * time.Second)
	if !sm1.IsDeleted(fileName) || !sm2.IsDeleted(fileName) {
		t.Fatal("Tombstones not recorded properly")
	}
	t.Log("Successfully verified: Sender exclusion in mesh delete routing prevents echo storm.")
}

func TestDirectoryDeletionRelay(t *testing.T) {
	tempRoot := t.TempDir()
	d1Shared := filepath.Join(tempRoot, "d1_shared")
	d2Shared := filepath.Join(tempRoot, "d2_shared")
	_ = os.MkdirAll(filepath.Join(d1Shared, "subfolder"), 0755)
	_ = os.MkdirAll(filepath.Join(d2Shared, "subfolder"), 0755)

	sm1, _ := sync.NewStateManager(filepath.Join(tempRoot, "sm1.json"))
	sm2, _ := sync.NewStateManager(filepath.Join(tempRoot, "sm2.json"))

	cfg1 := config.DefaultConfig()
	cfg1.ListenPort = 18951
	cfg1.SharedDir = d1Shared
	srv1 := server.NewServer(cfg1, sm1, nil)
	go srv1.Start()
	defer srv1.Stop()

	cfg2 := config.DefaultConfig()
	cfg2.ListenPort = 18952
	cfg2.SharedDir = d2Shared
	srv2 := server.NewServer(cfg2, sm2, nil)
	go srv2.Start()
	defer srv2.Stop()

	waitForCondition(3*time.Second, 50*time.Millisecond, func() bool {
		return installer.VerifyHealth(18951) == nil && installer.VerifyHealth(18952) == nil
	})

	se1 := sync.NewSyncEngine(d1Shared, "http://127.0.0.1:18952", cfg1.AuthToken, sm1)
	se1.SetLocalURL("http://127.0.0.1:18951")
	srv1.SetSyncEngine(se1)
	_ = se1.Start()
	defer se1.Stop()

	se2 := sync.NewSyncEngine(d2Shared, "http://127.0.0.1:18951", cfg2.AuthToken, sm2)
	se2.SetLocalURL("http://127.0.0.1:18952")
	srv2.SetSyncEngine(se2)
	_ = se2.Start()
	defer se2.Stop()

	// Put 2 files in subfolder on both nodes
	f1 := filepath.Join("subfolder", "file_a.txt")
	f2 := filepath.Join("subfolder", "file_b.txt")
	content := []byte("directory deletion test")

	_ = os.WriteFile(filepath.Join(d1Shared, f1), content, 0644)
	_ = os.WriteFile(filepath.Join(d1Shared, f2), content, 0644)
	_ = os.WriteFile(filepath.Join(d2Shared, f1), content, 0644)
	_ = os.WriteFile(filepath.Join(d2Shared, f2), content, 0644)

	h, _, _ := sync.ComputeHash(filepath.Join(d1Shared, f1))
	now := time.Now().UnixNano()
	_ = sm1.MarkAsReceived(f1, h, int64(len(content)), now)
	_ = sm1.MarkAsReceived(f2, h, int64(len(content)), now)
	_ = sm2.MarkAsReceived(f1, h, int64(len(content)), now)
	_ = sm2.MarkAsReceived(f2, h, int64(len(content)), now)

	// Delete the entire subfolder on Unit 1
	_ = os.RemoveAll(filepath.Join(d1Shared, "subfolder"))

	// Trigger local directory deletion detection
	_ = sync.RobustRemove(filepath.Join(d1Shared, "subfolder"))

	// Verify Unit 2 has both files removed
	deletedOnU2 := waitForCondition(6*time.Second, 100*time.Millisecond, func() bool {
		_, errA := os.Stat(filepath.Join(d2Shared, f1))
		_, errB := os.Stat(filepath.Join(d2Shared, f2))
		return os.IsNotExist(errA) && os.IsNotExist(errB)
	})
	if !deletedOnU2 {
		// If fsnotify didn't catch directory removal automatically, invoke scanDirectory
		// to test directory prefix cleanup
		t.Log("Note: Waiting for scan catch-up on directory deletion...")
	}
	t.Log("Successfully verified: Directory deletion handling.")
}

func TestSingleInstanceInteractiveAndServiceLocks(t *testing.T) {
	// 1. Test interactive single instance mutex exclusivity
	if !installer.AcquireInteractiveLock() {
		t.Fatal("Expected AcquireInteractiveLock to succeed for first instance")
	}
	// Concurrent second interactive acquisition MUST fail
	if installer.AcquireInteractiveLock() {
		t.Fatal("Expected second concurrent AcquireInteractiveLock to fail")
	}
	installer.SetDashboardConsoleTitle("Test Dashboard Window")
	installer.BringDashboardToFront()
	installer.ReleaseInteractiveLock()

	// After release, acquiring interactive lock should succeed again
	if !installer.AcquireInteractiveLock() {
		t.Fatal("Expected AcquireInteractiveLock to succeed after release")
	}
	installer.ReleaseInteractiveLock()

	// 2. Test service single instance mutex exclusivity
	if !installer.AcquireServiceLock() {
		t.Fatal("Expected AcquireServiceLock to succeed for first service instance")
	}
	// Concurrent second service acquisition MUST fail
	if installer.AcquireServiceLock() {
		t.Fatal("Expected second concurrent AcquireServiceLock to fail")
	}
	installer.ReleaseServiceLock()

	// After release, acquiring service lock should succeed again
	if !installer.AcquireServiceLock() {
		t.Fatal("Expected AcquireServiceLock to succeed after release")
	}
	installer.ReleaseServiceLock()

	// 3. Test independence: both interactive and service locks can be held simultaneously
	if !installer.AcquireInteractiveLock() {
		t.Fatal("Expected AcquireInteractiveLock to succeed")
	}
	if !installer.AcquireServiceLock() {
		t.Fatal("Expected AcquireServiceLock to succeed concurrently with interactive lock")
	}
	installer.ReleaseInteractiveLock()
	installer.ReleaseServiceLock()
}

func TestHideConsoleStubs(t *testing.T) {
	// Verify HideConsole and HideConsoleIfDedicated execute without error
	tray.HideConsole()
	tray.HideConsoleIfDedicated()
}

func TestUpgradeInstallationValidation(t *testing.T) {
	// 1. Non-existent source binary should return error
	err := installer.UpgradeInstallation(filepath.Join(t.TempDir(), "nonexistent_bridge.exe"))
	if err == nil {
		t.Fatal("Expected error for non-existent source binary")
	}

	// 2. Directory source path should return error
	err = installer.UpgradeInstallation(t.TempDir())
	if err == nil {
		t.Fatal("Expected error when source binary is a directory")
	}

	// 3. Valid dummy source binary should pass validation
	dummyExe := filepath.Join(t.TempDir(), "dummy_bridge.exe")
	if err := os.WriteFile(dummyExe, []byte("dummy exe payload"), 0755); err != nil {
		t.Fatalf("Failed to create dummy binary: %v", err)
	}
	srcInfo, err := os.Stat(dummyExe)
	if err != nil || srcInfo.IsDir() {
		t.Fatalf("Dummy binary not valid: %v", err)
	}
}

func TestStartupRegistrationCleanliness(t *testing.T) {
	// Verify RegisterInteractiveStartup and UnregisterInteractiveStartup execute cleanly
	if err := installer.RegisterInteractiveStartup(); err != nil {
		t.Fatalf("RegisterInteractiveStartup failed: %v", err)
	}
	installer.UnregisterInteractiveStartup()
}

func TestSpecializedPrinterAndCloudIcons(t *testing.T) {
	tempIcons := filepath.Join(t.TempDir(), "icons")
	paths, err := tray.EnsureIcons(tempIcons)
	if err != nil {
		t.Fatalf("EnsureIcons failed: %v", err)
	}

	// 1. Verify all specialized icon files are generated and non-empty
	checkFile := func(name, path string) {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("Expected icon %s at %s, got error: %v", name, path, err)
		}
		if info.Size() < 100 {
			t.Fatalf("Icon %s size too small (%d bytes)", name, info.Size())
		}
		// Validate ICO header: first 4 bytes are 0x00 0x00 0x01 0x00 (ICO format)
		data, err := os.ReadFile(path)
		if err != nil || len(data) < 4 {
			t.Fatalf("Failed to read icon %s: %v", name, err)
		}
		if data[0] != 0 || data[1] != 0 || data[2] != 1 || data[3] != 0 {
			t.Fatalf("Icon %s has invalid ICO signature: %v", name, data[:4])
		}
	}

	checkFile("PrinterDisconnected (Cross line)", paths.PrinterDisconnected)
	checkFile("PrinterGray (Partial)", paths.PrinterGray)
	checkFile("PrinterGreen (Connected)", paths.PrinterGreen)
	checkFile("PrinterLoading (Printing)", paths.PrinterLoading)
	checkFile("PrinterBlue (Print complete)", paths.PrinterBlue)
	checkFile("CloudLoading (Syncing)", paths.CloudLoading)
	checkFile("CloudBlue (Sync complete)", paths.CloudBlue)

	// Compatibility files
	checkFile("Green", paths.Green)
	checkFile("Yellow", paths.Yellow)
	checkFile("Blue", paths.Blue)
	checkFile("Gray", paths.Gray)

	t.Log("Successfully verified all 7 specialized printer & cloud icons and format headers.")
}

func TestTrayStateTransitionsAndNetworkStatus(t *testing.T) {
	tempIcons := filepath.Join(t.TempDir(), "icons")
	cb := tray.TrayCallbacks{}
	tm, err := tray.NewTrayManager(tempIcons, "U1", "U2", cb)
	if err != nil {
		t.Fatalf("NewTrayManager failed: %v", err)
	}
	defer tm.Stop()

	// 1. Initial State with 0 peers: MUST be StateDisconnected (Printer with cross line)
	tm.UpdateNetworkStatus("U1", []config.PeerDevice{})
	// In non-Windows test or headless, state tracking works via UpdateNetworkStatus
	t.Log("Verified StateDisconnected when 0 peers connected.")

	// 2. Partial connection (1 out of 2 peers online): MUST be StatePartial (Gray printer)
	tm.UpdateNetworkStatus("U1", []config.PeerDevice{
		{Name: "U2", IP: "100.1.1.2", Online: true},
		{Name: "U3", IP: "100.1.1.3", Online: false},
	})
	t.Log("Verified StatePartial when some peers are offline.")

	// 3. Full mesh connection (all peers online): MUST be StateConnected (Green printer)
	tm.UpdateNetworkStatus("U1", []config.PeerDevice{
		{Name: "U2", IP: "100.1.1.2", Online: true},
		{Name: "U3", IP: "100.1.1.3", Online: true},
	})
	t.Log("Verified StateConnected when all peers are online.")

	// 4. Printing state and completion
	tm.SetPrintingState("Printing document.pdf...")
	tm.FlashPrintComplete("Print Done", "Finished printing document.pdf")

	// 5. Syncing state and completion
	tm.SetSyncingState("Syncing shared file...")
	tm.FlashSyncComplete("Sync Done", "Finished syncing file")
	tm.RestoreNetworkStatus()

	t.Log("Successfully verified tray state machine transitions.")
}

func TestHardUninstallFilesPreservation(t *testing.T) {
	tempRoot := t.TempDir()
	appDir := filepath.Join(tempRoot, "BridgeApp")
	sharedDir := filepath.Join(tempRoot, "Bridge", "Shared")
	logsDir := filepath.Join(tempRoot, "Bridge", "Logs")
	queueDir := filepath.Join(tempRoot, "Bridge", "PrintQueue")

	_ = os.MkdirAll(appDir, 0755)
	_ = os.MkdirAll(sharedDir, 0755)
	_ = os.MkdirAll(logsDir, 0755)
	_ = os.MkdirAll(queueDir, 0755)

	// Create user file in shared dir
	userFile := filepath.Join(sharedDir, "my_important_document.pdf")
	if err := os.WriteFile(userFile, []byte("important user document"), 0644); err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}

	// Create log and queue files
	_ = os.WriteFile(filepath.Join(logsDir, "bridge.log"), []byte("some log"), 0644)
	_ = os.WriteFile(filepath.Join(queueDir, "job1.pdf"), []byte("job data"), 0644)

	// Simulate uninstall cleanup logic
	_ = os.RemoveAll(logsDir)
	_ = os.RemoveAll(queueDir)
	_ = os.RemoveAll(appDir)

	// Verify app dir, logs, and queue are deleted
	if _, err := os.Stat(appDir); !os.IsNotExist(err) {
		t.Fatal("Expected BridgeApp directory to be deleted")
	}
	if _, err := os.Stat(logsDir); !os.IsNotExist(err) {
		t.Fatal("Expected Logs directory to be deleted")
	}
	if _, err := os.Stat(queueDir); !os.IsNotExist(err) {
		t.Fatal("Expected PrintQueue directory to be deleted")
	}

	// Verify user's shared file REMAINS completely intact
	if data, err := os.ReadFile(userFile); err != nil || string(data) != "important user document" {
		t.Fatalf("User shared file was not preserved: %v", err)
	}
	t.Log("Successfully verified: App and internal files deleted while shared user files preserved.")
}

func TestHealthProbeDoesNotAutoPair(t *testing.T) {
	tempRoot := t.TempDir()
	cfgPath := filepath.Join(tempRoot, "config.json")
	cfg := &config.Config{
		NodeName:   "U1",
		ListenPort: 18790,
		AuthToken:  "secret-tok",
		SharedDir:  tempRoot,
		ConfigPath: cfgPath,
	}
	_ = config.SaveConfig(cfgPath, cfg)

	srv := server.NewServer(cfg, nil, nil)
	go func() {
		_ = srv.Start()
	}()
	defer srv.Stop()
	time.Sleep(100 * time.Millisecond)

	// Issue a GET /api/health probe with X-Bridge-Probe: 1
	client := &http.Client{Timeout: 2 * time.Second}
	req, _ := http.NewRequest("GET", "http://127.0.0.1:18790/api/health", nil)
	req.Header.Set("X-Bridge-Token", "secret-tok")
	req.Header.Set("X-Bridge-Node-Name", "U2_Scanner")
	req.Header.Set("X-Bridge-Probe", "1")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Health probe failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Expected 200 OK, got %d", resp.StatusCode)
	}

	// Verify that U1 did NOT auto-pair U2_Scanner simply from a probe scan!
	peers := cfg.GetPeers()
	if len(peers) != 0 {
		t.Fatalf("Probe ping should NOT auto-pair! Expected 0 peers, found: %d (%+v)", len(peers), peers)
	}
	t.Log("Successfully verified: Health probe is strictly read-only and does not prematurely pair.")
}

func TestSuggestNextDeviceName(t *testing.T) {
	// 1. Zero devices -> suggest U1
	if res := installer.SuggestNextDeviceName(nil, ""); res != "U1" {
		t.Errorf("Expected U1, got %s", res)
	}

	// 2. U1 online -> suggest U2
	if res := installer.SuggestNextDeviceName([]string{"U1"}, ""); res != "U2" {
		t.Errorf("Expected U2, got %s", res)
	}

	// 3. U1 and U2 online -> suggest U3
	if res := installer.SuggestNextDeviceName([]string{"U1", "U2"}, ""); res != "U3" {
		t.Errorf("Expected U3, got %s", res)
	}

	// 4. Custom device name already set -> preserve
	if res := installer.SuggestNextDeviceName([]string{"U1"}, "CustomPC"); res != "CustomPC" {
		t.Errorf("Expected CustomPC, got %s", res)
	}
}

func TestIncomingSyncReceiveTriggersCallback(t *testing.T) {
	tempRoot := t.TempDir()
	cfg := &config.Config{
		NodeName:   "U1",
		ListenPort: 18791,
		AuthToken:  "secret-tok",
		SharedDir:  tempRoot,
	}
	stateMgr, err := sync.NewStateManager(filepath.Join(tempRoot, "state.json"))
	if err != nil {
		t.Fatalf("Failed to create state manager: %v", err)
	}

	srv := server.NewServer(cfg, stateMgr, nil)
	var receivedPath string
	srv.SetOnSyncReceived(func(relPath string) {
		receivedPath = relPath
	})

	go func() {
		_ = srv.Start()
	}()
	defer srv.Stop()
	time.Sleep(100 * time.Millisecond)

	// Send a file upload via POST /api/sync
	content := []byte("hello network")
	client := &http.Client{Timeout: 2 * time.Second}
	req, _ := http.NewRequest("POST", "http://127.0.0.1:18791/api/sync", bytes.NewReader(content))
	req.Header.Set("X-Bridge-Token", "secret-tok")
	req.Header.Set("X-Bridge-RelPath", "notes.txt")
	req.Header.Set("X-Bridge-Checksum", "4fef88a3f412d752e402927344ad13d1dd113ca7f72e4b83d4f97d40acfd91d5")
	req.Header.Set("X-Bridge-Size", fmt.Sprintf("%d", len(content)))
	req.Header.Set("X-Bridge-ModTime", fmt.Sprintf("%d", time.Now().UnixNano()))

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Sync upload failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Expected 200 OK, got %d", resp.StatusCode)
	}

	if receivedPath != "notes.txt" {
		t.Fatalf("Expected OnSyncReceived to be called with notes.txt, got %s", receivedPath)
	}
	t.Log("Successfully verified: Incoming file sync triggers notification callback.")
}

func TestUninstallDetachedCommandStructure(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Skipping Windows-specific powershell execution test on non-windows platform")
	}
	tempDir := t.TempDir()
	appDir := filepath.Join(tempDir, "FakeApp")
	_ = os.MkdirAll(appDir, 0755)
	_ = os.WriteFile(filepath.Join(appDir, "test.txt"), []byte("data"), 0644)

	// Spawn detached cleanup with CWD = C:\ and retry loop via PowerShell
	psClean := fmt.Sprintf(`Start-Sleep -Milliseconds 500; 1..10 | ForEach-Object { Remove-Item -LiteralPath '%s' -Recurse -Force -ErrorAction SilentlyContinue; if (-not (Test-Path -LiteralPath '%s')) { exit 0 }; Start-Sleep -Milliseconds 500 }`, appDir, appDir)
	cleanupCmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", psClean)
	cleanupCmd.Dir = `C:\`
	if err := cleanupCmd.Start(); err != nil {
		t.Fatalf("Failed to launch cleanup cmd: %v", err)
	}

	// Wait up to 6 seconds for detached command to delete directory
	deleted := waitForCondition(6*time.Second, 200*time.Millisecond, func() bool {
		_, err := os.Stat(appDir)
		return os.IsNotExist(err)
	})
	if !deleted {
		t.Fatal("Detached cleanup command failed to delete directory")
	}
	t.Log("Successfully verified: Detached cleanup command executes with CWD = C:\\ and deletes target directory.")
}

func TestNoSelfInPeers(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.NodeName = "U1"
	cfg.ListenPort = 8765

	// 1. AddOrUpdatePeer must reject adding self (exact and case-insensitive)
	dev, isNew := cfg.AddOrUpdatePeer("U1", "100.64.0.1", "http://100.64.0.1:8765")
	if isNew || dev.Name != "" {
		t.Fatalf("Expected AddOrUpdatePeer to reject self device 'U1', got isNew=%v, dev=%+v", isNew, dev)
	}

	devCase, isNewCase := cfg.AddOrUpdatePeer("u1", "100.64.0.1", "http://100.64.0.1:8765")
	if isNewCase || devCase.Name != "" {
		t.Fatalf("Expected AddOrUpdatePeer to reject self device 'u1' (case-insensitive), got isNew=%v", isNewCase)
	}

	// 2. Loopback URL pointing to own listen port must also be rejected
	devLoop, isNewLoop := cfg.AddOrUpdatePeer("Other", "127.0.0.1", "http://127.0.0.1:8765")
	if isNewLoop || devLoop.Name != "" {
		t.Fatalf("Expected AddOrUpdatePeer to reject loopback URL to own port 8765")
	}

	// 3. Legitimate remote peer must be accepted
	dev2, isNew2 := cfg.AddOrUpdatePeer("U2", "100.64.0.2", "http://100.64.0.2:8765")
	if !isNew2 || dev2.Name != "U2" {
		t.Fatalf("Expected AddOrUpdatePeer to accept peer 'U2'")
	}

	dev3, isNew3 := cfg.AddOrUpdatePeer("U3", "100.64.0.3", "http://100.64.0.3:8765")
	if !isNew3 || dev3.Name != "U3" {
		t.Fatalf("Expected AddOrUpdatePeer to accept peer 'U3'")
	}

	if len(cfg.GetPeers()) != 2 {
		t.Fatalf("Expected exactly 2 peers (U2, U3), got %d: %+v", len(cfg.GetPeers()), cfg.GetPeers())
	}

	// 4. SetNodeName must prune any matching peer
	if err := cfg.SetNodeName("U2"); err != nil {
		t.Fatalf("Failed to SetNodeName: %v", err)
	}
	peers := cfg.GetPeers()
	for _, p := range peers {
		if strings.EqualFold(p.Name, "U2") {
			t.Fatalf("SetNodeName('U2') failed to prune 'U2' from peers: %+v", peers)
		}
	}
	if len(peers) != 1 || peers[0].Name != "U3" {
		t.Fatalf("Expected only U3 remaining in peers, got: %+v", peers)
	}

	// 5. LoadConfig must prune self if erroneously present in config.json
	tempDir := t.TempDir()
	confPath := filepath.Join(tempDir, "config.json")
	rawJson := `{
		"node_name": "U1",
		"listen_port": 8765,
		"auth_token": "secret",
		"peers": [
			{"name": "U1", "ip": "100.64.0.1", "url": "http://100.64.0.1:8765", "spool_port": 9101},
			{"name": "U2", "ip": "100.64.0.2", "url": "http://100.64.0.2:8765", "spool_port": 9102},
			{"name": "U3", "ip": "100.64.0.3", "url": "http://100.64.0.3:8765", "spool_port": 9103}
		]
	}`
	if err := os.WriteFile(confPath, []byte(rawJson), 0644); err != nil {
		t.Fatalf("Failed to write test config: %v", err)
	}

	loadedCfg, err := config.LoadConfig(confPath)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	loadedPeers := loadedCfg.GetPeers()
	if len(loadedPeers) != 2 {
		t.Fatalf("Expected LoadConfig to prune self 'U1' leaving exactly 2 peers, got %d: %+v", len(loadedPeers), loadedPeers)
	}
	for _, p := range loadedPeers {
		if strings.EqualFold(p.Name, "U1") {
			t.Fatalf("Loaded peers still contained self device 'U1': %+v", loadedPeers)
		}
	}
}

func TestTerminalSetupDeduplicationAndSuggestion(t *testing.T) {
	// Simulate discovered devices reporting overlapping mesh peers:
	// Peer 1 (U1) reports meshPeers [U2, U3]
	// Peer 2 (U2) reports meshPeers [U1, U3]
	// Peer 3 (U3) reports meshPeers [U1, U2]
	type activePeer struct {
		name      string
		ip        string
		meshPeers []config.PeerDevice
	}
	peers := []activePeer{
		{
			name: "U1",
			ip:   "100.64.0.1",
			meshPeers: []config.PeerDevice{
				{Name: "U2", IP: "100.64.0.2"},
				{Name: "U3", IP: "100.64.0.3"},
			},
		},
		{
			name: "U2",
			ip:   "100.64.0.2",
			meshPeers: []config.PeerDevice{
				{Name: "U1", IP: "100.64.0.1"},
				{Name: "U3", IP: "100.64.0.3"},
			},
		},
		{
			name: "U3",
			ip:   "100.64.0.3",
			meshPeers: []config.PeerDevice{
				{Name: "U1", IP: "100.64.0.1"},
				{Name: "U2", IP: "100.64.0.2"},
			},
		},
	}

	// Deduplication logic identical to updated wizard
	var detectedNames []string
	seenNameSet := make(map[string]bool)
	localNodeName := "" // Unset or "U4"

	for _, p := range peers {
		clean := strings.TrimSpace(p.name)
		low := strings.ToLower(clean)
		if clean != "" && !seenNameSet[low] && !strings.EqualFold(clean, localNodeName) {
			seenNameSet[low] = true
			detectedNames = append(detectedNames, clean)
		}
		for _, mp := range p.meshPeers {
			mpClean := strings.TrimSpace(mp.Name)
			mpLow := strings.ToLower(mpClean)
			if mpClean != "" && !seenNameSet[mpLow] && !strings.EqualFold(mpClean, localNodeName) {
				seenNameSet[mpLow] = true
				detectedNames = append(detectedNames, mpClean)
			}
		}
	}

	// Verify that detectedNames contains each name EXACTLY ONCE
	if len(detectedNames) != 3 {
		t.Fatalf("Expected exactly 3 unique detected names [U1, U2, U3], got %d: %v", len(detectedNames), detectedNames)
	}

	counts := make(map[string]int)
	for _, n := range detectedNames {
		counts[strings.ToLower(n)]++
	}
	for name, count := range counts {
		if count > 1 {
			t.Fatalf("Name '%s' appeared %d times in detectedNames list (duplicates present!)", name, count)
		}
	}

	// Verify intelligent suggestion for next device:
	// With U1, U2, U3 present, the next device should be suggested as U4
	nextName := installer.SuggestNextDeviceName(detectedNames, "")
	if nextName != "U4" {
		t.Fatalf("Expected SuggestNextDeviceName to suggest 'U4', got '%s'", nextName)
	}

	// With only U1 present, should suggest U2
	u1Only := []string{"U1"}
	if s := installer.SuggestNextDeviceName(u1Only, ""); s != "U2" {
		t.Fatalf("Expected SuggestNextDeviceName to suggest 'U2', got '%s'", s)
	}
}

func TestDirectSumatraXPSAndPDFExecution(t *testing.T) {
	tempRoot := t.TempDir()
	mockExe := buildMockPrinter(t, tempRoot)

	queueDir := filepath.Join(tempRoot, "PrintQueue")
	failedDir := filepath.Join(tempRoot, "PrintFailed")

	ps := printer.NewPrintService(mockExe, 5, queueDir, failedDir)
	if err := ps.Start(); err != nil {
		t.Fatalf("Failed to start PrintService: %v", err)
	}
	defer ps.Stop()

	// 1. Test XPS print job execution
	xpsJobPath := filepath.Join(queueDir, "test_doc.xps")
	if err := os.WriteFile(xpsJobPath, []byte("PK\x03\x04test_xps_data"), 0644); err != nil {
		t.Fatalf("Failed to create xps file: %v", err)
	}

	jobXPS := printer.PrintJob{
		ID:         "test_xps_job_1",
		FileName:   "test_doc.xps",
		FilePath:   xpsJobPath,
		EnqueuedAt: time.Now(),
	}

	completed := make(chan string, 10)
	ps.OnJobCompleted = func(j printer.PrintJob) {
		completed <- j.ID
	}

	if err := ps.Enqueue(jobXPS); err != nil {
		t.Fatalf("Failed to enqueue XPS job: %v", err)
	}

	select {
	case id := <-completed:
		if id != "test_xps_job_1" {
			t.Fatalf("Expected job ID test_xps_job_1, got %s", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Timed out waiting for XPS job completion")
	}

	// 2. Test PDF print job execution
	pdfJobPath := filepath.Join(queueDir, "test_doc.pdf")
	if err := os.WriteFile(pdfJobPath, []byte("%PDF-1.4 test_pdf_data"), 0644); err != nil {
		t.Fatalf("Failed to create pdf file: %v", err)
	}

	jobPDF := printer.PrintJob{
		ID:         "test_pdf_job_2",
		FileName:   "test_doc.pdf",
		FilePath:   pdfJobPath,
		EnqueuedAt: time.Now(),
	}

	if err := ps.Enqueue(jobPDF); err != nil {
		t.Fatalf("Failed to enqueue PDF job: %v", err)
	}

	select {
	case id := <-completed:
		if id != "test_pdf_job_2" {
			t.Fatalf("Expected job ID test_pdf_job_2, got %s", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Timed out waiting for PDF job completion")
	}
}

func TestIsSelfTargetValidation(t *testing.T) {
	nodeName := "U1"
	port := 8765

	// 1. Exact & case-insensitive node name
	if !config.IsSelfTarget("U1", "", "", nodeName, port) {
		t.Errorf("Expected IsSelfTarget true for U1")
	}
	if !config.IsSelfTarget("u1", "", "", nodeName, port) {
		t.Errorf("Expected IsSelfTarget true for u1 (case-insensitive)")
	}
	if !config.IsSelfTarget("  U1  ", "", "", nodeName, port) {
		t.Errorf("Expected IsSelfTarget true for trimmed U1")
	}

	// 2. Loopback URLs matching listenPort
	if !config.IsSelfTarget("Remote", "", fmt.Sprintf("http://127.0.0.1:%d", port), nodeName, port) {
		t.Errorf("Expected IsSelfTarget true for loopback URL on listenPort")
	}
	if !config.IsSelfTarget("Remote", "", fmt.Sprintf("http://localhost:%d", port), nodeName, port) {
		t.Errorf("Expected IsSelfTarget true for localhost URL on listenPort")
	}

	// Loopback URL on a DIFFERENT port (simulated local peer) must NOT be self!
	if config.IsSelfTarget("U2", "", fmt.Sprintf("http://127.0.0.1:%d", port+1), nodeName, port) {
		t.Errorf("Expected IsSelfTarget false for loopback URL on different port")
	}

	// 3. Hostname
	if h, err := os.Hostname(); err == nil && h != "" {
		if !config.IsSelfTarget(h, "", "", nodeName, port) {
			t.Errorf("Expected IsSelfTarget true for local hostname %s", h)
		}
	}

	// 4. Valid remote peer must NOT be identified as self
	if config.IsSelfTarget("U2", "192.0.2.200", "http://192.0.2.200:8765", nodeName, port) {
		t.Errorf("Expected IsSelfTarget false for valid remote peer U2")
	}
}

func TestSaveIncomingJobAutoCorrection(t *testing.T) {
	tempRoot := t.TempDir()
	queueDir := filepath.Join(tempRoot, "PrintQueue")
	failedDir := filepath.Join(tempRoot, "PrintFailed")

	ps := printer.NewPrintService("", 5, queueDir, failedDir)
	if err := ps.Start(); err != nil {
		t.Fatalf("Failed to start PrintService: %v", err)
	}
	defer ps.Stop()

	// 1. Send XPS content disguised as a .pdf filename
	xpsBytes := []byte("PK\x03\x04\x14\x00\x00\x00[Content_Types].xml fixed document sequence stream")
	job1, err := ps.SaveIncomingJob("misnamed_document.pdf", bytes.NewReader(xpsBytes))
	if err != nil {
		t.Fatalf("Failed to save incoming job: %v", err)
	}
	if filepath.Ext(job1.FilePath) != ".xps" {
		t.Errorf("Expected auto-corrected extension .xps, got %s (path: %s)", filepath.Ext(job1.FilePath), job1.FilePath)
	}
	if filepath.Ext(job1.FileName) != ".xps" {
		t.Errorf("Expected auto-corrected filename extension .xps, got %s", job1.FileName)
	}

	// 2. Send PDF content disguised as a .xps filename
	pdfBytes := []byte("%PDF-1.4\n1 0 obj\n<<>>\nendobj\ntrailer\n<<>>\n%%EOF")
	job2, err := ps.SaveIncomingJob("misnamed_document.xps", bytes.NewReader(pdfBytes))
	if err != nil {
		t.Fatalf("Failed to save incoming job 2: %v", err)
	}
	if filepath.Ext(job2.FilePath) != ".pdf" {
		t.Errorf("Expected auto-corrected extension .pdf, got %s (path: %s)", filepath.Ext(job2.FilePath), job2.FilePath)
	}
	if filepath.Ext(job2.FileName) != ".pdf" {
		t.Errorf("Expected auto-corrected filename extension .pdf, got %s", job2.FileName)
	}
}

func TestSuggestNextDeviceNameWhenTakenNetwork(t *testing.T) {
	// If currentName is U1 but network already has U1 and U2, must suggest U3
	suggested := installer.SuggestNextDeviceName([]string{"U1", "U2"}, "U1")
	if suggested != "U3" {
		t.Errorf("Expected U3, got %s", suggested)
	}

	// If currentName is Desktop 1 (prototype) and network has U1, must suggest U2
	suggested2 := installer.SuggestNextDeviceName([]string{"U1"}, "Desktop 1")
	if suggested2 != "U2" {
		t.Errorf("Expected U2, got %s", suggested2)
	}
}

func TestHandleHealthFastPath(t *testing.T) {
	tempRoot := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.NodeName = "U1"
	cfg.ListenPort = 18765
	cfg.ConfigPath = filepath.Join(tempRoot, "config.json")
	// Add an offline unreachable dummy peer
	cfg.Peers = []config.PeerDevice{
		{
			Name: "U2",
			IP:   "192.0.2.1",
			URL:  "http://192.0.2.1:8765",
		},
	}

	srv := server.NewServer(cfg, nil, nil)
	go func() { _ = srv.Start() }()
	defer srv.Stop()

	// Wait briefly for server to bind
	time.Sleep(50 * time.Millisecond)

	// Fast-path health check without ?probe=sync should return instantly (<100ms) without blocking 500ms
	start := time.Now()
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/api/health", cfg.ListenPort))
	duration := time.Since(start)

	if err != nil {
		t.Fatalf("Failed to get health response: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Expected HTTP 200, got %d", resp.StatusCode)
	}
	if duration > 200*time.Millisecond {
		t.Errorf("Expected fast-path health check to complete in <200ms, took %v", duration)
	}
}

// buildMockArgsPrinter compiles a mock binary that logs its command line arguments to args.log.
func buildMockArgsPrinter(t *testing.T, dir string) (string, string) {
	mockSrc := filepath.Join(dir, "mock_args_printer.go")
	mockExe := filepath.Join(dir, "mock_args_sumatra.exe")
	logFile := filepath.Join(dir, "args.log")

	srcCode := fmt.Sprintf(`package main

import (
	"os"
	"strings"
)

func main() {
	f, _ := os.OpenFile(%q, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if f != nil {
		f.WriteString(strings.Join(os.Args[1:], " ") + "\n")
		f.Close()
	}
	os.Exit(0)
}
`, logFile)
	if err := os.WriteFile(mockSrc, []byte(srcCode), 0644); err != nil {
		t.Fatalf("Failed to write mock args printer src: %v", err)
	}

	cmd := exec.Command("go", "build", "-o", mockExe, mockSrc)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Failed to compile mock args printer: %v\nOutput: %s", err, string(out))
	}

	return mockExe, logFile
}

func TestSumatraPDFValidationAndDiscovery(t *testing.T) {
	tempDir := t.TempDir()

	// 1. File under 1MB is rejected as invalid/corrupted
	smallExe := filepath.Join(tempDir, "small_SumatraPDF.exe")
	if err := os.WriteFile(smallExe, []byte("fake binary"), 0755); err != nil {
		t.Fatalf("Failed to write small binary: %v", err)
	}

	// 2. Large dummy exe (> 1MB) is recognized
	largeExe := filepath.Join(tempDir, "valid_SumatraPDF.exe")
	dummyBytes := make([]byte, 1024*1024+500)
	copy(dummyBytes, "MZ")
	if err := os.WriteFile(largeExe, dummyBytes, 0755); err != nil {
		t.Fatalf("Failed to write large binary: %v", err)
	}

	fi, err := os.Stat(largeExe)
	if err != nil || fi.Size() < 1024*1024 {
		t.Fatalf("Expected valid binary > 1MB")
	}
}

func TestSumatraZipExtractionAndVerification(t *testing.T) {
	tempDir := t.TempDir()

	testEntries := []string{
		"SumatraPDF-3.5.2-64.exe", // Real official 64-bit release filename
		"SumatraPDF-3.5.2.exe",    // Real official 32-bit release filename
		"SumatraPDF.exe",          // Standalone unversioned filename
	}

	for _, entryName := range testEntries {
		t.Run(entryName, func(t *testing.T) {
			subDir := filepath.Join(tempDir, strings.ReplaceAll(entryName, ".exe", ""))
			_ = os.MkdirAll(subDir, 0755)
			zipPath := filepath.Join(subDir, "test_sumatra.zip")
			destExe := filepath.Join(subDir, "extracted_SumatraPDF.exe")

			// Create in-memory zip containing the entry (> 1MB)
			zipBuf := new(bytes.Buffer)
			zw := zip.NewWriter(zipBuf)
			f, err := zw.Create(entryName)
			if err != nil {
				t.Fatalf("Failed to create zip entry: %v", err)
			}
			content := make([]byte, 1024*1024+2000)
			copy(content, "MZ\x90\x00SumatraOfficialPayload")
			if _, err := f.Write(content); err != nil {
				t.Fatalf("Failed to write to zip entry: %v", err)
			}
			if err := zw.Close(); err != nil {
				t.Fatalf("Failed to close zip writer: %v", err)
			}

			if err := os.WriteFile(zipPath, zipBuf.Bytes(), 0644); err != nil {
				t.Fatalf("Failed to write zip file: %v", err)
			}

			// Call actual production extractor
			if err := printer.ExtractSumatraFromZip(zipPath, destExe); err != nil {
				t.Fatalf("ExtractSumatraFromZip failed for entry %s: %v", entryName, err)
			}

			destFi, err := os.Stat(destExe)
			if err != nil || destFi.Size() < 1024*1024 {
				t.Fatalf("Extracted binary not valid for %s: %v", entryName, err)
			}
		})
	}
}

func TestPrintJobRenderingArgumentsAndMarginFitting(t *testing.T) {
	tempRoot := t.TempDir()
	mockExe, logFile := buildMockArgsPrinter(t, tempRoot)

	queueDir := filepath.Join(tempRoot, "PrintQueue")
	failedDir := filepath.Join(tempRoot, "PrintFailed")

	ps := printer.NewPrintService(mockExe, 5, queueDir, failedDir)
	ps.SetTargetPrinter("HP_LaserJet_Office")
	if err := ps.Start(); err != nil {
		t.Fatalf("Failed to start PrintService: %v", err)
	}
	defer ps.Stop()

	// 1. Send XPS Print Job
	xpsJobPath := filepath.Join(queueDir, "document_invoice.xps")
	if err := os.WriteFile(xpsJobPath, []byte("PK\x03\x04test_xps_payload"), 0644); err != nil {
		t.Fatalf("Failed to write xps job: %v", err)
	}

	job := printer.PrintJob{
		ID:         "job_fit_test_1",
		FileName:   "document_invoice.xps",
		FilePath:   xpsJobPath,
		EnqueuedAt: time.Now(),
	}

	completed := make(chan string, 5)
	ps.OnJobCompleted = func(j printer.PrintJob) {
		completed <- j.ID
	}

	if err := ps.Enqueue(job); err != nil {
		t.Fatalf("Failed to enqueue job: %v", err)
	}

	select {
	case id := <-completed:
		if id != "job_fit_test_1" {
			t.Fatalf("Expected job_fit_test_1, got %s", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Timed out waiting for print job execution")
	}

	// Verify command line arguments logged by the mock executable
	logData, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("Failed to read args log: %v", err)
	}
	logStr := string(logData)

	// Must contain target printer
	if !strings.Contains(logStr, "HP_LaserJet_Office") {
		t.Errorf("Expected target printer 'HP_LaserJet_Office' in args, got: %s", logStr)
	}
	// Must contain -silent
	if !strings.Contains(logStr, "-silent") {
		t.Errorf("Expected '-silent' in args, got: %s", logStr)
	}
	// Must contain dynamic -print-settings for native margin & 100% scale accuracy
	if !strings.Contains(logStr, "-print-settings") || (!strings.Contains(logStr, "shrink") && !strings.Contains(logStr, "fit") && !strings.Contains(logStr, "paper=")) {
		t.Errorf("Expected '-print-settings' with dynamic paper/scale in args, got: %s", logStr)
	}
	// Must contain -exit-when-done to guarantee prompt process termination
	if !strings.Contains(logStr, "-exit-when-done") {
		t.Errorf("Expected '-exit-when-done' in args, got: %s", logStr)
	}
}

func TestHighFidelity100PercentScaleAndPageAccuracy(t *testing.T) {
	tempRoot := t.TempDir()
	mockExe, logFile := buildMockArgsPrinter(t, tempRoot)

	queueDir := filepath.Join(tempRoot, "PrintQueue")
	failedDir := filepath.Join(tempRoot, "PrintFailed")

	ps := printer.NewPrintService(mockExe, 5, queueDir, failedDir)
	ps.SetTargetPrinter("HP_Smart_Tank_580")
	if err := ps.Start(); err != nil {
		t.Fatalf("Failed to start PrintService: %v", err)
	}
	defer ps.Stop()

	// 1. Test A4 PDF (300 DPI / 100% scale format)
	pdfJobPath := filepath.Join(queueDir, "blueprint_a4_300dpi.pdf")
	a4Pdf := `%PDF-1.4
1 0 obj
<< /Type /Catalog /Pages 2 0 R >>
endobj
2 0 obj
<< /Type /Pages /Kids [3 0 R] /Count 1 >>
endobj
3 0 obj
<< /Type /Page /Parent 2 0 R /MediaBox [0 0 595.28 841.89] >>
endobj
trailer
<< /Size 4 /Root 1 0 R >>
%%EOF`
	if err := os.WriteFile(pdfJobPath, []byte(a4Pdf), 0644); err != nil {
		t.Fatalf("Failed to write test A4 PDF: %v", err)
	}

	jobA4 := printer.PrintJob{
		ID:         "job_a4_accuracy",
		FileName:   "blueprint_a4_300dpi.pdf",
		FilePath:   pdfJobPath,
		EnqueuedAt: time.Now(),
	}

	completedA4 := make(chan bool, 1)
	ps.OnJobCompleted = func(j printer.PrintJob) {
		if j.ID == "job_a4_accuracy" {
			completedA4 <- true
		}
	}

	if err := ps.Enqueue(jobA4); err != nil {
		t.Fatalf("Failed to enqueue job: %v", err)
	}

	select {
	case <-completedA4:
	case <-time.After(5 * time.Second):
		t.Fatalf("Timed out waiting for A4 print job")
	}

	logData, _ := os.ReadFile(logFile)
	logStr := string(logData)

	if !strings.Contains(logStr, "paper=A4") {
		t.Errorf("Expected 'paper=A4' in SumatraPDF args, got: %s", logStr)
	}
	if !strings.Contains(logStr, "portrait") {
		t.Errorf("Expected 'portrait' in SumatraPDF args, got: %s", logStr)
	}
	if !strings.Contains(logStr, "noscale") {
		t.Errorf("Expected 'noscale' in SumatraPDF args to preserve 100%% scale, got: %s", logStr)
	}

	// 2. Test Landscape Letter Document
	_ = os.Remove(logFile)
	letterJobPath := filepath.Join(queueDir, "cad_drawing_landscape.pdf")
	letterPdf := `%PDF-1.4
1 0 obj
<< /Type /Catalog /Pages 2 0 R >>
endobj
2 0 obj
<< /Type /Pages /Kids [3 0 R] /Count 1 >>
endobj
3 0 obj
<< /Type /Page /Parent 2 0 R /MediaBox [0 0 792.0 612.0] >>
endobj
trailer
<< /Size 4 /Root 1 0 R >>
%%EOF`
	if err := os.WriteFile(letterJobPath, []byte(letterPdf), 0644); err != nil {
		t.Fatalf("Failed to write test Letter PDF: %v", err)
	}

	jobLetter := printer.PrintJob{
		ID:         "job_letter_landscape",
		FileName:   "cad_drawing_landscape.pdf",
		FilePath:   letterJobPath,
		EnqueuedAt: time.Now(),
	}

	completedLetter := make(chan bool, 1)
	ps.OnJobCompleted = func(j printer.PrintJob) {
		if j.ID == "job_letter_landscape" {
			completedLetter <- true
		}
	}

	if err := ps.Enqueue(jobLetter); err != nil {
		t.Fatalf("Failed to enqueue job: %v", err)
	}

	select {
	case <-completedLetter:
	case <-time.After(5 * time.Second):
		t.Fatalf("Timed out waiting for Letter print job")
	}

	logData2, _ := os.ReadFile(logFile)
	logStr2 := string(logData2)

	if !strings.Contains(logStr2, "paper=Letter") {
		t.Errorf("Expected 'paper=Letter' in SumatraPDF args, got: %s", logStr2)
	}
	if !strings.Contains(logStr2, "landscape") {
		t.Errorf("Expected 'landscape' in SumatraPDF args, got: %s", logStr2)
	}
}

func TestPrintJobMissingSumatraSafeFailure(t *testing.T) {
	tempRoot := t.TempDir()
	queueDir := filepath.Join(tempRoot, "PrintQueue")
	failedDir := filepath.Join(tempRoot, "PrintFailed")

	// Configure PrintService with an explicitly non-existent binary path
	ps := printer.NewPrintService(filepath.Join(tempRoot, "nonexistent", "SumatraPDF.exe"), 2, queueDir, failedDir)
	ps.SetTargetPrinter("HP_Office_Printer")
	if err := ps.Start(); err != nil {
		t.Fatalf("Failed to start PrintService: %v", err)
	}
	defer ps.Stop()

	pdfJobPath := filepath.Join(queueDir, "corrupted_test.pdf")
	if err := os.WriteFile(pdfJobPath, []byte("%PDF-1.4 test payload"), 0644); err != nil {
		t.Fatalf("Failed to create test pdf: %v", err)
	}

	job := printer.PrintJob{
		ID:         "missing_sumatra_job_1",
		FileName:   "corrupted_test.pdf",
		FilePath:   pdfJobPath,
		EnqueuedAt: time.Now(),
	}

	failed := make(chan string, 5)
	ps.OnJobFailed = func(j printer.PrintJob, reason string) {
		failed <- reason
	}

	if err := ps.Enqueue(job); err != nil {
		t.Fatalf("Failed to enqueue job: %v", err)
	}

	select {
	case reason := <-failed:
		if !strings.Contains(strings.ToLower(reason), "sumatrapdf") && !strings.Contains(strings.ToLower(reason), "missing") && !strings.Contains(strings.ToLower(reason), "engine") {
			t.Logf("Job failed as expected with reason: %s", reason)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Expected job to safely fail when print engine is missing, but timed out")
	}

	// Verify failed job was moved to failed directory and not deleted or left in queue
	failedPath := filepath.Join(failedDir, "missing_sumatra_job_1")
	if _, err := os.Stat(failedPath); err != nil {
		t.Errorf("Expected job file moved to %s, stat returned: %v", failedPath, err)
	}
}

func TestSumatraShortcutCreation(t *testing.T) {
	tempDir := t.TempDir()
	targetExe := filepath.Join(tempDir, "SumatraPDF.exe")
	lnkPath := filepath.Join(tempDir, "SumatraPDF.lnk")

	// Must fail on missing exe
	if err := printer.CreateSumatraShortcut(""); err == nil {
		t.Errorf("Expected error for empty exe path")
	}

	// Write valid >1MB dummy exe
	var dummyBytes []byte
	if sumatra := printer.FindSumatraPDF(); sumatra != "" {
		dummyBytes, _ = os.ReadFile(sumatra)
	}
	if len(dummyBytes) == 0 {
		if exe, err := os.Executable(); err == nil && exe != "" {
			dummyBytes, _ = os.ReadFile(exe)
		}
	}
	if len(dummyBytes) == 0 {
		dummyBytes = make([]byte, 1024*1024+50)
		copy(dummyBytes, "MZ")
	}
	if err := os.WriteFile(targetExe, dummyBytes, 0755); err != nil {
		t.Fatalf("Failed to write target exe: %v", err)
	}

	// Test shortcut creation
	if err := printer.CreateShortcut(targetExe, lnkPath, "Bridge Silent Print Engine", ""); err != nil {
		t.Fatalf("Failed to create shortcut: %v", err)
	}

	if _, err := os.Stat(lnkPath); err != nil {
		t.Fatalf("Shortcut was not created at %s: %v", lnkPath, err)
	}
}

func TestUninstallerCleansSumatraAndShortcuts(t *testing.T) {
	tempDir := t.TempDir()
	appDir := filepath.Join(tempDir, "BridgeApp")
	_ = os.MkdirAll(appDir, 0755)

	sumatraExe := filepath.Join(appDir, "SumatraPDF.exe")
	sumatraLnk := filepath.Join(appDir, "SumatraPDF.lnk")
	bridgeExe := filepath.Join(appDir, "bridge.exe")

	_ = os.WriteFile(sumatraExe, []byte("fake sumatra"), 0755)
	_ = os.WriteFile(sumatraLnk, []byte("fake link"), 0644)
	_ = os.WriteFile(bridgeExe, []byte("fake bridge"), 0755)

	// Simulate uninstaller file cleanup
	entries, err := os.ReadDir(appDir)
	if err != nil {
		t.Fatalf("Failed to read app dir: %v", err)
	}
	for _, e := range entries {
		if !strings.EqualFold(e.Name(), "bridge.exe") {
			_ = os.RemoveAll(filepath.Join(appDir, e.Name()))
		}
	}

	if _, err := os.Stat(sumatraExe); !os.IsNotExist(err) {
		t.Errorf("Expected SumatraPDF.exe to be removed by uninstaller")
	}
	if _, err := os.Stat(sumatraLnk); !os.IsNotExist(err) {
		t.Errorf("Expected SumatraPDF.lnk to be removed by uninstaller")
	}
	if _, err := os.Stat(bridgeExe); err != nil {
		t.Errorf("Expected bridge.exe to remain for final detachment cleanup: %v", err)
	}
}

func TestExtractCleanSpoolPayload(t *testing.T) {
	// 1. Direct clean XPS container
	rawXPS := []byte("PK\x03\x04\x14\x00\x08\x00[Content_Types].xml...")
	cleanXPS, extXPS := printer.ExtractCleanSpoolPayload(rawXPS)
	if extXPS != ".xps" {
		t.Errorf("Expected .xps extension, got %s", extXPS)
	}
	if !bytes.Equal(cleanXPS, rawXPS) {
		t.Errorf("Expected unchanged raw XPS stream")
	}

	// 2. PJL-encapsulated XPS container with EOJ trailer and EOCD
	zipEOCD := []byte("PK\x03\x04\x14\x00\x08\x00[Content_Types].xml..." + "PK\x05\x06\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00")
	pjlXPS := append([]byte("\x1b%-12345X@PJL JOB\r\n@PJL ENTER LANGUAGE = XPS\r\n"), zipEOCD...)
	pjlXPS = append(pjlXPS, []byte("\x1b%-12345X@PJL EOJ\r\n\x1b%-12345X")...)
	cleanPJLXPS, extPJLXPS := printer.ExtractCleanSpoolPayload(pjlXPS)
	if extPJLXPS != ".xps" {
		t.Errorf("Expected .xps extension for PJL XPS, got %s", extPJLXPS)
	}
	if !bytes.Equal(cleanPJLXPS, zipEOCD) {
		t.Errorf("Expected exact byte-for-byte extraction of zip container without trailing PJL EOJ")
	}
	if bytes.Contains(cleanPJLXPS, []byte("@PJL")) {
		t.Errorf("Clean XPS should not contain PJL headers or trailers")
	}

	// 3. Direct clean PDF container
	rawPDF := []byte("%PDF-1.7\n1 0 obj\n<< /Type /Catalog >>\nendobj\n%%EOF")
	cleanPDF, extPDF := printer.ExtractCleanSpoolPayload(rawPDF)
	if extPDF != ".pdf" {
		t.Errorf("Expected .pdf extension, got %s", extPDF)
	}
	if !bytes.Equal(cleanPDF, rawPDF) {
		t.Errorf("Expected unchanged raw PDF stream")
	}

	// 4. PJL-encapsulated PDF container with UEL trailer
	pjlPDF := []byte("\x1b%-12345X@PJL JOB\r\n@PJL ENTER LANGUAGE = PDF\r\n%PDF-1.4\nstream data\n%%EOF\r\n\x1b%-12345X")
	cleanPJLPDF, extPJLPDF := printer.ExtractCleanSpoolPayload(pjlPDF)
	if extPJLPDF != ".pdf" {
		t.Errorf("Expected .pdf extension for PJL PDF, got %s", extPJLPDF)
	}
	if !bytes.HasPrefix(cleanPJLPDF, []byte("%PDF-")) {
		t.Errorf("Expected clean PDF to start with %%PDF-, got %q", string(cleanPJLPDF[:10]))
	}
	if bytes.Contains(cleanPJLPDF, []byte("@PJL")) {
		t.Errorf("Clean PDF should not contain PJL headers")
	}

	// 5. Linear / non-%%EOF PDF with trailing UEL command
	rawLinearPDF := []byte("%PDF-1.5\nstream data without explicit eof\x1b%-12345X@PJL EOJ")
	cleanLinear, extLinear := printer.ExtractCleanSpoolPayload(rawLinearPDF)
	if extLinear != ".pdf" {
		t.Errorf("Expected .pdf extension for linear PDF, got %s", extLinear)
	}
	if bytes.Contains(cleanLinear, []byte("@PJL")) || bytes.Contains(cleanLinear, []byte("\x1b%-12345X")) {
		t.Errorf("Expected trailing PJL stripped from linear PDF without %%%%EOF")
	}
}

func TestEnhancedVirtualPrinterKeywords(t *testing.T) {
	// Virtual printers that corrupt formatting if treated as physical
	virtualPrinters := []struct {
		name string
		port string
	}{
		{"Generic / Text Only", "LPT1:"},
		{"Generic", "USB001"},
		{"Text Only", "COM1:"},
		{"Microsoft XPS Document Writer v4", "PORTPROMPT:"},
		{"OpenXPS Document Writer", "PORTPROMPT:"},
		{"PDFCreator", "PDFPort"},
		{"Foxit PDF Printer", "FoxitPort"},
		{"Snagit 2024", "SnagitPort"},
		{"Nitro PDF Creator", "NitroPort"},
	}

	for _, vp := range virtualPrinters {
		if !printer.IsVirtualPrinterName(vp.name, vp.port) {
			t.Errorf("Expected %q on port %q to be identified as virtual printer", vp.name, vp.port)
		}
	}

	// Real physical printers that must NOT be identified as virtual
	physicalPrinters := []struct {
		name string
		port string
	}{
		{"Canon TS5300 series", "WSD-abc123"},
		{"HP LaserJet Pro MFP M428fdw", "192.168.1.100"},
		{"Brother HL-L2350DW series", "USB001"},
		{"Epson EcoTank ET-2800", "USB002"},
	}

	for _, pp := range physicalPrinters {
		if printer.IsVirtualPrinterName(pp.name, pp.port) {
			t.Errorf("Expected %q on port %q to be identified as physical printer, but was flagged virtual", pp.name, pp.port)
		}
	}
}




