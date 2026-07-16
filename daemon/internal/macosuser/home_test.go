package macosuser

import (
	"os/user"
	"testing"
)

func TestHomeDirIgnoresCallerHOME(t *testing.T) {
	current, err := user.Current()
	if err != nil {
		t.Skipf("current operating-system account is unavailable: %v", err)
	}
	account, err := user.LookupId(current.Uid)
	if err != nil {
		t.Skipf("operating-system account lookup is unavailable: %v", err)
	}
	hostileHome := t.TempDir()
	t.Setenv("HOME", hostileHome)

	home, err := HomeDir()
	if err != nil {
		t.Fatalf("HomeDir() error = %v", err)
	}
	if home != account.HomeDir {
		t.Fatalf("HomeDir() = %q, want account-record home %q", home, account.HomeDir)
	}
	if home == hostileHome {
		t.Fatalf("HomeDir() trusted caller-controlled HOME %q", hostileHome)
	}
}
