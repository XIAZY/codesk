package main

import (
	"bytes"
	"crypto/sha256"
	"debug/macho"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const (
	releaseSchemaVersion = 1
	bundleIdentifier     = "com.getcodesk.desktop"
	minimumMacOSVersion  = "13.0"
	minimumMacOSPacked   = uint32(13 << 16)
	applicationName      = "Codesk.app"
	desktopExecutable    = "Codesk"
	agentToolExecutable  = "notty-agent-tool"
)

type releaseVersion struct {
	Artifact    string
	Bundle      string
	Development bool
}

func parseReleaseVersion(raw string, allowDevelopment bool) (releaseVersion, error) {
	if raw == "dev" {
		if !allowDevelopment {
			return releaseVersion{}, errors.New("codesk macOS release: dev is allowed only in explicit development mode")
		}
		return releaseVersion{Artifact: raw, Bundle: "0.0.0", Development: true}, nil
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return releaseVersion{}, errors.New("codesk macOS release: version must be three dot-separated integers")
	}
	for _, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return releaseVersion{}, errors.New("codesk macOS release: version components must be canonical non-negative integers")
		}
		for _, character := range part {
			if character < '0' || character > '9' {
				return releaseVersion{}, errors.New("codesk macOS release: version components must be canonical non-negative integers")
			}
		}
		value, err := strconv.ParseUint(part, 10, 31)
		if err != nil || value > 2147483647 {
			return releaseVersion{}, errors.New("codesk macOS release: version component is out of range")
		}
	}
	return releaseVersion{Artifact: raw, Bundle: raw}, nil
}

