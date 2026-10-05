package sessions

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ironpark/gelati/claude/internal/ids"
	"github.com/ironpark/gelati/claude/internal/transcript"
)

// ---------------------------------------------------------------------------
// memStoreFake: a test-only in-memory Store implementing every
// optional interface. Wrap it in the store* adapters below to hide some.
// ---------------------------------------------------------------------------

type memStoreFake struct {
	mu        sync.Mutex
	clock     int64
	entries   map[Key][]Entry
	mtimes    map[Key]int64
	summaries map[Key]SummaryEntry // main keys only

	loadCalls []string
	// onLoad, when set, runs before every Load; an error fails the load.
	onLoad func(ctx context.Context, key Key) error
}

func newMemStoreFake() *memStoreFake {
	return &memStoreFake{
		clock:     1_700_000_000_000,
		entries:   map[Key][]Entry{},
		mtimes:    map[Key]int64{},
		summaries: map[Key]SummaryEntry{},
	}
}

func (s *memStoreFake) Append(_ context.Context, key Key, entries []Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clock++ // strictly monotonic storage clock
	s.entries[key] = append(s.entries[key], entries...)
	s.mtimes[key] = s.clock
	if key.Subpath == "" {
		var prev *SummaryEntry
		if p, ok := s.summaries[key]; ok {
			prev = &p
		}
		next := FoldSummary(prev, key, entries, nil)
		next.MTime = s.clock
		s.summaries[key] = next
	}
	return nil
}

func (s *memStoreFake) Load(ctx context.Context, key Key) ([]Entry, error) {
	s.mu.Lock()
	s.loadCalls = append(s.loadCalls, key.SessionID)
	hook := s.onLoad
	s.mu.Unlock()
	if hook != nil {
		if err := hook(ctx, key); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[key]
	if !ok {
		return nil, nil
	}
	return slices.Clone(e), nil
}

func (s *memStoreFake) ListSessions(_ context.Context, projectKey string) ([]ListEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []ListEntry
	for k, mt := range s.mtimes {
		if k.ProjectKey == projectKey && k.Subpath == "" {
			out = append(out, ListEntry{SessionID: k.SessionID, MTime: mt})
		}
	}
	slices.SortFunc(out, func(a, b ListEntry) int { return strings.Compare(a.SessionID, b.SessionID) })
	return out, nil
}

func (s *memStoreFake) ListSessionSummaries(_ context.Context, projectKey string) ([]SummaryEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []SummaryEntry
	for k, v := range s.summaries {
		if k.ProjectKey == projectKey {
			v.Data = maps.Clone(v.Data)
			out = append(out, v)
		}
	}
	return out, nil
}

func (s *memStoreFake) ListSubkeys(_ context.Context, key ListSubkeysKey) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for k := range s.entries {
		if k.ProjectKey == key.ProjectKey && k.SessionID == key.SessionID && k.Subpath != "" {
			out = append(out, k.Subpath)
		}
	}
	slices.Sort(out)
	return out, nil
}

func (s *memStoreFake) loads() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.loadCalls)
}

// storeMinimal exposes only Append and Load.
type storeMinimal struct{ Store }

// storeListOnly adds ListSessions (the slow listing path).
type storeListOnly struct {
	Store
	Lister
}

// storeSummariesOnly adds ListSessionSummaries without ListSessions.
type storeSummariesOnly struct {
	Store
	SummaryLister
}

// storeSummaries exposes summaries, listing and subkeys; its summaries can
// be filtered or replaced.
type storeSummaries struct {
	*memStoreFake
	summaries func(ctx context.Context, projectKey string) ([]SummaryEntry, error)
	listing   func(ctx context.Context, projectKey string) ([]ListEntry, error)
}

func (s storeSummaries) ListSessionSummaries(ctx context.Context, projectKey string) ([]SummaryEntry, error) {
	if s.summaries != nil {
		return s.summaries(ctx, projectKey)
	}
	return s.memStoreFake.ListSessionSummaries(ctx, projectKey)
}

func (s storeSummaries) ListSessions(ctx context.Context, projectKey string) ([]ListEntry, error) {
	if s.listing != nil {
		return s.listing(ctx, projectKey)
	}
	return s.memStoreFake.ListSessions(ctx, projectKey)
}

