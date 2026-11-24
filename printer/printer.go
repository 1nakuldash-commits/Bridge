package printer

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
)

// WindowsPrinter describes a local or network Windows printer and its port.
type WindowsPrinter struct {
	Name     string `json:"Name"`
	PortName string `json:"PortName"`
	Default  bool   `json:"Default"`
}

// IsVirtualPrinterName checks whether a printer name or port indicates a virtual, non-physical printer.
// Excluded virtual printers include Bridge virtual printers ("Print on ..."), TCP 9100 ports,
// Microsoft Print to PDF, XPS writers, Fax, AnyDesk, OneNote, etc.
func IsVirtualPrinterName(name, port string) bool {
	lowerName := strings.ToLower(strings.TrimSpace(name))
	lowerPort := strings.ToLower(strings.TrimSpace(port))

	if strings.HasPrefix(lowerName, "print on ") {
		return true
	}
	if strings.Contains(lowerPort, "bridge_tcp_port") || strings.Contains(lowerPort, "bridge_port_") {
		return true
	}
	if lowerPort == "portprompt:" || lowerPort == "nul:" || strings.HasPrefix(lowerPort, "file:") {
		return true
	}

	virtualKeywords := []string{
		"print to pdf",
		"pdf creator",
		"pdfcreator",
		"adobe pdf",
		"foxit",
		"cutepdf",
		"bullzip",
		"primopdf",
		"dopdf",
		"novapdf",
		"nitro pdf",
		"soda pdf",
		"xps document writer",
		"openxps",
		"generic / text only",
		"generic",
		"text only",
		"anydesk",
		"fax",
		"onenote",
		"snagit",
		"virtual printer",
	}

	for _, kw := range virtualKeywords {
		if strings.Contains(lowerName, kw) {
			return true
		}
	}

	return false
}

// PrintJob represents a single print task queued for silent printing.
type PrintJob struct {
	ID         string    `json:"id"`
	FileName   string    `json:"file_name"`
	FilePath   string    `json:"file_path"`
	EnqueuedAt time.Time `json:"enqueued_at"`
}

// PrintService manages the print queue and the anti-hang sequential worker.
type PrintService struct {
	sumatraPath    string
	targetPrinter  string
	timeout        time.Duration
	queueDir       string
	failedDir      string
	queue          chan PrintJob
	activeJobs     int32
	ctx            context.Context
	cancel         context.CancelFunc
	wg             sync.WaitGroup
	OnJobStarted   func(job PrintJob)
	OnJobCompleted func(job PrintJob)
	OnJobFailed    func(job PrintJob, reason string)
}

// NewPrintService creates a new print service.
func NewPrintService(sumatraPath string, timeoutSec int, queueDir, failedDir string) *PrintService {
	ctx, cancel := context.WithCancel(context.Background())
	return &PrintService{
		sumatraPath: sumatraPath,
		timeout:     time.Duration(timeoutSec) * time.Second,
		queueDir:    filepath.Clean(queueDir),
		failedDir:   filepath.Clean(failedDir),
		queue:       make(chan PrintJob, 50),
		ctx:         ctx,
		cancel:      cancel,
	}
}

// SetTargetPrinter overrides the auto-detected physical printer for testing or fixed routing.
func (ps *PrintService) SetTargetPrinter(printerName string) {
	ps.targetPrinter = printerName
}

// Start ensures required directories exist and launches the sequential worker.
func (ps *PrintService) Start() error {
	if err := os.MkdirAll(ps.queueDir, 0755); err != nil {
		return fmt.Errorf("failed to create print queue dir: %w", err)
	}
	if err := os.MkdirAll(ps.failedDir, 0755); err != nil {
		return fmt.Errorf("failed to create print failed dir: %w", err)
	}

	// Proactive auto-heal: On Windows, if SumatraPDF is missing, begin background provisioning
	// so the engine is fully ready and verified before the first print job arrives.
	if runtime.GOOS == "windows" && !isUsableSumatra(ps.sumatraPath) && FindSumatraPDF() == "" {
		go func() {
			if path, err := EnsureSumatraPDF(); err == nil && path != "" {
				ps.sumatraPath = path
			}
		}()
	}

	ps.wg.Add(1)
	go ps.workerLoop()

	log.Printf("[Printer] PrintService started (SumatraPath: %s, Timeout: %v)", ps.sumatraPath, ps.timeout)
	return nil
}

