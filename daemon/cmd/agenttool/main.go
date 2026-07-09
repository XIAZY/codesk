package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
)

func main() {
	if len(os.Args) < 2 {
		fatalf("usage: notty-agent-tool <list-documents|get-document-by-path|get-thread|list-threads-for-document|list-inbox|get-inbox-item|complete-inbox-item|dismiss-inbox-item|diff-document|mark-document-viewed|create-thread|reply-thread> [flags] (legacy notification aliases are also supported)")
	}
	baseURL := strings.TrimSpace(os.Getenv("NOTTY_AGENT_TOOL_BASE_URL"))
	token := strings.TrimSpace(os.Getenv("NOTTY_AGENT_TOOL_TOKEN"))
	if baseURL == "" || token == "" {
		fatalf("missing NOTTY_AGENT_TOOL_BASE_URL or NOTTY_AGENT_TOOL_TOKEN")
	}

	switch os.Args[1] {
	case "list-documents":
		runListDocuments(baseURL, token)
	case "get-document-by-path":
		runGetDocumentByPath(baseURL, token, os.Args[2:])
	case "get-thread":
		runGetThread(baseURL, token, os.Args[2:])
	case "list-threads-for-document":
		runListThreadsForDocument(baseURL, token, os.Args[2:])
	case "list-notifications":
		runListNotifications(baseURL, token)
	case "get-notification":
		runGetNotification(baseURL, token, os.Args[2:])
	case "complete-notification":
		runCompleteNotification(baseURL, token, os.Args[2:])
	case "dismiss-notification":
		runDismissNotification(baseURL, token, os.Args[2:])
	case "list-inbox":
		runListInbox(baseURL, token, os.Args[2:])
	case "get-inbox-item":
		runGetInboxItem(baseURL, token, os.Args[2:])
	case "complete-inbox-item":
		runCompleteInboxItem(baseURL, token, os.Args[2:])
	case "dismiss-inbox-item":
		runDismissInboxItem(baseURL, token, os.Args[2:])
	case "diff-document":
		runDiffDocument(baseURL, token, os.Args[2:])
	case "mark-document-viewed":
		runMarkDocumentViewed(baseURL, token, os.Args[2:])
	case "create-thread":
		runCreateThread(baseURL, token, os.Args[2:])
	case "reply-thread":
		runReplyThread(baseURL, token, os.Args[2:])
	case "subscribe-document":
		runSubscribeDocument(baseURL, token, os.Args[2:])
	case "unsubscribe-document":
		runUnsubscribeDocument(baseURL, token, os.Args[2:])
	case "list-subscriptions":
		runListSubscriptions(baseURL, token)
	default:
		fatalf("unknown command %q", os.Args[1])
	}
}

func runListDocuments(baseURL, token string) {
	getJSON(baseURL+"/agent-tools/list-documents", token)
}

func runGetDocumentByPath(baseURL, token string, args []string) {
	fs := flag.NewFlagSet("get-document-by-path", flag.ExitOnError)
	path := fs.String("path", "", "document path")
	_ = fs.Parse(args)
	if strings.TrimSpace(*path) == "" {
		fatalf("path is required")
	}
	getJSON(baseURL+"/agent-tools/get-document-by-path?path="+url.QueryEscape(*path), token)
}

func runGetThread(baseURL, token string, args []string) {
	fs := flag.NewFlagSet("get-thread", flag.ExitOnError)
	threadID := fs.String("thread-id", "", "thread id")
	_ = fs.Parse(args)
	if strings.TrimSpace(*threadID) == "" {
		fatalf("thread-id is required")
	}
	getJSON(baseURL+"/agent-tools/get-thread?thread_id="+url.QueryEscape(*threadID), token)
}

func runListThreadsForDocument(baseURL, token string, args []string) {
	fs := flag.NewFlagSet("list-threads-for-document", flag.ExitOnError)
	documentID := fs.String("document-id", "", "document id")
	_ = fs.Parse(args)
	if strings.TrimSpace(*documentID) == "" {
		fatalf("document-id is required")
	}
	getJSON(baseURL+"/agent-tools/list-threads-for-document?document_id="+url.QueryEscape(*documentID), token)
}

func runListNotifications(baseURL, token string) {
	getJSON(baseURL+"/agent-tools/list-notifications", token)
}

func runListInbox(baseURL, token string, args []string) {
	fs := flag.NewFlagSet("list-inbox", flag.ExitOnError)
	box := fs.String("box", "for-me", "inbox box: for-me or general")
	if err := fs.Parse(args); err != nil {
		fatalf("parse list-inbox flags: %v", err)
	}
	getJSON(baseURL+"/agent-tools/list-inbox?box="+url.QueryEscape(*box), token)
}

