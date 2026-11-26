package printer

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestIterativeFidelityLoop runs a comprehensive verification suite across all document formats,
// paper sizes, orientations, and scaling scenarios until 100% accuracy is validated.
func TestIterativeFidelityLoop(t *testing.T) {
	tempDir := t.TempDir()

	// 1. Build Mock Sumatra Logger
	mockSrc := filepath.Join(tempDir, "mock_sumatra.go")
	mockExe := filepath.Join(tempDir, "mock_sumatra.exe")
	logFile := filepath.Join(tempDir, "sumatra_calls.log")

	mockCode := fmt.Sprintf(`package main
import (
	"os"
	"strings"
)
func main() {
	f, _ := os.OpenFile(%q, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	defer f.Close()
	f.WriteString(strings.Join(os.Args[1:], " ") + "\n")
	os.Exit(0)
}
`, logFile)

	if err := os.WriteFile(mockSrc, []byte(mockCode), 0644); err != nil {
		t.Fatalf("Failed to write mock sumatra src: %v", err)
	}

	buildCmd := exec.Command("go", "build", "-o", mockExe, mockSrc)
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("Failed to compile mock sumatra: %v\n%s", err, string(out))
	}

	testCases := []struct {
		name            string
		generator       func(path string) error
		wantFormat      string
		wantPaper       string
		wantOrientation string
		wantSettings    string
		wantPages       int
	}{
		{
			name: "A4_Portrait_300DPI_PDF",
			generator: func(p string) error {
				content := `%PDF-1.4
1 0 obj
<< /Type /Catalog /Pages 2 0 R >>
endobj
2 0 obj
<< /Type /Pages /Kids [3 0 R] /Count 1 >>
endobj
3 0 obj
<< /Type /Page /Parent 2 0 R /MediaBox [0 0 595.28 841.89] >>
endobj
xref
0 4
trailer
<< /Size 4 /Root 1 0 R >>
%%EOF`
				return os.WriteFile(p, []byte(content), 0644)
			},
			wantFormat:      ".pdf",
			wantPaper:       "A4",
			wantOrientation: "portrait",
			wantSettings:    "1-1,paper=A4,portrait,noscale",
			wantPages:       1,
		},
		{
			name: "A4_Landscape_PDF",
			generator: func(p string) error {
				content := `%PDF-1.4
1 0 obj
<< /Type /Catalog /Pages 2 0 R >>
endobj
2 0 obj
<< /Type /Pages /Kids [3 0 R] /Count 1 >>
endobj
3 0 obj
<< /Type /Page /Parent 2 0 R /MediaBox [0 0 841.89 595.28] >>
endobj
xref
0 4
trailer
<< /Size 4 /Root 1 0 R >>
%%EOF`
				return os.WriteFile(p, []byte(content), 0644)
			},
			wantFormat:      ".pdf",
			wantPaper:       "A4",
			wantOrientation: "landscape",
			wantSettings:    "1-1,paper=A4,landscape,noscale",
			wantPages:       1,
		},
		{
			name: "US_Letter_Portrait_PDF",
			generator: func(p string) error {
				content := `%PDF-1.4
1 0 obj
<< /Type /Catalog /Pages 2 0 R >>
endobj
2 0 obj
<< /Type /Pages /Kids [3 0 R] /Count 1 >>
endobj
3 0 obj
<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612.0 792.0] >>
endobj
xref
0 4
trailer
<< /Size 4 /Root 1 0 R >>
%%EOF`
				return os.WriteFile(p, []byte(content), 0644)
			},
			wantFormat:      ".pdf",
			wantPaper:       "Letter",
			wantOrientation: "portrait",
			wantSettings:    "1-1,paper=Letter,portrait,noscale",
			wantPages:       1,
		},
		{
			name: "A4_XPS_Spool_Document",
			generator: func(p string) error {
				f, err := os.Create(p)
				if err != nil {
					return err
				}
				defer f.Close()
				zw := zip.NewWriter(f)
				w, _ := zw.Create("Documents/1/Pages/1.fpage")
				_, _ = w.Write([]byte(`<FixedPage xmlns="http://schemas.microsoft.com/xps/2005/06" Width="793.76" Height="1122.56" />`))
				return zw.Close()
			},
			wantFormat:      ".xps",
			wantPaper:       "A4",
			wantOrientation: "portrait",
			wantSettings:    "1-1,paper=A4,portrait,noscale",
			wantPages:       1,
		},
		{
			name: "MultiPage_2Page_A4_PDF",
			generator: func(p string) error {
				content := `%PDF-1.4
1 0 obj
<< /Type /Catalog /Pages 2 0 R >>
endobj
2 0 obj
<< /Type /Pages /Kids [3 0 R 4 0 R] /Count 2 >>
endobj
3 0 obj
<< /Type /Page /Parent 2 0 R /MediaBox [0 0 595.28 841.89] >>
endobj
4 0 obj
<< /Type /Page /Parent 2 0 R /MediaBox [0 0 595.28 841.89] >>
endobj
xref
0 5
trailer
<< /Size 5 /Root 1 0 R >>
%%EOF`
				return os.WriteFile(p, []byte(content), 0644)
			},
			wantFormat:      ".pdf",
			wantPaper:       "A4",
			wantOrientation: "portrait",
			wantSettings:    "1-2,paper=A4,portrait,noscale",
			wantPages:       2,
		},
	}

	// Run test loop 3 iterations to guarantee rock-solid idempotency
	for iteration := 1; iteration <= 3; iteration++ {
		t.Logf("=== Starting Verification Loop Iteration %d/3 ===", iteration)

		for _, tc := range testCases {
			t.Run(fmt.Sprintf("Iter%d_%s", iteration, tc.name), func(t *testing.T) {
				filePath := filepath.Join(tempDir, fmt.Sprintf("%s_iter%d%s", tc.name, iteration, tc.wantFormat))
				if err := tc.generator(filePath); err != nil {
					t.Fatalf("Failed to generate test file: %v", err)
				}

				// Verify Geometry Detection
				geom := DetectDocumentGeometry(filePath)
				if geom.Format != tc.wantFormat {
					t.Errorf("[%s] Expected format %s, got %s", tc.name, tc.wantFormat, geom.Format)
				}
				if geom.PaperName != tc.wantPaper {
					t.Errorf("[%s] Expected paper %s, got %s", tc.name, tc.wantPaper, geom.PaperName)
				}
				if geom.Orientation != tc.wantOrientation {
					t.Errorf("[%s] Expected orientation %s, got %s", tc.name, tc.wantOrientation, geom.Orientation)
				}
				if geom.PageCount != tc.wantPages {
					t.Errorf("[%s] Expected pages %d, got %d", tc.name, tc.wantPages, geom.PageCount)
				}

				settings := BuildPrintSettings(geom)
				if settings != tc.wantSettings {
					t.Errorf("[%s] Expected print settings %q, got %q", tc.name, tc.wantSettings, settings)
				}

				// Execute PrintService with Mock Sumatra
				qDir := filepath.Join(tempDir, "q", tc.name)
				fDir := filepath.Join(tempDir, "f", tc.name)
				ps := NewPrintService(mockExe, 5, qDir, fDir)
				ps.SetTargetPrinter("HP_Smart_Tank_580")
				if err := ps.Start(); err != nil {
					t.Fatalf("Failed to start PrintService: %v", err)
				}
				defer ps.Stop()

				_ = os.Remove(logFile)

				job := PrintJob{
					ID:         fmt.Sprintf("job_%s_%d", tc.name, iteration),
					FileName:   filepath.Base(filePath),
					FilePath:   filePath,
					EnqueuedAt: time.Now(),
				}

				done := make(chan bool, 1)
				ps.OnJobCompleted = func(j PrintJob) {
					if j.ID == job.ID {
						done <- true
					}
				}

				if err := ps.Enqueue(job); err != nil {
					t.Fatalf("Enqueue failed: %v", err)
				}

				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Fatalf("Print job timed out")
				}

				logBytes, _ := os.ReadFile(logFile)
				callArgs := string(logBytes)

				if !strings.Contains(callArgs, fmt.Sprintf("-print-settings %s", tc.wantSettings)) {
					t.Errorf("[%s] Mock called with args %q, expected settings %q", tc.name, callArgs, tc.wantSettings)
				}
				if strings.Contains(callArgs, "-print-settings fit") {
					t.Errorf("[%s] FAILED: Argument contained forced 'fit' which distorts 100%% scaling!", tc.name)
				}
			})
		}
	}
}

