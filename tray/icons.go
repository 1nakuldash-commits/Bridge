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

// GenerateDotIcon creates a 16x16 32-bit RGBA smooth circular dot ICO file.
func GenerateDotIcon(r, g, b byte) []byte {
	const size = 16
	const radius = 6.0
	const center = 7.5

	buf := new(bytes.Buffer)
	binary.Write(buf, binary.LittleEndian, iconDir{0, 1, 1})

	imageSize := uint32(40 + (size * size * 4) + (size * 4))
	binary.Write(buf, binary.LittleEndian, iconDirEntry{
		Width:       size,
		Height:      size,
		ColorCount:  0,
		Reserved:    0,
		Planes:      1,
		BitCount:    32,
		BytesInRes:  imageSize,
		ImageOffset: 22,
	})

	bih := bitmapInfoHeader{
		Size:        40,
		Width:       size,
		Height:      size * 2,
		Planes:      1,
		BitCount:    32,
		Compression: 0,
		SizeImage:   imageSize - 40,
	}
	binary.Write(buf, binary.LittleEndian, bih)

	// Bottom-up BGRA pixel array
	for y := size - 1; y >= 0; y-- {
		for x := 0; x < size; x++ {
			dx := float64(x) - center
			dy := float64(y) - center
			dist := math.Sqrt(dx*dx + dy*dy)

			var alpha byte
			if dist <= radius-0.5 {
				alpha = 255
			} else if dist < radius+0.5 {
				alpha = byte(255 * (radius + 0.5 - dist))
			} else {
				alpha = 0
			}

			if alpha > 0 {
				buf.WriteByte(b)
				buf.WriteByte(g)
				buf.WriteByte(r)
				buf.WriteByte(alpha)
			} else {
				buf.Write([]byte{0, 0, 0, 0})
			}
		}
	}

	// 1-bit AND mask (all zeros for 32-bit RGBA)
	andRow := make([]byte, 4)
	for y := 0; y < size; y++ {
		buf.Write(andRow)
	}

	return buf.Bytes()
}

// IconPaths holds file paths to the generated state icons.
type IconPaths struct {
	Green  string
	Yellow string
	Blue   string
	Gray   string
}

// EnsureIcons generates the state dot icons in the designated directory.
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

	// 🟢 Green: Ready / Connected (34, 197, 94)
	_ = os.WriteFile(paths.Green, GenerateDotIcon(34, 197, 94), 0644)

	// 🟡 Yellow: Active / Printing / Syncing (234, 179, 8)
	_ = os.WriteFile(paths.Yellow, GenerateDotIcon(234, 179, 8), 0644)

	// 🔵 Blue: Print Completed (59, 130, 246)
	_ = os.WriteFile(paths.Blue, GenerateDotIcon(59, 130, 246), 0644)

	// ⚪ Gray: Offline / Waiting (156, 163, 175)
	_ = os.WriteFile(paths.Gray, GenerateDotIcon(156, 163, 175), 0644)

	return paths, nil
}
