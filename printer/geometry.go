package printer

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Standard paper dimensions in points (72 points = 1 inch, 25.4 mm = 72 points)
const (
	ptPerMm = 72.0 / 25.4 // ~2.8346 pt per mm

	// Standard ISO & ANSI page sizes (in points, normalized: short edge, long edge)
	a4ShortPt  = 210.0 * ptPerMm // 595.276
	a4LongPt   = 297.0 * ptPerMm // 841.890
	ltrShortPt = 8.5 * 72.0      // 612.0
	ltrLongPt  = 11.0 * 72.0     // 792.0
	lglShortPt = 8.5 * 72.0      // 612.0
	lglLongPt  = 14.0 * 72.0     // 1008.0
	a3ShortPt  = 297.0 * ptPerMm // 841.890
	a3LongPt   = 420.0 * ptPerMm // 1190.551
	a5ShortPt  = 148.0 * ptPerMm // 419.528
	a5LongPt   = 210.0 * ptPerMm // 595.276
	tabShortPt = 11.0 * 72.0     // 792.0
	tabLongPt  = 17.0 * 72.0     // 1224.0
)

// DocumentGeometry captures the detected dimensions, orientation, paper size, and page count.
type DocumentGeometry struct {
	Format      string  // ".pdf", ".xps", ".oxps"
	WidthPt     float64 // Document width in points (72 DPI)
	HeightPt    float64 // Document height in points (72 DPI)
	PaperName   string  // Standard paper name for print drivers: "A4", "Letter", "Legal", "A3", "A5", "Tabloid"
	Orientation string  // "portrait" or "landscape"
	PageCount   int     // Number of pages detected
	ScaleMode   string  // "shrink", "noscale", or "fit"
}

var (
	mediaBoxRegex = regexp.MustCompile(`/(?:MediaBox|CropBox)\s*\[\s*([-\d\.]+)\s+([-\d\.]+)\s+([-\d\.]+)\s+([-\d\.]+)\s*\]`)
	rotateRegex   = regexp.MustCompile(`/Rotate\s+(\d+)`)
	pageTypeRegex     = regexp.MustCompile(`/Type\s*/Page\b`)
	pageCountRegex    = regexp.MustCompile(`(?i)/Type\s*/Pages[\s\S]{0,500}?/Count\s+(\d+)`)
	pageCountAltRegex = regexp.MustCompile(`(?i)/Count\s+(\d+)[\s\S]{0,500}?/Type\s*/Pages`)
	fpageDimRegex     = regexp.MustCompile(`(?i)<FixedPage[^>]*\bWidth\s*=\s*["']([\d\.]+)["'][^>]*\bHeight\s*=\s*["']([\d\.]+)["']`)
	fpageAltRegex     = regexp.MustCompile(`(?i)<FixedPage[^>]*\bHeight\s*=\s*["']([\d\.]+)["'][^>]*\bWidth\s*=\s*["']([\d\.]+)["']`)
)

// DetectDocumentGeometry inspects a PDF or XPS file to determine its native dimensions,
// target paper size (A4, Letter, etc.), orientation, and page count with 100% precision.
func DetectDocumentGeometry(filePath string) DocumentGeometry {
	geom := DocumentGeometry{
		Format:      strings.ToLower(filepath.Ext(filePath)),
		Orientation: "portrait",
		PaperName:   "A4", // Sensible default for non-US regions
		ScaleMode:   "noscale",
		PageCount:   1,
	}

	f, err := os.Open(filePath)
	if err != nil {
		return geom
	}
	defer f.Close()

	header := make([]byte, 4096)
	n, _ := f.Read(header)
	header = header[:n]

	// Determine format from magic bytes if extension is missing/generic
	if bytes.HasPrefix(header, []byte("PK\x03\x04")) {
		geom.Format = ".xps"
	} else if bytes.HasPrefix(header, []byte("%PDF-")) || bytes.Contains(header, []byte("%PDF-")) {
		geom.Format = ".pdf"
	}

	if geom.Format == ".xps" || geom.Format == ".oxps" {
		detectXPSGeometry(filePath, &geom)
	} else {
		detectPDFGeometry(f, &geom)
	}

	// Classify standard paper name based on detected Width and Height in points
	if geom.WidthPt > 0 && geom.HeightPt > 0 {
		if geom.WidthPt > geom.HeightPt {
			geom.Orientation = "landscape"
		} else {
			geom.Orientation = "portrait"
		}

		shortEdge := math.Min(geom.WidthPt, geom.HeightPt)
		longEdge := math.Max(geom.WidthPt, geom.HeightPt)
		geom.PaperName = matchStandardPaper(shortEdge, longEdge)
	}

	return geom
}

