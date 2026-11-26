package printer

import (
	"archive/zip"
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Standard paper dimensions in points (72 points = 1 inch, 25.4 mm = 72 points)
const (
	ptPerMm = 72.0 / 25.4 // ~2.8346 pt per mm

	// Standard ISO & ANSI page sizes (in points, normalized: short edge, long edge)
	a2ShortPt   = 420.0 * ptPerMm  // 1190.551
	a2LongPt    = 594.0 * ptPerMm  // 1683.780
	a3ShortPt   = 297.0 * ptPerMm  // 841.890
	a3LongPt    = 420.0 * ptPerMm  // 1190.551
	a4ShortPt   = 210.0 * ptPerMm  // 595.276
	a4LongPt    = 297.0 * ptPerMm  // 841.890
	a5ShortPt   = 148.0 * ptPerMm  // 419.528
	a5LongPt    = 210.0 * ptPerMm  // 595.276
	a6ShortPt   = 105.0 * ptPerMm  // 297.638
	a6LongPt    = 148.0 * ptPerMm  // 419.528
	ltrShortPt  = 8.5 * 72.0       // 612.0
	ltrLongPt   = 11.0 * 72.0      // 792.0
	lglShortPt  = 8.5 * 72.0       // 612.0
	lglLongPt   = 14.0 * 72.0      // 1008.0
	tabShortPt  = 11.0 * 72.0      // 792.0
	tabLongPt   = 17.0 * 72.0      // 1224.0
	stmtShortPt = 5.5 * 72.0       // 396.0
	stmtLongPt  = 8.5 * 72.0       // 612.0
)

// DocumentGeometry captures the detected dimensions, orientation, paper size, and page count.
type DocumentGeometry struct {
	Format            string  // ".pdf", ".xps", ".oxps", or image extension (".png", ".jpg", etc.)
	WidthPt           float64 // Document width in points (72 DPI)
	HeightPt          float64 // Document height in points (72 DPI)
	PaperName         string  // Standard paper name for print drivers: "A4", "Letter", "Legal", "A3", "A2", "A5", "Tabloid", or "" if mixed/oversized
	Orientation       string  // "portrait", "landscape", or "" if mixed
	PageCount         int     // Number of pages detected
	ScaleMode         string  // "shrink", "noscale", or "fit"
	MixedSizes        bool    // True when pages in the document do not share a uniform paper size
	MixedOrientations bool    // True when pages in the document have mixed portrait/landscape orientations
}

type pageDimensions struct {
	widthPt  float64
	heightPt float64
}

type pdfPageAttrs struct {
	hasMediaBox bool
	mediaW      float64
	mediaH      float64
	hasCropBox  bool
	cropW       float64
	cropH       float64
	rotate      int
	userUnit    float64
}

var (
	mediaBoxOnlyRegex = regexp.MustCompile(`/MediaBox\s*\[\s*([-\d\.]+)\s+([-\d\.]+)\s+([-\d\.]+)\s+([-\d\.]+)\s*\]`)
	cropBoxOnlyRegex  = regexp.MustCompile(`/CropBox\s*\[\s*([-\d\.]+)\s+([-\d\.]+)\s+([-\d\.]+)\s+([-\d\.]+)\s*\]`)
	mediaBoxRefRegex  = regexp.MustCompile(`/MediaBox\s+(\d+)\s+\d+\s+R\b`)
	cropBoxRefRegex   = regexp.MustCompile(`/CropBox\s+(\d+)\s+\d+\s+R\b`)
	bareBoxRegex      = regexp.MustCompile(`\[\s*([-\d\.]+)\s+([-\d\.]+)\s+([-\d\.]+)\s+([-\d\.]+)\s*\]`)
	rotateRegex       = regexp.MustCompile(`/Rotate\s+(-?\d+)`)
	userUnitRegex     = regexp.MustCompile(`/UserUnit\s+([\d\.]+)`)
	pageTypeRegex     = regexp.MustCompile(`/Type\s*/Page\b`)
	pagesTypeRegex    = regexp.MustCompile(`/Type\s*/Pages\b`)
	catalogTypeRegex  = regexp.MustCompile(`/Type\s*/Catalog\b`)
	rootRefRegex      = regexp.MustCompile(`/Root\s+(\d+)\s+\d+\s+R\b`)
	pagesRefRegex     = regexp.MustCompile(`/Pages\s+(\d+)\s+\d+\s+R\b`)
	countRegex        = regexp.MustCompile(`/Count\s+(\d+)`)
	kidsInlineRegex   = regexp.MustCompile(`(?s)/Kids\s*\[(.*?)\]`)
	kidsRefRegex      = regexp.MustCompile(`/Kids\s+(\d+)\s+\d+\s+R\b`)
	objRefRegex       = regexp.MustCompile(`(\d+)\s+\d+\s+R\b`)
	objStartRegex     = regexp.MustCompile(`(?:^|[\r\n\s])(\d+)\s+\d+\s+obj\b`)
	streamStartRegex  = regexp.MustCompile(`>>\s*stream(?:\r\n|\n|\r)`)
	endstreamRegex    = regexp.MustCompile(`(?:\r\n|\n|\r)?endstream\b`)
	endobjRegex       = regexp.MustCompile(`\bendobj\b`)
	objStmTypeRegex   = regexp.MustCompile(`/Type\s*/ObjStm\b`)
	objStmNRegex      = regexp.MustCompile(`/N\s+(\d+)`)
	objStmFirstRegex  = regexp.MustCompile(`/First\s+(\d+)`)
	directLenRegex    = regexp.MustCompile(`/Length\s+(\d+)\b`)
	indirectLenRegex  = regexp.MustCompile(`/Length\s+\d+\s+\d+\s+R\b`)
	fpageDimRegex     = regexp.MustCompile(`(?i)<FixedPage[^>]*\bWidth\s*=\s*["']([\d\.]+)["'][^>]*\bHeight\s*=\s*["']([\d\.]+)["']`)
	fpageAltRegex     = regexp.MustCompile(`(?i)<FixedPage[^>]*\bHeight\s*=\s*["']([\d\.]+)["'][^>]*\bWidth\s*=\s*["']([\d\.]+)["']`)
)

// DetectDocumentGeometry inspects a PDF, XPS, or image file to determine its native dimensions,
// target paper size (A4, Letter, etc.), orientation, scaling mode, and page count.
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

	// Determine format from magic bytes if extension is missing/generic or mismatched
	if imgExt := detectImageMagic(header); imgExt != "" {
		if !isImageExtension(geom.Format) {
			geom.Format = imgExt
		}
	} else if bytes.HasPrefix(header, []byte("PK\x03\x04")) {
		if detected := detectZIPSpoolFormatFromFile(filePath); detected != "" {
			geom.Format = detected
		} else if geom.Format != ".oxps" {
			geom.Format = ".xps"
		}
	} else if bytes.HasPrefix(header, []byte("%PDF-")) || bytes.Contains(header, []byte("%PDF-")) {
		geom.Format = ".pdf"
	}

	if isImageExtension(geom.Format) {
		detectImageGeometry(f, &geom)
		return geom
	}

	var pages []pageDimensions
	if geom.Format == ".xps" || geom.Format == ".oxps" {
		pages = detectXPSGeometry(filePath, &geom)
	} else {
		pages = detectPDFGeometry(f, &geom)
	}

	finalizeMultiPageGeometry(pages, &geom)
	return geom
}

