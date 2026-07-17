package main

import (
	"debug/buildinfo"
	"encoding/asn1"
	"encoding/binary"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"

	"notty/daemon/internal/desktoprelease"
)

func TestVerifyGoBuildInfoRejectsUnpinnedOrVCSStampedBinary(t *testing.T) {
	valid := &buildinfo.BuildInfo{GoVersion: releaseGoVersion}
	if err := verifyGoBuildInfo(valid); err != nil {
		t.Fatal(err)
	}
	for name, info := range map[string]*buildinfo.BuildInfo{
		"missing":  nil,
		"wrong Go": {GoVersion: "go1.23.12"},
		"revision": {
			GoVersion: releaseGoVersion,
			Settings:  []debug.BuildSetting{{Key: "vcs.revision", Value: strings.Repeat("a", 40)}},
		},
		"modified": {
			GoVersion: releaseGoVersion,
			Settings:  []debug.BuildSetting{{Key: "vcs.modified", Value: "true"}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := verifyGoBuildInfo(info); err == nil {
				t.Fatal("verifyGoBuildInfo() accepted non-canonical build information")
			}
		})
	}
}

type signedDataFixture struct {
	contentType  asn1.ObjectIdentifier
	spcType      asn1.ObjectIdentifier
	digests      bool
	certificates bool
	signers      bool
}

func TestVerifyPKCS7Envelope(t *testing.T) {
	valid := signedDataEnvelope(t, signedDataFixture{
		contentType: signedDataOID, spcType: spcIndirectDataOID,
		digests: true, certificates: true, signers: true,
	})
	if err := verifyPKCS7Envelope(valid); err != nil {
		t.Fatal(err)
	}
	for padding := 1; padding <= 7; padding++ {
		padded := append(append([]byte(nil), valid...), make([]byte, padding)...)
		if err := verifyPKCS7Envelope(padded); err != nil {
			t.Fatalf("verifyPKCS7Envelope() rejected %d bytes of WIN_CERTIFICATE padding: %v", padding, err)
		}
	}
	for name, data := range map[string][]byte{
		"wrong content type": signedDataEnvelope(t, signedDataFixture{
			contentType: asn1.ObjectIdentifier{1, 2, 3}, spcType: spcIndirectDataOID,
			digests: true, certificates: true, signers: true,
		}),
		"wrong SPC content type": signedDataEnvelope(t, signedDataFixture{
			contentType: signedDataOID, spcType: asn1.ObjectIdentifier{1, 2, 3},
			digests: true, certificates: true, signers: true,
		}),
		"missing digests": signedDataEnvelope(t, signedDataFixture{
			contentType: signedDataOID, spcType: spcIndirectDataOID,
			certificates: true, signers: true,
		}),
		"missing certificates": signedDataEnvelope(t, signedDataFixture{
			contentType: signedDataOID, spcType: spcIndirectDataOID,
			digests: true, signers: true,
		}),
		"missing signers": signedDataEnvelope(t, signedDataFixture{
			contentType: signedDataOID, spcType: spcIndirectDataOID,
			digests: true, certificates: true,
		}),
		"empty SignedData": {
			0x30, 0x0f,
			0x06, 0x09, 0x2a, 0x86, 0x48, 0x86, 0xf7, 0x0d, 0x01, 0x07, 0x02,
			0xa0, 0x02, 0x30, 0x00,
		},
		"nonzero trailing data": append(append([]byte(nil), valid...), 1),
		"excessive padding":     append(append([]byte(nil), valid...), make([]byte, 8)...),
	} {
		t.Run(name, func(t *testing.T) {
			if err := verifyPKCS7Envelope(data); err == nil {
				t.Fatal("verifyPKCS7Envelope() accepted malformed data")
			}
		})
	}
}

func TestVerifyCertificateTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "signed.bin")
	envelope := signedDataEnvelope(t, signedDataFixture{
		contentType: signedDataOID, spcType: spcIndirectDataOID,
		digests: true, certificates: true, signers: true,
	})
	entryLength := 8 + len(envelope)
	tableLength := (entryLength + 7) &^ 7
	data := make([]byte, 8+tableLength)
	binary.LittleEndian.PutUint32(data[8:12], uint32(entryLength))
	binary.LittleEndian.PutUint16(data[12:14], 0x0200)
	binary.LittleEndian.PutUint16(data[14:16], 0x0002)
	copy(data[16:], envelope)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyCertificateTable(file, int64(len(data)), 8, uint32(tableLength), true); err != nil {
		t.Fatal(err)
	}
	if err := verifyCertificateTable(file, int64(len(data)), 8, uint32(tableLength), false); err == nil {
		t.Fatal("verifyCertificateTable() accepted a certificate in unsigned mode")
	}
	if err := verifyCertificateTable(file, int64(len(data)), 0, 0, true); err == nil {
		t.Fatal("verifyCertificateTable() accepted a missing required certificate")
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	data[len(data)-1] = 1
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	file, err = os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := verifyCertificateTable(file, int64(len(data)), 8, uint32(tableLength), true); err == nil {
		t.Fatal("verifyCertificateTable() accepted nonzero alignment padding")
	}
}

func signedDataEnvelope(t *testing.T, fixture signedDataFixture) []byte {
	t.Helper()
	version := marshalASN1(t, 1)
	var digestAlgorithms []byte
	if fixture.digests {
		digestAlgorithms = marshalASN1(t, struct {
			Algorithm asn1.ObjectIdentifier
		}{Algorithm: asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 1}})
	}
	digests := marshalASN1(t, asn1.RawValue{Tag: 17, IsCompound: true, Bytes: digestAlgorithms})

	contentType := marshalASN1(t, fixture.spcType)
	content := marshalASN1(t, asn1.RawValue{
		Class: 2, Tag: 0, IsCompound: true, Bytes: marshalASN1(t, struct{}{}),
	})
	encapsulatedContent := marshalASN1(t, asn1.RawValue{
		Tag: 16, IsCompound: true, Bytes: append(contentType, content...),
	})

	fields := append(append(version, digests...), encapsulatedContent...)
	if fixture.certificates {
		certificate := marshalASN1(t, asn1.RawValue{Tag: 16, IsCompound: true})
		fields = append(fields, marshalASN1(t, asn1.RawValue{
			Class: 2, Tag: 0, IsCompound: true, Bytes: certificate,
		})...)
	}
	var signerInfos []byte
	if fixture.signers {
		signerInfos = marshalASN1(t, asn1.RawValue{Tag: 16, IsCompound: true})
	}
	fields = append(fields, marshalASN1(t, asn1.RawValue{
		Tag: 17, IsCompound: true, Bytes: signerInfos,
	})...)
	signedData := marshalASN1(t, asn1.RawValue{Tag: 16, IsCompound: true, Bytes: fields})
	explicitSignedData := marshalASN1(t, asn1.RawValue{
		Class: 2, Tag: 0, IsCompound: true, Bytes: signedData,
	})
	outer := append(marshalASN1(t, fixture.contentType), explicitSignedData...)
	return marshalASN1(t, asn1.RawValue{Tag: 16, IsCompound: true, Bytes: outer})
}

func marshalASN1(t *testing.T, value any) []byte {
	t.Helper()
	data, err := asn1.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestValidateReleaseManifestHeaderRequiresSourceBinding(t *testing.T) {
	version, err := desktoprelease.ParseVersion("dev", windowsReleaseVersionPolicy)
	if err != nil {
		t.Fatal(err)
	}
	valid := releaseManifest{
		Version:        "dev",
		SourceRevision: testSourceRevision,
		Signed:         false,
		Toolchain:      canonicalReleaseToolchain,
	}
	if err := validateReleaseManifestHeader(valid, version, false); err != nil {
		t.Fatal(err)
	}

	for name, mutate := range map[string]func(*releaseManifest){
		"missing source revision": func(manifest *releaseManifest) { manifest.SourceRevision = "" },
		"zero source revision":    func(manifest *releaseManifest) { manifest.SourceRevision = strings.Repeat("0", 40) },
		"uppercase source revision": func(manifest *releaseManifest) {
			manifest.SourceRevision = strings.ToUpper(testSourceRevision)
		},
		"wrong version":   func(manifest *releaseManifest) { manifest.Version = "other" },
		"wrong signed":    func(manifest *releaseManifest) { manifest.Signed = true },
		"wrong toolchain": func(manifest *releaseManifest) { manifest.Toolchain.Go = "go0.0.0" },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := valid
			mutate(&candidate)
			if err := validateReleaseManifestHeader(candidate, version, false); err == nil {
				t.Fatal("validateReleaseManifestHeader() accepted mutated release metadata")
			}
		})
	}
}
