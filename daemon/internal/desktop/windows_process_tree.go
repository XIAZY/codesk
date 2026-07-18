//go:build windows

package desktop

import (
	"fmt"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

var desktopProcessTree struct {
	sync.Mutex
	handle windows.Handle
}

// ContainWindowsProcessTree assigns the desktop process to a job whose child
// processes are terminated when the desktop dies. The job handle intentionally
// remains open for the lifetime of the process.
func ContainWindowsProcessTree() error {
	desktopProcessTree.Lock()
	defer desktopProcessTree.Unlock()
	if desktopProcessTree.handle != 0 {
		return nil
	}

	job, err := newKillOnCloseJob()
	if err != nil {
		return err
	}
	if err := windows.AssignProcessToJobObject(job, windows.CurrentProcess()); err != nil {
		_ = windows.CloseHandle(job)
		return fmt.Errorf("assign desktop process to containment job: %w", err)
	}
	desktopProcessTree.handle = job
	return nil
}

func newKillOnCloseJob() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, fmt.Errorf("create desktop containment job: %w", err)
	}
	information := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	information.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&information)),
		uint32(unsafe.Sizeof(information)),
	); err != nil {
		_ = windows.CloseHandle(job)
		return 0, fmt.Errorf("configure desktop containment job: %w", err)
	}
	return job, nil
}