// storeUnsupportedSummaries forces the slow path through ErrUnsupported.
func storeUnsupportedSummaries(m *memStoreFake) Store {
	return storeSummaries{memStoreFake: m, summaries: func(context.Context, string) ([]SummaryEntry, error) {
		return nil, fmt.Errorf("not maintained: %w", errors.ErrUnsupported)
	}}
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

const storeTestDir = "/workspace/project"

var storeTestKey = ProjectKey(storeTestDir)

func storeUser(text, uid, parent, sid string) Entry {
	e := Entry{
		"type": "user", "uuid": uid, "parentUuid": nil, "sessionId": sid,
		"timestamp": "2024-01-01T00:00:00.000Z",
		"message":   map[string]any{"role": "user", "content": text},
	}
	if parent != "" {
		e["parentUuid"] = parent
	}
	return e
}

func storeAssistant(text, uid, parent, sid string) Entry {
	return Entry{
		"type": "assistant", "uuid": uid, "parentUuid": parent, "sessionId": sid,
		"timestamp": "2024-01-01T00:00:01.000Z",
		"message":   map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": text}}},
	}
}

// seedChain appends n user/assistant pairs and returns their uuids.
func seedChain(t *testing.T, store Store, sid string, n int) []string {
	t.Helper()
	var uuids []string
	var entries []Entry
	parent := ""
	for i := range n {
		u, a := ids.NewUUID(), ids.NewUUID()
		entries = append(entries, storeUser(fmt.Sprintf("prompt %d", i), u, parent, sid), storeAssistant(fmt.Sprintf("reply %d", i), a, u, sid))
		uuids = append(uuids, u, a)
		parent = a
	}
	appendEntries(t, store, Key{ProjectKey: storeTestKey, SessionID: sid}, entries...)
	return uuids
}

func appendEntries(t *testing.T, store Store, key Key, entries ...Entry) {
	t.Helper()
	if err := store.Append(context.Background(), key, entries); err != nil {
		t.Fatal(err)
	}
}

func mainKey(sid string) Key { return Key{ProjectKey: storeTestKey, SessionID: sid} }

func summaryUser(text any, ts string, extra ...any) Entry {
	e := Entry{"type": "user", "timestamp": ts, "message": map[string]any{"role": "user", "content": text}}
	for i := 0; i < len(extra); i += 2 {
		e[extra[i].(string)] = extra[i+1]
	}
	return e
}

// jsonlByteSize returns the byte size of entries as JSON.stringify lines, the
// FileSize the TypeScript SDK reports for store-backed sessions.
func jsonlByteSize(entries []Entry) int64 {
	return stringifySize(compactJSONL(entries))
}

// ---------------------------------------------------------------------------
// Serialization helpers
// ---------------------------------------------------------------------------

func TestSessionStoreEntriesToJSONL(t *testing.T) {
	t.Parallel()
	// Matches Python's _entries_to_jsonl except that non-type keys are
	// sorted (Go maps are unordered).
	got := entriesToJSONL([]Entry{
		{"b": "é😀<>&\u007f\u2028", "type": "user", "a": []any{1.0, "x"}},
		{"no": "type"},
	})
	want := `{"type":"user","a":[1,"x"],"b":"\u00e9\ud83d\ude00<>&\u007f\u2028"}` + "\n" + `{"no":"type"}` + "\n"
	if got != want {
		t.Errorf("entriesToJSONL =\n%s\nwant\n%s", got, want)
	}
	if got := entriesToJSONL(nil); got != "\n" {
		t.Errorf("empty = %q", got)
	}
}

func TestSessionStoreJSONLToLite(t *testing.T) {
	t.Parallel()
	small := jsonlToLite("abc\n", 7)
	if small.head != "abc\n" || small.tail != small.head || small.size != 4 || small.mtime != 7 {
		t.Errorf("small = %+v", small)
	}
	big := strings.Repeat("a", liteReadBufSize) + "é" + strings.Repeat("b", liteReadBufSize-2) + "Z"
	lite := jsonlToLite(big, 1)
	if len(lite.head) != liteReadBufSize || !strings.HasPrefix(lite.tail, "\ufffdb") || !strings.HasSuffix(lite.tail, "Z") {
		t.Errorf("big lite: head %d, tail %.8q", len(lite.head), lite.tail)
	}
}

func TestSessionStoreMTimeFromJSONLTail(t *testing.T) {
	t.Parallel()
	if got := mtimeFromJSONLTail(`{"a":1}` + "\n" + `{"timestamp":"2026-01-15T10:30:00.000Z"}` + "\n\n"); got != 1768473000000 {
		t.Errorf("got %d", got)
	}
	before := time.Now().UnixMilli()
	for _, s := range []string{`{"timestamp":"bogus"}`, "not json", `{"timestamp":5}`, ""} {
		if got := mtimeFromJSONLTail(s); got < before {
			t.Errorf("%q: got %d, want now", s, got)
		}
	}
}

// ---------------------------------------------------------------------------
// ListInStore
// ---------------------------------------------------------------------------

func TestSessionListFromStore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("seeded sessions sorted", func(t *testing.T) {
		t.Parallel()
		for name, store := range map[string]func(*memStoreFake) Store{
			"fast": func(m *memStoreFake) Store { return m },
			"slow": storeUnsupportedSummaries,
			"list": func(m *memStoreFake) Store { return storeListOnly{m, m} },
		} {
			m := newMemStoreFake()
			a, b := ids.NewUUID(), ids.NewUUID()
			seedChain(t, m, a, 2)
			seedChain(t, m, b, 2)
			got, err := ListInStore(ctx, store(m), &ListOptions{Directory: storeTestDir})
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if ids := sessionIDs(got); !slices.Equal(ids, []string{b, a}) {
				t.Errorf("%s: order %v, want newest first", name, ids)
			}
			for _, s := range got {
				// Summary-backed rows have no size; loaded ones report the
				// compact JSONL size.
				if s.Summary != "prompt 0" || s.FirstPrompt != "prompt 0" || (name == "fast") != (s.FileSize == 0) ||
					s.Cwd != transcript.CanonicalizePath(storeTestDir) || s.CreatedAt != 1704067200000 {
					t.Errorf("%s: %+v", name, s)
				}
			}
		}
	})

	t.Run("limit and offset", func(t *testing.T) {
		t.Parallel()
		m := newMemStoreFake()
		var sids []string
		for range 3 {
			sid := ids.NewUUID()
			seedChain(t, m, sid, 1)
			sids = append(sids, sid)
		}
		for _, store := range []Store{m, storeUnsupportedSummaries(m)} {
			page, err := ListInStore(ctx, store, &ListOptions{Directory: storeTestDir, Limit: 2, Offset: 1})
			if err != nil || !slices.Equal(sessionIDs(page), []string{sids[1], sids[0]}) {
				t.Errorf("page = %v, %v", sessionIDs(page), err)
			}
		}
	})

	t.Run("unsupported store", func(t *testing.T) {
		t.Parallel()
		_, err := ListInStore(ctx, storeMinimal{newMemStoreFake()}, &ListOptions{Directory: storeTestDir})
		if !errors.Is(err, errors.ErrUnsupported) || !strings.Contains(err.Error(), "ListSessions") {
			t.Errorf("err = %v", err)
		}
		_, err = ListInStore(ctx, storeUnsupportedSummaries(newMemStoreFake()), nil)
		if err != nil {
			t.Errorf("fallback with ListSessions: %v", err)
		}
	})

	t.Run("drops sidechains before paginating", func(t *testing.T) {
		t.Parallel()
		m := newMemStoreFake()
		var valid []string
		for range 5 {
			sid := ids.NewUUID()
			seedChain(t, m, sid, 1)
			valid = append(valid, sid)
		}
		for range 3 {
			sc := ids.NewUUID()
			e := storeUser("sidechain", ids.NewUUID(), "", sc)
			e["isSidechain"] = true
			appendEntries(t, m, mainKey(sc), e)
		}
		for name, store := range map[string]Store{"slow": storeUnsupportedSummaries(m), "fast": m} {
			page, err := ListInStore(ctx, store, &ListOptions{Directory: storeTestDir, Limit: 5})
			if err != nil || !slices.Equal(sortedCopy(sessionIDs(page)), sortedCopy(valid)) {
				t.Errorf("%s: page = %v, %v", name, sessionIDs(page), err)
			}
			page, _ = ListInStore(ctx, store, &ListOptions{Directory: storeTestDir, Limit: 5, Offset: 2})
			if len(page) != 3 {
				t.Errorf("%s: offset page = %v", name, sessionIDs(page))
			}
			for _, s := range page {
				if s.Summary == "" {
					t.Errorf("%s: empty summary row %+v", name, s)
				}
			}
		}
	})

	t.Run("does not mutate adapter listing", func(t *testing.T) {
		t.Parallel()
		internal := []ListEntry{{SessionID: "a", MTime: 1}, {SessionID: "b", MTime: 2}}
		store := storeSummaries{
			memStoreFake: newMemStoreFake(),
			summaries: func(context.Context, string) ([]SummaryEntry, error) {
				return nil, errors.ErrUnsupported
			},
			listing: func(context.Context, string) ([]ListEntry, error) { return internal, nil },
		}
		if _, err := ListInStore(ctx, store, &ListOptions{Directory: storeTestDir}); err != nil {
			t.Fatal(err)
		}
		if internal[0].SessionID != "a" || internal[1].SessionID != "b" {
			t.Errorf("listing mutated: %v", internal)
		}
	})

	t.Run("load error degrades row", func(t *testing.T) {
		t.Parallel()
		m := newMemStoreFake()
		good, bad := ids.NewUUID(), ids.NewUUID()
		seedChain(t, m, good, 1)
		seedChain(t, m, bad, 1)
		m.onLoad = func(_ context.Context, key Key) error {
			if key.SessionID == bad {
				return errors.New("backend down")
			}
			return nil
		}
		got, err := ListInStore(ctx, storeUnsupportedSummaries(m), &ListOptions{Directory: storeTestDir})
		if err != nil {
			t.Fatal(err)
		}
		byID := map[string]Info{}
		for _, s := range got {
			byID[s.SessionID] = s
		}
		if byID[good].Summary != "prompt 0" || byID[bad].Summary != "" || byID[bad].LastModified == 0 {
			t.Errorf("rows = %+v", got)
		}
	})

	t.Run("listing and summary errors propagate", func(t *testing.T) {
		t.Parallel()
		boom := errors.New("boom")
		store := storeSummaries{memStoreFake: newMemStoreFake(), summaries: func(context.Context, string) ([]SummaryEntry, error) {
			return nil, boom
		}}
		if _, err := ListInStore(ctx, store, nil); !errors.Is(err, boom) {
			t.Errorf("summary error = %v", err)
		}
		store = storeSummaries{memStoreFake: newMemStoreFake(), listing: func(context.Context, string) ([]ListEntry, error) {
			return nil, boom
		}}
		if _, err := ListInStore(ctx, store, nil); !errors.Is(err, boom) {
			t.Errorf("listing error = %v", err)
		}
	})

	t.Run("cwd falls back to directory", func(t *testing.T) {
		t.Parallel()
		m := newMemStoreFake()
		sid := ids.NewUUID()
		seedChain(t, m, sid, 1)
		canonical := transcript.CanonicalizePath(storeTestDir)
		info, err := GetInfoInStore(ctx, m, sid, storeTestDir)
		if err != nil || info == nil || info.Cwd != canonical {
			t.Errorf("info = %+v, %v", info, err)
		}
		listed, _ := ListInStore(ctx, m, &ListOptions{Directory: storeTestDir})
		if len(listed) != 1 || listed[0].Cwd != canonical {
			t.Errorf("listed = %+v", listed)
		}
	})
}

