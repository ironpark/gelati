package codex

import (
	"encoding/json/jsontext"
	"fmt"

	"github.com/ironpark/gelati/internal/jsonx"
)

// ---------------------------------------------------------------------------
// Handshake
// ---------------------------------------------------------------------------

// ClientInfo identifies the integration to the app-server. The name is also
// used by the OpenAI Compliance Logs Platform to identify the client.
type ClientInfo struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version"`
}

// ClientCapabilities are the optional capabilities advertised at initialize.
type ClientCapabilities struct {
	// ExperimentalApi opts into experimental methods and fields.
	ExperimentalApi bool `json:"experimentalApi,omitzero"`
	// OptOutNotificationMethods lists exact notification method names to
	// suppress for this connection.
	OptOutNotificationMethods []string `json:"optOutNotificationMethods,omitempty"`
	// RequestAttestation opts into the server-initiated attestation/generate
	// request.
	RequestAttestation bool `json:"requestAttestation,omitzero"`
	// McpServerOpenaiFormElicitation allows the OpenAI extended-form variant of
	// mcpServer/elicitation/request.
	McpServerOpenaiFormElicitation bool `json:"mcpServerOpenaiFormElicitation,omitzero"`
}

// InitializeParams are the parameters of the initialize request.
type InitializeParams struct {
	ClientInfo   ClientInfo          `json:"clientInfo"`
	Capabilities *ClientCapabilities `json:"capabilities,omitzero"`
}

// InitializeResult describes the connected app-server.
type InitializeResult struct {
	// UserAgent is the user agent the server presents to upstream services.
	UserAgent string `json:"userAgent"`
	// CodexHome is the absolute path of the server's $CODEX_HOME.
	CodexHome string `json:"codexHome,omitempty"`
	// PlatformFamily describes the runtime target family.
	PlatformFamily string `json:"platformFamily"`
	// PlatformOs describes the runtime operating system.
	PlatformOs string `json:"platformOs"`
}

// ---------------------------------------------------------------------------
// Policies
// ---------------------------------------------------------------------------

// ApprovalPolicy controls when Codex asks for approval before escalating.
type ApprovalPolicy string

// Approval policies accepted by thread/start, thread/resume, thread/fork, and
// turn/start.
const (
	// ApprovalUntrusted asks before running anything outside the trusted set.
	ApprovalUntrusted ApprovalPolicy = "untrusted"
	// ApprovalOnRequest lets the model decide when to ask.
	ApprovalOnRequest ApprovalPolicy = "on-request"
	// ApprovalNever never asks; escalations fail instead.
	ApprovalNever ApprovalPolicy = "never"
)

// ApprovalsReviewer selects who reviews approval requests.
type ApprovalsReviewer string

// Approval reviewers.
const (
	// ReviewerUser routes approval requests to the client (the default).
	ReviewerUser ApprovalsReviewer = "user"
	// ReviewerAutoReview routes them to a reviewing subagent that approves or
	// denies on a risk basis.
	ReviewerAutoReview ApprovalsReviewer = "auto_review"
)

// ApprovalMode is a high-level approval preset, mirroring upstream's
// ApprovalMode. Settings expands it into the approvalPolicy and
// approvalsReviewer pair the protocol takes.
type ApprovalMode string

// Approval presets.
const (
	// ApprovalModeAutoReview asks on request and lets the auto-review subagent
	// decide. It is upstream's default for new threads.
	ApprovalModeAutoReview ApprovalMode = "auto_review"
	// ApprovalModeDenyAll never asks, so every escalation is denied.
	ApprovalModeDenyAll ApprovalMode = "deny_all"
)

// Settings returns the approval policy and reviewer for the preset. The
// reviewer is empty for ApprovalModeDenyAll and for unknown presets, and the
// policy is empty for unknown presets.
func (m ApprovalMode) Settings() (ApprovalPolicy, ApprovalsReviewer) {
	switch m {
	case ApprovalModeAutoReview:
		return ApprovalOnRequest, ReviewerAutoReview
	case ApprovalModeDenyAll:
		return ApprovalNever, ""
	default:
		return "", ""
	}
}

// SandboxMode is the shorthand sandbox preset accepted by thread/start,
// thread/resume, and thread/fork.
type SandboxMode string

// Sandbox presets.
const (
	// SandboxModeReadOnly allows reads but no writes.
	SandboxModeReadOnly SandboxMode = "read-only"
	// SandboxModeWorkspaceWrite also allows writes inside the workspace and
	// configured writable roots.
	SandboxModeWorkspaceWrite SandboxMode = "workspace-write"
	// SandboxModeDangerFullAccess removes filesystem restrictions.
	SandboxModeDangerFullAccess SandboxMode = "danger-full-access"
)

// Policy returns the turn/start sandbox policy equivalent to the preset, or
// nil for an unknown preset.
func (m SandboxMode) Policy() *SandboxPolicy {
	switch m {
	case SandboxModeReadOnly:
		return &SandboxPolicy{Type: SandboxTypeReadOnly}
	case SandboxModeWorkspaceWrite:
		return &SandboxPolicy{Type: SandboxTypeWorkspaceWrite}
	case SandboxModeDangerFullAccess:
		return SandboxDangerFullAccess()
	default:
		return nil
	}
}

// Sandbox policy type discriminators.
const (
	SandboxTypeReadOnly         = "readOnly"
	SandboxTypeWorkspaceWrite   = "workspaceWrite"
	SandboxTypeDangerFullAccess = "dangerFullAccess"
	SandboxTypeExternalSandbox  = "externalSandbox"
)

// Network access values for the externalSandbox policy.
const (
	NetworkAccessRestricted = "restricted"
	NetworkAccessEnabled    = "enabled"
)

