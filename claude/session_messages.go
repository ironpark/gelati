package claude

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"io"
	"io/fs"
	"maps"
	"os"
	"regexp"
	"unicode/utf8"

	"github.com/ironpark/gelati/internal/jsonx"
)

// Reading session and subagent conversations from local transcripts, and the
// entry-to-message conversion (see session_chain.go) and agent metadata
// handling shared with the SessionStore readers.

const (
	// precompactSkipThreshold is the transcript size above which
	// GetSessionMessages reads only what follows the last compact boundary.
	precompactSkipThreshold = 5 << 20

	// precompactBoundaryWindow is how far into a line the
	// "compact_boundary" marker must start for the line to be parsed as a
	// boundary candidate during the pre-compact skip.
	precompactBoundaryWindow = 256
)

// ---------------------------------------------------------------------------
// GetSessionMessages: transcript reconstruction
// ---------------------------------------------------------------------------

// readTranscript reads the first size bytes of a transcript for message
// reconstruction. When size exceeds precompactSkipThreshold and
// skipPrecompact is set, only the part from the last compact boundary on
// is returned (see skipPrecompactLines). It returns nil on error.
func readTranscript(path string, size int64, skipPrecompact bool) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, size))
	if err != nil {
		return nil
	}
	if size > precompactSkipThreshold && skipPrecompact {
		return skipPrecompactLines(b)
	}
	return b
}

// skipPrecompactLines drops everything before the last compact boundary
// that did not preserve messages (a system compact_boundary line whose
// "compact_boundary" marker starts within its first 256 bytes and whose
// compactMetadata has neither preservedSegment nor preservedMessages).
// Attribution snapshots are dropped too, except the last one after that
// boundary, which is moved to the end. A final line without a newline is
// never treated as a boundary.
func skipPrecompactLines(b []byte) []byte {
	const snapshotPrefix = `{"type":"attribution-snapshot"`
	marker := []byte(`"compact_boundary"`)
	var out, lastSnap []byte
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		terminated := i >= 0
		if !terminated {
			i = len(b) - 1
		}
		line := b[:i+1]
		b = b[i+1:]
		if bytes.HasPrefix(line, []byte(snapshotPrefix)) {
			lastSnap = line
			continue
		}
		if idx := bytes.Index(line, marker); terminated && idx >= 0 && idx < precompactBoundaryWindow {
			var entry map[string]any
			if json.Unmarshal(line, &entry, jsonx.Foreign) == nil && entry["type"] == "system" && entry["subtype"] == "compact_boundary" {
				meta, _ := entry["compactMetadata"].(map[string]any)
				if !jsTruthy(meta["preservedSegment"]) && !jsTruthy(meta["preservedMessages"]) {
					out, lastSnap = out[:0], nil
				}
			}
		}
		out = append(out, line...)
	}
	if lastSnap != nil {
		if len(out) > 0 && out[len(out)-1] != '\n' {
			out = append(out, '\n')
		}
		out = append(out, lastSnap...)
	}
	return out
}

// precompactSkipEnabled reports whether the large-transcript pre-compact
// skip applies: CLAUDE_CODE_DISABLE_PRECOMPACT_SKIP=1/true/yes/on turns it
// off.
func precompactSkipEnabled(getenv func(string) string) bool {
	return !envTruthy(getenv("CLAUDE_CODE_DISABLE_PRECOMPACT_SKIP"))
}

// parseJSONLObjects parses JSONL content into its JSON object lines,
// skipping blank and corrupt lines. Invalid UTF-8 degrades to U+FFFD.
func parseJSONLObjects(content []byte) []map[string]any {
	var out []map[string]any
	for len(content) > 0 {
		i := bytes.IndexByte(content, '\n')
		if i < 0 {
			i = len(content)
		}
		line := bytes.TrimLeft(content[:i], "\x00\x01\x02\x03\x04\x05\x06\x07\x08\t\n\v\f\r\x0e\x0f\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f ")
		content = content[min(i+1, len(content)):]
		if len(line) == 0 {
			continue
		}
		var entry map[string]any
		if json.Unmarshal(line, &entry, jsonx.Foreign) == nil && entry != nil {
			out = append(out, entry)
		}
	}
	return out
}