func runGetNotification(baseURL, token string, args []string) {
	fs := flag.NewFlagSet("get-notification", flag.ExitOnError)
	notificationID := fs.String("notification-id", "", "notification id")
	if err := fs.Parse(args); err != nil {
		fatalf("parse get-notification flags: %v", err)
	}
	if *notificationID == "" {
		fatalf("--notification-id is required")
	}
	getJSON(baseURL+"/agent-tools/get-notification?notification_id="+url.QueryEscape(*notificationID), token)
}

func runGetInboxItem(baseURL, token string, args []string) {
	fs := flag.NewFlagSet("get-inbox-item", flag.ExitOnError)
	itemID := fs.String("item-id", "", "inbox item id")
	if err := fs.Parse(args); err != nil {
		fatalf("parse get-inbox-item flags: %v", err)
	}
	if *itemID == "" {
		fatalf("--item-id is required")
	}
	getJSON(baseURL+"/agent-tools/get-inbox-item?item_id="+url.QueryEscape(*itemID), token)
}

func runCompleteNotification(baseURL, token string, args []string) {
	fs := flag.NewFlagSet("complete-notification", flag.ExitOnError)
	notificationID := fs.String("notification-id", "", "notification id")
	if err := fs.Parse(args); err != nil {
		fatalf("parse complete-notification flags: %v", err)
	}
	if *notificationID == "" {
		fatalf("--notification-id is required")
	}
	postJSON(baseURL+"/agent-tools/complete-notification?notification_id="+url.QueryEscape(*notificationID), token, "")
}

func runCompleteInboxItem(baseURL, token string, args []string) {
	fs := flag.NewFlagSet("complete-inbox-item", flag.ExitOnError)
	itemID := fs.String("item-id", "", "inbox item id")
	if err := fs.Parse(args); err != nil {
		fatalf("parse complete-inbox-item flags: %v", err)
	}
	if *itemID == "" {
		fatalf("--item-id is required")
	}
	postJSON(baseURL+"/agent-tools/complete-inbox-item?item_id="+url.QueryEscape(*itemID), token, "")
}

func runDismissNotification(baseURL, token string, args []string) {
	fs := flag.NewFlagSet("dismiss-notification", flag.ExitOnError)
	notificationID := fs.String("notification-id", "", "notification id")
	if err := fs.Parse(args); err != nil {
		fatalf("parse dismiss-notification flags: %v", err)
	}
	if *notificationID == "" {
		fatalf("--notification-id is required")
	}
	postJSON(baseURL+"/agent-tools/dismiss-notification?notification_id="+url.QueryEscape(*notificationID), token, "")
}

func runDismissInboxItem(baseURL, token string, args []string) {
	fs := flag.NewFlagSet("dismiss-inbox-item", flag.ExitOnError)
	itemID := fs.String("item-id", "", "inbox item id")
	if err := fs.Parse(args); err != nil {
		fatalf("parse dismiss-inbox-item flags: %v", err)
	}
	if *itemID == "" {
		fatalf("--item-id is required")
	}
	postJSON(baseURL+"/agent-tools/dismiss-inbox-item?item_id="+url.QueryEscape(*itemID), token, "")
}

func runDiffDocument(baseURL, token string, args []string) {
	fs := flag.NewFlagSet("diff-document", flag.ExitOnError)
	documentID := fs.String("document-id", "", "document id")
	from := fs.String("from", "", "from version: last-viewed, head, or update id")
	to := fs.String("to", "", "to version: head or update id")
	if err := fs.Parse(args); err != nil {
		fatalf("parse diff-document flags: %v", err)
	}
	if strings.TrimSpace(*documentID) == "" {
		fatalf("--document-id is required")
	}
	query := url.Values{"document_id": []string{*documentID}}
	if strings.TrimSpace(*from) != "" {
		query.Set("from", *from)
	}
	if strings.TrimSpace(*to) != "" {
		query.Set("to", *to)
	}
	getJSON(baseURL+"/agent-tools/diff-document?"+query.Encode(), token)
}

func runMarkDocumentViewed(baseURL, token string, args []string) {
	fs := flag.NewFlagSet("mark-document-viewed", flag.ExitOnError)
	documentID := fs.String("document-id", "", "document id")
	if err := fs.Parse(args); err != nil {
		fatalf("parse mark-document-viewed flags: %v", err)
	}
	if strings.TrimSpace(*documentID) == "" {
		fatalf("--document-id is required")
	}
	postJSON(baseURL+"/agent-tools/mark-document-viewed?document_id="+url.QueryEscape(*documentID), token, "")
}

