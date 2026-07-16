package main

import (
	"bytes"
	"crypto/sha256"
	"debug/macho"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestParseReleaseVersion(t *testing.T) {
	tests := []struct {
		raw              string
		allowDevelopment bool
		wantBundle       string
		wantFailure      bool
	}{
		{raw: "1.2.3", wantBundle: "1.2.3"},
		{raw: "0.0.0", wantBundle: "0.0.0"},
		{raw: "dev", allowDevelopment: true, wantBundle: "0.0.0"},
		{raw: "dev", wantFailure: true},
		{raw: "1.2", wantFailure: true},
		{raw: "01.2.3", wantFailure: true},
		{raw: "1.2.3-beta", wantFailure: true},
		{raw: "+1.2.3", wantFailure: true},
		{raw: "1.2.2147483648", wantFailure: true},
		{raw: "1.2.-1", wantFailure: true},
	}
	for _, test := range tests {
		t.Run(test.raw, func(t *testing.T) {
			version, err := parseReleaseVersion(test.raw, test.allowDevelopment)
			if (err != nil) != test.wantFailure {
				t.Fatalf("parseReleaseVersion() error = %v, wantFailure = %t", err, test.wantFailure)
			}
			if err == nil && version.Bundle != test.wantBundle {
				t.Fatalf("Bundle = %q, want %q", version.Bundle, test.wantBundle)
			}
		})
	}
}

func TestVerifyReleaseRejectsNonCanonicalManifest(t *testing.T) {
	version, err := parseReleaseVersion("1.2.3", false)
	if err != nil {
		t.Fatal(err)
	}
	releaseRoot := t.TempDir()
	writeTestApp(t, releaseRoot, version)
	dmgPath := filepath.Join(releaseRoot, diskImageName(version))
	if err := os.WriteFile(dmgPath, []byte("test disk image"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeManifest(releaseRoot, version, strings.Repeat("a", 40), false); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(releaseRoot, "manifest.json")
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	manifest = bytes.Replace(manifest, []byte("  \"schema\""), []byte("    \"schema\""), 1)
	if err := os.WriteFile(manifestPath, manifest, 0o644); err != nil {
		t.Fatal(err)
	}
	dmg, err := inspectArtifact(dmgPath)
	if err != nil {
		t.Fatal(err)
	}
	manifestHash := sha256.Sum256(manifest)
	checksums := fmt.Sprintf("%s  %s\n%s  manifest.json\n", dmg.SHA256, dmg.Path, hex.EncodeToString(manifestHash[:]))
	if err := os.WriteFile(filepath.Join(releaseRoot, "SHA256SUMS"), []byte(checksums), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verifyRelease(releaseRoot, version, true); err == nil {
		t.Fatal("verifyRelease() accepted non-canonical manifest JSON with matching checksums")
	}
}

func TestValidateSourceRevisionRejectsZeroAndNonCanonicalValues(t *testing.T) {
	for _, revision := range []string{strings.Repeat("0", 40), strings.Repeat("A", 40), "abc"} {
		if err := validateSourceRevision(revision); err == nil {
			t.Fatalf("validateSourceRevision(%q) unexpectedly succeeded", revision)
		}
	}
	if err := validateSourceRevision(strings.Repeat("a", 40)); err != nil {
		t.Fatalf("validateSourceRevision(valid) error = %v", err)
	}
}

func TestRepositoryContainsNoLegacyMacOSDesktopIdentifier(t *testing.T) {
	repositoryRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	legacy := strings.Join([]string{"com", "codesk", "desktop"}, ".")
	command := exec.Command("git", "-C", repositoryRoot, "grep", "-n", "-F", legacy, "--", ".")
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("repository retains legacy macOS desktop identity:\n%s", output)
	}
	exitError, ok := err.(*exec.ExitError)
	if !ok || exitError.ExitCode() != 1 {
		t.Fatalf("git grep failed: %v\n%s", err, output)
	}
}

func TestMacOSKeychainPolicySupportsProfilelessLoginKeychain(t *testing.T) {
	repositoryRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	native, err := os.ReadFile(filepath.Join(repositoryRoot, "daemon", "internal", "desktop", "darwin_native.m"))
	if err != nil {
		t.Fatal(err)
	}
	nativeSource := string(native)
	requireOrderedFragments(t, nativeSource,
		"static NSMutableDictionary *codesk_keychain_query",
		"kSecClassGenericPassword",
		"kSecAttrService",
		"kSecAttrAccount",
		"kSecUseAuthenticationUIFail",
	)
	requireOrderedFragments(t, nativeSource,
		"[NSData dataWithBytesNoCopy",
		"kSecValueData: data",
		"SecItemUpdate",
		"SecItemAdd",
	)
	requireOrderedFragments(t, nativeSource,
		"kSecReturnData",
		"NSData *data = CFBridgingRelease(result)",
	)
	for _, forbidden := range []string{
		"kSecUseDataProtectionKeychain",
		"kSecAttrAccessGroup",
		"kSecAttrAccessible",
		"kSecAttrSynchronizable",
	} {
		if strings.Contains(nativeSource, forbidden) {
			t.Fatalf("profile-less login Keychain policy contains %q", forbidden)
		}
	}

	entitlements, err := os.ReadFile(filepath.Join(repositoryRoot, "daemon", "cmd", "codesk-desktop", "codesk.entitlements"))
	if err != nil {
		t.Fatal(err)
	}
	requireEmptyEntitlementsPlist(t, entitlements)

	for _, relativePath := range []string{
		filepath.Join("docs", "desktop-architecture.md"),
		filepath.Join("scripts", "README.md"),
	} {
		document, err := os.ReadFile(filepath.Join(repositoryRoot, relativePath))
		if err != nil {
			t.Fatal(err)
		}
		normalized := strings.Join(strings.Fields(string(document)), " ")
		requireOrderedFragments(t, normalized,
			"standard per-user login Keychain",
			"does not provide Keychain access-group app isolation",
			"data-protection Keychain requires an Apple Developer App ID, provisioning profile",
		)
		for _, forbidden := range []string{
			"selects the data-protection Keychain",
			"uses the data-protection Keychain",
			"stores it in the data-protection Keychain",
			"kSecAttrAccessible",
		} {
			if strings.Contains(normalized, forbidden) {
				t.Fatalf("%s still claims profile-only Keychain policy %q is active", relativePath, forbidden)
			}
		}
	}
}

func requireEmptyEntitlementsPlist(t *testing.T, data []byte) {
	t.Helper()
	type element struct {
		XMLName  xml.Name
		InnerXML string `xml:",innerxml"`
	}
	var document struct {
		XMLName  xml.Name  `xml:"plist"`
		Version  string    `xml:"version,attr"`
		Children []element `xml:",any"`
	}
	if err := xml.Unmarshal(data, &document); err != nil {
		t.Fatalf("parse codesk.entitlements: %v", err)
	}
	if document.Version != "1.0" {
		t.Fatalf("codesk.entitlements plist version = %q, want 1.0", document.Version)
	}
	if len(document.Children) != 1 || document.Children[0].XMLName.Local != "dict" {
		t.Fatalf("codesk.entitlements must contain exactly one dictionary, got %#v", document.Children)
	}
	if strings.TrimSpace(document.Children[0].InnerXML) != "" {
		t.Fatal("profile-less Codesk release must not declare application or Keychain entitlements")
	}
}

func TestRenderInfoPlistContainsNativeApplicationContract(t *testing.T) {
	version, err := parseReleaseVersion("1.2.3", false)
	if err != nil {
		t.Fatal(err)
	}
	plist := string(renderInfoPlist(version))
	for _, value := range []string{
		"<string>com.getcodesk.desktop</string>",
		"<key>LSMinimumSystemVersion</key>\n\t<string>13.0</string>",
		"<key>LSUIElement</key>\n\t<true/>",
		"<key>LSMultipleInstancesProhibited</key>\n\t<true/>",
		"<key>CFBundleShortVersionString</key>\n\t<string>1.2.3</string>",
	} {
		if !strings.Contains(plist, value) {
			t.Errorf("Info.plist missing %q", value)
		}
	}
}

func TestVerifyAppAndReleaseManifest(t *testing.T) {
	version, err := parseReleaseVersion("1.2.3", false)
	if err != nil {
		t.Fatal(err)
	}
	releaseRoot := t.TempDir()
	appRoot := writeTestApp(t, releaseRoot, version)
	verification, err := verifyApp(appRoot, version)
	if err != nil {
		t.Fatalf("verifyApp() error = %v", err)
	}
	if len(verification.TreeSHA256) != 64 {
		t.Fatalf("tree hash = %q", verification.TreeSHA256)
	}
	dmg := filepath.Join(releaseRoot, diskImageName(version))
	if err := os.WriteFile(dmg, []byte("test disk image"), 0o644); err != nil {
		t.Fatal(err)
	}
	revision := strings.Repeat("a", 40)
	if err := writeManifest(releaseRoot, version, revision, false); err != nil {
		t.Fatalf("writeManifest() error = %v", err)
	}
	if err := verifyRelease(releaseRoot, version, true); err != nil {
		t.Fatalf("verifyRelease() error = %v", err)
	}
	if err := verifyRelease(releaseRoot, version, false); err == nil {
		t.Fatal("verifyRelease() accepted unsigned artifact without override")
	}
	if err := os.WriteFile(dmg, []byte("mutated disk image"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verifyRelease(releaseRoot, version, true); err == nil {
		t.Fatal("verifyRelease() accepted a mutated disk image")
	}
}

func TestVerifyAppRejectsUnexpectedAndSymlinkedEntries(t *testing.T) {
	version, err := parseReleaseVersion("1.0.0", false)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("unexpected file", func(t *testing.T) {
		root := t.TempDir()
		app := writeTestApp(t, root, version)
		if err := os.WriteFile(filepath.Join(app, "Contents", "unexpected"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := verifyApp(app, version); err == nil {
			t.Fatal("verifyApp() accepted an unexpected file")
		}
	})
	t.Run("symlink", func(t *testing.T) {
		root := t.TempDir()
		app := writeTestApp(t, root, version)
		link := filepath.Join(app, "Contents", "Resources", "link")
		if err := os.Symlink("Codesk.icns", link); err != nil {
			t.Fatal(err)
		}
		if _, err := verifyApp(app, version); err == nil {
			t.Fatal("verifyApp() accepted a symlink")
		}
	})
	t.Run("group writable directory", func(t *testing.T) {
		root := t.TempDir()
		app := writeTestApp(t, root, version)
		if err := os.Chmod(filepath.Join(app, "Contents", "Resources"), 0o775); err != nil {
			t.Fatal(err)
		}
		if _, err := verifyApp(app, version); err == nil {
			t.Fatal("verifyApp() accepted a group-writable directory")
		}
	})
}

func TestVerifyAppTreeHashIncludesDirectoryModes(t *testing.T) {
	version, err := parseReleaseVersion("1.0.0", false)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	app := writeTestApp(t, root, version)
	first, err := verifyApp(app, version)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(app, "Contents", "Resources"), 0o700); err != nil {
		t.Fatal(err)
	}
	second, err := verifyApp(app, version)
	if err != nil {
		t.Fatal(err)
	}
	if first.TreeSHA256 == second.TreeSHA256 {
		t.Fatal("application tree hash did not change with a directory mode")
	}
}

func TestVerifyAppRejectsMismatchedMachODeploymentTarget(t *testing.T) {
	version, err := parseReleaseVersion("1.0.0", false)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	app := writeTestApp(t, root, version)
	executable := filepath.Join(app, "Contents", "MacOS", desktopExecutable)
	data, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	const amd64MinimumOffset = 4096 + 44
	binary.LittleEndian.PutUint32(data[amd64MinimumOffset:amd64MinimumOffset+4], uint32(14<<16))
	if err := os.WriteFile(executable, data, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyApp(app, version); err == nil {
		t.Fatal("verifyApp() accepted a Mach-O deployment target that disagrees with Info.plist")
	}
}

func TestVerifyAppRejectsInvalidICNS(t *testing.T) {
	version, err := parseReleaseVersion("1.0.0", false)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	app := writeTestApp(t, root, version)
	icon := filepath.Join(app, "Contents", "Resources", "Codesk.icns")
	if err := os.WriteFile(icon, []byte("not an ICNS file"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyApp(app, version); err == nil {
		t.Fatal("verifyApp() accepted an invalid ICNS file")
	}
}

func TestVerifyAppRejectsCorruptOrTrailingICNSPayload(t *testing.T) {
	version, err := parseReleaseVersion("1.0.0", false)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("corrupt IDAT", func(t *testing.T) {
		root := t.TempDir()
		app := writeTestApp(t, root, version)
		icon := filepath.Join(app, "Contents", "Resources", "Codesk.icns")
		data, err := os.ReadFile(icon)
		if err != nil {
			t.Fatal(err)
		}
		idat := bytes.Index(data, []byte("IDAT"))
		if idat < 4 || idat+4 >= len(data) {
			t.Fatal("test icon has no mutable IDAT payload")
		}
		payloadLength := int(binary.BigEndian.Uint32(data[idat-4 : idat]))
		if payloadLength == 0 || idat+4+payloadLength > len(data) {
			t.Fatal("test icon has an invalid IDAT payload")
		}
		data[idat+4] ^= 0xff
		if err := os.WriteFile(icon, data, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := verifyApp(app, version); err == nil {
			t.Fatal("verifyApp() accepted a corrupt ICNS PNG payload")
		}
	})

	t.Run("trailing PNG payload", func(t *testing.T) {
		root := t.TempDir()
		app := writeTestApp(t, root, version)
		icon := filepath.Join(app, "Contents", "Resources", "Codesk.icns")
		data, err := os.ReadFile(icon)
		if err != nil {
			t.Fatal(err)
		}
		firstChunkLength := int(binary.BigEndian.Uint32(data[12:16]))
		firstChunkEnd := 8 + firstChunkLength
		mutated := make([]byte, 0, len(data)+1)
		mutated = append(mutated, data[:firstChunkEnd]...)
		mutated = append(mutated, 0)
		mutated = append(mutated, data[firstChunkEnd:]...)
		binary.BigEndian.PutUint32(mutated[4:8], uint32(len(mutated)))
		binary.BigEndian.PutUint32(mutated[12:16], uint32(firstChunkLength+1))
		if err := os.WriteFile(icon, mutated, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := verifyApp(app, version); err == nil {
			t.Fatal("verifyApp() accepted trailing bytes inside an ICNS PNG payload")
		}
	})
}

func TestVerifyAppRejectsIncompleteUniversalMachO(t *testing.T) {
	version, err := parseReleaseVersion("1.0.0", false)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	app := writeTestApp(t, root, version)
	executable := filepath.Join(app, "Contents", "MacOS", desktopExecutable)
	data, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	// Keep a valid fat Mach-O and its valid Intel deployment target, but hide
	// the arm64 descriptor. Per-slice checks alone must not accept it.
	binary.BigEndian.PutUint32(data[4:8], 1)
	if err := os.WriteFile(executable, data, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyApp(app, version); err == nil {
		t.Fatal("verifyApp() accepted a fat Mach-O without the exact x86_64+arm64 pair")
	}
}

func TestVerifyAppRejectsFatHeaderSliceIdentityMismatch(t *testing.T) {
	version, err := parseReleaseVersion("1.0.0", false)
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name   string
		mutate func([]byte, int)
	}{
		{
			name: "CPU",
			mutate: func(data []byte, offset int) {
				binary.LittleEndian.PutUint32(data[offset+4:offset+8], uint32(macho.CpuAmd64))
			},
		},
		{
			name: "subtype",
			mutate: func(data []byte, offset int) {
				binary.LittleEndian.PutUint32(data[offset+8:offset+12], 2)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			app := writeTestApp(t, root, version)
			executable := filepath.Join(app, "Contents", "MacOS", desktopExecutable)
			data, err := os.ReadFile(executable)
			if err != nil {
				t.Fatal(err)
			}
			secondSliceOffset := int(binary.BigEndian.Uint32(data[36:40]))
			test.mutate(data, secondSliceOffset)
			if err := os.WriteFile(executable, data, 0o755); err != nil {
				t.Fatal(err)
			}
			if _, err := verifyApp(app, version); err == nil {
				t.Fatalf("verifyApp() accepted a fat-header/thin-slice %s mismatch", test.name)
			}
		})
	}
}

func TestVerifyReleaseRejectsIntegrityMutations(t *testing.T) {
	version, err := parseReleaseVersion("1.2.3", false)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("application changed after manifest", func(t *testing.T) {
		root, app := writeTestRelease(t, version, false)
		executable := filepath.Join(app, "Contents", "MacOS", desktopExecutable)
		data, err := os.ReadFile(executable)
		if err != nil {
			t.Fatal(err)
		}
		data[100] ^= 0xff // Mutate fat-file padding without invalidating either slice.
		if err := os.WriteFile(executable, data, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := verifyApp(app, version); err != nil {
			t.Fatalf("mutated app must remain structurally valid: %v", err)
		}
		if err := verifyRelease(root, version, true); err == nil {
			t.Fatal("verifyRelease() accepted an app whose tree no longer matches the manifest")
		}
	})

	t.Run("canonical checksums changed", func(t *testing.T) {
		root, _ := writeTestRelease(t, version, false)
		if err := os.WriteFile(filepath.Join(root, "SHA256SUMS"), []byte("tampered\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := verifyRelease(root, version, true); err == nil {
			t.Fatal("verifyRelease() accepted non-canonical SHA256SUMS")
		}
	})

	t.Run("unexpected release entry", func(t *testing.T) {
		root, _ := writeTestRelease(t, version, false)
		if err := os.WriteFile(filepath.Join(root, "unexpected.txt"), []byte("unexpected"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := verifyRelease(root, version, true); err == nil {
			t.Fatal("verifyRelease() accepted an unexpected release-root entry")
		}
	})

	t.Run("symlinked release entry", func(t *testing.T) {
		root, _ := writeTestRelease(t, version, false)
		checksumsPath := filepath.Join(root, "SHA256SUMS")
		checksums, err := os.ReadFile(checksumsPath)
		if err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(t.TempDir(), "checksums")
		if err := os.WriteFile(target, checksums, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(checksumsPath); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, checksumsPath); err != nil {
			t.Skipf("symlinks are unavailable: %v", err)
		}
		if err := verifyRelease(root, version, true); err == nil {
			t.Fatal("verifyRelease() accepted a symlinked release-root entry")
		}
	})

	t.Run("manifest identity changed", func(t *testing.T) {
		root, _ := writeTestRelease(t, version, false)
		rewriteTestManifest(t, root, func(manifest *releaseManifest) {
			manifest.Product = "Different Product"
		})
		if err := verifyRelease(root, version, true); err == nil {
			t.Fatal("verifyRelease() accepted a manifest with the wrong product identity")
		}
	})

	t.Run("bundle identity changed", func(t *testing.T) {
		root, _ := writeTestRelease(t, version, false)
		rewriteTestManifest(t, root, func(manifest *releaseManifest) {
			manifest.BundleIdentifier = strings.Join([]string{"com", "codesk", "desktop"}, ".")
		})
		if err := verifyRelease(root, version, true); err == nil {
			t.Fatal("verifyRelease() accepted a manifest with the legacy bundle identity")
		}
	})

	t.Run("source revision is not a canonical Git SHA", func(t *testing.T) {
		root, _ := writeTestRelease(t, version, false)
		rewriteTestManifest(t, root, func(manifest *releaseManifest) {
			manifest.SourceRevision = strings.Repeat("0", 40)
		})
		if err := verifyRelease(root, version, true); err == nil {
			t.Fatal("verifyRelease() accepted a manifest with an invalid source revision")
		}
	})
}

func TestVerifyReleaseRejectsSignedDevelopmentArtifact(t *testing.T) {
	version, err := parseReleaseVersion("dev", true)
	if err != nil {
		t.Fatal(err)
	}
	root, _ := writeTestRelease(t, version, true)
	if err := verifyRelease(root, version, true); err == nil {
		t.Fatal("verifyRelease() accepted a development artifact marked signed")
	}
}

func TestVerifyAppRejectsNonCanonicalMetadataAndModes(t *testing.T) {
	version, err := parseReleaseVersion("1.0.0", false)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("Info.plist bytes", func(t *testing.T) {
		root := t.TempDir()
		app := writeTestApp(t, root, version)
		plist := filepath.Join(app, "Contents", "Info.plist")
		data, err := os.ReadFile(plist)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(plist, append(data, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := verifyApp(app, version); err == nil {
			t.Fatal("verifyApp() accepted non-canonical Info.plist bytes")
		}
	})

	for _, executable := range []struct {
		name string
		path func(string) string
	}{
		{name: "application", path: func(app string) string { return filepath.Join(app, "Contents", "MacOS", desktopExecutable) }},
		{name: "helper", path: func(app string) string { return filepath.Join(app, "Contents", "Helpers", agentToolExecutable) }},
	} {
		t.Run(executable.name+" executable bit", func(t *testing.T) {
			root := t.TempDir()
			app := writeTestApp(t, root, version)
			if err := os.Chmod(executable.path(app), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := verifyApp(app, version); err == nil {
				t.Fatalf("verifyApp() accepted a non-executable %s", executable.name)
			}
		})
	}
}

func TestVerifyMountedVolumeRequiresExactInventory(t *testing.T) {
	validVolume := func(t *testing.T) string {
		t.Helper()
		root := t.TempDir()
		if err := os.Mkdir(filepath.Join(root, applicationName), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("/Applications", filepath.Join(root, "Applications")); err != nil {
			t.Fatal(err)
		}
		return root
	}

	t.Run("valid", func(t *testing.T) {
		if err := verifyMountedVolume(validVolume(t)); err != nil {
			t.Fatalf("verifyMountedVolume() error = %v", err)
		}
	})
	t.Run("unexpected entry", func(t *testing.T) {
		root := validVolume(t)
		if err := os.WriteFile(filepath.Join(root, ".unexpected"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := verifyMountedVolume(root); err == nil {
			t.Fatal("verifyMountedVolume() accepted an unexpected entry")
		}
	})
	t.Run("symlinked app", func(t *testing.T) {
		root := validVolume(t)
		if err := os.Remove(filepath.Join(root, applicationName)); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(t.TempDir(), filepath.Join(root, applicationName)); err != nil {
			t.Fatal(err)
		}
		if err := verifyMountedVolume(root); err == nil {
			t.Fatal("verifyMountedVolume() accepted a symlinked application")
		}
	})
	t.Run("wrong Applications target", func(t *testing.T) {
		root := validVolume(t)
		if err := os.Remove(filepath.Join(root, "Applications")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("/tmp", filepath.Join(root, "Applications")); err != nil {
			t.Fatal(err)
		}
		if err := verifyMountedVolume(root); err == nil {
			t.Fatal("verifyMountedVolume() accepted the wrong Applications target")
		}
	})
}

func TestVerifyScriptRejectsMountedVolumeMutations(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	repositoryRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	fakeBin := t.TempDir()
	writeTestCommand(t, fakeBin, "uname", "#!/bin/sh\nprintf 'Darwin\\n'\n")
	writeTestCommand(t, fakeBin, "go", `#!/bin/sh
set -eu
if [ "$1" = env ] && [ "$2" = GOVERSION ]; then
    printf 'go1.26.5\n'
    exit 0
fi
if [ "$1" = build ]; then
    shift
    output=''
    while [ "$#" -gt 0 ]; do
        case "$1" in
            -o) output="$2"; shift 2 ;;
            *) shift ;;
        esac
    done
    cat >"$output" <<'TOOL'
#!/bin/sh
set -eu
command="$1"
shift
case "$command" in
    verify) exit 0 ;;
    verify-volume)
        mount=''
        while [ "$#" -gt 0 ]; do
            case "$1" in
                --mount) mount="$2"; shift 2 ;;
                *) shift ;;
            esac
        done
        if [ -e "$mount/unexpected" ]; then
            printf '%s\n' 'codesk macOS release: unexpected mounted volume entry "unexpected"' >&2
            exit 1
        fi
        ;;
    verify-app)
        app=''
        while [ "$#" -gt 0 ]; do
            case "$1" in
                --app) app="$2"; shift 2 ;;
                *) shift ;;
            esac
        done
        case "$app:${CODESK_TEST_TREE_MISMATCH:-}" in
            */mount/Codesk.app:1) printf 'mounted-tree\n' ;;
            *) printf 'source-tree\n' ;;
        esac
        ;;
    *) exit 2 ;;
esac
TOOL
    chmod 0755 "$output"
    exit 0
fi
exit 2
`)
	writeTestCommand(t, fakeBin, "hdiutil", `#!/bin/sh
set -eu
command="$1"
shift
case "$command" in
    verify|detach) exit 0 ;;
    attach)
        mount=''
        while [ "$#" -gt 0 ]; do
            if [ "$1" = -mountpoint ]; then
                mount="$2"
                shift 2
            else
                shift
            fi
        done
        mkdir -p "$mount/Codesk.app"
        ln -s /Applications "$mount/Applications"
        if [ "${CODESK_TEST_EXTRA_MOUNT_ENTRY:-}" = 1 ]; then
            : >"$mount/unexpected"
        fi
        ;;
    *) exit 2 ;;
esac
`)
	writeTestCommand(t, fakeBin, "plutil", `#!/bin/sh
set -eu
[ "$1" = -extract ] && [ "$2" = signed_and_notarized ] || exit 2
printf '%s\n' "${CODESK_TEST_MANIFEST_SIGNED:-false}"
`)
	writeTestCommand(t, fakeBin, "codesign", `#!/bin/sh
set -eu
if [ "${CODESK_TEST_TRUST_FAIL:-}" = 1 ]; then
    printf '%s\n' 'signed-manifest trust check executed' >&2
    exit 37
fi
exit 0
`)
	for _, command := range []string{"spctl", "xcrun"} {
		writeTestCommand(t, fakeBin, command, "#!/bin/sh\nexit 0\n")
	}

	releaseRoot := t.TempDir()
	if err := os.Mkdir(filepath.Join(releaseRoot, applicationName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(releaseRoot, "Codesk_1.2.3_macos_universal.dmg"), []byte("dmg"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(releaseRoot, "manifest.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name        string
		environment string
		want        string
	}{
		{name: "application tree mismatch", environment: "CODESK_TEST_TREE_MISMATCH=1", want: "DMG application tree differs from the release app"},
		{name: "unexpected volume entry", environment: "CODESK_TEST_EXTRA_MOUNT_ENTRY=1", want: "unexpected mounted volume entry"},
	} {
		t.Run(test.name, func(t *testing.T) {
			command := exec.Command("/bin/sh", filepath.Join(repositoryRoot, "scripts", "verify-macos-desktop-release.sh"), releaseRoot, "1.2.3")
			command.Env = append(os.Environ(),
				"ALLOW_UNSIGNED_MACOS_DESKTOP=1",
				test.environment,
				"PATH="+fakeBin+string(os.PathListSeparator)+"/usr/bin"+string(os.PathListSeparator)+"/bin",
			)
			output, err := command.CombinedOutput()
			if err == nil {
				t.Fatalf("verify script accepted %s:\n%s", test.name, output)
			}
			if !bytes.Contains(output, []byte(test.want)) {
				t.Fatalf("verify script failed for the wrong reason: %v\n%s", err, output)
			}
		})
	}

	t.Run("signed manifest cannot suppress trust checks with unsigned relaxation", func(t *testing.T) {
		command := exec.Command("/bin/sh", filepath.Join(repositoryRoot, "scripts", "verify-macos-desktop-release.sh"), releaseRoot, "1.2.3")
		command.Env = append(os.Environ(),
			"ALLOW_UNSIGNED_MACOS_DESKTOP=1",
			"CODESK_TEST_MANIFEST_SIGNED=true",
			"CODESK_TEST_TRUST_FAIL=1",
			"PATH="+fakeBin+string(os.PathListSeparator)+"/usr/bin"+string(os.PathListSeparator)+"/bin",
		)
		output, err := command.CombinedOutput()
		if err == nil {
			t.Fatalf("verify script skipped signed-manifest trust obligations:\n%s", output)
		}
		if !bytes.Contains(output, []byte("signed-manifest trust check executed")) {
			t.Fatalf("verify script failed before the signed-manifest trust check: %v\n%s", err, output)
		}
	})
}

func TestMacOSBuildScriptStagesHostArchiveAndUsesFileFirstLipo(t *testing.T) {
	repositoryRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(repositoryRoot, "scripts", "build-macos-desktop-release.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	requireOrderedFragments(t, script,
		"build-macos-desktop-release: staging host yffi library from a clean locked build",
		`"$root_dir/scripts/build-yffi.sh"`,
		"go test ./daemon/cmd/codesk-macos-release",
	)
	for _, want := range []string{
		`"$lipo" "$desktop" -verify_arch x86_64 arm64`,
		`"$lipo" "$agent_tool" -verify_arch x86_64 arm64`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("build script does not use file-first lipo verification %q", want)
		}
	}
	if strings.Contains(script, `"$lipo" -verify_arch x86_64 arm64`) {
		t.Fatal("build script still uses the invalid architecture-first lipo syntax")
	}
	workflow, err := os.ReadFile(filepath.Join(repositoryRoot, ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(workflow, []byte(`lipo -verify_arch x86_64 arm64`)) {
		t.Fatal("macOS CI still uses the invalid architecture-first lipo syntax")
	}
	if bytes.Count(workflow, []byte(`-verify_arch x86_64 arm64`)) < 2 {
		t.Fatal("macOS CI does not verify both universal executables")
	}
	requireOrderedFragments(t, string(workflow),
		"rm -rf third_party/y-crdt/target",
		`scripts/build-macos-desktop-release.sh 0.0.0`,
	)
}

func TestTestTmpCanonicalizesTrailingSlashRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	repositoryRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command("/bin/sh", "-c", `. "$1"; notty_test_mktemp canonical`, "sh", filepath.Join(repositoryRoot, "scripts", "lib", "testtmp.sh"))
	command.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"TMPDIR=" + t.TempDir() + string(os.PathSeparator),
	}
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	created := strings.TrimSpace(string(output))
	t.Cleanup(func() { _ = os.RemoveAll(created) })
	if created != filepath.Clean(created) || !filepath.IsAbs(created) {
		t.Fatalf("notty_test_mktemp() = %q, want an absolute clean path", created)
	}
	resolved, err := filepath.EvalSymlinks(created)
	if err != nil {
		t.Fatal(err)
	}
	if created != resolved {
		t.Fatalf("notty_test_mktemp() = %q, want canonical %q", created, resolved)
	}
}

func TestNativeAcceptanceHarnessCausalBindings(t *testing.T) {
	repositoryRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(repositoryRoot, "scripts", "test-macos-desktop-native.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	t.Run("unsigned functional evidence never claims trust", func(t *testing.T) {
		requireOrderedFragments(t, script,
			`case "${CODESK_MACOS_ACCEPT_UNSIGNED_FUNCTIONAL:-}" in`,
			`evidence_scope='native-functional-only'`,
			`artifact_trust='NOT_ESTABLISHED'`,
			`publishable='false'`,
			`PASS complete native macOS functional-only evidence=$evidence_dir artifact_trust=NOT_ESTABLISHED publishable=false`,
		)
	})
	t.Run("prepare quits every process before logout", func(t *testing.T) {
		requireOrderedFragments(t, script,
			`wait_login_enabled`,
			`write_value prepare-app-pid "$app_pid"`,
			`normal_quit`,
			`[ -z "$(codesk_all_pids)" ] || fail 'Codesk was still running at the login-cycle boundary'`,
			`write_value stage awaiting-login`,
		)
	})
	t.Run("external commands have a finite watchdog", func(t *testing.T) {
		requireOrderedFragments(t, script,
			`run_bounded_to_log() {`,
			`"$@" >>"$log" 2>&1 &`,
			`command_pid=$!`,
			`sleep "$timeout_seconds"`,
			`if kill -0 "$command_pid" 2>/dev/null; then`,
			`kill -TERM "$command_pid"`,
			`kill -KILL "$command_pid"`,
			`[ ! -e "$timeout_marker" ] || fail "$label exceeded CODESK_MACOS_ACCEPT_TIMEOUT=${timeout_seconds}s"`,
		)
	})
	t.Run("restart advances the online service generation", func(t *testing.T) {
		requireOrderedFragments(t, script,
			`generation_before_restart="$(latest_online_service_generation)"`,
			`menu_click 'Restart daemon'`,
			`generation_after_restart="$(wait_for_new_online_service_generation "$generation_before_restart")"`,
			`[ "$(wait_for_single_app)" = "$candidate_pid" ] || fail 'Restart daemon replaced the desktop process'`,
			`run_sync_stage restart`,
			`PASS Restart daemon advanced online service generation $generation_before_restart->$generation_after_restart`,
		)
	})
	requireOrderedFragments(t, script,
		`assert_dmg_identity "$dmg" "$expected_dmg_hash" "$expected_dmg_size"`,
		`hdiutil attach -quiet -readonly -nobrowse -mountpoint "$mount_dir" "$dmg"`,
		`[ "$mounted_tree_hash" = "$expected_tree_hash" ]`,
		`verify_installed_app "$version" "$expected_tree_hash"`,
	)
	requireOrderedFragments(t, script,
		`run_driver_action "$sync_driver" "$sync_stage" plan '' ''`,
		`[ ! -e "$local_path" ] && [ ! -L "$local_path" ]`,
		`run_driver_action "$sync_driver" "$sync_stage" trigger "$relative" "$expected_hash"`,
		`remote-to-local sync stage=$sync_stage`,
	)
	requireOrderedFragments(t, script,
		`provider_pids_before="$tmp_dir/$provider_stage-pids-before"`,
		`provider PID existed before its trigger`,
		`provider_actual_paths="$(lsof`,
		`provider did not expose exactly one program-text executable`,
		`provider executable hash differs from the driver claim`,
		`provider ancestry is not rooted in Codesk`,
		`provider started before its trigger`,
	)
}

func requireOrderedFragments(t *testing.T, value string, fragments ...string) {
	t.Helper()
	position := 0
	for _, fragment := range fragments {
		index := strings.Index(value[position:], fragment)
		if index < 0 {
			t.Fatalf("missing ordered fragment %q after byte %d", fragment, position)
		}
		position += index + len(fragment)
	}
}

func writeTestRelease(t *testing.T, version releaseVersion, signed bool) (string, string) {
	t.Helper()
	root := t.TempDir()
	app := writeTestApp(t, root, version)
	if err := os.WriteFile(filepath.Join(root, diskImageName(version)), []byte("test disk image"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeManifest(root, version, strings.Repeat("a", 40), signed); err != nil {
		t.Fatal(err)
	}
	return root, app
}

func rewriteTestManifest(t *testing.T, root string, mutate func(*releaseManifest)) {
	t.Helper()
	manifestPath := filepath.Join(root, "manifest.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest releaseManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	mutate(&manifest)
	data, err = encodeManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	dmg, err := inspectArtifact(filepath.Join(root, manifest.DiskImage.Path))
	if err != nil {
		t.Fatal(err)
	}
	manifestHash := sha256.Sum256(data)
	checksums := fmt.Sprintf("%s  %s\n%s  manifest.json\n", dmg.SHA256, dmg.Path, hex.EncodeToString(manifestHash[:]))
	if err := os.WriteFile(filepath.Join(root, "SHA256SUMS"), []byte(checksums), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeTestCommand(t *testing.T, directory, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(directory, name), []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func writeTestApp(t *testing.T, releaseRoot string, version releaseVersion) string {
	t.Helper()
	app := filepath.Join(releaseRoot, applicationName)
	contents := filepath.Join(app, "Contents")
	macOSDir := filepath.Join(contents, "MacOS")
	helpersDir := filepath.Join(contents, "Helpers")
	resourcesDir := filepath.Join(contents, "Resources")
	for _, directory := range []string{macOSDir, helpersDir, resourcesDir} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(contents, "Info.plist"), renderInfoPlist(version), 0o644); err != nil {
		t.Fatal(err)
	}
	writeTestICNS(t, filepath.Join(resourcesDir, "Codesk.icns"))
	writeTestUniversalMachO(t, filepath.Join(macOSDir, desktopExecutable))
	writeTestUniversalMachO(t, filepath.Join(helpersDir, agentToolExecutable))
	return app
}

func writeTestICNS(t *testing.T, path string) {
	t.Helper()
	var body bytes.Buffer
	for tag, size := range expectedICNSChunks {
		imageOut := image.NewNRGBA(image.Rect(0, 0, size, size))
		imageOut.SetNRGBA(0, 0, color.NRGBA{A: 0xff})
		var encoded bytes.Buffer
		if err := png.Encode(&encoded, imageOut); err != nil {
			t.Fatal(err)
		}
		body.WriteString(tag)
		if err := binary.Write(&body, binary.BigEndian, uint32(8+encoded.Len())); err != nil {
			t.Fatal(err)
		}
		body.Write(encoded.Bytes())
	}
	var output bytes.Buffer
	output.WriteString("icns")
	if err := binary.Write(&output, binary.BigEndian, uint32(8+body.Len())); err != nil {
		t.Fatal(err)
	}
	output.Write(body.Bytes())
	if err := os.WriteFile(path, output.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeTestUniversalMachO(t *testing.T, path string) {
	t.Helper()
	const (
		firstOffset  = 4096
		secondOffset = 8192
		thinSize     = 56
	)
	data := make([]byte, secondOffset+thinSize)
	binary.BigEndian.PutUint32(data[0:4], 0xcafebabe)
	binary.BigEndian.PutUint32(data[4:8], 2)
	writeFatArch(data[8:28], macho.CpuAmd64, 3, firstOffset, thinSize)
	writeFatArch(data[28:48], macho.CpuArm64, 0, secondOffset, thinSize)
	writeThinMachO(data[firstOffset:firstOffset+thinSize], macho.CpuAmd64, 3)
	writeThinMachO(data[secondOffset:secondOffset+thinSize], macho.CpuArm64, 0)
	if err := os.WriteFile(path, data, 0o755); err != nil {
		t.Fatal(err)
	}
}

func writeFatArch(data []byte, cpu macho.Cpu, subCPU uint32, offset, size uint32) {
	binary.BigEndian.PutUint32(data[0:4], uint32(cpu))
	binary.BigEndian.PutUint32(data[4:8], subCPU)
	binary.BigEndian.PutUint32(data[8:12], offset)
	binary.BigEndian.PutUint32(data[12:16], size)
	binary.BigEndian.PutUint32(data[16:20], 12)
}

func writeThinMachO(data []byte, cpu macho.Cpu, subCPU uint32) {
	binary.LittleEndian.PutUint32(data[0:4], uint32(macho.Magic64))
	binary.LittleEndian.PutUint32(data[4:8], uint32(cpu))
	binary.LittleEndian.PutUint32(data[8:12], subCPU)
	binary.LittleEndian.PutUint32(data[12:16], uint32(macho.TypeExec))
	binary.LittleEndian.PutUint32(data[16:20], 1)
	binary.LittleEndian.PutUint32(data[20:24], 24)
	// flags and the 64-bit reserved word remain zero.
	binary.LittleEndian.PutUint32(data[32:36], 0x32) // LC_BUILD_VERSION
	binary.LittleEndian.PutUint32(data[36:40], 24)
	binary.LittleEndian.PutUint32(data[40:44], 1) // PLATFORM_MACOS
	binary.LittleEndian.PutUint32(data[44:48], minimumMacOSPacked)
	binary.LittleEndian.PutUint32(data[48:52], minimumMacOSPacked)
	// ntools remains zero.
}
