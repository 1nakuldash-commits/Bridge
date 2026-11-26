package printer

import (
	"archive/zip"
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"strings"
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

func TestDetectDocumentGeometryPDF_A2_A3(t *testing.T) {
	tempDir := t.TempDir()

	// Test A3 PDF: 841.89 x 1190.55 pt
	a3Path := filepath.Join(tempDir, "test_a3.pdf")
	a3Content := `%PDF-1.4
1 0 obj
<< /Type /Catalog /Pages 2 0 R >>
endobj
2 0 obj
<< /Type /Pages /Kids [3 0 R] /Count 1 >>
endobj
3 0 obj
<< /Type /Page /Parent 2 0 R /MediaBox [0 0 841.89 1190.55] >>
endobj
xref
trailer
<< /Size 4 /Root 1 0 R >>
%%EOF`
	if err := os.WriteFile(a3Path, []byte(a3Content), 0644); err != nil {
		t.Fatalf("Failed to create A3 PDF: %v", err)
	}
	geomA3 := DetectDocumentGeometry(a3Path)
	if geomA3.PaperName != "A3" {
		t.Errorf("Expected PaperName A3, got %s", geomA3.PaperName)
	}
	if s := BuildPrintSettings(geomA3); s != "1-1,paper=A3,portrait,noscale" {
		t.Errorf("Expected '1-1,paper=A3,portrait,noscale', got %s", s)
	}

	// Test A2 PDF: 1190.55 x 1683.78 pt
	a2Path := filepath.Join(tempDir, "test_a2.pdf")
	a2Content := `%PDF-1.4
1 0 obj
<< /Type /Catalog /Pages 2 0 R >>
endobj
2 0 obj
<< /Type /Pages /Kids [3 0 R] /Count 1 >>
endobj
3 0 obj
<< /Type /Page /Parent 2 0 R /MediaBox [0 0 1190.55 1683.78] >>
endobj
xref
trailer
<< /Size 4 /Root 1 0 R >>
%%EOF`
	if err := os.WriteFile(a2Path, []byte(a2Content), 0644); err != nil {
		t.Fatalf("Failed to create A2 PDF: %v", err)
	}
	geomA2 := DetectDocumentGeometry(a2Path)
	if geomA2.PaperName != "A2" {
		t.Errorf("Expected PaperName A2, got %s", geomA2.PaperName)
	}
	if s := BuildPrintSettings(geomA2); s != "1-1,paper=A2,portrait,noscale" {
		t.Errorf("Expected '1-1,paper=A2,portrait,noscale', got %s", s)
	}
}

func TestDetectDocumentGeometryPDF_ObjStmCompressed(t *testing.T) {
	tempDir := t.TempDir()
	pdfPath := filepath.Join(tempDir, "compressed_objstm.pdf")

	// Build compressed /ObjStm holding Catalog (1), Pages (2), and Page (3)
	objs := []struct {
		id   int
		body string
	}{
		{1, "<< /Type /Catalog /Pages 2 0 R >>"},
		{2, "<< /Type /Pages /Kids [3 0 R 4 0 R 5 0 R] /Count 3 >>"},
		{3, "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612.0 792.0] >>"},
	}
	var headerBuf, bodyBuf bytes.Buffer
	for _, o := range objs {
		fmt.Fprintf(&headerBuf, "%d %d ", o.id, bodyBuf.Len())
		bodyBuf.WriteString(o.body)
		bodyBuf.WriteByte('\n')
	}
	firstOffset := headerBuf.Len()
	var plainStm bytes.Buffer
	plainStm.Write(headerBuf.Bytes())
	plainStm.Write(bodyBuf.Bytes())

	var compressed bytes.Buffer
	zw := zlib.NewWriter(&compressed)
	_, _ = zw.Write(plainStm.Bytes())
	_ = zw.Close()

	var pdf bytes.Buffer
	pdf.WriteString("%PDF-1.5\n")
	fmt.Fprintf(&pdf, "10 0 obj\n<< /Type /ObjStm /N %d /First %d /Filter /FlateDecode /Length %d >>\nstream\n",
		len(objs), firstOffset, compressed.Len())
	pdf.Write(compressed.Bytes())
	pdf.WriteString("\nendstream\nendobj\n")
	pdf.WriteString("11 0 obj\n<< /Type /XRef /Size 12 /Root 1 0 R /Length 0 >>\nstream\nendstream\nendobj\n%%EOF\n")

	if err := os.WriteFile(pdfPath, pdf.Bytes(), 0644); err != nil {
		t.Fatalf("Failed to write compressed PDF: %v", err)
	}

	geom := DetectDocumentGeometry(pdfPath)
	if geom.PaperName != "Letter" {
		t.Errorf("Expected PaperName Letter, got %q", geom.PaperName)
	}
	if geom.Orientation != "portrait" {
		t.Errorf("Expected Orientation portrait, got %q", geom.Orientation)
	}
	if geom.PageCount != 3 {
		t.Errorf("Expected PageCount 3, got %d", geom.PageCount)
	}
	if s := BuildPrintSettings(geom); s != "1-3,paper=Letter,portrait,noscale" {
		t.Errorf("Expected '1-3,paper=Letter,portrait,noscale', got %q", s)
	}
}

func TestDetectDocumentGeometryPDF_NestedPages(t *testing.T) {
	tempDir := t.TempDir()
	pdfPath := filepath.Join(tempDir, "nested_pages.pdf")

	// Child /Pages node (10 0 obj, /Count 1) appears BEFORE root /Pages node (2 0 obj, /Count 4)
	pdfContent := `%PDF-1.4
10 0 obj
<< /Type /Pages /Parent 2 0 R /Kids [11 0 R] /Count 1 >>
endobj
11 0 obj
<< /Type /Page /Parent 10 0 R >>
endobj
20 0 obj
<< /Type /Pages /Parent 2 0 R /Kids [21 0 R 22 0 R 23 0 R] /Count 3 >>
endobj
21 0 obj
<< /Type /Page /Parent 20 0 R >>
endobj
22 0 obj
<< /Type /Page /Parent 20 0 R >>
endobj
23 0 obj
<< /Type /Page /Parent 20 0 R >>
endobj
1 0 obj
<< /Type /Catalog /Pages 2 0 R >>
endobj
2 0 obj
<< /Type /Pages /Kids [10 0 R 20 0 R] /Count 4 /MediaBox [0 0 595.28 841.89] >>
endobj
trailer
<< /Size 24 /Root 1 0 R >>
%%EOF`
	if err := os.WriteFile(pdfPath, []byte(pdfContent), 0644); err != nil {
		t.Fatalf("Failed to write nested pages PDF: %v", err)
	}

	geom := DetectDocumentGeometry(pdfPath)
	if geom.PageCount != 4 {
		t.Errorf("Expected root PageCount 4, got %d", geom.PageCount)
	}
	if geom.PaperName != "A4" {
		t.Errorf("Expected inherited PaperName A4, got %q", geom.PaperName)
	}
	if s := BuildPrintSettings(geom); s != "1-4,paper=A4,portrait,noscale" {
		t.Errorf("Expected '1-4,paper=A4,portrait,noscale', got %q", s)
	}
}

func TestDetectDocumentGeometryPDF_UserUnitAndOversized(t *testing.T) {
	tempDir := t.TempDir()

	// 1. Half-size MediaBox with /UserUnit 2 => A4 portrait at 1:1
	uuA4Path := filepath.Join(tempDir, "userunit_a4.pdf")
	uuA4 := `%PDF-1.6
1 0 obj
<< /Type /Catalog /Pages 2 0 R >>
endobj
2 0 obj
<< /Type /Pages /Kids [3 0 R] /Count 1 >>
endobj
3 0 obj
<< /Type /Page /Parent 2 0 R /MediaBox [0 0 297.64 420.945] /UserUnit 2 >>
endobj
trailer
<< /Size 4 /Root 1 0 R >>
%%EOF`
	_ = os.WriteFile(uuA4Path, []byte(uuA4), 0644)
	geom := DetectDocumentGeometry(uuA4Path)
	if geom.PaperName != "A4" || geom.ScaleMode != "noscale" {
		t.Errorf("Expected A4/noscale for UserUnit=2 half-A4, got paper=%q scale=%q", geom.PaperName, geom.ScaleMode)
	}

	// 2. Oversized A0 PDF (2383.94 x 3370.39 pt) => no invalid paper=A0, shrink fallback
	a0Path := filepath.Join(tempDir, "oversized_a0.pdf")
	a0PDF := `%PDF-1.4
1 0 obj
<< /Type /Catalog /Pages 2 0 R >>
endobj
2 0 obj
<< /Type /Pages /Kids [3 0 R] /Count 1 >>
endobj
3 0 obj
<< /Type /Page /Parent 2 0 R /MediaBox [0 0 2383.94 3370.39] >>
endobj
trailer
<< /Size 4 /Root 1 0 R >>
%%EOF`
	_ = os.WriteFile(a0Path, []byte(a0PDF), 0644)
	geomA0 := DetectDocumentGeometry(a0Path)
	if geomA0.PaperName != "" {
		t.Errorf("Expected empty PaperName for oversized A0, got %q", geomA0.PaperName)
	}
	if geomA0.ScaleMode != "shrink" {
		t.Errorf("Expected ScaleMode 'shrink' for oversized A0, got %q", geomA0.ScaleMode)
	}
	if s := BuildPrintSettings(geomA0); s != "1-1,portrait,shrink" {
		t.Errorf("Expected '1-1,portrait,shrink', got %q", s)
	}
}

func TestDetectDocumentGeometryPDF_MixedOrientationAndCropBox(t *testing.T) {
	tempDir := t.TempDir()
	pdfPath := filepath.Join(tempDir, "mixed_orient.pdf")

	// Page 1: A4 portrait (MediaBox)
	// Page 2: A4 rotated 90 degrees => landscape (CropBox fallback)
	pdfContent := `%PDF-1.4
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
<< /Type /Page /Parent 2 0 R /CropBox [0 0 595.28 841.89] /Rotate 90 >>
endobj
trailer
<< /Size 5 /Root 1 0 R >>
%%EOF`
	_ = os.WriteFile(pdfPath, []byte(pdfContent), 0644)

	geom := DetectDocumentGeometry(pdfPath)
	if !geom.MixedOrientations {
		t.Errorf("Expected MixedOrientations=true")
	}
	if geom.MixedSizes {
		t.Errorf("Expected MixedSizes=false (both pages are A4)")
	}
	if geom.Orientation != "" {
		t.Errorf("Expected empty Orientation when mixed, got %q", geom.Orientation)
	}
	if s := BuildPrintSettings(geom); s != "1-2,paper=A4,noscale" {
		t.Errorf("Expected '1-2,paper=A4,noscale', got %q", s)
	}
}

func TestDetectDocumentGeometryXPS_FPageOrdering(t *testing.T) {
	tempDir := t.TempDir()
	xpsPath := filepath.Join(tempDir, "ordered_pages.xps")

	f, err := os.Create(xpsPath)
	if err != nil {
		t.Fatalf("Failed to create xps: %v", err)
	}
	zw := zip.NewWriter(f)

	// Intentionally write 10.fpage and 2.fpage before 1.fpage in ZIP directory order.
	// 1.fpage is Letter portrait; 2.fpage & 10.fpage are also Letter portrait.
	pages := []struct {
		name string
		w, h string
	}{
		{"Documents/1/Pages/10.fpage", "816", "1056"},
		{"Documents/1/Pages/2.fpage", "816", "1056"},
		{"Documents/1/Pages/1.fpage", "793.7", "1122.5"}, // Page 1 is A4 portrait
	}
	for _, p := range pages {
		w, _ := zw.Create(p.name)
		_, _ = fmt.Fprintf(w, `<FixedPage xmlns="http://schemas.microsoft.com/xps/2005/06" Width="%s" Height="%s" />`, p.w, p.h)
	}
	_ = zw.Close()
	_ = f.Close()

	geom := DetectDocumentGeometry(xpsPath)
	if geom.PageCount != 3 {
		t.Errorf("Expected PageCount 3, got %d", geom.PageCount)
	}
	// Page 1 must be 1.fpage (A4: ~595.28 x 841.88 pt), not 10.fpage (Letter: 612 x 792 pt)
	if geom.WidthPt < 590 || geom.WidthPt > 600 {
		t.Errorf("Expected WidthPt from 1.fpage (~595.3), got %.2f", geom.WidthPt)
	}
	if !geom.MixedSizes {
		t.Errorf("Expected MixedSizes=true across A4 page 1 and Letter pages 2,10")
	}
	if s := BuildPrintSettings(geom); s != "1-3,portrait,noscale" {
		t.Errorf("Expected '1-3,portrait,noscale', got %q", s)
	}
}

func TestDetectDocumentGeometryImage_ScalingMode(t *testing.T) {
	tempDir := t.TempDir()
	imgPath := filepath.Join(tempDir, "photo_landscape.png")

	img := image.NewRGBA(image.Rect(0, 0, 200, 100))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	f, err := os.Create(imgPath)
	if err != nil {
		t.Fatalf("Failed to create png: %v", err)
	}
	if err := png.Encode(f, img); err != nil {
		_ = f.Close()
		t.Fatalf("Failed to encode png: %v", err)
	}
	_ = f.Close()

	geom := DetectDocumentGeometry(imgPath)
	if geom.Format != ".png" {
		t.Errorf("Expected Format .png, got %q", geom.Format)
	}
	if geom.ScaleMode != "fit" {
		t.Errorf("Expected ScaleMode 'fit' for image, got %q", geom.ScaleMode)
	}
	if geom.Orientation != "landscape" {
		t.Errorf("Expected Orientation 'landscape' for 200x100 PNG, got %q", geom.Orientation)
	}
	if geom.PageCount != 1 {
		t.Errorf("Expected PageCount 1, got %d", geom.PageCount)
	}
	s := BuildPrintSettings(geom)
	if strings.Contains(s, "noscale") || !strings.Contains(s, "fit") {
		t.Errorf("Expected image print settings to use 'fit' and not 'noscale', got %q", s)
	}
	if geom.WidthPt <= 0 || geom.HeightPt <= 0 || geom.WidthPt <= geom.HeightPt {
		t.Errorf("Expected positive landscape WidthPt > HeightPt for 200x100 PNG, got %.2fx%.2f", geom.WidthPt, geom.HeightPt)
	}
}

func TestDetectDocumentGeometryPDF_PrefersCropBoxOverMediaBox(t *testing.T) {
	tempDir := t.TempDir()
	pdfPath := filepath.Join(tempDir, "crop_over_media.pdf")

	// MediaBox is Letter (612x792), CropBox is A5 (419.53x595.28)
	pdfContent := `%PDF-1.4
1 0 obj
<< /Type /Catalog /Pages 2 0 R >>
endobj
2 0 obj
<< /Type /Pages /Kids [3 0 R] /Count 1 >>
endobj
3 0 obj
<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612.0 792.0] /CropBox [0 0 419.53 595.28] >>
endobj
trailer
<< /Size 4 /Root 1 0 R >>
%%EOF`
	if err := os.WriteFile(pdfPath, []byte(pdfContent), 0644); err != nil {
		t.Fatalf("write pdf: %v", err)
	}

	geom := DetectDocumentGeometry(pdfPath)
	if geom.PaperName != "A5" {
		t.Errorf("Expected /CropBox to take precedence and detect A5, got %q (%.1fx%.1f)", geom.PaperName, geom.WidthPt, geom.HeightPt)
	}
}

func TestDetectDocumentGeometryXPS_PrintTicketLandscape(t *testing.T) {
	tempDir := t.TempDir()
	xpsPath := filepath.Join(tempDir, "pt_landscape.xps")

	f, err := os.Create(xpsPath)
	if err != nil {
		t.Fatalf("create xps: %v", err)
	}
	zw := zip.NewWriter(f)

	w1, _ := zw.Create("Documents/1/Pages/1.fpage")
	_, _ = w1.Write([]byte(`<FixedPage xmlns="http://schemas.microsoft.com/xps/2005/06" Width="793.7" Height="1122.5"/>`))

	wPT, _ := zw.Create("Documents/1/Metadata/Page1_PT.xml")
	_, _ = wPT.Write([]byte(`<?xml version="1.0" encoding="utf-8"?>
<psf:PrintTicket xmlns:psf="http://schemas.microsoft.com/windows/2003/08/printing/printschemaframework" xmlns:psk="http://schemas.microsoft.com/windows/2003/08/printing/printschemakeywords">
  <psf:Feature name="psk:PageOrientation">
    <psf:Option name="psk:Landscape" />
  </psf:Feature>
</psf:PrintTicket>`))
	_ = zw.Close()
	_ = f.Close()

	geom := DetectDocumentGeometry(xpsPath)
	if geom.Orientation != "landscape" {
		t.Errorf("Expected PrintTicket Landscape to yield landscape orientation, got %q", geom.Orientation)
	}
	if geom.WidthPt <= geom.HeightPt {
		t.Errorf("Expected WidthPt > HeightPt for landscape XPS, got %.2fx%.2f", geom.WidthPt, geom.HeightPt)
	}
}

func buildTestTIFF(width, height uint32, orient uint16, littleEndian bool) []byte {
	var order binary.ByteOrder = binary.BigEndian
	magic := []byte("MM\x00*")
	if littleEndian {
		order = binary.LittleEndian
		magic = []byte("II*\x00")
	}

	var buf bytes.Buffer
	buf.Write(magic)
	_ = binary.Write(&buf, order, uint32(8)) // IFD0 at offset 8

	_ = binary.Write(&buf, order, uint16(3)) // 3 entries

	writeEntry := func(tag, ftype uint16, val uint32) {
		_ = binary.Write(&buf, order, tag)
		_ = binary.Write(&buf, order, ftype)
		_ = binary.Write(&buf, order, uint32(1))
		if ftype == 3 {
			_ = binary.Write(&buf, order, uint16(val))
			_ = binary.Write(&buf, order, uint16(0))
		} else {
			_ = binary.Write(&buf, order, val)
		}
	}
	writeEntry(0x0100, 4, width)
	writeEntry(0x0101, 4, height)
	writeEntry(0x0112, 3, uint32(orient))
	_ = binary.Write(&buf, order, uint32(0)) // next IFD
	return buf.Bytes()
}

func injectJPEGExifOrientation(t *testing.T, rawJPEG []byte, orient uint16) []byte {
	t.Helper()
	if len(rawJPEG) < 2 || rawJPEG[0] != 0xFF || rawJPEG[1] != 0xD8 {
		t.Fatalf("invalid JPEG header")
	}
	// Build minimal TIFF with 1 IFD0 entry (Orientation 0x0112)
	var tiff bytes.Buffer
	tiff.WriteString("II*\x00")
	_ = binary.Write(&tiff, binary.LittleEndian, uint32(8))
	_ = binary.Write(&tiff, binary.LittleEndian, uint16(1))
	_ = binary.Write(&tiff, binary.LittleEndian, uint16(0x0112))
	_ = binary.Write(&tiff, binary.LittleEndian, uint16(3))
	_ = binary.Write(&tiff, binary.LittleEndian, uint32(1))
	_ = binary.Write(&tiff, binary.LittleEndian, orient)
	_ = binary.Write(&tiff, binary.LittleEndian, uint16(0))
	_ = binary.Write(&tiff, binary.LittleEndian, uint32(0))

	app1Payload := append([]byte("Exif\x00\x00"), tiff.Bytes()...)
	segLen := uint16(len(app1Payload) + 2)

	var out bytes.Buffer
	out.Write(rawJPEG[:2])
	out.Write([]byte{0xFF, 0xE1})
	_ = binary.Write(&out, binary.BigEndian, segLen)
	out.Write(app1Payload)
	out.Write(rawJPEG[2:])
	return out.Bytes()
}

func TestDetectDocumentGeometryImage_EXIFOrientationAndTIFF(t *testing.T) {
	tempDir := t.TempDir()

	// 1. Landscape pixel JPEG (160x80) with EXIF Orientation=6 (rotated 90 deg CW -> portrait)
	img := image.NewRGBA(image.Rect(0, 0, 160, 80))
	var jpgBuf bytes.Buffer
	if err := jpeg.Encode(&jpgBuf, img, nil); err != nil {
		t.Fatalf("jpeg encode: %v", err)
	}
	exifJPG := injectJPEGExifOrientation(t, jpgBuf.Bytes(), 6)
	jpgPath := filepath.Join(tempDir, "phone_portrait.jpg")
	if err := os.WriteFile(jpgPath, exifJPG, 0644); err != nil {
		t.Fatalf("write jpg: %v", err)
	}

	geomJPG := DetectDocumentGeometry(jpgPath)
	if geomJPG.Orientation != "portrait" {
		t.Errorf("Expected portrait for 160x80 JPEG with EXIF orient=6, got %q", geomJPG.Orientation)
	}
	if math.Abs(geomJPG.WidthPt-60.0) > 0.1 || math.Abs(geomJPG.HeightPt-120.0) > 0.1 {
		t.Errorf("Expected swapped 60x120 pt for EXIF orient=6, got %.2fx%.2f", geomJPG.WidthPt, geomJPG.HeightPt)
	}

	// 2. TIFF little-endian 400x200 (landscape) & big-endian 400x200 with orient=8 (portrait)
	tifLE := filepath.Join(tempDir, "scan_le.tif")
	_ = os.WriteFile(tifLE, buildTestTIFF(400, 200, 1, true), 0644)
	geomTifLE := DetectDocumentGeometry(tifLE)
	if geomTifLE.Format != ".tif" || geomTifLE.Orientation != "landscape" {
		t.Errorf("Expected .tif landscape, got format=%q orient=%q", geomTifLE.Format, geomTifLE.Orientation)
	}
	if math.Abs(geomTifLE.WidthPt-300.0) > 0.1 || math.Abs(geomTifLE.HeightPt-150.0) > 0.1 {
		t.Errorf("Expected 300x150 pt for 400x200 TIFF, got %.2fx%.2f", geomTifLE.WidthPt, geomTifLE.HeightPt)
	}

	tifBE := filepath.Join(tempDir, "scan_be.tiff")
	_ = os.WriteFile(tifBE, buildTestTIFF(400, 200, 8, false), 0644)
	geomTifBE := DetectDocumentGeometry(tifBE)
	if geomTifBE.Orientation != "portrait" || geomTifBE.WidthPt >= geomTifBE.HeightPt {
		t.Errorf("Expected portrait 150x300 pt for big-endian TIFF orient=8, got %q %.2fx%.2f",
			geomTifBE.Orientation, geomTifBE.WidthPt, geomTifBE.HeightPt)
	}
}

func TestMatchStandardPaper_A6_Statement_AndCustom(t *testing.T) {
	tests := []struct {
		short float64
		long  float64
		want  string
	}{
		{297.64, 419.53, "A6"},
		{396.0, 612.0, "Statement"},
		{500.0, 707.0, ""}, // Custom ISO-ratio size must NOT coerce to A5 (419x595)
		{300.0, 300.0, ""}, // Square custom size must NOT coerce to A4
		{612.0, 936.0, ""}, // Foolscap/Folio must NOT coerce to A4
	}
	for _, tc := range tests {
		got := matchStandardPaper(tc.short, tc.long)
		if got != tc.want {
			t.Errorf("matchStandardPaper(%.2f, %.2f) = %q; want %q", tc.short, tc.long, got, tc.want)
		}
	}
}
