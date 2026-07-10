package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Presentation-layer formatting for `list-inbox` (task #2 ergonomics): the gateway still returns raw JSON
// (contract untouched), and this binary renders it into a labeled, per-box structure that an LLM reads
// without burning tokens parsing JSON shape.

type inboxItem struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	Box        string `json:"box"`
	Status     string `json:"status"`
	DocumentID string `json:"documentId"`
	ThreadID   string `json:"threadId"`
	Summary    string `json:"summary"`
	Prompt     string `json:"prompt"`
	CreatedAt  string `json:"createdAt"`
	UpdatedAt  string `json:"updatedAt"`
}

// inboxResponse accepts the current {items} shape and the legacy {notifications} alias so list-inbox and
// list-notifications render identically.
type inboxResponse struct {
	Items         []inboxItem `json:"items"`
	Notifications []inboxItem `json:"notifications"`
}

func (r inboxResponse) items() []inboxItem {
	if len(r.Items) > 0 {
		return r.Items
	}
	return r.Notifications
}

// displayBox maps a normalized box value to its CLI display name.
func displayBox(box string) string {
	switch strings.ToLower(strings.ReplaceAll(strings.TrimSpace(box), "-", "_")) {
	case "general":
		return "general"
	case "muted":
		return "muted"
	default:
		return "for-me"
	}
}

// formatInbox renders the gateway's JSON inbox response. box is the requested filter: "" prints all three
// boxes in order (for-me, general, muted); a specific box prints only that section. Every render ends with
// the action trailer.
func formatInbox(data []byte, box string) (string, error) {
	var resp inboxResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", err
	}
	boxes := []string{"for-me", "general", "muted"}
	if strings.TrimSpace(box) != "" {
		boxes = []string{displayBox(box)}
	}

	allItems := resp.items()
	var b strings.Builder
	for _, display := range boxes {
		items := itemsForBox(allItems, display)
		noun := "items"
		if len(items) == 1 {
			noun = "item"
		}
		b.WriteString(fmt.Sprintf("you have %d %s in the %s inbox", len(items), noun, display))
		if len(items) == 0 {
			b.WriteString("\n\n")
			continue
		}
		b.WriteString(":\n")
		for _, item := range items {
			// list-inbox groups by box, so the per-item box/status lines are redundant here; get-inbox-item
			// (a single item, ungrouped) shows them.
			writeInboxItemBlock(&b, item, false)
		}
		b.WriteString("\n")
	}
	b.WriteString(inboxActionTrailer)
	return b.String(), nil
}

// inboxActionTrailer is AlphaToad's action sentence, shared by every inbox render.
const inboxActionTrailer = "if you need to, mark them as resolved by using complete-inbox-item --item-id <id>, or mark a document as viewed using mark-document-viewed --document-id <id>. for other usages, use notty-agent-tool --help\n"

// writeInboxItemBlock renders one inbox item as a labeled block. showBoxStatus adds box + status lines, which
// list-inbox omits (its items are grouped under a box header) but single-item views include.
func writeInboxItemBlock(b *strings.Builder, item inboxItem, showBoxStatus bool) {
	b.WriteString("- id: " + item.ID + "\n")
	b.WriteString("  type: " + item.Type + "\n")
	if showBoxStatus {
		b.WriteString("  box: " + displayBox(item.Box) + "\n")
		if item.Status != "" {
			b.WriteString("  status: " + item.Status + "\n")
		}
	}
	if item.DocumentID != "" {
		b.WriteString("  documentId: " + item.DocumentID + "\n")
	}
	if item.ThreadID != "" {
		b.WriteString("  threadId: " + item.ThreadID + "\n")
	}
	if item.Summary != "" {
		b.WriteString("  summary: " + item.Summary + "\n")
	}
	if item.Prompt != "" {
		b.WriteString("  details: " + item.Prompt + "\n")
	}
	b.WriteString("  created: " + item.CreatedAt + "  updated: " + item.UpdatedAt + "\n")
}

func itemsForBox(items []inboxItem, display string) []inboxItem {
	out := []inboxItem{}
	for _, item := range items {
		if displayBox(item.Box) == display {
			out = append(out, item)
		}
	}
	return out
}
