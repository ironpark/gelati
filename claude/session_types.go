package claude

import (
	"context"
	"errors"
	"time"
)

// ---------------------------------------------------------------------------
// Session store
// ---------------------------------------------------------------------------

// DefaultSessionLoadTimeout is the per-call bound on SessionStore reads during
// resume materialization when Options.LoadTimeout is zero.
const DefaultSessionLoadTimeout = 60 * time.Second

// ErrSessionNotFound is returned (wrapped) when a session or subagent
// transcript does not exist.
var ErrSessionNotFound = errors.New("claude: session not found")

// ErrInvalidSessionID is returned (wrapped) when a session id is not a UUID.
// Session ids are used as path components, so anything else is rejected.
var ErrInvalidSessionID = errors.New("claude: invalid session id")

// SessionKey identifies a session transcript, or one of its subagent
// transcripts, in a SessionStore.
type SessionKey struct {
	// ProjectKey is a caller-defined scope. The SDK defaults it to the
	// sanitized working directory (see ProjectKeyForDirectory); multi-tenant
	// deployments may use a tenant id or project name instead.
	ProjectKey string `json:"project_key"`
	SessionID  string `json:"session_id"`
	// Subpath is empty for the main transcript and set for subagent files,
	// e.g. "subagents/agent-<id>", mirroring the on-disk layout. Adapters
	// should treat it as an opaque storage key suffix.
	Subpath string `json:"subpath,omitempty"`
}

// SessionStoreEntry is one JSONL transcript line. Its shape is the CLI's
// internal on-disk format; adapters must treat it as an opaque JSON object
// and return it deep-equal from Load (byte-equal serialization is not
// required). Every entry has a "type" field; most also carry "uuid" and
// "timestamp".
type SessionStoreEntry = map[string]any

// SessionStoreListEntry is one session returned by SessionLister.
type SessionStoreListEntry struct {
	SessionID string `json:"session_id"`
	// MTime is the last-modified time in Unix epoch milliseconds.
	MTime int64 `json:"mtime"`
}

// SessionSummaryEntry is an incrementally maintained session summary. Stores
// produce it with FoldSessionSummary inside Append and persist it verbatim.
type SessionSummaryEntry struct {
	SessionID string `json:"session_id"`
	// MTime is the storage write time of the summary in Unix epoch
	// milliseconds, on the same clock as SessionStoreListEntry.MTime.
	// FoldSessionSummary preserves the value from prev; stamp it after
	// persisting. Do not derive it from entry timestamps.
	MTime int64 `json:"mtime"`
	// Data is opaque SDK-owned state. Persist it verbatim; do not
	// interpret it. Its keys match the TypeScript SDK's (camelCase), so
	// summaries are interchangeable between SDKs.
	Data map[string]any `json:"data"`
}

// SessionListSubkeysKey is the argument to SessionSubkeyLister.ListSubkeys.
type SessionListSubkeysKey struct {
	ProjectKey string `json:"project_key"`
	SessionID  string `json:"session_id"`
}

// SessionStoreFlushMode controls when mirrored transcript entries are flushed
// to a SessionStore.
type SessionStoreFlushMode string

const (
	// SessionStoreFlushBatched buffers entries and flushes once per turn
	// (on the result message) or when the buffer exceeds 500 entries or
	// 1 MiB. It keeps adapter latency off the streaming path.
	SessionStoreFlushBatched SessionStoreFlushMode = "batched"
	// SessionStoreFlushEager starts a background flush after every
	// transcript frame. Appends stay serialized in order; a slow adapter
	// sees frames coalesced while it is busy.
	SessionStoreFlushEager SessionStoreFlushMode = "eager"
)

// SessionStore mirrors session transcripts to external storage. The CLI
// still writes its transcript to local disk; the store receives a secondary
// copy. The SDK never deletes from a store except through
// DeleteSessionViaStore; retention is the adapter's responsibility.
//
// Only Append and Load are required. A store may also implement any of
// SessionLister, SessionSummaryLister, SessionDeleter and SessionSubkeyLister;
// the SDK probes for them with type assertions.
type SessionStore interface {
	// Append mirrors a batch of entries for key. It is called after the
	// CLI's local write succeeded, at roughly 100ms cadence during active
	// turns. Within a process, persist entries in call order. Entries with
	// a "uuid" should be treated as idempotent (upsert / ignore duplicate);
	// entries without one are appended as-is. A failing batch is retried
	// (3 attempts in total) and then dropped and reported as a
	// MirrorErrorMessage; the session itself continues.
	Append(ctx context.Context, key SessionKey, entries []SessionStoreEntry) error

	// Load returns every entry appended for key, in order, or nil when the
	// key was never written. Adapters that cannot tell "never written" from
	// "emptied" may return nil for both.
	Load(ctx context.Context, key SessionKey) ([]SessionStoreEntry, error)
}

