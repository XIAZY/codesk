package main

import (
	"bytes"
	"debug/buildinfo"
	"debug/pe"
	"encoding/asn1"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"notty/daemon/internal/desktopsetup"
)

const (
	peSubsystemGUI         = 2
	peSubsystemConsole     = 3
	maximumResourceSize    = 16 << 20
	maximumCertificateSize = 16 << 20
)

var windowsMachines = map[string]uint16{
	"amd64": pe.IMAGE_FILE_MACHINE_AMD64,
	"arm64": pe.IMAGE_FILE_MACHINE_ARM64,
}

var (
	signedDataOID      = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 2}
	spcIndirectDataOID = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 311, 2, 1, 4}
)

func verifyRelease(versionDirectory, version string, allowUnsigned bool) error {
	if err := validateReleaseVersion(version); err != nil {
		return err
	}
	if err := verifyReleaseEntries(versionDirectory, version); err != nil {
		return err
	}
	manifest, err := readReleaseManifest(filepath.Join(versionDirectory, "manifest.json"))
	if err != nil {
		return err
	}
	expectedSigned := !allowUnsigned
	if manifest.Version != version {
		return fmt.Errorf("manifest version %q, want %q", manifest.Version, version)
	}
	if manifest.Signed != expectedSigned {
		return fmt.Errorf("manifest signed=%t, want %t", manifest.Signed, expectedSigned)
	}
	if manifest.Toolchain != canonicalReleaseToolchain {
		return fmt.Errorf("manifest toolchain %#v, want %#v", manifest.Toolchain, canonicalReleaseToolchain)
	}
	if len(manifest.Artifacts) != len(releaseArchitectures) {
		return fmt.Errorf("manifest has %d artifacts, want %d", len(manifest.Artifacts), len(releaseArchitectures))
	}
	checksums, err := readChecksums(filepath.Join(versionDirectory, "SHA256SUMS"))
	if err != nil {
		return err
	}
	if len(checksums) != len(releaseArchitectures) {
		return fmt.Errorf("SHA256SUMS has %d entries, want %d", len(checksums), len(releaseArchitectures))
	}

	artifacts := make(map[string]releaseArtifact, len(manifest.Artifacts))
	for _, artifact := range manifest.Artifacts {
		if _, supported := windowsMachines[artifact.Arch]; !supported {
			return fmt.Errorf("manifest has unsupported architecture %q", artifact.Arch)
		}
		if _, duplicate := artifacts[artifact.Arch]; duplicate {
			return fmt.Errorf("manifest has duplicate windows/%s artifact", artifact.Arch)
		}
		if artifact.Signed != expectedSigned {
			return fmt.Errorf("windows/%s artifact signed=%t, want %t", artifact.Arch, artifact.Signed, expectedSigned)
		}
		artifacts[artifact.Arch] = artifact
	}

	for _, arch := range releaseArchitectures {
		artifact, found := artifacts[arch]
		if !found {
			return fmt.Errorf("manifest is missing windows/%s", arch)
		}
		expectedName := setupFilename(version, arch)
		if artifact.File != expectedName {
			return fmt.Errorf("windows/%s artifact is %q, want %q", arch, artifact.File, expectedName)
		}
		setupPath := filepath.Join(versionDirectory, artifact.File)
		actualHash, err := fileSHA256(setupPath)
		if err != nil {
			return err
		}
		if artifact.SHA256 != actualHash {
			return fmt.Errorf("manifest hash for %s is %q, want %q", artifact.File, artifact.SHA256, actualHash)
		}
		if checksum := checksums[artifact.File]; checksum != actualHash {
			return fmt.Errorf("SHA256SUMS hash for %s is %q, want %q", artifact.File, checksum, actualHash)
		}
		if err := inspectPE(setupPath, windowsMachines[arch], peSubsystemGUI, true, expectedSigned); err != nil {
			return fmt.Errorf("%s: %w", artifact.File, err)
		}
		payload, err := desktopsetup.OpenPayload(setupPath)
		if err != nil {
			return fmt.Errorf("%s: %w", artifact.File, err)
		}
		if _, err := payload.Verify(version, arch); err != nil {
			return fmt.Errorf("%s: %w", artifact.File, err)
		}
		temporary, err := os.MkdirTemp("", "codesk-release-verify-*")
		if err != nil {
			return err
		}
		verifyErr := verifyPayloadBinaries(payload, filepath.Join(temporary, "payload"), arch, expectedSigned)
		removeErr := os.RemoveAll(temporary)
		if err := errors.Join(verifyErr, removeErr); err != nil {
			return fmt.Errorf("%s: %w", artifact.File, err)
		}
		fmt.Printf("verified windows/%s %s (machine=0x%04x signed=%t)\n", arch, artifact.File, windowsMachines[arch], expectedSigned)
	}
	return nil
}

