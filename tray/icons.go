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

// GenerateBridgeIcon creates a multi-resolution (16x16, 32x32, and 48x48) 32-bit RGBA modern
// architectural Bridge icon with illuminated status nodes for Windows High-DPI taskbars.
func GenerateBridgeIcon(r, g, b byte) []byte {
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
		pixels := renderBridgePixels(sz, r, g, b)

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

// IconPaths holds file paths to the generated state icons.
type IconPaths struct {
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
		Green:  filepath.Join(dir, "dot_green.ico"),
		Yellow: filepath.Join(dir, "dot_yellow.ico"),
		Blue:   filepath.Join(dir, "dot_blue.ico"),
		Gray:   filepath.Join(dir, "dot_gray.ico"),
	}

	// 🟢 Emerald Green: Ready / Mesh Connected (16, 185, 129)
	_ = os.WriteFile(paths.Green, GenerateBridgeIcon(16, 185, 129), 0644)

	// 🟡 Amber Gold: Active / Syncing / Multi-Device Transit (245, 158, 11)
	_ = os.WriteFile(paths.Yellow, GenerateBridgeIcon(245, 158, 11), 0644)

	// 🔵 Electric Azure: Print Dispatched / Operation Completed (59, 130, 246)
	_ = os.WriteFile(paths.Blue, GenerateBridgeIcon(59, 130, 246), 0644)

	// ⚪ Slate Silver: Offline / Standby (148, 163, 184)
	_ = os.WriteFile(paths.Gray, GenerateBridgeIcon(148, 163, 184), 0644)

	return paths, nil
}
