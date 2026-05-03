package syncer

import (
	"strings"
	"testing"
)

func TestResolveThreadTargetFindsQuoteOnSpecificLine(t *testing.T) {
	content := "intro\nrepeat target\nrepeat target\n"

	target, err := resolveThreadTarget(content, createThreadPayload{
		Line:  2,
		Quote: "target",
	})
	if err != nil {
		t.Fatalf("resolve target: %v", err)
	}
	if target.start != 13 || target.end != 19 || target.line != 2 || target.excerpt != "target" {
		t.Fatalf("unexpected target: %#v", target)
	}
}

func TestResolveThreadTargetRejectsAmbiguousQuoteWithoutLine(t *testing.T) {
	_, err := resolveThreadTarget("repeat target\nrepeat target\n", createThreadPayload{
		Quote: "target",
	})
	if err == nil || !strings.Contains(err.Error(), "multiple times") {
		t.Fatalf("expected ambiguous quote error, got %v", err)
	}
}

func TestResolveThreadTargetUsesUTF16LineColumns(t *testing.T) {
	target, err := resolveThreadTarget("a😀b\n", createThreadPayload{
		StartLine:   1,
		StartColumn: 2,
		EndLine:     1,
		EndColumn:   4,
	})
	if err != nil {
		t.Fatalf("resolve target: %v", err)
	}
	if target.start != 1 || target.end != 3 || target.excerpt != "😀" {
		t.Fatalf("expected emoji range in UTF-16 units, got %#v", target)
	}
}
