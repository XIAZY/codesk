package buildinfo

import "testing"

func TestRequire(t *testing.T) {
	previous := Version
	t.Cleanup(func() { Version = previous })

	for _, invalid := range []string{"", "dev"} {
		Version = invalid
		if _, err := Require(); err == nil {
			t.Fatalf("Require() accepted invalid version %q", invalid)
		}
	}

	Version = "1.2.3"
	if got, err := Require(); err != nil || got != Version {
		t.Fatalf("Require() = %q, %v; want %q, nil", got, err, Version)
	}
}