func TestSessionListFromStoreLoadConcurrency(t *testing.T) {
	t.Parallel()
	for name, wrap := range map[string]func(*memStoreFake) Store{
		"slow path": storeUnsupportedSummaries,
		"gap fill": func(m *memStoreFake) Store {
			return storeSummaries{memStoreFake: m, summaries: func(context.Context, string) ([]SummaryEntry, error) {
				return nil, nil // everything is missing a summary
			}}
		},
	} {
		m := newMemStoreFake()
		n := storeListLoadConcurrency * 3
		for i := range n {
			appendEntries(t, m, mainKey(ids.NewUUID()), summaryUser(fmt.Sprintf("p%d", i), "2024-01-01T00:00:00Z", "uuid", fmt.Sprintf("u%d", i)))
		}
		var inFlight, peak atomic.Int64
		gate := make(chan struct{})
		saturated := make(chan struct{}, n)
		m.onLoad = func(ctx context.Context, _ Key) error {
			cur := inFlight.Add(1)
			for {
				p := peak.Load()
				if cur <= p || peak.CompareAndSwap(p, cur) {
					break
				}
			}
			saturated <- struct{}{}
			<-gate
			inFlight.Add(-1)
			return nil
		}
		done := make(chan []Info)
		go func() {
			got, err := ListInStore(context.Background(), wrap(m), &ListOptions{Directory: storeTestDir})
			if err != nil {
				t.Error(err)
			}
			done <- got
		}()
		for range storeListLoadConcurrency {
			<-saturated
		}
		time.Sleep(20 * time.Millisecond) // give extra loads a chance to start
		if p := peak.Load(); p != storeListLoadConcurrency {
			t.Errorf("%s: peak at saturation = %d, want %d", name, p, storeListLoadConcurrency)
		}
		close(gate)
		if got := <-done; len(got) != n {
			t.Errorf("%s: got %d sessions, want %d", name, len(got), n)
		}
		if p := peak.Load(); p > storeListLoadConcurrency {
			t.Errorf("%s: peak = %d", name, p)
		}
	}
}