func isImageExtension(ext string) bool {
	switch strings.ToLower(ext) {
	case ".png", ".jpg", ".jpeg", ".tif", ".tiff", ".gif", ".bmp":
		return true
	default:
		return false
	}
}

func detectImageMagic(header []byte) string {
	switch {
	case bytes.HasPrefix(header, []byte("\x89PNG\r\n\x1a\n")):
		return ".png"
	case bytes.HasPrefix(header, []byte("\xff\xd8\xff")):
		return ".jpg"
	case bytes.HasPrefix(header, []byte("GIF87a")) || bytes.HasPrefix(header, []byte("GIF89a")):
		return ".gif"
	case bytes.HasPrefix(header, []byte("BM")):
		return ".bmp"
	case bytes.HasPrefix(header, []byte("II*\x00")) || bytes.HasPrefix(header, []byte("MM\x00*")):
		return ".tif"
	default:
		return ""
	}
}

func detectImageGeometry(f *os.File, geom *DocumentGeometry) {
	geom.PageCount = 1
	geom.ScaleMode = "fit"
	geom.PaperName = "A4"
	geom.Orientation = "portrait"

	w, h, orient := readImageDimensionsAndOrientation(f)
	if w > 0 && h > 0 {
		// Apply EXIF/TIFF orientation 5..8 (90/270 degree rotation)
		if orient >= 5 && orient <= 8 {
			w, h = h, w
		}
		// Convert pixels to points at 96 DPI (72/96 = 0.75 pt/px)
		geom.WidthPt = float64(w) * 0.75
		geom.HeightPt = float64(h) * 0.75
		if w > h {
			geom.Orientation = "landscape"
		} else {
			geom.Orientation = "portrait"
		}
	}
}

func readImageDimensionsAndOrientation(f *os.File) (width int, height int, exifOrient int) {
	_, _ = f.Seek(0, io.SeekStart)
	var magic [8]byte
	n, _ := io.ReadFull(f, magic[:])
	hdr := magic[:n]

	// 1. TIFF (II*\x00 or MM\x00*)
	if bytes.HasPrefix(hdr, []byte("II*\x00")) || bytes.HasPrefix(hdr, []byte("MM\x00*")) {
		var order binary.ByteOrder = binary.LittleEndian
		if hdr[0] == 'M' {
			order = binary.BigEndian
		}
		w, h, orient := parseTIFFIFD0(f, order, 0)
		return w, h, orient
	}

	// 2. BMP (width/height at bytes 18..25)
	if bytes.HasPrefix(hdr, []byte("BM")) {
		_, _ = f.Seek(0, io.SeekStart)
		var bmpHdr [26]byte
		if bn, err := io.ReadFull(f, bmpHdr[:]); err == nil && bn == 26 {
			w := int(int32(bmpHdr[18]) | int32(bmpHdr[19])<<8 | int32(bmpHdr[20])<<16 | int32(bmpHdr[21])<<24)
			h := int(int32(bmpHdr[22]) | int32(bmpHdr[23])<<8 | int32(bmpHdr[24])<<16 | int32(bmpHdr[25])<<24)
			if h < 0 {
				h = -h
			}
			return w, h, 0
		}
	}

	// 3. Standard PNG / JPEG / GIF via image.DecodeConfig
	_, _ = f.Seek(0, io.SeekStart)
	if cfg, format, err := image.DecodeConfig(f); err == nil && cfg.Width > 0 && cfg.Height > 0 {
		width, height = cfg.Width, cfg.Height
		if format == "jpeg" {
			exifOrient = readJPEGExifOrientation(f)
		}
		return width, height, exifOrient
	}

	return 0, 0, 0
}

