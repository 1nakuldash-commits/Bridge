package printer

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
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
	mu                   sync.RWMutex
	sumatraPath          string
	targetPrinter        string
	timeout              time.Duration
	queueDir             string
	failedDir            string
	queue                chan PrintJob
	activeJobs           int32
	ctx                  context.Context
	cancel               context.CancelFunc
	wg                   sync.WaitGroup
	resolveTargetPrinter func() (string, error)
	findSumatra          func() string
	ensureSumatra        func() (string, error)
	OnJobStarted         func(job PrintJob)
	OnJobCompleted       func(job PrintJob)
	OnJobFailed          func(job PrintJob, reason string)
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

func (ps *PrintService) getSumatraPath() string {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return ps.sumatraPath
}

func (ps *PrintService) setSumatraPath(path string) {
	ps.mu.Lock()
	ps.sumatraPath = path
	ps.mu.Unlock()
}

func (ps *PrintService) getTargetPrinter() string {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return ps.targetPrinter
}

// SetTargetPrinter overrides the auto-detected physical printer for testing or fixed routing.
func (ps *PrintService) SetTargetPrinter(printerName string) {
	ps.mu.Lock()
	ps.targetPrinter = printerName
	ps.mu.Unlock()
}

// Start ensures required directories exist, recovers orphaned queued jobs, and launches the sequential worker.
func (ps *PrintService) Start() error {
	if err := os.MkdirAll(ps.queueDir, 0755); err != nil {
		return fmt.Errorf("failed to create print queue dir: %w", err)
	}
	if err := os.MkdirAll(ps.failedDir, 0755); err != nil {
		return fmt.Errorf("failed to create print failed dir: %w", err)
	}

	// Proactive auto-heal: On Windows, if SumatraPDF is missing, begin background provisioning
	// so the engine is fully ready and verified before the first print job arrives.
	initialSumatra := ps.getSumatraPath()
	findFn := ps.getFindSumatra()
	ensureFn := ps.getEnsureSumatra()
	if runtime.GOOS == "windows" {
		ConfigureSumatraDefaults(initialSumatra)
		if !isUsableSumatra(initialSumatra) && findFn() == "" {
			go func() {
				if path, err := ensureFn(); err == nil && path != "" {
					ps.setSumatraPath(path)
					ConfigureSumatraDefaults(path)
				}
			}()
		}
	}

	ps.recoverOrphanedJobs()

	ps.wg.Add(1)
	go ps.workerLoop()

	log.Printf("[Printer] PrintService started (SumatraPath: %s, Timeout: %v)", initialSumatra, ps.timeout)
	return nil
}

func (ps *PrintService) recoverOrphanedJobs() {
	entries, err := os.ReadDir(ps.queueDir)
	if err != nil {
		return
	}

	type orphanedEntry struct {
		name    string
		path    string
		modTime time.Time
	}
	var orphans []orphanedEntry

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		lower := strings.ToLower(name)
		if strings.HasPrefix(name, ".") || strings.HasSuffix(lower, ".tmp") || strings.HasSuffix(lower, ".part") || strings.HasSuffix(lower, ".clean") {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			continue
		}
		fullPath := filepath.Join(ps.queueDir, name)
		if !isValidQueuedJobFile(fullPath, lower) {
			continue
		}
		orphans = append(orphans, orphanedEntry{
			name:    name,
			path:    fullPath,
			modTime: info.ModTime(),
		})
	}

	sort.Slice(orphans, func(i, j int) bool {
		if !orphans[i].modTime.Equal(orphans[j].modTime) {
			return orphans[i].modTime.Before(orphans[j].modTime)
		}
		return orphans[i].name < orphans[j].name
	})

	for _, o := range orphans {
		job := PrintJob{
			ID:         o.name,
			FileName:   extractOriginalJobName(o.name),
			FilePath:   o.path,
			EnqueuedAt: o.modTime,
		}
		if err := ps.Enqueue(job); err != nil {
			log.Printf("[Printer] Warning: failed to recover orphaned job %s: %v", o.name, err)
			break
		}
		log.Printf("[Printer] Recovered orphaned queued print job %s", o.name)
	}
}

