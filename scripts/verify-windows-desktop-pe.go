//go:build ignore

package main

import (
	"debug/pe"
	"fmt"
	"os"
)

var windowsMachines = map[string]uint16{
	"amd64": pe.IMAGE_FILE_MACHINE_AMD64,
	"arm64": pe.IMAGE_FILE_MACHINE_ARM64,
}

var windowsSubsystems = map[string]uint16{
	"gui":     pe.IMAGE_SUBSYSTEM_WINDOWS_GUI,
	"console": pe.IMAGE_SUBSYSTEM_WINDOWS_CUI,
}

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintf(os.Stderr, "usage: %s <image.exe> <amd64|arm64> <gui|console>\n", os.Args[0])
		os.Exit(2)
	}
	expectedMachine, ok := windowsMachines[os.Args[2]]
	if !ok {
		fmt.Fprintf(os.Stderr, "unsupported architecture %q\n", os.Args[2])
		os.Exit(2)
	}
	expectedSubsystem, ok := windowsSubsystems[os.Args[3]]
	if !ok {
		fmt.Fprintf(os.Stderr, "unsupported subsystem %q\n", os.Args[3])
		os.Exit(2)
	}

	image, err := pe.Open(os.Args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse Windows PE: %v\n", err)
		os.Exit(1)
	}
	defer image.Close()
	if image.FileHeader.Machine != expectedMachine {
		fmt.Fprintf(os.Stderr, "PE machine 0x%04x, want 0x%04x\n", image.FileHeader.Machine, expectedMachine)
		os.Exit(1)
	}

	var subsystem uint16
	switch header := image.OptionalHeader.(type) {
	case *pe.OptionalHeader32:
		subsystem = header.Subsystem
	case *pe.OptionalHeader64:
		subsystem = header.Subsystem
	default:
		fmt.Fprintf(os.Stderr, "Windows PE has unsupported optional header %T\n", image.OptionalHeader)
		os.Exit(1)
	}
	if subsystem != expectedSubsystem {
		fmt.Fprintf(os.Stderr, "PE subsystem %d, want %s (%d)\n", subsystem, os.Args[3], expectedSubsystem)
		os.Exit(1)
	}

	fmt.Printf("verified %s: PE machine 0x%04x, Windows %s subsystem\n", os.Args[1], expectedMachine, os.Args[3])
}