func TestSessionListFromStoreCancellation(t *testing.T) {
	t.Parallel()
	m := newMemStoreFake()
	for range storeListLoadConcurrency * 2 {
		seedChain(t, m, ids.NewUUID(), 1)
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.onLoad = func(ctx context.Context, _ Key) error {
		cancel()
		<-ctx.Done()
		return ctx.Err()
	}
	got, err := ListInStore(ctx, storeUnsupportedSummaries(m), &ListOptions{Directory: storeTestDir})
	if !errors.Is(err, context.Canceled) || got != nil {
		t.Errorf("got %v, %v", got, err)
	}
}

// ---------------------------------------------------------------------------
// ListInStore fast path (summaries)
// ---------------------------------------------------------------------------

func TestSessionListFromStoreFastPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts := &ListOptions{Directory: storeTestDir}

	t.Run("skips load", func(t *testing.T) {
		t.Parallel()
		m := newMemStoreFake()
		a, b := ids.NewUUID(), ids.NewUUID()
		appendEntries(t, m, mainKey(a), summaryUser("first a", "2024-01-01T00:00:00Z", "cwd", storeTestDir))
		appendEntries(t, m, mainKey(b), summaryUser("first b", "2024-01-02T00:00:00Z", "cwd", storeTestDir))
		got, err := ListInStore(ctx, m, opts)
		if err != nil || !slices.Equal(sessionIDs(got), []string{b, a}) {
			t.Fatalf("got %v, %v", sessionIDs(got), err)
		}
		if got[0].Summary != "first b" || got[1].FirstPrompt != "first a" || got[0].Cwd != storeTestDir {
			t.Errorf("got %+v", got)
		}
		if loads := m.loads(); loads != nil {
			t.Errorf("fast path loaded %v", loads)
		}
	})

	t.Run("filters sidechain and empty", func(t *testing.T) {
		t.Parallel()
		m := newMemStoreFake()
		main, side, empty := ids.NewUUID(), ids.NewUUID(), ids.NewUUID()
		appendEntries(t, m, mainKey(main), summaryUser("hello", "2024-01-01T00:00:00Z"))
		appendEntries(t, m, mainKey(side), summaryUser("x", "2024-01-01T00:00:00Z", "isSidechain", true))
		appendEntries(t, m, mainKey(empty), Entry{"type": "x", "timestamp": "2024-01-01T00:00:00Z"})
		got, _ := ListInStore(ctx, m, opts)
		if !slices.Equal(sessionIDs(got), []string{main}) {
			t.Errorf("got %v", sessionIDs(got))
		}
	})

	t.Run("limit and offset", func(t *testing.T) {
		t.Parallel()
		m := newMemStoreFake()
		var sids []string
		for i := range 5 {
			sid := ids.NewUUID()
			sids = append(sids, sid)
			appendEntries(t, m, mainKey(sid), summaryUser(fmt.Sprintf("p%d", i), fmt.Sprintf("2024-01-0%dT00:00:00Z", i+1)))
		}
		got, _ := ListInStore(ctx, m, &ListOptions{Directory: storeTestDir, Limit: 2, Offset: 1})
		if !slices.Equal(sessionIDs(got), []string{sids[3], sids[2]}) {
			t.Errorf("got %v", sessionIDs(got))
		}
	})

	t.Run("gap fill for missing summaries", func(t *testing.T) {
		t.Parallel()
		m := newMemStoreFake()
		with, without := ids.NewUUID(), ids.NewUUID()
		store := storeSummaries{memStoreFake: m, summaries: func(ctx context.Context, pk string) ([]SummaryEntry, error) {
			all, _ := m.ListSessionSummaries(ctx, pk)
			return slices.DeleteFunc(all, func(s SummaryEntry) bool { return s.SessionID != with }), nil
		}}
		appendEntries(t, m, mainKey(with), summaryUser("has sidecar", "2024-01-02T00:00:00Z"))
		appendEntries(t, m, mainKey(without), summaryUser("no sidecar", "2024-01-01T00:00:00Z"))
		got, err := ListInStore(ctx, store, opts)
		if err != nil || !slices.Equal(sessionIDs(got), []string{without, with}) {
			t.Fatalf("got %v, %v", sessionIDs(got), err)
		}
		if got[0].Summary != "no sidecar" || got[1].Summary != "has sidecar" {
			t.Errorf("got %+v", got)
		}
		if loads := m.loads(); !slices.Equal(loads, []string{without}) {
			t.Errorf("loads = %v", loads)
		}
	})

	t.Run("gap fill bounded by limit", func(t *testing.T) {
		t.Parallel()
		m := newMemStoreFake()
		with := ids.NewUUID()
		store := storeSummaries{memStoreFake: m, summaries: func(ctx context.Context, pk string) ([]SummaryEntry, error) {
			all, _ := m.ListSessionSummaries(ctx, pk)
			return slices.DeleteFunc(all, func(s SummaryEntry) bool { return s.SessionID != with }), nil
		}}
		for i := range 5 {
			appendEntries(t, m, mainKey(ids.NewUUID()), summaryUser(fmt.Sprintf("without %d", i), fmt.Sprintf("2024-01-0%dT00:00:00Z", i+1)))
		}
		appendEntries(t, m, mainKey(with), summaryUser("with", "2024-01-10T00:00:00Z"))
		page, _ := ListInStore(ctx, store, &ListOptions{Directory: storeTestDir, Limit: 2})
		if len(page) != 2 || page[0].SessionID != with {
			t.Errorf("page = %+v", page)
		}
		if loads := m.loads(); len(loads) != 1 {
			t.Errorf("loads = %v", loads)
		}
	})

	t.Run("sidechain summary does not consume page slot", func(t *testing.T) {
		t.Parallel()
		m := newMemStoreFake()
		sids := []string{ids.NewUUID(), ids.NewUUID(), ids.NewUUID()}
		appendEntries(t, m, mainKey(sids[2]), summaryUser("real 2", "2024-01-01T00:00:00Z"))
		appendEntries(t, m, mainKey(sids[1]), summaryUser("real 1", "2024-01-02T00:00:00Z"))
		appendEntries(t, m, mainKey(sids[0]), summaryUser("x", "2024-01-03T00:00:00Z", "isSidechain", true))
		page, _ := ListInStore(ctx, m, &ListOptions{Directory: storeTestDir, Limit: 2})
		if !slices.Equal(sessionIDs(page), []string{sids[1], sids[2]}) {
			t.Errorf("page = %v", sessionIDs(page))
		}
	})

	t.Run("stale summary triggers gap fill", func(t *testing.T) {
		t.Parallel()
		m := newMemStoreFake()
		sid := ids.NewUUID()
		const stale = 1_000
		store := storeSummaries{memStoreFake: m, summaries: func(context.Context, string) ([]SummaryEntry, error) {
			return []SummaryEntry{{SessionID: sid, MTime: stale, Data: map[string]any{
				"custom_title": "old", "first_prompt": "old prompt", "first_prompt_locked": true, "created_at": int64(stale),
			}}}, nil
		}}
		appendEntries(t, m, mainKey(sid),
			summaryUser("fresh prompt", "2024-01-02T00:00:00Z"),
			Entry{"type": "x", "timestamp": "2024-01-02T00:01:00Z", "customTitle": "fresh"},
		)
		got, err := ListInStore(ctx, store, opts)
		if err != nil || len(got) != 1 {
			t.Fatalf("got %+v, %v", got, err)
		}
		if got[0].CustomTitle != "fresh" || got[0].Summary != "fresh" || got[0].LastModified <= stale {
			t.Errorf("got %+v", got[0])
		}
		if loads := m.loads(); !slices.Equal(loads, []string{sid}) {
			t.Errorf("loads = %v", loads)
		}
	})

	t.Run("fresh summary with equal storage mtime is not gap filled", func(t *testing.T) {
		t.Parallel()
		m := newMemStoreFake()
		sid := ids.NewUUID()
		const t2 = 1_704_067_200_250
		store := storeSummaries{
			memStoreFake: m,
			listing: func(ctx context.Context, pk string) ([]ListEntry, error) {
				all, _ := m.ListSessions(ctx, pk)
				for i := range all {
					all[i].MTime = t2
				}
				return all, nil
			},
			summaries: func(ctx context.Context, pk string) ([]SummaryEntry, error) {
				all, _ := m.ListSessionSummaries(ctx, pk)
				for i := range all {
					all[i].MTime = t2
				}
				return all, nil
			},
		}
		appendEntries(t, m, mainKey(sid),
			summaryUser("fresh prompt", "2024-01-01T00:00:00.000Z"),
			Entry{"type": "x", "timestamp": "2024-01-01T00:00:00.000Z", "customTitle": "fresh"},
		)
		got, _ := ListInStore(ctx, store, opts)
		if len(got) != 1 || got[0].Summary != "fresh" || got[0].LastModified != t2 {
			t.Errorf("got %+v", got)
		}
		if loads := m.loads(); loads != nil {
			t.Errorf("loads = %v", loads)
		}
	})

	t.Run("summary without listing is dropped", func(t *testing.T) {
		t.Parallel()
		m := newMemStoreFake()
		real, ghost := ids.NewUUID(), ids.NewUUID()
		store := storeSummaries{memStoreFake: m, listing: func(ctx context.Context, pk string) ([]ListEntry, error) {
			all, _ := m.ListSessions(ctx, pk)
			return slices.DeleteFunc(all, func(e ListEntry) bool { return e.SessionID == ghost }), nil
		}}
		appendEntries(t, m, mainKey(real), summaryUser("real", "2024-01-02T00:00:00Z"))
		appendEntries(t, m, mainKey(ghost), summaryUser("ghost", "2024-01-01T00:00:00Z"))
		got, _ := ListInStore(ctx, store, opts)
		if !slices.Equal(sessionIDs(got), []string{real}) {
			t.Errorf("got %v", sessionIDs(got))
		}
	})

	t.Run("summaries without ListSessions", func(t *testing.T) {
		t.Parallel()
		m := newMemStoreFake()
		a := ids.NewUUID()
		appendEntries(t, m, mainKey(a), summaryUser("only summary", "2024-01-02T00:00:00Z"))
		got, err := ListInStore(ctx, storeSummariesOnly{m, m}, opts)
		if err != nil || !slices.Equal(sessionIDs(got), []string{a}) || got[0].Summary != "only summary" {
			t.Errorf("got %+v, %v", got, err)
		}
	})

	t.Run("gap-filled sidechain shorts the page", func(t *testing.T) {
		t.Parallel()
		m := newMemStoreFake()
		real, side := ids.NewUUID(), ids.NewUUID()
		store := storeSummaries{memStoreFake: m, summaries: func(context.Context, string) ([]SummaryEntry, error) {
			return nil, nil
		}}
		appendEntries(t, m, mainKey(real), summaryUser("real", "2024-01-01T00:00:00Z"))
		appendEntries(t, m, mainKey(side), summaryUser("x", "2024-01-02T00:00:00Z", "isSidechain", true))
		page, _ := ListInStore(ctx, store, &ListOptions{Directory: storeTestDir, Limit: 1})
		if page != nil {
			t.Errorf("page = %+v", page)
		}
	})
}