// SessionLister is an optional SessionStore extension that lists the
// sessions in a project. Result order is unspecified; the SDK sorts by MTime.
// ContinueConversation with a store, and ListSessionsFromStore, require it.
type SessionLister interface {
	ListSessions(ctx context.Context, projectKey string) ([]SessionStoreListEntry, error)
}

// SessionSummaryLister is an optional SessionStore extension that returns
// every session summary of a project in one call, excluding subpath keys.
// Stores maintain the summaries with FoldSessionSummary inside Append (skip
// the fold for keys with a Subpath) and must serialize that read-fold-write
// per session. Without it, ListSessionsFromStore falls back to ListSessions
// plus one Load per session.
type SessionSummaryLister interface {
	ListSessionSummaries(ctx context.Context, projectKey string) ([]SessionSummaryEntry, error)
}

// SessionDeleter is an optional SessionStore extension. Deleting a main
// transcript key (empty Subpath) must cascade to every subkey of the
// session; a key with a Subpath removes only that entry. Without it,
// deletion through the store is a no-op.
type SessionDeleter interface {
	Delete(ctx context.Context, key SessionKey) error
}

// SessionSubkeyLister is an optional SessionStore extension that lists the
// subpaths stored under a session, such as subagent transcripts. Without it,
// resume materializes only the main transcript.
type SessionSubkeyLister interface {
	ListSubkeys(ctx context.Context, key SessionListSubkeysKey) ([]string, error)
}

// ---------------------------------------------------------------------------
// Session listing and reading
// ---------------------------------------------------------------------------

// SessionInfo is session metadata returned by ListSessions and friends. It
// holds only what a stat plus head/tail read can extract. Ported from
// SDKSessionInfo; optional fields are zero when unknown.
type SessionInfo struct {
	// SessionID is the session's UUID.
	SessionID string `json:"session_id"`
	// Summary is the display title: custom title, generated summary or
	// first prompt.
	Summary string `json:"summary"`
	// LastModified is the last modification time in Unix epoch
	// milliseconds.
	LastModified int64 `json:"last_modified"`
	// FileSize is the transcript size in bytes. Store-backed results
	// report the size of the transcript serialized as compact JSONL, or
	// zero when they come from a session summary.
	FileSize int64 `json:"file_size,omitzero"`
	// CustomTitle is the user-set or generated session title.
	CustomTitle string `json:"custom_title,omitempty"`
	// FirstPrompt is the first meaningful user prompt.
	FirstPrompt string `json:"first_prompt,omitempty"`
	// GitBranch is the git branch at the end of the session.
	GitBranch string `json:"git_branch,omitempty"`
	// Cwd is the session's working directory.
	Cwd string `json:"cwd,omitempty"`
	// Tag is the user-set session tag.
	Tag string `json:"tag,omitempty"`
	// CreatedAt is the creation time in Unix epoch milliseconds, taken
	// from the first entry's timestamp.
	CreatedAt int64 `json:"created_at,omitzero"`
}

