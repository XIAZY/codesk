//go:build ignore

package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
)

var iconSizes = []int{16, 20, 24, 32, 40, 48, 64, 128, 256}

type iconFrame struct {
	size int
	png  []byte
}

func main() {
	if len(os.Args) != 2 {
		fatalf("usage: go run ./scripts/generate-codesk-icon.go <output.ico>")
	}
	frames := make([]iconFrame, 0, len(iconSizes))
	for _, size := range iconSizes {
		data, err := renderPNG(size)
		if err != nil {
			fatalf("render %dpx frame: %v", size, err)
		}
		frames = append(frames, iconFrame{size: size, png: data})
	}
	if err := writeICO(os.Args[1], frames); err != nil {
		fatalf("write icon: %v", err)
	}
}

func renderPNG(size int) ([]byte, error) {
	const samples = 4
	imageOut := image.NewNRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			var red, green, blue, alpha uint32
			for sampleY := 0; sampleY < samples; sampleY++ {
				for sampleX := 0; sampleX < samples; sampleX++ {
					px := (float64(x) + (float64(sampleX)+0.5)/samples) * 512 / float64(size)
					py := (float64(y) + (float64(sampleY)+0.5)/samples) * 512 / float64(size)
					value := sampleMark(px, py)
					red += uint32(value.R)
					green += uint32(value.G)
					blue += uint32(value.B)
					alpha += uint32(value.A)
				}
			}
			count := uint32(samples * samples)
			imageOut.SetNRGBA(x, y, color.NRGBA{
				R: uint8(red / count), G: uint8(green / count), B: uint8(blue / count), A: uint8(alpha / count),
			})
		}
	}
	var output bytes.Buffer
	encoder := png.Encoder{CompressionLevel: png.BestCompression}
	if err := encoder.Encode(&output, imageOut); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func sampleMark(x, y float64) color.NRGBA {
	value := color.NRGBA{R: 0x1b, G: 0x1a, B: 0x17, A: 0xff}
	if !insideRoundedSquare(x, y, 115) {
		value = color.NRGBA{}
	}
	if distance(x, y, 122.9, 256) <= 46.1 {
		value = color.NRGBA{R: 0xe3, G: 0xa1, B: 0x5b, A: 0xff}
	}
	if distance(x, y, 389.1, 256) <= 46.1 {
		value = color.NRGBA{R: 0x7f, G: 0xc1, B: 0xd6, A: 0xff}
	}
	if distanceToSegment(x, y, 202.8, 202.8, 309.2, 309.2) <= 20 ||
		distanceToSegment(x, y, 309.2, 202.8, 202.8, 309.2) <= 20 {
		value = color.NRGBA{R: 0xfc, G: 0xfb, B: 0xf7, A: 0xff}
	}
	return value
}

func insideRoundedSquare(x, y, radius float64) bool {
	if x < 0 || y < 0 || x > 512 || y > 512 {
		return false
	}
	nearestX := math.Max(radius, math.Min(512-radius, x))
	nearestY := math.Max(radius, math.Min(512-radius, y))
	return distance(x, y, nearestX, nearestY) <= radius
}

func distance(x1, y1, x2, y2 float64) float64 {
	return math.Hypot(x1-x2, y1-y2)
}

func distanceToSegment(px, py, x1, y1, x2, y2 float64) float64 {
	dx := x2 - x1
	dy := y2 - y1
	lengthSquared := dx*dx + dy*dy
	if lengthSquared == 0 {
		return distance(px, py, x1, y1)
	}
	projection := ((px-x1)*dx + (py-y1)*dy) / lengthSquared
	projection = math.Max(0, math.Min(1, projection))
	return distance(px, py, x1+projection*dx, y1+projection*dy)
}

func writeICO(path string, frames []iconFrame) error {
	var output bytes.Buffer
	_ = binary.Write(&output, binary.LittleEndian, uint16(0))
	_ = binary.Write(&output, binary.LittleEndian, uint16(1))
	_ = binary.Write(&output, binary.LittleEndian, uint16(len(frames)))
	offset := uint32(6 + 16*len(frames))
	for _, frame := range frames {
		width := byte(frame.size)
		if frame.size == 256 {
			width = 0
		}
		output.WriteByte(width)
		output.WriteByte(width)
		output.WriteByte(0)
		output.WriteByte(0)
		_ = binary.Write(&output, binary.LittleEndian, uint16(1))
		_ = binary.Write(&output, binary.LittleEndian, uint16(32))
		_ = binary.Write(&output, binary.LittleEndian, uint32(len(frame.png)))
		_ = binary.Write(&output, binary.LittleEndian, offset)
		offset += uint32(len(frame.png))
	}
	for _, frame := range frames {
		output.Write(frame.png)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, output.Bytes(), 0o644)
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