// SandboxPolicy is the tagged sandbox configuration accepted by turn/start.
// Thread lifecycle requests take a SandboxMode instead.
type SandboxPolicy struct {
	// Type is one of the SandboxType* constants.
	Type string `json:"type"`
	// WritableRoots applies to the workspaceWrite policy.
	WritableRoots []string `json:"writableRoots,omitempty"`
	// NetworkAccess is a bool for readOnly and workspaceWrite and one of the
	// NetworkAccess* strings for externalSandbox.
	NetworkAccess any `json:"networkAccess,omitzero"`
	// ExcludeSlashTmp and ExcludeTmpdirEnvVar keep /tmp and $TMPDIR out of
	// the workspaceWrite writable set.
	ExcludeSlashTmp     bool `json:"excludeSlashTmp,omitzero"`
	ExcludeTmpdirEnvVar bool `json:"excludeTmpdirEnvVar,omitzero"`
}

// SandboxReadOnly returns a readOnly sandbox policy.
func SandboxReadOnly(networkAccess bool) *SandboxPolicy {
	return &SandboxPolicy{Type: SandboxTypeReadOnly, NetworkAccess: networkAccess}
}

// SandboxWorkspaceWrite returns a workspaceWrite sandbox policy.
func SandboxWorkspaceWrite(writableRoots []string, networkAccess bool) *SandboxPolicy {
	return &SandboxPolicy{
		Type:          SandboxTypeWorkspaceWrite,
		WritableRoots: writableRoots,
		NetworkAccess: networkAccess,
	}
}

// SandboxDangerFullAccess returns the unsandboxed policy.
func SandboxDangerFullAccess() *SandboxPolicy {
	return &SandboxPolicy{Type: SandboxTypeDangerFullAccess}
}

// SandboxExternal returns the externalSandbox policy for hosts that already
// sandbox the server process. networkAccess is NetworkAccessRestricted or
// NetworkAccessEnabled.
func SandboxExternal(networkAccess string) *SandboxPolicy {
	return &SandboxPolicy{Type: SandboxTypeExternalSandbox, NetworkAccess: networkAccess}
}

// ---------------------------------------------------------------------------
// Input items
// ---------------------------------------------------------------------------

// InputItem is one element of a turn's user input.
type InputItem interface {
	inputItem()
}

// TextInput is plain user text.
type TextInput struct {
	Text string `json:"text"`
}

// ImageInput references a remote image by URL.
type ImageInput struct {
	URL string `json:"url"`
}

// LocalImageInput references an image on the local filesystem.
type LocalImageInput struct {
	Path string `json:"path"`
}

// SkillInput attaches a skill so the server injects its full instructions.
type SkillInput struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// MentionInput references an app or file by path, such as "app://demo-app".
type MentionInput struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

func (TextInput) inputItem()       {}
func (ImageInput) inputItem()      {}
func (LocalImageInput) inputItem() {}
func (SkillInput) inputItem()      {}
func (MentionInput) inputItem()    {}

// MarshalJSON emits the tagged text input item.
func (i TextInput) MarshalJSON() ([]byte, error) {
	return jsonx.Marshal(map[string]any{"type": "text", "text": i.Text})
}

// MarshalJSON emits the tagged image input item.
func (i ImageInput) MarshalJSON() ([]byte, error) {
	return jsonx.Marshal(map[string]any{"type": "image", "url": i.URL})
}

// MarshalJSON emits the tagged local image input item.
func (i LocalImageInput) MarshalJSON() ([]byte, error) {
	return jsonx.Marshal(map[string]any{"type": "localImage", "path": i.Path})
}

// MarshalJSON emits the tagged skill input item.
func (i SkillInput) MarshalJSON() ([]byte, error) {
	return jsonx.Marshal(map[string]any{"type": "skill", "name": i.Name, "path": i.Path})
}

// MarshalJSON emits the tagged mention input item.
func (i MentionInput) MarshalJSON() ([]byte, error) {
	return jsonx.Marshal(map[string]any{"type": "mention", "name": i.Name, "path": i.Path})
}

// Text is shorthand for a single text input item.
func Text(text string) []InputItem { return []InputItem{TextInput{Text: text}} }

// ---------------------------------------------------------------------------
// Threads and turns
// ---------------------------------------------------------------------------

// Thread status type discriminators.
const (
	ThreadStatusNotLoaded   = "notLoaded"
	ThreadStatusIdle        = "idle"
	ThreadStatusSystemError = "systemError"
	ThreadStatusActive      = "active"
)

// ThreadStatus is a loaded thread's runtime status.
type ThreadStatus struct {
	// Type is one of the ThreadStatus* constants.
	Type string `json:"type"`
	// ActiveFlags describes what an active thread is waiting on.
	ActiveFlags []string `json:"activeFlags,omitempty"`
}

// GitInfo is persisted Git metadata for a stored thread.
type GitInfo struct {
	SHA       *string `json:"sha,omitzero"`
	Branch    *string `json:"branch,omitzero"`
	OriginURL *string `json:"originUrl,omitzero"`
}

