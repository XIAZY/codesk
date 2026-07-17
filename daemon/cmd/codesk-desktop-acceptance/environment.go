package main

import (
	"os"
	"sort"
	"strings"
)

func environmentWithOverrides(values map[string]string) []string {
	return environmentWithOverridesFrom(os.Environ(), values)
}

func environmentWithOverridesFrom(base []string, values map[string]string) []string {
	environment := make([]string, 0, len(base)+len(values))
	for _, entry := range base {
		separator := strings.IndexByte(entry, '=')
		if separator <= 0 {
			environment = append(environment, entry)
			continue
		}
		name := entry[:separator]
		overridden := false
		for override := range values {
			if strings.EqualFold(name, override) {
				overridden = true
				break
			}
		}
		if !overridden {
			environment = append(environment, entry)
		}
	}

	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		environment = append(environment, name+"="+values[name])
	}
	return environment
}