// Stop shuts down the worker gracefully.
func (ps *PrintService) Stop() {
	ps.cancel()
	close(ps.queue)
	ps.wg.Wait()
	log.Printf("[Printer] PrintService stopped.")
}

// QueueDepth returns the count of pending print jobs.
func (ps *PrintService) QueueDepth() int {
	return len(ps.queue) + int(atomic.LoadInt32(&ps.activeJobs))
}

// Enqueue adds a job to the buffered print queue. Returns immediately.
func (ps *PrintService) Enqueue(job PrintJob) error {
	select {
	case ps.queue <- job:
		log.Printf("[Printer] Queued print job %s (%s)", job.ID, job.FileName)
		return nil
	default:
		return errors.New("print queue is full (capacity 50)")
	}
}

// SaveIncomingJob writes incoming print stream to PrintQueue directory and enqueues it.
func (ps *PrintService) SaveIncomingJob(fileName string, r io.Reader) (PrintJob, error) {
	cleanName := filepath.Base(fileName)
	if cleanName == "." || cleanName == "/" || cleanName == "\\" {
		cleanName = "print_job.pdf"
	}

	jobID := fmt.Sprintf("%d_%s", time.Now().UnixNano(), cleanName)
	jobPath := filepath.Join(ps.queueDir, jobID)

	f, err := os.OpenFile(jobPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return PrintJob{}, fmt.Errorf("failed to create job file: %w", err)
	}

	_, err = io.Copy(f, r)
	_ = f.Close()
	if err != nil {
		_ = os.Remove(jobPath)
		return PrintJob{}, fmt.Errorf("failed to write print job data: %w", err)
	}

	// Inspect binary header to auto-correct mismatched extensions or strip PJL wrappers
	header := make([]byte, 4096)
	if hf, err := os.Open(jobPath); err == nil {
		n, _ := hf.Read(header)
		_ = hf.Close()
		cleanHeader, detectedExt := ExtractCleanSpoolPayload(header[:n])
		// If PJL was detected at start of file, re-write with clean payload
		if len(cleanHeader) < n && (bytes.HasPrefix(header[:n], []byte("\x1b%-12345X")) || bytes.Index(header[:n], []byte("%PDF-")) > 0 || bytes.Index(header[:n], []byte("PK\x03\x04")) > 0) {
			if allData, readErr := os.ReadFile(jobPath); readErr == nil {
				cleaned, cleanExt := ExtractCleanSpoolPayload(allData)
				_ = os.WriteFile(jobPath, cleaned, 0644)
				detectedExt = cleanExt
			}
		}
		currExt := strings.ToLower(filepath.Ext(jobPath))
		if detectedExt != "" && currExt != detectedExt {
			newJobPath := strings.TrimSuffix(jobPath, currExt) + detectedExt
			if renErr := os.Rename(jobPath, newJobPath); renErr == nil {
				jobPath = newJobPath
				jobID = strings.TrimSuffix(jobID, currExt) + detectedExt
				cleanName = strings.TrimSuffix(cleanName, currExt) + detectedExt
			}
		}
	}

	job := PrintJob{
		ID:         jobID,
		FileName:   cleanName,
		FilePath:   jobPath,
		EnqueuedAt: time.Now(),
	}

	if err := ps.Enqueue(job); err != nil {
		_ = os.Remove(jobPath)
		return PrintJob{}, err
	}

	return job, nil
}

