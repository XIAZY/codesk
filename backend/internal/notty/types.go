package notty

import "time"

type WorkspaceState struct {
	WorkspaceID         string                         `json:"workspaceId"`
	Name                string                         `json:"name"`
	RootDocumentID      string                         `json:"rootDocumentId"`
	ContentDocuments    map[string]*Document           `json:"contentDocuments"`
	DocumentCheckpoints map[string]*DocumentCheckpoint `json:"documentCheckpoints,omitempty"`
	UpdatedAt           time.Time                      `json:"updatedAt"`
}

type Document struct {
	ID                      string    `json:"id"`
	Hidden                  bool      `json:"hidden,omitempty"`
	StateVector             string    `json:"stateVector,omitempty"`
	UpdateID                int64     `json:"updateId,omitempty"`
	UpdatedAt               time.Time `json:"updatedAt"`
	ClientIDSeed            uint64    `json:"clientIdSeed,omitempty"`
	CreateClientOperationID string    `json:"createClientOperationId,omitempty"`
}

type DocumentMetadata struct {
	ID                      string    `json:"id"`
	StateVector             string    `json:"stateVector,omitempty"`
	UpdateID                int64     `json:"updateId,omitempty"`
	UpdatedAt               time.Time `json:"updatedAt"`
	ClientIDSeed            uint64    `json:"clientIdSeed,omitempty"`
	CreateClientOperationID string    `json:"createClientOperationId,omitempty"`
}

// DocumentSubscriberAgent is the lean projection the Participants panel reads (task #4): the identity fields
// a subscriber row renders, and nothing more — deliberately not the full Agent (no systemPrompt/status/etc.)
// so the read's contract stays tight and never leaks agent internals into this workspace-member surface.
type DocumentSubscriberAgent struct {
	ID     string `json:"id"`
	Handle string `json:"handle"`
	Name   string `json:"name"`
	Kind   string `json:"kind"`
}

type ThreadAnchor struct {
	Kind          string `json:"kind"`
	RelativeStart string `json:"relativeStart,omitempty"`
	RelativeEnd   string `json:"relativeEnd,omitempty"`
	Excerpt       string `json:"excerpt,omitempty"`
	// StateAtAnchor is the anchor-time Y.js state vector (opaque base64), persisted so the identity-based
	// orphan classifier can filter post-anchor inserts. The backend never interprets it — it travels
	// verbatim alongside the relative positions.
	StateAtAnchor string `json:"stateAtAnchor,omitempty"`
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
	ClientOperationID  string           `json:"clientOperationId,omitempty"`
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
	ID                      string    `json:"id"`
	Email                   string    `json:"email"`
	DisplayName             string    `json:"displayName"`
	EmailVerified           bool      `json:"emailVerified"`
	LastAccessedWorkspaceID string    `json:"lastAccessedWorkspaceId,omitempty"`
	CreatedAt               time.Time `json:"createdAt"`
	UpdatedAt               time.Time `json:"updatedAt"`
}

type Workspace struct {
	ID                     string    `json:"id"`
	Slug                   string    `json:"slug"`
	Name                   string    `json:"name"`
	RootDocumentID         string    `json:"rootDocumentId,omitempty"`
	LastAccessedDocumentID string    `json:"lastAccessedDocumentId,omitempty"`
	DefaultRuntime         string    `json:"defaultRuntime,omitempty"`
	CreatedAt              time.Time `json:"createdAt"`
	UpdatedAt              time.Time `json:"updatedAt"`
}

type WorkspaceInvite struct {
	ID              string    `json:"id"`
	WorkspaceID     string    `json:"workspaceId"`
	CreatedByUserID string    `json:"-"`
	ExpiresAt       time.Time `json:"expiresAt"`
	CreatedAt       time.Time `json:"createdAt"`
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
	ID                 string             `json:"id"`
	WorkspaceID        string             `json:"workspaceId"`
	Name               string             `json:"name"`
	Status             string             `json:"status"`
	ConnectionStatus   string             `json:"connectionStatus"`
	Version            string             `json:"version,omitempty"`
	OS                 string             `json:"os,omitempty"`
	Arch               string             `json:"arch,omitempty"`
	Runtimes           []RuntimeDetection `json:"runtimes,omitempty"`
	LastSeenAt         time.Time          `json:"lastSeenAt,omitempty"`
	LastSeenAgeSeconds int64              `json:"lastSeenAgeSeconds"`
	CreatedAt          time.Time          `json:"createdAt"`
	DeletedAt          time.Time          `json:"deletedAt,omitempty"`
}

