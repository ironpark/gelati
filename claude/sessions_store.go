package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// SessionStore-backed counterparts of the session readers in sessions.go.
// Ported from the *_from_store functions of _internal/sessions.py.

// ProjectKeyForDirectory returns the SessionStore project key for a
// directory; empty means the current working directory.
//
// It applies the same realpath, NFC normalization and djb2-hashed
// sanitization the CLI uses to name project directories, so keys match
// between local transcripts and store-mirrored ones, even on filesystems
// that decompose Unicode (macOS HFS+). When CLAUDE_CONFIG_DIR and a valid
// CLAUDE_CODE_PROJECT_DIR_NAME are both set in the process environment,
// the key is that name for every directory, as in the CLI.
func ProjectKeyForDirectory(directory string) string {
	return projectKeyForDirectory(directory, os.Getenv)
}

// projectKeyForDirectory is ProjectKeyForDirectory against an explicit
// environment.
func projectKeyForDirectory(directory string, getenv func(string) string) string {
	if override := projectDirNameOverrideFromEnv(getenv); override != "" {
		return override
	}
	if directory == "" {
		directory = "."
	}
	return sanitizePath(canonicalizePath(directory))
}

// storeProjectPath is the canonical project path for a store call's
// directory argument; empty means the current working directory.
func storeProjectPath(directory string) string {
	if directory == "" {
		directory = "."
	}
	return canonicalizePath(directory)
}

// entriesToJSONL serializes store entries to JSONL the way the Python SDK
// does (json.dumps with compact separators and ensure_ascii), hoisting
// "type" to the front of each object, where the CLI writes it too: adapters
// may reorder keys (Postgres JSONB does).
// The remaining keys are written in sorted order.
func entriesToJSONL(entries []SessionStoreEntry) string {
	return asciiEscapeJSON(compactJSONL(entries))
}

// jsonlByteSize returns the byte size of entries as JSON.stringify lines, the
// FileSize the TypeScript SDK reports for store-backed sessions.
func jsonlByteSize(entries []SessionStoreEntry) int64 {
	return stringifySize(compactJSONL(entries))
}

// stringifySize converts the length of compactJSONL output to the length
// JSON.stringify would produce: Go escapes U+2028 and U+2029 (6 bytes) where
// JSON.stringify writes them raw (3 bytes).
func stringifySize(raw []byte) int64 {
	return int64(len(raw) - 3*(bytes.Count(raw, []byte(`\u2028`))+bytes.Count(raw, []byte(`\u2029`))))
}

// compactJSONL is entriesToJSONL before the ASCII escaping: compact JSON
// lines with "type" first, as JSON.stringify writes them except that U+2028
// and U+2029 are escaped.
func compactJSONL(entries []SessionStoreEntry) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	encode := func(v any) {
		if err := enc.Encode(v); err != nil {
			buf.WriteString("null\n")
		}
		buf.Truncate(buf.Len() - 1) // drop Encode's trailing newline
	}
	for _, e := range entries {
		if e == nil {
			buf.WriteString("null\n")
			continue
		}
		buf.WriteByte('{')
		first := true
		writeKey := func(k string) {
			if !first {
				buf.WriteByte(',')
			}
			first = false
			encode(k)
			buf.WriteByte(':')
			encode(e[k])
		}
		if _, ok := e["type"]; ok {
			writeKey("type")
		}
		keys := make([]string, 0, len(e))
		for k := range e {
			if k != "type" {
				keys = append(keys, k)
			}
		}
		slices.Sort(keys)
		for _, k := range keys {
			writeKey(k)
		}
		buf.WriteString("}\n")
	}
	if len(entries) == 0 {
		buf.WriteByte('\n')
	}
	return buf.Bytes()
}

// asciiEscapeJSON rewrites every non-ASCII character (and DEL) of
// serialized JSON as a \uXXXX escape, as Python's ensure_ascii does. Such
// characters only occur inside strings, where the escape is equivalent.
func asciiEscapeJSON(b []byte) string {
	var sb strings.Builder
	sb.Grow(len(b))
	for i := 0; i < len(b); {
		c := b[i]
		if c < utf8.RuneSelf && c != 0x7f {
			sb.WriteByte(c)
			i++
			continue
		}
		r, size := utf8.DecodeRune(b[i:])
		i += size
		if r > 0xFFFF {
			r -= 0x10000
			fmt.Fprintf(&sb, `\u%04x\u%04x`, 0xD800+(r>>10), 0xDC00+(r&0x3FF))
		} else {
			fmt.Fprintf(&sb, `\u%04x`, r)
		}
	}
	return sb.String()
}