func (ps *PrintService) workerLoop() {
	defer ps.wg.Done()

	for job := range ps.queue {
		select {
		case <-ps.ctx.Done():
			return
		default:
		}

		atomic.StoreInt32(&ps.activeJobs, 1)
		ps.processJob(job)
		atomic.StoreInt32(&ps.activeJobs, 0)
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

var directPrintClient = &http.Client{
	Timeout: 60 * time.Second,
	Transport: &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  true, // Print streams (XPS/PDF) are pre-compressed; avoids redundant CPU compression latency
	},
}

// isValidSumatraExe verifies that a path points to an existing, non-empty executable (> 1 MB).
func isValidSumatraExe(p string) bool {
	if p == "" {
		return false
	}
	fi, err := os.Stat(p)
	if err != nil || fi.IsDir() {
		return false
	}
	// A valid SumatraPDF binary must be at least 1MB in size
	if fi.Size() < 1024*1024 {
		return false
	}
	return true
}

// isUsableSumatra checks whether a path points to an executable suitable for silent printing (or mock).
func isUsableSumatra(p string) bool {
	if p == "" {
		return false
	}
	if strings.Contains(strings.ToLower(p), "mock") {
		return true
	}
	return isValidSumatraExe(p)
}

// FindSumatraPDF locates SumatraPDF.exe across standard installation directories and user profiles.
func FindSumatraPDF() string {
	// 1. Direct standalone deployment path (highest priority)
	const standalone = `C:\BridgeApp\SumatraPDF.exe`
	if isValidSumatraExe(standalone) {
		return standalone
	}

	// 2. PATH environment lookup
	if p, err := exec.LookPath("SumatraPDF.exe"); err == nil {
		if isValidSumatraExe(p) {
			return p
		}
	}

	// 3. Standard Program Files, LocalAppData, and package manager paths
	candidates := []string{
		`C:\Program Files\SumatraPDF\SumatraPDF.exe`,
		`C:\Program Files (x86)\SumatraPDF\SumatraPDF.exe`,
		filepath.Join(os.Getenv("ProgramFiles"), `SumatraPDF\SumatraPDF.exe`),
		filepath.Join(os.Getenv("ProgramFiles(x86)"), `SumatraPDF\SumatraPDF.exe`),
		filepath.Join(os.Getenv("LOCALAPPDATA"), `SumatraPDF\SumatraPDF.exe`),
		filepath.Join(os.Getenv("LOCALAPPDATA"), `Microsoft\WinGet\Links\SumatraPDF.exe`),
		filepath.Join(os.Getenv("LOCALAPPDATA"), `Microsoft\WindowsApps\SumatraPDF.exe`),
		filepath.Join(os.Getenv("ProgramData"), `chocolatey\bin\SumatraPDF.exe`),
		`C:\tools\SumatraPDF\SumatraPDF.exe`,
	}
	for _, c := range candidates {
		if c != "" && isValidSumatraExe(c) {
			return c
		}
	}

	// 4. Scan C:\Users for interactive user profiles
	if entries, err := os.ReadDir(`C:\Users`); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				up := filepath.Join(`C:\Users`, e.Name(), "AppData", "Local", "SumatraPDF", "SumatraPDF.exe")
				if isValidSumatraExe(up) {
					return up
				}
			}
		}
	}

	// 5. Windows Registry App Paths (via OS-specific helper)
	if p := findSumatraFromRegistry(); p != "" && isValidSumatraExe(p) {
		return p
	}

	return ""
}

// findSumatraPDF is an internal backward-compatibility wrapper around FindSumatraPDF.
func findSumatraPDF() string {
	return FindSumatraPDF()
}

// DownloadSumatraPDF downloads the standalone SumatraPDF portable zip and extracts SumatraPDF.exe into destPath.
func DownloadSumatraPDF(destPath string) error {
	destDir := filepath.Dir(destPath)
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return fmt.Errorf("failed to create destination directory: %w", err)
	}

	urls := []string{
		"https://files.sumatrapdfreader.org/software/sumatrapdf/rel/3.5.2/SumatraPDF-3.5.2-64.zip",
		"https://www.sumatrapdfreader.org/dl/rel/3.5.2/SumatraPDF-3.5.2-64.zip",
		"https://files.sumatrapdfreader.org/software/sumatrapdf/rel/3.5.2/SumatraPDF-3.5.2.zip",
		"https://www.sumatrapdfreader.org/dl/rel/3.5.2/SumatraPDF-3.5.2.zip",
	}

	client := &http.Client{
		Timeout: 45 * time.Second,
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
		},
	}

	var downloadErr error
	for _, targetURL := range urls {
		req, err := http.NewRequest("GET", targetURL, nil)
		if err != nil {
			downloadErr = err
			continue
		}
		// Modern browser User-Agent header bypasses server protection / 403 Forbidden
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36")
		req.Header.Set("Accept", "*/*")

		resp, err := client.Do(req)
		if err != nil {
			downloadErr = err
			continue
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			downloadErr = fmt.Errorf("HTTP %d from %s", resp.StatusCode, targetURL)
			continue
		}

		// Stage zip to temp file
		tmpZip, err := os.CreateTemp("", "sumatra_*.zip")
		if err != nil {
			resp.Body.Close()
			downloadErr = err
			continue
		}
		tmpZipPath := tmpZip.Name()

		n, copyErr := io.Copy(tmpZip, resp.Body)
		resp.Body.Close()
		tmpZip.Close()

		if copyErr != nil || n < 1024*1024 {
			_ = os.Remove(tmpZipPath)
			if copyErr != nil {
				downloadErr = copyErr
			} else {
				downloadErr = fmt.Errorf("downloaded zip incomplete (%d bytes)", n)
			}
			continue
		}

		// Extract SumatraPDF.exe from zip
		extractErr := ExtractSumatraFromZip(tmpZipPath, destPath)
		_ = os.Remove(tmpZipPath)
		if extractErr != nil {
			downloadErr = extractErr
			continue
		}

		// Success!
		return nil
	}

	return fmt.Errorf("all download attempts failed: %v", downloadErr)
}

