package desktopsetup

import "testing"

func TestValidateOptions(t *testing.T) {
	tests := []struct {
		name    string
		options Options
		valid   bool
	}{
		{name: "install", options: Options{Version: "1.2.3", Arch: "amd64"}, valid: true},
		{name: "uninstall", options: Options{Version: "1.2.3", Arch: "arm64", Uninstall: true}, valid: true},
		{name: "unknown arch", options: Options{Version: "1.2.3", Arch: "x86"}},
		{name: "invalid version", options: Options{Version: "../1", Arch: "amd64"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateOptions(test.options)
			if test.valid && err != nil {
				t.Fatalf("validateOptions() error = %v", err)
			}
			if !test.valid && err == nil {
				t.Fatal("validateOptions() unexpectedly succeeded")
			}
		})
	}
}
