package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"image"
	_ "image/png"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var bundledHomepageTemplate = regexp.MustCompile(`(?s)<script type="__bundler/template"[^>]*>(.*?)</script>`)

func TestBrowserIconShellsStaySafariCompatibleAndSynchronized(t *testing.T) {
	frontendIndex := readBrowserShellFile(t, "frontend", "index.html")
	homepageIndex := readBrowserShellFile(t, "homepage", "index.html")
	homepageOuter, homepageInner := decodeBundledHomepage(t, homepageIndex)

	assertBrowserIconMetadata(t, "Vite app", frontendIndex)
	assertBrowserIconMetadata(t, "homepage outer shell", homepageOuter)
	assertBrowserIconMetadata(t, "homepage embedded page", homepageInner)

	assets := []string{
		"favicon.svg",
		"app-icon.svg",
		"favicon-16x16.png",
		"favicon-32x32.png",
		"favicon.ico",
		"apple-touch-icon.png",
		"safari-pinned-tab.svg",
	}
	for _, asset := range assets {
		frontendAsset := readBrowserShellFile(t, "frontend", "public", asset)
		homepageAsset := readBrowserShellFile(t, "homepage", asset)
		if !bytes.Equal(frontendAsset, homepageAsset) {
			t.Errorf("%s differs between frontend/public and homepage", asset)
		}
	}

	for asset, want := range map[string]image.Point{
		"favicon-16x16.png":    {X: 16, Y: 16},
		"favicon-32x32.png":    {X: 32, Y: 32},
		"apple-touch-icon.png": {X: 180, Y: 180},
	} {
		assertPNGDimensions(t, filepath.Join("..", "frontend", "public", asset), want)
	}
	assertICOFrames(t, filepath.Join("..", "frontend", "public", "favicon.ico"), []int{16, 32, 48})

	mask := string(readBrowserShellFile(t, "frontend", "public", "safari-pinned-tab.svg"))
	if !strings.Contains(mask, `viewBox="0 0 16 16"`) {
		t.Error("Safari pinned-tab icon does not use Safari's required 16x16 viewBox")
	}
	if strings.Contains(mask, "#E3A15B") || strings.Contains(mask, "#7FC1D6") || strings.Count(mask, "#000000") != 4 {
		t.Error("Safari pinned-tab icon is not a single-color mask of the OXO geometry")
	}
}

func TestHomepageStartFreeLinksOpenRegistration(t *testing.T) {
	homepageIndex := readBrowserShellFile(t, "homepage", "index.html")
	_, homepageInner := decodeBundledHomepage(t, homepageIndex)
	anchorPattern := regexp.MustCompile(`(?s)<a\b([^>]*)>(.*?)</a>`)
	startFreeCount := 0
	for _, anchor := range anchorPattern.FindAllStringSubmatch(string(homepageInner), -1) {
		if !strings.Contains(anchor[2], "Start free") {
			continue
		}
		startFreeCount++
		if !strings.Contains(anchor[1], `href="https://app.getcodesk.com/register"`) {
			t.Errorf("Start free link has non-registration attributes: %s", anchor[1])
		}
	}
	if startFreeCount != 3 {
		t.Errorf("homepage Start free link count = %d, want 3", startFreeCount)
	}
}

func readBrowserShellFile(t *testing.T, pathParts ...string) []byte {
	t.Helper()
	path := filepath.Join(append([]string{".."}, pathParts...)...)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

func decodeBundledHomepage(t *testing.T, source []byte) ([]byte, []byte) {
	t.Helper()
	matchIndexes := bundledHomepageTemplate.FindSubmatchIndex(source)
	if len(matchIndexes) != 4 {
		t.Fatal("homepage bundled template is missing or ambiguous")
	}
	var inner string
	if err := json.Unmarshal(source[matchIndexes[2]:matchIndexes[3]], &inner); err != nil {
		t.Fatalf("decode homepage bundled template: %v", err)
	}
	return source[:matchIndexes[0]], []byte(inner)
}

func assertBrowserIconMetadata(t *testing.T, name string, document []byte) {
	t.Helper()
	wants := []string{
		`rel="icon" type="image/svg+xml" sizes="any" href="/favicon.svg?v=2"`,
		`rel="icon" type="image/png" sizes="32x32" href="/favicon-32x32.png"`,
		`rel="icon" type="image/png" sizes="16x16" href="/favicon-16x16.png"`,
		`rel="shortcut icon" href="/favicon.ico"`,
		`rel="apple-touch-icon" sizes="180x180" href="/apple-touch-icon.png"`,
		`rel="mask-icon" href="/safari-pinned-tab.svg" color="#1B1A17"`,
	}
	for _, want := range wants {
		if count := bytes.Count(document, []byte(want)); count != 1 {
			t.Errorf("%s metadata count for %q = %d, want 1", name, want, count)
		}
	}
}

func assertPNGDimensions(t *testing.T, path string, want image.Point) {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer file.Close()
	config, format, err := image.DecodeConfig(file)
	if err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	if format != "png" || config.Width != want.X || config.Height != want.Y {
		t.Errorf("%s = %s %dx%d, want PNG %dx%d", path, format, config.Width, config.Height, want.X, want.Y)
	}
}

func assertICOFrames(t *testing.T, path string, want []int) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if len(data) < 6 || binary.LittleEndian.Uint16(data[0:2]) != 0 || binary.LittleEndian.Uint16(data[2:4]) != 1 {
		t.Fatalf("%s does not have an ICO header", path)
	}
	frameCount := int(binary.LittleEndian.Uint16(data[4:6]))
	if frameCount != len(want) || len(data) < 6+16*frameCount {
		t.Fatalf("%s frame count = %d, want %d", path, frameCount, len(want))
	}
	got := make(map[int]bool, frameCount)
	for index := 0; index < frameCount; index++ {
		entry := data[6+16*index : 6+16*(index+1)]
		width, height := int(entry[0]), int(entry[1])
		if width == 0 {
			width = 256
		}
		if height == 0 {
			height = 256
		}
		if width != height {
			t.Errorf("%s frame %d is %dx%d", path, index, width, height)
		}
		got[width] = true
	}
	for _, size := range want {
		if !got[size] {
			t.Errorf("%s is missing a %dx%d frame", path, size, size)
		}
	}
}
