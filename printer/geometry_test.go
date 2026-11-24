package printer

import (
	"archive/zip"
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
	if settings != "paper=A4,portrait,shrink" {
		t.Errorf("Expected 'paper=A4,portrait,shrink', got %s", settings)
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
	if settings != "paper=Letter,landscape,shrink" {
		t.Errorf("Expected 'paper=Letter,landscape,shrink', got %s", settings)
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
	if settings != "paper=A4,portrait,shrink" {
		t.Errorf("Expected 'paper=A4,portrait,shrink', got %s", settings)
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
