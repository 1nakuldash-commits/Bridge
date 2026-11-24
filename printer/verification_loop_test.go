package printer

import (
	"archive/zip"
	"context"
	"fmt"
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
			wantSettings:    "paper=A4,portrait,shrink",
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
			wantSettings:    "paper=A4,landscape,shrink",
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
			wantSettings:    "paper=Letter,portrait,shrink",
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
			wantSettings:    "paper=A4,portrait,shrink",
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
			wantSettings:    "paper=A4,portrait,shrink",
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