// Thread is a conversation between a user and the Codex agent.
type Thread struct {
	ID string `json:"id"`
	// SessionID identifies the live session tree root; forked threads keep the
	// session id of the root they came from.
	SessionID string `json:"sessionId,omitempty"`
	// Name is the user-facing thread title, when one has been set.
	Name *string `json:"name,omitzero"`
	// Preview is a short excerpt of the thread's first user message.
	Preview string `json:"preview,omitempty"`
	// Ephemeral reports an in-memory thread that is not listed in storage.
	Ephemeral bool `json:"ephemeral,omitzero"`
	// ForkedFromID is the source thread of a fork, when available.
	ForkedFromID string `json:"forkedFromId,omitempty"`
	// ModelProvider is the provider backing the thread, such as "openai".
	ModelProvider string `json:"modelProvider,omitempty"`
	// Model is the configured model when loaded, otherwise the latest
	// persisted one.
	Model string `json:"model,omitempty"`
	// ReasoningEffort is the thread's reasoning effort, when known.
	ReasoningEffort string `json:"reasoningEffort,omitempty"`
	// Cwd is the thread's working directory.
	Cwd string `json:"cwd,omitempty"`
	// CliVersion is the version of the CLI that created the thread.
	CliVersion string `json:"cliVersion,omitempty"`
	// Path is the thread's location on disk (unstable upstream).
	Path string `json:"path,omitempty"`
	// ParentThreadID is set for threads spawned by another thread.
	ParentThreadID string `json:"parentThreadId,omitempty"`
	// ThreadSource is the client-supplied source classification.
	ThreadSource string `json:"threadSource,omitempty"`
	// Source is the session source the server recorded.
	Source jsontext.Value `json:"source,omitempty"`
	// CreatedAt and UpdatedAt are Unix timestamps in seconds.
	CreatedAt int64 `json:"createdAt,omitzero"`
	UpdatedAt int64 `json:"updatedAt,omitzero"`
	// Status is the runtime status, present on read and list responses.
	Status *ThreadStatus `json:"status,omitzero"`
	// GitInfo is persisted Git metadata.
	GitInfo *GitInfo `json:"gitInfo,omitzero"`
	// Turns is populated when the caller asked for turn history.
	Turns []Turn `json:"turns,omitempty"`
}

// Turn statuses reported by turn/start and turn/completed.
const (
	TurnInProgress  = "inProgress"
	TurnCompleted   = "completed"
	TurnInterrupted = "interrupted"
	TurnFailed      = "failed"
)

// Turn is a single user request and the agent work that follows.
type Turn struct {
	ID string `json:"id"`
	// Status is one of TurnInProgress, TurnCompleted, TurnInterrupted, or
	// TurnFailed.
	Status string `json:"status"`
	// Items holds the turn's items. turn/started and turn/completed carry an
	// empty list; use item/* notifications as the source of truth.
	Items []ThreadItem `json:"items,omitempty"`
	// ItemsView reports how much of Items was loaded: "notLoaded",
	// "summary", or "full".
	ItemsView string `json:"itemsView,omitempty"`
	// Error is set when Status is TurnFailed, and sometimes when it is
	// TurnInterrupted.
	Error *TurnError `json:"error,omitzero"`
	// StartedAt and CompletedAt are Unix timestamps in seconds, when known.
	StartedAt   *int64 `json:"startedAt,omitzero"`
	CompletedAt *int64 `json:"completedAt,omitzero"`
	// DurationMs is the time between start and completion, when known.
	DurationMs *int64 `json:"durationMs,omitzero"`
}

// IsTerminal reports whether the turn has reached a final status.
func (t *Turn) IsTerminal() bool {
	switch t.Status {
	case TurnCompleted, TurnInterrupted, TurnFailed:
		return true
	default:
		return false
	}
}

// TurnError describes why a turn failed.
type TurnError struct {
	Message string `json:"message"`
	// CodexErrorInfo classifies the failure. It is either a bare string, one
	// of the ErrorInfo* constants, or an object with a single ErrorInfo* key
	// whose value can carry an httpStatusCode. Use Kind and HTTPStatusCode
	// rather than decoding it by hand.
	CodexErrorInfo jsontext.Value `json:"codexErrorInfo,omitempty"`
	// AdditionalDetails carries free-form server detail.
	AdditionalDetails jsontext.Value `json:"additionalDetails,omitempty"`
}

// Error implements the error interface.
func (e *TurnError) Error() string {
	// Error is optional even on a failed turn, so a nil receiver is reachable
	// from any caller that has a Turn: it must describe the failure, not panic
	// and take the process down with it.
	if e == nil {
		return "codex: turn failed"
	}
	if kind := e.Kind(); kind != "" {
		return fmt.Sprintf("codex: turn failed (%s): %s", kind, e.Message)
	}
	return "codex: turn failed: " + e.Message
}

// Kind returns the codexErrorInfo discriminator: the bare string, or the
// single key of the externally tagged object form. It returns the empty
// string when no error info is present.
func (e *TurnError) Kind() string {
	kind, _ := e.errorInfo()
	return kind
}

// HTTPStatusCode returns the upstream HTTP status forwarded by the server, if
// any.
func (e *TurnError) HTTPStatusCode() (int, bool) {
	_, detail := e.errorInfo()
	if len(detail) == 0 {
		return 0, false
	}
	var obj struct {
		HTTPStatusCode *int `json:"httpStatusCode"`
	}
	if err := jsonx.Unmarshal(detail, &obj); err != nil || obj.HTTPStatusCode == nil {
		return 0, false
	}
	return *obj.HTTPStatusCode, true
}

// errorInfo splits codexErrorInfo into its discriminator and, for the object
// form {"<kind>": {...}}, the variant's payload.
func (e *TurnError) errorInfo() (string, jsontext.Value) {
	if e == nil || len(e.CodexErrorInfo) == 0 {
		return "", nil
	}
	var s string
	if err := jsonx.Unmarshal(e.CodexErrorInfo, &s); err == nil {
		return s, nil
	}
	var obj map[string]jsontext.Value
	if err := jsonx.Unmarshal(e.CodexErrorInfo, &obj); err != nil || len(obj) != 1 {
		return "", nil
	}
	for kind, detail := range obj {
		return kind, detail
	}
	return "", nil
}

