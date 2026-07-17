package main

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const wixNamespace = "http://wixtoolset.org/schemas/v4/wxs"

type wixElement struct {
	XMLName xml.Name
	Attrs   []xml.Attr   `xml:",any,attr"`
	Nodes   []wixElement `xml:",any"`
	Text    string       `xml:",chardata"`
}

func TestWindowsInstallerCIUsesArchitectureBoundProductPayloads(t *testing.T) {
	path := filepath.Join("..", "..", "..", ".github", "workflows", "ci.yml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(data)
	for source, count := range map[string]int{
		"uses: actions/upload-artifact@v4":                                                                  2,
		"uses: actions/download-artifact@v4":                                                                1,
		"name: windows-desktop-payload-amd64":                                                               1,
		"name: windows-desktop-payload-arm64":                                                               1,
		"name: windows-desktop-payload-${{ matrix.go_arch }}":                                               1,
		"needs: windows-daemon-build":                                                                       1,
		`-o "$payload_dir/Codesk.exe" ./daemon/cmd/codesk-desktop`:                                          1,
		`-o "$payload_dir/notty-agent-tool.exe" ./daemon/cmd/agenttool`:                                     1,
		`go run ./scripts/verify-windows-desktop-pe.go "$payload_dir/Codesk.exe" "$arch" gui`:               1,
		`go run ./scripts/verify-windows-desktop-pe.go "$payload_dir/notty-agent-tool.exe" "$arch" console`: 1,
		"path: ${{ runner.temp }}/windows-desktop-payload/amd64/":                                           1,
		"path: ${{ runner.temp }}/windows-desktop-payload/arm64/":                                           1,
		`(Join-Path $payload "Codesk.exe")`:                                                                 1,
		`(Join-Path $payload "notty-agent-tool.exe")`:                                                       1,
		`"-p:ProductVersion=not-a-valid-msi-version"`:                                                       1,
		`throw "WiX accepted the compiler-only invalid ProductVersion mutation"`:                            1,
	} {
		if got := strings.Count(workflow, source); got != count {
			t.Errorf("CI source count for %q = %d, want %d", source, got, count)
		}
	}
	for _, placeholder := range []string{"WriteAllBytes", "[byte[]](0x4d, 0x5a)"} {
		if strings.Contains(workflow, placeholder) {
			t.Errorf("CI installer payload must not use placeholder construction %q", placeholder)
		}
	}
}

func TestWindowsInstallerProjectPinsWiXBuildInputs(t *testing.T) {
	path := filepath.Join("..", "..", "..", "deploy", "windows-desktop", "Codesk.wixproj")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var root wixElement
	if err := xml.Unmarshal(data, &root); err != nil {
		t.Fatalf("parse WiX project: %v", err)
	}
	if root.XMLName.Local != "Project" || root.XMLName.Space != "" {
		t.Fatalf("unexpected WiX project root: {%s}%s", root.XMLName.Space, root.XMLName.Local)
	}
	assertWixAttrs(t, root, map[string]string{"Sdk": "WixToolset.Sdk/4.0.5"})
	assertWixElementCounts(t, root, map[string]int{
		"Project": 1, "PropertyGroup": 1, "OutputType": 1, "Platform": 1, "DefineConstants": 1,
	})
	if len(root.Nodes) != 1 {
		t.Fatalf("WiX project has %d root elements, want exactly PropertyGroup", len(root.Nodes))
	}
	propertyGroup, err := wixDirectChild(root, "PropertyGroup")
	if err != nil {
		t.Fatal(err)
	}
	assertWixAttrs(t, propertyGroup, map[string]string{})
	if len(propertyGroup.Nodes) != 3 {
		t.Fatalf("WiX PropertyGroup has %d elements, want exactly 3", len(propertyGroup.Nodes))
	}
	for name, value := range map[string]string{
		"OutputType": "Package",
		"Platform":   "$(InstallerPlatform)",
	} {
		element, err := wixDirectChild(propertyGroup, name)
		if err != nil {
			t.Fatal(err)
		}
		assertWixAttrs(t, element, map[string]string{})
		if len(element.Nodes) != 0 {
			t.Fatalf("%s has %d nested elements, want none", name, len(element.Nodes))
		}
		if got := strings.TrimSpace(element.Text); got != value {
			t.Errorf("%s text = %q, want %q", name, got, value)
		}
	}
	constants, err := wixDirectChild(propertyGroup, "DefineConstants")
	if err != nil {
		t.Fatal(err)
	}
	assertWixAttrs(t, constants, map[string]string{})
	if len(constants.Nodes) != 0 {
		t.Fatalf("DefineConstants has %d nested elements, want none", len(constants.Nodes))
	}
	assertWixDefinitions(t, constants.Text, map[string]string{
		"ProductVersion":     "$(ProductVersion)",
		"VersionProductCode": "$(VersionProductCode)",
		"CodeskExe":          "$(CodeskExe)",
		"AgentToolExe":       "$(AgentToolExe)",
		"CodeskIcon":         "$(CodeskIcon)",
	})
}

func TestWindowsInstallerIsThinPerUserWiXPackage(t *testing.T) {
	path := filepath.Join("..", "..", "..", "deploy", "windows-desktop", "Package.wxs")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	assertNoInstallerDirectives(t, data)
	var root wixElement
	if err := xml.Unmarshal(data, &root); err != nil {
		t.Fatalf("parse WiX package: %v", err)
	}
	if root.XMLName.Local != "Wix" || root.XMLName.Space != wixNamespace {
		t.Fatalf("unexpected WiX root: {%s}%s", root.XMLName.Space, root.XMLName.Local)
	}
	assertWixAttrs(t, root, map[string]string{"RequiredVersion": "4.0.0"})
	assertWixVocabulary(t, root, map[string]struct{}{
		"Wix": {}, "Package": {}, "MajorUpgrade": {}, "MediaTemplate": {},
		"Icon": {}, "Property": {}, "StandardDirectory": {}, "Directory": {},
		"Component": {}, "File": {}, "Shortcut": {}, "RemoveFolder": {},
		"RegistryValue": {}, "Feature": {}, "ComponentRef": {},
	})
	assertWixElementCounts(t, root, map[string]int{
		"Wix": 1, "Package": 1, "MajorUpgrade": 1, "MediaTemplate": 1,
		"Icon": 1, "Property": 1, "StandardDirectory": 2, "Directory": 3,
		"Component": 3, "File": 2, "Shortcut": 1, "RemoveFolder": 1,
		"RegistryValue": 1, "Feature": 1, "ComponentRef": 3,
	})
	assertWixTopology(t, root)

	packages := wixElements(root, "Package")
	if len(packages) != 1 {
		t.Fatalf("got %d Package elements, want 1", len(packages))
	}
	packageElement := packages[0]
	assertWixAttrs(t, packageElement, map[string]string{
		"Name":             "Codesk",
		"Manufacturer":     "Codesk",
		"Version":          "$(ProductVersion)",
		"ProductCode":      "$(VersionProductCode)",
		"Language":         "1033",
		"UpgradeCode":      "0C8C0BBA-06EE-43BA-BC34-768B9B740A09",
		"Scope":            "perUser",
		"InstallerVersion": "500",
		"Compressed":       "yes",
	})

	if got := len(wixElements(root, "CustomAction")); got != 0 {
		t.Fatalf("custom installer actions are not allowed, got %d", got)
	}
	assertWixAttrs(t, wixElements(root, "MajorUpgrade")[0], map[string]string{
		"DowngradeErrorMessage": "A newer version of Codesk is already installed.",
		"Schedule":              "afterInstallExecute",
	})
	assertWixAttrs(t, wixElements(root, "MediaTemplate")[0], map[string]string{
		"EmbedCab":         "yes",
		"CompressionLevel": "high",
	})
	assertWixAttrs(t, assertWixElementByID(t, root, "StandardDirectory", "LocalAppDataFolder"), map[string]string{
		"Id": "LocalAppDataFolder",
	})
	assertWixAttrs(t, assertWixElementByID(t, root, "StandardDirectory", "ProgramMenuFolder"), map[string]string{
		"Id": "ProgramMenuFolder",
	})
	assertWixAttrs(t, assertWixElementByID(t, root, "Directory", "PerUserProgramsFolder"), map[string]string{
		"Id":   "PerUserProgramsFolder",
		"Name": "Programs",
	})
	assertWixAttrs(t, assertWixElementByID(t, root, "Directory", "INSTALLFOLDER"), map[string]string{
		"Id":   "INSTALLFOLDER",
		"Name": "Codesk",
	})
	assertWixAttrs(t, assertWixElementByID(t, root, "Directory", "CodeskProgramsFolder"), map[string]string{
		"Id":   "CodeskProgramsFolder",
		"Name": "Codesk",
	})
	assertWixAttrs(t, assertWixElementByID(t, root, "Component", "CodeskExecutable"), map[string]string{
		"Id":   "CodeskExecutable",
		"Guid": "*",
	})
	assertWixAttrs(t, assertWixElementByID(t, root, "Component", "AgentToolExecutable"), map[string]string{
		"Id":   "AgentToolExecutable",
		"Guid": "*",
	})
	assertWixAttrs(t, assertWixElementByID(t, root, "Component", "CodeskLoginItemCleanup"), map[string]string{
		"Id":             "CodeskLoginItemCleanup",
		"Guid":           "A11ADE55-B9B8-45E9-9DAB-60203C2A824E",
		"NeverOverwrite": "yes",
	})

	codeskFile := assertWixElementByID(t, root, "File", "CodeskExe")
	assertWixAttrs(t, codeskFile, map[string]string{
		"Id":      "CodeskExe",
		"Source":  "$(CodeskExe)",
		"KeyPath": "yes",
	})
	shortcut := assertWixElementByID(t, codeskFile, "Shortcut", "CodeskStartMenuShortcut")
	assertWixAttrs(t, shortcut, map[string]string{
		"Id":               "CodeskStartMenuShortcut",
		"Directory":        "CodeskProgramsFolder",
		"Name":             "Codesk",
		"Description":      "Codesk desktop app",
		"WorkingDirectory": "INSTALLFOLDER",
		"Icon":             "Codesk.ico",
	})
	agentFile := assertWixElementByID(t, root, "File", "AgentToolExe")
	assertWixAttrs(t, agentFile, map[string]string{
		"Id":      "AgentToolExe",
		"Name":    "notty-agent-tool.exe",
		"Source":  "$(AgentToolExe)",
		"KeyPath": "yes",
	})
	loginItem := assertWixElementByID(t, root, "RegistryValue", "")
	assertWixAttrs(t, loginItem, map[string]string{
		"Root":    "HKCU",
		"Key":     `Software\Microsoft\Windows\CurrentVersion\Run`,
		"Name":    "Codesk",
		"Type":    "string",
		"Value":   "",
		"KeyPath": "yes",
	})
	removeFolder := assertWixElementByID(t, root, "RemoveFolder", "RemoveCodeskProgramsFolder")
	assertWixAttrs(t, removeFolder, map[string]string{
		"Id":        "RemoveCodeskProgramsFolder",
		"Directory": "CodeskProgramsFolder",
		"On":        "uninstall",
	})

	icon := assertWixElementByID(t, root, "Icon", "Codesk.ico")
	assertWixAttrs(t, icon, map[string]string{
		"Id":         "Codesk.ico",
		"SourceFile": "$(CodeskIcon)",
	})
	assertWixAttrs(t, assertWixElementByID(t, root, "Property", "ARPPRODUCTICON"), map[string]string{
		"Id":    "ARPPRODUCTICON",
		"Value": "Codesk.ico",
	})
	assertWixAttrs(t, assertWixElementByID(t, root, "Feature", "CodeskFeature"), map[string]string{
		"Id":    "CodeskFeature",
		"Title": "Codesk",
		"Level": "1",
	})
	assertWixAttrs(t, assertWixElementByID(t, root, "ComponentRef", "CodeskExecutable"), map[string]string{
		"Id": "CodeskExecutable",
	})
	assertWixAttrs(t, assertWixElementByID(t, root, "ComponentRef", "AgentToolExecutable"), map[string]string{
		"Id": "AgentToolExecutable",
	})
	assertWixAttrs(t, assertWixElementByID(t, root, "ComponentRef", "CodeskLoginItemCleanup"), map[string]string{
		"Id": "CodeskLoginItemCleanup",
	})
}

func TestWindowsInstallerRejectsUnexpectedElementAttributes(t *testing.T) {
	expected := map[string]string{
		"Id":   "CodeskExecutable",
		"Guid": "*",
	}
	var baseline wixElement
	if err := xml.Unmarshal([]byte(`<Component xmlns="`+wixNamespace+`" Id="CodeskExecutable" Guid="*" />`), &baseline); err != nil {
		t.Fatal(err)
	}
	if err := checkWixAttrs(baseline, expected); err != nil {
		t.Fatalf("valid component failed the exact WiX attribute contract: %v", err)
	}

	var mutated wixElement
	if err := xml.Unmarshal([]byte(`<Component xmlns="`+wixNamespace+`" Id="CodeskExecutable" Guid="*" Permanent="yes" />`), &mutated); err != nil {
		t.Fatal(err)
	}
	if err := checkWixAttrs(mutated, expected); err == nil {
		t.Fatal("unexpected Permanent attribute passed the exact WiX attribute contract")
	} else if !strings.Contains(err.Error(), "Permanent") {
		t.Fatalf("unexpected-attribute mutation failed for the wrong reason: %v", err)
	}
}

func TestWindowsInstallerRejectsRelocatedAgentComponent(t *testing.T) {
	path := filepath.Join("..", "..", "..", "deploy", "windows-desktop", "Package.wxs")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	agentComponent := []byte(`          <Component Id="AgentToolExecutable" Guid="*">
            <File Id="AgentToolExe" Name="notty-agent-tool.exe" Source="$(AgentToolExe)" KeyPath="yes" />
          </Component>
`)
	if got := bytes.Count(data, agentComponent); got != 1 {
		t.Fatalf("got %d canonical agent component blocks, want 1", got)
	}
	mutated := bytes.Replace(data, agentComponent, nil, 1)
	menuDirectory := []byte("      <Directory Id=\"CodeskProgramsFolder\" Name=\"Codesk\" />\n")
	if got := bytes.Count(mutated, menuDirectory); got != 1 {
		t.Fatalf("got %d canonical menu directory elements, want 1", got)
	}
	relocatedMenuDirectory := append([]byte("      <Directory Id=\"CodeskProgramsFolder\" Name=\"Codesk\">\n"), agentComponent...)
	relocatedMenuDirectory = append(relocatedMenuDirectory, []byte("      </Directory>\n")...)
	mutated = bytes.Replace(mutated, menuDirectory, relocatedMenuDirectory, 1)

	var root wixElement
	if err := xml.Unmarshal(mutated, &root); err != nil {
		t.Fatal(err)
	}
	if err := checkWixTopology(root); err == nil {
		t.Fatal("relocated AgentToolExecutable component passed the exact WiX topology contract")
	} else if !strings.Contains(err.Error(), "AgentToolExecutable") {
		t.Fatalf("component-relocation mutation failed for the wrong reason: %v", err)
	}
}

func assertNoInstallerDirectives(t *testing.T, data []byte) {
	t.Helper()
	decoder := xml.NewDecoder(bytes.NewReader(data))
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return
		}
		if err != nil {
			t.Fatalf("scan WiX package tokens: %v", err)
		}
		switch token := token.(type) {
		case xml.Directive:
			t.Errorf("installer directives are not allowed: %q", token)
		case xml.ProcInst:
			if token.Target != "xml" {
				t.Errorf("installer processing instruction %q is not allowed", token.Target)
			}
		}
	}
}