func runSubscribeDocument(baseURL, token string, args []string) {
	fs := flag.NewFlagSet("subscribe-document", flag.ExitOnError)
	documentID := fs.String("document-id", "", "document id to subscribe to")
	if err := fs.Parse(args); err != nil {
		fatalf("parse subscribe-document flags: %v", err)
	}
	if strings.TrimSpace(*documentID) == "" {
		fatalf("--document-id is required")
	}
	postJSON(baseURL+"/agent-tools/subscribe-document?document_id="+url.QueryEscape(*documentID), token, "")
}

func runUnsubscribeDocument(baseURL, token string, args []string) {
	fs := flag.NewFlagSet("unsubscribe-document", flag.ExitOnError)
	documentID := fs.String("document-id", "", "document id to unsubscribe from")
	if err := fs.Parse(args); err != nil {
		fatalf("parse unsubscribe-document flags: %v", err)
	}
	if strings.TrimSpace(*documentID) == "" {
		fatalf("--document-id is required")
	}
	postJSON(baseURL+"/agent-tools/unsubscribe-document?document_id="+url.QueryEscape(*documentID), token, "")
}

func runListSubscriptions(baseURL, token string) {
	getJSON(baseURL+"/agent-tools/list-subscriptions", token)
}

func runCreateThread(baseURL, token string, args []string) {
	fs := flag.NewFlagSet("create-thread", flag.ExitOnError)
	documentID := fs.String("document-id", "", "document id")
	path := fs.String("path", "", "document path")
	document := fs.Bool("document", false, "create a document-level thread")
	line := fs.Int("line", 0, "1-based line to anchor")
	quote := fs.String("quote", "", "exact text to anchor; combine with --line when repeated")
	startLine := fs.Int("start-line", 0, "1-based start line for a precise range")
	startColumn := fs.Int("start-column", 0, "1-based UTF-16 start column for a precise range")
	endLine := fs.Int("end-line", 0, "1-based end line for a precise range")
	endColumn := fs.Int("end-column", 0, "1-based UTF-16 exclusive end column for a precise range")
	start := fs.Int("start", 0, "UTF-16 start offset")
	end := fs.Int("end", 0, "UTF-16 exclusive end offset")
	excerpt := fs.String("excerpt", "", "display excerpt override")
	title := fs.String("title", "", "thread title")
	body := fs.String("body", "", "thread body")
	_ = fs.Parse(args)
	if (strings.TrimSpace(*documentID) == "") == (strings.TrimSpace(*path) == "") {
		fatalf("exactly one of --document-id or --path is required")
	}
	if strings.TrimSpace(*body) == "" {
		fatalf("body is required")
	}
	request := map[string]any{
		"documentId":  *documentID,
		"path":        *path,
		"document":    *document,
		"line":        *line,
		"quote":       *quote,
		"startLine":   *startLine,
		"startColumn": *startColumn,
		"endLine":     *endLine,
		"endColumn":   *endColumn,
		"start":       *start,
		"end":         *end,
		"excerpt":     *excerpt,
		"title":       *title,
		"body":        *body,
	}
	postJSON(baseURL+"/agent-tools/create-thread", token, request)
}

func runReplyThread(baseURL, token string, args []string) {
	fs := flag.NewFlagSet("reply-thread", flag.ExitOnError)
	threadID := fs.String("thread-id", "", "thread id")
	body := fs.String("body", "", "reply body")
	kind := fs.String("kind", "comment", "reply kind")
	_ = fs.Parse(args)
	if strings.TrimSpace(*threadID) == "" || strings.TrimSpace(*body) == "" {
		fatalf("thread-id and body are required")
	}
	request := map[string]any{
		"threadId": *threadID,
		"body":     *body,
		"kind":     *kind,
	}
	postJSON(baseURL+"/agent-tools/reply-thread", token, request)
}

func postJSON(url, token string, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		fatalf("marshal request: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		fatalf("request failed: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode >= http.StatusBadRequest {
		var failure map[string]any
		_ = json.NewDecoder(res.Body).Decode(&failure)
		fatalf("request failed: %s", firstString(failure["error"], res.Status))
	}
	var response any
	if err := json.NewDecoder(res.Body).Decode(&response); err != nil {
		fatalf("decode response: %v", err)
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(response); err != nil {
		fatalf("encode response: %v", err)
	}
}

func getJSON(url, token string) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		fatalf("request failed: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode >= http.StatusBadRequest {
		var failure map[string]any
		_ = json.NewDecoder(res.Body).Decode(&failure)
		fatalf("request failed: %s", firstString(failure["error"], res.Status))
	}
	var response any
	if err := json.NewDecoder(res.Body).Decode(&response); err != nil {
		fatalf("decode response: %v", err)
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(response); err != nil {
		fatalf("encode response: %v", err)
	}
}

func firstString(values ...any) string {
	for _, value := range values {
		if text, ok := value.(string); ok && strings.TrimSpace(text) != "" {
			return text
		}
	}
	return ""
}

func fatalf(format string, args ...any) {
	_, _ = fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