// TestRemotePrintHeaderPropagation verifies that SendDirectPrint inspects geometry
// and attaches X-Bridge-Paper-Size and X-Bridge-Orientation headers to remote HTTP requests.
func TestRemotePrintHeaderPropagation(t *testing.T) {
	tempDir := t.TempDir()
	a4PdfPath := filepath.Join(tempDir, "remote_a4.pdf")

	a4Content := `%PDF-1.4
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
	_ = os.WriteFile(a4PdfPath, []byte(a4Content), 0644)

	var receivedPaper, receivedOrient string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPaper = r.Header.Get("X-Bridge-Paper-Size")
		receivedOrient = r.Header.Get("X-Bridge-Orientation")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"queued","job_id":"test_1"}`))
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := SendDirectPrint(ctx, server.URL, "dummy_token", a4PdfPath)
	if err != nil {
		t.Fatalf("SendDirectPrint failed: %v", err)
	}

	if receivedPaper != "A4" {
		t.Errorf("Expected X-Bridge-Paper-Size 'A4', got: %q", receivedPaper)
	}
	if receivedOrient != "portrait" {
		t.Errorf("Expected X-Bridge-Orientation 'portrait', got: %q", receivedOrient)
	}
}

func TestSaveIncomingJob_NonPDFPreservationAndLargePJL(t *testing.T) {
	tempDir := t.TempDir()
	qDir := filepath.Join(tempDir, "queue")
	fDir := filepath.Join(tempDir, "failed")
	_ = os.MkdirAll(qDir, 0755)
	_ = os.MkdirAll(fDir, 0755)

	ps := NewPrintService("mock_sumatra.exe", 5, qDir, fDir)

	// 1. Valid PNG must keep .png extension and exact bytes (never renamed to .pdf)
	var pngBuf bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 10, 10))
	img.Set(0, 0, color.RGBA{G: 200, A: 255})
	if err := png.Encode(&pngBuf, img); err != nil {
		t.Fatalf("png.Encode failed: %v", err)
	}
	pngJob, err := ps.SaveIncomingJob("photo.png", bytes.NewReader(pngBuf.Bytes()))
	if err != nil {
		t.Fatalf("SaveIncomingJob(photo.png) failed: %v", err)
	}
	if filepath.Ext(pngJob.FilePath) != ".png" || pngJob.FileName != "photo.png" {
		t.Errorf("Expected .png preservation, got path=%q name=%q", pngJob.FilePath, pngJob.FileName)
	}
	savedPNG, _ := os.ReadFile(pngJob.FilePath)
	if !bytes.Equal(savedPNG, pngBuf.Bytes()) {
		t.Errorf("PNG bytes were altered by SaveIncomingJob")
	}

	// 2. Valid JPEG bytes must keep .jpg extension
	jpgBytes := []byte("\xff\xd8\xff\xe0\x00\x10JFIF\x00\x01\x01\x00\x00\x01\x00\x01\x00\x00\xff\xd9")
	jpgJob, err := ps.SaveIncomingJob("scan.jpg", bytes.NewReader(jpgBytes))
	if err != nil {
		t.Fatalf("SaveIncomingJob(scan.jpg) failed: %v", err)
	}
	if filepath.Ext(jpgJob.FilePath) != ".jpg" {
		t.Errorf("Expected .jpg preservation, got %q", jpgJob.FilePath)
	}

	// 3. Non-PDF/non-XPS arbitrary file must NOT be renamed to .pdf
	txtBytes := []byte("plain text receipt\nline 2\n")
	txtJob, err := ps.SaveIncomingJob("notes.txt", bytes.NewReader(txtBytes))
	if err != nil {
		t.Fatalf("SaveIncomingJob(notes.txt) failed: %v", err)
	}
	if filepath.Ext(txtJob.FilePath) != ".txt" {
		t.Errorf("Expected .txt preservation, got %q", txtJob.FilePath)
	}

	// 4. Genuine PJL wrapper > 4096 bytes wrapping a PDF must strip PJL and rename to .pdf
	var pjlBuf bytes.Buffer
	pjlBuf.WriteString("\x1b%-12345X@PJL JOB\r\n")
	for pjlBuf.Len() < 6000 {
		pjlBuf.WriteString("@PJL COMMENT PADDING HEADER LINE FOR LARGE SPOOL TEST\r\n")
	}
	pjlBuf.WriteString("@PJL ENTER LANGUAGE = PDF\r\n")
	pdfPayload := []byte("%PDF-1.4\n1 0 obj\n<< /Type /Catalog >>\nendobj\n%%EOF\n")
	pjlBuf.Write(pdfPayload)
	pjlBuf.WriteString("\x1b%-12345X@PJL EOJ\r\n\x1b%-12345X")

	pjlJob, err := ps.SaveIncomingJob("spool_job.prn", bytes.NewReader(pjlBuf.Bytes()))
	if err != nil {
		t.Fatalf("SaveIncomingJob(spool_job.prn) failed: %v", err)
	}
	if filepath.Ext(pjlJob.FilePath) != ".pdf" {
		t.Errorf("Expected PJL-wrapped PDF to normalize to .pdf, got %q", pjlJob.FilePath)
	}
	cleaned, _ := os.ReadFile(pjlJob.FilePath)
	if !bytes.HasPrefix(cleaned, []byte("%PDF-")) || bytes.Contains(cleaned, []byte("@PJL")) {
		t.Errorf("Expected PJL header/trailer stripped from PDF, got prefix=%q", string(cleaned[:minInt(len(cleaned), 32)]))
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestPrintQueueStartupRecovery(t *testing.T) {
	tempDir := t.TempDir()

	mockSrc := filepath.Join(tempDir, "mock_sumatra.go")
	mockExe := filepath.Join(tempDir, "mock_sumatra.exe")
	logFile := filepath.Join(tempDir, "recovery_calls.log")
	mockCode := fmt.Sprintf(`package main
import (
	"os"
	"strings"
)
func main() {
	f, _ := os.OpenFile(%q, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	defer f.Close()
	f.WriteString(strings.Join(os.Args[1:], " ") + "\n")
}
`, logFile)
	_ = os.WriteFile(mockSrc, []byte(mockCode), 0644)
	if out, err := exec.Command("go", "build", "-o", mockExe, mockSrc).CombinedOutput(); err != nil {
		t.Fatalf("Failed to build mock sumatra: %v\n%s", err, string(out))
	}

	qDir := filepath.Join(tempDir, "PrintQueue")
	fDir := filepath.Join(tempDir, "PrintFailed")
	_ = os.MkdirAll(qDir, 0755)
	_ = os.MkdirAll(fDir, 0755)

	orphanPDF := filepath.Join(qDir, "1700000000_recovered.pdf")
	pdfData := "%PDF-1.4\n1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n3 0 obj\n<< /Type /Page /Parent 2 0 R /MediaBox [0 0 595.28 841.89] >>\nendobj\ntrailer\n<< /Root 1 0 R >>\n%%EOF"
	_ = os.WriteFile(orphanPDF, []byte(pdfData), 0644)

	// Invalid/temp files that must be ignored on startup
	_ = os.WriteFile(filepath.Join(qDir, "partial.tmp"), []byte(pdfData), 0644)
	_ = os.WriteFile(filepath.Join(qDir, "empty.pdf"), []byte{}, 0644)

	ps := NewPrintService(mockExe, 5, qDir, fDir)
	ps.SetTargetPrinter("Mock_Recovery_Printer")

	done := make(chan PrintJob, 1)
	ps.OnJobCompleted = func(job PrintJob) {
		done <- job
	}

	if err := ps.Start(); err != nil {
		t.Fatalf("PrintService.Start failed: %v", err)
	}
	defer ps.Stop()

	select {
	case job := <-done:
		if job.ID != "1700000000_recovered.pdf" || job.FileName != "recovered.pdf" {
			t.Errorf("Unexpected recovered job metadata: %+v", job)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Timed out waiting for orphaned queue job recovery")
	}

	logBytes, _ := os.ReadFile(logFile)
	if !strings.Contains(string(logBytes), "1700000000_recovered.pdf") {
		t.Errorf("Expected mock sumatra to print recovered job, got log: %q", string(logBytes))
	}
	if strings.Contains(string(logBytes), "partial.tmp") || strings.Contains(string(logBytes), "empty.pdf") {
		t.Errorf("Temp or empty file should not have been printed, got log: %q", string(logBytes))
	}
}

func TestConfigureSumatraDefaults_PreservesUserSettings(t *testing.T) {
	tempDir := t.TempDir()

	// 1. Existing user file: preserve user settings and fix invalid PrintScale
	userCfg := filepath.Join(tempDir, "existing", "SumatraPDF-settings.txt")
	_ = os.MkdirAll(filepath.Dir(userCfg), 0755)
	initial := "MainWindowBackground = #fff200\nZoom = 125%\nPrinterDefaults [\n\tPrintScale = shrink\n]\n"
	_ = os.WriteFile(userCfg, []byte(initial), 0644)

	applySumatraSettingsFile(userCfg)

	gotBytes, err := os.ReadFile(userCfg)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	got := string(gotBytes)
	if !strings.Contains(got, "MainWindowBackground = #fff200") || !strings.Contains(got, "Zoom = 125%") {
		t.Errorf("User settings were clobbered! Got:\n%s", got)
	}
	if !strings.Contains(got, "PrintScale = none") || strings.Contains(got, "PrintScale = noscale") {
		t.Errorf("Expected valid 'PrintScale = none' without 'noscale', got:\n%s", got)
	}

	// 2. Missing file: creates default with valid PrintScale = none
	newCfg := filepath.Join(tempDir, "fresh", "SumatraPDF-settings.txt")
	applySumatraSettingsFile(newCfg)
	freshBytes, err := os.ReadFile(newCfg)
	if err != nil {
		t.Fatalf("Expected default config to be created: %v", err)
	}
	if !strings.Contains(string(freshBytes), "PrintScale = none") || strings.Contains(string(freshBytes), "noscale") {
		t.Errorf("Expected fresh config to contain 'PrintScale = none', got:\n%s", string(freshBytes))
	}

	// 3. Duplicate PrintScale lines (including CRLF endings & shrink) are deduplicated to a single PrintScale = none
	dupCfg := filepath.Join(tempDir, "dup", "SumatraPDF-settings.txt")
	_ = os.MkdirAll(filepath.Dir(dupCfg), 0755)
	dupInitial := "Theme = Light\r\nPrinterDefaults [\r\n\tPrintScale = none\r\n\tPrintScale = shrink\r\n\tPrintScale = fit\r\n\tPrintMargin = 0 0 0 0\r\n]\r\n"
	_ = os.WriteFile(dupCfg, []byte(dupInitial), 0644)

	applySumatraSettingsFile(dupCfg)

	dupGotBytes, err := os.ReadFile(dupCfg)
	if err != nil {
		t.Fatalf("ReadFile dupCfg failed: %v", err)
	}
	dupGot := string(dupGotBytes)
	if count := strings.Count(dupGot, "PrintScale"); count != 1 {
		t.Errorf("Expected exactly 1 PrintScale line after deduplication, got %d:\n%s", count, dupGot)
	}
	if !strings.Contains(dupGot, "PrintScale = none") || strings.Contains(dupGot, "shrink") || strings.Contains(dupGot, "fit") {
		t.Errorf("Expected single 'PrintScale = none' without shrink/fit, got:\n%s", dupGot)
	}
	if !strings.Contains(dupGot, "PrintMargin = 0 0 0 0") || !strings.Contains(dupGot, "Theme = Light") {
		t.Errorf("Expected unrelated settings preserved after deduplication, got:\n%s", dupGot)
	}
}

func TestSumatraShortcutRemovalAndNoCreation(t *testing.T) {
	tempDir := t.TempDir()

	// Empty or missing exe must still fail validation
	if err := CreateSumatraShortcut(""); err == nil {
		t.Fatalf("Expected CreateSumatraShortcut(\"\") to return an error")
	}

	// Create valid (> 1MB) dummy executable inside tempDir
	dummyExe := filepath.Join(tempDir, "BridgeApp", "SumatraPDF.exe")
	if err := os.MkdirAll(filepath.Dir(dummyExe), 0755); err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}
	dummyData := make([]byte, 1024*1024+64)
	copy(dummyData, "MZ")
	if err := os.WriteFile(dummyExe, dummyData, 0755); err != nil {
		t.Fatalf("WriteFile dummyExe failed: %v", err)
	}

	// Stage stale SumatraPDF.lnk files in USERPROFILE Desktop locations
	userProfile := filepath.Join(tempDir, "UserProfile")
	desktopLnk := filepath.Join(userProfile, "Desktop", "SumatraPDF.lnk")
	oneDriveLnk := filepath.Join(userProfile, "OneDrive", "Desktop", "SumatraPDF.lnk")
	for _, p := range []string{desktopLnk, oneDriveLnk} {
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatalf("MkdirAll failed: %v", err)
		}
		if err := os.WriteFile(p, []byte("stale-shortcut"), 0644); err != nil {
			t.Fatalf("WriteFile stale shortcut failed: %v", err)
		}
	}

	// Also verify removeStaleSumatraShortcuts cleans arbitrary stale app-dir shortcut paths
	staleAppLnk := filepath.Join(filepath.Dir(dummyExe), "SumatraPDF.lnk")
	if err := os.WriteFile(staleAppLnk, []byte("stale-app-shortcut"), 0644); err != nil {
		t.Fatalf("WriteFile staleAppLnk failed: %v", err)
	}
	removeStaleSumatraShortcuts([]string{staleAppLnk})
	if _, err := os.Stat(staleAppLnk); !os.IsNotExist(err) {
		t.Errorf("Expected stale app shortcut %s to be removed", staleAppLnk)
	}

	t.Setenv("USERPROFILE", userProfile)
	if err := CreateSumatraShortcut(dummyExe); err != nil {
		t.Fatalf("CreateSumatraShortcut failed: %v", err)
	}

	// Stale desktop shortcuts must be removed and no shortcut created anywhere
	for _, p := range []string{desktopLnk, oneDriveLnk, staleAppLnk} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("Expected no SumatraPDF.lnk at %s, stat err=%v", p, err)
		}
	}

	// Fresh profile with no Desktop dir must NOT get a Desktop/SumatraPDF.lnk created
	freshProfile := filepath.Join(tempDir, "FreshProfile")
	t.Setenv("USERPROFILE", freshProfile)
	if err := CreateSumatraShortcut(dummyExe); err != nil {
		t.Fatalf("CreateSumatraShortcut on fresh profile failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(freshProfile, "Desktop", "SumatraPDF.lnk")); !os.IsNotExist(err) {
		t.Errorf("CreateSumatraShortcut must not create SumatraPDF.lnk")
	}

	// If SumatraPDF is installed locally, verify EnsureSumatraPDF does not create shortcuts either
	if existing := FindSumatraPDF(); existing != "" {
		found, err := EnsureSumatraPDF()
		if err != nil {
			t.Fatalf("EnsureSumatraPDF returned unexpected error: %v", err)
		}
		if found == "" {
			t.Fatalf("Expected non-empty SumatraPDF path")
		}
		if _, err := os.Stat(filepath.Join(freshProfile, "Desktop", "SumatraPDF.lnk")); !os.IsNotExist(err) {
			t.Errorf("EnsureSumatraPDF must not create Desktop SumatraPDF.lnk")
		}
		if _, err := os.Stat(filepath.Join(filepath.Dir(found), "SumatraPDF.lnk")); !os.IsNotExist(err) {
			t.Errorf("EnsureSumatraPDF must not create SumatraPDF.lnk alongside %s", found)
		}
	}
}