func wixElements(root wixElement, name string) []wixElement {
	var result []wixElement
	if root.XMLName.Local == name {
		result = append(result, root)
	}
	for _, child := range root.Nodes {
		result = append(result, wixElements(child, name)...)
	}
	return result
}

func assertWixVocabulary(t *testing.T, root wixElement, allowed map[string]struct{}) {
	t.Helper()
	if root.XMLName.Space != wixNamespace {
		t.Errorf("element %s uses namespace %q", root.XMLName.Local, root.XMLName.Space)
	}
	if _, ok := allowed[root.XMLName.Local]; !ok {
		t.Errorf("installer element %s is outside the thin MSI vocabulary", root.XMLName.Local)
	}
	for _, child := range root.Nodes {
		assertWixVocabulary(t, child, allowed)
	}
}

func assertWixElementCounts(t *testing.T, root wixElement, expected map[string]int) {
	t.Helper()
	for name, count := range expected {
		if got := len(wixElements(root, name)); got != count {
			t.Fatalf("got %d %s elements, want %d", got, name, count)
		}
	}
}

func assertWixTopology(t *testing.T, root wixElement) {
	t.Helper()
	if err := checkWixTopology(root); err != nil {
		t.Error(err)
	}
}

func checkWixTopology(root wixElement) error {
	packageElement, err := wixDirectChild(root, "Package")
	if err != nil {
		return fmt.Errorf("Wix: %w", err)
	}
	for _, name := range []string{"MajorUpgrade", "MediaTemplate"} {
		if _, err := wixDirectChild(packageElement, name); err != nil {
			return fmt.Errorf("Package: %w", err)
		}
	}
	for _, child := range []struct {
		name string
		id   string
	}{
		{name: "Icon", id: "Codesk.ico"},
		{name: "Property", id: "ARPPRODUCTICON"},
		{name: "Feature", id: "CodeskFeature"},
	} {
		if _, err := wixDirectChildByID(packageElement, child.name, child.id); err != nil {
			return fmt.Errorf("Package: %w", err)
		}
	}

	localAppData, err := wixDirectChildByID(packageElement, "StandardDirectory", "LocalAppDataFolder")
	if err != nil {
		return fmt.Errorf("Package: %w", err)
	}
	perUserPrograms, err := wixDirectChildByID(localAppData, "Directory", "PerUserProgramsFolder")
	if err != nil {
		return fmt.Errorf("StandardDirectory[LocalAppDataFolder]: %w", err)
	}
	installFolder, err := wixDirectChildByID(perUserPrograms, "Directory", "INSTALLFOLDER")
	if err != nil {
		return fmt.Errorf("Directory[PerUserProgramsFolder]: %w", err)
	}
	codeskComponent, err := wixDirectChildByID(installFolder, "Component", "CodeskExecutable")
	if err != nil {
		return fmt.Errorf("Directory[INSTALLFOLDER]: %w", err)
	}
	agentComponent, err := wixDirectChildByID(installFolder, "Component", "AgentToolExecutable")
	if err != nil {
		return fmt.Errorf("Directory[INSTALLFOLDER]: %w", err)
	}
	codeskFile, err := wixDirectChildByID(codeskComponent, "File", "CodeskExe")
	if err != nil {
		return fmt.Errorf("Component[CodeskExecutable]: %w", err)
	}
	if _, err := wixDirectChildByID(codeskFile, "Shortcut", "CodeskStartMenuShortcut"); err != nil {
		return fmt.Errorf("File[CodeskExe]: %w", err)
	}
	if _, err := wixDirectChildByID(codeskComponent, "RemoveFolder", "RemoveCodeskProgramsFolder"); err != nil {
		return fmt.Errorf("Component[CodeskExecutable]: %w", err)
	}
	if _, err := wixDirectChildByID(agentComponent, "File", "AgentToolExe"); err != nil {
		return fmt.Errorf("Component[AgentToolExecutable]: %w", err)
	}
	loginItemComponent, err := wixDirectChildByID(installFolder, "Component", "CodeskLoginItemCleanup")
	if err != nil {
		return fmt.Errorf("Directory[INSTALLFOLDER]: %w", err)
	}
	if _, err := wixDirectChild(loginItemComponent, "RegistryValue"); err != nil {
		return fmt.Errorf("Component[CodeskLoginItemCleanup]: %w", err)
	}

	programMenu, err := wixDirectChildByID(packageElement, "StandardDirectory", "ProgramMenuFolder")
	if err != nil {
		return fmt.Errorf("Package: %w", err)
	}
	if _, err := wixDirectChildByID(programMenu, "Directory", "CodeskProgramsFolder"); err != nil {
		return fmt.Errorf("StandardDirectory[ProgramMenuFolder]: %w", err)
	}
	feature, err := wixDirectChildByID(packageElement, "Feature", "CodeskFeature")
	if err != nil {
		return fmt.Errorf("Package: %w", err)
	}
	for _, id := range []string{"CodeskExecutable", "AgentToolExecutable", "CodeskLoginItemCleanup"} {
		if _, err := wixDirectChildByID(feature, "ComponentRef", id); err != nil {
			return fmt.Errorf("Feature[CodeskFeature]: %w", err)
		}
	}
	return nil
}