// TokenUsage reports token consumption for a thread or turn. InputTokens
// includes CachedInputTokens rather than excluding it.
type TokenUsage struct {
	InputTokens       int64 `json:"inputTokens,omitzero"`
	CachedInputTokens int64 `json:"cachedInputTokens,omitzero"`
	CacheWriteTokens  int64 `json:"cacheWriteInputTokens,omitzero"`
	OutputTokens      int64 `json:"outputTokens,omitzero"`
	ReasoningTokens   int64 `json:"reasoningOutputTokens,omitzero"`
	TotalTokens       int64 `json:"totalTokens,omitzero"`
}

// ThreadTokenUsage is the tokenUsage object carried by
// thread/tokenUsage/updated. Total accumulates over the whole thread while Last
// is only the request that triggered the update, and a single turn triggers one
// update per model request — so a turn's own spend is the change in Total
// across it, not any one Last.
type ThreadTokenUsage struct {
	Total TokenUsage `json:"total"`
	Last  TokenUsage `json:"last"`
	// ModelContextWindow is the context window of the model the thread is
	// using, when the server reports one.
	ModelContextWindow int64 `json:"modelContextWindow,omitzero"`
}

// Sub returns the usage spent between an earlier reading of Total and this one.
func (u TokenUsage) Sub(earlier TokenUsage) TokenUsage {
	return TokenUsage{
		InputTokens:       u.InputTokens - earlier.InputTokens,
		CachedInputTokens: u.CachedInputTokens - earlier.CachedInputTokens,
		CacheWriteTokens:  u.CacheWriteTokens - earlier.CacheWriteTokens,
		OutputTokens:      u.OutputTokens - earlier.OutputTokens,
		ReasoningTokens:   u.ReasoningTokens - earlier.ReasoningTokens,
		TotalTokens:       u.TotalTokens - earlier.TotalTokens,
	}
}

// ---------------------------------------------------------------------------
// Thread items
// ---------------------------------------------------------------------------

// Item type discriminators carried in ThreadItem.
const (
	ItemUserMessage       = "userMessage"
	ItemAgentMessage      = "agentMessage"
	ItemReasoning         = "reasoning"
	ItemCommandExecution  = "commandExecution"
	ItemFileChange        = "fileChange"
	ItemMcpToolCall       = "mcpToolCall"
	ItemWebSearch         = "webSearch"
	ItemPlan              = "plan"
	ItemEnteredReviewMode = "enteredReviewMode"
	ItemExitedReviewMode  = "exitedReviewMode"
	ItemContextCompaction = "contextCompaction"
	ItemDynamicToolCall   = "dynamicToolCall"
	ItemCollabAgentCall   = "collabAgentToolCall"
	ItemImageView         = "imageView"
)

// Item is one unit of input or output inside a turn.
type Item interface {
	// ItemID returns the item id that deltas refer to.
	ItemID() string
	// ItemType returns the item's type discriminator.
	ItemType() string
}

// UserMessageItem is user-supplied input recorded in the thread.
type UserMessageItem struct {
	ID string `json:"id"`
	// Content holds the raw user input items as sent by the client.
	Content []jsontext.Value `json:"content,omitempty"`
}

// AgentMessageItem is the accumulated agent reply.
type AgentMessageItem struct {
	ID   string `json:"id"`
	Text string `json:"text"`
	// Phase is PhaseCommentary, PhaseFinalAnswer, or empty.
	Phase string `json:"phase,omitempty"`
}

// Agent message phases, in Responses API wire form.
const (
	PhaseCommentary  = "commentary"
	PhaseFinalAnswer = "final_answer"
)

// ReasoningItem holds streamed reasoning summaries and raw reasoning blocks.
type ReasoningItem struct {
	ID      string   `json:"id"`
	Summary []string `json:"summary,omitempty"`
	Content []string `json:"content,omitempty"`
}

// Command execution and file change item statuses.
const (
	ItemStatusInProgress = "inProgress"
	ItemStatusCompleted  = "completed"
	ItemStatusFailed     = "failed"
	ItemStatusDeclined   = "declined"
)

// CommandExecutionItem describes a command the agent runs.
type CommandExecutionItem struct {
	ID               string         `json:"id"`
	Command          string         `json:"command"`
	Cwd              string         `json:"cwd,omitempty"`
	Status           string         `json:"status"`
	CommandActions   jsontext.Value `json:"commandActions,omitempty"`
	AggregatedOutput string         `json:"aggregatedOutput,omitempty"`
	ExitCode         *int           `json:"exitCode,omitzero"`
	DurationMs       *int64         `json:"durationMs,omitzero"`
}

// Patch change kinds.
const (
	PatchAdd    = "add"
	PatchDelete = "delete"
	PatchUpdate = "update"
)

// PatchChangeKind is the kind of one file edit.
type PatchChangeKind struct {
	// Type is PatchAdd, PatchDelete, or PatchUpdate.
	Type string `json:"type"`
	// MovePath is the destination of a renaming update.
	MovePath string `json:"move_path,omitempty"`
}

// FileChange is one proposed edit inside a FileChangeItem.
type FileChange struct {
	Path string          `json:"path"`
	Kind PatchChangeKind `json:"kind"`
	Diff string          `json:"diff"`
}

// FileChangeItem describes proposed edits.
type FileChangeItem struct {
	ID      string       `json:"id"`
	Changes []FileChange `json:"changes"`
	Status  string       `json:"status"`
}