type RuntimeDetection struct {
	Kind         string               `json:"kind"`
	Available    bool                 `json:"available"`
	Version      string               `json:"version,omitempty"`
	Path         string               `json:"path,omitempty"`
	Reason       string               `json:"reason,omitempty"`
	ModelCatalog *RuntimeModelCatalog `json:"modelCatalog,omitempty"`
}

// RuntimeModelCatalog is the backend mirror of the daemon's projected model
// catalog. It is a protocol mirror of the daemon-side type (daemon owns the
// producer); the sealed JSON casing below is the only coupling, so these tags
// must match the daemon's RuntimeDetection.ModelCatalog exactly. A nil
// *RuntimeModelCatalog means the runtime reported no catalog (an old/unsupported
// daemon, or a runtime like Claude with no catalog today); a non-nil catalog
// with a non-empty Error means the runtime is capable but the probe failed; a
// non-nil catalog with an empty Error and empty Models means capable but the
// account has no available models. Explicit model/effort choices require a
// present, error-free catalog; inheritance ("", "") never depends on it.
type RuntimeModelCatalog struct {
	Models []RuntimeModel `json:"models"`
	Error  string         `json:"error,omitempty"`
}

// RuntimeModel is one entry in the projected model catalog. Model is the opaque
// provider value passed to thread/start and thread/resume; DisplayName is for
// rendering only and must never be substituted for Model.
type RuntimeModel struct {
	Model                  string   `json:"model"`
	DisplayName            string   `json:"displayName"`
	IsDefault              bool     `json:"isDefault"`
	ReasoningEfforts       []string `json:"reasoningEfforts"`
	DefaultReasoningEffort string   `json:"defaultReasoningEffort,omitempty"`
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
	Model            string    `json:"model"`
	ReasoningEffort  string    `json:"reasoningEffort"`
	SystemPrompt     string    `json:"systemPrompt"`
	WorkspaceRoot    string    `json:"workspaceRoot"`
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
	ID          int64         `json:"-"`
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
	Token      string       `json:"token,omitempty"`
	Account    *Account     `json:"account"`
	Workspaces []*Workspace `json:"workspaces,omitempty"`
}

type VerifyEmailRequest struct {
	Token string `json:"token"`
}

type ResendVerificationRequest struct {
	Email string `json:"email"`
}

type ForgotPasswordRequest struct {
	Email string `json:"email"`
}

type ResetPasswordRequest struct {
	Token    string `json:"token"`
	Password string `json:"password"`
}

// UpdateThreadRequest sets a thread's lifecycle status. Valid values are
// "open" and "resolved"; both directions are allowed and re-applying the
// current status is an idempotent success.
type UpdateThreadRequest struct {
	Status string `json:"status"`
}

// UpdateThreadAnchorRequest re-anchors a thread (e.g. an orphan whose original text was deleted) to a
// new position. Kind is inferred from the relative positions when omitted, exactly as on create.
// Excerpt is a pointer so an omitted key preserves the stored excerpt (partial-update convention) while
// a provided value — including "" — replaces it; the re-anchor picker always sends the fresh excerpt.
type UpdateThreadAnchorRequest struct {
	Kind          string  `json:"kind,omitempty"`
	RelativeStart string  `json:"relativeStart"`
	RelativeEnd   string  `json:"relativeEnd"`
	Excerpt       *string `json:"excerpt,omitempty"`
	// StateAtAnchor is captured fresh at pick time on every re-anchor (a repaired anchor's originals date
	// from the repair), so — unlike excerpt — it is taken from the request verbatim, never preserved.
	StateAtAnchor string `json:"stateAtAnchor,omitempty"`
}

