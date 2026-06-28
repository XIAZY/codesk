package notty

import (
	"fmt"
	"regexp"
)

const (
	workspaceSlugMinLen   = 2
	workspaceSlugMaxLen   = 64
	workspaceHandleMinLen = 2
	workspaceHandleMaxLen = 32
)

var identifierPattern = regexp.MustCompile(`^[a-z0-9_-]+$`)

func validateWorkspaceSlug(value string) (string, error) {
	return validateIdentifier(value, "Workspace slug", workspaceSlugMinLen, workspaceSlugMaxLen)
}

func validateHandle(value string) (string, error) {
	return validateIdentifier(value, "Handle", workspaceHandleMinLen, workspaceHandleMaxLen)
}

func validateIdentifier(value string, label string, minLen int, maxLen int) (string, error) {
	if value == "" {
		return "", fmt.Errorf("%s is required.", label)
	}
	if len(value) < minLen || len(value) > maxLen {
		return "", fmt.Errorf("%s must be between %d and %d characters.", label, minLen, maxLen)
	}
	if !identifierPattern.MatchString(value) {
		return "", fmt.Errorf("%s can only contain lowercase letters, numbers, underscores, and dashes.", label)
	}
	return value, nil
}