// SessionMessage is a message read back from a session transcript: a user
// or assistant message, or a system message when
// SessionMessagesOptions.IncludeSystemMessages is set.
//
// The fields after ParentAgentID mirror runtime-only fields of the
// TypeScript SDK's SessionMessage that are not part of its declared type;
// they are zero when absent.
type SessionMessage struct {
	// Type is "user", "assistant" or "system".
	Type      string `json:"type"`
	UUID      string `json:"uuid"`
	SessionID string `json:"session_id"`
	// Message is the raw Anthropic API message (role, content, ...). For
	// system messages it is usually nil: their payload is in the
	// transcript entry itself.
	Message any `json:"message"`
	// ParentToolUseID is set on subagent messages: the id of the Agent
	// tool_use block in the parent session that spawned the subagent.
	ParentToolUseID string `json:"parent_tool_use_id,omitempty"`
	// ParentAgentID is set on subagent messages spawned by another
	// subagent: that subagent's agent id.
	ParentAgentID string `json:"parent_agent_id,omitempty"`

	// Timestamp is the entry's ISO-8601 timestamp.
	Timestamp string `json:"timestamp,omitempty"`
	// IsMeta marks messages not typed by the user: meta messages kept
	// for their origin (channel, observer, peer, ...), compact summaries
	// and transcript-only messages.
	IsMeta bool `json:"is_meta,omitzero"`
	// IsCompactSummary marks the summary that replaced the conversation
	// before a compaction.
	IsCompactSummary bool `json:"isCompactSummary,omitzero"`
	// IsQueuedCommand marks a prompt that was queued while a turn was
	// running and recorded as a queued_command attachment.
	IsQueuedCommand bool `json:"isQueuedCommand,omitzero"`
	// IsCompletedLocalCommand marks the record and output of a local slash
	// command that ran to completion.
	IsCompletedLocalCommand bool `json:"isCompletedLocalCommand,omitzero"`
	// InterruptedByShutdown marks a message cut short by a shutdown.
	InterruptedByShutdown bool `json:"interruptedByShutdown,omitzero"`
	// ToolDenialUnanswered is "stream-closed" when a permission prompt was
	// left unanswered because the stream closed; empty otherwise.
	ToolDenialUnanswered string `json:"toolDenialUnanswered,omitempty"`
	// Origin describes where a non-typed message came from (its "kind" is
	// e.g. "channel", "peer" or "task-notification").
	Origin map[string]any `json:"origin,omitempty"`
}

// ListSessionsOptions configures ListSessions and ListSessionsFromStore.
type ListSessionsOptions struct {
	// Directory scopes the listing to one project directory. Empty lists
	// every project (local) or uses the current directory (store).
	Directory string
	// Limit caps the number of sessions returned; zero means no limit.
	Limit int
	// Offset skips that many sessions, newest first.
	Offset int
	// ExcludeWorktrees stops a Directory listing from also including the
	// sessions of that repository's git worktrees.
	ExcludeWorktrees bool
	// ExcludeProgrammatic hides programmatic and headless sessions: those
	// started through an SDK (entrypoints sdk-cli, sdk-ts, sdk-py,
	// sdk-py-client, sdk-go and sdk-go-client) and daemon or
	// daemon-worker sessions. It is the inverse of the TypeScript SDK's
	// includeProgrammatic and only applies to local listings; the store
	// variant ignores it.
	ExcludeProgrammatic bool
}

// SessionMessagesOptions configures GetSessionMessages,
// GetSubagentMessages and their store variants.
type SessionMessagesOptions struct {
	// Directory is the project directory; empty searches every project
	// (local) or uses the current directory (store).
	Directory string
	// Limit caps the number of messages returned; zero means no limit.
	Limit int
	// Offset skips that many messages from the start.
	Offset int
	// IncludeSystemMessages also returns the system messages of the
	// conversation chain (compact boundaries, informational notices, ...)
	// with Type "system". Subagent readers ignore it.
	IncludeSystemMessages bool
}

// ForkSessionOptions configures ForkSession and ForkSessionViaStore.
type ForkSessionOptions struct {
	// Directory is the project directory; empty searches every project
	// (local) or uses the current directory (store).
	Directory string
	// UpToMessageID truncates the fork after this message uuid; empty
	// copies the whole transcript.
	UpToMessageID string
	// Title is the fork's title; empty derives one from the source.
	Title string
}

// ForkSessionResult is the result of a fork.
type ForkSessionResult struct {
	// SessionID is the UUID of the new session.
	SessionID string `json:"session_id"`
}

// ImportSessionOptions configures ImportSessionToStore.
type ImportSessionOptions struct {
	// Directory is the project directory; empty searches every project.
	// It also selects the destination project key (see
	// ProjectKeyForDirectory), so empty means the current directory's key.
	Directory string
	// ExcludeSubagents skips the session's subagent transcripts.
	ExcludeSubagents bool
	// BatchSize is the number of entries per Append call; zero means 500.
	BatchSize int
}
