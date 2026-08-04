package main

import (
	"debug/pe"
	"testing"
)

func TestWindowsMachinesForArchitecture(t *testing.T) {
	tests := []struct {
		name         string
		architecture string
		want         map[string]uint16
		wantError    bool
	}{
		{name: "all", want: windowsMachines},
		{name: "amd64", architecture: "amd64", want: map[string]uint16{"amd64": pe.IMAGE_FILE_MACHINE_AMD64}},
		{name: "arm64", architecture: "arm64", want: map[string]uint16{"arm64": pe.IMAGE_FILE_MACHINE_ARM64}},
		{name: "unsupported", architecture: "386", wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := windowsMachinesForArchitecture(test.architecture)
			if (err != nil) != test.wantError {
				t.Fatalf("windowsMachinesForArchitecture(%q) error = %v, wantError %t", test.architecture, err, test.wantError)
			}
			if test.wantError {
				return
			}
			if len(got) != len(test.want) {
				t.Fatalf("machine count = %d, want %d", len(got), len(test.want))
			}
			for architecture, machine := range test.want {
				if got[architecture] != machine {
					t.Errorf("machine for %s = %#x, want %#x", architecture, got[architecture], machine)
				}
			}
		})
	}
}