// ---------------------------------------------------------------------------
// Single-session store readers
// ---------------------------------------------------------------------------

func TestSessionGetSessionInfoFromStore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	m := newMemStoreFake()
	sid := ids.NewUUID()
	seedChain(t, m, sid, 1)

	info, err := GetInfoInStore(ctx, m, sid, storeTestDir)
	loaded, _ := m.Load(ctx, mainKey(sid))
	if err != nil || info == nil || info.SessionID != sid || info.Summary != "prompt 0" ||
		info.CreatedAt != 1704067200000 || info.LastModified != 1704067201000 || info.FileSize != jsonlByteSize(loaded) {
		t.Fatalf("info = %+v, %v", info, err)
	}
	for _, id := range []string{ids.NewUUID(), "not-a-uuid"} {
		if info, err := GetInfoInStore(ctx, m, id, storeTestDir); info != nil || err != nil {
			t.Errorf("%q: %+v, %v", id, info, err)
		}
	}

	appendEntries(t, m, mainKey(sid),
		Entry{"type": "custom-title", "customTitle": "My Title", "sessionId": sid},
		Entry{"type": "tag", "tag": "exp", "sessionId": sid},
	)
	info, _ = GetInfoInStore(ctx, m, sid, storeTestDir)
	if info == nil || info.CustomTitle != "My Title" || info.Summary != "My Title" || info.Tag != "exp" {
		t.Errorf("after rename/tag: %+v", info)
	}

	boom := errors.New("boom")
	m.onLoad = func(context.Context, Key) error { return boom }
	if _, err := GetInfoInStore(ctx, m, sid, storeTestDir); !errors.Is(err, boom) {
		t.Errorf("load error = %v", err)
	}
}

