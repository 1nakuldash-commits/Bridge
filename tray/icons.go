package tray

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
)

type iconDir struct {
	Reserved uint16
	Type     uint16
	Count    uint16
}

type iconDirEntry struct {
	Width       byte
	Height      byte
	ColorCount  byte
	Reserved    byte
	Planes      uint16
	BitCount    uint16
	BytesInRes  uint32
	ImageOffset uint32
}

type bitmapInfoHeader struct {
	Size          uint32
	Width         int32
	Height        int32 // doubled for ICO
	Planes        uint16
	BitCount      uint16
	Compression   uint32
	SizeImage     uint32
	XPelsPerMeter int32
	YPelsPerMeter int32
	ClrUsed       uint32
	ClrImportant  uint32
}

type rgbaPixel struct {
	R, G, B, A byte
}

// generateIco creates a multi-resolution (16x16, 32x32, and 48x48) 32-bit RGBA Windows ICO.
func generateIco(renderFunc func(sz int) [][]rgbaPixel) []byte {
	sizes := []int{16, 32, 48}
	numImages := len(sizes)

	type imgPayload struct {
		size       int
		header     bitmapInfoHeader
		pixelBytes []byte
		maskBytes  []byte
		totalSize  uint32
	}

	payloads := make([]imgPayload, numImages)
	for i, sz := range sizes {
		pixels := renderFunc(sz)

		// Bottom-up BGRA rows
		pixelBuf := new(bytes.Buffer)
		for y := sz - 1; y >= 0; y-- {
			for x := 0; x < sz; x++ {
				p := pixels[y][x]
				pixelBuf.WriteByte(p.B)
				pixelBuf.WriteByte(p.G)
				pixelBuf.WriteByte(p.R)
				pixelBuf.WriteByte(p.A)
			}
		}

		// 1-bit AND mask padded to 32-bit boundary per row
		rowBytes := (sz + 31) / 32 * 4
		maskBuf := new(bytes.Buffer)
		for y := 0; y < sz; y++ {
			row := make([]byte, rowBytes)
			for x := 0; x < sz; x++ {
				if pixels[sz-1-y][x].A == 0 {
					row[x/8] |= (1 << (7 - (x % 8)))
				}
			}
			maskBuf.Write(row)
		}

		imgSize := uint32(40 + pixelBuf.Len() + maskBuf.Len())
		bih := bitmapInfoHeader{
			Size:        40,
			Width:       int32(sz),
			Height:      int32(sz * 2), // doubled for ICO
			Planes:      1,
			BitCount:    32,
			Compression: 0,
			SizeImage:   uint32(pixelBuf.Len() + maskBuf.Len()),
		}

		payloads[i] = imgPayload{
			size:       sz,
			header:     bih,
			pixelBytes: pixelBuf.Bytes(),
			maskBytes:  maskBuf.Bytes(),
			totalSize:  imgSize,
		}
	}

	out := new(bytes.Buffer)
	_ = binary.Write(out, binary.LittleEndian, iconDir{0, 1, uint16(numImages)})

	offset := uint32(6 + (16 * numImages))
	for _, p := range payloads {
		_ = binary.Write(out, binary.LittleEndian, iconDirEntry{
			Width:       byte(p.size),
			Height:      byte(p.size),
			ColorCount:  0,
			Reserved:    0,
			Planes:      1,
			BitCount:    32,
			BytesInRes:  p.totalSize,
			ImageOffset: offset,
		})
		offset += p.totalSize
	}

	for _, p := range payloads {
		_ = binary.Write(out, binary.LittleEndian, p.header)
		out.Write(p.pixelBytes)
		out.Write(p.maskBytes)
	}

	return out.Bytes()
}

