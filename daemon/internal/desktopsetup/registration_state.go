package desktopsetup

var (
	uninstallStringValues = []string{
		"DisplayName",
		"DisplayVersion",
		"Publisher",
		"DisplayIcon",
		"InstallLocation",
		"UninstallString",
		"QuietUninstallString",
	}
	uninstallDWORDValues = []string{"NoModify", "NoRepair", "EstimatedSize"}
)

const maximumRegistrationBlobBytes = 32 << 10

type stringValueState struct {
	Present bool   `json:"present"`
	Value   string `json:"value,omitempty"`
	Type    uint32 `json:"type,omitempty"`
}

type fileValueState struct {
	Present bool   `json:"present"`
	Data    []byte `json:"data,omitempty"`
}

type registryValueState struct {
	Name string `json:"name"`
	Type uint32 `json:"type"`
	Data []byte `json:"data,omitempty"`
}

type uninstallRegistrationState struct {
	Existed bool                 `json:"existed"`
	Values  []registryValueState `json:"values,omitempty"`
}

type registrationState struct {
	Run       stringValueState           `json:"run"`
	Shortcut  fileValueState             `json:"shortcut"`
	Uninstall uninstallRegistrationState `json:"uninstall"`
}
