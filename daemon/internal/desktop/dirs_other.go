//go:build !windows && !darwin

package desktop

func DefaultDirs() (Dirs, error) {
	return Dirs{}, ErrUnsupportedPlatform
}