func isValidQueuedJobFile(path, lowerName string) bool {
	ext := filepath.Ext(lowerName)
	switch ext {
	case ".pdf", ".xps", ".oxps", ".png", ".jpg", ".jpeg", ".tif", ".tiff", ".gif", ".bmp":
		return true
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, 512)
	n, _ := f.Read(buf)
	hdr := buf[:n]
	return detectImageMagic(hdr) != "" ||
		bytes.HasPrefix(hdr, []byte("%PDF-")) ||
		bytes.HasPrefix(hdr, []byte("PK\x03\x04")) ||
		bytes.HasPrefix(hdr, []byte("\x1b%-12345X")) ||
		bytes.HasPrefix(hdr, []byte("@PJL"))
}

func extractOriginalJobName(id string) string {
	idx := strings.IndexByte(id, '_')
	if idx > 0 && idx < len(id)-1 {
		allDigits := true
		for i := 0; i < idx; i++ {
			if id[i] < '0' || id[i] > '9' {
				allDigits = false
				break
			}
		}
		if allDigits {
			return id[idx+1:]
		}
	}
	return id
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

	// Inspect binary header to strip genuine PJL wrappers or normalize mislabeled PDF/XPS spool streams.
	// Never alter or rename valid image files or non-PDF/XPS formats.
	jobPath, jobID, cleanName = normalizeIncomingJobFile(jobPath, jobID, cleanName)

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

func isGenericSpoolExt(ext string) bool {
	switch ext {
	case "", ".pdf", ".prn", ".spl", ".bin", ".dat", ".raw":
		return true
	default:
		return false
	}
}

func normalizeIncomingJobFile(jobPath, jobID, cleanName string) (string, string, string) {
	hf, err := os.Open(jobPath)
	if err != nil {
		return jobPath, jobID, cleanName
	}
	header := make([]byte, 4096)
	n, _ := hf.Read(header)
	_ = hf.Close()
	if n == 0 {
		return jobPath, jobID, cleanName
	}
	hdr := header[:n]
	currExt := strings.ToLower(filepath.Ext(jobPath))

	// Preserve valid image files and non-PJL/PDF/XPS formats untouched
	if detectImageMagic(hdr) != "" {
		return jobPath, jobID, cleanName
	}

	var detectedExt string
	isPJL := bytes.HasPrefix(hdr, []byte("\x1b%-12345X")) || bytes.HasPrefix(hdr, []byte("@PJL"))

	if isPJL {
		if payloadStart, ext, ok := findPJLPayloadStart(jobPath, 1024*1024); ok {
			payloadEnd := findPayloadEnd(jobPath, payloadStart, ext)
			if stripFileSlice(jobPath, payloadStart, payloadEnd) == nil {
				detectedExt = ext
			}
		}
	} else if bytes.HasPrefix(hdr, []byte("%PDF-")) {
		detectedExt = ".pdf"
	} else if bytes.HasPrefix(hdr, []byte("PK\x03\x04")) {
		if currExt == ".oxps" {
			detectedExt = ".oxps"
		} else if currExt == ".xps" || isGenericSpoolExt(currExt) {
			detectedExt = ".xps"
		}
	}

	// Never rename valid non-PDF/XPS files when no genuine PDF/XPS stream was detected
	if detectedExt == "" || currExt == detectedExt {
		return jobPath, jobID, cleanName
	}
	if detectedExt == ".xps" && currExt == ".oxps" {
		return jobPath, jobID, cleanName
	}

	newJobPath := strings.TrimSuffix(jobPath, filepath.Ext(jobPath)) + detectedExt
	if renErr := os.Rename(jobPath, newJobPath); renErr == nil {
		jobPath = newJobPath
		jobID = strings.TrimSuffix(jobID, filepath.Ext(jobID)) + detectedExt
		cleanName = strings.TrimSuffix(cleanName, filepath.Ext(cleanName)) + detectedExt
	}
	return jobPath, jobID, cleanName
}

// findPJLPayloadStart scans a PJL spool file in fixed-size chunks (without loading the full file into RAM)
// to locate the start offset of an encapsulated %PDF- or PK\x03\x04 stream.
func findPJLPayloadStart(path string, maxScanBytes int64) (int64, string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, "", false
	}
	defer f.Close()

	const chunkSize = 64 * 1024
	const overlap = 16
	buf := make([]byte, chunkSize+overlap)
	carry := 0
	var offset int64

	for offset < maxScanBytes {
		n, readErr := f.Read(buf[carry : carry+chunkSize])
		if n == 0 {
			break
		}
		window := buf[:carry+n]
		pdfIdx := bytes.Index(window, []byte("%PDF-"))
		xpsIdx := bytes.Index(window, []byte("PK\x03\x04"))

		if pdfIdx >= 0 && (xpsIdx < 0 || pdfIdx < xpsIdx) {
			return offset - int64(carry) + int64(pdfIdx), ".pdf", true
		}
		if xpsIdx >= 0 {
			return offset - int64(carry) + int64(xpsIdx), ".xps", true
		}

		offset += int64(n)
		if readErr != nil {
			break
		}
		if len(window) >= overlap {
			copy(buf[:overlap], window[len(window)-overlap:])
			carry = overlap
		} else {
			copy(buf[:len(window)], window)
			carry = len(window)
		}
	}
	return 0, "", false
}

