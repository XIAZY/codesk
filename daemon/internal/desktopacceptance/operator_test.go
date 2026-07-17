package desktopacceptance

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func TestInteractiveOperatorRejectsClosedInput(t *testing.T) {
	var output bytes.Buffer
	err := (InteractiveOperator{Input: strings.NewReader(""), Output: &output}).Perform(
		context.Background(),
		ActionConnect,
		"perform the native action",
	)
	var blocked blockedError
	if !errors.As(err, &blocked) || !strings.Contains(err.Error(), "input closed") {
		t.Fatalf("Perform error = %v, want closed-input block", err)
	}
}

func TestInteractiveOperatorAcceptsOnlyEmptyLine(t *testing.T) {
	var output bytes.Buffer
	operator := InteractiveOperator{Input: strings.NewReader("\n"), Output: &output}
	if err := operator.Perform(context.Background(), ActionConnect, "perform the native action"); err != nil {
		t.Fatal(err)
	}
}
