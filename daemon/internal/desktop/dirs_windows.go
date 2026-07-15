package desktop

import "os"

const appName = "Codesk"

func DefaultDirs() (Dirs, error) {
	local := os.Getenv("LOCALAPPDATA")
	if local == "" {
		return Dirs{}, errNoAppDir("LOCALAPPDATA is not set")
	}
	base := local + `\` + appName
	return Dirs{
		Data:  base,
		Logs:  base + `\Logs`,
		Cache: base + `\Cache`,
	}, nil
}
