package main

import (
	"bytes"
	"image"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestCodeskMacOSAssetsAreReproducibleAndRetinaReady(t *testing.T) {
	tempDir := t.TempDir()
	generatedIcon := filepath.Join(tempDir, "Codesk.icns")
	generatedTemplate := filepath.Join(tempDir, "codesk-tray-template.png")
	command := exec.Command(
		"go", "run", "-buildvcs=false", "./generate-codesk-macos-assets.go",
		generatedIcon, generatedTemplate,
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("generate macOS assets: %v\n%s", err, output)
	}

	assertFileBytesEqual(t, generatedIcon, filepath.Join("..", "daemon", "cmd", "codesk-desktop", "assets", "Codesk.icns"))
	assertFileBytesEqual(t, generatedTemplate, filepath.Join("..", "daemon", "cmd", "codesk-desktop", "assets", "codesk-tray-template.png"))
	assertRetinaTemplateMask(t, generatedTemplate)
}

func assertFileBytesEqual(t *testing.T, generatedPath, committedPath string) {
	t.Helper()
	generated, err := os.ReadFile(generatedPath)
	if err != nil {
		t.Fatalf("read generated asset %q: %v", generatedPath, err)
	}
	committed, err := os.ReadFile(committedPath)
	if err != nil {
		t.Fatalf("read committed asset %q: %v", committedPath, err)
	}
	if !bytes.Equal(generated, committed) {
		t.Fatalf("committed asset %q is not reproducible", committedPath)
	}
}

func assertRetinaTemplateMask(t *testing.T, path string) {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open tray template: %v", err)
	}
	defer file.Close()

	decoded, err := png.Decode(file)
	if err != nil {
		t.Fatalf("decode tray template: %v", err)
	}
	if bounds := decoded.Bounds(); bounds != image.Rect(0, 0, 32, 32) {
		t.Fatalf("tray template bounds = %v, want 32x32 (16pt @2x)", bounds)
	}

	hasTransparent := false
	hasOpaque := false
	hasAntialiasedEdge := false
	for y := 0; y < 32; y++ {
		for x := 0; x < 32; x++ {
			red, green, blue, alpha := decoded.At(x, y).RGBA()
			if red != 0 || green != 0 || blue != 0 {
				t.Fatalf("tray template pixel (%d,%d) is colored: rgba16=(%d,%d,%d,%d)", x, y, red, green, blue, alpha)
			}
			switch alpha {
			case 0:
				hasTransparent = true
			case 0xffff:
				hasOpaque = true
			default:
				hasAntialiasedEdge = true
			}
		}
	}
	if !hasTransparent || !hasOpaque || !hasAntialiasedEdge {
		t.Fatalf("tray template alpha coverage = transparent:%t opaque:%t antialiased:%t", hasTransparent, hasOpaque, hasAntialiasedEdge)
	}
}