func findPayloadEnd(path string, start int64, ext string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return -1
	}
	size := fi.Size()
	if start >= size {
		return size
	}

	tailSize := int64(4096)
	if size-start < tailSize {
		tailSize = size - start
	}
	f, err := os.Open(path)
	if err != nil {
		return size
	}
	defer f.Close()

	tailOffset := size - tailSize
	tail := make([]byte, tailSize)
	n, _ := f.ReadAt(tail, tailOffset)
	tail = tail[:n]

	if ext == ".pdf" {
		if eofIdx := bytes.LastIndex(tail, []byte("%%EOF")); eofIdx != -1 {
			end := eofIdx + 5
			for end < len(tail) && (tail[end] == '\r' || tail[end] == '\n' || tail[end] == ' ') {
				end++
			}
			return tailOffset + int64(end)
		}
	} else if ext == ".xps" {
		if eocdIdx := bytes.LastIndex(tail, []byte("PK\x05\x06")); eocdIdx != -1 && len(tail) >= eocdIdx+22 {
			commentLen := int(binary.LittleEndian.Uint16(tail[eocdIdx+20 : eocdIdx+22]))
			zipEnd := eocdIdx + 22 + commentLen
			if zipEnd <= len(tail) {
				return tailOffset + int64(zipEnd)
			}
		}
	}

	if uelIdx := bytes.Index(tail, []byte("\x1b%-12345X")); uelIdx != -1 {
		return tailOffset + int64(uelIdx)
	}
	return size
}