// jsonlToLite builds the head/tail lite shape from an in-memory transcript,
// with the byte semantics of readSessionLite.
func jsonlToLite(jsonl string, mtime int64) *liteSessionFile {
	size := len(jsonl)
	head := decodeUTF8Replace([]byte(jsonl[:min(size, liteReadBufSize)]))
	tail := head
	if size > liteReadBufSize {
		tail = decodeUTF8Replace([]byte(jsonl[size-liteReadBufSize:]))
	}
	return &liteSessionFile{mtime: mtime, size: int64(size), head: head, tail: tail}
}

// mtimeFromJSONLTail returns the last entry's timestamp in Unix epoch
// milliseconds, falling back to the current time when it is absent or
// unparseable.
func mtimeFromJSONLTail(jsonl string) int64 {
	trimmed := strings.TrimRightFunc(jsonl, pyIsSpace)
	lastLine := trimmed[strings.LastIndexByte(trimmed, '\n')+1:]
	var obj map[string]any
	if json.Unmarshal([]byte(lastLine), &obj) == nil {
		if ts, ok := obj["timestamp"].(string); ok {
			if ms, ok := isoToEpochMillis(ts); ok {
				return ms
			}
		}
	}
	return time.Now().UnixMilli()
}

// loadStoreEntriesAsJSONL loads a main transcript from the store and
// serializes it to JSONL; ok is false when the session has no entries.
// size is jsonlByteSize of the entries.
func loadStoreEntriesAsJSONL(ctx context.Context, store SessionStore, projectKey, sessionID string) (jsonl string, size int64, ok bool, err error) {
	entries, err := store.Load(ctx, SessionKey{ProjectKey: projectKey, SessionID: sessionID})
	if err != nil || len(entries) == 0 {
		return "", 0, false, err
	}
	raw := compactJSONL(entries)
	return asciiEscapeJSON(raw), stringifySize(raw), true, nil
}

// cmpMTimeDesc orders Unix-millisecond times newest first.
func cmpMTimeDesc(a, b int64) int {
	switch {
	case a > b:
		return -1
	case a < b:
		return 1
	}
	return 0
}

// deriveInfosViaLoad derives SessionInfo for each listing entry by loading
// its transcript and lite-parsing it like the filesystem path.
//
// Loads run concurrently, at most storeListLoadConcurrency at a time. A
// failing load degrades its row to an empty Summary (LastModified kept)
// instead of failing the whole listing; sidechain and summary-less sessions
// are dropped. The result keeps listing order. Only cancellation of ctx is
// returned as an error.
func deriveInfosViaLoad(ctx context.Context, store SessionStore, listing []SessionStoreListEntry, projectKey, projectPath string) ([]SessionInfo, error) {
	type outcome struct {
		jsonl string
		size  int64
		ok    bool
		err   error
	}
	settled := make([]outcome, len(listing))
	sem := make(chan struct{}, storeListLoadConcurrency)
	var wg sync.WaitGroup
spawn:
	for i, entry := range listing {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			break spawn
		}
		wg.Go(func() {
			defer func() { <-sem }()
			jsonl, size, ok, err := loadStoreEntriesAsJSONL(ctx, store, projectKey, entry.SessionID)
			settled[i] = outcome{jsonl, size, ok, err}
		})
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var results []SessionInfo
	for i, entry := range listing {
		o := settled[i]
		if o.err != nil {
			results = append(results, SessionInfo{SessionID: entry.SessionID, LastModified: entry.MTime})
			continue
		}
		if !o.ok {
			continue
		}
		info := parseSessionInfoFromLite(entry.SessionID, jsonlToLite(o.jsonl, entry.MTime), projectPath)
		if info == nil {
			continue // sidechain or no summary, as on disk
		}
		info.LastModified = entry.MTime
		info.FileSize = o.size
		results = append(results, *info)
	}
	return results, nil
}