// GenerateBridgeIcon creates a multi-resolution (16x16, 32x32, and 48x48) 32-bit RGBA modern
// architectural Bridge icon with illuminated status nodes for Windows High-DPI taskbars.
func GenerateBridgeIcon(r, g, b byte) []byte {
	return generateIco(func(sz int) [][]rgbaPixel {
		return renderBridgePixels(sz, r, g, b)
	})
}

// renderBridgePixels paints a modern Bridge mesh icon onto an RGBA pixel grid.
func renderBridgePixels(size int, r, g, b byte) [][]rgbaPixel {
	grid := make([][]rgbaPixel, size)
	for y := 0; y < size; y++ {
		grid[y] = make([]rgbaPixel, size)
	}

	setPixel := func(x, y int, pr, pg, pb, pa byte) {
		if x < 0 || x >= size || y < 0 || y >= size || pa == 0 {
			return
		}
		curr := grid[y][x]
		if curr.A == 0 {
			grid[y][x] = rgbaPixel{pr, pg, pb, pa}
			return
		}
		// Alpha blending
		fa := float64(pa) / 255.0
		inv := 1.0 - fa
		outR := byte(float64(pr)*fa + float64(curr.R)*inv)
		outG := byte(float64(pg)*fa + float64(curr.G)*inv)
		outB := byte(float64(pb)*fa + float64(curr.B)*inv)
		outA := byte(math.Min(255, float64(curr.A)+float64(pa)))
		grid[y][x] = rgbaPixel{outR, outG, outB, outA}
	}

	fillCircle := func(cx, cy, radius float64, pr, pg, pb byte, maxAlpha byte) {
		minX := int(math.Floor(cx - radius - 1))
		maxX := int(math.Ceil(cx + radius + 1))
		minY := int(math.Floor(cy - radius - 1))
		maxY := int(math.Ceil(cy + radius + 1))

		for y := minY; y <= maxY; y++ {
			for x := minX; x <= maxX; x++ {
				dx := float64(x) - cx
				dy := float64(y) - cy
				dist := math.Sqrt(dx*dx + dy*dy)
				if dist <= radius-0.5 {
					setPixel(x, y, pr, pg, pb, maxAlpha)
				} else if dist < radius+0.5 {
					a := byte(float64(maxAlpha) * (radius + 0.5 - dist))
					setPixel(x, y, pr, pg, pb, a)
				}
			}
		}
	}

	// Architectural Bridge Structure:
	scale := float64(size) / 16.0
	towerWidth := math.Max(1.5, 2.0*scale)
	leftTowerX := 2.5 * scale
	rightTowerX := float64(size) - 2.5*scale - towerWidth
	baseY := float64(size) - 2.0*scale
	topY := 3.0 * scale

	// 1. Dark sleek metallic frame colors for structural towers and deck
	frameR, frameG, frameB := byte(30), byte(41), byte(59)       // Deep slate-800
	highlightR, highlightG, highlightB := byte(100), byte(116), byte(139) // Slate-500

	// Draw bottom road deck
	deckY := int(baseY - 1.0*scale)
	for y := deckY; y <= int(baseY); y++ {
		for x := int(1.0 * scale); x < int(float64(size)-1.0*scale); x++ {
			setPixel(x, y, frameR, frameG, frameB, 220)
		}
	}

	// Draw left tower and right tower (vertical piers)
	for y := int(topY); y <= int(baseY); y++ {
		for x := int(leftTowerX); x <= int(leftTowerX+towerWidth); x++ {
			setPixel(x, y, frameR, frameG, frameB, 240)
		}
		for x := int(rightTowerX); x <= int(rightTowerX+towerWidth); x++ {
			setPixel(x, y, frameR, frameG, frameB, 240)
		}
	}

	// Highlight edge on towers
	for y := int(topY); y <= int(baseY); y++ {
		setPixel(int(leftTowerX), y, highlightR, highlightG, highlightB, 180)
		setPixel(int(rightTowerX+towerWidth), y, highlightR, highlightG, highlightB, 180)
	}

	// Draw graceful catenary suspension cables from towers to center
	centerX := float64(size) / 2.0
	for x := int(leftTowerX); x <= int(rightTowerX+towerWidth); x++ {
		// Parabolic curve: y = topY at towers, dips near deck in center
		normX := (float64(x) - centerX) / (centerX - leftTowerX)
		cableY := topY + (baseY-topY-1.0*scale)*(1.0-normX*normX*0.6)
		y := int(math.Round(cableY))
		setPixel(x, y, highlightR, highlightG, highlightB, 200)
	}

	// 2. Active illuminated status nodes:
	// - Left tower device node
	nodeRadius := math.Max(1.0, 1.4*scale)
	fillCircle(leftTowerX+towerWidth/2.0, topY, nodeRadius, r, g, b, 255)
	// - Right tower device node
	fillCircle(rightTowerX+towerWidth/2.0, topY, nodeRadius, r, g, b, 255)

	// - Central multi-function bridge mesh core (illuminated glowing status node)
	coreRadius := math.Max(1.8, 2.8*scale)
	coreY := baseY - 1.5*scale
	// Subtle glow halo
	fillCircle(centerX, coreY, coreRadius+1.0*scale, r, g, b, 90)
	// Main illuminated status core
	fillCircle(centerX, coreY, coreRadius, r, g, b, 255)
	// Bright center shine
	fillCircle(centerX, coreY-0.4*scale, coreRadius*0.4, 255, 255, 255, 220)

	return grid
}

