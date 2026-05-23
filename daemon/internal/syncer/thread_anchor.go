package syncer

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf16"

	crdt "notty/internal/ycrdt"
)

type resolvedThreadTarget struct {
	start   int
	end     int
	line    int
	excerpt string
}

func (s *Service) prepareCreateThreadPayload(ctx context.Context, run *agentRun, payload createThreadPayload) (createThreadPayload, error) {
	if strings.TrimSpace(payload.Body) == "" {
		return payload, fmt.Errorf("thread body is required")
	}
	document, err := s.resolveThreadDocument(ctx, payload)
	if err != nil {
		return payload, err
	}
	payload.DocumentID = document.ID
	payload.Path = ""

	if payload.Document {
		if hasThreadRangeTarget(payload) || strings.TrimSpace(payload.Quote) != "" {
			return payload, fmt.Errorf("--document cannot be combined with line, range, quote, or offset anchors")
		}
		payload.Kind = "document"
		payload.RelativeStart = ""
		payload.RelativeEnd = ""
		payload.Start = 0
		payload.End = 0
		payload.Line = 0
		payload.Excerpt = ""
		return payload, nil
	}

	if strings.TrimSpace(payload.RelativeStart) != "" || strings.TrimSpace(payload.RelativeEnd) != "" {
		if strings.TrimSpace(payload.RelativeStart) == "" || strings.TrimSpace(payload.RelativeEnd) == "" {
			return payload, fmt.Errorf("relativeStart and relativeEnd must be provided together")
		}
		payload.Kind = "text-range"
		return payload, nil
	}

	doc, err := s.loadThreadAnchorDoc(ctx, run, document)
	if err != nil {
		return payload, err
	}
	defer doc.Close()
	text := doc.GetText("content")
	content := text.ToString()
	target, err := resolveThreadTarget(content, payload)
	if err != nil {
		return payload, err
	}
	if target.start < 0 || target.end < target.start || target.end > text.Len() {
		return payload, fmt.Errorf("resolved thread range [%d,%d] is outside document length %d", target.start, target.end, text.Len())
	}
	payload.RelativeStart = encodeRelativeAnchor(text, target.start)
	payload.RelativeEnd = encodeRelativeAnchor(text, target.end)
	payload.Kind = "text-range"
	payload.Start = target.start
	payload.End = target.end
	payload.Line = target.line
	payload.Excerpt = target.excerpt
	payload.Quote = ""
	payload.StartLine = 0
	payload.StartColumn = 0
	payload.EndLine = 0
	payload.EndColumn = 0
	return payload, nil
}