func TestSessionGetSessionMessagesFromStore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	m := newMemStoreFake()
	sid := ids.NewUUID()
	uuids := seedChain(t, m, sid, 3)
	opts := &MessagesOptions{Directory: storeTestDir}

	msgs, err := GetMessagesInStore(ctx, m, sid, opts)
	if err != nil || !slices.Equal(messageUUIDs(msgs), uuids) || msgs[0].Type != "user" || msgs[1].Type != "assistant" {
		t.Fatalf("msgs = %+v, %v", msgs, err)
	}
	for _, msg := range msgs {
		if msg.ParentToolUseID != "" || msg.ParentAgentID != "" {
			t.Errorf("parent ids set: %+v", msg)
		}
	}
	page, _ := GetMessagesInStore(ctx, m, sid, &MessagesOptions{Directory: storeTestDir, Limit: 2, Offset: 2})
	if !slices.Equal(messageUUIDs(page), uuids[2:4]) {
		t.Errorf("page = %v", messageUUIDs(page))
	}

	appendEntries(t, m, mainKey(sid),
		Entry{"type": "custom-title", "customTitle": "Title", "uuid": ids.NewUUID()},
		Entry{"type": "tag", "tag": "exp", "uuid": ids.NewUUID()},
	)
	if msgs, _ := GetMessagesInStore(ctx, m, sid, opts); len(msgs) != len(uuids) {
		t.Errorf("metadata entries leaked: %d", len(msgs))
	}
	for _, id := range []string{ids.NewUUID(), "not-a-uuid"} {
		if msgs, err := GetMessagesInStore(ctx, m, id, nil); msgs != nil || err != nil {
			t.Errorf("%q: %v, %v", id, msgs, err)
		}
	}
	boom := errors.New("boom")
	m.onLoad = func(context.Context, Key) error { return boom }
	if _, err := GetMessagesInStore(ctx, m, sid, opts); !errors.Is(err, boom) {
		t.Errorf("load error = %v", err)
	}
}

