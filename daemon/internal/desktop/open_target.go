package desktop

import (
	"errors"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

const maximumOpenTargetBytes = 8 << 10

func validateOpenTarget(target, logsDir string) (isDirectory bool, err error) {
	if err := requireAbsolute("logs", logsDir); err != nil {
		return false, err
	}
	if strings.ContainsRune(logsDir, '\x00') {
		return false, errors.New("desktop: invalid logs directory")
	}
	if target == logsDir {
		return true, nil
	}
	if target == "" || len(target) > maximumOpenTargetBytes || target != strings.TrimSpace(target) ||
		!utf8.ValidString(target) {
		return false, errors.New("desktop: invalid open target")
	}
	for _, character := range target {
		if unicode.IsControl(character) {
			return false, errors.New("desktop: invalid open target")
		}
	}
	parsed, parseErr := url.Parse(target)
	if parseErr != nil || parsed.Opaque != "" || (parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.Host == "" || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" {
		return false, errors.New("desktop: open target must be an absolute HTTP(S) URL or the Codesk logs directory")
	}
	return false, nil
}