// ListSessionsFromStore lists the sessions of a project from a SessionStore.
// It is the store-backed counterpart of ListSessions and derives the same
// metadata, so disk and store paths agree for the same transcript.
//
// opts.Directory selects the project key (see ProjectKeyForDirectory) and
// defaults to the current directory; opts.ExcludeWorktrees does not apply,
// since a store holds a single project key. Results are sorted newest
// first and paginated by opts.Offset and a positive opts.Limit. opts may
// be nil. FileSize is the size of the compact JSONL serialization of a
// loaded transcript, and zero for summary-backed rows.
//
// When the store implements SessionSummaryLister, listing costs one
// ListSessionSummaries call plus, when the store is also a SessionLister,
// one ListSessions call to gap-fill sessions whose summary is missing or
// stale (older than the session's MTime); only gap-filled sessions on the
// requested page are loaded. Summaries for sessions ListSessions no longer
// reports are dropped. Without ListSessions, sessions lacking a summary
// cannot be discovered and are omitted. A ListSessionSummaries error
// wrapping errors.ErrUnsupported falls back to the slow path.
//
// Otherwise the store must implement SessionLister: every session is
// loaded (at most 16 loads at a time) and lite-parsed, which on remote
// backends with many or large sessions can be expensive. A failing load
// yields a row with an empty Summary rather than an error. If the store
// implements neither interface, an error wrapping errors.ErrUnsupported is
// returned.
//
// Sidechain sessions and sessions without a summary are omitted. On the
// summary path, a gap-filled session can still resolve to nothing after
// pagination, so a page may come back short.
func ListSessionsFromStore(ctx context.Context, store SessionStore, opts *ListSessionsOptions) ([]SessionInfo, error) {
	if opts == nil {
		opts = &ListSessionsOptions{}
	}
	projectPath := storeProjectPath(opts.Directory)
	projectKey := ProjectKeyForDirectory(opts.Directory)
	lister, hasList := store.(SessionLister)

	if summaryLister, ok := store.(SessionSummaryLister); ok {
		summaries, err := summaryLister.ListSessionSummaries(ctx, projectKey)
		switch {
		case err == nil:
			return listFromSummaries(ctx, store, lister, summaries, projectKey, projectPath, opts.Limit, opts.Offset)
		case !errors.Is(err, errors.ErrUnsupported):
			return nil, err
		}
	}

	if !hasList {
		return nil, fmt.Errorf("claude: session store implements neither ListSessionSummaries nor ListSessions; "+
			"cannot list sessions: %w", errors.ErrUnsupported)
	}
	// Copy: the adapter may return its internal state.
	listing, err := lister.ListSessions(ctx, projectKey)
	if err != nil {
		return nil, err
	}
	listing = slices.Clone(listing)
	// Filter (sidechain/empty) before paginating so Limit and Offset index
	// the same set as the disk path.
	results, err := deriveInfosViaLoad(ctx, store, listing, projectKey, projectPath)
	if err != nil {
		return nil, err
	}
	return applySortLimitOffset(results, opts.Limit, opts.Offset), nil
}