// GenerateDotIcon generates a circular dot ICO file (backward compatibility).
func GenerateDotIcon(r, g, b byte) []byte {
	return GenerateBridgeIcon(r, g, b)
}

// GeneratePrinterIcon creates a multi-resolution Windows ICO of a printer.
// If crossed is true, a red diagonal cross line cuts through the printer (disconnected).
func GeneratePrinterIcon(r, g, b byte, crossed bool, loadingPhase int) []byte {
	return generateIco(func(sz int) [][]rgbaPixel {
		return renderPrinterPixels(sz, r, g, b, crossed, loadingPhase)
	})
}

// GenerateCloudIcon creates a multi-resolution Windows ICO of a cloud for file upload/syncing.
func GenerateCloudIcon(r, g, b byte, loadingPhase int, isComplete bool) []byte {
	return generateIco(func(sz int) [][]rgbaPixel {
		return renderCloudPixels(sz, r, g, b, loadingPhase, isComplete)
	})
}

func renderPrinterPixels(size int, r, g, b byte, crossed bool, loadingPhase int) [][]rgbaPixel {
	grid := make([][]rgbaPixel, size)
	for y := 0; y < size; y++ {
		grid[y] = make([]rgbaPixel, size)
	}

	setPixel := func(x, y int, pr, pg, pb, pa byte) {
		if x < 0 || x >= size || y < 0 || y >= size || pa == 0 {
			return
		}
		curr := grid[y][x]
		if curr.A == 0 {
			grid[y][x] = rgbaPixel{pr, pg, pb, pa}
			return
		}
		fa := float64(pa) / 255.0
		inv := 1.0 - fa
		outR := byte(float64(pr)*fa + float64(curr.R)*inv)
		outG := byte(float64(pg)*fa + float64(curr.G)*inv)
		outB := byte(float64(pb)*fa + float64(curr.B)*inv)
		outA := byte(math.Min(255, float64(curr.A)+float64(pa)))
		grid[y][x] = rgbaPixel{outR, outG, outB, outA}
	}

	fillRect := func(x1, y1, x2, y2 float64, pr, pg, pb, pa byte) {
		minX := int(math.Floor(x1))
		maxX := int(math.Ceil(x2))
		minY := int(math.Floor(y1))
		maxY := int(math.Ceil(y2))
		for y := minY; y <= maxY; y++ {
			for x := minX; x <= maxX; x++ {
				fx1 := math.Max(0, math.Min(1, float64(x+1)-x1))
				fx2 := math.Max(0, math.Min(1, x2-float64(x)))
				fy1 := math.Max(0, math.Min(1, float64(y+1)-y1))
				fy2 := math.Max(0, math.Min(1, y2-float64(y)))
				coverage := fx1 * fx2 * fy1 * fy2
				if coverage > 0.05 {
					setPixel(x, y, pr, pg, pb, byte(float64(pa)*coverage))
				}
			}
		}
	}

	fillCircle := func(cx, cy, radius float64, pr, pg, pb byte, maxAlpha byte) {
		minX := int(math.Floor(cx - radius - 1))
		maxX := int(math.Ceil(cx + radius + 1))
		minY := int(math.Floor(cy - radius - 1))
		maxY := int(math.Ceil(cy + radius + 1))
		for y := minY; y <= maxY; y++ {
			for x := minX; x <= maxX; x++ {
				dx := float64(x) - cx
				dy := float64(y) - cy
				dist := math.Sqrt(dx*dx + dy*dy)
				if dist <= radius-0.5 {
					setPixel(x, y, pr, pg, pb, maxAlpha)
				} else if dist < radius+0.5 {
					a := byte(float64(maxAlpha) * (radius + 0.5 - dist))
					setPixel(x, y, pr, pg, pb, a)
				}
			}
		}
	}

	drawLine := func(x1, y1, x2, y2, thickness float64, pr, pg, pb, pa byte) {
		dx := x2 - x1
		dy := y2 - y1
		length := math.Sqrt(dx*dx + dy*dy)
		if length == 0 {
			return
		}
		steps := int(length * 3)
		radius := thickness / 2.0
		for i := 0; i <= steps; i++ {
			t := float64(i) / float64(steps)
			cx := x1 + dx*t
			cy := y1 + dy*t
			fillCircle(cx, cy, radius, pr, pg, pb, pa)
		}
	}

	scale := float64(size) / 16.0

	// 1. Paper in top feeder tray
	fillRect(4.5*scale, 1.5*scale, 11.5*scale, 6.0*scale, 240, 244, 250, 255)
	fillRect(4.5*scale, 1.5*scale, 11.5*scale, 2.3*scale, 203, 213, 225, 255)

	// 2. Printer body (main enclosure)
	fillRect(1.5*scale, 5.0*scale, 14.5*scale, 11.8*scale, r, g, b, 255)
	// Top bevel highlight
	hiR := byte(math.Min(255, float64(r)*1.25+30))
	hiG := byte(math.Min(255, float64(g)*1.25+30))
	hiB := byte(math.Min(255, float64(b)*1.25+30))
	fillRect(1.5*scale, 5.0*scale, 14.5*scale, 6.0*scale, hiR, hiG, hiB, 255)
	// Bottom bevel shadow
	shR := byte(math.Max(0, float64(r)*0.75-20))
	shG := byte(math.Max(0, float64(g)*0.75-20))
	shB := byte(math.Max(0, float64(b)*0.75-20))
	fillRect(1.5*scale, 11.0*scale, 14.5*scale, 11.8*scale, shR, shG, shB, 255)

	// 3. Paper output slot
	fillRect(3.5*scale, 8.0*scale, 12.5*scale, 9.4*scale, 15, 23, 42, 255)

	// 4. Paper emerging from slot
	outputPaperY2 := 14.2 * scale
	fillRect(4.5*scale, 9.0*scale, 11.5*scale, outputPaperY2, 255, 255, 255, 255)
	// Printed text lines simulation
	fillRect(5.5*scale, 10.5*scale, 10.5*scale, 11.3*scale, 148, 163, 184, 255)
	fillRect(5.5*scale, 12.2*scale, 9.5*scale, 13.0*scale, 148, 163, 184, 255)

	// 5. LED Status Dot on printer front
	ledX := 13.0 * scale
	ledY := 6.6 * scale
	ledR, ledG, ledB := byte(255), byte(255), byte(255)
	if crossed {
		ledR, ledG, ledB = 239, 68, 68 // Red
	} else if loadingPhase > 0 {
		ledR, ledG, ledB = 245, 158, 11 // Amber pulsating
	} else if r == 59 && g == 130 {
		ledR, ledG, ledB = 147, 197, 253 // Blue
	} else if r == 34 && g == 197 {
		ledR, ledG, ledB = 134, 239, 172 // Green
	}
	fillCircle(ledX, ledY, 0.9*scale, ledR, ledG, ledB, 255)

	// 6. If disconnected/offline: Draw bold high-contrast red diagonal slash across entire printer
	if crossed {
		shadowThick := math.Max(2.0, 2.2*scale)
		slashThick := math.Max(1.4, 1.6*scale)
		// Dark outline shadow for visibility on light or dark taskbars
		drawLine(1.8*scale, 1.8*scale, 14.2*scale, 14.2*scale, shadowThick, 15, 23, 42, 240)
		// Vibrant red strike-through
		drawLine(1.8*scale, 1.8*scale, 14.2*scale, 14.2*scale, slashThick, 239, 68, 68, 255)
	}

	return grid
}

