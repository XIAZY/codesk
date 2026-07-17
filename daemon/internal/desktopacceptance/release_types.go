package desktopacceptance

type Release struct {
	Platform       string            `json:"platform"`
	Version        string            `json:"version"`
	SourceRevision string            `json:"source_revision"`
	Signed         bool              `json:"signed"`
	Toolchain      map[string]string `json:"toolchain"`
	ManifestPath   string            `json:"manifest_path"`
	ManifestSHA256 string            `json:"manifest_sha256"`
	SumsPath       string            `json:"sums_path"`
	SumsSHA256     string            `json:"sums_sha256"`
	Artifacts      []ReleaseArtifact `json:"artifacts"`
}

type ReleaseArtifact struct {
	Architecture       string `json:"architecture"`
	Path               string `json:"path"`
	SHA256             string `json:"sha256"`
	Size               int64  `json:"size"`
	ManifestSigned     bool   `json:"manifest_signed"`
	NativeFormat       string `json:"native_format"`
	NativeArchitecture string `json:"native_architecture"`
	SignatureValid     bool   `json:"signature_valid"`
	SignatureError     string `json:"signature_error,omitempty"`
}

func (r Release) Artifact(architecture string) (ReleaseArtifact, bool) {
	for _, artifact := range r.Artifacts {
		if artifact.Architecture == architecture {
			return artifact, true
		}
	}
	return ReleaseArtifact{}, false
}