func stripFileSlice(path string, start, end int64) error {
	if start <= 0 && end < 0 {
		return nil
	}
	src, err := os.Open(path)
	if err != nil {
		return err
	}
	fi, err := src.Stat()
	if err != nil {
		_ = src.Close()
		return err
	}
	if end < 0 || end > fi.Size() {
		end = fi.Size()
	}
	if start == 0 && end == fi.Size() {
		_ = src.Close()
		return nil
	}
	if _, err := src.Seek(start, io.SeekStart); err != nil {
		_ = src.Close()
		return err
	}

	tmpPath := path + ".clean"
	dst, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		_ = src.Close()
		return err
	}
	_, copyErr := io.CopyN(dst, src, end-start)
	closeDstErr := dst.Close()
	_ = src.Close()
	if copyErr != nil || closeDstErr != nil {
		_ = os.Remove(tmpPath)
		if copyErr != nil {
			return copyErr
		}
		return closeDstErr
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
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
	Transport: &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   15 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		DisableCompression:    true, // Print streams (XPS/PDF) are pre-compressed; avoids redundant CPU compression latency
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
		ConfigureSumatraDefaults(targetPath)
		return targetPath, nil
	}

	// Secondary fallback: winget if available on Windows
	log.Printf("[Printer] Direct download returned error or incomplete. Attempting winget fallback...")
	cmd := exec.Command("winget", "install", "-e", "--id", "SumatraPDF.SumatraPDF", "--accept-package-agreements", "--accept-source-agreements", "--silent")
	hideCmd(cmd)
	_ = cmd.Run()
	for i := 0; i < 15; i++ {
		if found := FindSumatraPDF(); found != "" {
			ConfigureSumatraDefaults(found)
			return found, nil
		}
		time.Sleep(1 * time.Second)
	}

	return "", errors.New("provisioned SumatraPDF binary failed validation")
}

var (
	sumatraPrintScaleLineRe = regexp.MustCompile(`(?im)^([ \t]*PrintScale[ \t]*=[ \t]*)(\S+)([ \t]*)$`)
	sumatraPrinterDefaultsRe = regexp.MustCompile(`(?im)^([ \t]*PrinterDefaults[ \t]*\[[ \t]*\r?\n)`)
)

// ConfigureSumatraDefaults ensures SumatraPDF-settings.txt uses valid "PrintScale = none"
// without clobbering existing user settings.
func ConfigureSumatraDefaults(exePath string) {
	if exePath == "" {
		exePath = FindSumatraPDF()
	}
	if exePath == "" {
		exePath = `C:\BridgeApp\SumatraPDF.exe`
	}

	paths := []string{
		filepath.Join(filepath.Dir(exePath), "SumatraPDF-settings.txt"),
		`C:\BridgeApp\SumatraPDF-settings.txt`,
	}

	// User profile AppData locations (create default if missing, or patch PrintScale cleanly)
	if appData := os.Getenv("APPDATA"); appData != "" {
		paths = append(paths, filepath.Join(appData, "SumatraPDF", "SumatraPDF-settings.txt"))
	}
	if localAppData := os.Getenv("LOCALAPPDATA"); localAppData != "" {
		paths = append(paths, filepath.Join(localAppData, "SumatraPDF", "SumatraPDF-settings.txt"))
	}

	configureSumatraSettingsAt(paths)
}

func configureSumatraSettingsAt(paths []string) {
	seen := make(map[string]bool, len(paths))
	for _, p := range paths {
		if p == "" {
			continue
		}
		clean := filepath.Clean(p)
		if seen[clean] {
			continue
		}
		seen[clean] = true
		applySumatraSettingsFile(clean)
	}
}

func applySumatraSettingsFile(settingsPath string) {
	const defaultContent = "Theme = Light\nPrinterDefaults [\n\tPrintScale = none\n]\n"

	existing, err := os.ReadFile(settingsPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return
		}
		if mkErr := os.MkdirAll(filepath.Dir(settingsPath), 0755); mkErr != nil {
			return
		}
		_ = os.WriteFile(settingsPath, []byte(defaultContent), 0644)
		return
	}

	s := string(existing)
	updated := s
	if sumatraPrintScaleLineRe.MatchString(s) {
		updated = sumatraPrintScaleLineRe.ReplaceAllString(s, "${1}none${3}")
	} else if sumatraPrinterDefaultsRe.MatchString(s) {
		updated = sumatraPrinterDefaultsRe.ReplaceAllString(s, "${1}\tPrintScale = none\n")
	} else {
		if updated != "" && !strings.HasSuffix(updated, "\n") {
			updated += "\n"
		}
		updated += "PrinterDefaults [\n\tPrintScale = none\n]\n"
	}

	if updated != s {
		_ = os.WriteFile(settingsPath, []byte(updated), 0644)
	}
}

