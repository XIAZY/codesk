package desktop

import (
	"errors"
	"path/filepath"

	"golang.org/x/sys/windows"
)

const appName = "Codesk"

func DefaultDirs() (Dirs, error) {
	local, err := windows.KnownFolderPath(windows.FOLDERID_LocalAppData, windows.KF_FLAG_CREATE)
	if err != nil {
		return Dirs{}, errNoAppDir("resolve current-user local application data")
	}
	if local == "" || !filepath.IsAbs(local) || filepath.Clean(local) != local {
		return Dirs{}, errNoAppDir("current-user local application data path is invalid")
	}
	base := filepath.Join(local, appName)
	dirs := Dirs{
		Data:  base,
		Logs:  filepath.Join(base, "Logs"),
		Cache: filepath.Join(base, "Cache"),
	}
	if err := dirs.Validate(); err != nil {
		return Dirs{}, errors.New("desktop: current-user application directories are invalid")
	}
	return dirs, nil
}
