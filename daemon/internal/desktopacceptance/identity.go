package desktopacceptance

import (
	"errors"
	"strings"
)

func validateExactRevision(revision string) error {
	if len(revision) != 40 || strings.Trim(revision, "0") == "" {
		return errors.New("exact source revision must be a nonzero full 40-character hexadecimal identity")
	}
	for _, character := range revision {
		if !strings.ContainsRune("0123456789abcdef", character) {
			return errors.New("exact source revision must be lowercase hexadecimal")
		}
	}
	return nil
}
