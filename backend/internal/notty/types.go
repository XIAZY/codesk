package notty

import "time"

type WorkspaceState struct {
	WorkspaceID         string                         `json:"workspaceId"`
	Name                string                         `json:"name"`
	Documents           map[string]*Document           `json:"documents"`
	Users               map[string]*User               `json:"users"`
	Daemons             map[string]*Daemon             `json:"daemons"`
	Agents              map[string]*Agent              `json:"agents"`
	AgentRuns           map[string]*AgentRun           `json:"agentRuns"`
	Threads             map[string]*Thread             `json:"threads"`
	AgentEvents         map[string]*AgentEvent         `json:"agentEvents"`
	AgentDocumentViews  map[string]*AgentDocumentView  `json:"agentDocumentViews,omitempty"`
	DocumentCheckpoints map[string]*DocumentCheckpoint `json:"documentCheckpoints,omitempty"`
	Presences           map[string]*Presence           `json:"presences"`
	Activities          []*ActivityEvent               `json:"activities"`
	UpdatedAt           time.Time                      `json:"updatedAt"`
}

type Document struct {
	ID                 string    `json:"id"`
	Path               string    `json:"path"`
	DesiredPath        string    `json:"desiredPath,omitempty"`
	Title              string    `json:"title"`
	NotificationPolicy string    `json:"notificationPolicy,omitempty"`
	StateVector        string    `json:"stateVector,omitempty"`
	UpdateID           int64     `json:"updateId,omitempty"`
	UpdatedAt          time.Time `json:"updatedAt"`
}

type DocumentMetadata struct {
	ID                 string    `json:"id"`
	Path               string    `json:"path"`
	DesiredPath        string    `json:"desiredPath,omitempty"`
	Title              string    `json:"title"`
	NotificationPolicy string    `json:"notificationPolicy,omitempty"`
	StateVector        string    `json:"stateVector,omitempty"`
	UpdateID           int64     `json:"updateId,omitempty"`
	UpdatedAt          time.Time `json:"updatedAt"`
}

type ThreadAnchor struct {
	Kind          string `json:"kind"`
	RelativeStart string `json:"relativeStart,omitempty"`
	RelativeEnd   string `json:"relativeEnd,omitempty"`
	Excerpt       string `json:"excerpt,omitempty"`
}