// listFromSummaries is the ListSessionsFromStore fast path. lister is nil
// when the store does not implement SessionLister.
func listFromSummaries(ctx context.Context, store SessionStore, lister SessionLister,
	summaries []SessionSummaryEntry, projectKey, projectPath string, limit, offset int,
) ([]SessionInfo, error) {
	var listing []SessionStoreListEntry
	knownMTimes := map[string]int64{}
	if lister != nil {
		var err error
		if listing, err = lister.ListSessions(ctx, projectKey); err != nil {
			return nil, err
		}
		for _, e := range listing {
			knownMTimes[e.SessionID] = e.MTime
		}
	}

	// One slot per session: fresh summaries carry their info up front;
	// sessions that are listed but lack a fresh summary get a placeholder
	// that is gap-filled by loading after pagination.
	type slot struct {
		mtime     int64
		sessionID string
		info      *SessionInfo
	}
	var slots []*slot
	fresh := map[string]bool{}
	for _, s := range summaries {
		if lister != nil {
			known, ok := knownMTimes[s.SessionID]
			if !ok || s.MTime < known {
				// Gone from the listing, or a stale summary to re-fold
				// from source.
				continue
			}
		}
		fresh[s.SessionID] = true
		// Summary-backed sidechain/empty sessions are dropped before
		// pagination so they do not consume page slots.
		if info := summaryEntryToSessionInfo(s, projectPath); info != nil {
			slots = append(slots, &slot{mtime: s.MTime, info: info})
		}
	}
	for _, e := range listing {
		if !fresh[e.SessionID] {
			slots = append(slots, &slot{mtime: e.MTime, sessionID: e.SessionID})
		}
	}

	// Paginate before loading, so the number of gap-fill loads is bounded
	// by the page size rather than by the number of missing summaries.
	slices.SortStableFunc(slots, func(a, b *slot) int { return cmpMTimeDesc(a.mtime, b.mtime) })
	page := slots
	if offset > 0 {
		page = page[min(offset, len(page)):]
	}
	if limit > 0 && limit < len(page) {
		page = page[:limit]
	}

	var toFill []SessionStoreListEntry
	for _, sl := range page {
		if sl.info == nil {
			toFill = append(toFill, SessionStoreListEntry{SessionID: sl.sessionID, MTime: sl.mtime})
		}
	}
	if len(toFill) > 0 {
		filled, err := deriveInfosViaLoad(ctx, store, toFill, projectKey, projectPath)
		if err != nil {
			return nil, err
		}
		bySID := make(map[string]*SessionInfo, len(filled))
		for i := range filled {
			bySID[filled[i].SessionID] = &filled[i]
		}
		for _, sl := range page {
			if sl.info == nil {
				sl.info = bySID[sl.sessionID]
			}
		}
	}

	// Placeholders that resolved to nothing are dropped after pagination;
	// only they can short a page.
	var out []SessionInfo
	for _, sl := range page {
		if sl.info != nil {
			out = append(out, *sl.info)
		}
	}
	return out, nil
}

// GetSessionInfoFromStore reads the metadata of one session from a
// SessionStore; it is the store-backed counterpart of GetSessionInfo.
// directory selects the project key and defaults to the current directory.
//
// LastModified is the timestamp of the last entry (the current time when it
// has none) and FileSize the size of the transcript serialized as compact
// JSONL, as in the TypeScript SDK. It returns (nil, nil) when sessionID is
// not a UUID, the session has no entries, or it is a sidechain or has no
// extractable summary. Store errors are returned.
func GetSessionInfoFromStore(ctx context.Context, store SessionStore, sessionID, directory string) (*SessionInfo, error) {
	if !validateUUID(sessionID) {
		return nil, nil
	}
	projectPath := storeProjectPath(directory)
	jsonl, size, ok, err := loadStoreEntriesAsJSONL(ctx, store, ProjectKeyForDirectory(directory), sessionID)
	if err != nil || !ok {
		return nil, err
	}
	info := parseSessionInfoFromLite(sessionID, jsonlToLite(jsonl, mtimeFromJSONLTail(jsonl)), projectPath)
	if info != nil {
		info.FileSize = size
	}
	return info, nil
}

// GetSessionMessagesFromStore reads a session's conversation from a
// SessionStore; it is the store-backed counterpart of GetSessionMessages,
// feeding the loaded entries straight into the chain builder.
//
// opts.Directory selects the project key and defaults to the current
// directory; opts.Offset and a positive opts.Limit page the result and
// opts.IncludeSystemMessages adds system messages. The transcript is never
// cut at compact boundaries (there is no size threshold for stores). opts
// may be nil. It returns nil when sessionID is not a UUID or the session
// has no visible messages. Store errors are returned.
func GetSessionMessagesFromStore(ctx context.Context, store SessionStore, sessionID string, opts *SessionMessagesOptions) ([]SessionMessage, error) {
	if opts == nil {
		opts = &SessionMessagesOptions{}
	}
	if !validateUUID(sessionID) {
		return nil, nil
	}
	key := SessionKey{ProjectKey: ProjectKeyForDirectory(opts.Directory), SessionID: sessionID}
	entries, err := store.Load(ctx, key)
	if err != nil || len(entries) == 0 {
		return nil, err
	}
	return entriesToSessionMessages(entries, opts), nil
}