// readJPEGExifOrientation scans JPEG APP1 segments for an Exif TIFF IFD0 Orientation tag (0x0112).
func readJPEGExifOrientation(f *os.File) int {
	_, _ = f.Seek(2, io.SeekStart)
	var marker [4]byte
	for i := 0; i < 64; i++ {
		if _, err := io.ReadFull(f, marker[:2]); err != nil {
			return 0
		}
		if marker[0] != 0xFF {
			return 0
		}
		// Skip fill bytes
		for marker[1] == 0xFF {
			if _, err := io.ReadFull(f, marker[1:2]); err != nil {
				return 0
			}
		}
		// Start of Scan (0xDA) or EOI (0xD9): no more metadata segments
		if marker[1] == 0xDA || marker[1] == 0xD9 {
			return 0
		}
		// Standalone markers without length (RSTn, TEM)
		if marker[1] == 0x01 || (marker[1] >= 0xD0 && marker[1] <= 0xD8) {
			continue
		}
		if _, err := io.ReadFull(f, marker[2:4]); err != nil {
			return 0
		}
		segLen := int(binary.BigEndian.Uint16(marker[2:4]))
		if segLen < 2 {
			return 0
		}
		payloadLen := segLen - 2
		segStart, err := f.Seek(0, io.SeekCurrent)
		if err != nil {
			return 0
		}

		// APP1 (0xE1) with "Exif\x00\x00" header
		if marker[1] == 0xE1 && payloadLen >= 14 {
			var exifHdr [8]byte
			if _, err := io.ReadFull(f, exifHdr[:6]); err == nil && bytes.Equal(exifHdr[:6], []byte("Exif\x00\x00")) {
				tiffBase := segStart + 6
				var tiffMagic [4]byte
				if _, err := io.ReadFull(f, tiffMagic[:4]); err == nil {
					var order binary.ByteOrder
					if bytes.Equal(tiffMagic[:4], []byte("II*\x00")) {
						order = binary.LittleEndian
					} else if bytes.Equal(tiffMagic[:4], []byte("MM\x00*")) {
						order = binary.BigEndian
					}
					if order != nil {
						_, _, orient := parseTIFFIFD0(f, order, tiffBase)
						return orient
					}
				}
			}
		}

		if _, err := f.Seek(segStart+int64(payloadLen), io.SeekStart); err != nil {
			return 0
		}
	}
	return 0
}

// parseTIFFIFD0 reads ImageWidth (0x0100), ImageLength (0x0101), and Orientation (0x0112)
// from a TIFF IFD0 starting relative to tiffBase.
func parseTIFFIFD0(r io.ReaderAt, order binary.ByteOrder, tiffBase int64) (width int, height int, orient int) {
	var ifdOffBuf [4]byte
	if _, err := r.ReadAt(ifdOffBuf[:], tiffBase+4); err != nil {
		return 0, 0, 0
	}
	ifdOff := int64(order.Uint32(ifdOffBuf[:]))
	if ifdOff < 8 {
		return 0, 0, 0
	}

	var countBuf [2]byte
	if _, err := r.ReadAt(countBuf[:], tiffBase+ifdOff); err != nil {
		return 0, 0, 0
	}
	numEntries := int(order.Uint16(countBuf[:]))
	if numEntries <= 0 || numEntries > 512 {
		return 0, 0, 0
	}

	entryBuf := make([]byte, numEntries*12)
	if _, err := r.ReadAt(entryBuf, tiffBase+ifdOff+2); err != nil {
		return 0, 0, 0
	}

	readScalar := func(entry []byte) int {
		fieldType := order.Uint16(entry[2:4])
		count := order.Uint32(entry[4:8])
		if count == 0 {
			return 0
		}
		switch fieldType {
		case 3: // SHORT (2 bytes)
			return int(order.Uint16(entry[8:10]))
		case 4: // LONG (4 bytes)
			return int(order.Uint32(entry[8:12]))
		case 1: // BYTE (1 byte)
			return int(entry[8])
		default:
			return 0
		}
	}

	for i := 0; i < numEntries; i++ {
		entry := entryBuf[i*12 : (i+1)*12]
		tag := order.Uint16(entry[0:2])
		switch tag {
		case 0x0100: // ImageWidth
			width = readScalar(entry)
		case 0x0101: // ImageLength
			height = readScalar(entry)
		case 0x0112: // Orientation
			orient = readScalar(entry)
		}
	}
	return width, height, orient
}