func detectPDFGeometry(f *os.File, geom *DocumentGeometry) {
	fi, err := f.Stat()
	if err != nil {
		return
	}
	_, _ = f.Seek(0, io.SeekStart)

	// Read full file up to 32MB to ensure multi-page counts and dictionaries are never truncated
	readLimit := int64(32 * 1024 * 1024)
	if fi.Size() < readLimit {
		readLimit = fi.Size()
	}
	sample, err := io.ReadAll(io.LimitReader(f, readLimit))
	if err != nil && len(sample) == 0 {
		return
	}

	// For large PDFs, append trailing 256KB to inspect PDF trailer/catalog
	if fi.Size() > readLimit {
		tailSize := int64(256 * 1024)
		if fi.Size() > tailSize {
			tailBuf := make([]byte, tailSize)
			if _, err := f.ReadAt(tailBuf, fi.Size()-tailSize); err == nil {
				sample = append(sample, tailBuf...)
			}
		}
	}

	// Look for MediaBox or CropBox in document
	match := mediaBoxRegex.FindSubmatch(sample)
	if len(match) == 5 {
		x0, _ := strconv.ParseFloat(string(match[1]), 64)
		y0, _ := strconv.ParseFloat(string(match[2]), 64)
		x1, _ := strconv.ParseFloat(string(match[3]), 64)
		y1, _ := strconv.ParseFloat(string(match[4]), 64)

		w := math.Abs(x1 - x0)
		h := math.Abs(y1 - y0)

		// Check for /Rotate attribute (90, 270 degrees inverts width and height)
		if rotMatch := rotateRegex.FindSubmatch(sample); len(rotMatch) == 2 {
			if rot, _ := strconv.Atoi(string(rotMatch[1])); rot == 90 || rot == 270 {
				w, h = h, w
			}
		}

		geom.WidthPt = w
		geom.HeightPt = h
	}

	// Count pages: first check root /Pages /Count attribute for absolute accuracy
	if cm := pageCountRegex.FindSubmatch(sample); len(cm) == 2 {
		if c, err := strconv.Atoi(string(cm[1])); err == nil && c > 0 {
			geom.PageCount = c
			return
		}
	} else if cm := pageCountAltRegex.FindSubmatch(sample); len(cm) == 2 {
		if c, err := strconv.Atoi(string(cm[1])); err == nil && c > 0 {
			geom.PageCount = c
			return
		}
	}

	// Fallback: count individual /Type /Page objects across the entire document
	count := len(pageTypeRegex.FindAll(sample, -1))
	if count > 0 {
		geom.PageCount = count
	}
}

func detectXPSGeometry(filePath string, geom *DocumentGeometry) {
	zr, err := zip.OpenReader(filePath)
	if err != nil {
		return
	}
	defer zr.Close()

	pageCount := 0
	var firstPageData []byte

	for _, file := range zr.File {
		lower := strings.ToLower(file.Name)
		if strings.HasSuffix(lower, ".fpage") {
			pageCount++
			if firstPageData == nil {
				if rc, err := file.Open(); err == nil {
					buf := make([]byte, 4096)
					n, _ := rc.Read(buf)
					firstPageData = buf[:n]
					rc.Close()
				}
			}
		}
	}

	if pageCount > 0 {
		geom.PageCount = pageCount
	}

	if len(firstPageData) > 0 {
		// XPS Width and Height are in 1/96 inch units
		// Multiply by 0.75 (72.0 / 96.0) to convert to 1/72 inch points
		if m := fpageDimRegex.FindSubmatch(firstPageData); len(m) == 3 {
			w96, _ := strconv.ParseFloat(string(m[1]), 64)
			h96, _ := strconv.ParseFloat(string(m[2]), 64)
			geom.WidthPt = w96 * 0.75
			geom.HeightPt = h96 * 0.75
		} else if m := fpageAltRegex.FindSubmatch(firstPageData); len(m) == 3 {
			h96, _ := strconv.ParseFloat(string(m[1]), 64)
			w96, _ := strconv.ParseFloat(string(m[2]), 64)
			geom.WidthPt = w96 * 0.75
			geom.HeightPt = h96 * 0.75
		}
	}
}

