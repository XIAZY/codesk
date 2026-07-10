package syncer

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

type threadAnchor struct {
	Kind          string `json:"kind"`
	RelativeStart string `json:"relativeStart,omitempty"`
	RelativeEnd   string `json:"relativeEnd,omitempty"`
	Excerpt       string `json:"excerpt,omitempty"`
}

type threadMessage struct {
	ID           string    `json:"id"`
	ThreadID     string    `json:"threadId"`
	AuthorID     string    `json:"authorId"`
	AuthorType   string    `json:"authorType"`
	AuthorHandle string    `json:"authorHandle"`
	AuthorName   string    `json:"authorName"`
	Body         string    `json:"body"`
	Kind         string    `json:"kind"`
	CreatedAt    time.Time `json:"createdAt"`
}

type thread struct {
	ID                 string           `json:"id"`
	DocumentID         string           `json:"documentId"`
	Title              string           `json:"title"`
	Status             string           `json:"status"`
	Anchor             threadAnchor     `json:"anchor"`
	ParticipantIDs     []string         `json:"participantIds"`
	ParticipantHandles []string         `json:"participantHandles"`
	Messages           []*threadMessage `json:"messages"`
	UpdatedAt          time.Time        `json:"updatedAt"`
}

type agentEvent struct {
	ID              string    `json:"id"`
	AgentID         string    `json:"agentId"`
	AgentHandle     string    `json:"agentHandle"`
	Type            string    `json:"type"`
	Box             string    `json:"box"`
	Status          string    `json:"status"`
	DocumentID      string    `json:"documentId"`
	ThreadID        string    `json:"threadId"`
	ThreadMessageID string    `json:"threadMessageId"`
	FromUpdateID    int64     `json:"fromUpdateId"`
	ToUpdateID      int64     `json:"toUpdateId"`
	Summary         string    `json:"summary"`
	Prompt          string    `json:"prompt"`
	RunID           string    `json:"runId"`
	LastError       string    `json:"lastError"`
	CreatedAt       time.Time `json:"createdAt"`
	UpdatedAt       time.Time `json:"updatedAt"`
}

type createThreadPayload struct {
	DocumentID    string `json:"documentId"`
	Path          string `json:"path,omitempty"`
	Title         string `json:"title"`
	Body          string `json:"body"`
	Kind          string `json:"kind,omitempty"`
	Document      bool   `json:"document,omitempty"`
	Quote         string `json:"quote,omitempty"`
	RelativeStart string `json:"relativeStart,omitempty"`
	RelativeEnd   string `json:"relativeEnd,omitempty"`
	Start         int    `json:"start"`
	End           int    `json:"end"`
	Line          int    `json:"line,omitempty"`
	StartLine     int    `json:"startLine,omitempty"`
	StartColumn   int    `json:"startColumn,omitempty"`
	EndLine       int    `json:"endLine,omitempty"`
	EndColumn     int    `json:"endColumn,omitempty"`
	Excerpt       string `json:"excerpt,omitempty"`
}

type replyThreadPayload struct {
	Body string `json:"body"`
	Kind string `json:"kind"`
}

type threadMutationResponse struct {
	Thread *thread `json:"thread"`
}

type agentSkillExecutor struct {
	service *Service
}

