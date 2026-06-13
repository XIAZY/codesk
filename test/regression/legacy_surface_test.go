package regression

import (
	"os"
	"strings"
	"testing"
)

func TestRegressionCreateDocumentDoesNotUseLegacyNamespacePayload(t *testing.T) {
	data, err := os.ReadFile("sync_regression_test.go")
	if err != nil {
		t.Fatalf("read regression source: %v", err)
	}
	source := string(data)
	forbidden := []string{
		`map[string]string{"path": path, "content": content}`,
		`map[string]any{"path": path, "content": content}`,
		`map[string]interface{}{"path": path, "content": content}`,
	}
	var matches []string
	for _, token := range forbidden {
		if strings.Contains(source, token) {
			matches = append(matches, token)
		}
	}
	if len(matches) != 0 {
		t.Fatalf("regression createDocument must not send legacy path/content create payload: %v", matches)
	}
}
