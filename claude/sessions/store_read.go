package sessions

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/ironpark/gelati/claude/internal/ids"
	"github.com/ironpark/gelati/claude/internal/transcript"
)

// Store-backed counterparts of the local session readers (list.go,
// messages.go). Ported from the *_from_store functions of
// _internal/sessions.py.

// storeListLoadConcurrency bounds the concurrent Store.Load calls issued by
// ListInStore, so large listings do not exhaust adapter connection pools or
// trip backend rate limits.
const storeListLoadConcurrency = 16

// loadStoreEntriesAsJSONL loads a main transcript from the store and
// serializes it to JSONL; ok is false when the session has no entries.
// size is the byte size of the entries as JSON.stringify lines (see
// stringifySize), the FileSize the TypeScript SDK reports.
func loadStoreEntriesAsJSONL(ctx context.Context, store Store, projectKey, sessionID string) (jsonl string, size int64, ok bool, err error) {
	entries, err := store.Load(ctx, Key{ProjectKey: projectKey, SessionID: sessionID})
	if err != nil || len(entries) == 0 {
		return "", 0, false, err
	}
	raw := compactJSONL(entries)
	return asciiEscapeJSON(raw), stringifySize(raw), true, nil
}

// deriveInfosViaLoad derives Info for each listing entry by loading its
// transcript and lite-parsing it like the filesystem path.
//
// Loads run concurrently, at most storeListLoadConcurrency at a time. A
// failing load degrades its row to an empty Summary (LastModified kept)
// instead of failing the whole listing; sidechain and summary-less sessions
// are dropped. The result keeps listing order. Only cancellation of ctx is
// returned as an error.
func deriveInfosViaLoad(ctx context.Context, store Store, listing []ListEntry, projectKey, projectPath string) ([]Info, error) {
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

	var results []Info
	for i, entry := range listing {
		o := settled[i]
		if o.err != nil {
			results = append(results, Info{SessionID: entry.SessionID, LastModified: entry.MTime})
			continue
		}
		if !o.ok {
			continue
		}
		info := parseSessionInfoFromLite(entry.SessionID, jsonlToLite(o.jsonl, entry.MTime), projectPath, "")
		if info == nil {
			continue // sidechain or no summary, as on disk
		}
		info.LastModified = entry.MTime
		info.FileSize = o.size
		results = append(results, *info)
	}
	return results, nil
}

