package buildinfo

import "fmt"

var Version string

func Require() (string, error) {
	if Version == "" || Version == "dev" {
		return "", fmt.Errorf("embedded build version is missing or invalid")
	}
	return Version, nil
}