// ListSubagentsFromStore lists the subagent ids of a session from a
// SessionStore; it is the store-backed counterpart of ListSubagents.
// directory selects the project key and defaults to the current directory.
//
// The ids are taken from subkeys under "subagents/" whose last path
// component is agent-<id> (including nested ones such as
// subagents/workflows/<runId>/agent-<id>), deduplicated in order. It
// returns nil when sessionID is not a UUID or the session has no
// subagents. The store must implement SessionSubkeyLister; otherwise an
// error wrapping errors.ErrUnsupported is returned. Store errors are
// returned.
func ListSubagentsFromStore(ctx context.Context, store SessionStore, sessionID, directory string) ([]string, error) {
	if !validateUUID(sessionID) {
		return nil, nil
	}
	subkeyLister, ok := store.(SessionSubkeyLister)
	if !ok {
		return nil, fmt.Errorf("claude: session store does not implement ListSubkeys; "+
			"cannot list subagents: %w", errors.ErrUnsupported)
	}
	subkeys, err := subkeyLister.ListSubkeys(ctx, SessionListSubkeysKey{
		ProjectKey: ProjectKeyForDirectory(directory), SessionID: sessionID,
	})
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var ids []string
	for _, subpath := range subkeys {
		if !strings.HasPrefix(subpath, "subagents/") {
			continue
		}
		last := subpath[strings.LastIndexByte(subpath, '/')+1:]
		if agentID, ok := strings.CutPrefix(last, "agent-"); ok && !seen[agentID] {
			seen[agentID] = true
			ids = append(ids, agentID)
		}
	}
	return ids, nil
}

// GetSubagentMessagesFromStore reads a subagent's conversation from a
// SessionStore; it is the store-backed counterpart of GetSubagentMessages.
//
// The transcript is found among the session's subkeys when the store
// implements SessionSubkeyLister (it may be nested, e.g.
// subagents/workflows/<runId>/agent-<id>); otherwise the direct subpath
// subagents/agent-<id> is loaded. ParentToolUseID and ParentAgentID come
// from the last agent_metadata entry of the subagent's stream (the store's
// copy of the .meta.json sidecar) and are empty without one.
//
// opts.Directory selects the project key and defaults to the current
// directory; opts.Offset and a positive opts.Limit page the result. opts
// may be nil. It returns nil when sessionID is not a UUID, agentID is
// empty, or the subagent is not found or has no messages. Store errors
// are returned.
func GetSubagentMessagesFromStore(ctx context.Context, store SessionStore, sessionID, agentID string, opts *SessionMessagesOptions) ([]SessionMessage, error) {
	if opts == nil {
		opts = &SessionMessagesOptions{}
	}
	if !validateUUID(sessionID) || agentID == "" {
		return nil, nil
	}
	projectKey := ProjectKeyForDirectory(opts.Directory)

	subpath := "subagents/agent-" + agentID
	if subkeyLister, ok := store.(SessionSubkeyLister); ok {
		subkeys, err := subkeyLister.ListSubkeys(ctx, SessionListSubkeysKey{ProjectKey: projectKey, SessionID: sessionID})
		if err != nil {
			return nil, err
		}
		target := "agent-" + agentID
		i := slices.IndexFunc(subkeys, func(sk string) bool {
			return strings.HasPrefix(sk, "subagents/") && sk[strings.LastIndexByte(sk, '/')+1:] == target
		})
		if i < 0 {
			return nil, nil
		}
		subpath = subkeys[i]
	}

	entries, err := store.Load(ctx, SessionKey{ProjectKey: projectKey, SessionID: sessionID, Subpath: subpath})
	if err != nil || len(entries) == 0 {
		return nil, err
	}
	// The agent_metadata entry is not a transcript line: take the parent
	// ids from it, then drop it.
	meta, transcript := splitAgentMetadata(entries)
	if len(transcript) == 0 {
		return nil, nil
	}
	toolUseID, parentAgentID := parentIDsFromAgentMetadata(meta)
	return entriesToSubagentMessages(transcript, opts.Limit, opts.Offset, toolUseID, parentAgentID), nil
}