func detectPDFGeometry(f *os.File, geom *DocumentGeometry) []pageDimensions {
	fi, err := f.Stat()
	if err != nil {
		return nil
	}
	_, _ = f.Seek(0, io.SeekStart)

	// Read full file up to 32MB to ensure multi-page counts and dictionaries are never truncated
	readLimit := int64(32 * 1024 * 1024)
	if fi.Size() < readLimit {
		readLimit = fi.Size()
	}
	sample, err := io.ReadAll(io.LimitReader(f, readLimit))
	if err != nil && len(sample) == 0 {
		return nil
	}

	// For large PDFs, inspect trailing 256KB separately so trailer / XRef root is captured
	var tailBuf []byte
	if fi.Size() > readLimit {
		tailSize := int64(256 * 1024)
		if fi.Size() > tailSize {
			buf := make([]byte, tailSize)
			if _, err := f.ReadAt(buf, fi.Size()-tailSize); err == nil {
				tailBuf = buf
			}
		}
	}

	objects, objStmTexts := parsePDFObjects(sample)
	if len(tailBuf) > 0 {
		tailObjs, tailStms := parsePDFObjects(tailBuf)
		for k, v := range tailObjs {
			objects[k] = v
		}
		objStmTexts = append(objStmTexts, tailStms...)
	}

	// 1. Resolve root /Catalog -> /Pages -> /Count and walk /Kids tree
	rootCatalogID := findRootCatalogID(sample, tailBuf, objects)
	var pages []pageDimensions
	rootCount := 0

	if rootCatalogID > 0 {
		if catObj, ok := objects[rootCatalogID]; ok {
			if pm := pagesRefRegex.FindSubmatch(catObj); len(pm) == 2 {
				rootPagesID, _ := strconv.Atoi(string(pm[1]))
				if rootPagesObj, ok := objects[rootPagesID]; ok {
					if cm := countRegex.FindSubmatch(rootPagesObj); len(cm) == 2 {
						if c, err := strconv.Atoi(string(cm[1])); err == nil && c > 0 {
							rootCount = c
						}
					}
					visited := make(map[int]bool)
					collectPDFPages(rootPagesID, objects, pdfPageAttrs{userUnit: 1.0}, visited, &pages)
				}
			}
		}
	}

	if rootCount > 0 {
		geom.PageCount = rootCount
	} else if len(pages) > 0 {
		geom.PageCount = len(pages)
	}

	// 2. Fallback for minimal/synthetic PDFs missing complete /Catalog -> /Pages linkage
	if len(pages) == 0 {
		searchBuf := sample
		for _, dec := range objStmTexts {
			searchBuf = append(searchBuf, '\n')
			searchBuf = append(searchBuf, dec...)
		}

		// Prefer /CropBox (visible area) over /MediaBox, falling back to /MediaBox
		if w, h, ok := parseBoxDims(searchBuf, cropBoxOnlyRegex); ok {
			uu := parseUserUnit(searchBuf)
			w *= uu
			h *= uu
			if rot := parseRotate(searchBuf); rot == 90 || rot == 270 {
				w, h = h, w
			}
			pages = append(pages, pageDimensions{widthPt: w, heightPt: h})
		} else if w, h, ok := parseBoxDims(searchBuf, mediaBoxOnlyRegex); ok {
			uu := parseUserUnit(searchBuf)
			w *= uu
			h *= uu
			if rot := parseRotate(searchBuf); rot == 90 || rot == 270 {
				w, h = h, w
			}
			pages = append(pages, pageDimensions{widthPt: w, heightPt: h})
		}

		if geom.PageCount <= 1 && rootCount == 0 {
			if count := len(pageTypeRegex.FindAll(searchBuf, -1)); count > 0 {
				geom.PageCount = count
			}
		}
	}

	return pages
}

func parsePDFObjects(data []byte) (map[int][]byte, [][]byte) {
	objects := make(map[int][]byte)
	var objStmTexts [][]byte

	cursor := 0
	for cursor < len(data) {
		loc := objStartRegex.FindIndex(data[cursor:])
		if loc == nil {
			break
		}
		matchBytes := data[cursor+loc[0] : cursor+loc[1]]
		sub := objStartRegex.FindSubmatch(matchBytes)
		if len(sub) != 2 {
			cursor += loc[1]
			continue
		}
		objNum, _ := strconv.Atoi(string(sub[1]))
		bodyStart := cursor + loc[1]

		endobjLoc := endobjRegex.FindIndex(data[bodyStart:])
		streamLoc := streamStartRegex.FindIndex(data[bodyStart:])

		// Stream object if >> stream appears before endobj
		if streamLoc != nil && (endobjLoc == nil || streamLoc[0] < endobjLoc[0]) {
			dictPart := data[bodyStart : bodyStart+streamLoc[0]+2]
			streamDataStart := bodyStart + streamLoc[1]
			streamDataEnd := -1
			afterStream := streamDataStart

			// Use direct /Length if available and matches endstream boundary
			if !indirectLenRegex.Match(dictPart) {
				if lm := directLenRegex.FindSubmatch(dictPart); len(lm) == 2 {
					if streamLen, err := strconv.Atoi(string(lm[1])); err == nil && streamLen >= 0 && streamDataStart+streamLen <= len(data) {
						checkEnd := streamDataStart + streamLen + 32
						if checkEnd > len(data) {
							checkEnd = len(data)
						}
						if esLoc := endstreamRegex.FindIndex(data[streamDataStart+streamLen : checkEnd]); esLoc != nil {
							streamDataEnd = streamDataStart + streamLen
							afterStream = streamDataStart + streamLen + esLoc[1]
						}
					}
				}
			}

			if streamDataEnd == -1 {
				if esLoc := endstreamRegex.FindIndex(data[streamDataStart:]); esLoc != nil {
					streamDataEnd = streamDataStart + esLoc[0]
					afterStream = streamDataStart + esLoc[1]
				}
			}

			objects[objNum] = dictPart
			if streamDataEnd >= streamDataStart && objStmTypeRegex.Match(dictPart) {
				if dec := extractObjStm(dictPart, data[streamDataStart:streamDataEnd], objects); len(dec) > 0 {
					objStmTexts = append(objStmTexts, dec)
				}
			}

			if afterEndobj := endobjRegex.FindIndex(data[afterStream:]); afterEndobj != nil {
				cursor = afterStream + afterEndobj[1]
			} else {
				cursor = afterStream
			}
			continue
		}

		if endobjLoc != nil {
			objects[objNum] = data[bodyStart : bodyStart+endobjLoc[0]]
			cursor = bodyStart + endobjLoc[1]
		} else {
			objects[objNum] = data[bodyStart:]
			break
		}
	}

	return objects, objStmTexts
}

