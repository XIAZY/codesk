package syncer

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

type threadAnchor struct {
	DocumentID    string `json:"documentId"`
	Kind          string `json:"kind"`
	RelativeStart string `json:"relativeStart,omitempty"`
	RelativeEnd   string `json:"relativeEnd,omitempty"`
	Start         int    `json:"start"`
	End           int    `json:"end"`
	Line          int    `json:"line"`
	Excerpt       string `json:"excerpt"`
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
	AnchorStart     int       `json:"anchorStart"`
	AnchorEnd       int       `json:"anchorEnd"`
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
	DocumentID string `json:"documentId"`
	Title      string `json:"title"`
	Body       string `json:"body"`
	Start      int    `json:"start"`
	End        int    `json:"end"`
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
	prompt := buildNotificationPrompt(currentAgent, forMe, general, workspace)
	return e.service.sessions.ScheduleNotificationTurn(ctx, currentAgent, prompt, notificationSignature(forMe), notificationSignature(general))
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

func findDocumentByID(documents []*document, documentID string) *document {
	for _, current := range documents {
		if current != nil && current.ID == documentID {
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
	default:
		return "for_me"
	}
}

func buildNotificationPrompt(currentAgent *agent, forMe []*agentEvent, general []*agentEvent, workspace *workspaceResponse) string {
	var builder strings.Builder
	builder.WriteString("You have new items in your notification center.\n\n")
	appendNotificationSection(&builder, "For-me inbox", forMe, workspace, 5)
	appendNotificationSection(&builder, "General inbox", general, workspace, 3)
	builder.WriteString("Use the notification center tools if you want details, diffs, or to act on any item.\n")
	if currentAgent != nil && strings.TrimSpace(currentAgent.Role) != "" {
		builder.WriteString("\nRole:\n- " + currentAgent.Role + "\n")
	}
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
		if doc := findDocumentByID(workspace.Documents, notification.DocumentID); doc != nil {
			parts = append(parts, doc.Path)
		}
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