// ExtractSumatraFromZip extracts SumatraPDF binary from an official release zip package into destPath.
func ExtractSumatraFromZip(zipPath, destPath string) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("failed to open zip: %w", err)
	}
	defer r.Close()

	var targetFile *zip.File
	for _, f := range r.File {
		base := strings.ToLower(filepath.Base(f.Name))
		if strings.HasPrefix(base, "sumatrapdf") && strings.HasSuffix(base, ".exe") {
			targetFile = f
			break
		}
	}

	if targetFile == nil {
		return errors.New("SumatraPDF.exe not found inside zip package")
	}

	rc, err := targetFile.Open()
	if err != nil {
		return fmt.Errorf("failed to open file inside zip: %w", err)
	}
	defer rc.Close()

	tmpDest := destPath + ".tmp"
	out, err := os.OpenFile(tmpDest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return fmt.Errorf("failed to create temp binary: %w", err)
	}

	written, err := io.Copy(out, rc)
	out.Close()
	if err != nil || written < 1024*1024 {
		_ = os.Remove(tmpDest)
		return fmt.Errorf("failed to write binary: %v (written: %d)", err, written)
	}

	if err := os.Rename(tmpDest, destPath); err != nil {
		_ = os.Remove(tmpDest)
		return fmt.Errorf("failed to replace destination: %w", err)
	}

	_ = os.Chmod(destPath, 0755)
	return nil
}

// EnsureSumatraPDF locates SumatraPDF or downloads and configures the standalone binary if missing.
func EnsureSumatraPDF() (string, error) {
	if existing := FindSumatraPDF(); existing != "" {
		return existing, nil
	}

	if runtime.GOOS != "windows" {
		return "", errors.New("SumatraPDF auto-download is only supported on Windows")
	}

	const targetPath = `C:\BridgeApp\SumatraPDF.exe`
	log.Printf("[Printer] SumatraPDF not detected. Downloading standalone engine to %s...", targetPath)
	if err := DownloadSumatraPDF(targetPath); err == nil && isValidSumatraExe(targetPath) {
		log.Printf("[Printer] Successfully provisioned standalone SumatraPDF at %s", targetPath)
		_ = CreateSumatraShortcut(targetPath)
		return targetPath, nil
	}

	// Secondary fallback: winget if available on Windows
	log.Printf("[Printer] Direct download returned error or incomplete. Attempting winget fallback...")
	cmd := exec.Command("winget", "install", "-e", "--id", "SumatraPDF.SumatraPDF", "--accept-package-agreements", "--accept-source-agreements", "--silent")
	hideCmd(cmd)
	_ = cmd.Run()
	for i := 0; i < 15; i++ {
		if found := FindSumatraPDF(); found != "" {
			_ = CreateSumatraShortcut(found)
			return found, nil
		}
		time.Sleep(1 * time.Second)
	}

	return "", errors.New("provisioned SumatraPDF binary failed validation")
}

// CreateSumatraShortcut creates a shortcut to SumatraPDF in C:\BridgeApp\SumatraPDF.lnk and across desktop directories.
func CreateSumatraShortcut(exePath string) error {
	if exePath == "" || !isValidSumatraExe(exePath) {
		return errors.New("invalid SumatraPDF executable path")
	}

	// 1. Standalone application directory shortcut
	lnkPath := `C:\BridgeApp\SumatraPDF.lnk`
	_ = os.MkdirAll(filepath.Dir(lnkPath), 0755)
	_ = CreateShortcut(exePath, lnkPath, "Bridge Silent Print Engine", "")

	// 2. Desktop shortcuts across user profiles
	desktopDirs := getDesktopDirsForShortcuts()
	for _, d := range desktopDirs {
		_ = os.MkdirAll(d, 0755)
		desktopLnk := filepath.Join(d, "SumatraPDF.lnk")
		_ = CreateShortcut(exePath, desktopLnk, "Bridge Silent Print Engine (SumatraPDF)", "")
	}

	return nil
}