func verifyReleaseEntries(directory, version string) error {
	info, err := os.Lstat(directory)
	if err != nil {
		return fmt.Errorf("stat release directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("release path is not a real directory")
	}
	expected := map[string]bool{
		"manifest.json":                 true,
		"SHA256SUMS":                    true,
		setupFilename(version, "amd64"): true,
		setupFilename(version, "arm64"): true,
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return fmt.Errorf("read release directory: %w", err)
	}
	if len(entries) != len(expected) {
		return fmt.Errorf("release directory has %d entries, want %d", len(entries), len(expected))
	}
	for _, entry := range entries {
		if !expected[entry.Name()] {
			return fmt.Errorf("release directory has unexpected entry %q", entry.Name())
		}
		entryInfo, err := entry.Info()
		if err != nil {
			return fmt.Errorf("stat release entry %q: %w", entry.Name(), err)
		}
		if entry.Type()&os.ModeSymlink != 0 || !entryInfo.Mode().IsRegular() {
			return fmt.Errorf("release entry %q is not a regular file", entry.Name())
		}
	}
	return nil
}

func verifyPayloadBinaries(payload *desktopsetup.Payload, directory, arch string, signed bool) error {
	if err := payload.Extract(directory); err != nil {
		return err
	}
	if err := inspectPE(filepath.Join(directory, "Codesk.exe"), windowsMachines[arch], peSubsystemGUI, true, signed); err != nil {
		return fmt.Errorf("Codesk.exe: %w", err)
	}
	if err := inspectPE(filepath.Join(directory, "notty-agent-tool.exe"), windowsMachines[arch], peSubsystemConsole, false, signed); err != nil {
		return fmt.Errorf("notty-agent-tool.exe: %w", err)
	}
	iconInfo, err := os.Stat(filepath.Join(directory, "codesk.ico"))
	if err != nil || !iconInfo.Mode().IsRegular() || iconInfo.Size() <= 0 {
		return errors.New("codesk.ico is missing or invalid")
	}
	return nil
}

func inspectPE(path string, machine, subsystem uint16, resourcesRequired, signed bool) error {
	if err := verifyGoBuildSettings(path); err != nil {
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("not a regular PE file")
	}
	image, err := pe.NewFile(file)
	if err != nil {
		return fmt.Errorf("parse PE: %w", err)
	}
	defer image.Close()
	if image.FileHeader.Machine != machine {
		return fmt.Errorf("machine=0x%04x, want 0x%04x", image.FileHeader.Machine, machine)
	}
	actualSubsystem, certificateOffset, certificateSize, err := peMetadata(image)
	if err != nil {
		return err
	}
	if actualSubsystem != subsystem {
		return fmt.Errorf("subsystem=%d, want %d", actualSubsystem, subsystem)
	}
	if err := verifyCertificateTable(file, info.Size(), certificateOffset, certificateSize, signed); err != nil {
		return err
	}
	if resourcesRequired {
		if err := verifyResources(image); err != nil {
			return err
		}
	}
	return nil
}

func verifyGoBuildSettings(path string) error {
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read Go build information: %w", err)
	}
	return verifyGoBuildInfo(info)
}

func verifyGoBuildInfo(info *buildinfo.BuildInfo) error {
	if info == nil {
		return errors.New("Go build information is missing")
	}
	if info.GoVersion != releaseGoVersion {
		return fmt.Errorf("Go toolchain=%q, want %q", info.GoVersion, releaseGoVersion)
	}
	for _, setting := range info.Settings {
		if strings.HasPrefix(setting.Key, "vcs.") {
			return fmt.Errorf("Go build contains forbidden VCS setting %q", setting.Key)
		}
	}
	return nil
}

