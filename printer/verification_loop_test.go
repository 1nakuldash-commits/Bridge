package printer

import (
	"archive/zip"
	"bytes"
	"context"
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
}