func (s *Service) driveAgentAutomation(ctx context.Context, workspace *workspaceResponse) error {
	if workspace == nil {
		return nil
	}
	skills := agentSkillExecutor{service: s}
	for _, currentAgent := range workspace.Agents {
		if currentAgent == nil {
			continue
		}
		if err := s.driveSingleAgentAutomation(ctx, skills, currentAgent, workspace); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) driveSingleAgentAutomation(ctx context.Context, skills agentSkillExecutor, currentAgent *agent, workspace *workspaceResponse) error {
	forMe, general, err := s.fetchPendingInboxForAgent(ctx, currentAgent.ID)
	if err != nil {
		return err
	}
	if len(forMe) == 0 && len(general) == 0 {
		return nil
	}
	return skills.startNotificationCheck(ctx, currentAgent, forMe, general, workspace)
}

func (e agentSkillExecutor) startNotificationCheck(ctx context.Context, currentAgent *agent, forMe []*agentEvent, general []*agentEvent, workspace *workspaceResponse) error {
	// Turn-assembly time only: fetch the muted count to DECORATE the prompt. This runs solely when a turn is
	// already firing for pushed (for_me/general) items — NOT in the per-cycle poll — so it never re-adds the
	// per-agent synthetic document walk to the hot path. The count is deliberately excluded from the
	// suppression signature below: muted must never admit a turn, so the count may be stale (as-of this turn).
	mutedCount := e.service.mutedInboxCount(ctx, currentAgent.ID)
	prompt := buildNotificationPrompt(currentAgent, forMe, general, mutedCount, workspace)
	return e.service.sessions.ScheduleNotificationTurn(ctx, currentAgent, prompt, notificationSignature(forMe), notificationSignature(general))
}

// mutedInboxCount returns how many items sit in the agent's muted box, for the count-only prompt pointer.
// Best-effort: on any error it returns 0 (the pointer is decoration and must never fail a firing turn).
func (s *Service) mutedInboxCount(ctx context.Context, agentID string) int {
	response, err := s.listInboxForAgent(ctx, agentID, "muted")
	if err != nil || response == nil {
		return 0
	}
	return len(response.Items)
}

func findThreadByID(threads []*thread, threadID string) *thread {
	for _, current := range threads {
		if current != nil && current.ID == threadID {
			return current
		}
	}
	return nil
}

func findThreadMessageByID(messages []*threadMessage, messageID string) *threadMessage {
	for _, current := range messages {
		if current != nil && current.ID == messageID {
			return current
		}
	}
	return nil
}

func findAgentByID(agents []*agent, agentID string) *agent {
	for _, current := range agents {
		if current != nil && current.ID == agentID {
			return current
		}
	}
	return nil
}

func pendingNotificationsForAgent(events []*agentEvent, agentID string, box string) []*agentEvent {
	notifications := make([]*agentEvent, 0)
	for _, event := range events {
		if event == nil || event.AgentID != agentID {
			continue
		}
		if normalizeInboxBox(event.Box) != normalizeInboxBox(box) {
			continue
		}
		switch event.Status {
		case "completed", "dismissed":
			continue
		}
		notifications = append(notifications, event)
	}
	sort.Slice(notifications, func(i, j int) bool {
		if notifications[i].CreatedAt.Equal(notifications[j].CreatedAt) {
			return notifications[i].ID < notifications[j].ID
		}
		return notifications[i].CreatedAt.Before(notifications[j].CreatedAt)
	})
	return notifications
}

func normalizeInboxBox(box string) string {
	switch strings.TrimSpace(strings.ToLower(strings.ReplaceAll(box, "-", "_"))) {
	case "general":
		return "general"
	case "muted":
		// Muted is the never-pushed box (task #2) — must be recognized here as on the backend, or the
		// default branch routes it to for_me (a PUSHED box) and the daemon would surface a muted item as an
		// actionable notification. Both normalizer copies MUST agree; an adversarial test pins that.
		return "muted"
	default:
		return "for_me"
	}
}

func buildNotificationPrompt(currentAgent *agent, forMe []*agentEvent, general []*agentEvent, mutedCount int, workspace *workspaceResponse) string {
	var builder strings.Builder
	builder.WriteString("You have new items in your notification center.\n\n")
	appendNotificationSection(&builder, "For-me inbox", forMe, workspace, 5)
	appendNotificationSection(&builder, "General inbox", general, workspace, 3)
	// Count-only pointer to the muted box. Never pushed and never a reason you were woken — this line is
	// decoration on a wake that already fired. The count is as of this turn.
	if mutedCount > 0 {
		builder.WriteString(fmt.Sprintf("Muted inbox: %d item(s) waiting\n\n", mutedCount))
	}
	// One closer: a bare list-inbox now inspects every box (for-me + general + muted), so this single command
	// covers the full inbox.
	builder.WriteString("run notty-agent-tool list-inbox to inspect full inbox\n")
	return builder.String()
}

func appendNotificationSection(builder *strings.Builder, title string, notifications []*agentEvent, workspace *workspaceResponse, limit int) {
	builder.WriteString(title + ":\n")
	if len(notifications) == 0 {
		builder.WriteString("- No pending items.\n\n")
		return
	}
	for index, notification := range notifications {
		if index >= limit {
			builder.WriteString(fmt.Sprintf("- ...and %d more items.\n", len(notifications)-index))
			break
		}
		builder.WriteString("- " + summarizeNotification(notification, workspace) + "\n")
	}
	builder.WriteString("\n")
}

func summarizeNotification(notification *agentEvent, workspace *workspaceResponse) string {
	if notification == nil {
		return "<nil>"
	}
	parts := []string{}
	if notification.ID != "" {
		parts = append(parts, "id "+notification.ID)
	}
	if notification.Type != "" {
		parts = append(parts, notification.Type)
	}
	if workspace != nil {
		if thread := findThreadByID(workspace.Threads, notification.ThreadID); thread != nil {
			parts = append(parts, "thread \""+thread.Title+"\"")
			if messageSummary := notificationThreadMessageSummary(notification, thread); messageSummary != "" {
				parts = append(parts, messageSummary)
			}
		}
	}
	if notification.FromUpdateID != 0 || notification.ToUpdateID != 0 {
		parts = append(parts, fmt.Sprintf("versions %d -> %d", notification.FromUpdateID, notification.ToUpdateID))
	}
	if summary := firstNonEmptyText(notification.Summary, notification.Prompt); summary != "" {
		parts = append(parts, summary)
	}
	return strings.Join(parts, "; ")
}

func notificationThreadMessageSummary(notification *agentEvent, thread *thread) string {
	if notification == nil || thread == nil {
		return ""
	}
	if notification.ThreadMessageID != "" {
		if message := findThreadMessageByID(thread.Messages, notification.ThreadMessageID); message != nil {
			return "trigger @" + message.AuthorHandle + ": " + oneLine(message.Body)
		}
		if prompt := oneLine(notification.Prompt); prompt != "" {
			return "trigger: " + prompt
		}
		return "trigger message unavailable"
	}
	if messages := lastThreadMessages(thread.Messages, 1); len(messages) > 0 {
		return "latest @" + messages[0].AuthorHandle + ": " + oneLine(messages[0].Body)
	}
	return ""
}

func lastThreadMessages(messages []*threadMessage, limit int) []*threadMessage {
	if len(messages) <= limit {
		return messages
	}
	return messages[len(messages)-limit:]
}

func oneLine(value string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
}

func firstNonEmptyText(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func notificationSignature(notifications []*agentEvent) string {
	if len(notifications) == 0 {
		return ""
	}
	parts := make([]string, 0, len(notifications))
	for _, notification := range notifications {
		if notification == nil {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s:%s:%s:%d:%d", notification.ID, notification.Status, notification.UpdatedAt.UTC().Format(time.RFC3339Nano), notification.FromUpdateID, notification.ToUpdateID))
	}
	sort.Strings(parts)
	return strings.Join(parts, "|")
}