func peMetadata(image *pe.File) (uint16, uint32, uint32, error) {
	const certificateDirectory = 4
	switch optional := image.OptionalHeader.(type) {
	case *pe.OptionalHeader32:
		if len(optional.DataDirectory) <= certificateDirectory {
			return 0, 0, 0, errors.New("PE certificate directory is missing")
		}
		entry := optional.DataDirectory[certificateDirectory]
		return optional.Subsystem, entry.VirtualAddress, entry.Size, nil
	case *pe.OptionalHeader64:
		if len(optional.DataDirectory) <= certificateDirectory {
			return 0, 0, 0, errors.New("PE certificate directory is missing")
		}
		entry := optional.DataDirectory[certificateDirectory]
		return optional.Subsystem, entry.VirtualAddress, entry.Size, nil
	default:
		return 0, 0, 0, errors.New("unsupported PE optional header")
	}
}

func verifyCertificateTable(file *os.File, fileSize int64, offset, size uint32, required bool) error {
	if offset == 0 && size == 0 {
		if required {
			return errors.New("Authenticode certificate table is missing")
		}
		return nil
	}
	if !required {
		return errors.New("unexpected Authenticode certificate table in unsigned artifact")
	}
	if offset == 0 || size < 8 || size > maximumCertificateSize || offset%8 != 0 || int64(offset)+int64(size) != fileSize {
		return errors.New("invalid Authenticode certificate table")
	}
	data := make([]byte, int(size))
	if _, err := file.ReadAt(data, int64(offset)); err != nil {
		return fmt.Errorf("read Authenticode certificate table: %w", err)
	}
	for position := 0; position < len(data); {
		if len(data)-position < 8 {
			return errors.New("truncated Authenticode certificate entry")
		}
		length := int(binary.LittleEndian.Uint32(data[position : position+4]))
		revision := binary.LittleEndian.Uint16(data[position+4 : position+6])
		certificateType := binary.LittleEndian.Uint16(data[position+6 : position+8])
		if length < 8 || length > len(data)-position || revision != 0x0200 || certificateType != 0x0002 {
			return errors.New("invalid Authenticode certificate entry")
		}
		if err := verifyPKCS7Envelope(data[position+8 : position+length]); err != nil {
			return err
		}
		aligned := (length + 7) &^ 7
		if aligned > len(data)-position || !allZero(data[position+length:position+aligned]) {
			return errors.New("invalid Authenticode certificate padding")
		}
		position += aligned
	}
	return nil
}

func verifyPKCS7Envelope(data []byte) error {
	var envelope struct {
		ContentType asn1.ObjectIdentifier
		Content     asn1.RawValue `asn1:"tag:0,explicit"`
	}
	rest, err := asn1.Unmarshal(data, &envelope)
	if err != nil || len(rest) > 7 || !allZero(rest) || !envelope.ContentType.Equal(signedDataOID) || len(envelope.Content.FullBytes) == 0 {
		return errors.New("invalid Authenticode PKCS#7 envelope")
	}

	var signedData asn1.RawValue
	rest, err = asn1.Unmarshal(envelope.Content.Bytes, &signedData)
	if err != nil || len(rest) != 0 || signedData.Class != 0 || signedData.Tag != 16 || !signedData.IsCompound {
		return errors.New("invalid Authenticode SignedData")
	}
	fields, err := rawASN1Values(signedData.Bytes)
	if err != nil || len(fields) < 5 || fields[0].Class != 0 || fields[0].Tag != 2 ||
		fields[1].Class != 0 || fields[1].Tag != 17 || !fields[1].IsCompound || len(fields[1].Bytes) == 0 ||
		fields[2].Class != 0 || fields[2].Tag != 16 || !fields[2].IsCompound {
		return errors.New("invalid Authenticode SignedData")
	}
	if err := verifySPCContentInfo(fields[2]); err != nil {
		return err
	}
	index := 3
	if fields[index].Class != 2 || fields[index].Tag != 0 || !fields[index].IsCompound || len(fields[index].Bytes) == 0 {
		return errors.New("Authenticode SignedData certificates are missing")
	}
	index++
	if index < len(fields)-1 && fields[index].Class == 2 && fields[index].Tag == 1 {
		index++
	}
	if index != len(fields)-1 {
		return errors.New("invalid Authenticode SignedData fields")
	}
	signers := fields[index]
	if signers.Class != 0 || signers.Tag != 17 || !signers.IsCompound || len(signers.Bytes) == 0 {
		return errors.New("Authenticode SignedData signer infos are missing")
	}
	return nil
}

