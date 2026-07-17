package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestMSILifecycleBaselineFailureCannotMutatePreExistingProduct(t *testing.T) {
	state := msiLifecycleState{
		ProductCode:    "{11111111-1111-4111-8111-111111111111}",
		Version:        "1.0.0",
		PayloadExists:  true,
		PayloadHashes:  map[string]string{"Codesk.exe": "pre-existing"},
		ShortcutExists: true,
		RunKeyExists:   true,
		RunValueExists: true,
		RunValue:       `"C:\Program Files\Codesk\Codesk.exe"`,
		RunState:       "other",
	}
	original := cloneMSILifecycleState(state)
	if err := requireCleanMSILifecycleBaseline(state); err == nil {
		t.Fatal("pre-existing Codesk MSI unexpectedly passed the clean-account baseline")
	}

	var productCleanupCalls int
	var fixtureCleanupCalls int
	ownership := msiLifecycleCleanupOwnership{}
	for pass := 0; pass < 2; pass++ {
		if err := runMSILifecycleCleanupPass(time.Second, &ownership, func(context.Context) error {
			productCleanupCalls++
			state = msiLifecycleState{}
			return nil
		}, func(ctx context.Context) error {
			if _, ok := ctx.Deadline(); !ok {
				t.Fatal("fixture cleanup context has no deadline")
			}
			fixtureCleanupCalls++
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if productCleanupCalls != 0 || fixtureCleanupCalls != 2 {
		t.Fatalf("baseline failure cleanup calls: products=%d fixtures=%d", productCleanupCalls, fixtureCleanupCalls)
	}
	if !reflect.DeepEqual(state, original) {
		t.Fatalf("baseline failure mutated pre-existing product state: got=%+v want=%+v", state, original)
	}
}

func TestKnownProductCodeWithCorruptARPMetadataFailsWithoutMutation(t *testing.T) {
	const productCode = "{ABCDEF11-1111-4111-8111-111111111111}"
	known := map[string]string{productCode: productCode}
	canonical, recognized := canonicalKnownMSIProductCode(strings.ToLower(productCode), known)
	if !recognized || canonical != productCode {
		t.Fatalf("recognized=%t canonical=%q", recognized, canonical)
	}
	metadata := msiRegistrationMetadata{
		DisplayName: "Corrupt", DisplayNameValid: true,
		Version: "1.0.0", VersionValid: true,
		Publisher: "Codesk", PublisherValid: true,
		WindowsInstaller: 1, WindowsInstallerValid: true,
	}
	if err := requireSourceBoundMSIRegistration(metadata, "1.0.0"); err == nil {
		t.Fatal("recognized ProductCode with corrupt ARP metadata unexpectedly passed")
	}
	state := msiLifecycleState{ProductCode: canonical, Version: "1.0.0", PayloadExists: true}
	original := cloneMSILifecycleState(state)
	var productCleanupCalls int
	ownership := msiLifecycleCleanupOwnership{}
	if err := runMSILifecycleCleanupPass(time.Second, &ownership, func(context.Context) error {
		productCleanupCalls++
		state = msiLifecycleState{}
		return nil
	}, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if productCleanupCalls != 0 || !reflect.DeepEqual(state, original) {
		t.Fatalf("baseline rejection mutated recognized corrupt registration: calls=%d got=%+v want=%+v", productCleanupCalls, state, original)
	}
}

func TestOwnedKnownProductCodeCleanupIgnoresCorruptARPDisplayName(t *testing.T) {
	const productCode = "{ABCDEF11-1111-4111-8111-111111111111}"
	known := map[string]string{productCode: productCode}
	canonical, recognized := canonicalKnownMSIProductCode(strings.ToLower(productCode), known)
	if !recognized {
		t.Fatal("owned ProductCode was not recognized independently of ARP metadata")
	}
	ownership := msiLifecycleCleanupOwnership{}
	ownership.armProducts()
	var removed string
	if err := runMSILifecycleCleanupPass(time.Second, &ownership, func(context.Context) error {
		removed = canonical
		return nil
	}, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if removed != productCode || ownership.productsArmed {
		t.Fatalf("removed=%q ownership=%+v", removed, ownership)
	}
}

func TestMSILifecycleProductCleanupRequiresExplicitOwnership(t *testing.T) {
	if err := requireCleanMSILifecycleBaseline(msiLifecycleState{}); err != nil {
		t.Fatal(err)
	}
	ownership := msiLifecycleCleanupOwnership{}
	ownership.armProducts()
	state := msiLifecycleState{
		ProductCode:   "{11111111-1111-4111-8111-111111111111}",
		PayloadExists: true,
		RunKeyExists:  true,
	}
	var productCleanupCalls int
	var fixtureCleanupCalls int
	fixtureArmed := true
	for pass := 0; pass < 2; pass++ {
		if err := runMSILifecycleCleanupPass(time.Second, &ownership, func(ctx context.Context) error {
			if _, ok := ctx.Deadline(); !ok {
				t.Fatal("product cleanup context has no deadline")
			}
			productCleanupCalls++
			state.ProductCode = ""
			state.PayloadExists = false
			return nil
		}, func(ctx context.Context) error {
			if _, ok := ctx.Deadline(); !ok {
				t.Fatal("fixture cleanup context has no deadline")
			}
			fixtureCleanupCalls++
			fixtureArmed = false
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if productCleanupCalls != 1 {
		t.Fatalf("product cleanup calls = %d, want 1", productCleanupCalls)
	}
	if state.ProductCode != "" || state.PayloadExists {
		t.Fatalf("partial product install remained after cleanup: %+v", state)
	}
	if fixtureCleanupCalls != 2 || fixtureArmed {
		t.Fatalf("fixture cleanup was not independently idempotent: calls=%d armed=%t", fixtureCleanupCalls, fixtureArmed)
	}
	if ownership.productsArmed || !ownership.productsOwned {
		t.Fatalf("cleanup ownership = %+v, want owned but no longer armed", ownership)
	}
}

func TestMSILifecycleFailedProductCleanupRetainsOwnershipForRetry(t *testing.T) {
	ownership := msiLifecycleCleanupOwnership{}
	ownership.armProducts()
	var productCleanupCalls int
	var fixtureCleanupCalls int
	cleanup := func() error {
		return runMSILifecycleCleanupPass(time.Second, &ownership, func(context.Context) error {
			productCleanupCalls++
			if productCleanupCalls == 1 {
				return errors.New("injected partial MSI cleanup failure")
			}
			return nil
		}, func(context.Context) error {
			fixtureCleanupCalls++
			return nil
		})
	}
	if err := cleanup(); err == nil || !ownership.productsArmed {
		t.Fatalf("first cleanup err=%v ownership=%+v", err, ownership)
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	if productCleanupCalls != 2 || fixtureCleanupCalls != 2 || ownership.productsArmed {
		t.Fatalf("cleanup retry calls: products=%d fixtures=%d ownership=%+v", productCleanupCalls, fixtureCleanupCalls, ownership)
	}
}

func TestMSILifecycleCleanupCatchesLateInstallerRegistration(t *testing.T) {
	var scans int
	var removed []string
	var events []string
	err := drainKnownMSIProducts(
		context.Background(),
		100*time.Millisecond,
		time.Millisecond,
		func() ([]string, error) {
			scans++
			if scans == 3 {
				events = append(events, "scan-late")
				return []string{"{11111111-1111-4111-8111-111111111111}"}, nil
			}
			events = append(events, "scan-empty")
			return nil, nil
		},
		func(_ context.Context, code string) error {
			events = append(events, "uninstall")
			removed = append(removed, code)
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if scans < 4 || !reflect.DeepEqual(removed, []string{"{11111111-1111-4111-8111-111111111111}"}) {
		t.Fatalf("late registration was not drained: scans=%d removed=%v", scans, removed)
	}
	wantPrefix := []string{"scan-empty", "scan-empty", "scan-late", "uninstall", "scan-empty"}
	if len(events) < len(wantPrefix) || !reflect.DeepEqual(events[:len(wantPrefix)], wantPrefix) {
		t.Fatalf("late registration cleanup order = %v, want prefix %v", events, wantPrefix)
	}
}

func TestCorruptPreviousMSIStateStopsUpgradeBeforeCandidateExecution(t *testing.T) {
	expected := msiLifecycleInstallExpectation{
		ProductCode:  "{11111111-1111-4111-8111-111111111111}",
		Version:      "1.0.0",
		Architecture: "amd64",
		PayloadHashes: map[string]string{
			"Codesk.exe":           "codesk-previous",
			"notty-agent-tool.exe": "agent-previous",
		},
		ShortcutTarget:           `C:\Users\qa\AppData\Local\Programs\Codesk\Codesk.exe`,
		ShortcutWorkingDirectory: `C:\Users\qa\AppData\Local\Programs\Codesk`,
		RunState:                 "enabled",
		RunValue:                 `"C:\Users\qa\AppData\Local\Programs\Codesk\Codesk.exe"`,
		SiblingValue:             "preserve-this-sibling-value",
	}
	state := msiLifecycleState{
		ProductCode:   expected.ProductCode,
		Version:       expected.Version,
		PayloadExists: true,
		PayloadHashes: cloneStringMap(expected.PayloadHashes),
		PayloadMachines: map[string]string{
			"Codesk.exe":           "amd64",
			"notty-agent-tool.exe": "amd64",
		},
		ShortcutRoot:             true,
		ShortcutExists:           true,
		ShortcutTarget:           expected.ShortcutTarget,
		ShortcutWorkingDirectory: expected.ShortcutWorkingDirectory,
		RunKeyExists:             true,
		RunValueExists:           true,
		RunValue:                 expected.RunValue,
		RunState:                 expected.RunState,
		SiblingExists:            true,
		SiblingValue:             expected.SiblingValue,
	}
	state.PayloadHashes["Codesk.exe"] = "causal-corruption"
	var candidateExecutions int
	started, err := runAfterValidatedMSILifecycleState(state, expected, func() error {
		candidateExecutions++
		return nil
	})
	if err == nil || started || candidateExecutions != 0 {
		t.Fatalf("corrupt prior state reached candidate: started=%t err=%v executions=%d", started, err, candidateExecutions)
	}

	state.PayloadHashes["Codesk.exe"] = expected.PayloadHashes["Codesk.exe"]
	started, err = runAfterValidatedMSILifecycleState(state, expected, func() error {
		candidateExecutions++
		return nil
	})
	if err != nil || !started || candidateExecutions != 1 {
		t.Fatalf("valid prior state did not reach candidate exactly once: started=%t err=%v executions=%d", started, err, candidateExecutions)
	}
}

func cloneMSILifecycleState(state msiLifecycleState) msiLifecycleState {
	clone := state
	if state.PayloadHashes != nil {
		clone.PayloadHashes = cloneStringMap(state.PayloadHashes)
	}
	if state.PayloadMachines != nil {
		clone.PayloadMachines = cloneStringMap(state.PayloadMachines)
	}
	return clone
}

func cloneStringMap(values map[string]string) map[string]string {
	clone := make(map[string]string, len(values))
	for name, value := range values {
		clone[name] = value
	}
	return clone
}