func wixDirectChild(parent wixElement, name string) (wixElement, error) {
	var matches []wixElement
	for _, child := range parent.Nodes {
		if child.XMLName.Local == name {
			matches = append(matches, child)
		}
	}
	if len(matches) != 1 {
		return wixElement{}, fmt.Errorf("got %d direct %s children, want 1", len(matches), name)
	}
	return matches[0], nil
}

func wixDirectChildByID(parent wixElement, name, id string) (wixElement, error) {
	var matches []wixElement
	for _, child := range parent.Nodes {
		if child.XMLName.Local == name && wixAttr(child, "Id") == id {
			matches = append(matches, child)
		}
	}
	if len(matches) != 1 {
		return wixElement{}, fmt.Errorf("got %d direct %s children with Id=%q, want 1", len(matches), name, id)
	}
	return matches[0], nil
}

func assertWixElementByID(t *testing.T, root wixElement, name, id string) wixElement {
	t.Helper()
	for _, candidate := range wixElements(root, name) {
		if wixAttr(candidate, "Id") == id {
			return candidate
		}
	}
	t.Fatalf("missing %s element with Id=%q", name, id)
	return wixElement{}
}

func assertWixAttrs(t *testing.T, element wixElement, expected map[string]string) {
	t.Helper()
	if err := checkWixAttrs(element, expected); err != nil {
		t.Error(err)
	}
}