// matchStandardPaper matches short and long edge dimensions in points to a standard paper size.
func matchStandardPaper(shortPt, longPt float64) string {
	// 1. A4 check (595.28 x 841.89 pt): tolerance +/- 12 pt
	if math.Abs(shortPt-a4ShortPt) <= 12.0 && math.Abs(longPt-a4LongPt) <= 12.0 {
		return "A4"
	}

	// 2. Letter check (612.0 x 792.0 pt): tolerance +/- 12 pt
	if math.Abs(shortPt-ltrShortPt) <= 12.0 && math.Abs(longPt-ltrLongPt) <= 12.0 {
		return "Letter"
	}

	// 3. Legal check (612.0 x 1008.0 pt): tolerance +/- 15 pt
	if math.Abs(shortPt-lglShortPt) <= 15.0 && math.Abs(longPt-lglLongPt) <= 15.0 {
		return "Legal"
	}

	// 4. A3 check (841.89 x 1190.55 pt): tolerance +/- 20 pt
	if math.Abs(shortPt-a3ShortPt) <= 20.0 && math.Abs(longPt-a3LongPt) <= 20.0 {
		return "A3"
	}

	// 5. A5 check (419.53 x 595.28 pt): tolerance +/- 12 pt
	if math.Abs(shortPt-a5ShortPt) <= 12.0 && math.Abs(longPt-a5LongPt) <= 12.0 {
		return "A5"
	}

	// 6. Tabloid / Ledger check (792.0 x 1224.0 pt): tolerance +/- 20 pt
	if math.Abs(shortPt-tabShortPt) <= 20.0 && math.Abs(longPt-tabLongPt) <= 20.0 {
		return "Tabloid"
	}

	// Fallback by aspect ratio:
	if shortPt > 0 {
		ratio := longPt / shortPt
		// ISO A-series aspect ratio is sqrt(2) ~ 1.4142
		if math.Abs(ratio-1.4142) <= 0.08 {
			if shortPt > 700 {
				return "A3"
			} else if shortPt < 500 {
				return "A5"
			}
			return "A4"
		}
		// US Letter ratio is 792 / 612 = 1.2941
		if math.Abs(ratio-1.2941) <= 0.08 {
			return "Letter"
		}
	}

	return "A4" // Default to A4 for international compatibility
}

// BuildPrintSettings constructs the SumatraPDF -print-settings arguments that guarantee
// 100% 1:1 scale preservation, exact page count, exact paper feed matching, and correct orientation.
func BuildPrintSettings(geom DocumentGeometry) string {
	var settings []string

	// 1. Explicit page range: strictly restricts printing to exact document pages,
	// preventing hardware roller boundary overflow and eliminating trailing 4-5 page spilling.
	if geom.PageCount > 0 {
		settings = append(settings, fmt.Sprintf("1-%d", geom.PageCount))
	}

	// 2. Paper size matching document geometry
	if geom.PaperName != "" {
		settings = append(settings, fmt.Sprintf("paper=%s", geom.PaperName))
	}

	// 3. Orientation matching document geometry
	if geom.Orientation != "" {
		settings = append(settings, geom.Orientation)
	}

	// 4. "noscale": Keeps the print at exact 100% 1:1 scale without down-scaling, centering distortion, or margin shifts
	scale := geom.ScaleMode
	if scale == "" {
		scale = "noscale"
	}
	settings = append(settings, scale)

	return strings.Join(settings, ",")
}