// CreateSumatraShortcut is kept for API compatibility; SumatraPDF runs silently in the background
// with no shortcuts, so this validates exePath and removes any stale SumatraPDF.lnk files.
func CreateSumatraShortcut(exePath string) error {
	if exePath == "" || !isValidSumatraExe(exePath) {
		return errors.New("invalid SumatraPDF executable path")
	}

	targets := []string{`C:\BridgeApp\SumatraPDF.lnk`}
	for _, d := range getDesktopDirsForShortcuts() {
		targets = append(targets, filepath.Join(d, "SumatraPDF.lnk"))
	}
	removeStaleSumatraShortcuts(targets)
	return nil
}

func removeStaleSumatraShortcuts(paths []string) {
	for _, p := range paths {
		if p == "" {
			continue
		}
		if err := os.Remove(p); err == nil {
			log.Printf("[Printer] Removed stale SumatraPDF shortcut: %s", p)
		}
	}
}

func (ps *PrintService) getFindSumatra() func() string {
	if ps.findSumatra != nil {
		return ps.findSumatra
	}
	return FindSumatraPDF
}

func (ps *PrintService) getEnsureSumatra() func() (string, error) {
	if ps.ensureSumatra != nil {
		return ps.ensureSumatra
	}
	return EnsureSumatraPDF
}

func (ps *PrintService) failJob(job PrintJob, reason string) {
	ps.moveToFailed(job, reason)
	if ps.OnJobFailed != nil {
		ps.OnJobFailed(job, reason)
	}
}