// McpToolCallItem describes an MCP (or connector app) tool call.
type McpToolCallItem struct {
	ID         string         `json:"id"`
	Server     string         `json:"server"`
	Tool       string         `json:"tool"`
	Status     string         `json:"status"`
	Arguments  jsontext.Value `json:"arguments,omitempty"`
	AppContext jsontext.Value `json:"appContext,omitempty"`
	PluginID   string         `json:"pluginId,omitempty"`
	Result     jsontext.Value `json:"result,omitempty"`
	// Error is set when the call failed.
	Error      *McpToolCallError `json:"error,omitzero"`
	DurationMs *int64            `json:"durationMs,omitzero"`
}

// McpToolCallError reports a failed MCP tool call.
type McpToolCallError struct {
	Message string `json:"message"`
}

// DynamicToolCallItem is a call to a client-provided dynamic tool.
type DynamicToolCallItem struct {
	ID        string         `json:"id"`
	Tool      string         `json:"tool"`
	Namespace string         `json:"namespace,omitempty"`
	Status    string         `json:"status"`
	Arguments jsontext.Value `json:"arguments,omitempty"`
	// ContentItems is the tool output, once reported.
	ContentItems jsontext.Value `json:"contentItems,omitempty"`
	Success      *bool          `json:"success,omitzero"`
	DurationMs   *int64         `json:"durationMs,omitzero"`
}

// CollabAgentToolCallItem is a call that spawns or messages other agent
// threads.
type CollabAgentToolCallItem struct {
	ID                string         `json:"id"`
	Tool              string         `json:"tool"`
	Status            string         `json:"status"`
	SenderThreadID    string         `json:"senderThreadId"`
	ReceiverThreadIDs []string       `json:"receiverThreadIds"`
	Prompt            string         `json:"prompt,omitempty"`
	Model             string         `json:"model,omitempty"`
	AgentsStates      jsontext.Value `json:"agentsStates,omitempty"`
}

// ImageViewItem records the agent viewing a local image.
type ImageViewItem struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}

// WebSearchAction describes what a web search item is doing.
type WebSearchAction struct {
	// Type is "search", "openPage", or "findInPage".
	Type    string   `json:"type"`
	Query   string   `json:"query,omitempty"`
	Queries []string `json:"queries,omitempty"`
	URL     string   `json:"url,omitempty"`
	Pattern string   `json:"pattern,omitempty"`
}

// WebSearchItem is a web search issued by the agent.
type WebSearchItem struct {
	ID     string           `json:"id"`
	Query  string           `json:"query,omitempty"`
	Action *WebSearchAction `json:"action,omitzero"`
}

// PlanItem carries proposed plan text in plan mode.
type PlanItem struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// EnteredReviewModeItem is emitted when the reviewer starts.
type EnteredReviewModeItem struct {
	ID     string `json:"id"`
	Review string `json:"review"`
}

// ExitedReviewModeItem carries the final review text.
type ExitedReviewModeItem struct {
	ID     string `json:"id"`
	Review string `json:"review"`
}

// ContextCompactionItem is emitted when Codex compacts conversation history.
type ContextCompactionItem struct {
	ID string `json:"id"`
}

// UnknownItem preserves items whose type this client does not model, so that
// newer server versions do not break decoding.
type UnknownItem struct {
	ID   string         `json:"id"`
	Type string         `json:"type"`
	Raw  jsontext.Value `json:"-"`
}

// ItemID returns the item id.
func (i UserMessageItem) ItemID() string { return i.ID }

// ItemType returns the item type discriminator.
func (i UserMessageItem) ItemType() string { return ItemUserMessage }

// ItemID returns the item id.
func (i AgentMessageItem) ItemID() string { return i.ID }

// ItemType returns the item type discriminator.
func (i AgentMessageItem) ItemType() string { return ItemAgentMessage }

// ItemID returns the item id.
func (i ReasoningItem) ItemID() string { return i.ID }

// ItemType returns the item type discriminator.
func (i ReasoningItem) ItemType() string { return ItemReasoning }

// ItemID returns the item id.
func (i CommandExecutionItem) ItemID() string { return i.ID }

// ItemType returns the item type discriminator.
func (i CommandExecutionItem) ItemType() string { return ItemCommandExecution }

// ItemID returns the item id.
func (i FileChangeItem) ItemID() string { return i.ID }

// ItemType returns the item type discriminator.
func (i FileChangeItem) ItemType() string { return ItemFileChange }

// ItemID returns the item id.
func (i McpToolCallItem) ItemID() string { return i.ID }

// ItemType returns the item type discriminator.
func (i McpToolCallItem) ItemType() string { return ItemMcpToolCall }

// ItemID returns the item id.
func (i WebSearchItem) ItemID() string { return i.ID }

// ItemType returns the item type discriminator.
func (i WebSearchItem) ItemType() string { return ItemWebSearch }

// ItemID returns the item id.
func (i PlanItem) ItemID() string { return i.ID }

// ItemType returns the item type discriminator.
func (i PlanItem) ItemType() string { return ItemPlan }

// ItemID returns the item id.
func (i EnteredReviewModeItem) ItemID() string { return i.ID }

// ItemType returns the item type discriminator.
func (i EnteredReviewModeItem) ItemType() string { return ItemEnteredReviewMode }

// ItemID returns the item id.
func (i ExitedReviewModeItem) ItemID() string { return i.ID }

// ItemType returns the item type discriminator.
func (i ExitedReviewModeItem) ItemType() string { return ItemExitedReviewMode }

// ItemID returns the item id.
func (i ContextCompactionItem) ItemID() string { return i.ID }

// ItemType returns the item type discriminator.
func (i ContextCompactionItem) ItemType() string { return ItemContextCompaction }

