package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

type msiLifecycleState struct {
	ProductCode              string            `json:"product_code,omitempty"`
	Version                  string            `json:"version,omitempty"`
	PayloadExists            bool              `json:"payload_exists"`
	PayloadHashes            map[string]string `json:"payload_hashes,omitempty"`
	PayloadMachines          map[string]string `json:"payload_machines,omitempty"`
	ShortcutRoot             bool              `json:"shortcut_root_exists"`
	ShortcutExists           bool              `json:"shortcut_exists"`
	ShortcutTarget           string            `json:"shortcut_target,omitempty"`
	ShortcutWorkingDirectory string            `json:"shortcut_working_directory,omitempty"`
	ShortcutArguments        string            `json:"shortcut_arguments,omitempty"`
	RunKeyExists             bool              `json:"run_key_exists"`
	RunValueExists           bool              `json:"run_value_exists"`
	RunValue                 string            `json:"run_value,omitempty"`
	RunState                 string            `json:"run_state"`
	SiblingExists            bool              `json:"sibling_exists"`
	SiblingValue             string            `json:"sibling_value,omitempty"`
	ResidentProcess          int               `json:"resident_process_count"`
}

type msiLifecycleInstallExpectation struct {
	ProductCode              string
	Version                  string
	Architecture             string
	PayloadHashes            map[string]string
	ShortcutTarget           string
	ShortcutWorkingDirectory string
	ShortcutArguments        string
	RunState                 string
	RunValue                 string
	SiblingValue             string
}

type msiLifecycleCleanupOwnership struct {
	productsArmed bool
	productsOwned bool
}

type msiRegistrationMetadata struct {
	DisplayName           string
	DisplayNameValid      bool
	Version               string
	VersionValid          bool
	Publisher             string
	PublisherValid        bool
	WindowsInstaller      uint64
	WindowsInstallerValid bool
}

func (ownership *msiLifecycleCleanupOwnership) armProducts() {
	ownership.productsArmed = true
	ownership.productsOwned = true
}

func runMSILifecycleCleanupPass(
	timeout time.Duration,
	ownership *msiLifecycleCleanupOwnership,
	products, fixtures func(context.Context) error,
) error {
	var productErr error
	if ownership.productsArmed {
		productCtx, cancel := context.WithTimeout(context.Background(), timeout)
		productErr = products(productCtx)
		cancel()
		if productErr == nil {
			ownership.productsArmed = false
		}
	}
	fixtureCtx, cancel := context.WithTimeout(context.Background(), timeout)
	fixtureErr := fixtures(fixtureCtx)
	cancel()
	return errors.Join(productErr, fixtureErr)
}

func requireCleanMSILifecycleBaseline(state msiLifecycleState) error {
	if state.ProductCode != "" || state.Version != "" || state.PayloadExists || len(state.PayloadHashes) != 0 || len(state.PayloadMachines) != 0 ||
		state.ShortcutRoot || state.ShortcutExists || state.ShortcutTarget != "" || state.ShortcutWorkingDirectory != "" ||
		state.ShortcutArguments != "" || state.RunValueExists || state.RunValue != "" ||
		state.SiblingExists || state.ResidentProcess != 0 {
		return fmt.Errorf("dedicated account has pre-existing MSI state: %+v", state)
	}
	return nil
}

func drainKnownMSIProducts(
	ctx context.Context,
	quietPeriod, pollInterval time.Duration,
	scan func() ([]string, error),
	uninstall func(context.Context, string) error,
) error {
	if quietPeriod <= 0 || pollInterval <= 0 {
		return errors.New("MSI cleanup quiet period and poll interval must be positive")
	}
	quietSince := time.Now()
	for {
		codes, err := scan()
		if err != nil {
			return err
		}
		if len(codes) != 0 {
			if err := uninstall(ctx, codes[0]); err != nil {
				return err
			}
			quietSince = time.Now()
			continue
		}
		remaining := quietPeriod - time.Since(quietSince)
		if remaining <= 0 {
			return nil
		}
		wait := min(pollInterval, remaining)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func requireMSILifecycleInstallState(state msiLifecycleState, expected msiLifecycleInstallExpectation) error {
	wantMachines := make(map[string]string, len(expected.PayloadHashes))
	for name := range expected.PayloadHashes {
		wantMachines[name] = expected.Architecture
	}
	if state.ProductCode != expected.ProductCode || state.Version != expected.Version || !state.PayloadExists ||
		!equalStringMap(state.PayloadHashes, expected.PayloadHashes) || !equalStringMap(state.PayloadMachines, wantMachines) ||
		!state.ShortcutRoot || !state.ShortcutExists ||
		!strings.EqualFold(state.ShortcutTarget, expected.ShortcutTarget) ||
		!strings.EqualFold(state.ShortcutWorkingDirectory, expected.ShortcutWorkingDirectory) ||
		state.ShortcutArguments != expected.ShortcutArguments || !state.RunKeyExists || !state.RunValueExists ||
		state.RunState != expected.RunState || state.RunValue != expected.RunValue ||
		!state.SiblingExists || state.SiblingValue != expected.SiblingValue || state.ResidentProcess != 0 {
		return fmt.Errorf("MSI state does not match %s/%s with %s Run choice: %+v", expected.Version, expected.Architecture, expected.RunState, state)
	}
	return nil
}

func runAfterValidatedMSILifecycleState(
	state msiLifecycleState,
	expected msiLifecycleInstallExpectation,
	operation func() error,
) (bool, error) {
	if err := requireMSILifecycleInstallState(state, expected); err != nil {
		return false, fmt.Errorf("previous MSI install precondition: %w", err)
	}
	return true, operation()
}

func equalStringMap(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

func canonicalKnownMSIProductCode(value string, known map[string]string) (string, bool) {
	canonical, ok := known[strings.ToUpper(value)]
	return canonical, ok
}

func requireSourceBoundMSIRegistration(metadata msiRegistrationMetadata, expectedVersion string) error {
	if !metadata.DisplayNameValid || metadata.DisplayName != "Codesk" ||
		!metadata.VersionValid || metadata.Version != expectedVersion ||
		!metadata.PublisherValid || metadata.Publisher != "Codesk" ||
		!metadata.WindowsInstallerValid || metadata.WindowsInstaller != 1 {
		return fmt.Errorf("MSI registration does not match source-bound version %s", expectedVersion)
	}
	return nil
}
