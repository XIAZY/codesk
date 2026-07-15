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

var appIconSizes = []int{16, 32, 64, 128, 256, 512, 1024}

const trayTemplatePixels = 32 // fyne/systray renders the image at 16 points.

type icnsChunk struct {
	tag  string
	size int
}

var icnsChunks = []icnsChunk{
	{tag: "icp4", size: 16},
	{tag: "icp5", size: 32},
	{tag: "icp6", size: 64},
	{tag: "ic07", size: 128},
	{tag: "ic08", size: 256},
	{tag: "ic09", size: 512},
	{tag: "ic10", size: 1024},
	{tag: "ic11", size: 32},
	{tag: "ic12", size: 64},
	{tag: "ic13", size: 256},
	{tag: "ic14", size: 512},
}

func main() {
	if len(os.Args) != 3 {
		fatalf("usage: go run ./scripts/generate-codesk-macos-assets.go <output.icns> <template.png>")
	}
	frames := make(map[int][]byte, len(appIconSizes))
	for _, size := range appIconSizes {
		data, err := renderPNG(size, sampleAppMark)
		if err != nil {
			fatalf("render %dpx application icon: %v", size, err)
		}
		frames[size] = data
	}
	if err := writeICNS(os.Args[1], frames); err != nil {
		fatalf("write application icon: %v", err)
	}
	template, err := renderPNG(trayTemplatePixels, sampleTemplateMark)
	if err != nil {
		fatalf("render tray template: %v", err)
	}
	if err := writeFile(os.Args[2], template); err != nil {
		fatalf("write tray template: %v", err)
	}
}

func renderPNG(size int, sample func(float64, float64) color.NRGBA) ([]byte, error) {
	const samples = 4
	imageOut := image.NewNRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			var red, green, blue, alpha uint32
			for sampleY := 0; sampleY < samples; sampleY++ {
				for sampleX := 0; sampleX < samples; sampleX++ {
					px := (float64(x) + (float64(sampleX)+0.5)/samples) * 512 / float64(size)
					py := (float64(y) + (float64(sampleY)+0.5)/samples) * 512 / float64(size)
					value := sample(px, py)
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

func sampleAppMark(x, y float64) color.NRGBA {
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

// Template images are alpha masks. AppKit supplies the correct foreground
// color for the current menu-bar appearance.
func sampleTemplateMark(x, y float64) color.NRGBA {
	if distance(x, y, 92, 256) <= 42 || distance(x, y, 420, 256) <= 42 ||
		distanceToSegment(x, y, 190, 190, 322, 322) <= 25 ||
		distanceToSegment(x, y, 322, 190, 190, 322) <= 25 {
		return color.NRGBA{A: 0xff}
	}
	return color.NRGBA{}
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

func writeICNS(path string, frames map[int][]byte) error {
	var body bytes.Buffer
	for _, chunk := range icnsChunks {
		data := frames[chunk.size]
		if len(data) == 0 {
			return fmt.Errorf("missing %dpx frame", chunk.size)
		}
		body.WriteString(chunk.tag)
		if err := binary.Write(&body, binary.BigEndian, uint32(8+len(data))); err != nil {
			return err
		}
		body.Write(data)
	}
	var output bytes.Buffer
	output.WriteString("icns")
	if err := binary.Write(&output, binary.BigEndian, uint32(8+body.Len())); err != nil {
		return err
	}
	output.Write(body.Bytes())
	return writeFile(path, output.Bytes())
}

func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