type CreateWorkspaceRequest struct {
	Name   string `json:"name"`
	Slug   string `json:"slug"`
	Handle string `json:"handle"`
}

type UpdateWorkspaceRequest struct {
	Name *string `json:"name"`
}

// DeleteWorkspaceRequest carries the server-enforced confirmation: the caller
// must echo the workspace's exact current name to prove intent.
type DeleteWorkspaceRequest struct {
	ConfirmName string `json:"confirmName"`
}

type AddWorkspaceMemberRequest struct {
	Email       string `json:"email"`
	DisplayName string `json:"displayName"`
	Handle      string `json:"handle"`
	Role        string `json:"role"`
}

type CreateWorkspaceInviteResponse struct {
	Invite *WorkspaceInvite `json:"invite"`
	URL    string           `json:"url"`
}

type WorkspaceInvitePreview struct {
	Name string `json:"name"`
	Slug string `json:"slug"`
}

type WorkspaceInvitePreviewResponse struct {
	Workspace *WorkspaceInvitePreview `json:"workspace"`
	ExpiresAt time.Time               `json:"expiresAt"`
}

type AcceptWorkspaceInviteRequest struct {
	Handle string `json:"handle"`
}

type AcceptWorkspaceInviteResponse struct {
	Workspace *Workspace `json:"workspace"`
}

type CreateDaemonRequest struct {
	Name string `json:"name"`
}

type CreateDaemonResponse struct {
	Daemon *Daemon `json:"daemon"`
	Token  string  `json:"token"`
}

type CreateThreadRequest struct {
	DocumentID        string `json:"documentId"`
	ClientOperationID string `json:"clientOperationId,omitempty"`
	Title             string `json:"title"`
	Body              string `json:"body"`
	Kind              string `json:"kind"`
	RelativeStart     string `json:"relativeStart"`
	RelativeEnd       string `json:"relativeEnd"`
	Excerpt           string `json:"excerpt"`
	StateAtAnchor     string `json:"stateAtAnchor,omitempty"`
}

type ReplyThreadRequest struct {
	Author string `json:"author"`
	Body   string `json:"body"`
	Kind   string `json:"kind"`
}

type CreateDocumentRequest struct {
	DocumentID        string `json:"documentId,omitempty"`
	ClientOperationID string `json:"clientOperationId,omitempty"`
}

type UpdateLastAccessedRequest struct {
	DocumentID string `json:"documentId,omitempty"`
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

type StartAgentRunRequest struct {
	AgentID         string `json:"agentId"`
	AgentName       string `json:"agentName"`
	Role            string `json:"role"`
	AgentKind       string `json:"agentKind"`
	Prompt          string `json:"prompt"`
	AssignedTaskRef string `json:"assignedTaskRef"`
}

type CreateAgentRequest struct {
	DaemonID        string `json:"daemonId"`
	Handle          string `json:"handle"`
	Name            string `json:"name"`
	Role            string `json:"role"`
	Kind            string `json:"kind"`
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoningEffort"`
	SystemPrompt    string `json:"systemPrompt"`
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
	SessionID       string `json:"sessionId,omitempty"`
	CurrentTurnID   string `json:"currentTurnId,omitempty"`
	CurrentActivity string `json:"currentActivity,omitempty"`
	LastHeartbeatAt string `json:"lastHeartbeatAt,omitempty"`
}

type UpdateDaemonStatusRequest struct {
	Version  string             `json:"version,omitempty"`
	OS       string             `json:"os,omitempty"`
	Arch     string             `json:"arch,omitempty"`
	Runtimes []RuntimeDetection `json:"runtimes,omitempty"`
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
		ID:                      document.ID,
		StateVector:             document.StateVector,
		UpdateID:                document.UpdateID,
		UpdatedAt:               document.UpdatedAt,
		ClientIDSeed:            document.ClientIDSeed,
		CreateClientOperationID: document.CreateClientOperationID,
	}
}
