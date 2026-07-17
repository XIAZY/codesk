//go:build !windows

package main

import (
	"errors"

	"notty/daemon/internal/desktopacceptance"
)

func newNativeAdapter(desktopacceptance.Config) (desktopacceptance.NativeAdapter, error) {
	return nil, errors.New("native Windows execution is required")
}