func renderCloudPixels(size int, r, g, b byte, loadingPhase int, isComplete bool) [][]rgbaPixel {
	grid := make([][]rgbaPixel, size)
	for y := 0; y < size; y++ {
		grid[y] = make([]rgbaPixel, size)
	}

	setPixel := func(x, y int, pr, pg, pb, pa byte) {
		if x < 0 || x >= size || y < 0 || y >= size || pa == 0 {
			return
		}
		curr := grid[y][x]
		if curr.A == 0 {
			grid[y][x] = rgbaPixel{pr, pg, pb, pa}
			return
		}
		fa := float64(pa) / 255.0
		inv := 1.0 - fa
		outR := byte(float64(pr)*fa + float64(curr.R)*inv)
		outG := byte(float64(pg)*fa + float64(curr.G)*inv)
		outB := byte(float64(pb)*fa + float64(curr.B)*inv)
		outA := byte(math.Min(255, float64(curr.A)+float64(pa)))
		grid[y][x] = rgbaPixel{outR, outG, outB, outA}
	}

	fillRect := func(x1, y1, x2, y2 float64, pr, pg, pb, pa byte) {
		minX := int(math.Floor(x1))
		maxX := int(math.Ceil(x2))
		minY := int(math.Floor(y1))
		maxY := int(math.Ceil(y2))
		for y := minY; y <= maxY; y++ {
			for x := minX; x <= maxX; x++ {
				fx1 := math.Max(0, math.Min(1, float64(x+1)-x1))
				fx2 := math.Max(0, math.Min(1, x2-float64(x)))
				fy1 := math.Max(0, math.Min(1, float64(y+1)-y1))
				fy2 := math.Max(0, math.Min(1, y2-float64(y)))
				coverage := fx1 * fx2 * fy1 * fy2
				if coverage > 0.05 {
					setPixel(x, y, pr, pg, pb, byte(float64(pa)*coverage))
				}
			}
		}
	}

	fillCircle := func(cx, cy, radius float64, pr, pg, pb byte, maxAlpha byte) {
		minX := int(math.Floor(cx - radius - 1))
		maxX := int(math.Ceil(cx + radius + 1))
		minY := int(math.Floor(cy - radius - 1))
		maxY := int(math.Ceil(cy + radius + 1))
		for y := minY; y <= maxY; y++ {
			for x := minX; x <= maxX; x++ {
				dx := float64(x) - cx
				dy := float64(y) - cy
				dist := math.Sqrt(dx*dx + dy*dy)
				if dist <= radius-0.5 {
					setPixel(x, y, pr, pg, pb, maxAlpha)
				} else if dist < radius+0.5 {
					a := byte(float64(maxAlpha) * (radius + 0.5 - dist))
					setPixel(x, y, pr, pg, pb, a)
				}
			}
		}
	}

	drawLine := func(x1, y1, x2, y2, thickness float64, pr, pg, pb, pa byte) {
		dx := x2 - x1
		dy := y2 - y1
		length := math.Sqrt(dx*dx + dy*dy)
		if length == 0 {
			return
		}
		steps := int(length * 3)
		radius := thickness / 2.0
		for i := 0; i <= steps; i++ {
			t := float64(i) / float64(steps)
			cx := x1 + dx*t
			cy := y1 + dy*t
			fillCircle(cx, cy, radius, pr, pg, pb, pa)
		}
	}

	scale := float64(size) / 16.0

	// 1. Cloud puffy circles
	// Left lobe
	fillCircle(4.8*scale, 9.6*scale, 2.9*scale, r, g, b, 255)
	// Center top main lobe
	fillCircle(8.2*scale, 6.2*scale, 3.8*scale, r, g, b, 255)
	// Right lobe
	fillCircle(11.8*scale, 9.4*scale, 2.7*scale, r, g, b, 255)
	// Base fill connecting the lobes
	fillRect(3.2*scale, 8.5*scale, 13.0*scale, 12.5*scale, r, g, b, 255)
	fillRect(4.8*scale, 6.5*scale, 11.8*scale, 12.5*scale, r, g, b, 255)

	// Top rim highlight
	hiR := byte(math.Min(255, float64(r)*1.3+35))
	hiG := byte(math.Min(255, float64(g)*1.3+35))
	hiB := byte(math.Min(255, float64(b)*1.3+35))
	fillCircle(8.2*scale, 5.8*scale, 3.4*scale, hiR, hiG, hiB, 180)

	// 2. Action glyph inside cloud
	if isComplete {
		// White checkmark for completed upload
		drawLine(5.5*scale, 9.5*scale, 7.5*scale, 11.5*scale, 1.8*scale, 255, 255, 255, 255)
		drawLine(7.5*scale, 11.5*scale, 11.5*scale, 6.8*scale, 1.8*scale, 255, 255, 255, 255)
	} else if loadingPhase >= 0 {
		// Upward arrow for file uploading / syncing
		// Vertical stem
		fillRect(7.6*scale, 7.8*scale, 8.8*scale, 11.8*scale, 255, 255, 255, 255)
		// Arrow wings
		drawLine(8.2*scale, 5.0*scale, 5.8*scale, 7.8*scale, 1.6*scale, 255, 255, 255, 255)
		drawLine(8.2*scale, 5.0*scale, 10.6*scale, 7.8*scale, 1.6*scale, 255, 255, 255, 255)
	}

	return grid
}