// entriesToSessionMessages reconstructs the visible conversation from every
// parsed line of a main transcript and applies paging. It is shared by the
// filesystem and SessionStore paths.
func entriesToSessionMessages(parsed []map[string]any, opts *SessionMessagesOptions) []SessionMessage {
	messages := sessionMessagesFromParsed(parsed, opts.IncludeSystemMessages)
	return nilIfEmpty(pageSlice(messages, opts.Limit, opts.Offset))
}

// getSessionMessages implements GetSessionMessages.
func (s localSessions) getSessionMessages(sessionID string, opts *SessionMessagesOptions) []SessionMessage {
	if opts == nil {
		opts = &SessionMessagesOptions{}
	}
	if !validateUUID(sessionID) {
		return nil
	}
	found, ok := s.findSessionFile(sessionID, opts.Directory)
	if !ok {
		return nil
	}
	content := readTranscript(found.path, found.size, s.skipPrecompact)
	if len(content) == 0 {
		return nil
	}
	return entriesToSessionMessages(parseJSONLObjects(content), opts)
}

// GetSessionMessages reads a session's conversation from its JSONL
// transcript. It rebuilds the conversation chain from the parentUuid links
// (following the relinking that compactions with preserved messages
// record) and returns its messages in chronological order: user and
// assistant messages, plus system messages when opts.IncludeSystemMessages
// is set. Sidechain, team and meta messages are left out, except meta
// messages from channel, observer and peer origins; compact summaries are
// kept. Prompts queued during a turn (queued_command attachments) are
// returned as user messages with IsQueuedCommand set once a turn reached
// them, and when they trail the conversation.
//
// For transcripts over 5 MiB, only the part after the last compact
// boundary is read, unless CLAUDE_CODE_DISABLE_PRECOMPACT_SKIP is set to
// 1, true, yes or on.
//
// opts.Directory is the project directory (its git worktrees are searched
// too); empty searches every project. opts.Offset skips messages from the
// start and a positive opts.Limit caps the count. opts may be nil.
//
// It returns nil when sessionID is not a UUID, the transcript is not found,
// or it has no visible messages. Corrupt lines are skipped. The error is
// currently always nil.
func GetSessionMessages(sessionID string, opts *SessionMessagesOptions) ([]SessionMessage, error) {
	return localSessionsFromEnv().getSessionMessages(sessionID, opts), nil
}

// ---------------------------------------------------------------------------
// Subagent transcripts
// ---------------------------------------------------------------------------

// readAgentMetadataSidecar reads the .meta.json sidecar beside a subagent
// transcript. It returns (nil, nil) when the sidecar is missing, is not
// valid JSON or is not a JSON object; other read errors (permission denied,
// a directory in its place) are returned.
func readAgentMetadataSidecar(transcriptPath string) (map[string]any, error) {
	b, err := os.ReadFile(agentMetadataSidecarPath(transcriptPath))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	if !utf8.Valid(b) {
		return nil, nil // Python's UnicodeDecodeError is a ValueError
	}
	var meta map[string]any
	if json.Unmarshal(b, &meta, jsonx.Foreign) != nil {
		return nil, nil
	}
	return meta, nil
}

// splitAgentMetadata separates the synthetic agent_metadata entries a
// subagent's SessionStore stream carries in place of the .meta.json sidecar
// from its transcript lines. The last metadata entry wins (it is rewritten
// on resume) and is returned as a copy; metadata is nil when there is none.
func splitAgentMetadata(entries []SessionStoreEntry) (metadata map[string]any, transcript []SessionStoreEntry) {
	for _, e := range entries {
		if e != nil && e["type"] == "agent_metadata" {
			metadata = maps.Clone(e)
		} else {
			transcript = append(transcript, e)
		}
	}
	return metadata, transcript
}

// toolUseIDRE and agentIDRE validate the parent ids taken from agent
// metadata; malformed values count as absent.
var (
	toolUseIDRE = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)
	agentIDRE   = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
)

