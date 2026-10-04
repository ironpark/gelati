package claude

import (
	"cmp"
	"context"
	"maps"
	"slices"
	"sync"
	"time"
)

// InMemorySessionStore is a SessionStore that keeps transcripts in memory,
// for tests and development; its data is lost when the process exits. It is
// the reference implementation of the SessionStore contract: it implements
// SessionLister, SessionSummaryLister, SessionDeleter and
// SessionSubkeyLister, and maintains session summaries with
// FoldSessionSummary inside Append. Ported from
// _internal/session_store.py.
//
// It is safe for concurrent use. The zero value is ready to use. Entries are
// copied one level deep on Append and Load; nested values are shared with
// the caller and must not be modified. The ctx arguments are ignored: no
// operation blocks.
type InMemorySessionStore struct {
	mu        sync.Mutex
	seq       int64 // creation counter: listings report keys in creation order
	lastMTime int64
	records   map[SessionKey]*inMemoryRecord
	summaries map[inMemorySessionRef]*inMemorySummary
}

type inMemoryRecord struct {
	seq     int64
	mtime   int64
	entries []SessionStoreEntry
}

type inMemorySessionRef struct{ projectKey, sessionID string }

type inMemorySummary struct {
	seq     int64
	summary SessionSummaryEntry
}

var (
	_ SessionStore         = (*InMemorySessionStore)(nil)
	_ SessionLister        = (*InMemorySessionStore)(nil)
	_ SessionSummaryLister = (*InMemorySessionStore)(nil)
	_ SessionDeleter       = (*InMemorySessionStore)(nil)
	_ SessionSubkeyLister  = (*InMemorySessionStore)(nil)
)

// NewInMemorySessionStore returns an empty InMemorySessionStore.
func NewInMemorySessionStore() *InMemorySessionStore {
	return &InMemorySessionStore{}
}

// nextMTime returns this store's storage write time in Unix epoch
// milliseconds. It is strictly increasing across calls, so back-to-back
// appends always get distinct mtimes, as real backends get from their
// commit ordering.
func (s *InMemorySessionStore) nextMTime() int64 {
	now := time.Now().UnixMilli()
	if now <= s.lastMTime {
		now = s.lastMTime + 1
	}
	s.lastMTime = now
	return now
}

// Append implements SessionStore. An empty batch still creates the key. For
// main transcript keys (empty Subpath) it also folds the batch into the
// session's summary, stamped with the same storage time ListSessions
// reports.
func (s *InMemorySessionStore) Append(_ context.Context, key SessionKey, entries []SessionStoreEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.records == nil {
		s.records = map[SessionKey]*inMemoryRecord{}
		s.summaries = map[inMemorySessionRef]*inMemorySummary{}
	}
	rec := s.records[key]
	if rec == nil {
		s.seq++
		rec = &inMemoryRecord{seq: s.seq, entries: []SessionStoreEntry{}}
		s.records[key] = rec
	}
	for _, e := range entries {
		rec.entries = append(rec.entries, maps.Clone(e))
	}
	now := s.nextMTime()
	// Subagent transcripts do not contribute to the main session's summary.
	if key.Subpath == "" {
		ref := inMemorySessionRef{key.ProjectKey, key.SessionID}
		cur := s.summaries[ref]
		var prev *SessionSummaryEntry
		if cur != nil {
			prev = &cur.summary
		} else {
			s.seq++
			cur = &inMemorySummary{seq: s.seq}
			s.summaries[ref] = cur
		}
		cur.summary = FoldSessionSummaryWithOptions(prev, key, entries, &FoldSessionOptions{MTime: now})
	}
	rec.mtime = now
	return nil
}

// Load implements SessionStore. It returns nil for a key that was never
// written, and a non-nil (possibly empty) slice otherwise.
func (s *InMemorySessionStore) Load(_ context.Context, key SessionKey) ([]SessionStoreEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.records[key]
	if rec == nil {
		return nil, nil
	}
	return cloneEntries(rec.entries), nil
}

// ListSessions implements SessionLister: the main transcripts of
// projectKey, in creation order, with their last Append time.
func (s *InMemorySessionStore) ListSessions(_ context.Context, projectKey string) ([]SessionStoreListEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []SessionStoreListEntry
	for _, k := range s.sortedKeysLocked() {
		if k.ProjectKey == projectKey && k.Subpath == "" {
			out = append(out, SessionStoreListEntry{SessionID: k.SessionID, MTime: s.records[k].mtime})
		}
	}
	return out, nil
}

// ListSessionSummaries implements SessionSummaryLister, in creation order.
func (s *InMemorySessionStore) ListSessionSummaries(_ context.Context, projectKey string) ([]SessionSummaryEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var picked []*inMemorySummary
	for ref, sum := range s.summaries {
		if ref.projectKey == projectKey {
			picked = append(picked, sum)
		}
	}
	slices.SortFunc(picked, func(a, b *inMemorySummary) int { return cmp.Compare(a.seq, b.seq) })
	var out []SessionSummaryEntry
	for _, sum := range picked {
		e := sum.summary
		e.Data = maps.Clone(e.Data)
		out = append(out, e)
	}
	return out, nil
}

// Delete implements SessionDeleter. Deleting a main transcript key also
// deletes the session's summary and every subkey (subagent transcripts), so
// they are not orphaned; a key with a Subpath removes only that entry.
// Deleting a missing key is not an error.
func (s *InMemorySessionStore) Delete(_ context.Context, key SessionKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.records, key)
	if key.Subpath == "" {
		delete(s.summaries, inMemorySessionRef{key.ProjectKey, key.SessionID})
		for k := range s.records {
			if k.ProjectKey == key.ProjectKey && k.SessionID == key.SessionID {
				delete(s.records, k)
			}
		}
	}
	return nil
}

// ListSubkeys implements SessionSubkeyLister: the subpaths stored under the
// session, in creation order.
func (s *InMemorySessionStore) ListSubkeys(_ context.Context, key SessionListSubkeysKey) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, k := range s.sortedKeysLocked() {
		if k.ProjectKey == key.ProjectKey && k.SessionID == key.SessionID && k.Subpath != "" {
			out = append(out, k.Subpath)
		}
	}
	return out, nil
}

// Entries returns a copy of the entries stored under key, or nil when there
// are none. It is a test helper (Python's get_entries).
func (s *InMemorySessionStore) Entries(key SessionKey) []SessionStoreEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.records[key]
	if rec == nil || len(rec.entries) == 0 {
		return nil
	}
	return cloneEntries(rec.entries)
}

// Len returns the number of stored sessions, counting main transcripts only.
// It is a test helper (Python's size).
func (s *InMemorySessionStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for k := range s.records {
		if k.Subpath == "" {
			n++
		}
	}
	return n
}

// Clear removes all stored data. It is a test helper.
func (s *InMemorySessionStore) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = nil
	s.summaries = nil
	s.seq = 0
	s.lastMTime = 0
}

// sortedKeysLocked returns the stored keys in creation order.
func (s *InMemorySessionStore) sortedKeysLocked() []SessionKey {
	keys := slices.Collect(maps.Keys(s.records))
	slices.SortFunc(keys, func(a, b SessionKey) int { return cmp.Compare(s.records[a].seq, s.records[b].seq) })
	return keys
}

// cloneEntries copies entries one level deep.
func cloneEntries(entries []SessionStoreEntry) []SessionStoreEntry {
	out := make([]SessionStoreEntry, len(entries))
	for i, e := range entries {
		out[i] = maps.Clone(e)
	}
	return out
}
