package desktop

import (
	"errors"
	"strconv"
)

const darwinWatchdogArgument = "--codesk-process-watchdog"

type darwinWatchdogInvocation struct {
	parentPID int
	groupID   int
	readyFD   int
}

func darwinWatchdogParentExited(invocation darwinWatchdogInvocation, actualParentPID, actualParentGroup int) (bool, error) {
	if actualParentGroup != invocation.groupID {
		return false, errors.New("desktop: process watchdog parent identity mismatch")
	}
	if actualParentPID == invocation.parentPID {
		return false, nil
	}
	// A Darwin orphan is reparented to launchd (PID 1). Treat that transition as
	// the parent-exit race it represents so descendants cannot escape between
	// the identity check and kqueue registration.
	if actualParentPID == 1 {
		return true, nil
	}
	return false, errors.New("desktop: process watchdog parent identity mismatch")
}

func parseDarwinWatchdogInvocation(arguments []string) (darwinWatchdogInvocation, bool, error) {
	if len(arguments) == 0 || arguments[0] != darwinWatchdogArgument {
		return darwinWatchdogInvocation{}, false, nil
	}
	if len(arguments) != 4 {
		return darwinWatchdogInvocation{}, true, errors.New("desktop: invalid process watchdog invocation")
	}
	parentPID, parentErr := strconv.Atoi(arguments[1])
	groupID, groupErr := strconv.Atoi(arguments[2])
	readyFD, readyErr := strconv.Atoi(arguments[3])
	if parentErr != nil || groupErr != nil || readyErr != nil || parentPID <= 1 || groupID != parentPID || readyFD != 3 {
		return darwinWatchdogInvocation{}, true, errors.New("desktop: invalid process watchdog invocation")
	}
	return darwinWatchdogInvocation{parentPID: parentPID, groupID: groupID, readyFD: readyFD}, true, nil
}