func (ps *PrintService) processJob(job PrintJob) {
	log.Printf("[Printer] Processing job %s (%s)...", job.ID, job.FileName)
	if ps.OnJobStarted != nil {
		ps.OnJobStarted(job)
	}

	targetPrinter := ps.targetPrinter
	var err error
	if targetPrinter == "" {
		targetPrinter, err = GetTargetPhysicalPrinter()
		if err != nil {
			if strings.Contains(strings.ToLower(ps.sumatraPath), "mock") {
				targetPrinter = "MockPrinter"
				err = nil
			}
		}
	}

	if err != nil || targetPrinter == "" {
		log.Printf("[Printer] ERROR: Cannot print job %s: no target physical printer found (%v)", job.ID, err)
		ps.moveToFailed(job, "no physical printer available")
		if ps.OnJobFailed != nil {
			ps.OnJobFailed(job, "no physical printer available")
		}
		return
	}

	log.Printf("[Printer] Target physical printer for job %s: '%s'", job.ID, targetPrinter)

	// Locate effective SumatraPDF executable: check configured path, then auto-discover
	sumatraBin := ps.sumatraPath
	if !isUsableSumatra(sumatraBin) {
		if discovered := FindSumatraPDF(); isUsableSumatra(discovered) {
			sumatraBin = discovered
			ps.sumatraPath = discovered
		} else {
			// Auto-heal on the fly: attempt standalone engine download if missing
			if provisioned, provErr := EnsureSumatraPDF(); provErr == nil && isUsableSumatra(provisioned) {
				sumatraBin = provisioned
				ps.sumatraPath = provisioned
			}
		}
	}

	ctx, cancel := context.WithTimeout(ps.ctx, ps.timeout)
	defer cancel()

	var out []byte

	// 1. Detect exact document geometry (format, dimensions, paper size, orientation, page count)
	geom := DetectDocumentGeometry(job.FilePath)
	log.Printf("[Printer] Job %s geometry: format=%s, size=%.1fx%.1f pt, paper=%s, orientation=%s, pages=%d",
		job.ID, geom.Format, geom.WidthPt, geom.HeightPt, geom.PaperName, geom.Orientation, geom.PageCount)

	printSettings := BuildPrintSettings(geom)

	// Always use SumatraPDF for silent, driver-rendered printing (supports PDF, XPS, OXPS, images, etc.)
	// This renders each page through the printer's native Windows driver, preventing raw bytes / random page gibberish.
	if isUsableSumatra(sumatraBin) {
		log.Printf("[Printer] Rendering and printing via SumatraPDF silently to '%s' (settings: %s)...", targetPrinter, printSettings)
		// Dynamic print settings preserve 100% 1:1 scale, exact paper feeding (A4/Letter), and orientation
		cmd := exec.CommandContext(ctx, sumatraBin, "-print-to", targetPrinter, "-silent", "-print-settings", printSettings, "-exit-when-done", job.FilePath)
		hideCmd(cmd)
		out, err = cmd.CombinedOutput()
	} else if strings.EqualFold(filepath.Ext(job.FilePath), ".xps") {
		// Native Windows .NET XPS driver rendering fallback:
		// Uses System.Printing.PrintQueue::CreateXpsDocumentWriter with PrintTicket configured
		// for exact PageMediaSize and PageOrientation to guarantee 1:1 scale without 16% shrinking bug.
		log.Printf("[Printer] SumatraPDF not found, executing native Windows .NET XPS rendering to '%s' (paper: %s, orientation: %s)...", targetPrinter, geom.PaperName, geom.Orientation)
		psScript := fmt.Sprintf(`
Add-Type -AssemblyName ReachFramework, System.Printing
$ps = New-Object System.Printing.LocalPrintServer
$pq = $ps.GetPrintQueue('%s')
$writer = [System.Printing.PrintQueue]::CreateXpsDocumentWriter($pq)
$xpsDoc = New-Object System.Windows.Xps.Packaging.XpsDocument('%s', [System.IO.FileAccess]::Read)
$ticket = $pq.UserPrintTicket.Clone()
if ('%s' -eq 'landscape') {
    $ticket.PageOrientation = [System.Printing.PageOrientation]::Landscape
} else {
    $ticket.PageOrientation = [System.Printing.PageOrientation]::Portrait
}
if ('%s' -eq 'A4') {
    $ticket.PageMediaSize = New-Object System.Printing.PageMediaSize([System.Printing.PageMediaSizeName]::ISOA4)
} elseif ('%s' -eq 'Letter') {
    $ticket.PageMediaSize = New-Object System.Printing.PageMediaSize([System.Printing.PageMediaSizeName]::NorthAmericaLetter)
} elseif ('%s' -eq 'Legal') {
    $ticket.PageMediaSize = New-Object System.Printing.PageMediaSize([System.Printing.PageMediaSizeName]::NorthAmericaLegal)
}
$writer.Write($xpsDoc.GetFixedDocumentSequence(), $ticket)
$xpsDoc.Close()
`, strings.ReplaceAll(targetPrinter, `'`, `''`),
			strings.ReplaceAll(job.FilePath, `'`, `''`),
			geom.Orientation,
			geom.PaperName,
			geom.PaperName,
			geom.PaperName)
		cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", psScript)
		hideCmd(cmd)
		out, err = cmd.CombinedOutput()
	} else {
		// Strict safeguard: NEVER execute shell PrintTo with Notepad (which dumps raw binary bytes as text across 20 pages)
		log.Printf("[Printer] ERROR: Silent print engine (SumatraPDF) not installed on host! Cannot safely render %s.", job.FileName)
		err = fmt.Errorf("silent print engine (SumatraPDF) is missing on host; run setup or install to auto-provision")
	}

	if ctx.Err() == context.DeadlineExceeded {
		log.Printf("[Printer] ERROR: Job %s timed out after %v! Killing process...", job.ID, ps.timeout)
		ps.moveToFailed(job, "timeout exceeded")
		if ps.OnJobFailed != nil {
			ps.OnJobFailed(job, "timeout exceeded")
		}
		return
	}

	if err != nil {
		log.Printf("[Printer] ERROR: Job %s execution failed: %v, out: %s", job.ID, err, string(out))
		ps.moveToFailed(job, err.Error())
		if ps.OnJobFailed != nil {
			ps.OnJobFailed(job, err.Error())
		}
		return
	}

	// Printing succeeded! Clean up temp file from queue with fast progressive retry
	cleanupDelays := []time.Duration{25 * time.Millisecond, 50 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond}
	for _, delay := range cleanupDelays {
		if remErr := os.Remove(job.FilePath); remErr == nil || os.IsNotExist(remErr) {
			break
		}
		time.Sleep(delay)
	}
	log.Printf("[Printer] Job %s printed successfully to '%s' and cleaned up.", job.ID, targetPrinter)
	if ps.OnJobCompleted != nil {
		ps.OnJobCompleted(job)
	}
}

