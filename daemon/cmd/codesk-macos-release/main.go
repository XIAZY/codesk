package main

import (
	"flag"
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		fatalf("usage: codesk-macos-release <validate-version|plist|verify-app|verify-volume|manifest|verify> [arguments]")
	}
	var err error
	switch os.Args[1] {
	case "validate-version":
		err = runValidateVersion(os.Args[2:])
	case "plist":
		err = runPlist(os.Args[2:])
	case "verify-app":
		err = runVerifyApp(os.Args[2:])
	case "verify-volume":
		err = runVerifyVolume(os.Args[2:])
	case "manifest":
		err = runManifest(os.Args[2:])
	case "verify":
		err = runVerify(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fatalf("%v", err)
	}
}

func runVerifyVolume(arguments []string) error {
	flags := flag.NewFlagSet("verify-volume", flag.ContinueOnError)
	mount := flags.String("mount", "", "mounted Codesk disk-image root")
	flags.SetOutput(os.Stderr)
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 || *mount == "" {
		return fmt.Errorf("usage: codesk-macos-release verify-volume --mount <path>")
	}
	return verifyMountedVolume(*mount)
}

func runValidateVersion(arguments []string) error {
	flags := flag.NewFlagSet("validate-version", flag.ContinueOnError)
	development := flags.Bool("development", false, "allow the unsigned development version")
	flags.SetOutput(os.Stderr)
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return fmt.Errorf("usage: codesk-macos-release validate-version [--development] <version>")
	}
	version, err := parseReleaseVersion(flags.Arg(0), *development)
	if err != nil {
		return err
	}
	fmt.Println(version.Bundle)
	return nil
}

func runPlist(arguments []string) error {
	flags := flag.NewFlagSet("plist", flag.ContinueOnError)
	output := flags.String("output", "", "Info.plist output path")
	versionValue := flags.String("version", "", "release version")
	development := flags.Bool("development", false, "allow the unsigned development version")
	flags.SetOutput(os.Stderr)
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 || *output == "" || *versionValue == "" {
		return fmt.Errorf("usage: codesk-macos-release plist --output <path> --version <version> [--development]")
	}
	version, err := parseReleaseVersion(*versionValue, *development)
	if err != nil {
		return err
	}
	return writeAtomic(*output, renderInfoPlist(version), 0o644)
}

func runVerifyApp(arguments []string) error {
	flags := flag.NewFlagSet("verify-app", flag.ContinueOnError)
	app := flags.String("app", "", "Codesk.app path")
	versionValue := flags.String("version", "", "release version")
	development := flags.Bool("development", false, "allow the unsigned development version")
	printTreeHash := flags.Bool("print-tree-hash", false, "print the verified application tree hash")
	flags.SetOutput(os.Stderr)
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 || *app == "" || *versionValue == "" {
		return fmt.Errorf("usage: codesk-macos-release verify-app --app <Codesk.app> --version <version> [--development]")
	}
	version, err := parseReleaseVersion(*versionValue, *development)
	if err != nil {
		return err
	}
	verification, err := verifyApp(*app, version)
	if err != nil {
		return err
	}
	if *printTreeHash {
		fmt.Println(verification.TreeSHA256)
	}
	return nil
}

func runManifest(arguments []string) error {
	flags := flag.NewFlagSet("manifest", flag.ContinueOnError)
	output := flags.String("output", "", "release directory")
	versionValue := flags.String("version", "", "release version")
	sourceRevision := flags.String("source-revision", "", "full source Git revision")
	signed := flags.Bool("signed", true, "artifact is Developer ID signed and notarized")
	development := flags.Bool("development", false, "allow the unsigned development version")
	flags.SetOutput(os.Stderr)
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 || *output == "" || *versionValue == "" || *sourceRevision == "" {
		return fmt.Errorf("usage: codesk-macos-release manifest --output <dir> --version <version> --source-revision <sha> --signed=<bool> [--development]")
	}
	version, err := parseReleaseVersion(*versionValue, *development)
	if err != nil {
		return err
	}
	if *signed && version.Development {
		return fmt.Errorf("codesk macOS release: development artifacts cannot be marked signed")
	}
	return writeManifest(*output, version, *sourceRevision, *signed)
}

func runVerify(arguments []string) error {
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	allowUnsigned := flags.Bool("allow-unsigned", false, "allow construction-only unsigned artifacts")
	flags.SetOutput(os.Stderr)
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 2 {
		return fmt.Errorf("usage: codesk-macos-release verify [--allow-unsigned] <release-dir> <version>")
	}
	version, err := parseReleaseVersion(flags.Arg(1), flags.Arg(1) == "dev")
	if err != nil {
		return err
	}
	return verifyRelease(flags.Arg(0), version, *allowUnsigned)
}

func fatalf(format string, arguments ...any) {
	fmt.Fprintf(os.Stderr, "codesk-macos-release: "+format+"\n", arguments...)
	os.Exit(1)
}
