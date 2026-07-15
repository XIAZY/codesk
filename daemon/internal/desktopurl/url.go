package desktopurl

import (
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

const maximumURLBytes = 2048

// Valid reports whether raw is a browser-safe absolute HTTP(S) workspace URL.
func Valid(raw string) bool {
	if raw == "" || len(raw) > maximumURLBytes || raw != strings.TrimSpace(raw) ||
		!utf8.ValidString(raw) || strings.Contains(raw, "#") {
		return false
	}
	for _, character := range raw {
		if unicode.IsControl(character) {
			return false
		}
	}
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return false
	}
	return parsed.User == nil && parsed.RawQuery == "" && !parsed.ForceQuery && parsed.Fragment == ""
}
