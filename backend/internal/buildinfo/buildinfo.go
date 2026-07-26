// Package buildinfo exposes the identity of the running build. The values are
// injected at link time with `-ldflags -X`; when unset (local `go run`, `go test`)
// they report the "dev" fallback so callers never see an empty string.
package buildinfo

var (
	// Commit is the short git SHA the binary was built from.
	Commit = "dev"
	// Time is the RFC3339 UTC timestamp the binary was built at.
	Time = "unknown"
)