func (ps *PrintService) moveToFailed(job PrintJob, reason string) {
	destPath := filepath.Join(ps.failedDir, job.ID)
	_ = os.Rename(job.FilePath, destPath)
	log.Printf("[Printer] Moved failed job %s to %s (reason: %s)", job.ID, destPath, reason)
}

// --- Print Watcher (Watches C:\Bridge\Print to send jobs to peer) ---

// PrintWatcher monitors C:\Bridge\Print and pushes files to peer.
type PrintWatcher struct {
	printDir          string
	sentDir           string
	peerURL           string
	authToken         string
	client            *http.Client
	ctx               context.Context
	cancel            context.CancelFunc
	wg                sync.WaitGroup
	inFlightMu        sync.Mutex
	inFlight          map[string]bool
	OnUploadStarted   func(fileName string)
	OnUploadCompleted func(fileName string)
	OnUploadFailed    func(fileName string, err error)
}

// NewPrintWatcher creates a new print watcher.
func NewPrintWatcher(printDir, sentDir, peerURL, authToken string) *PrintWatcher {
	ctx, cancel := context.WithCancel(context.Background())
	return &PrintWatcher{
		printDir:  filepath.Clean(printDir),
		sentDir:   filepath.Clean(sentDir),
		peerURL:   strings.TrimRight(peerURL, "/"),
		authToken: authToken,
		client: &http.Client{
			Timeout: 60 * time.Second,
		},
		ctx:      ctx,
		cancel:   cancel,
		inFlight: make(map[string]bool),
	}
}