// IconPaths holds file paths to the generated state icons.
type IconPaths struct {
	// Specialized taskbar icons:
	PrinterDisconnected string // 🖨️❌ Printer with cross line (0 devices connected)
	PrinterGray         string // 🖨️⚪ Gray printer (partial devices connected)
	PrinterGreen        string // 🖨️🟢 Green printer (all devices connected)
	PrinterLoading      string // 🖨️🟡 Printer loading (active printing)
	PrinterBlue         string // 🖨️🔵 Printer blue (print completed)
	CloudLoading        string // ☁️🟡 Cloud loading (file upload / syncing)
	CloudBlue           string // ☁️🔵 Cloud blue (file upload completed)

	// Backward compatibility:
	Green  string
	Yellow string
	Blue   string
	Gray   string
}

// EnsureIcons generates the state icons in the designated directory.
func EnsureIcons(dir string) (IconPaths, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return IconPaths{}, fmt.Errorf("failed to create icon dir: %w", err)
	}

	paths := IconPaths{
		PrinterDisconnected: filepath.Join(dir, "printer_disconnected.ico"),
		PrinterGray:         filepath.Join(dir, "printer_gray.ico"),
		PrinterGreen:        filepath.Join(dir, "printer_green.ico"),
		PrinterLoading:      filepath.Join(dir, "printer_loading.ico"),
		PrinterBlue:         filepath.Join(dir, "printer_blue.ico"),
		CloudLoading:        filepath.Join(dir, "cloud_loading.ico"),
		CloudBlue:           filepath.Join(dir, "cloud_blue.ico"),

		Green:  filepath.Join(dir, "dot_green.ico"),
		Yellow: filepath.Join(dir, "dot_yellow.ico"),
		Blue:   filepath.Join(dir, "dot_blue.ico"),
		Gray:   filepath.Join(dir, "dot_gray.ico"),
	}

	// 1. 🖨️❌ Disconnected: Printer icon with red cross line (0 devices connected)
	if err := os.WriteFile(paths.PrinterDisconnected, GeneratePrinterIcon(100, 116, 139, true, 0), 0644); err != nil {
		return IconPaths{}, fmt.Errorf("failed to write disconnected icon: %w", err)
	}

	// 2. 🖨️⚪ Partial: Printer icon in sleek gray, cross removed (1 or more devices connected)
	if err := os.WriteFile(paths.PrinterGray, GeneratePrinterIcon(148, 163, 184, false, 0), 0644); err != nil {
		return IconPaths{}, fmt.Errorf("failed to write partial icon: %w", err)
	}

	// 3. 🖨️🟢 All Connected: Printer icon in vibrant emerald green (all devices connected)
	if err := os.WriteFile(paths.PrinterGreen, GeneratePrinterIcon(34, 197, 94, false, 0), 0644); err != nil {
		return IconPaths{}, fmt.Errorf("failed to write connected icon: %w", err)
	}

	// 4. 🖨️🟡 Printing: Printer icon in amber with loading/progress
	if err := os.WriteFile(paths.PrinterLoading, GeneratePrinterIcon(245, 158, 11, false, 1), 0644); err != nil {
		return IconPaths{}, fmt.Errorf("failed to write printing icon: %w", err)
	}

	// 5. 🖨️🔵 Print Complete: Printer icon in electric azure blue
	if err := os.WriteFile(paths.PrinterBlue, GeneratePrinterIcon(59, 130, 246, false, 0), 0644); err != nil {
		return IconPaths{}, fmt.Errorf("failed to write print-complete icon: %w", err)
	}

	// 6. ☁️🟡 Cloud Uploading / Syncing: Cloud icon in amber with upload arrow
	if err := os.WriteFile(paths.CloudLoading, GenerateCloudIcon(245, 158, 11, 1, false), 0644); err != nil {
		return IconPaths{}, fmt.Errorf("failed to write cloud-loading icon: %w", err)
	}

	// 7. ☁️🔵 Cloud Upload Complete: Cloud icon in electric azure blue with checkmark
	if err := os.WriteFile(paths.CloudBlue, GenerateCloudIcon(59, 130, 246, -1, true), 0644); err != nil {
		return IconPaths{}, fmt.Errorf("failed to write cloud-complete icon: %w", err)
	}

	// Backward-compatibility dot icons
	_ = os.WriteFile(paths.Green, paths_or_bytes(paths.PrinterGreen), 0644)
	_ = os.WriteFile(paths.Yellow, paths_or_bytes(paths.PrinterLoading), 0644)
	_ = os.WriteFile(paths.Blue, paths_or_bytes(paths.PrinterBlue), 0644)
	_ = os.WriteFile(paths.Gray, paths_or_bytes(paths.PrinterDisconnected), 0644)

	return paths, nil
}

func paths_or_bytes(path string) []byte {
	data, err := os.ReadFile(path)
	if err == nil {
		return data
	}
	return GenerateBridgeIcon(148, 163, 184)
}
