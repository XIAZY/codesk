package main

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestDaemonContainerProvidesClaudeToolShell(t *testing.T) {
	data, err := os.ReadFile("../daemon/Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	if err := checkDaemonContainerClaudeShell(source); err != nil {
		t.Fatal(err)
	}

	mutations := []struct {
		name, old, replacement string
	}{
		{"bash package removed", "nodejs npm libgcc bash", "nodejs npm libgcc"},
		{"SHELL declaration removed", "ENV IS_SANDBOX=1 SHELL=/bin/bash", "ENV IS_SANDBOX=1"},
		{"unsupported BusyBox shell restored", "SHELL=/bin/bash", "SHELL=/bin/sh"},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			mutated := strings.Replace(source, mutation.old, mutation.replacement, 1)
			if mutated == source {
				t.Fatalf("mutation source %q was not found", mutation.old)
			}
			if err := checkDaemonContainerClaudeShell(mutated); err == nil {
				t.Fatal("daemon Claude shell mutation survived")
			}
		})
	}
}

func checkDaemonContainerClaudeShell(source string) error {
	for required, want := range map[string]int{
		"RUN apk add --no-cache nodejs npm libgcc bash": 1,
		"ENV IS_SANDBOX=1 SHELL=/bin/bash":              1,
	} {
		if got := strings.Count(source, required); got != want {
			return fmt.Errorf("daemon container source count for %q = %d, want %d", required, got, want)
		}
	}
	return nil
}