// ItemID returns the item id.
func (i DynamicToolCallItem) ItemID() string { return i.ID }

// ItemType returns the item type discriminator.
func (i DynamicToolCallItem) ItemType() string { return ItemDynamicToolCall }

// ItemID returns the item id.
func (i CollabAgentToolCallItem) ItemID() string { return i.ID }

// ItemType returns the item type discriminator.
func (i CollabAgentToolCallItem) ItemType() string { return ItemCollabAgentCall }

// ItemID returns the item id.
func (i ImageViewItem) ItemID() string { return i.ID }

// ItemType returns the item type discriminator.
func (i ImageViewItem) ItemType() string { return ItemImageView }

// ItemID returns the item id.
func (i UnknownItem) ItemID() string { return i.ID }

// ItemType returns the item type discriminator as sent by the server.
func (i UnknownItem) ItemType() string { return i.Type }

// ThreadItem is the tagged union carried in turn responses and item/*
// notifications. Item holds the decoded value and Raw the original JSON.
type ThreadItem struct {
	// Item is the decoded item; unmodeled types decode to UnknownItem.
	Item Item
	// Raw is the original JSON object.
	Raw jsontext.Value
}

// Type returns the item's type discriminator, or "" for a zero value.
func (t ThreadItem) Type() string {
	if t.Item == nil {
		return ""
	}
	return t.Item.ItemType()
}

// ID returns the item id, or "" for a zero value.
func (t ThreadItem) ID() string {
	if t.Item == nil {
		return ""
	}
	return t.Item.ItemID()
}

// UnmarshalJSON decodes a tagged thread item, falling back to UnknownItem for
// types this client does not model.
func (t *ThreadItem) UnmarshalJSON(data []byte) error {
	t.Raw = append(jsontext.Value(nil), data...)

	var head struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	}
	if err := jsonx.Unmarshal(data, &head); err != nil {
		return fmt.Errorf("codex: decode thread item: %w", err)
	}

	decode := func(v Item) error {
		if err := jsonx.Unmarshal(data, v); err != nil {
			return fmt.Errorf("codex: decode %s item: %w", head.Type, err)
		}
		t.Item = v
		return nil
	}

	switch head.Type {
	case ItemUserMessage:
		return decode(&UserMessageItem{})
	case ItemAgentMessage:
		return decode(&AgentMessageItem{})
	case ItemReasoning:
		return decode(&ReasoningItem{})
	case ItemCommandExecution:
		return decode(&CommandExecutionItem{})
	case ItemFileChange:
		return decode(&FileChangeItem{})
	case ItemMcpToolCall:
		return decode(&McpToolCallItem{})
	case ItemWebSearch:
		return decode(&WebSearchItem{})
	case ItemPlan:
		return decode(&PlanItem{})
	case ItemEnteredReviewMode:
		return decode(&EnteredReviewModeItem{})
	case ItemExitedReviewMode:
		return decode(&ExitedReviewModeItem{})
	case ItemContextCompaction:
		return decode(&ContextCompactionItem{})
	case ItemDynamicToolCall:
		return decode(&DynamicToolCallItem{})
	case ItemCollabAgentCall:
		return decode(&CollabAgentToolCallItem{})
	case ItemImageView:
		return decode(&ImageViewItem{})
	default:
		t.Item = &UnknownItem{ID: head.ID, Type: head.Type, Raw: t.Raw}
		return nil
	}
}

// MarshalJSON re-emits the original JSON when available.
func (t ThreadItem) MarshalJSON() ([]byte, error) {
	if len(t.Raw) > 0 {
		return t.Raw, nil
	}
	if t.Item == nil {
		return []byte("null"), nil
	}
	return jsonx.Marshal(t.Item)
}

// ---------------------------------------------------------------------------
// Request and response payloads
// ---------------------------------------------------------------------------

// ThreadSettings are the configuration overrides shared by thread/start,
// thread/resume, and thread/fork. Zero fields keep the server's value.
type ThreadSettings struct {
	Model         string `json:"model,omitempty"`
	ModelProvider string `json:"modelProvider,omitempty"`
	Cwd           string `json:"cwd,omitempty"`
	// ApprovalPolicy and ApprovalsReviewer control escalation prompts; see
	// ApprovalMode.Settings for upstream's presets.
	ApprovalPolicy    ApprovalPolicy    `json:"approvalPolicy,omitempty"`
	ApprovalsReviewer ApprovalsReviewer `json:"approvalsReviewer,omitempty"`
	// Sandbox is the filesystem access preset.
	Sandbox SandboxMode `json:"sandbox,omitempty"`
	// Config holds config.toml overrides keyed by dotted path.
	Config                map[string]any `json:"config,omitempty"`
	BaseInstructions      string         `json:"baseInstructions,omitempty"`
	DeveloperInstructions string         `json:"developerInstructions,omitempty"`
	ServiceTier           string         `json:"serviceTier,omitempty"`
}

// StartThreadParams are the parameters of thread/start.
type StartThreadParams struct {
	ThreadSettings
	// Ephemeral creates an in-memory thread that is not persisted.
	Ephemeral bool `json:"ephemeral,omitzero"`
	// Personality is deprecated upstream: "none", "friendly", or "pragmatic".
	Personality string `json:"personality,omitempty"`
	// ServiceName tags thread-level metrics with the integration's name.
	ServiceName string `json:"serviceName,omitempty"`
	// ThreadSource is a client-supplied source classification.
	ThreadSource string `json:"threadSource,omitempty"`
	// SessionStartSource is "startup" or "clear".
	SessionStartSource string `json:"sessionStartSource,omitempty"`
}

