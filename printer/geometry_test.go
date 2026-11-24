package printer

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestDetectDocumentGeometryPDF_A4(t *testing.T) {
	tempDir := t.TempDir()
	pdfPath := filepath.Join(tempDir, "test_a4.pdf")

	// Standard A4 PDF (595.28 x 841.89 pt)
	pdfContent := `%PDF-1.4
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
0000000000 65535 f 
0000000009 00000 n 
0000000058 00000 n 
0000000115 00000 n 
trailer
<< /Size 4 /Root 1 0 R >>
startxref
200
%%EOF`

	if err := os.WriteFile(pdfPath, []byte(pdfContent), 0644); err != nil {
		t.Fatalf("Failed to create test PDF: %v", err)
	}

	geom := DetectDocumentGeometry(pdfPath)
	if geom.PaperName != "A4" {
		t.Errorf("Expected PaperName A4, got %s", geom.PaperName)
	}
	if geom.Orientation != "portrait" {
		t.Errorf("Expected Orientation portrait, got %s", geom.Orientation)
	}

	settings := BuildPrintSettings(geom)
	if settings != "1-1,paper=A4,portrait,noscale" {
		t.Errorf("Expected '1-1,paper=A4,portrait,noscale', got %s", settings)
	}
}

func TestDetectDocumentGeometryPDF_LandscapeLetter(t *testing.T) {
	tempDir := t.TempDir()
	pdfPath := filepath.Join(tempDir, "test_letter_landscape.pdf")

	// Landscape Letter PDF (792.0 x 612.0 pt)
	pdfContent := `%PDF-1.4
1 0 obj
<< /Type /Catalog /Pages 2 0 R >>
endobj
2 0 obj
<< /Type /Pages /Kids [3 0 R] /Count 1 >>
endobj
3 0 obj
<< /Type /Page /Parent 2 0 R /MediaBox [0 0 792.0 612.0] >>
endobj
xref
0 4
0000000000 65535 f 
trailer
<< /Size 4 /Root 1 0 R >>
%%EOF`

	if err := os.WriteFile(pdfPath, []byte(pdfContent), 0644); err != nil {
		t.Fatalf("Failed to write letter landscape PDF: %v", err)
	}

	geom := DetectDocumentGeometry(pdfPath)
	if geom.PaperName != "Letter" {
		t.Errorf("Expected PaperName Letter, got %s", geom.PaperName)
	}
	if geom.Orientation != "landscape" {
		t.Errorf("Expected Orientation landscape, got %s", geom.Orientation)
	}

	settings := BuildPrintSettings(geom)
	if settings != "1-1,paper=Letter,landscape,noscale" {
		t.Errorf("Expected '1-1,paper=Letter,landscape,noscale', got %s", settings)
	}
}

func TestDetectDocumentGeometryXPS_A4(t *testing.T) {
	tempDir := t.TempDir()
	xpsPath := filepath.Join(tempDir, "test_doc.xps")

	// XPS file containing A4 dimensions at 96 DPI (793.7 x 1122.5 units)
	fpageContent := `<FixedPage xmlns="http://schemas.microsoft.com/xps/2005/06" Width="793.7" Height="1122.5" xml:lang="en-US">
		<Glyphs UnicodeString="Test A4 Spool" />
	</FixedPage>`

	f, err := os.Create(xpsPath)
	if err != nil {
		t.Fatalf("Failed to create xps: %v", err)
	}
	zw := zip.NewWriter(f)
	w, err := zw.Create("Documents/1/Pages/1.fpage")
	if err != nil {
		t.Fatalf("Failed to create fpage in zip: %v", err)
	}
	_, _ = w.Write([]byte(fpageContent))
	_ = zw.Close()
	_ = f.Close()

	geom := DetectDocumentGeometry(xpsPath)
	if geom.PaperName != "A4" {
		t.Errorf("Expected PaperName A4 for XPS, got %s", geom.PaperName)
	}
	if geom.Orientation != "portrait" {
		t.Errorf("Expected Orientation portrait for XPS, got %s", geom.Orientation)
	}

	settings := BuildPrintSettings(geom)
	if settings != "1-1,paper=A4,portrait,noscale" {
		t.Errorf("Expected '1-1,paper=A4,portrait,noscale', got %s", settings)
	}
}

func TestMatchStandardPaperAllSizes(t *testing.T) {
	tests := []struct {
		short float64
		long  float64
		want  string
	}{
		{595.28, 841.89, "A4"},
		{612.0, 792.0, "Letter"},
		{612.0, 1008.0, "Legal"},
		{841.89, 1190.55, "A3"},
		{419.53, 595.28, "A5"},
		{792.0, 1224.0, "Tabloid"},
	}

	for _, tt := range tests {
		got := matchStandardPaper(tt.short, tt.long)
		if got != tt.want {
			t.Errorf("matchStandardPaper(%v, %v) = %s; want %s", tt.short, tt.long, got, tt.want)
		}
	}
}

func TestDetectDocumentGeometryLargePDF(t *testing.T) {
	tempDir := t.TempDir()
	pdfPath := filepath.Join(tempDir, "large_document.pdf")

	// Create a PDF larger than 200 KB with /Count 5 located beyond 150 KB
	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n")
	buf.WriteString("3 0 obj\n<< /Type /Page /MediaBox [0 0 595.28 841.89] >>\nendobj\n")

	// Padding to push dictionary past 150 KB
	padding := make([]byte, 160*1024)
	for i := range padding {
		padding[i] = ' '
	}
	buf.Write(padding)

	buf.WriteString("2 0 obj\n<< /Type /Pages /Kids [3 0 R 4 0 R 5 0 R 6 0 R 7 0 R] /Count 5 >>\nendobj\n")
	buf.WriteString("xref\n0 8\ntrailer\n<< /Size 8 /Root 1 0 R >>\nstartxref\n180000\n%%EOF\n")

	if err := os.WriteFile(pdfPath, buf.Bytes(), 0644); err != nil {
		t.Fatalf("Failed to write large test PDF: %v", err)
	}

	geom := DetectDocumentGeometry(pdfPath)
	if geom.PageCount != 5 {
		t.Errorf("Expected PageCount 5 in large PDF, got %d", geom.PageCount)
	}
	if geom.PaperName != "A4" {
		t.Errorf("Expected PaperName A4, got %s", geom.PaperName)
	}

	settings := BuildPrintSettings(geom)
	expectedSettings := "1-5,paper=A4,portrait,noscale"
	if settings != expectedSettings {
		t.Errorf("Expected settings %q, got %q", expectedSettings, settings)
	}
}