func extractObjStm(dict, streamData []byte, objects map[int][]byte) []byte {
	if bytes.Contains(dict, []byte("/Filter")) {
		if !bytes.Contains(dict, []byte("/FlateDecode")) {
			return nil
		}
		zr, err := zlib.NewReader(bytes.NewReader(streamData))
		if err != nil {
			return nil
		}
		defer zr.Close()
		decompressed, err := io.ReadAll(io.LimitReader(zr, 32*1024*1024))
		if err != nil && len(decompressed) == 0 {
			return nil
		}
		streamData = decompressed
	}

	nMatch := objStmNRegex.FindSubmatch(dict)
	firstMatch := objStmFirstRegex.FindSubmatch(dict)
	if len(nMatch) != 2 || len(firstMatch) != 2 {
		return streamData
	}
	count, _ := strconv.Atoi(string(nMatch[1]))
	first, _ := strconv.Atoi(string(firstMatch[1]))
	if count <= 0 || first < 0 || first > len(streamData) {
		return streamData
	}

	type objEntry struct {
		id  int
		off int
	}
	fields := strings.Fields(string(streamData[:first]))
	entries := make([]objEntry, 0, count)
	for i := 0; i+1 < len(fields) && len(entries) < count; i += 2 {
		id, err1 := strconv.Atoi(fields[i])
		off, err2 := strconv.Atoi(fields[i+1])
		if err1 == nil && err2 == nil && off >= 0 {
			entries = append(entries, objEntry{id: id, off: off})
		}
	}

	for i, e := range entries {
		start := first + e.off
		if start > len(streamData) {
			continue
		}
		end := len(streamData)
		if i+1 < len(entries) {
			nextStart := first + entries[i+1].off
			if nextStart >= start && nextStart <= len(streamData) {
				end = nextStart
			}
		}
		objects[e.id] = streamData[start:end]
	}

	return streamData
}

func findRootCatalogID(sample, tailBuf []byte, objects map[int][]byte) int {
	// Prefer the last /Root reference in tailBuf (incremental updates) or sample
	for _, buf := range [][]byte{tailBuf, sample} {
		if len(buf) == 0 {
			continue
		}
		if matches := rootRefRegex.FindAllSubmatch(buf, -1); len(matches) > 0 {
			last := matches[len(matches)-1]
			if id, err := strconv.Atoi(string(last[1])); err == nil && id > 0 {
				return id
			}
		}
	}
	for id, body := range objects {
		if catalogTypeRegex.Match(body) {
			return id
		}
	}
	return 0
}

func collectPDFPages(objID int, objects map[int][]byte, inherited pdfPageAttrs, visited map[int]bool, out *[]pageDimensions) {
	if visited[objID] {
		return
	}
	visited[objID] = true

	obj, ok := objects[objID]
	if !ok {
		return
	}

	attrs := inherited
	if w, h, ok := resolveBox(obj, objects, mediaBoxOnlyRegex, mediaBoxRefRegex); ok {
		attrs.hasMediaBox = true
		attrs.mediaW = w
		attrs.mediaH = h
	}
	if w, h, ok := resolveBox(obj, objects, cropBoxOnlyRegex, cropBoxRefRegex); ok {
		attrs.hasCropBox = true
		attrs.cropW = w
		attrs.cropH = h
	}
	if rotateRegex.Match(obj) {
		attrs.rotate = parseRotate(obj)
	}
	if userUnitRegex.Match(obj) {
		attrs.userUnit = parseUserUnit(obj)
	}

	kidIDs := extractKidIDs(obj, objects)
	if len(kidIDs) > 0 || pagesTypeRegex.Match(obj) {
		for _, kidID := range kidIDs {
			collectPDFPages(kidID, objects, attrs, visited, out)
		}
		return
	}

	// Leaf /Page node: prefer /CropBox (visible/printed region), fall back to /MediaBox
	var w, h float64
	if attrs.hasCropBox && attrs.cropW > 0 && attrs.cropH > 0 {
		w, h = attrs.cropW, attrs.cropH
	} else if attrs.hasMediaBox {
		w, h = attrs.mediaW, attrs.mediaH
	}
	if w <= 0 || h <= 0 {
		return
	}

	if attrs.userUnit > 0 {
		w *= attrs.userUnit
		h *= attrs.userUnit
	}
	if attrs.rotate == 90 || attrs.rotate == 270 {
		w, h = h, w
	}

	*out = append(*out, pageDimensions{widthPt: w, heightPt: h})
}

func extractKidIDs(obj []byte, objects map[int][]byte) []int {
	var rawKids []byte
	if km := kidsInlineRegex.FindSubmatch(obj); len(km) == 2 {
		rawKids = km[1]
	} else if kr := kidsRefRegex.FindSubmatch(obj); len(kr) == 2 {
		refID, _ := strconv.Atoi(string(kr[1]))
		rawKids = objects[refID]
	}
	if len(rawKids) == 0 {
		return nil
	}
	matches := objRefRegex.FindAllSubmatch(rawKids, -1)
	ids := make([]int, 0, len(matches))
	for _, m := range matches {
		if id, err := strconv.Atoi(string(m[1])); err == nil && id > 0 {
			ids = append(ids, id)
		}
	}
	return ids
}