func verifySPCContentInfo(contentInfo asn1.RawValue) error {
	var contentType asn1.ObjectIdentifier
	rest, err := asn1.Unmarshal(contentInfo.Bytes, &contentType)
	if err != nil || !contentType.Equal(spcIndirectDataOID) {
		return errors.New("invalid Authenticode SPC content type")
	}
	var content asn1.RawValue
	rest, err = asn1.Unmarshal(rest, &content)
	if err != nil || len(rest) != 0 || content.Class != 2 || content.Tag != 0 || !content.IsCompound || len(content.Bytes) == 0 {
		return errors.New("invalid Authenticode SPC content")
	}
	return nil
}

func rawASN1Values(data []byte) ([]asn1.RawValue, error) {
	var values []asn1.RawValue
	for len(data) > 0 {
		var value asn1.RawValue
		rest, err := asn1.Unmarshal(data, &value)
		if err != nil || len(rest) >= len(data) {
			return nil, errors.New("invalid Authenticode ASN.1 fields")
		}
		values = append(values, value)
		data = rest
	}
	return values, nil
}

func verifyResources(image *pe.File) error {
	var resourceSection *pe.Section
	for _, section := range image.Sections {
		if section.Name == ".rsrc" {
			resourceSection = section
			break
		}
	}
	if resourceSection == nil || resourceSection.Size == 0 || resourceSection.Size > maximumResourceSize {
		return errors.New("bounded PE resource section is missing")
	}
	data, err := resourceSection.Data()
	if err != nil {
		return fmt.Errorf("read PE resources: %w", err)
	}
	types, err := rootResourceTypes(data)
	if err != nil {
		return err
	}
	for _, required := range []uint32{3, 14, 16, 24} {
		if !types[required] {
			return fmt.Errorf("PE resource type %d is missing", required)
		}
	}
	return nil
}

func rootResourceTypes(data []byte) (map[uint32]bool, error) {
	if len(data) < 16 {
		return nil, errors.New("truncated PE resource directory")
	}
	named := int(binary.LittleEndian.Uint16(data[12:14]))
	ids := int(binary.LittleEndian.Uint16(data[14:16]))
	count := named + ids
	if count < 0 || count > (len(data)-16)/8 {
		return nil, errors.New("invalid PE resource directory")
	}
	types := make(map[uint32]bool, ids)
	for index := 0; index < count; index++ {
		entry := data[16+index*8 : 24+index*8]
		name := binary.LittleEndian.Uint32(entry[:4])
		child := binary.LittleEndian.Uint32(entry[4:])
		childOffset := int(child & 0x7fffffff)
		if child&0x80000000 == 0 || childOffset > len(data)-16 {
			return nil, errors.New("invalid PE resource child directory")
		}
		if name&0x80000000 == 0 {
			types[name] = true
		}
	}
	return types, nil
}

func readReleaseManifest(path string) (releaseManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return releaseManifest{}, fmt.Errorf("read manifest: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var manifest releaseManifest
	if err := decoder.Decode(&manifest); err != nil {
		return releaseManifest{}, fmt.Errorf("decode manifest: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return releaseManifest{}, errors.New("decode manifest: trailing data")
	}
	return manifest, nil
}

func readChecksums(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read SHA256SUMS: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	checksums := make(map[string]string, len(lines))
	for index, line := range lines {
		fields := strings.Fields(line)
		if len(fields) != 2 || filepath.Base(fields[1]) != fields[1] {
			return nil, fmt.Errorf("SHA256SUMS line %d is malformed", index+1)
		}
		decoded, err := hex.DecodeString(fields[0])
		if err != nil || len(decoded) != 32 || fields[0] != strings.ToLower(fields[0]) {
			return nil, fmt.Errorf("SHA256SUMS line %d has an invalid hash", index+1)
		}
		if _, duplicate := checksums[fields[1]]; duplicate {
			return nil, fmt.Errorf("SHA256SUMS has duplicate file %q", fields[1])
		}
		checksums[fields[1]] = fields[0]
	}
	return checksums, nil
}

func allZero(data []byte) bool {
	for _, value := range data {
		if value != 0 {
			return false
		}
	}
	return true
}