// ResumeThreadParams are the parameters of thread/resume.
type ResumeThreadParams struct {
	ThreadID string `json:"threadId"`
	ThreadSettings
	Personality string `json:"personality,omitempty"`
	// ExcludeTurns returns only thread metadata, without Thread.Turns.
	ExcludeTurns bool `json:"excludeTurns,omitzero"`
}

// ForkThreadParams are the parameters of thread/fork.
type ForkThreadParams struct {
	ThreadID string `json:"threadId"`
	ThreadSettings
	// LastTurnID copies history through that turn, inclusive.
	LastTurnID string `json:"lastTurnId,omitempty"`
	// Ephemeral creates an in-memory fork.
	Ephemeral bool `json:"ephemeral,omitzero"`
	// ExcludeTurns returns only thread metadata, without Thread.Turns.
	ExcludeTurns bool `json:"excludeTurns,omitzero"`
	// ThreadSource is a client-supplied source classification.
	ThreadSource string `json:"threadSource,omitempty"`
}

// ThreadResult is the common `{ "thread": ... }` response body.
type ThreadResult struct {
	Thread Thread `json:"thread"`
}

// ReadThreadParams are the parameters of thread/read.
type ReadThreadParams struct {
	ThreadID     string `json:"threadId"`
	IncludeTurns bool   `json:"includeTurns,omitzero"`
}

// Thread list sort keys.
const (
	SortKeyCreatedAt = "created_at"
	SortKeyUpdatedAt = "updated_at"
	SortKeyRecencyAt = "recency_at"
)

// Thread list sort directions.
const (
	SortAscending  = "asc"
	SortDescending = "desc"
)

// ListThreadsParams are the parameters of thread/list.
type ListThreadsParams struct {
	Cursor         string   `json:"cursor,omitempty"`
	Limit          int      `json:"limit,omitzero"`
	SortKey        string   `json:"sortKey,omitempty"`
	SortDirection  string   `json:"sortDirection,omitempty"`
	ModelProviders []string `json:"modelProviders,omitempty"`
	SourceKinds    []string `json:"sourceKinds,omitempty"`
	Archived       bool     `json:"archived,omitzero"`
	Cwd            []string `json:"cwd,omitempty"`
	UseStateDBOnly bool     `json:"useStateDbOnly,omitzero"`
	SearchTerm     string   `json:"searchTerm,omitempty"`
}

// ListThreadsResult is one page of thread/list results.
type ListThreadsResult struct {
	Data []Thread `json:"data"`
	// NextCursor is empty on the final page.
	NextCursor string `json:"nextCursor,omitempty"`
}

// UnsubscribeResult reports the outcome of thread/unsubscribe.
type UnsubscribeResult struct {
	// Status is "unsubscribed", "notSubscribed", or "notLoaded".
	Status string `json:"status"`
}

// TurnOptions are the per-turn overrides accepted by turn/start. When set they
// become the defaults for later turns on the same thread, except OutputSchema
// which applies only to the current turn.
type TurnOptions struct {
	Model             string            `json:"model,omitempty"`
	Cwd               string            `json:"cwd,omitempty"`
	ApprovalPolicy    ApprovalPolicy    `json:"approvalPolicy,omitempty"`
	ApprovalsReviewer ApprovalsReviewer `json:"approvalsReviewer,omitempty"`
	// SandboxPolicy overrides the sandbox; SandboxMode.Policy converts a
	// preset.
	SandboxPolicy *SandboxPolicy `json:"sandboxPolicy,omitzero"`
	// Effort is a reasoning effort the model advertises, such as "low",
	// "medium", or "high".
	Effort string `json:"effort,omitempty"`
	// Summary is "auto", "concise", "detailed", or "none".
	Summary     string `json:"summary,omitempty"`
	Personality string `json:"personality,omitempty"`
	// ServiceTier updates the thread default; ServiceTierForTurn applies
	// only to the turn this request starts.
	ServiceTier        string `json:"serviceTier,omitempty"`
	ServiceTierForTurn string `json:"serviceTierForTurn,omitempty"`
	// OutputSchema is a JSON Schema constraining the final assistant message
	// of this turn only.
	OutputSchema jsontext.Value `json:"outputSchema,omitempty"`
	// TurnTrigger labels what started the turn; it grants no authority.
	TurnTrigger string `json:"turnTrigger,omitempty"`
}

// ExternalMessage is untrusted content delivered by another agent, tool, or
// application. It has tool-level authority, below user and developer
// instructions, and never counts as user approval. Pass it to
// StartExternalTurn.
type ExternalMessage struct {
	// ToolName identifies the tool delivering the content.
	ToolName  string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	// Output is a string or a list of Responses-compatible function output
	// content items.
	Output any `json:"output"`
}

// StartTurnParams are the parameters of turn/start.
type StartTurnParams struct {
	ThreadID string      `json:"threadId"`
	Input    []InputItem `json:"input"`
	// ToolOutput replaces Input with external content; see ExternalMessage.
	ToolOutput *ExternalMessage `json:"toolOutput,omitzero"`
	TurnOptions
}

// StartTurnResult is the turn/start response body.
type StartTurnResult struct {
	Turn Turn `json:"turn"`
}

// SteerTurnParams are the parameters of turn/steer.
type SteerTurnParams struct {
	ThreadID string      `json:"threadId"`
	Input    []InputItem `json:"input"`
	// ExpectedTurnID must match the active turn id.
	ExpectedTurnID string `json:"expectedTurnId"`
}

// SteerTurnResult is the turn/steer response body.
type SteerTurnResult struct {
	TurnID string `json:"turnId"`
}

// InterruptTurnParams are the parameters of turn/interrupt.
type InterruptTurnParams struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
}