func checkWixAttrs(element wixElement, expected map[string]string) error {
	actual := make(map[string]string, len(element.Attrs))
	for _, attr := range element.Attrs {
		if isDefaultWixNamespaceDeclaration(element, attr) {
			continue
		}
		if attr.Name.Space != "" {
			return fmt.Errorf("%s has namespaced attribute {%s}%s", element.XMLName.Local, attr.Name.Space, attr.Name.Local)
		}
		if _, exists := actual[attr.Name.Local]; exists {
			return fmt.Errorf("%s has duplicate attribute %s", element.XMLName.Local, attr.Name.Local)
		}
		actual[attr.Name.Local] = attr.Value
	}
	for name, value := range actual {
		want, ok := expected[name]
		if !ok {
			return fmt.Errorf("%s has unexpected attribute %s=%q", element.XMLName.Local, name, value)
		}
		if value != want {
			return fmt.Errorf("%s[%s]: got %q, want %q", element.XMLName.Local, name, value, want)
		}
	}
	for name, value := range expected {
		if got, ok := actual[name]; !ok {
			return fmt.Errorf("%s is missing required attribute %s=%q", element.XMLName.Local, name, value)
		} else if got != value {
			return fmt.Errorf("%s[%s]: got %q, want %q", element.XMLName.Local, name, got, value)
		}
	}
	return nil
}