func resolveBox(obj []byte, objects map[int][]byte, inlineRe, refRe *regexp.Regexp) (float64, float64, bool) {
	if w, h, ok := parseBoxDims(obj, inlineRe); ok {
		return w, h, true
	}
	if rm := refRe.FindSubmatch(obj); len(rm) == 2 {
		refID, _ := strconv.Atoi(string(rm[1]))
		if target, ok := objects[refID]; ok {
			return parseBoxDims(target, bareBoxRegex)
		}
	}
	return 0, 0, false
}

func parseBoxDims(data []byte, re *regexp.Regexp) (float64, float64, bool) {
	m := re.FindSubmatch(data)
	if len(m) != 5 {
		return 0, 0, false
	}
	x0, err0 := strconv.ParseFloat(string(m[1]), 64)
	y0, err1 := strconv.ParseFloat(string(m[2]), 64)
	x1, err2 := strconv.ParseFloat(string(m[3]), 64)
	y1, err3 := strconv.ParseFloat(string(m[4]), 64)
	if err0 != nil || err1 != nil || err2 != nil || err3 != nil {
		return 0, 0, false
	}
	return math.Abs(x1 - x0), math.Abs(y1 - y0), true
}

func parseRotate(data []byte) int {
	m := rotateRegex.FindSubmatch(data)
	if len(m) != 2 {
		return 0
	}
	rot, err := strconv.Atoi(string(m[1]))
	if err != nil {
		return 0
	}
	return ((rot % 360) + 360) % 360
}

func parseUserUnit(data []byte) float64 {
	m := userUnitRegex.FindSubmatch(data)
	if len(m) != 2 {
		return 1.0
	}
	uu, err := strconv.ParseFloat(string(m[1]), 64)
	if err != nil || uu <= 0 {
		return 1.0
	}
	return uu
}

var (
	xpsPTOrientFeatureRe = regexp.MustCompile(`(?is)<[^:>]*:?Feature[^>]*name\s*=\s*["'][^"']*PageOrientation["'][^>]*>(.*?)</[^:>]*:?Feature>`)
	xpsPTOptionNameRe    = regexp.MustCompile(`(?is)<[^:>]*:?Option[^>]*name\s*=\s*["']([^"']+)["']`)
	xpsRelsTargetRe      = regexp.MustCompile(`(?is)<Relationship[^>]*Target\s*=\s*["']([^"']+)["']`)
)

func detectXPSGeometry(filePath string, geom *DocumentGeometry) []pageDimensions {
	zr, err := zip.OpenReader(filePath)
	if err != nil {
		return nil
	}
	defer zr.Close()

	var fpageFiles []*zip.File
	filesByLower := make(map[string]*zip.File, len(zr.File))
	ptByLower := make(map[string]string)

	for _, file := range zr.File {
		lower := strings.ToLower(strings.TrimLeft(filepath.ToSlash(file.Name), "/"))
		filesByLower[lower] = file
		if strings.HasSuffix(lower, ".fpage") {
			fpageFiles = append(fpageFiles, file)
		} else if strings.HasSuffix(lower, "_pt.xml") || strings.Contains(lower, "printticket") && strings.HasSuffix(lower, ".xml") {
			if orient := parseXPSPrintTicketOrientation(file); orient != "" {
				ptByLower[lower] = orient
			}
		}
	}
	if len(fpageFiles) == 0 {
		return nil
	}

	// Determine job/document-level PrintTicket fallback orientation
	jobDefaultOrient := ""
	for name, orient := range ptByLower {
		if strings.Contains(name, "job_pt.xml") || strings.Contains(name, "fixeddocseq") || strings.Contains(name, "doc") && !strings.Contains(name, "page") {
			jobDefaultOrient = orient
			break
		}
	}
	if jobDefaultOrient == "" && len(ptByLower) == 1 {
		for _, orient := range ptByLower {
			jobDefaultOrient = orient
		}
	}

	// Sort deterministically by natural order so 1.fpage comes before 2.fpage and 10.fpage
	sort.Slice(fpageFiles, func(i, j int) bool {
		return naturalLess(strings.ToLower(fpageFiles[i].Name), strings.ToLower(fpageFiles[j].Name))
	})

	geom.PageCount = len(fpageFiles)
	pages := make([]pageDimensions, 0, len(fpageFiles))

	for _, file := range fpageFiles {
		rc, err := file.Open()
		if err != nil {
			continue
		}
		data, _ := io.ReadAll(io.LimitReader(rc, 64*1024))
		_ = rc.Close()

		if len(data) == 0 {
			continue
		}

		var wPt, hPt float64
		// XPS Width and Height are in 1/96 inch units -> multiply by 0.75 for 1/72 inch points
		if m := fpageDimRegex.FindSubmatch(data); len(m) == 3 {
			w96, _ := strconv.ParseFloat(string(m[1]), 64)
			h96, _ := strconv.ParseFloat(string(m[2]), 64)
			wPt, hPt = w96*0.75, h96*0.75
		} else if m := fpageAltRegex.FindSubmatch(data); len(m) == 3 {
			h96, _ := strconv.ParseFloat(string(m[1]), 64)
			w96, _ := strconv.ParseFloat(string(m[2]), 64)
			wPt, hPt = w96*0.75, h96*0.75
		}
		if wPt <= 0 || hPt <= 0 {
			continue
		}

		ptOrient := resolvePagePrintTicketOrientation(file.Name, filesByLower, ptByLower, jobDefaultOrient)
		if ptOrient == "landscape" && wPt < hPt {
			wPt, hPt = hPt, wPt
		} else if ptOrient == "portrait" && wPt > hPt {
			wPt, hPt = hPt, wPt
		}

		pages = append(pages, pageDimensions{widthPt: wPt, heightPt: hPt})
	}

	return pages
}