func TestSendDirectPrint_URLValidationAndTransport(t *testing.T) {
	tempDir := t.TempDir()
	pdfPath := filepath.Join(tempDir, "sample.pdf")
	pdfContent := "%PDF-1.4\n1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n3 0 obj\n<< /Type /Page /Parent 2 0 R /MediaBox [0 0 595.28 841.89] >>\nendobj\ntrailer\n<< /Root 1 0 R >>\n%%EOF"
	if err := os.WriteFile(pdfPath, []byte(pdfContent), 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	invalidURLs := []string{
		"",
		"   ",
		"/",
		"127.0.0.1:8765",
		"ftp://127.0.0.1:8765",
		"://missing-scheme",
		"http://",
		"https:///",
		"http://127.0.0.1:99999",
		"http://127.0.0.1:8765?query=1",
		"http://127.0.0.1:8765#frag",
		"http://127.0.0.1:8765/has space",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	for _, badURL := range invalidURLs {
		err := SendDirectPrint(ctx, badURL, "tok", pdfPath)
		if err == nil {
			t.Errorf("Expected SendDirectPrint to reject invalid peerURL %q, but got nil error", badURL)
		}
	}

	// Verify directPrintClient uses Transport timeouts rather than a short whole-request Client.Timeout
	tr, ok := directPrintClient.Transport.(*http.Transport)
	if !ok || tr == nil {
		t.Fatalf("Expected directPrintClient.Transport to be *http.Transport")
	}
	if tr.ResponseHeaderTimeout < 120*time.Second {
		t.Errorf("Expected ResponseHeaderTimeout >= 120s, got %v", tr.ResponseHeaderTimeout)
	}
	if directPrintClient.Timeout != 0 {
		t.Errorf("Expected directPrintClient.Timeout == 0 (relying on Transport timeouts + ctx for huge uploads), got %v", directPrintClient.Timeout)
	}

	// Verify valid HTTP URL with trailing slash delivers cleanly
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := SendDirectPrint(ctx, srv.URL+"/", "tok", pdfPath); err != nil {
		t.Fatalf("Expected valid peerURL to succeed, got: %v", err)
	}
	if gotPath != "/api/print" {
		t.Errorf("Expected request path /api/print, got %q", gotPath)
	}
}

func TestPrintService_OnJobFailedAllPaths(t *testing.T) {
	tempDir := t.TempDir()

	// Build single controllable mock binary that simulates both exit-code failure and timeout
	mockSrc := filepath.Join(tempDir, "mock_sumatra_fail.go")
	mockExe := filepath.Join(tempDir, "mock_sumatra_fail.exe")
	mockCode := `package main
import (
	"fmt"
	"os"
	"time"
)
func main() {
	var printerName string
	for i := 0; i < len(os.Args)-1; i++ {
		if os.Args[i] == "-print-to" {
			printerName = os.Args[i+1]
		}
	}
	switch printerName {
	case "Printer_Fail_Exit1":
		fmt.Fprintln(os.Stderr, "simulated printer driver error")
		os.Exit(1)
	case "Printer_Timeout":
		time.Sleep(3 * time.Second)
	}
}
`
	if err := os.WriteFile(mockSrc, []byte(mockCode), 0644); err != nil {
		t.Fatalf("WriteFile mockSrc failed: %v", err)
	}
	if out, err := exec.Command("go", "build", "-o", mockExe, mockSrc).CombinedOutput(); err != nil {
		t.Fatalf("Failed to compile mock binary: %v\n%s", err, string(out))
	}

	samplePDF := []byte("%PDF-1.4\n1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n3 0 obj\n<< /Type /Page /Parent 2 0 R /MediaBox [0 0 595.28 841.89] >>\nendobj\ntrailer\n<< /Root 1 0 R >>\n%%EOF")

	type failedResult struct {
		job    PrintJob
		reason string
	}

	t.Run("MissingPhysicalPrinter", func(t *testing.T) {
		qDir := filepath.Join(tempDir, "q_noprinter")
		fDir := filepath.Join(tempDir, "f_noprinter")
		ps := NewPrintService(mockExe, 5, qDir, fDir)
		ps.resolveTargetPrinter = func() (string, error) {
			return "", errors.New("no physical printer connected on this computer")
		}
		if err := ps.Start(); err != nil {
			t.Fatalf("Start failed: %v", err)
		}
		defer ps.Stop()

		failed := make(chan failedResult, 1)
		ps.OnJobFailed = func(j PrintJob, reason string) {
			failed <- failedResult{job: j, reason: reason}
		}

		pdfPath := filepath.Join(qDir, "job_noprinter.pdf")
		_ = os.WriteFile(pdfPath, samplePDF, 0644)
		job := PrintJob{ID: "job_noprinter", FileName: "job_noprinter.pdf", FilePath: pdfPath, EnqueuedAt: time.Now()}
		if err := ps.Enqueue(job); err != nil {
			t.Fatalf("Enqueue failed: %v", err)
		}

		select {
		case res := <-failed:
			if !strings.Contains(strings.ToLower(res.reason), "physical printer") {
				t.Errorf("Expected physical printer failure reason, got %q", res.reason)
			}
			if _, err := os.Stat(filepath.Join(fDir, job.ID)); err != nil {
				t.Errorf("Expected failed job moved to %s: %v", fDir, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("Timed out waiting for OnJobFailed on missing physical printer")
		}
	})

	t.Run("MissingSumatraPDF", func(t *testing.T) {
		qDir := filepath.Join(tempDir, "q_nosumatra")
		fDir := filepath.Join(tempDir, "f_nosumatra")
		ps := NewPrintService(filepath.Join(tempDir, "nonexistent", "SumatraPDF.exe"), 5, qDir, fDir)
		ps.SetTargetPrinter("Test_Physical_Printer")
		ps.findSumatra = func() string { return "" }
		ps.ensureSumatra = func() (string, error) { return "", errors.New("sumatra unavailable") }
		if err := ps.Start(); err != nil {
			t.Fatalf("Start failed: %v", err)
		}
		defer ps.Stop()

		failed := make(chan failedResult, 1)
		ps.OnJobFailed = func(j PrintJob, reason string) {
			failed <- failedResult{job: j, reason: reason}
		}

		pdfPath := filepath.Join(qDir, "job_nosumatra.pdf")
		_ = os.WriteFile(pdfPath, samplePDF, 0644)
		job := PrintJob{ID: "job_nosumatra", FileName: "job_nosumatra.pdf", FilePath: pdfPath, EnqueuedAt: time.Now()}
		if err := ps.Enqueue(job); err != nil {
			t.Fatalf("Enqueue failed: %v", err)
		}

		select {
		case res := <-failed:
			if !strings.Contains(strings.ToLower(res.reason), "sumatrapdf") {
				t.Errorf("Expected SumatraPDF missing reason, got %q", res.reason)
			}
			if _, err := os.Stat(filepath.Join(fDir, job.ID)); err != nil {
				t.Errorf("Expected failed job moved to %s: %v", fDir, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("Timed out waiting for OnJobFailed on missing SumatraPDF")
		}
	})

	t.Run("ExecutionError", func(t *testing.T) {
		qDir := filepath.Join(tempDir, "q_execerr")
		fDir := filepath.Join(tempDir, "f_execerr")
		ps := NewPrintService(mockExe, 5, qDir, fDir)
		ps.SetTargetPrinter("Printer_Fail_Exit1")
		if err := ps.Start(); err != nil {
			t.Fatalf("Start failed: %v", err)
		}
		defer ps.Stop()

		failed := make(chan failedResult, 1)
		ps.OnJobFailed = func(j PrintJob, reason string) {
			failed <- failedResult{job: j, reason: reason}
		}

		pdfPath := filepath.Join(qDir, "job_execerr.pdf")
		_ = os.WriteFile(pdfPath, samplePDF, 0644)
		job := PrintJob{ID: "job_execerr", FileName: "job_execerr.pdf", FilePath: pdfPath, EnqueuedAt: time.Now()}
		if err := ps.Enqueue(job); err != nil {
			t.Fatalf("Enqueue failed: %v", err)
		}

		select {
		case res := <-failed:
			if res.reason == "" {
				t.Errorf("Expected non-empty execution error reason")
			}
			if _, err := os.Stat(filepath.Join(fDir, job.ID)); err != nil {
				t.Errorf("Expected failed job moved to %s: %v", fDir, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("Timed out waiting for OnJobFailed on execution error")
		}
	})

	t.Run("Timeout", func(t *testing.T) {
		qDir := filepath.Join(tempDir, "q_timeout")
		fDir := filepath.Join(tempDir, "f_timeout")
		ps := NewPrintService(mockExe, 1, qDir, fDir)
		ps.SetTargetPrinter("Printer_Timeout")
		if err := ps.Start(); err != nil {
			t.Fatalf("Start failed: %v", err)
		}
		defer ps.Stop()

		failed := make(chan failedResult, 1)
		ps.OnJobFailed = func(j PrintJob, reason string) {
			failed <- failedResult{job: j, reason: reason}
		}

		pdfPath := filepath.Join(qDir, "job_timeout.pdf")
		_ = os.WriteFile(pdfPath, samplePDF, 0644)
		job := PrintJob{ID: "job_timeout", FileName: "job_timeout.pdf", FilePath: pdfPath, EnqueuedAt: time.Now()}
		if err := ps.Enqueue(job); err != nil {
			t.Fatalf("Enqueue failed: %v", err)
		}

		select {
		case res := <-failed:
			if !strings.Contains(strings.ToLower(res.reason), "timeout") {
				t.Errorf("Expected timeout failure reason, got %q", res.reason)
			}
			if _, err := os.Stat(filepath.Join(fDir, job.ID)); err != nil {
				t.Errorf("Expected failed job moved to %s: %v", fDir, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("Timed out waiting for OnJobFailed on job timeout")
		}
	})
}
