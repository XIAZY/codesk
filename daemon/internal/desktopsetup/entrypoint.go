package desktopsetup

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
)

const (
	ExitSuccess = 0
	ExitFailure = 1
)

type MessageKind uint8

const (
	ErrorMessage MessageKind = iota
	InformationMessage
)

type ShowMessage func(kind MessageKind, message string)

func RunMain(
	arguments []string,
	version string,
	arch string,
	setup func(Options) error,
	showMessage ShowMessage,
) int {
	quiet := argumentPresent(arguments, "--quiet")
	options, err := parseOptions(arguments, version, arch)
	if err == nil {
		quiet = options.Quiet
		err = setup(options)
	}
	if err != nil {
		if !quiet {
			showMessage(ErrorMessage, "Codesk setup could not complete.\n\n"+err.Error())
		}
		return ExitFailure
	}
	if !quiet {
		message := "Codesk was installed successfully."
		if options.Uninstall {
			message = "Codesk was uninstalled successfully."
		}
		showMessage(InformationMessage, message)
	}
	return ExitSuccess
}

func parseOptions(arguments []string, version, arch string) (Options, error) {
	flags := flag.NewFlagSet("CodeskSetup", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var options Options
	flags.BoolVar(&options.Quiet, "quiet", false, "suppress setup dialogs")
	flags.BoolVar(&options.NoLaunch, "no-launch", false, "do not launch Codesk after installation")
	flags.BoolVar(&options.Uninstall, "uninstall", false, "uninstall Codesk")
	if err := flags.Parse(arguments); err != nil {
		return Options{}, fmt.Errorf("invalid setup arguments: %w", err)
	}
	if flags.NArg() != 0 {
		return Options{}, errors.New("invalid setup arguments: unexpected positional arguments")
	}
	options.Version = version
	options.Arch = arch
	return options, nil
}

func argumentPresent(arguments []string, name string) bool {
	for _, argument := range arguments {
		if strings.EqualFold(argument, name) || strings.HasPrefix(strings.ToLower(argument), strings.ToLower(name)+"=") {
			return true
		}
	}
	return false
}