// ---------------------------------------------------------------------------
// Subagents in a store
// ---------------------------------------------------------------------------

func TestSessionSubagentsFromStore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sub := func(sid, subpath string) Key {
		return Key{ProjectKey: storeTestKey, SessionID: sid, Subpath: subpath}
	}

	t.Run("list and get", func(t *testing.T) {
		t.Parallel()
		m := newMemStoreFake()
		sid := ids.NewUUID()
		seedChain(t, m, sid, 1)
		u, a := ids.NewUUID(), ids.NewUUID()
		appendEntries(t, m, sub(sid, "subagents/agent-abc123"), storeUser("sub prompt", u, "", sid), storeAssistant("sub reply", a, u, sid))
		ids, err := ListSubagentsInStore(ctx, m, sid, storeTestDir)
		if err != nil || !slices.Equal(ids, []string{"abc123"}) {
			t.Errorf("ids = %v, %v", ids, err)
		}
		msgs, err := GetSubagentMessagesInStore(ctx, m, sid, "abc123", &MessagesOptions{Directory: storeTestDir})
		if err != nil || !slices.Equal(messageUUIDs(msgs), []string{u, a}) || msgs[0].Type != "user" || msgs[1].Type != "assistant" {
			t.Errorf("msgs = %+v, %v", msgs, err)
		}
		if msgs, _ := GetSubagentMessagesInStore(ctx, m, sid, "missing", &MessagesOptions{Directory: storeTestDir}); msgs != nil {
			t.Errorf("missing agent: %v", msgs)
		}
	})

	t.Run("nested workflow and dedupe", func(t *testing.T) {
		t.Parallel()
		m := newMemStoreFake()
		sid := ids.NewUUID()
		u := ids.NewUUID()
		appendEntries(t, m, sub(sid, "subagents/workflows/run-1/agent-nested"), storeUser("hi", u, "", sid))
		appendEntries(t, m, sub(sid, "subagents/agent-abc"), storeUser("x", u, "", sid))
		appendEntries(t, m, sub(sid, "subagents/workflows/run-1/agent-abc"), storeUser("x", u, "", sid))
		appendEntries(t, m, sub(sid, "other/agent-zzz"), storeUser("x", u, "", sid))
		appendEntries(t, m, sub(sid, "subagents/notes"), storeUser("x", u, "", sid))
		ids, _ := ListSubagentsInStore(ctx, m, sid, storeTestDir)
		if !slices.Equal(ids, []string{"abc", "nested"}) {
			t.Errorf("ids = %v", ids)
		}
		msgs, _ := GetSubagentMessagesInStore(ctx, m, sid, "nested", &MessagesOptions{Directory: storeTestDir})
		if len(msgs) != 1 {
			t.Errorf("nested msgs = %v", msgs)
		}
	})

	t.Run("agent metadata", func(t *testing.T) {
		t.Parallel()
		m := newMemStoreFake()
		sid := ids.NewUUID()
		u, a := ids.NewUUID(), ids.NewUUID()
		appendEntries(t, m, sub(sid, "subagents/agent-x"),
			Entry{"type": "agent_metadata", "agentType": "gp", "toolUseId": "toolu_old"},
			storeUser("hi", u, "", sid),
			storeAssistant("hello", a, u, sid),
			Entry{"type": "agent_metadata", "agentType": "gp", "toolUseId": "toolu_new", "parentAgentId": "a-parent"},
		)
		msgs, _ := GetSubagentMessagesInStore(ctx, m, sid, "x", &MessagesOptions{Directory: storeTestDir})
		if !slices.Equal(messageUUIDs(msgs), []string{u, a}) {
			t.Fatalf("msgs = %v", messageUUIDs(msgs))
		}
		for _, msg := range msgs {
			if msg.ParentToolUseID != "toolu_new" || msg.ParentAgentID != "a-parent" {
				t.Errorf("parent ids = %+v", msg)
			}
		}

		sid2 := ids.NewUUID()
		appendEntries(t, m, sub(sid2, "subagents/agent-x"),
			Entry{"type": "agent_metadata", "toolUseId": 7.0, "parentAgentId": nil},
			storeUser("hi", ids.NewUUID(), "", sid2),
		)
		msgs, _ = GetSubagentMessagesInStore(ctx, m, sid2, "x", &MessagesOptions{Directory: storeTestDir})
		if len(msgs) != 1 || msgs[0].ParentToolUseID != "" || msgs[0].ParentAgentID != "" {
			t.Errorf("non-string metadata = %+v", msgs)
		}

		sid3 := ids.NewUUID()
		appendEntries(t, m, sub(sid3, "subagents/agent-x"), Entry{"type": "agent_metadata", "toolUseId": "t"})
		if msgs, err := GetSubagentMessagesInStore(ctx, m, sid3, "x", &MessagesOptions{Directory: storeTestDir}); msgs != nil || err != nil {
			t.Errorf("metadata only = %v, %v", msgs, err)
		}
	})

	t.Run("limit and offset", func(t *testing.T) {
		t.Parallel()
		m := newMemStoreFake()
		sid := ids.NewUUID()
		var uuids []string
		var entries []Entry
		for i := range 4 {
			uid := ids.NewUUID()
			parent := ""
			if i > 0 {
				parent = uuids[i-1]
			}
			uuids = append(uuids, uid)
			entries = append(entries, storeUser(fmt.Sprint(i), uid, parent, sid))
		}
		appendEntries(t, m, sub(sid, "subagents/agent-p"), entries...)
		msgs, _ := GetSubagentMessagesInStore(ctx, m, sid, "p", &MessagesOptions{Directory: storeTestDir, Limit: 2, Offset: 1})
		if !slices.Equal(messageUUIDs(msgs), uuids[1:3]) {
			t.Errorf("page = %v", messageUUIDs(msgs))
		}
	})

	t.Run("invalid ids", func(t *testing.T) {
		t.Parallel()
		m := newMemStoreFake()
		if ids, err := ListSubagentsInStore(ctx, m, "not-a-uuid", storeTestDir); ids != nil || err != nil {
			t.Errorf("list = %v, %v", ids, err)
		}
		for _, tt := range []struct{ sid, agent string }{{"not-a-uuid", "x"}, {ids.NewUUID(), ""}} {
			if msgs, err := GetSubagentMessagesInStore(ctx, m, tt.sid, tt.agent, nil); msgs != nil || err != nil {
				t.Errorf("%q/%q = %v, %v", tt.sid, tt.agent, msgs, err)
			}
		}
	})

	t.Run("without ListSubkeys", func(t *testing.T) {
		t.Parallel()
		m := newMemStoreFake()
		sid := ids.NewUUID()
		_, err := ListSubagentsInStore(ctx, storeMinimal{m}, sid, storeTestDir)
		if !errors.Is(err, errors.ErrUnsupported) || !strings.Contains(err.Error(), "ListSubkeys") {
			t.Errorf("err = %v", err)
		}
		appendEntries(t, m, sub(sid, "subagents/agent-direct"), storeUser("hi", ids.NewUUID(), "", sid))
		appendEntries(t, m, sub(sid, "subagents/workflows/r/agent-nested"), storeUser("hi", ids.NewUUID(), "", sid))
		msgs, err := GetSubagentMessagesInStore(ctx, storeMinimal{m}, sid, "direct", &MessagesOptions{Directory: storeTestDir})
		if err != nil || len(msgs) != 1 {
			t.Errorf("direct path = %v, %v", msgs, err)
		}
		// Nested transcripts are only reachable through ListSubkeys.
		if msgs, _ := GetSubagentMessagesInStore(ctx, storeMinimal{m}, sid, "nested", &MessagesOptions{Directory: storeTestDir}); msgs != nil {
			t.Errorf("nested without ListSubkeys = %v", msgs)
		}
	})
}