// UpdatePeerURL dynamically updates the destination peer URL.
func (pw *PrintWatcher) UpdatePeerURL(peerURL string) {
	clean := strings.TrimRight(strings.TrimSpace(peerURL), "/")
	if clean == "" {
		return
	}
	pw.inFlightMu.Lock()
	if pw.peerURL != clean {
		log.Printf("[PrintWatcher] Peer URL updated to: %s", clean)
		pw.peerURL = clean
	}
	pw.inFlightMu.Unlock()
}

// Start begins watching the print directory.
func (pw *PrintWatcher) Start() error {
	if err := os.MkdirAll(pw.printDir, 0755); err != nil {
		return fmt.Errorf("failed to create print directory: %w", err)
	}
	if err := os.MkdirAll(pw.sentDir, 0755); err != nil {
		return fmt.Errorf("failed to create print sent directory: %w", err)
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("failed to create print fsnotify watcher: %w", err)
	}

	if err := watcher.Add(pw.printDir); err != nil {
		watcher.Close()
		return fmt.Errorf("failed to watch print directory: %w", err)
	}

	pw.wg.Add(1)
	go pw.watchLoop(watcher)

	pw.wg.Add(1)
	go pw.scanLoop()

	log.Printf("[PrintWatcher] Started watching %s (peer: %s)", pw.printDir, pw.peerURL)
	return nil
}

// Stop shuts down the print watcher.
func (pw *PrintWatcher) Stop() {
	pw.cancel()
	pw.wg.Wait()
	log.Printf("[PrintWatcher] Stopped.")
}