// parentIDsFromAgentMetadata extracts toolUseId and parentAgentId from a
// sidecar or agent_metadata entry; non-string or malformed values count as
// absent.
func parentIDsFromAgentMetadata(meta map[string]any) (toolUseID, parentAgentID string) {
	toolUseID, parentAgentID = str(meta["toolUseId"]), str(meta["parentAgentId"])
	if !toolUseIDRE.MatchString(toolUseID) {
		toolUseID = ""
	}
	if !agentIDRE.MatchString(parentAgentID) {
		parentAgentID = ""
	}
	return toolUseID, parentAgentID
}

// entriesToSubagentMessages builds the subagent chain from parsed lines and
// applies paging. Every message carries the same parent ids.
func entriesToSubagentMessages(parsed []map[string]any, limit, offset int, parentToolUseID, parentAgentID string) []SessionMessage {
	return nilIfEmpty(pageSlice(subagentMessagesFromParsed(parsed, parentToolUseID, parentAgentID), limit, offset))
}

// listSubagents implements ListSubagents.
func (s localSessions) listSubagents(sessionID, directory string) []string {
	if !validateUUID(sessionID) {
		return nil
	}
	dir := s.resolveSubagentsDir(sessionID, directory)
	if dir == "" {
		return nil
	}
	var ids []string
	for _, f := range collectAgentFiles(dir) {
		ids = append(ids, f.agentID)
	}
	return ids
}

// ListSubagents lists the ids of a session's subagents by scanning
// <project>/<sessionID>/subagents/ (including nested directories such as
// workflows/<runId>/) for agent-<id>.jsonl transcripts.
//
// directory is the project directory (its git worktrees are searched too);
// empty searches every project. It returns nil when sessionID is not a
// UUID, the session is not found, or it has no subagents. The error is
// currently always nil.
func ListSubagents(sessionID, directory string) ([]string, error) {
	return localSessionsFromEnv().listSubagents(sessionID, directory), nil
}

// getSubagentMessages implements GetSubagentMessages.
func (s localSessions) getSubagentMessages(sessionID, agentID string, opts *SessionMessagesOptions) []SessionMessage {
	if opts == nil {
		opts = &SessionMessagesOptions{}
	}
	if !validateUUID(sessionID) || agentID == "" {
		return nil
	}
	dir := s.resolveSubagentsDir(sessionID, opts.Directory)
	if dir == "" {
		return nil
	}
	var match string
	for _, f := range collectAgentFiles(dir) {
		if f.agentID == agentID {
			match = f.path
			break
		}
	}
	if match == "" {
		return nil
	}
	content, err := os.ReadFile(match)
	if err != nil || len(content) == 0 {
		return nil
	}
	// The sidecar records which Agent tool_use spawned the subagent. Any
	// failure to read it degrades to "no metadata".
	meta, _ := readAgentMetadataSidecar(match)
	toolUseID, parentAgentID := parentIDsFromAgentMetadata(meta)
	return entriesToSubagentMessages(parseJSONLObjects(content), opts.Limit, opts.Offset, toolUseID, parentAgentID)
}

// GetSubagentMessages reads a subagent's conversation from its JSONL
// transcript and returns its user and assistant messages in chronological
// order. agentID is an id returned by ListSubagents.
//
// Every message's ParentToolUseID is the id of the Agent tool_use in the
// parent session that spawned the subagent, and ParentAgentID the spawning
// subagent for nested subagents; both come from the agent-<id>.meta.json
// sidecar beside the transcript and are empty when it is missing or
// unusable.
//
// opts.Directory is the project directory (its git worktrees are searched
// too); empty searches every project. opts.Offset and a positive
// opts.Limit page the result. opts may be nil. It returns nil when
// sessionID is not a UUID, agentID is empty, the session or subagent is not
// found, or the transcript has no messages. The error is currently always
// nil.
func GetSubagentMessages(sessionID, agentID string, opts *SessionMessagesOptions) ([]SessionMessage, error) {
	return localSessionsFromEnv().getSubagentMessages(sessionID, agentID, opts), nil
}