func (ps *PrintService) processJob(job PrintJob) {
	log.Printf("[Printer] Processing job %s (%s)...", job.ID, job.FileName)
	if ps.OnJobStarted != nil {
		ps.OnJobStarted(job)
	}

	targetPrinter := strings.TrimSpace(ps.getTargetPrinter())
	sumatraBin := ps.getSumatraPath()
	var err error
	if targetPrinter == "" {
		if ps.resolveTargetPrinter != nil {
			targetPrinter, err = ps.resolveTargetPrinter()
		} else {
			targetPrinter, err = GetTargetPhysicalPrinter()
			if err != nil && strings.Contains(strings.ToLower(sumatraBin), "mock") {
				targetPrinter = "MockPrinter"
				err = nil
			}
		}
		targetPrinter = strings.TrimSpace(targetPrinter)
	}

	if err != nil || targetPrinter == "" {
		reason := "no physical printer available"
		if err != nil {
			reason = fmt.Sprintf("no physical printer available: %v", err)
		}
		log.Printf("[Printer] ERROR: Job %s (%s) failed — no target physical printer found (%s)", job.ID, job.FileName, reason)
		ps.failJob(job, reason)
		return
	}

	log.Printf("[Printer] Target physical printer selected for job %s (%s): '%s'", job.ID, job.FileName, targetPrinter)

	// Locate effective SumatraPDF executable: check configured path, then auto-discover
	if !isUsableSumatra(sumatraBin) {
		if discovered := ps.getFindSumatra()(); isUsableSumatra(discovered) {
			sumatraBin = discovered
			ps.setSumatraPath(discovered)
		} else {
			// Auto-heal on the fly: attempt standalone engine download if missing
			if provisioned, provErr := ps.getEnsureSumatra()(); provErr == nil && isUsableSumatra(provisioned) {
				sumatraBin = provisioned
				ps.setSumatraPath(provisioned)
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
		log.Printf("[Printer] Job %s (%s): printing via SumatraPDF to target printer '%s' (settings: %s)...", job.ID, job.FileName, targetPrinter, printSettings)
		// Dynamic print settings preserve 100% 1:1 scale, exact page count, exact paper feeding (A4/Letter), and orientation
		cmd := exec.CommandContext(ctx, sumatraBin, "-print-to", targetPrinter, "-silent", "-print-settings", printSettings, "-exit-when-done", job.FilePath)
		hideCmd(cmd)
		out, err = cmd.CombinedOutput()
		if err != nil && ctx.Err() == nil && strings.EqualFold(filepath.Ext(job.FilePath), ".xps") {
			log.Printf("[Printer] Job %s (%s): SumatraPDF returned error (%v) for target printer '%s'; attempting native Windows .NET XPS fallback...", job.ID, job.FileName, err, targetPrinter)
			out, err = printNativeXPS(ctx, targetPrinter, job.FilePath, geom)
		}
	} else if strings.EqualFold(filepath.Ext(job.FilePath), ".xps") {
		log.Printf("[Printer] Job %s (%s): SumatraPDF not found; printing via native Windows .NET XPS to target printer '%s' (paper: %s, orientation: %s)...", job.ID, job.FileName, targetPrinter, geom.PaperName, geom.Orientation)
		out, err = printNativeXPS(ctx, targetPrinter, job.FilePath, geom)
	} else {
		// Strict safeguard: NEVER execute shell PrintTo with Notepad (which dumps raw binary bytes as text across 20 pages)
		log.Printf("[Printer] ERROR: Job %s (%s) failed for target printer '%s': silent print engine (SumatraPDF) not installed on host.", job.ID, job.FileName, targetPrinter)
		err = fmt.Errorf("silent print engine (SumatraPDF) is missing on host; run setup or install to auto-provision")
	}

	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		log.Printf("[Printer] ERROR: Job %s (%s) timed out after %v on target printer '%s'! Killing process...", job.ID, job.FileName, ps.timeout, targetPrinter)
		ps.failJob(job, "timeout exceeded")
		return
	}

	if err != nil {
		log.Printf("[Printer] ERROR: Job %s (%s) failed on target printer '%s': %v, out: %s", job.ID, job.FileName, targetPrinter, err, strings.TrimSpace(string(out)))
		ps.failJob(job, err.Error())
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
	log.Printf("[Printer] Job %s (%s) printed successfully to target printer '%s' and cleaned up.", job.ID, job.FileName, targetPrinter)
	if ps.OnJobCompleted != nil {
		ps.OnJobCompleted(job)
	}
}

func (ps *PrintService) moveToFailed(job PrintJob, reason string) {
	destPath := filepath.Join(ps.failedDir, job.ID)
	_ = os.Rename(job.FilePath, destPath)
	log.Printf("[Printer] Moved failed job %s to %s (reason: %s)", job.ID, destPath, reason)
}

func printNativeXPS(ctx context.Context, targetPrinter, filePath string, geom DocumentGeometry) ([]byte, error) {
	psScript := fmt.Sprintf(`
Add-Type -AssemblyName ReachFramework, System.Printing
$ps = New-Object System.Printing.LocalPrintServer
$pq = $ps.GetPrintQueue('%s')
$writer = [System.Printing.PrintQueue]::CreateXpsDocumentWriter($pq)
$xpsDoc = New-Object System.Windows.Xps.Packaging.XpsDocument('%s', [System.IO.FileAccess]::Read)
$ticket = $pq.UserPrintTicket
if (-not $ticket) { $ticket = $pq.DefaultPrintTicket }
if ($ticket) { $ticket = $ticket.Clone() } else { $ticket = New-Object System.Printing.PrintTicket }
if ('%s' -eq 'landscape') {
    $ticket.PageOrientation = [System.Printing.PageOrientation]::Landscape
} else {
    $ticket.PageOrientation = [System.Printing.PageOrientation]::Portrait
}
$paper = '%s'
if ($paper -eq 'A4') {
    $ticket.PageMediaSize = New-Object System.Printing.PageMediaSize([System.Printing.PageMediaSizeName]::ISOA4)
} elseif ($paper -eq 'A3') {
    $ticket.PageMediaSize = New-Object System.Printing.PageMediaSize([System.Printing.PageMediaSizeName]::ISOA3)
} elseif ($paper -eq 'A2') {
    $ticket.PageMediaSize = New-Object System.Printing.PageMediaSize([System.Printing.PageMediaSizeName]::ISOA2)
} elseif ($paper -eq 'A1') {
    $ticket.PageMediaSize = New-Object System.Printing.PageMediaSize([System.Printing.PageMediaSizeName]::ISOA1)
} elseif ($paper -eq 'A0') {
    $ticket.PageMediaSize = New-Object System.Printing.PageMediaSize([System.Printing.PageMediaSizeName]::ISOA0)
} elseif ($paper -eq 'A5') {
    $ticket.PageMediaSize = New-Object System.Printing.PageMediaSize([System.Printing.PageMediaSizeName]::ISOA5)
} elseif ($paper -eq 'Letter') {
    $ticket.PageMediaSize = New-Object System.Printing.PageMediaSize([System.Printing.PageMediaSizeName]::NorthAmericaLetter)
} elseif ($paper -eq 'Legal') {
    $ticket.PageMediaSize = New-Object System.Printing.PageMediaSize([System.Printing.PageMediaSizeName]::NorthAmericaLegal)
} elseif ($paper -eq 'Tabloid') {
    $ticket.PageMediaSize = New-Object System.Printing.PageMediaSize([System.Printing.PageMediaSizeName]::NorthAmericaTabloid)
}
$ticket.PageScalingFactor = 100
$writer.Write($xpsDoc.GetFixedDocumentSequence(), $ticket)
$xpsDoc.Close()
`, strings.ReplaceAll(targetPrinter, `'`, `''`),
		strings.ReplaceAll(filePath, `'`, `''`),
		geom.Orientation,
		geom.PaperName)
	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", psScript)
	hideCmd(cmd)
	return cmd.CombinedOutput()
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

func validatePeerHTTPURL(peerURL string) (string, error) {
	trimmed := strings.TrimSpace(peerURL)
	if trimmed == "" {
		return "", errors.New("invalid peer URL: peer PC is not connected yet (blank URL)")
	}
	if strings.ContainsAny(trimmed, " \t\r\n") {
		return "", fmt.Errorf("invalid peer URL %q: contains whitespace", peerURL)
	}
	cleanPeer := strings.TrimRight(trimmed, "/")
	if cleanPeer == "" {
		return "", errors.New("invalid peer URL: peer PC is not connected yet (blank URL)")
	}
	u, err := url.Parse(cleanPeer)
	if err != nil {
		return "", fmt.Errorf("invalid peer URL %q: %w", peerURL, err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("invalid peer URL %q: unsupported scheme %q (expected http or https)", peerURL, u.Scheme)
	}
	if u.Hostname() == "" {
		return "", fmt.Errorf("invalid peer URL %q: missing host", peerURL)
	}
	if p := u.Port(); p != "" {
		portNum, convErr := strconv.Atoi(p)
		if convErr != nil || portNum < 1 || portNum > 65535 {
			return "", fmt.Errorf("invalid peer URL %q: invalid port %q", peerURL, p)
		}
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("invalid peer URL %q: query parameters and fragments are not allowed", peerURL)
	}
	u.Scheme = scheme
	return strings.TrimRight(u.String(), "/"), nil
}

// SendDirectPrint directly uploads a document file to the peer's print endpoint.
// Optional key-value header pairs (e.g. "X-Bridge-Node-Name", nodeName) can be provided.
func SendDirectPrint(ctx context.Context, peerURL, authToken, filePath string, extraHeaders ...string) error {
	cleanPeer, err := validatePeerHTTPURL(peerURL)
	if err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}

	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("failed to open file %s: %w", filePath, err)
	}
	defer f.Close()

	if fi, statErr := f.Stat(); statErr == nil && fi.IsDir() {
		return fmt.Errorf("print target %s is a directory, expected a file", filePath)
	}

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