func (s *Service) loadThreadAnchorDoc(ctx context.Context, run *agentRun, document *document) (*crdt.Doc, error) {
	if document == nil || strings.TrimSpace(document.ID) == "" {
		return nil, fmt.Errorf("document is required")
	}
	state := s.threadAnchorState(run)
	if state == nil {
		return nil, fmt.Errorf("workspace stream state is unavailable")
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	projection, err := state.GetContentProjection(ctx, document.ID)
	if err != nil {
		return nil, err
	}
	if projection != nil && projection.Dirty {
		return nil, fmt.Errorf("document %s has pending local edits; wait for sync before creating an anchored thread", document.Path)
	}
	blocked, err := state.HasBlockingFSJob(ctx, document.ID)
	if err != nil {
		return nil, err
	}
	if blocked {
		return nil, fmt.Errorf("document %s has pending filesystem projection work; wait for sync before creating an anchored thread", document.Path)
	}
	remote, err := state.UnappliedInbox(ctx, document.ID, 1)
	if err != nil {
		return nil, err
	}
	if len(remote) > 0 {
		return nil, fmt.Errorf("document has pending remote update(s); wait for reconciliation before creating an anchored thread")
	}
	local, err := state.PendingOutboxCount(ctx, document.ID)
	if err != nil {
		return nil, err
	}
	if local > 0 {
		return nil, fmt.Errorf("document %s has pending local update(s); wait for sync before creating an anchored thread", document.Path)
	}
	stream, err := state.GetStream(ctx, document.ID)
	if errors.Is(err, sql.ErrNoRows) {
		if document.UpdateID != 0 {
			return nil, fmt.Errorf("document %s is not available in daemon stream state yet", document.Path)
		}
		return crdt.New(crdt.WithGUID(document.ID)), nil
	}
	if err != nil {
		return nil, err
	}
	if !stream.LatestStateID.Valid && document.UpdateID != 0 {
		return nil, fmt.Errorf("document %s is not available in daemon stream state yet", document.Path)
	}
	return state.LoadStreamDoc(ctx, document.ID, stream.LatestStateID)
}

func (s *Service) threadAnchorState(run *agentRun) *WorkspaceStateDB {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if run != nil && strings.TrimSpace(run.AgentID) != "" {
		if managed := s.agentStreams[run.AgentID]; managed != nil && managed.projection != nil && managed.projection.state != nil {
			return managed.projection.state
		}
	}
	if s.primaryStream != nil && s.primaryStream.state != nil {
		return s.primaryStream.state
	}
	return s.state
}

func (s *Service) resolveThreadDocument(ctx context.Context, payload createThreadPayload) (*document, error) {
	documentID := strings.TrimSpace(payload.DocumentID)
	path := strings.TrimSpace(payload.Path)
	if documentID == "" && path == "" {
		return nil, fmt.Errorf("document-id or path is required")
	}
	if documentID != "" && path != "" {
		return nil, fmt.Errorf("provide only one of document-id or path")
	}
	if doc := s.findLatestDocument(documentID, path); doc != nil {
		return doc, nil
	}
	if err := s.refreshLatestWorkspaceForLookup(ctx); err != nil {
		return nil, err
	}
	if doc := s.findLatestDocument(documentID, path); doc != nil {
		return doc, nil
	}
	if path != "" {
		return s.fetchDocumentByPath(ctx, path)
	}
	return nil, fmt.Errorf("document %s not found", documentID)
}

func (s *Service) findLatestDocument(documentID, path string) *document {
	s.mu.Lock()
	workspace := s.latestWorkspace
	s.mu.Unlock()
	if workspace == nil {
		return nil
	}
	for _, doc := range workspace.Documents {
		if doc == nil {
			continue
		}
		if documentID != "" && doc.ID == documentID {
			copy := *doc
			return &copy
		}
		if path != "" && doc.Path == path {
			copy := *doc
			return &copy
		}
	}
	return nil
}

func (s *Service) refreshLatestWorkspaceForLookup(ctx context.Context) error {
	workspace, err := s.fetchWorkspace(ctx)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.latestWorkspace = workspace
	s.mu.Unlock()
	return nil
}

func (s *Service) fetchDocumentByPath(ctx context.Context, path string) (*document, error) {
	req, err := s.newBackendRequest(ctx, http.MethodGet, "/api/documents/by-path?path="+url.QueryEscape(path), nil)
	if err != nil {
		return nil, err
	}
	res, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode >= http.StatusBadRequest {
		return nil, fmt.Errorf("get document by path failed: %s", res.Status)
	}
	var response toolDocumentResponse
	if err := json.NewDecoder(res.Body).Decode(&response); err != nil {
		return nil, err
	}
	if response.Document == nil {
		return nil, fmt.Errorf("document path %q not found", path)
	}
	return response.Document, nil
}

func resolveThreadTarget(content string, payload createThreadPayload) (resolvedThreadTarget, error) {
	if payload.Start > 0 || payload.End > 0 {
		start := maxInt(0, payload.Start)
		end := maxInt(start, payload.End)
		length := utf16Length(content)
		if start > length || end > length {
			return resolvedThreadTarget{}, fmt.Errorf("offset range [%d,%d] exceeds document length %d", start, end, length)
		}
		return targetFromOffsets(content, start, end, payload.Line, payload.Excerpt), nil
	}
	if payload.StartLine > 0 || payload.EndLine > 0 || payload.StartColumn > 0 || payload.EndColumn > 0 {
		startLine := payload.StartLine
		if startLine == 0 {
			startLine = payload.Line
		}
		if startLine <= 0 {
			return resolvedThreadTarget{}, fmt.Errorf("start-line is required for a line-column range")
		}
		endLine := payload.EndLine
		if endLine == 0 {
			endLine = startLine
		}
		startColumn := payload.StartColumn
		if startColumn == 0 {
			startColumn = 1
		}
		start, err := lineColumnToUTF16Index(content, startLine, startColumn, false)
		if err != nil {
			return resolvedThreadTarget{}, err
		}
		end, err := lineColumnToUTF16Index(content, endLine, payload.EndColumn, true)
		if err != nil {
			return resolvedThreadTarget{}, err
		}
		if end < start {
			return resolvedThreadTarget{}, fmt.Errorf("end range precedes start range")
		}
		return targetFromOffsets(content, start, end, startLine, payload.Excerpt), nil
	}
	quote := payload.Quote
	if strings.TrimSpace(quote) != "" {
		start, end, line, err := findQuoteRange(content, quote, payload.Line)
		if err != nil {
			return resolvedThreadTarget{}, err
		}
		return targetFromOffsets(content, start, end, line, payload.Excerpt), nil
	}
	if payload.Line > 0 {
		span, err := lineSpanForLine(content, payload.Line)
		if err != nil {
			return resolvedThreadTarget{}, err
		}
		return targetFromOffsets(content, span.startIndex, span.endIndex, payload.Line, payload.Excerpt), nil
	}
	return resolvedThreadTarget{}, fmt.Errorf("provide --line, --quote, a line-column range, UTF-16 offsets, or --document")
}

func hasThreadRangeTarget(payload createThreadPayload) bool {
	return payload.Start > 0 || payload.End > 0 || payload.Line > 0 ||
		payload.StartLine > 0 || payload.StartColumn > 0 ||
		payload.EndLine > 0 || payload.EndColumn > 0
}

func targetFromOffsets(content string, start, end, line int, excerpt string) resolvedThreadTarget {
	if line <= 0 {
		line = lineForUTF16Offset(content, start)
	}
	excerpt = strings.TrimSpace(excerpt)
	if excerpt == "" {
		excerpt = strings.TrimSpace(sliceByUTF16(content, start, end))
	}
	if excerpt == "" {
		span, err := lineSpanForLine(content, line)
		if err == nil {
			excerpt = strings.TrimSpace(sliceByUTF16(content, span.startIndex, span.endIndex))
		}
	}
	return resolvedThreadTarget{
		start:   start,
		end:     end,
		line:    maxInt(1, line),
		excerpt: excerpt,
	}
}

type threadLineSpan struct {
	line       int
	startByte  int
	endByte    int
	startIndex int
	endIndex   int
}

func findQuoteRange(content, quote string, line int) (int, int, int, error) {
	if line > 0 {
		span, err := lineSpanForLine(content, line)
		if err != nil {
			return 0, 0, 0, err
		}
		lineText := content[span.startByte:span.endByte]
		offset := strings.Index(lineText, quote)
		if offset < 0 {
			return 0, 0, 0, fmt.Errorf("quote was not found on line %d", line)
		}
		if strings.Index(lineText[offset+len(quote):], quote) >= 0 {
			return 0, 0, 0, fmt.Errorf("quote appears multiple times on line %d; use a more precise quote or line-column range", line)
		}
		start := span.startIndex + utf16Length(lineText[:offset])
		return start, start + utf16Length(quote), line, nil
	}
	offset := strings.Index(content, quote)
	if offset < 0 {
		return 0, 0, 0, fmt.Errorf("quote was not found")
	}
	if strings.Index(content[offset+len(quote):], quote) >= 0 {
		return 0, 0, 0, fmt.Errorf("quote appears multiple times; add --line or use a line-column range")
	}
	start := utf16Length(content[:offset])
	return start, start + utf16Length(quote), lineForUTF16Offset(content, start), nil
}

func lineSpanForLine(content string, targetLine int) (threadLineSpan, error) {
	if targetLine <= 0 {
		return threadLineSpan{}, fmt.Errorf("line must be >= 1")
	}
	line := 1
	startByte := 0
	startIndex := 0
	index := 0
	for byteIndex, r := range content {
		if r == '\n' {
			if line == targetLine {
				return threadLineSpan{
					line:       line,
					startByte:  startByte,
					endByte:    byteIndex,
					startIndex: startIndex,
					endIndex:   index,
				}, nil
			}
			line++
			startByte = byteIndex + 1
			index++
			startIndex = index
			continue
		}
		index += runeUTF16Length(r)
	}
	if line == targetLine {
		return threadLineSpan{
			line:       line,
			startByte:  startByte,
			endByte:    len(content),
			startIndex: startIndex,
			endIndex:   index,
		}, nil
	}
	return threadLineSpan{}, fmt.Errorf("line %d is outside document line count %d", targetLine, line)
}

func lineColumnToUTF16Index(content string, line int, column int, defaultToLineEnd bool) (int, error) {
	span, err := lineSpanForLine(content, line)
	if err != nil {
		return 0, err
	}
	if column <= 0 {
		if defaultToLineEnd {
			return span.endIndex, nil
		}
		column = 1
	}
	offset := column - 1
	lineLength := span.endIndex - span.startIndex
	if offset > lineLength {
		return 0, fmt.Errorf("column %d is outside line %d length %d", column, line, lineLength+1)
	}
	return span.startIndex + offset, nil
}

func lineForUTF16Offset(content string, offset int) int {
	if offset <= 0 {
		return 1
	}
	line := 1
	index := 0
	for _, r := range content {
		if index >= offset {
			return line
		}
		if r == '\n' {
			line++
			index++
			continue
		}
		index += runeUTF16Length(r)
	}
	return line
}

func sliceByUTF16(content string, start, end int) string {
	if end < start {
		end = start
	}
	startByte := utf16OffsetToByteIndex(content, start)
	endByte := utf16OffsetToByteIndex(content, end)
	return content[startByte:endByte]
}

func utf16OffsetToByteIndex(content string, offset int) int {
	if offset <= 0 {
		return 0
	}
	index := 0
	for byteIndex, r := range content {
		next := index + runeUTF16Length(r)
		if next > offset {
			return byteIndex
		}
		if next == offset {
			return byteIndex + len(string(r))
		}
		index = next
	}
	return len(content)
}

func utf16Length(content string) int {
	total := 0
	for _, r := range content {
		total += runeUTF16Length(r)
	}
	return total
}

func runeUTF16Length(r rune) int {
	if r < 0x10000 {
		return 1
	}
	return len(utf16.Encode([]rune{r}))
}

func encodeRelativeAnchor(text *crdt.YText, index int) string {
	assoc := 0
	if index >= text.Len() {
		assoc = -1
	}
	anchor, err := text.EncodeRelativeAnchor(index, assoc)
	if err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(anchor)
}

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}
