package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"notty/daemon/internal/buildinfo"
)

func main() {
	if len(os.Args) < 2 {
		fatalf("usage: notty-agent-tool <list-documents|get-document-by-path|get-thread|list-threads-for-document|list-inbox|get-inbox-item|complete-inbox-item|dismiss-inbox-item|diff-document|mark-document-viewed|create-thread|reply-thread> [flags] (legacy notification aliases are also supported)")
	}
	if os.Args[1] == "--version" || os.Args[1] == "version" {
		fmt.Println(buildinfo.Version)
		return
	}
	if os.Args[1] == "--help" || os.Args[1] == "-h" || os.Args[1] == "help" {
		printUsage(os.Stdout)
		return
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
	render(fetchRaw(baseURL+"/agent-tools/list-documents", token), formatDocuments)
}

func runGetDocumentByPath(baseURL, token string, args []string) {
	fs := flag.NewFlagSet("get-document-by-path", flag.ExitOnError)
	path := fs.String("path", "", "document path")
	_ = fs.Parse(args)
	if strings.TrimSpace(*path) == "" {
		fatalf("path is required")
	}
	render(fetchRaw(baseURL+"/agent-tools/get-document-by-path?path="+url.QueryEscape(*path), token), formatDocument)
}

func runGetThread(baseURL, token string, args []string) {
	fs := flag.NewFlagSet("get-thread", flag.ExitOnError)
	threadID := fs.String("thread-id", "", "thread id")
	_ = fs.Parse(args)
	if strings.TrimSpace(*threadID) == "" {
		fatalf("thread-id is required")
	}
	render(fetchRaw(baseURL+"/agent-tools/get-thread?thread_id="+url.QueryEscape(*threadID), token), formatThread)
}

func runListThreadsForDocument(baseURL, token string, args []string) {
	fs := flag.NewFlagSet("list-threads-for-document", flag.ExitOnError)
	documentID := fs.String("document-id", "", "document id")
	_ = fs.Parse(args)
	if strings.TrimSpace(*documentID) == "" {
		fatalf("document-id is required")
	}
	render(fetchRaw(baseURL+"/agent-tools/list-threads-for-document?document_id="+url.QueryEscape(*documentID), token), formatThreads)
}

func runListNotifications(baseURL, token string) {
	render(fetchRaw(baseURL+"/agent-tools/list-notifications", token), func(data []byte) (string, error) {
		return formatInbox(data, "")
	})
}

func runListInbox(baseURL, token string, args []string) {
	fs := flag.NewFlagSet("list-inbox", flag.ExitOnError)
	box := fs.String("box", "", "inbox box to filter to: for-me, general, or muted (default: all boxes)")
	if err := fs.Parse(args); err != nil {
		fatalf("parse list-inbox flags: %v", err)
	}
	// list-inbox groups the gateway's JSON into labeled per-box sections (task #2). Like every command it
	// renders for reading; the gateway/backend JSON contract is untouched.
	data := fetchRaw(baseURL+"/agent-tools/list-inbox?box="+url.QueryEscape(*box), token)
	formatted, err := formatInbox(data, *box)
	if err != nil {
		fatalf("format inbox: %v", err)
	}
	fmt.Print(formatted)
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
	render(fetchRaw(baseURL+"/agent-tools/get-notification?notification_id="+url.QueryEscape(*notificationID), token), func(data []byte) (string, error) {
		return formatInboxItem(data, "")
	})
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
	render(fetchRaw(baseURL+"/agent-tools/get-inbox-item?item_id="+url.QueryEscape(*itemID), token), func(data []byte) (string, error) {
		return formatInboxItem(data, "")
	})
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
	render(postRaw(baseURL+"/agent-tools/complete-notification?notification_id="+url.QueryEscape(*notificationID), token, ""), func(data []byte) (string, error) {
		return formatInboxItem(data, "marked completed")
	})
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
	render(postRaw(baseURL+"/agent-tools/complete-inbox-item?item_id="+url.QueryEscape(*itemID), token, ""), func(data []byte) (string, error) {
		return formatInboxItem(data, "marked completed")
	})
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
	render(postRaw(baseURL+"/agent-tools/dismiss-notification?notification_id="+url.QueryEscape(*notificationID), token, ""), func(data []byte) (string, error) {
		return formatInboxItem(data, "marked dismissed")
	})
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
	render(postRaw(baseURL+"/agent-tools/dismiss-inbox-item?item_id="+url.QueryEscape(*itemID), token, ""), func(data []byte) (string, error) {
		return formatInboxItem(data, "marked dismissed")
	})
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
	render(fetchRaw(baseURL+"/agent-tools/diff-document?"+query.Encode(), token), formatDiff)
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
	render(postRaw(baseURL+"/agent-tools/mark-document-viewed?document_id="+url.QueryEscape(*documentID), token, ""), formatMarkViewed)
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
	render(postRaw(baseURL+"/agent-tools/subscribe-document?document_id="+url.QueryEscape(*documentID), token, ""), func(data []byte) (string, error) {
		return formatSubscriptions(data, subscribeVerb, *documentID)
	})
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
	render(postRaw(baseURL+"/agent-tools/unsubscribe-document?document_id="+url.QueryEscape(*documentID), token, ""), func(data []byte) (string, error) {
		return formatSubscriptions(data, "unsubscribed from", *documentID)
	})
}

func runListSubscriptions(baseURL, token string) {
	render(fetchRaw(baseURL+"/agent-tools/list-subscriptions", token), func(data []byte) (string, error) {
		return formatSubscriptions(data, "", "")
	})
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
	render(postRaw(baseURL+"/agent-tools/create-thread", token, request), func(data []byte) (string, error) {
		return formatThreadMutation(data, "thread created:")
	})
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
	render(postRaw(baseURL+"/agent-tools/reply-thread", token, request), func(data []byte) (string, error) {
		return formatThreadMutation(data, "reply posted to")
	})
}

// render applies a formatter to a raw response body and prints it, or fails with the formatter error. Every
// command routes through here so the gateway/backend JSON stays untouched while the CLI output is readable.
func render(data []byte, formatter func([]byte) (string, error)) {
	out, err := formatter(data)
	if err != nil {
		fatalf("error: %v", err)
	}
	fmt.Print(out)
}

// fetchRaw performs an authenticated GET and returns the raw response body for the formatters to render.
// A non-2xx status is surfaced as `error: <message>` from the error envelope.
func fetchRaw(url, token string) []byte {
	return doRaw(http.MethodGet, url, token, nil)
}

// postRaw performs an authenticated POST with a JSON payload and returns the raw response body.
func postRaw(url, token string, payload any) []byte {
	body, err := json.Marshal(payload)
	if err != nil {
		fatalf("error: marshal request: %v", err)
	}
	return doRaw(http.MethodPost, url, token, body)
}

func doRaw(method, url, token string, payload []byte) []byte {
	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		fatalf("error: build request: %v", err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		fatalf("error: request failed: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode >= http.StatusBadRequest {
		var failure map[string]any
		_ = json.NewDecoder(res.Body).Decode(&failure)
		fatalf("error: %s", firstString(failure["error"], res.Status))
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		fatalf("error: read response: %v", err)
	}
	return body
}

func firstString(values ...any) string {
	for _, value := range values {
		if text, ok := value.(string); ok && strings.TrimSpace(text) != "" {
			return text
		}
	}
	return ""
}

func printUsage(w io.Writer) {
	lines := []string{
		"notty-agent-tool — the agent CLI to the notty workspace.",
		"",
		"Usage: notty-agent-tool <command> [flags]",
		"",
		"Inbox / notification center:",
		"  list-inbox [--box for-me|general|muted]      list pending items (bare = all boxes)",
		"  get-inbox-item --item-id <id>                full detail of one item",
		"  complete-inbox-item --item-id <id>           mark an item handled (this silences it)",
		"  dismiss-inbox-item --item-id <id>            mark an item ignored without acting",
		"",
		"Document subscriptions:",
		"  subscribe-document --document-id <id>        watch a document — notifications on new edits and thread messages",
		"  unsubscribe-document --document-id <id>      stop watching a document",
		"  list-subscriptions                           list documents you are subscribed to",
		"",
		"Documents & threads:",
		"  diff-document --document-id <id> [--from <v> --to <v>]   show what changed since last viewed",
		"  mark-document-viewed --document-id <id>      advance your viewed watermark",
		"  list-documents                               list documents in the workspace",
		"  get-document-by-path --path <path>           fetch a document by path",
		"  get-thread --thread-id <id>                  fetch a thread",
		"  list-threads-for-document --document-id <id> list a document's threads",
		"  create-thread --path <file> [--line <n>] --body <text>   open a document thread",
		"  reply-thread --thread-id <id> --body <text>  reply in a thread",
	}
	for _, line := range lines {
		_, _ = fmt.Fprintln(w, line)
	}
}

func fatalf(format string, args ...any) {
	_, _ = fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