// ListInStore lists the sessions of a project from a Store. It is the
// store-backed counterpart of List and derives the same metadata, so disk
// and store paths agree for the same transcript.
//
// opts.Directory selects the project key (see ProjectKey) and defaults to
// the current directory; opts.ExcludeWorktrees does not apply, since a store
// holds a single project key. Results are sorted newest first and paginated
// by opts.Offset and a positive opts.Limit. opts may be nil. FileSize is the
// size of the compact JSONL serialization of a loaded transcript, and zero
// for summary-backed rows.
//
// When the store implements SummaryLister, listing costs one
// ListSessionSummaries call plus, when the store is also a Lister, one
// ListSessions call to gap-fill sessions whose summary is missing or stale
// (older than the session's MTime); only gap-filled sessions on the
// requested page are loaded. Summaries for sessions ListSessions no longer
// reports are dropped. Without ListSessions, sessions lacking a summary
// cannot be discovered and are omitted. A ListSessionSummaries error
// wrapping errors.ErrUnsupported falls back to the slow path.
//
// Otherwise the store must implement Lister: every session is loaded (at
// most 16 loads at a time) and lite-parsed, which on remote backends with
// many or large sessions can be expensive. A failing load yields a row with
// an empty Summary rather than an error. If the store implements neither
// interface, an error wrapping errors.ErrUnsupported is returned.
//
// Sidechain sessions and sessions without a summary are omitted. On the
// summary path, a gap-filled session can still resolve to nothing after
// pagination, so a page may come back short.
func ListInStore(ctx context.Context, store Store, opts *ListOptions) ([]Info, error) {
	if opts == nil {
		opts = &ListOptions{}
	}
	projectPath := transcript.StoreProjectPath(opts.Directory)
	projectKey := ProjectKey(opts.Directory)
	lister, hasList := store.(Lister)

	if summaryLister, ok := store.(SummaryLister); ok {
		summaries, err := summaryLister.ListSessionSummaries(ctx, projectKey)
		switch {
		case err == nil:
			return listFromSummaries(ctx, store, lister, summaries, projectKey, projectPath, opts.Limit, opts.Offset)
		case !errors.Is(err, errors.ErrUnsupported):
			return nil, err
		}
	}

	if !hasList {
		return nil, fmt.Errorf("sessions: session store implements neither ListSessionSummaries nor ListSessions; "+
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
	slices.SortStableFunc(results, func(a, b Info) int { return cmpMTimeDesc(a.LastModified, b.LastModified) })
	return nilIfEmpty(paginate(results, opts.Limit, opts.Offset)), nil
}

// listFromSummaries is the ListInStore fast path. lister is nil when the
// store does not implement Lister.
func listFromSummaries(ctx context.Context, store Store, lister Lister,
	summaries []SummaryEntry, projectKey, projectPath string, limit, offset int,
) ([]Info, error) {
	var listing []ListEntry
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
		info      *Info
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
	page := paginate(slots, limit, offset)

	var toFill []ListEntry
	for _, sl := range page {
		if sl.info == nil {
			toFill = append(toFill, ListEntry{SessionID: sl.sessionID, MTime: sl.mtime})
		}
	}
	if len(toFill) > 0 {
		filled, err := deriveInfosViaLoad(ctx, store, toFill, projectKey, projectPath)
		if err != nil {
			return nil, err
		}
		bySID := make(map[string]*Info, len(filled))
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
	var out []Info
	for _, sl := range page {
		if sl.info != nil {
			out = append(out, *sl.info)
		}
	}
	return out, nil
}

// GetInfoInStore reads the metadata of one session from a Store; it is the
// store-backed counterpart of GetInfo. directory selects the project key and
// defaults to the current directory.
//
// LastModified is the timestamp of the last entry (the current time when it
// has none) and FileSize the size of the transcript serialized as compact
// JSONL, as in the TypeScript SDK. It returns (nil, nil) when sessionID is
// not a UUID, the session has no entries, or it is a sidechain or has no
// extractable summary. Store errors are returned.
func GetInfoInStore(ctx context.Context, store Store, sessionID, directory string) (*Info, error) {
	if !ids.IsUUID(sessionID) {
		return nil, nil
	}
	projectPath := transcript.StoreProjectPath(directory)
	jsonl, size, ok, err := loadStoreEntriesAsJSONL(ctx, store, ProjectKey(directory), sessionID)
	if err != nil || !ok {
		return nil, err
	}
	info := parseSessionInfoFromLite(sessionID, jsonlToLite(jsonl, mtimeFromJSONLTail(jsonl)), projectPath, "")
	if info != nil {
		info.FileSize = size
	}
	return info, nil
}

// GetMessagesInStore reads a session's conversation from a Store; it is the
// store-backed counterpart of GetMessages, feeding the loaded entries
// straight into the chain builder.
//
// opts.Directory selects the project key and defaults to the current
// directory; opts.Offset and a positive opts.Limit page the result and
// opts.IncludeSystemMessages adds system messages. The transcript is never
// cut at compact boundaries (there is no size threshold for stores). opts
// may be nil. It returns nil when sessionID is not a UUID or the session
// has no visible messages. Store errors are returned.
func GetMessagesInStore(ctx context.Context, store Store, sessionID string, opts *MessagesOptions) ([]Message, error) {
	if opts == nil {
		opts = &MessagesOptions{}
	}
	if !ids.IsUUID(sessionID) {
		return nil, nil
	}
	key := Key{ProjectKey: ProjectKey(opts.Directory), SessionID: sessionID}
	entries, err := store.Load(ctx, key)
	if err != nil || len(entries) == 0 {
		return nil, err
	}
	return entriesToSessionMessages(entries, opts), nil
}

// ListSubagentsInStore lists the subagent ids of a session from a Store; it
// is the store-backed counterpart of ListSubagents. directory selects the
// project key and defaults to the current directory.
//
// The ids are taken from subkeys under "subagents/" whose last path
// component is agent-<id> (including nested ones such as
// subagents/workflows/<runId>/agent-<id>), deduplicated in order. It returns
// nil when sessionID is not a UUID or the session has no subagents. The
// store must implement SubkeyLister; otherwise an error wrapping
// errors.ErrUnsupported is returned. Store errors are returned.
func ListSubagentsInStore(ctx context.Context, store Store, sessionID, directory string) ([]string, error) {
	if !ids.IsUUID(sessionID) {
		return nil, nil
	}
	subkeyLister, ok := store.(SubkeyLister)
	if !ok {
		return nil, fmt.Errorf("sessions: session store does not implement ListSubkeys; "+
			"cannot list subagents: %w", errors.ErrUnsupported)
	}
	subkeys, err := subkeyLister.ListSubkeys(ctx, ListSubkeysKey{
		ProjectKey: ProjectKey(directory), SessionID: sessionID,
	})
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var ids []string
	for _, subpath := range subkeys {
		if agentID, ok := subagentIDFromSubkey(subpath); ok && !seen[agentID] {
			seen[agentID] = true
			ids = append(ids, agentID)
		}
	}
	return ids, nil
}

// subagentIDFromSubkey returns the agent id of a subagent transcript subkey:
// one under "subagents/" whose last path component is agent-<id>.
func subagentIDFromSubkey(subpath string) (string, bool) {
	if !strings.HasPrefix(subpath, "subagents/") {
		return "", false
	}
	return strings.CutPrefix(subpath[strings.LastIndexByte(subpath, '/')+1:], "agent-")
}

// GetSubagentMessagesInStore reads a subagent's conversation from a Store;
// it is the store-backed counterpart of GetSubagentMessages.
//
// The transcript is found among the session's subkeys when the store
// implements SubkeyLister (it may be nested, e.g.
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
func GetSubagentMessagesInStore(ctx context.Context, store Store, sessionID, agentID string, opts *MessagesOptions) ([]Message, error) {
	if opts == nil {
		opts = &MessagesOptions{}
	}
	if !ids.IsUUID(sessionID) || agentID == "" {
		return nil, nil
	}
	projectKey := ProjectKey(opts.Directory)

	subpath := "subagents/agent-" + agentID
	if subkeyLister, ok := store.(SubkeyLister); ok {
		subkeys, err := subkeyLister.ListSubkeys(ctx, ListSubkeysKey{ProjectKey: projectKey, SessionID: sessionID})
		if err != nil {
			return nil, err
		}
		i := slices.IndexFunc(subkeys, func(sk string) bool {
			id, ok := subagentIDFromSubkey(sk)
			return ok && id == agentID
		})
		if i < 0 {
			return nil, nil
		}
		subpath = subkeys[i]
	}

	entries, err := store.Load(ctx, Key{ProjectKey: projectKey, SessionID: sessionID, Subpath: subpath})
	if err != nil || len(entries) == 0 {
		return nil, err
	}
	// The agent_metadata entry is not a transcript line: take the parent
	// ids from it, then drop it.
	meta, lines := transcript.SplitAgentMetadata(entries)
	if len(lines) == 0 {
		return nil, nil
	}
	toolUseID, parentAgentID := parentIDsFromAgentMetadata(meta)
	return entriesToSubagentMessages(lines, opts.Limit, opts.Offset, toolUseID, parentAgentID), nil
}