type ThreadMessage struct {
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

type Thread struct {
	ID                 string           `json:"id"`
	DocumentID         string           `json:"documentId"`
	Title              string           `json:"title"`
	Status             string           `json:"status"`
	Anchor             ThreadAnchor     `json:"anchor"`
	CreatedByID        string           `json:"createdById"`
	CreatedByType      string           `json:"createdByType"`
	CreatedByHandle    string           `json:"createdByHandle"`
	CreatedByName      string           `json:"createdByName"`
	ParticipantIDs     []string         `json:"participantIds"`
	ParticipantHandles []string         `json:"participantHandles"`
	Messages           []*ThreadMessage `json:"messages"`
	CreatedAt          time.Time        `json:"createdAt"`
	UpdatedAt          time.Time        `json:"updatedAt"`
}

type User struct {
	ID        string    `json:"id"`
	Handle    string    `json:"handle"`
	Name      string    `json:"name"`
	Role      string    `json:"role"`
	Kind      string    `json:"kind"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type Account struct {
	ID          string    `json:"id"`
	Email       string    `json:"email"`
	DisplayName string    `json:"displayName"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

type Workspace struct {
	ID        string    `json:"id"`
	Slug      string    `json:"slug"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type WorkspaceMember struct {
	WorkspaceID    string    `json:"workspaceId"`
	AccountID      string    `json:"accountId"`
	UserID         string    `json:"userId"`
	UserHandle     string    `json:"userHandle,omitempty"`
	UserName       string    `json:"userName,omitempty"`
	MembershipRole string    `json:"membershipRole"`
	Status         string    `json:"status"`
	InvitedBy      string    `json:"invitedBy,omitempty"`
	CreatedAt      time.Time `json:"createdAt"`
	AcceptedAt     time.Time `json:"acceptedAt,omitempty"`
}

type Daemon struct {
	ID                 string    `json:"id"`
	WorkspaceID        string    `json:"workspaceId"`
	Name               string    `json:"name"`
	Status             string    `json:"status"`
	ConnectionStatus   string    `json:"connectionStatus"`
	LastSeenAt         time.Time `json:"lastSeenAt,omitempty"`
	LastSeenAgeSeconds int64     `json:"lastSeenAgeSeconds"`
	CreatedAt          time.Time `json:"createdAt"`
	DeletedAt          time.Time `json:"deletedAt,omitempty"`
}

type Presence struct {
	ActorID    string    `json:"actorId"`
	ActorType  string    `json:"actorType"`
	DocumentID string    `json:"documentId"`
	FilePath   string    `json:"filePath"`
	Mode       string    `json:"mode"`
	Selection  []int     `json:"selection"`
	Activity   string    `json:"activity"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

type Agent struct {
	ID               string    `json:"id"`
	DaemonID         string    `json:"daemonId"`
	Handle           string    `json:"handle"`
	Name             string    `json:"name"`
	Role             string    `json:"role"`
	Kind             string    `json:"kind"`
	SystemPrompt     string    `json:"systemPrompt"`
	WorkspaceRoot    string    `json:"workspaceRoot"`
	CodexThreadID    string    `json:"codexThreadId,omitempty"`
	CurrentTurnID    string    `json:"currentTurnId,omitempty"`
	SessionID        string    `json:"sessionId,omitempty"`
	Status           string    `json:"status"`
	CurrentTask      string    `json:"currentTask"`
	CurrentActivity  string    `json:"currentActivity"`
	CurrentRunID     string    `json:"currentRunId"`
	LastHeartbeatAt  time.Time `json:"lastHeartbeatAt,omitempty"`
	LastRunCompleted time.Time `json:"lastRunCompleted,omitempty"`
	UpdatedAt        time.Time `json:"updatedAt"`
}

type AgentRun struct {
	ID              string    `json:"id"`
	AgentID         string    `json:"agentId"`
	AgentHandle     string    `json:"agentHandle"`
	AgentName       string    `json:"agentName"`
	AgentKind       string    `json:"agentKind"`
	SystemPrompt    string    `json:"systemPrompt"`
	SessionID       string    `json:"sessionId,omitempty"`
	WorkspaceID     string    `json:"workspaceId"`
	WorkspaceRoot   string    `json:"workspaceRoot"`
	WorkingDir      string    `json:"workingDirectory"`
	Prompt          string    `json:"prompt"`
	Status          string    `json:"status"`
	DesiredStatus   string    `json:"desiredStatus"`
	ProcessID       int       `json:"processId,omitempty"`
	LaunchTime      time.Time `json:"launchTime,omitempty"`
	LastHeartbeatAt time.Time `json:"lastHeartbeatAt,omitempty"`
	CompletedAt     time.Time `json:"completedAt,omitempty"`
	ExitCode        int       `json:"exitCode,omitempty"`
	LastMessage     string    `json:"lastMessage,omitempty"`
	LogTail         []string  `json:"logTail,omitempty"`
	Error           string    `json:"error,omitempty"`
	AssignedTaskRef string    `json:"assignedTaskRef,omitempty"`
	UpdatedAt       time.Time `json:"updatedAt"`
}

type AgentEvent struct {
	ID              string    `json:"id"`
	AgentID         string    `json:"agentId"`
	AgentHandle     string    `json:"agentHandle"`
	Type            string    `json:"type"`
	Box             string    `json:"box,omitempty"`
	Status          string    `json:"status"`
	DocumentID      string    `json:"documentId,omitempty"`
	ThreadID        string    `json:"threadId,omitempty"`
	ThreadMessageID string    `json:"threadMessageId,omitempty"`
	FromUpdateID    int64     `json:"fromUpdateId"`
	ToUpdateID      int64     `json:"toUpdateId"`
	Summary         string    `json:"summary"`
	Prompt          string    `json:"prompt,omitempty"`
	DedupKey        string    `json:"dedupKey,omitempty"`
	ClaimedBy       string    `json:"claimedBy,omitempty"`
	RunID           string    `json:"runId,omitempty"`
	LastError       string    `json:"lastError,omitempty"`
	AttemptCount    int       `json:"attemptCount"`
	AvailableAt     time.Time `json:"availableAt"`
	ClaimedAt       time.Time `json:"claimedAt,omitempty"`
	CompletedAt     time.Time `json:"completedAt,omitempty"`
	CreatedAt       time.Time `json:"createdAt"`
	UpdatedAt       time.Time `json:"updatedAt"`
}

type AgentDocumentView struct {
	AgentID     string    `json:"agentId"`
	DocumentID  string    `json:"documentId"`
	UpdateID    int64     `json:"updateId"`
	StateVector string    `json:"stateVector,omitempty"`
	ViewedAt    time.Time `json:"viewedAt"`
}

type DocumentCheckpoint struct {
	ID          int64     `json:"id,omitempty"`
	DocumentID  string    `json:"documentId"`
	UpdateID    int64     `json:"updateId"`
	CRDTState   string    `json:"crdtState"`
	StateVector string    `json:"stateVector,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
}

type DocumentDiff struct {
	DocumentID   string             `json:"documentId"`
	FromUpdateID int64              `json:"fromUpdateId"`
	ToUpdateID   int64              `json:"toUpdateId"`
	FromContent  string             `json:"fromContent"`
	ToContent    string             `json:"toContent"`
	Unified      string             `json:"unified"`
	Hunks        []DocumentDiffHunk `json:"hunks"`
}

type DocumentDiffHunk struct {
	Op   string `json:"op"`
	Text string `json:"text"`
}

type ActivityEvent struct {
	Type        string        `json:"type"`
	DocumentID  string        `json:"documentId,omitempty"`
	ActorID     string        `json:"actorId"`
	ActorType   string        `json:"actorType"`
	Summary     string        `json:"summary"`
	OccurredAt  time.Time     `json:"occurredAt"`
	Provenance  OperationMeta `json:"provenance"`
	PresenceRef string        `json:"presenceRef,omitempty"`
}

type OperationMeta struct {
	ActorID        string `json:"actorId"`
	ActorType      string `json:"actorType"`
	ExecutionID    string `json:"executionId"`
	Tool           string `json:"tool"`
	Trigger        string `json:"trigger"`
	Autonomous     bool   `json:"autonomous"`
	Confidence     string `json:"confidence"`
	RequestedBy    string `json:"requestedBy,omitempty"`
	Source         string `json:"source"`
	IntendedScope  string `json:"intendedScope,omitempty"`
	ReadSetSummary string `json:"readSetSummary,omitempty"`
}

type RegisterRequest struct {
	Email       string `json:"email"`
	Password    string `json:"password"`
	DisplayName string `json:"displayName"`
}

type LoginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type AuthResponse struct {
	Token      string       `json:"token"`
	Account    *Account     `json:"account"`
	Workspaces []*Workspace `json:"workspaces,omitempty"`
}

type CreateWorkspaceRequest struct {
	Name   string `json:"name"`
	Slug   string `json:"slug"`
	Handle string `json:"handle"`
}

type AddWorkspaceMemberRequest struct {
	Email       string `json:"email"`
	DisplayName string `json:"displayName"`
	Handle      string `json:"handle"`
	Role        string `json:"role"`
}

type CreateDaemonRequest struct {
	Name string `json:"name"`
}

type CreateDaemonResponse struct {
	Daemon *Daemon `json:"daemon"`
	Token  string  `json:"token"`
}

type CreateThreadRequest struct {
	DocumentID    string `json:"documentId"`
	Title         string `json:"title"`
	Body          string `json:"body"`
	Kind          string `json:"kind"`
	RelativeStart string `json:"relativeStart"`
	RelativeEnd   string `json:"relativeEnd"`
	Excerpt       string `json:"excerpt"`
}

type ReplyThreadRequest struct {
	Author string `json:"author"`
	Body   string `json:"body"`
	Kind   string `json:"kind"`
}

type CreateDocumentRequest struct {
	Path               string `json:"path"`
	Content            string `json:"content"`
	NotificationPolicy string `json:"notificationPolicy,omitempty"`
}

type UpdateDocumentRequest struct {
	Path string `json:"path"`
}

type UpsertPresenceRequest struct {
	ActorID    string `json:"actorId"`
	ActorType  string `json:"actorType"`
	DocumentID string `json:"documentId"`
	FilePath   string `json:"filePath"`
	Mode       string `json:"mode"`
	Selection  []int  `json:"selection"`
	Activity   string `json:"activity"`
}

type ClaimAgentEventRequest struct {
	AgentID   string `json:"agentId"`
	ClaimedBy string `json:"claimedBy"`
}

type UpdateAgentEventRequest struct {
	Status    string `json:"status"`
	ThreadID  string `json:"threadId"`
	RunID     string `json:"runId"`
	LastError string `json:"lastError"`
}

type CreateUserRequest struct {
	Name   string `json:"name"`
	Handle string `json:"handle"`
	Role   string `json:"role"`
}

type UpdateUserRequest struct {
	Name   string `json:"name"`
	Handle string `json:"handle"`
	Role   string `json:"role"`
}

type StartAgentRunRequest struct {
	AgentID         string `json:"agentId"`
	AgentName       string `json:"agentName"`
	Role            string `json:"role"`
	AgentKind       string `json:"agentKind"`
	Prompt          string `json:"prompt"`
	AssignedTaskRef string `json:"assignedTaskRef"`
}

type CreateAgentRequest struct {
	DaemonID     string `json:"daemonId"`
	Handle       string `json:"handle"`
	Name         string `json:"name"`
	Role         string `json:"role"`
	Kind         string `json:"kind"`
	SystemPrompt string `json:"systemPrompt"`
}

type UpdateAgentRequest struct {
	Handle       string `json:"handle"`
	Name         string `json:"name"`
	Role         string `json:"role"`
	SystemPrompt string `json:"systemPrompt"`
}

type UpdateAgentRunRequest struct {
	Status          string   `json:"status"`
	DesiredStatus   string   `json:"desiredStatus"`
	SessionID       string   `json:"sessionId"`
	ProcessID       int      `json:"processId"`
	LastHeartbeatAt string   `json:"lastHeartbeatAt"`
	LastMessage     string   `json:"lastMessage"`
	LogTail         []string `json:"logTail"`
	Error           string   `json:"error"`
	ExitCode        *int     `json:"exitCode"`
}

type UpdateAgentSessionRequest struct {
	Status          string `json:"status"`
	CodexThreadID   string `json:"codexThreadId,omitempty"`
	CurrentTurnID   string `json:"currentTurnId,omitempty"`
	CurrentActivity string `json:"currentActivity,omitempty"`
	LastHeartbeatAt string `json:"lastHeartbeatAt,omitempty"`
}

type UpdateAgentNotificationRequest struct {
	Status string `json:"status"`
}

type MarkDocumentViewedRequest struct {
	UpdateID int64 `json:"updateId,omitempty"`
}

type EventEnvelope struct {
	Type string      `json:"type"`
	Data interface{} `json:"data"`
}

type DocumentUpdateEvent struct {
	DocumentID string    `json:"documentId"`
	UpdateID   int64     `json:"updateId,omitempty"`
	Update     string    `json:"update"`
	Path       string    `json:"path"`
	UpdatedAt  time.Time `json:"updatedAt"`
	ActorID    string    `json:"actorId"`
}

type DocumentLifecycleEvent struct {
	DocumentID string    `json:"documentId"`
	Path       string    `json:"path"`
	OldPath    string    `json:"oldPath,omitempty"`
	Title      string    `json:"title"`
	UpdatedAt  time.Time `json:"updatedAt"`
	ActorID    string    `json:"actorId"`
}

type AgentInboxChangedEvent struct {
	WorkspaceID      string `json:"workspaceId"`
	DaemonID         string `json:"daemonId,omitempty"`
	AgentID          string `json:"agentId"`
	Box              string `json:"box"`
	EventID          string `json:"eventId"`
	NotificationType string `json:"notificationType"`
}

func cloneDocument(document *Document) *Document {
	clone := *document
	return &clone
}

func documentMetadata(document *Document) *DocumentMetadata {
	if document == nil {
		return nil
	}
	return &DocumentMetadata{
		ID:                 document.ID,
		Path:               document.Path,
		DesiredPath:        document.DesiredPath,
		Title:              document.Title,
		NotificationPolicy: document.NotificationPolicy,
		StateVector:        document.StateVector,
		UpdateID:           document.UpdateID,
		UpdatedAt:          document.UpdatedAt,
	}
}