func isDefaultWixNamespaceDeclaration(element wixElement, attr xml.Attr) bool {
	if element.XMLName.Space != wixNamespace || attr.Value != wixNamespace {
		return false
	}
	return (attr.Name.Space == "" && attr.Name.Local == "xmlns") ||
		(attr.Name.Space == "xmlns" && (attr.Name.Local == "" || attr.Name.Local == "xmlns"))
}

func wixAttr(element wixElement, name string) string {
	for _, attr := range element.Attrs {
		if attr.Name.Local == name {
			return attr.Value
		}
	}
	return ""
}

func assertWixDefinitions(t *testing.T, source string, expected map[string]string) {
	t.Helper()
	actual := map[string]string{}
	for _, definition := range strings.Split(source, ";") {
		definition = strings.TrimSpace(definition)
		if definition == "" {
			continue
		}
		name, value, ok := strings.Cut(definition, "=")
		if !ok || strings.TrimSpace(name) == "" {
			t.Fatalf("invalid WiX definition %q", definition)
		}
		name = strings.TrimSpace(name)
		value = strings.TrimSpace(value)
		if _, exists := actual[name]; exists {
			t.Fatalf("duplicate WiX definition %q", name)
		}
		actual[name] = value
	}
	if len(actual) != len(expected) {
		t.Fatalf("WiX definitions = %#v, want %#v", actual, expected)
	}
	for name, value := range expected {
		if got, ok := actual[name]; !ok || got != value {
			t.Errorf("WiX definition %s = %q (present=%t), want %q", name, got, ok, value)
		}
	}
}