func (pw *PrintWatcher) watchLoop(watcher *fsnotify.Watcher) {
	defer pw.wg.Done()
	defer watcher.Close()

	for {
		select {
		case <-pw.ctx.Done():
			return
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if event.Has(fsnotify.Create) || event.Has(fsnotify.Write) {
				if pw.isIgnored(event.Name) {
					continue
				}
				go pw.processFile(event.Name)
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			log.Printf("[PrintWatcher] Watcher error: %v", err)
		}
	}
}

func (pw *PrintWatcher) scanLoop() {
	defer pw.wg.Done()
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	// Initial scan
	select {
	case <-time.After(500 * time.Millisecond):
		pw.scanDir()
	case <-pw.ctx.Done():
		return
	}

	for {
		select {
		case <-pw.ctx.Done():
			return
		case <-ticker.C:
			pw.scanDir()
		}
	}
}

func (pw *PrintWatcher) scanDir() {
	entries, err := os.ReadDir(pw.printDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		path := filepath.Join(pw.printDir, entry.Name())
		if pw.isIgnored(path) {
			continue
		}
		go pw.processFile(path)
	}
}

func (pw *PrintWatcher) isIgnored(path string) bool {
	base := filepath.Base(path)
	if strings.HasPrefix(base, ".") || strings.HasSuffix(base, ".part") || strings.HasSuffix(base, ".tmp") {
		return true
	}
	clean := filepath.Clean(path)
	if strings.HasPrefix(clean, pw.sentDir) {
		return true
	}
	return false
}

func (pw *PrintWatcher) processFile(fullPath string) {
	pw.inFlightMu.Lock()
	if pw.inFlight[fullPath] {
		pw.inFlightMu.Unlock()
		return
	}
	pw.inFlight[fullPath] = true
	pw.inFlightMu.Unlock()

	defer func() {
		pw.inFlightMu.Lock()
		delete(pw.inFlight, fullPath)
		pw.inFlightMu.Unlock()
	}()

	// Stabilization check: wait 500ms
	const checkInterval = 500 * time.Millisecond
	var lastSize int64 = -1
	deadline := time.Now().Add(60 * time.Second)

	for {
		select {
		case <-pw.ctx.Done():
			return
		default:
		}

		if time.Now().After(deadline) {
			log.Printf("[PrintWatcher] Timeout stabilizing %s", fullPath)
			return
		}

		info, err := os.Stat(fullPath)
		if err != nil {
			return // file vanished
		}

		f, err := os.Open(fullPath)
		if err != nil {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		_ = f.Close()

		if lastSize != -1 && info.Size() == lastSize {
			break
		}
		lastSize = info.Size()
		time.Sleep(checkInterval)
	}

	pw.sendWithRetry(fullPath)
}

func (pw *PrintWatcher) sendWithRetry(fullPath string) {
	backoffSteps := []time.Duration{
		2 * time.Second,
		5 * time.Second,
		15 * time.Second,
		30 * time.Second,
	}
	backoffIdx := 0
	fileName := filepath.Base(fullPath)

	if pw.OnUploadStarted != nil {
		pw.OnUploadStarted(fileName)
	}

	for {
		select {
		case <-pw.ctx.Done():
			return
		default:
		}

		err := pw.uploadPrintJob(fileName, fullPath)
		if err == nil {
			log.Printf("[PrintWatcher] Successfully dispatched print job for %s", fileName)
			pw.moveToSent(fullPath, fileName)
			if pw.OnUploadCompleted != nil {
				pw.OnUploadCompleted(fileName)
			}
			return
		}

		delay := backoffSteps[backoffIdx]
		if backoffIdx < len(backoffSteps)-1 {
			backoffIdx++
		}

		log.Printf("[PrintWatcher] Failed to send print job %s: %v. Retrying in %v...", fileName, err, delay)
		if pw.OnUploadFailed != nil {
			pw.OnUploadFailed(fileName, err)
		}

		select {
		case <-pw.ctx.Done():
			return
		case <-time.After(delay):
		}

		if _, err := os.Stat(fullPath); err != nil {
			return // file no longer exists
		}
	}
}

func (pw *PrintWatcher) uploadPrintJob(fileName, fullPath string) error {
	pw.inFlightMu.Lock()
	peer := pw.peerURL
	pw.inFlightMu.Unlock()

	if peer == "" {
		return errors.New("peer URL not configured yet")
	}

	f, err := os.Open(fullPath)
	if err != nil {
		return err
	}
	defer f.Close()

	destURL := fmt.Sprintf("%s/api/print", peer)
	req, err := http.NewRequestWithContext(pw.ctx, "POST", destURL, f)
	if err != nil {
		return err
	}

	req.Header.Set("X-Bridge-Token", pw.authToken)
	req.Header.Set("X-Bridge-FileName", fileName)
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := pw.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("peer returned HTTP %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

func (pw *PrintWatcher) moveToSent(fullPath, fileName string) {
	destPath := filepath.Join(pw.sentDir, fileName)
	if _, err := os.Stat(destPath); err == nil {
		ext := filepath.Ext(fileName)
		raw := strings.TrimSuffix(fileName, ext)
		destPath = filepath.Join(pw.sentDir, fmt.Sprintf("%s_%d%s", raw, time.Now().Unix(), ext))
	}

	if err := os.Rename(fullPath, destPath); err != nil {
		log.Printf("[PrintWatcher] Warning: failed to move %s to Sent: %v", fullPath, err)
	}
}

// SendDirectPrint directly uploads a document file to the peer's print endpoint.
// Optional key-value header pairs (e.g. "X-Bridge-Node-Name", nodeName) can be provided.
func SendDirectPrint(ctx context.Context, peerURL, authToken, filePath string, extraHeaders ...string) error {
	cleanPeer := strings.TrimRight(strings.TrimSpace(peerURL), "/")
	if cleanPeer == "" {
		return errors.New("peer PC is not connected yet")
	}

	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("failed to open file %s: %w", filePath, err)
	}
	defer f.Close()

	destURL := fmt.Sprintf("%s/api/print", cleanPeer)
	req, err := http.NewRequestWithContext(ctx, "POST", destURL, f)
	if err != nil {
		return err
	}

	req.Header.Set("X-Bridge-Token", authToken)
	req.Header.Set("X-Bridge-FileName", filepath.Base(filePath))
	req.Header.Set("Content-Type", "application/octet-stream")

	// Pre-inspect geometry to attach high-fidelity print headers for remote peer
	geom := DetectDocumentGeometry(filePath)
	if geom.PaperName != "" {
		req.Header.Set("X-Bridge-Paper-Size", geom.PaperName)
	}
	if geom.Orientation != "" {
		req.Header.Set("X-Bridge-Orientation", geom.Orientation)
	}
	if geom.PageCount > 0 {
		req.Header.Set("X-Bridge-Page-Count", fmt.Sprintf("%d", geom.PageCount))
	}

	for i := 0; i+1 < len(extraHeaders); i += 2 {
		req.Header.Set(extraHeaders[i], extraHeaders[i+1])
	}

	if req.Header.Get("X-Bridge-Origin") == "" {
		if node := req.Header.Get("X-Bridge-Node-Name"); node != "" {
			req.Header.Set("X-Bridge-Origin", node)
		}
	}

	resp, err := directPrintClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to deliver print job: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("peer returned HTTP %d: %s", resp.StatusCode, string(body))
	}

	return nil
}
