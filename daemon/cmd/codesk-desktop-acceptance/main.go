package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"notty/daemon/internal/desktopacceptance"
)

var builtRunnerSourceRevision string

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(arguments []string) int {
	flags := flag.NewFlagSet("codesk-desktop-acceptance", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	var config desktopacceptance.Config
	config.TargetPlatform = "windows"
	var previous desktopacceptance.ReleaseInput
	var timeout time.Duration
	var phase string
	flags.StringVar(&phase, "phase", "", "native login-cycle phase: prepare or resume")
	flags.StringVar(&config.SourceRevision, "source-revision", "", "exact full source revision")
	flags.StringVar(&config.Candidate.Directory, "candidate-dir", "", "candidate version directory")
	flags.StringVar(&config.Candidate.Version, "candidate-version", "", "candidate release version")
	flags.StringVar(&previous.Directory, "previous-dir", "", "previous version directory")
	flags.StringVar(&previous.Version, "previous-version", "", "previous release version")
	flags.StringVar(&config.EvidenceDir, "evidence-dir", "", "new evidence directory")
	flags.DurationVar(&timeout, "timeout", 5*time.Minute, "per-step timeout")
	flags.BoolVar(&config.Destructive, "destructive", false, "confirm dedicated-account install/uninstall testing")
	flags.BoolVar(&config.AllowUnsignedFunctional, "allow-unsigned-functional", false, "allow non-publishable unsigned functional evidence")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "codesk-desktop-acceptance: unexpected positional arguments")
		return 2
	}
	switch desktopacceptance.Phase(phase) {
	case desktopacceptance.PhasePrepare, desktopacceptance.PhaseResume:
		config.Phase = desktopacceptance.Phase(phase)
	default:
		fmt.Fprintln(os.Stderr, "codesk-desktop-acceptance: --phase must be prepare or resume")
		return 2
	}
	config.Timeout = timeout
	config.Previous = &previous
	config.RunnerSourceRevision = builtRunnerSourceRevision
	adapter, err := newNativeAdapter(config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "codesk-desktop-acceptance: %v\n", err)
		return 1
	}
	engine := desktopacceptance.Engine{
		Adapter: adapter,
		Operator: desktopacceptance.InteractiveOperator{
			Input:  os.Stdin,
			Output: os.Stderr,
		},
	}
	report, err := engine.Run(context.Background(), config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "codesk-desktop-acceptance: verdict=%s evidence=%s error=%v\n", report.Verdict, config.EvidenceDir, err)
		var runError *desktopacceptance.RunError
		if errors.As(err, &runError) && runError.Status == desktopacceptance.StatusBlocked {
			return 2
		}
		return 1
	}
	fmt.Fprintf(os.Stderr, "codesk-desktop-acceptance: verdict=%s publishable=%t evidence=%s\n", report.Verdict, report.Publishable, config.EvidenceDir)
	return 0
}
