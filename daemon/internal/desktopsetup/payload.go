package desktopsetup

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	payloadFooterSize      = 16 + 8 + sha256.Size
	maximumPayloadBytes    = 512 << 20
	maximumPayloadFileSize = 256 << 20
	maximumManifestBytes   = 64 << 10
	maximumVersionBytes    = 128
)

var (
	payloadFooterMagic = [16]byte{'C', 'O', 'D', 'E', 'S', 'K', '-', 'P', 'A', 'Y', 'L', 'O', 'A', 'D', 1, 0}
	payloadFileNames   = []string{"Codesk.exe", "notty-agent-tool.exe", "codesk.ico"}
	payloadEntryNames  = []string{"Codesk.exe", "notty-agent-tool.exe", "codesk.ico", "payload.json"}
)

type PayloadFile struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type PayloadManifest struct {
	Version string        `json:"version"`
	Arch    string        `json:"arch"`
	Files   []PayloadFile `json:"files"`
}

type Payload struct {
	data []byte

	mu       sync.RWMutex
	verified bool
	manifest PayloadManifest
	files    map[string][]byte
}

// OpenPayload finds and verifies the fixed footer, including when an
// Authenticode certificate table was appended after the footer by a signer.
func OpenPayload(executablePath string) (*Payload, error) {
	file, err := os.Open(executablePath)
	if err != nil {
		return nil, fmt.Errorf("desktop setup: open executable: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("desktop setup: stat executable: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() < payloadFooterSize {
		return nil, errors.New("desktop setup: executable has no payload footer")
	}

	footerOffset, footer, err := locatePayloadFooter(file, info.Size())
	if err != nil {
		return nil, err
	}
	payloadLength := binary.LittleEndian.Uint64(footer[16:24])
	if payloadLength == 0 || payloadLength > maximumPayloadBytes || payloadLength > uint64(footerOffset) {
		return nil, errors.New("desktop setup: invalid payload length")
	}
	payloadOffset := footerOffset - int64(payloadLength)
	data := make([]byte, int(payloadLength))
	if _, err := file.ReadAt(data, payloadOffset); err != nil {
		return nil, fmt.Errorf("desktop setup: read payload: %w", err)
	}
	digest := sha256.Sum256(data)
	if !bytes.Equal(digest[:], footer[24:]) {
		return nil, errors.New("desktop setup: payload hash mismatch")
	}
	return &Payload{data: data}, nil
}

func locatePayloadFooter(file *os.File, size int64) (int64, []byte, error) {
	footerOffset := size - payloadFooterSize
	footer, err := readAt(file, footerOffset, payloadFooterSize)
	if err == nil && bytes.Equal(footer[:16], payloadFooterMagic[:]) {
		return footerOffset, footer, nil
	}

	certificateOffset, certificateSize, err := peCertificateTable(file, size)
	if err != nil {
		return 0, nil, errors.New("desktop setup: payload footer not found")
	}
	if certificateOffset <= payloadFooterSize || certificateSize == 0 ||
		certificateOffset+certificateSize != size {
		return 0, nil, errors.New("desktop setup: invalid certificate table")
	}
	for padding := int64(0); padding < 8; padding++ {
		candidate := certificateOffset - padding - payloadFooterSize
		if candidate < 0 {
			break
		}
		if padding > 0 {
			pad, readErr := readAt(file, candidate+payloadFooterSize, int(padding))
			if readErr != nil || !allZero(pad) {
				continue
			}
		}
		candidateFooter, readErr := readAt(file, candidate, payloadFooterSize)
		if readErr == nil && bytes.Equal(candidateFooter[:16], payloadFooterMagic[:]) {
			return candidate, candidateFooter, nil
		}
	}
	return 0, nil, errors.New("desktop setup: signed payload footer not found")
}

func peCertificateTable(file *os.File, size int64) (int64, int64, error) {
	headerSize := int64(4096)
	if size < headerSize {
		headerSize = size
	}
	header, err := readAt(file, 0, int(headerSize))
	if err != nil {
		return 0, 0, err
	}
	if len(header) < 0x40 || string(header[:2]) != "MZ" {
		return 0, 0, errors.New("not a PE executable")
	}
	peOffset := int(binary.LittleEndian.Uint32(header[0x3c:0x40]))
	if peOffset < 0x40 || peOffset+24 > len(header) || string(header[peOffset:peOffset+4]) != "PE\x00\x00" {
		return 0, 0, errors.New("invalid PE header")
	}
	optionalSize := int(binary.LittleEndian.Uint16(header[peOffset+20 : peOffset+22]))
	optionalOffset := peOffset + 24
	if optionalSize < 0 || optionalOffset+optionalSize > len(header) {
		return 0, 0, errors.New("invalid PE optional header")
	}
	optional := header[optionalOffset : optionalOffset+optionalSize]
	if len(optional) < 2 {
		return 0, 0, errors.New("missing PE optional header")
	}
	var numberOffset, directoryOffset int
	switch binary.LittleEndian.Uint16(optional[:2]) {
	case 0x10b:
		numberOffset, directoryOffset = 92, 96
	case 0x20b:
		numberOffset, directoryOffset = 108, 112
	default:
		return 0, 0, errors.New("unsupported PE optional header")
	}
	const certificateDirectoryIndex = 4
	entryOffset := directoryOffset + certificateDirectoryIndex*8
	if len(optional) < numberOffset+4 || binary.LittleEndian.Uint32(optional[numberOffset:numberOffset+4]) <= certificateDirectoryIndex || len(optional) < entryOffset+8 {
		return 0, 0, errors.New("missing PE certificate directory")
	}
	offset := int64(binary.LittleEndian.Uint32(optional[entryOffset : entryOffset+4]))
	length := int64(binary.LittleEndian.Uint32(optional[entryOffset+4 : entryOffset+8]))
	return offset, length, nil
}

func readAt(file *os.File, offset int64, length int) ([]byte, error) {
	if offset < 0 || length < 0 {
		return nil, errors.New("desktop setup: invalid read range")
	}
	data := make([]byte, length)
	if _, err := file.ReadAt(data, offset); err != nil {
		return nil, err
	}
	return data, nil
}

func allZero(data []byte) bool {
	for _, value := range data {
		if value != 0 {
			return false
		}
	}
	return true
}

func (p *Payload) Verify(expectedVersion, expectedArch string) (PayloadManifest, error) {
	if p == nil || len(p.data) == 0 {
		return PayloadManifest{}, errors.New("desktop setup: payload is not initialized")
	}
	if err := validateVersion(expectedVersion); err != nil {
		return PayloadManifest{}, err
	}
	if err := validateArch(expectedArch); err != nil {
		return PayloadManifest{}, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.verified {
		if p.manifest.Version != expectedVersion || p.manifest.Arch != expectedArch {
			return PayloadManifest{}, errors.New("desktop setup: payload identity mismatch")
		}
		return cloneManifest(p.manifest), nil
	}

	reader, err := zip.NewReader(bytes.NewReader(p.data), int64(len(p.data)))
	if err != nil {
		return PayloadManifest{}, fmt.Errorf("desktop setup: open payload ZIP: %w", err)
	}
	entries := make(map[string]*zip.File, len(reader.File))
	for _, entry := range reader.File {
		if !allowedPayloadEntry(entry.Name) || entry.FileInfo().IsDir() || strings.Contains(entry.Name, "\\") {
			return PayloadManifest{}, fmt.Errorf("desktop setup: disallowed payload entry %q", entry.Name)
		}
		if _, duplicate := entries[entry.Name]; duplicate {
			return PayloadManifest{}, fmt.Errorf("desktop setup: duplicate payload entry %q", entry.Name)
		}
		if entry.UncompressedSize64 == 0 || entry.UncompressedSize64 > maximumPayloadFileSize {
			return PayloadManifest{}, fmt.Errorf("desktop setup: invalid payload entry size for %q", entry.Name)
		}
		if entry.Method != zip.Store && entry.Method != zip.Deflate {
			return PayloadManifest{}, fmt.Errorf("desktop setup: unsupported compression for %q", entry.Name)
		}
		entries[entry.Name] = entry
	}
	if len(entries) != len(payloadEntryNames) {
		return PayloadManifest{}, fmt.Errorf("desktop setup: payload has %d entries, want %d", len(entries), len(payloadEntryNames))
	}
	for _, name := range payloadEntryNames {
		if entries[name] == nil {
			return PayloadManifest{}, fmt.Errorf("desktop setup: payload is missing %q", name)
		}
	}

	manifestData, err := readZIPEntry(entries["payload.json"], maximumManifestBytes)
	if err != nil {
		return PayloadManifest{}, err
	}
	manifest, err := decodeManifest(manifestData)
	if err != nil {
		return PayloadManifest{}, err
	}
	if err := validateManifest(manifest, expectedVersion, expectedArch); err != nil {
		return PayloadManifest{}, err
	}

	files := make(map[string][]byte, len(payloadEntryNames))
	files["payload.json"] = manifestData
	for index, expectedName := range payloadFileNames {
		manifestFile := manifest.Files[index]
		data, readErr := readZIPEntry(entries[expectedName], maximumPayloadFileSize)
		if readErr != nil {
			return PayloadManifest{}, readErr
		}
		digest := sha256.Sum256(data)
		if int64(len(data)) != manifestFile.Size || hex.EncodeToString(digest[:]) != manifestFile.SHA256 {
			return PayloadManifest{}, fmt.Errorf("desktop setup: payload entry %q does not match its manifest", expectedName)
		}
		files[expectedName] = data
	}

	p.verified = true
	p.manifest = cloneManifest(manifest)
	p.files = files
	return cloneManifest(manifest), nil
}

func decodeManifest(data []byte) (PayloadManifest, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var manifest PayloadManifest
	if err := decoder.Decode(&manifest); err != nil {
		return PayloadManifest{}, fmt.Errorf("desktop setup: decode payload manifest: %w", err)
	}
	var trailer any
	if err := decoder.Decode(&trailer); !errors.Is(err, io.EOF) {
		if err == nil {
			return PayloadManifest{}, errors.New("desktop setup: payload manifest contains multiple JSON values")
		}
		return PayloadManifest{}, fmt.Errorf("desktop setup: decode payload manifest trailer: %w", err)
	}
	return manifest, nil
}

func validateManifest(manifest PayloadManifest, expectedVersion, expectedArch string) error {
	if err := validateVersion(manifest.Version); err != nil {
		return err
	}
	if err := validateArch(manifest.Arch); err != nil {
		return err
	}
	if manifest.Version != expectedVersion || manifest.Arch != expectedArch {
		return errors.New("desktop setup: payload identity mismatch")
	}
	if len(manifest.Files) != len(payloadFileNames) {
		return fmt.Errorf("desktop setup: manifest has %d files, want %d", len(manifest.Files), len(payloadFileNames))
	}
	for index, name := range payloadFileNames {
		file := manifest.Files[index]
		if file.Name != name {
			return fmt.Errorf("desktop setup: manifest file %d is %q, want %q", index, file.Name, name)
		}
		if file.Size <= 0 || file.Size > maximumPayloadFileSize {
			return fmt.Errorf("desktop setup: invalid manifest size for %q", name)
		}
		decoded, err := hex.DecodeString(file.SHA256)
		if err != nil || len(decoded) != sha256.Size || file.SHA256 != strings.ToLower(file.SHA256) {
			return fmt.Errorf("desktop setup: invalid manifest hash for %q", name)
		}
	}
	return nil
}

func validateVersion(version string) error {
	if version == "" || len(version) > maximumVersionBytes || version != strings.TrimSpace(version) {
		return errors.New("desktop setup: invalid version")
	}
	for index, char := range version {
		valid := char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' ||
			(index > 0 && strings.ContainsRune("._+-", char))
		if !valid {
			return errors.New("desktop setup: invalid version")
		}
	}
	return nil
}

func validateArch(arch string) error {
	if arch != "amd64" && arch != "arm64" {
		return fmt.Errorf("desktop setup: unsupported architecture %q", arch)
	}
	return nil
}

func allowedPayloadEntry(name string) bool {
	for _, allowed := range payloadEntryNames {
		if name == allowed {
			return true
		}
	}
	return false
}

func readZIPEntry(entry *zip.File, limit int64) ([]byte, error) {
	if entry == nil || limit <= 0 {
		return nil, errors.New("desktop setup: invalid ZIP entry")
	}
	reader, err := entry.Open()
	if err != nil {
		return nil, fmt.Errorf("desktop setup: open payload entry %q: %w", entry.Name, err)
	}
	defer reader.Close()
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, fmt.Errorf("desktop setup: read payload entry %q: %w", entry.Name, err)
	}
	if int64(len(data)) > limit || uint64(len(data)) != entry.UncompressedSize64 {
		return nil, fmt.Errorf("desktop setup: invalid payload entry size for %q", entry.Name)
	}
	return data, nil
}

func cloneManifest(manifest PayloadManifest) PayloadManifest {
	clone := manifest
	clone.Files = append([]PayloadFile(nil), manifest.Files...)
	return clone
}

func (p *Payload) Extract(destination string) (returnErr error) {
	if destination == "" || !filepath.IsAbs(destination) || destination != filepath.Clean(destination) {
		return errors.New("desktop setup: extraction destination must be an absolute clean path")
	}
	p.mu.RLock()
	if !p.verified {
		p.mu.RUnlock()
		return errors.New("desktop setup: payload must be verified before extraction")
	}
	files := make(map[string][]byte, len(p.files))
	for name, data := range p.files {
		files[name] = data
	}
	p.mu.RUnlock()

	if _, err := os.Lstat(destination); err == nil {
		return errors.New("desktop setup: extraction destination already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("desktop setup: inspect extraction destination: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return fmt.Errorf("desktop setup: create extraction parent: %w", err)
	}
	if err := os.Mkdir(destination, 0o755); err != nil {
		return fmt.Errorf("desktop setup: create extraction destination: %w", err)
	}
	defer func() {
		if returnErr != nil {
			_ = os.RemoveAll(destination)
		}
	}()

	for _, name := range payloadEntryNames {
		mode := os.FileMode(0o600)
		if strings.HasSuffix(strings.ToLower(name), ".exe") {
			mode = 0o700
		}
		path := filepath.Join(destination, name)
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		if err != nil {
			return fmt.Errorf("desktop setup: create extracted file %q: %w", name, err)
		}
		if _, err := file.Write(files[name]); err != nil {
			_ = file.Close()
			return fmt.Errorf("desktop setup: write extracted file %q: %w", name, err)
		}
		if err := file.Sync(); err != nil {
			_ = file.Close()
			return fmt.Errorf("desktop setup: sync extracted file %q: %w", name, err)
		}
		if err := file.Close(); err != nil {
			return fmt.Errorf("desktop setup: close extracted file %q: %w", name, err)
		}
	}
	return nil
}

// CreatePayloadArchive writes a deterministic allowlisted ZIP and manifest.
func CreatePayloadArchive(outputPath, version, arch string, sources map[string]string) error {
	if err := validateVersion(version); err != nil {
		return err
	}
	if err := validateArch(arch); err != nil {
		return err
	}
	if len(sources) != len(payloadFileNames) {
		return fmt.Errorf("desktop setup: got %d payload sources, want %d", len(sources), len(payloadFileNames))
	}

	manifest := PayloadManifest{Version: version, Arch: arch, Files: make([]PayloadFile, 0, len(payloadFileNames))}
	fileData := make(map[string][]byte, len(payloadFileNames))
	for _, name := range payloadFileNames {
		path := sources[name]
		if path == "" {
			return fmt.Errorf("desktop setup: missing source for %q", name)
		}
		data, err := readBoundedFile(path, maximumPayloadFileSize)
		if err != nil {
			return fmt.Errorf("desktop setup: read source for %q: %w", name, err)
		}
		digest := sha256.Sum256(data)
		manifest.Files = append(manifest.Files, PayloadFile{
			Name: name, Size: int64(len(data)), SHA256: hex.EncodeToString(digest[:]),
		})
		fileData[name] = data
	}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("desktop setup: encode payload manifest: %w", err)
	}
	manifestData = append(manifestData, '\n')

	return writeFileAtomically(outputPath, 0o600, func(output *os.File) error {
		archive := zip.NewWriter(output)
		for _, name := range payloadEntryNames {
			data := fileData[name]
			mode := os.FileMode(0o600)
			if name == "payload.json" {
				data = manifestData
			} else if strings.HasSuffix(strings.ToLower(name), ".exe") {
				mode = 0o700
			}
			header := &zip.FileHeader{Name: name, Method: zip.Deflate}
			header.SetMode(mode)
			writer, createErr := archive.CreateHeader(header)
			if createErr != nil {
				_ = archive.Close()
				return fmt.Errorf("desktop setup: create payload entry %q: %w", name, createErr)
			}
			if _, writeErr := writer.Write(data); writeErr != nil {
				_ = archive.Close()
				return fmt.Errorf("desktop setup: write payload entry %q: %w", name, writeErr)
			}
		}
		if err := archive.Close(); err != nil {
			return fmt.Errorf("desktop setup: close payload ZIP: %w", err)
		}
		return nil
	})
}

func AppendPayload(stubPath, zipPath, outputPath string) error {
	stubAbsolute, err := filepath.Abs(stubPath)
	if err != nil {
		return err
	}
	zipAbsolute, err := filepath.Abs(zipPath)
	if err != nil {
		return err
	}
	outputAbsolute, err := filepath.Abs(outputPath)
	if err != nil {
		return err
	}
	if outputAbsolute == stubAbsolute || outputAbsolute == zipAbsolute {
		return errors.New("desktop setup: output must differ from payload inputs")
	}
	zipInfo, err := os.Stat(zipAbsolute)
	if err != nil {
		return fmt.Errorf("desktop setup: stat payload ZIP: %w", err)
	}
	if !zipInfo.Mode().IsRegular() || zipInfo.Size() <= 0 || zipInfo.Size() > maximumPayloadBytes {
		return errors.New("desktop setup: invalid payload ZIP size")
	}

	return writeFileAtomically(outputAbsolute, 0o700, func(output *os.File) error {
		stub, err := os.Open(stubAbsolute)
		if err != nil {
			return fmt.Errorf("desktop setup: open setup stub: %w", err)
		}
		if _, err := io.Copy(output, stub); err != nil {
			_ = stub.Close()
			return fmt.Errorf("desktop setup: copy setup stub: %w", err)
		}
		if err := stub.Close(); err != nil {
			return fmt.Errorf("desktop setup: close setup stub: %w", err)
		}

		payload, err := os.Open(zipAbsolute)
		if err != nil {
			return fmt.Errorf("desktop setup: open payload ZIP: %w", err)
		}
		hash := sha256.New()
		written, copyErr := io.Copy(io.MultiWriter(output, hash), payload)
		closeErr := payload.Close()
		if copyErr != nil {
			return fmt.Errorf("desktop setup: append payload ZIP: %w", copyErr)
		}
		if closeErr != nil {
			return fmt.Errorf("desktop setup: close payload ZIP: %w", closeErr)
		}
		if written != zipInfo.Size() {
			return errors.New("desktop setup: payload ZIP changed while reading")
		}

		footer := make([]byte, payloadFooterSize)
		copy(footer[:16], payloadFooterMagic[:])
		binary.LittleEndian.PutUint64(footer[16:24], uint64(written))
		copy(footer[24:], hash.Sum(nil))
		if _, err := output.Write(footer); err != nil {
			return fmt.Errorf("desktop setup: write payload footer: %w", err)
		}
		return nil
	})
}

func readBoundedFile(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > limit {
		return nil, errors.New("invalid file size")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != info.Size() {
		return nil, errors.New("file changed while reading")
	}
	return data, nil
}

func writeFileAtomically(path string, mode os.FileMode, write func(*os.File) error) (returnErr error) {
	if path == "" || write == nil {
		return errors.New("desktop setup: invalid output")
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return fmt.Errorf("desktop setup: create output directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".codesk-setup-*")
	if err != nil {
		return fmt.Errorf("desktop setup: create temporary output: %w", err)
	}
	temporaryPath := temporary.Name()
	closed := false
	defer func() {
		if !closed {
			if err := temporary.Close(); returnErr == nil && err != nil {
				returnErr = err
			}
		}
		_ = os.Remove(temporaryPath)
	}()
	if err := temporary.Chmod(mode); err != nil {
		return fmt.Errorf("desktop setup: set output permissions: %w", err)
	}
	if err := write(temporary); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("desktop setup: sync output: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("desktop setup: close output: %w", err)
	}
	closed = true
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("desktop setup: replace output: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("desktop setup: publish output: %w", err)
	}
	return nil
}