// ---------------------------------------------------------------------------
// Notification payloads
// ---------------------------------------------------------------------------

// Notification method names this client handles.
const (
	MethodThreadStarted       = "thread/started"
	MethodThreadStatusChanged = "thread/status/changed"
	MethodThreadArchived      = "thread/archived"
	MethodThreadUnarchived    = "thread/unarchived"
	MethodThreadDeleted       = "thread/deleted"
	MethodThreadClosed        = "thread/closed"
	MethodThreadNameUpdated   = "thread/name/updated"
	MethodTokenUsageUpdated   = "thread/tokenUsage/updated"

	MethodTurnStarted   = "turn/started"
	MethodTurnCompleted = "turn/completed"
	MethodTurnDiff      = "turn/diff/updated"
	MethodTurnPlan      = "turn/plan/updated"

	MethodItemStarted   = "item/started"
	MethodItemCompleted = "item/completed"

	MethodAgentMessageDelta           = "item/agentMessage/delta"
	MethodPlanDelta                   = "item/plan/delta"
	MethodReasoningSummaryTextDelta   = "item/reasoning/summaryTextDelta"
	MethodReasoningSummaryPartAdded   = "item/reasoning/summaryPartAdded"
	MethodReasoningTextDelta          = "item/reasoning/textDelta"
	MethodCommandExecutionOutputDelta = "item/commandExecution/outputDelta"

	MethodError                 = "error"
	MethodServerRequestResolved = "serverRequest/resolved"
	MethodAccountUpdated        = "account/updated"
	MethodLoginCompleted        = "account/login/completed"
)

// ThreadStartedParams is the payload of thread/started.
type ThreadStartedParams struct {
	Thread Thread `json:"thread"`
}

// ThreadStatusChangedParams is the payload of thread/status/changed.
type ThreadStatusChangedParams struct {
	ThreadID string       `json:"threadId"`
	Status   ThreadStatus `json:"status"`
}

// ThreadIDParams is the payload of the thread/archived, thread/unarchived,
// thread/deleted, and thread/closed notifications.
type ThreadIDParams struct {
	ThreadID string `json:"threadId"`
}

// ThreadNameUpdatedParams is the payload of thread/name/updated.
type ThreadNameUpdatedParams struct {
	ThreadID string `json:"threadId"`
	// Name is empty when the name was cleared.
	Name string `json:"threadName,omitempty"`
}

// TurnParams is the payload of turn/started and turn/completed.
type TurnParams struct {
	// ThreadID is present when the server scopes the notification.
	ThreadID string `json:"threadId,omitempty"`
	Turn     Turn   `json:"turn"`
}

// TurnDiffParams is the payload of turn/diff/updated.
type TurnDiffParams struct {
	ThreadID string `json:"threadId,omitempty"`
	TurnID   string `json:"turnId"`
	Diff     string `json:"diff"`
}

// Plan step statuses.
const (
	PlanStepPending    = "pending"
	PlanStepInProgress = "inProgress"
	PlanStepCompleted  = "completed"
)

// PlanStep is one entry of an agent plan.
type PlanStep struct {
	Step   string `json:"step"`
	Status string `json:"status"`
}

// TurnPlanParams is the payload of turn/plan/updated.
type TurnPlanParams struct {
	ThreadID    string     `json:"threadId,omitempty"`
	TurnID      string     `json:"turnId"`
	Explanation string     `json:"explanation,omitempty"`
	Plan        []PlanStep `json:"plan"`
}

// ItemParams is the payload of item/started and item/completed.
type ItemParams struct {
	ThreadID string     `json:"threadId,omitempty"`
	TurnID   string     `json:"turnId,omitempty"`
	Item     ThreadItem `json:"item"`
}

// DeltaParams is the payload of the text delta notifications
// (item/agentMessage/delta, item/plan/delta, item/reasoning/textDelta, and
// item/reasoning/summaryTextDelta).
type DeltaParams struct {
	ThreadID string `json:"threadId,omitempty"`
	TurnID   string `json:"turnId,omitempty"`
	ItemID   string `json:"itemId"`
	Delta    string `json:"delta"`
	// SummaryIndex increments when a new reasoning summary section opens.
	SummaryIndex int `json:"summaryIndex,omitzero"`
	// ContentIndex identifies the raw reasoning content block.
	ContentIndex int `json:"contentIndex,omitzero"`
}

// CommandOutputDeltaParams is the payload of
// item/commandExecution/outputDelta.
type CommandOutputDeltaParams struct {
	ThreadID string `json:"threadId,omitempty"`
	TurnID   string `json:"turnId,omitempty"`
	ItemID   string `json:"itemId"`
	// Delta is the appended output text.
	Delta string `json:"delta"`
}

// ErrorParams is the payload of the error notification, which reports a
// turn error the server may still be retrying.
type ErrorParams struct {
	ThreadID string    `json:"threadId,omitempty"`
	TurnID   string    `json:"turnId,omitempty"`
	Error    TurnError `json:"error"`
	// WillRetry reports that the server retries and the turn continues.
	WillRetry bool `json:"willRetry"`
}

// TokenUsageParams is the payload of thread/tokenUsage/updated.
type TokenUsageParams struct {
	ThreadID string           `json:"threadId,omitempty"`
	TurnID   string           `json:"turnId,omitempty"`
	Usage    ThreadTokenUsage `json:"tokenUsage"`
}

// ServerRequestResolvedParams is the payload of serverRequest/resolved.
type ServerRequestResolvedParams struct {
	ThreadID  string         `json:"threadId,omitempty"`
	RequestID jsontext.Value `json:"requestId,omitempty"`
}
