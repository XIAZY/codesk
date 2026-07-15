package desktop

import "os"

const appName = "Codesk"

func DefaultDirs() (Dirs, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Dirs{}, errNoAppDir(err.Error())
	}
	return Dirs{
		Data:  home + "/Library/Application Support/" + appName,
		Logs:  home + "/Library/Logs/" + appName,
		Cache: home + "/Library/Caches/" + appName,
	}, nil
}
