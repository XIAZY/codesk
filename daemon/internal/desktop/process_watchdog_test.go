package desktop

import (
	"strconv"
	"testing"
)

func TestParseDarwinWatchdogInvocation(t *testing.T) {
	valid := []string{darwinWatchdogArgument, "123", "123", "3"}
	invocation, handled, err := parseDarwinWatchdogInvocation(valid)
	if err != nil || !handled {
		t.Fatalf("parse valid invocation = %#v, %t, %v", invocation, handled, err)
	}
	if invocation.parentPID != 123 || invocation.groupID != 123 || invocation.readyFD != 3 {
		t.Fatalf("invocation = %#v", invocation)
	}

	invalid := [][]string{
		{darwinWatchdogArgument},
		{darwinWatchdogArgument, "abc", "123", "3"},
		{darwinWatchdogArgument, "123", "124", "3"},
		{darwinWatchdogArgument, "123", "123", "4"},
		{darwinWatchdogArgument, strconv.Itoa(1), "1", "3"},
	}
	for _, arguments := range invalid {
		if _, handled, err := parseDarwinWatchdogInvocation(arguments); err == nil || !handled {
			t.Fatalf("parse invalid invocation %q = handled %t, error %v", arguments, handled, err)
		}
	}
	if _, handled, err := parseDarwinWatchdogInvocation([]string{"--other"}); err != nil || handled {
		t.Fatalf("unrelated arguments = handled %t, error %v", handled, err)
	}
}

func TestDarwinWatchdogParentExited(t *testing.T) {
	invocation := darwinWatchdogInvocation{parentPID: 123, groupID: 123, readyFD: 3}
	tests := []struct {
		name        string
		parentPID   int
		parentGroup int
		wantExited  bool
		wantFailure bool
	}{
		{name: "bound parent", parentPID: 123, parentGroup: 123},
		{name: "reparented to launchd", parentPID: 1, parentGroup: 123, wantExited: true},
		{name: "wrong parent", parentPID: 456, parentGroup: 123, wantFailure: true},
		{name: "wrong group", parentPID: 123, parentGroup: 456, wantFailure: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			exited, err := darwinWatchdogParentExited(invocation, test.parentPID, test.parentGroup)
			if (err != nil) != test.wantFailure {
				t.Fatalf("darwinWatchdogParentExited() error = %v, wantFailure = %t", err, test.wantFailure)
			}
			if exited != test.wantExited {
				t.Fatalf("darwinWatchdogParentExited() = %t, want %t", exited, test.wantExited)
			}
		})
	}
}
