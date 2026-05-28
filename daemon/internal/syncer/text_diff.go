package syncer

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/sergi/go-diff/diffmatchpatch"
)

var errUnsupportedTextContent = errors.New("unsupported text content")

type unsupportedTextContentError struct {
	reason string
}

func (e *unsupportedTextContentError) Error() string {
	if e == nil || e.reason == "" {
		return errUnsupportedTextContent.Error()
	}
	return fmt.Sprintf("%s: %s", errUnsupportedTextContent, e.reason)
}

func (e *unsupportedTextContentError) Unwrap() error {
	return errUnsupportedTextContent
}

func unsupportedTextContent(reason string) error {
	return &unsupportedTextContentError{reason: reason}
}

func unsupportedTextContentReason(err error) string {
	var unsupported *unsupportedTextContentError
	if errors.As(err, &unsupported) && unsupported.reason != "" {
		return unsupported.reason
	}
	return err.Error()
}

type localTextEditKind int

const (
	localTextEditInsert localTextEditKind = iota + 1
	localTextEditDelete
)

type localTextEdit struct {
	Kind   localTextEditKind
	Text   string
	Start  int
	Length int
}

func computeLocalTextEdits(baseContent, localContent string) ([]localTextEdit, error) {
	if !utf8.ValidString(baseContent) {
		return nil, unsupportedTextContent("projected base is not valid UTF-8")
	}
	if !utf8.ValidString(localContent) {
		return nil, unsupportedTextContent("local file is not valid UTF-8")
	}
	if strings.IndexByte(baseContent, 0) >= 0 {
		return nil, unsupportedTextContent("projected base contains NUL byte")
	}
	if strings.IndexByte(localContent, 0) >= 0 {
		return nil, unsupportedTextContent("local file contains NUL byte")
	}

	dmp := diffmatchpatch.New()
	diffs := dmp.DiffMain(baseContent, localContent, false)
	edits := make([]localTextEdit, 0, len(diffs))
	cursor := 0
	for _, diff := range diffs {
		length := utf16Length(diff.Text)
		switch diff.Type {
		case diffmatchpatch.DiffEqual:
			cursor += length
		case diffmatchpatch.DiffDelete:
			if length > 0 {
				edits = append(edits, localTextEdit{
					Kind:   localTextEditDelete,
					Text:   diff.Text,
					Start:  cursor,
					Length: length,
				})
			}
		case diffmatchpatch.DiffInsert:
			if diff.Text != "" {
				edits = append(edits, localTextEdit{
					Kind:   localTextEditInsert,
					Text:   diff.Text,
					Start:  cursor,
					Length: length,
				})
				cursor += length
			}
		}
	}
	return edits, nil
}