func parseXPSPrintTicketOrientation(file *zip.File) string {
	rc, err := file.Open()
	if err != nil {
		return ""
	}
	data, _ := io.ReadAll(io.LimitReader(rc, 128*1024))
	_ = rc.Close()
	if len(data) == 0 {
		return ""
	}
	if fm := xpsPTOrientFeatureRe.FindSubmatch(data); len(fm) == 2 {
		if om := xpsPTOptionNameRe.FindSubmatch(fm[1]); len(om) == 2 {
			opt := strings.ToLower(string(om[1]))
			if strings.Contains(opt, "landscape") {
				return "landscape"
			}
			if strings.Contains(opt, "portrait") {
				return "portrait"
			}
		}
	}
	return ""
}

func resolvePagePrintTicketOrientation(fpageName string, filesByLower map[string]*zip.File, ptByLower map[string]string, fallback string) string {
	if len(ptByLower) == 0 {
		return ""
	}
	slashName := strings.TrimLeft(filepath.ToSlash(fpageName), "/")
	dir := filepath.ToSlash(filepath.Dir(slashName))
	base := filepath.Base(slashName)
	stem := strings.TrimSuffix(base, filepath.Ext(base))

	// 1. Check page .rels file (e.g. Documents/1/Pages/_rels/1.fpage.rels)
	relsKey := strings.ToLower(filepath.ToSlash(filepath.Join(dir, "_rels", base+".rels")))
	if relsFile, ok := filesByLower[relsKey]; ok {
		if rc, err := relsFile.Open(); err == nil {
			relsData, _ := io.ReadAll(io.LimitReader(rc, 32*1024))
			_ = rc.Close()
			for _, m := range xpsRelsTargetRe.FindAllSubmatch(relsData, -1) {
				target := strings.ToLower(string(m[1]))
				if !strings.Contains(target, "_pt.xml") && !strings.Contains(target, "printticket") {
					continue
				}
				resolved := strings.ToLower(strings.TrimLeft(filepath.ToSlash(filepath.Clean(filepath.Join(dir, target))), "/"))
				if orient, found := ptByLower[resolved]; found {
					return orient
				}
			}
		}
	}

	// 2. Match Metadata/Page<N>_PT.xml or <stem>_PT.xml
	for name, orient := range ptByLower {
		ptBase := filepath.Base(name)
		if strings.EqualFold(ptBase, "page"+stem+"_pt.xml") || strings.EqualFold(ptBase, stem+"_pt.xml") {
			return orient
		}
	}

	return fallback
}

func naturalLess(a, b string) bool {
	ia, ib := 0, 0
	for ia < len(a) && ib < len(b) {
		da := a[ia] >= '0' && a[ia] <= '9'
		db := b[ib] >= '0' && b[ib] <= '9'
		if da && db {
			ja, jb := ia, ib
			for ja < len(a) && a[ja] >= '0' && a[ja] <= '9' {
				ja++
			}
			for jb < len(b) && b[jb] >= '0' && b[jb] <= '9' {
				jb++
			}
			na, errA := strconv.Atoi(a[ia:ja])
			nb, errB := strconv.Atoi(b[ib:jb])
			if errA == nil && errB == nil && na != nb {
				return na < nb
			}
			if a[ia:ja] != b[ib:jb] {
				return a[ia:ja] < b[ib:jb]
			}
			ia, ib = ja, jb
			continue
		}
		if a[ia] != b[ib] {
			return a[ia] < b[ib]
		}
		ia++
		ib++
	}
	return len(a) < len(b)
}

func isOversizedPaper(shortPt, longPt float64) bool {
	return shortPt > a2ShortPt+25.0 || longPt > a2LongPt+25.0
}

func finalizeMultiPageGeometry(pages []pageDimensions, geom *DocumentGeometry) {
	if len(pages) == 0 {
		return
	}

	var firstOrient, firstPaper string
	var firstShort, firstLong float64
	var firstOversized, anyOversized bool

	validSeen := false
	for _, p := range pages {
		if p.widthPt <= 0 || p.heightPt <= 0 {
			continue
		}

		orient := "portrait"
		if p.widthPt > p.heightPt {
			orient = "landscape"
		}
		shortEdge := math.Min(p.widthPt, p.heightPt)
		longEdge := math.Max(p.widthPt, p.heightPt)
		oversized := isOversizedPaper(shortEdge, longEdge)
		paper := ""
		if !oversized {
			paper = matchStandardPaper(shortEdge, longEdge)
		}
		if oversized {
			anyOversized = true
		}

		if !validSeen {
			validSeen = true
			geom.WidthPt = p.widthPt
			geom.HeightPt = p.heightPt
			firstOrient = orient
			firstPaper = paper
			firstShort = shortEdge
			firstLong = longEdge
			firstOversized = oversized
			continue
		}

		if orient != firstOrient {
			geom.MixedOrientations = true
		}
		if oversized != firstOversized || paper != firstPaper ||
			math.Abs(shortEdge-firstShort) > 25.0 || math.Abs(longEdge-firstLong) > 25.0 {
			geom.MixedSizes = true
		}
	}

	if !validSeen {
		return
	}

	if geom.MixedOrientations {
		geom.Orientation = ""
	} else {
		geom.Orientation = firstOrient
	}

	if geom.MixedSizes || anyOversized {
		geom.PaperName = ""
	} else {
		geom.PaperName = firstPaper
	}

	if anyOversized {
		geom.ScaleMode = "shrink"
	}
}