func renderInfoPlist(version releaseVersion) []byte {
	return []byte(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleDevelopmentRegion</key>
	<string>en</string>
	<key>CFBundleDisplayName</key>
	<string>Codesk</string>
	<key>CFBundleExecutable</key>
	<string>Codesk</string>
	<key>CFBundleIconFile</key>
	<string>Codesk.icns</string>
	<key>CFBundleIdentifier</key>
	<string>com.getcodesk.desktop</string>
	<key>CFBundleInfoDictionaryVersion</key>
	<string>6.0</string>
	<key>CFBundleName</key>
	<string>Codesk</string>
	<key>CFBundlePackageType</key>
	<string>APPL</string>
	<key>CFBundleShortVersionString</key>
	<string>%s</string>
	<key>CFBundleSupportedPlatforms</key>
	<array>
		<string>MacOSX</string>
	</array>
	<key>CFBundleVersion</key>
	<string>%s</string>
	<key>LSApplicationCategoryType</key>
	<string>public.app-category.developer-tools</string>
	<key>LSMinimumSystemVersion</key>
	<string>13.0</string>
	<key>LSMultipleInstancesProhibited</key>
	<true/>
	<key>LSUIElement</key>
	<true/>
	<key>NSHighResolutionCapable</key>
	<true/>
	<key>NSHumanReadableCopyright</key>
	<string>Copyright (c) 2026 Codesk</string>
</dict>
</plist>
`, version.Bundle, version.Bundle))
}

type appVerification struct {
	TreeSHA256 string
}

func verifyApp(root string, version releaseVersion) (appVerification, error) {
	root, err := cleanAbsolute(root)
	if err != nil {
		return appVerification{}, err
	}
	if filepath.Base(root) != applicationName {
		return appVerification{}, fmt.Errorf("codesk macOS release: app must be named %s", applicationName)
	}
	contents := filepath.Join(root, "Contents")
	macOSDir := filepath.Join(contents, "MacOS")
	helpersDir := filepath.Join(contents, "Helpers")
	resourcesDir := filepath.Join(contents, "Resources")
	requiredDirs := map[string]struct{}{
		root: {}, contents: {}, macOSDir: {}, helpersDir: {}, resourcesDir: {},
	}
	requiredFiles := map[string]struct{}{
		filepath.Join(contents, "Info.plist"):          {},
		filepath.Join(macOSDir, desktopExecutable):     {},
		filepath.Join(helpersDir, agentToolExecutable): {},
		filepath.Join(resourcesDir, "Codesk.icns"):     {},
	}
	optionalDirs := map[string]struct{}{
		filepath.Join(contents, "_CodeSignature"): {},
	}
	optionalFiles := map[string]struct{}{
		filepath.Join(contents, "_CodeSignature", "CodeResources"): {},
	}

	seenDirs := make(map[string]struct{})
	seenFiles := make(map[string]struct{})
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("codesk macOS release: app contains symlink %q", path)
		}
		if entry.IsDir() {
			if _, required := requiredDirs[path]; !required {
				if _, optional := optionalDirs[path]; !optional {
					return fmt.Errorf("codesk macOS release: unexpected app directory %q", path)
				}
			}
			if info.Mode().Perm()&0o022 != 0 {
				return fmt.Errorf("codesk macOS release: app directory %q is group/other writable", path)
			}
			seenDirs[path] = struct{}{}
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("codesk macOS release: non-regular app entry %q", path)
		}
		if _, required := requiredFiles[path]; !required {
			if _, optional := optionalFiles[path]; !optional {
				return fmt.Errorf("codesk macOS release: unexpected app file %q", path)
			}
		}
		if info.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("codesk macOS release: app file %q is group/other writable", path)
		}
		seenFiles[path] = struct{}{}
		return nil
	})
	if err != nil {
		return appVerification{}, err
	}
	for path := range requiredDirs {
		if _, ok := seenDirs[path]; !ok {
			return appVerification{}, fmt.Errorf("codesk macOS release: missing app directory %q", path)
		}
	}
	for path := range requiredFiles {
		if _, ok := seenFiles[path]; !ok {
			return appVerification{}, fmt.Errorf("codesk macOS release: missing app file %q", path)
		}
	}

	plistPath := filepath.Join(contents, "Info.plist")
	plist, err := os.ReadFile(plistPath)
	if err != nil {
		return appVerification{}, err
	}
	if !bytes.Equal(plist, renderInfoPlist(version)) {
		return appVerification{}, errors.New("codesk macOS release: Info.plist does not match the canonical metadata")
	}
	if err := verifyICNS(filepath.Join(resourcesDir, "Codesk.icns")); err != nil {
		return appVerification{}, err
	}
	for _, executable := range []string{filepath.Join(macOSDir, desktopExecutable), filepath.Join(helpersDir, agentToolExecutable)} {
		info, err := os.Stat(executable)
		if err != nil {
			return appVerification{}, err
		}
		if info.Mode().Perm()&0o111 == 0 {
			return appVerification{}, fmt.Errorf("codesk macOS release: %q is not executable", executable)
		}
		if err := verifyUniversalMachO(executable); err != nil {
			return appVerification{}, err
		}
	}
	treeHash, err := appTreeHash(root)
	if err != nil {
		return appVerification{}, err
	}
	return appVerification{TreeSHA256: treeHash}, nil
}

var expectedICNSChunks = map[string]int{
	"icp4": 16, "icp5": 32, "icp6": 64, "ic07": 128, "ic08": 256, "ic09": 512, "ic10": 1024,
	"ic11": 32, "ic12": 64, "ic13": 256, "ic14": 512,
}

func verifyICNS(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("codesk macOS release: read icon: %w", err)
	}
	if len(data) < 8 || string(data[:4]) != "icns" || int(binary.BigEndian.Uint32(data[4:8])) != len(data) {
		return errors.New("codesk macOS release: invalid ICNS header")
	}
	seen := make(map[string]struct{}, len(expectedICNSChunks))
	for offset := 8; offset < len(data); {
		if offset+8 > len(data) {
			return errors.New("codesk macOS release: truncated ICNS chunk")
		}
		tag := string(data[offset : offset+4])
		length := int(binary.BigEndian.Uint32(data[offset+4 : offset+8]))
		if length < 8 || offset+length > len(data) {
			return errors.New("codesk macOS release: invalid ICNS chunk length")
		}
		expectedSize, ok := expectedICNSChunks[tag]
		if !ok {
			return fmt.Errorf("codesk macOS release: unexpected ICNS chunk %q", tag)
		}
		if _, duplicate := seen[tag]; duplicate {
			return fmt.Errorf("codesk macOS release: duplicate ICNS chunk %q", tag)
		}
		payload := bytes.NewReader(data[offset+8 : offset+length])
		decoded, err := png.Decode(payload)
		if err != nil || decoded.Bounds().Dx() != expectedSize || decoded.Bounds().Dy() != expectedSize {
			return fmt.Errorf("codesk macOS release: ICNS chunk %q is not a %dx%d PNG", tag, expectedSize, expectedSize)
		}
		if payload.Len() != 0 {
			return fmt.Errorf("codesk macOS release: ICNS chunk %q has trailing PNG payload", tag)
		}
		seen[tag] = struct{}{}
		offset += length
	}
	if len(seen) != len(expectedICNSChunks) {
		return errors.New("codesk macOS release: ICNS icon is missing required representations")
	}
	return nil
}

func verifyUniversalMachO(path string) error {
	fat, err := macho.OpenFat(path)
	if err != nil {
		return fmt.Errorf("codesk macOS release: %q is not a universal Mach-O: %w", path, err)
	}
	defer fat.Close()
	if len(fat.Arches) != 2 {
		return fmt.Errorf("codesk macOS release: %q has %d Mach-O slices, want 2", path, len(fat.Arches))
	}
	seen := make(map[macho.Cpu]struct{}, 2)
	for _, arch := range fat.Arches {
		declaredCPU := arch.FatArchHeader.Cpu
		declaredSubCPU := arch.FatArchHeader.SubCpu
		if declaredCPU != arch.File.Cpu || declaredSubCPU != arch.File.SubCpu {
			return fmt.Errorf(
				"codesk macOS release: %q fat architecture %s/%d does not match its Mach-O slice %s/%d",
				path, declaredCPU, declaredSubCPU, arch.File.Cpu, arch.File.SubCpu,
			)
		}
		if arch.File.Type != macho.TypeExec {
			return fmt.Errorf("codesk macOS release: %q contains a non-executable Mach-O slice", path)
		}
		if arch.Cpu != macho.CpuAmd64 && arch.Cpu != macho.CpuArm64 {
			return fmt.Errorf("codesk macOS release: %q contains unsupported CPU %s", path, arch.Cpu)
		}
		expectedSubCPU := uint32(0)
		if arch.Cpu == macho.CpuAmd64 {
			expectedSubCPU = 3
		}
		if arch.SubCpu != expectedSubCPU {
			return fmt.Errorf("codesk macOS release: %q CPU %s has unsupported subtype %d", path, arch.Cpu, arch.SubCpu)
		}
		if _, duplicate := seen[arch.Cpu]; duplicate {
			return fmt.Errorf("codesk macOS release: %q contains duplicate CPU %s", path, arch.Cpu)
		}
		if err := verifyMachOMinimumVersion(arch.File); err != nil {
			return fmt.Errorf("codesk macOS release: %q CPU %s: %w", path, arch.Cpu, err)
		}
		seen[arch.Cpu] = struct{}{}
	}
	if _, ok := seen[macho.CpuAmd64]; !ok {
		return fmt.Errorf("codesk macOS release: %q is missing Intel x86_64", path)
	}
	if _, ok := seen[macho.CpuArm64]; !ok {
		return fmt.Errorf("codesk macOS release: %q is missing Apple Silicon arm64", path)
	}
	return nil
}

func verifyMachOMinimumVersion(file *macho.File) error {
	const (
		loadCommandVersionMinMacOSX = uint32(0x24)
		loadCommandBuildVersion     = uint32(0x32)
		platformMacOS               = uint32(1)
	)
	found := false
	for _, load := range file.Loads {
		raw := load.Raw()
		if len(raw) < 8 {
			return errors.New("malformed Mach-O load command")
		}
		command := file.ByteOrder.Uint32(raw[:4])
		var minimum uint32
		switch command {
		case loadCommandVersionMinMacOSX:
			if len(raw) < 16 {
				return errors.New("malformed LC_VERSION_MIN_MACOSX command")
			}
			minimum = file.ByteOrder.Uint32(raw[8:12])
		case loadCommandBuildVersion:
			if len(raw) < 24 {
				return errors.New("malformed LC_BUILD_VERSION command")
			}
			if platform := file.ByteOrder.Uint32(raw[8:12]); platform != platformMacOS {
				return fmt.Errorf("LC_BUILD_VERSION platform is %d, want macOS", platform)
			}
			minimum = file.ByteOrder.Uint32(raw[12:16])
		default:
			continue
		}
		if found {
			return errors.New("duplicate macOS minimum-version load command")
		}
		found = true
		if minimum != minimumMacOSPacked {
			return fmt.Errorf("Mach-O deployment target is %s, want %s", formatMachOVersion(minimum), minimumMacOSVersion)
		}
	}
	if !found {
		return errors.New("missing macOS minimum-version load command")
	}
	return nil
}

func formatMachOVersion(version uint32) string {
	return fmt.Sprintf("%d.%d.%d", version>>16, version>>8&0xff, version&0xff)
}

type releaseManifest struct {
	Schema               int              `json:"schema"`
	Product              string           `json:"product"`
	Version              string           `json:"version"`
	BundleIdentifier     string           `json:"bundle_identifier"`
	MinimumSystemVersion string           `json:"minimum_system_version"`
	SourceRevision       string           `json:"source_revision"`
	SignedAndNotarized   bool             `json:"signed_and_notarized"`
	Application          applicationEntry `json:"application"`
	DiskImage            artifactEntry    `json:"disk_image"`
}

type applicationEntry struct {
	Path       string `json:"path"`
	TreeSHA256 string `json:"tree_sha256"`
}

type artifactEntry struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

func diskImageName(version releaseVersion) string {
	return "Codesk_" + version.Artifact + "_macos_universal.dmg"
}

func writeManifest(root string, version releaseVersion, sourceRevision string, signed bool) error {
	root, err := cleanAbsolute(root)
	if err != nil {
		return err
	}
	if err := validateSourceRevision(sourceRevision); err != nil {
		return err
	}
	app, err := verifyApp(filepath.Join(root, applicationName), version)
	if err != nil {
		return err
	}
	dmgPath := filepath.Join(root, diskImageName(version))
	dmg, err := inspectArtifact(dmgPath)
	if err != nil {
		return err
	}
	manifest := releaseManifest{
		Schema:               releaseSchemaVersion,
		Product:              "Codesk Desktop for macOS",
		Version:              version.Artifact,
		BundleIdentifier:     bundleIdentifier,
		MinimumSystemVersion: minimumMacOSVersion,
		SourceRevision:       sourceRevision,
		SignedAndNotarized:   signed,
		Application:          applicationEntry{Path: applicationName, TreeSHA256: app.TreeSHA256},
		DiskImage:            dmg,
	}
	encoded, err := encodeManifest(manifest)
	if err != nil {
		return err
	}
	manifestPath := filepath.Join(root, "manifest.json")
	if err := writeAtomic(manifestPath, encoded, 0o644); err != nil {
		return err
	}
	manifestHash := sha256.Sum256(encoded)
	checksums := fmt.Sprintf("%s  %s\n%s  manifest.json\n", dmg.SHA256, dmg.Path, hex.EncodeToString(manifestHash[:]))
	return writeAtomic(filepath.Join(root, "SHA256SUMS"), []byte(checksums), 0o644)
}

func verifyRelease(root string, version releaseVersion, allowUnsigned bool) error {
	root, err := cleanAbsolute(root)
	if err != nil {
		return err
	}
	if err := verifyReleaseRootEntries(root, version); err != nil {
		return err
	}
	manifestBytes, err := os.ReadFile(filepath.Join(root, "manifest.json"))
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(manifestBytes))
	decoder.DisallowUnknownFields()
	var manifest releaseManifest
	if err := decoder.Decode(&manifest); err != nil {
		return fmt.Errorf("codesk macOS release: decode manifest: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return err
	}
	canonicalManifest, err := encodeManifest(manifest)
	if err != nil {
		return err
	}
	if !bytes.Equal(manifestBytes, canonicalManifest) {
		return errors.New("codesk macOS release: manifest JSON is not canonical")
	}
	if manifest.Schema != releaseSchemaVersion || manifest.Product != "Codesk Desktop for macOS" ||
		manifest.Version != version.Artifact || manifest.BundleIdentifier != bundleIdentifier ||
		manifest.MinimumSystemVersion != minimumMacOSVersion || manifest.Application.Path != applicationName ||
		manifest.DiskImage.Path != diskImageName(version) {
		return errors.New("codesk macOS release: manifest identity does not match the requested release")
	}
	if err := validateSourceRevision(manifest.SourceRevision); err != nil {
		return err
	}
	if !manifest.SignedAndNotarized && !allowUnsigned {
		return errors.New("codesk macOS release: artifact is unsigned; use --allow-unsigned only for construction evidence")
	}
	if manifest.SignedAndNotarized && version.Development {
		return errors.New("codesk macOS release: development artifact is incorrectly marked signed")
	}
	app, err := verifyApp(filepath.Join(root, applicationName), version)
	if err != nil {
		return err
	}
	if app.TreeSHA256 != manifest.Application.TreeSHA256 {
		return errors.New("codesk macOS release: application tree hash mismatch")
	}
	dmg, err := inspectArtifact(filepath.Join(root, manifest.DiskImage.Path))
	if err != nil {
		return err
	}
	if dmg != manifest.DiskImage {
		return errors.New("codesk macOS release: disk image hash or size mismatch")
	}
	manifestHash := sha256.Sum256(manifestBytes)
	wantChecksums := fmt.Sprintf("%s  %s\n%s  manifest.json\n", dmg.SHA256, dmg.Path, hex.EncodeToString(manifestHash[:]))
	checksums, err := os.ReadFile(filepath.Join(root, "SHA256SUMS"))
	if err != nil {
		return err
	}
	if string(checksums) != wantChecksums {
		return errors.New("codesk macOS release: SHA256SUMS is not canonical")
	}
	return nil
}

func verifyReleaseRootEntries(root string, version releaseVersion) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	want := map[string]bool{
		applicationName:        false,
		diskImageName(version): false,
		"manifest.json":        false,
		"SHA256SUMS":           false,
	}
	for _, entry := range entries {
		if _, ok := want[entry.Name()]; !ok {
			return fmt.Errorf("codesk macOS release: unexpected release entry %q", entry.Name())
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("codesk macOS release: release entry %q is a symlink", entry.Name())
		}
		if info.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("codesk macOS release: release entry %q is group/other writable", entry.Name())
		}
		want[entry.Name()] = true
	}
	for name, present := range want {
		if !present {
			return fmt.Errorf("codesk macOS release: missing release entry %q", name)
		}
	}
	return nil
}

func verifyMountedVolume(root string) error {
	root, err := cleanAbsolute(root)
	if err != nil {
		return err
	}
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return fmt.Errorf("codesk macOS release: inspect mounted volume: %w", err)
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("codesk macOS release: mounted volume root is not a real directory")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("codesk macOS release: read mounted volume: %w", err)
	}
	want := map[string]bool{
		applicationName: false,
		"Applications":  false,
	}
	for _, entry := range entries {
		if _, ok := want[entry.Name()]; !ok {
			return fmt.Errorf("codesk macOS release: unexpected mounted volume entry %q", entry.Name())
		}
		path := filepath.Join(root, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		switch entry.Name() {
		case applicationName:
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("codesk macOS release: mounted %s is not a real directory", applicationName)
			}
		case "Applications":
			if info.Mode()&os.ModeSymlink == 0 {
				return errors.New("codesk macOS release: mounted Applications entry is not a symlink")
			}
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if target != "/Applications" {
				return fmt.Errorf("codesk macOS release: mounted Applications symlink targets %q", target)
			}
		}
		want[entry.Name()] = true
	}
	for name, present := range want {
		if !present {
			return fmt.Errorf("codesk macOS release: missing mounted volume entry %q", name)
		}
	}
	return nil
}

func inspectArtifact(path string) (artifactEntry, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return artifactEntry{}, fmt.Errorf("codesk macOS release: inspect artifact: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() == 0 {
		return artifactEntry{}, fmt.Errorf("codesk macOS release: artifact %q is not a non-empty regular file", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return artifactEntry{}, err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return artifactEntry{}, err
	}
	return artifactEntry{Path: filepath.Base(path), SHA256: hex.EncodeToString(hash.Sum(nil)), Size: info.Size()}, nil
}

func appTreeHash(root string) (string, error) {
	var entries []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		entries = append(entries, path)
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(entries)
	tree := sha256.New()
	for _, path := range entries {
		info, err := os.Lstat(path)
		if err != nil {
			return "", err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return "", err
		}
		relative = filepath.ToSlash(relative)
		if info.IsDir() {
			fmt.Fprintf(tree, "d %04o %s\n", info.Mode().Perm(), relative)
			continue
		}
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("codesk macOS release: cannot hash non-regular app entry %q", path)
		}
		file, err := os.Open(path)
		if err != nil {
			return "", err
		}
		content := sha256.New()
		_, copyErr := io.Copy(content, file)
		closeErr := file.Close()
		if copyErr != nil {
			return "", copyErr
		}
		if closeErr != nil {
			return "", closeErr
		}
		fmt.Fprintf(tree, "f %04o %d %s %s\n", info.Mode().Perm(), info.Size(), hex.EncodeToString(content.Sum(nil)), relative)
	}
	return hex.EncodeToString(tree.Sum(nil)), nil
}

func validateSourceRevision(revision string) error {
	if len(revision) != 40 || strings.ToLower(revision) != revision || strings.Trim(revision, "0") == "" {
		return errors.New("codesk macOS release: source revision must be a full lowercase Git SHA")
	}
	decoded, err := hex.DecodeString(revision)
	if err != nil || len(decoded) != 20 {
		return errors.New("codesk macOS release: source revision must be a full lowercase Git SHA")
	}
	return nil
}

func encodeManifest(manifest releaseManifest) ([]byte, error) {
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}

func cleanAbsolute(path string) (string, error) {
	if path == "" || path != strings.TrimSpace(path) || strings.ContainsRune(path, '\x00') || !filepath.IsAbs(path) || path != filepath.Clean(path) {
		return "", errors.New("codesk macOS release: path must be absolute and clean")
	}
	return path, nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("codesk macOS release: manifest contains multiple JSON values")
		}
		return fmt.Errorf("codesk macOS release: decode manifest trailer: %w", err)
	}
	return nil
}

func writeAtomic(path string, data []byte, mode os.FileMode) (returnErr error) {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".codesk-release-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	closed := false
	defer func() {
		if !closed {
			if closeErr := temporary.Close(); returnErr == nil && closeErr != nil {
				returnErr = closeErr
			}
		}
		if removeErr := os.Remove(temporaryPath); returnErr == nil && removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			returnErr = removeErr
		}
	}()
	if err := temporary.Chmod(mode); err != nil {
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	closed = true
	return os.Rename(temporaryPath, path)
}