// matchStandardPaper matches short and long edge dimensions in points to a standard paper size
// supported by SumatraPDF/GDI print settings (A2, A3, A4, A5, A6, Letter, Legal, Tabloid, Statement).
// Custom/non-standard dimensions return "" so noscale never clips them onto an incompatible paper size.
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

	// 5. A2 check (1190.55 x 1683.78 pt): tolerance +/- 25 pt
	if math.Abs(shortPt-a2ShortPt) <= 25.0 && math.Abs(longPt-a2LongPt) <= 25.0 {
		return "A2"
	}

	// 6. A5 check (419.53 x 595.28 pt): tolerance +/- 12 pt
	if math.Abs(shortPt-a5ShortPt) <= 12.0 && math.Abs(longPt-a5LongPt) <= 12.0 {
		return "A5"
	}

	// 7. A6 check (297.64 x 419.53 pt): tolerance +/- 10 pt
	if math.Abs(shortPt-a6ShortPt) <= 10.0 && math.Abs(longPt-a6LongPt) <= 10.0 {
		return "A6"
	}

	// 8. Statement check (396.0 x 612.0 pt): tolerance +/- 10 pt
	if math.Abs(shortPt-stmtShortPt) <= 10.0 && math.Abs(longPt-stmtLongPt) <= 10.0 {
		return "Statement"
	}

	// 9. Tabloid / Ledger check (792.0 x 1224.0 pt): tolerance +/- 20 pt
	if math.Abs(shortPt-tabShortPt) <= 20.0 && math.Abs(longPt-tabLongPt) <= 20.0 {
		return "Tabloid"
	}

	// Oversized (> A2) or custom/non-standard dimensions: omit forced paper=... so noscale does not clip
	return ""
}

func isValidSumatraPaperName(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "a2", "a3", "a4", "a5", "a6", "letter", "legal", "tabloid", "statement":
		return true
	default:
		return false
	}
}

// BuildPrintSettings constructs the SumatraPDF -print-settings arguments that preserve
// 1:1 scale for uniform standard documents while safely handling mixed/oversized/image jobs.
func BuildPrintSettings(geom DocumentGeometry) string {
	var settings []string

	// 1. Explicit page range: strictly restricts printing to exact document pages
	if geom.PageCount > 0 {
		settings = append(settings, fmt.Sprintf("1-%d", geom.PageCount))
	}

	// 2. Paper size matching document geometry (omit when mixed or invalid/oversized like A0/A1)
	oversizedPaper := false
	if geom.PaperName != "" && !geom.MixedSizes {
		if isValidSumatraPaperName(geom.PaperName) {
			settings = append(settings, fmt.Sprintf("paper=%s", geom.PaperName))
		} else {
			oversizedPaper = true
		}
	}

	// 3. Orientation matching document geometry (omit when mixed so SumatraPDF auto-rotates per page)
	if geom.Orientation != "" && !geom.MixedOrientations {
		settings = append(settings, geom.Orientation)
	}

	// 4. Scaling mode: default "noscale" for 1:1 accuracy, "shrink" for oversized, "fit" for images
	scale := geom.ScaleMode
	if scale == "" {
		scale = "noscale"
	}
	if oversizedPaper && scale == "noscale" {
		scale = "shrink"
	}
	settings = append(settings, scale)

	return strings.Join(settings, ",")
}

// BuildPrintSettingsWithOptions builds SumatraPDF -print-settings with optional PrintOptions overrides.
func BuildPrintSettingsWithOptions(geom DocumentGeometry, opts PrintOptions) string {
	merged := applyPrintOptionsToGeometry(geom, opts)
	base := BuildPrintSettings(merged)

	var extra []string
	if opts.Copies > 1 {
		extra = append(extra, fmt.Sprintf("%dx", opts.Copies))
	}
	if opts.Color != nil {
		if *opts.Color {
			extra = append(extra, "color")
		} else {
			extra = append(extra, "monochrome")
		}
	}
	if len(extra) == 0 {
		return base
	}
	if base == "" {
		return strings.Join(extra, ",")
	}
	return base + "," + strings.Join(extra, ",")
}

func applyPrintOptionsToGeometry(geom DocumentGeometry, opts PrintOptions) DocumentGeometry {
	if p := strings.TrimSpace(opts.PaperSize); p != "" {
		geom.PaperName = p
		geom.MixedSizes = false
	}
	if o := strings.ToLower(strings.TrimSpace(opts.Orientation)); o == "portrait" || o == "landscape" {
		geom.Orientation = o
		geom.MixedOrientations = false
	}
	if opts.PageCount > 0 {
		geom.PageCount = opts.PageCount
	}
	if s := strings.TrimSpace(opts.ScaleMode); s != "" {
		geom.ScaleMode = s
	}
	return geom
}
