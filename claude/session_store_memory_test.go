package claude

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"testing"
)

// The SessionStore contract itself is checked by the sessionstoretest
// package, which runs its conformance suite against InMemorySessionStore.

func TestInMemorySessionStoreHelpers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	key := SessionKey{ProjectKey: "proj", SessionID: "sess"}

	t.Run("Entries returns a copy", func(t *testing.T) {
		t.Parallel()
		s := NewInMemorySessionStore()
		if got := s.Entries(key); got != nil {
			t.Errorf("empty = %v", got)
		}
		appendEntries(t, s, key, SessionStoreEntry{"n": 1}, SessionStoreEntry{"n": 2})
		got := s.Entries(key)
		if !reflect.DeepEqual(got, []SessionStoreEntry{{"n": 1}, {"n": 2}}) {
			t.Fatalf("entries = %v", got)
		}
		_ = append(got, SessionStoreEntry{"n": 999})
		got[0]["n"] = 999
		if again := s.Entries(key); !reflect.DeepEqual(again, []SessionStoreEntry{{"n": 1}, {"n": 2}}) {
			t.Errorf("after mutation = %v", again)
		}
	})

	t.Run("Len counts main transcripts only", func(t *testing.T) {
		t.Parallel()
		s := NewInMemorySessionStore()
		if s.Len() != 0 {
			t.Fatal(s.Len())
		}
		appendEntries(t, s, SessionKey{ProjectKey: "p", SessionID: "a"}, SessionStoreEntry{"n": 1})
		appendEntries(t, s, SessionKey{ProjectKey: "p", SessionID: "b"}, SessionStoreEntry{"n": 1})
		appendEntries(t, s, SessionKey{ProjectKey: "p", SessionID: "a", Subpath: "sub/x"}, SessionStoreEntry{"n": 1})
		if s.Len() != 2 {
			t.Errorf("Len = %d", s.Len())
		}
	})

	t.Run("Clear", func(t *testing.T) {
		t.Parallel()
		s := NewInMemorySessionStore()
		appendEntries(t, s, key, SessionStoreEntry{"n": 1})
		appendEntries(t, s, SessionKey{ProjectKey: "proj", SessionID: "sess", Subpath: "sub/x"}, SessionStoreEntry{"n": 1})
		s.Clear()
		if s.Len() != 0 {
			t.Errorf("Len = %d", s.Len())
		}
		if got, _ := s.Load(ctx, key); got != nil {
			t.Errorf("Load = %v", got)
		}
		if got, _ := s.ListSessions(ctx, "proj"); len(got) != 0 {
			t.Errorf("ListSessions = %v", got)
		}
		if got, _ := s.ListSessionSummaries(ctx, "proj"); len(got) != 0 {
			t.Errorf("ListSessionSummaries = %v", got)
		}
		appendEntries(t, s, key, SessionStoreEntry{"n": 2})
		if got := s.Entries(key); len(got) != 1 {
			t.Errorf("after reuse = %v", got)
		}
	})

	t.Run("Load returns a copy", func(t *testing.T) {
		t.Parallel()
		s := NewInMemorySessionStore()
		in := SessionStoreEntry{"n": 1}
		appendEntries(t, s, key, in)
		in["n"] = 5 // the caller's map is not stored
		loaded, _ := s.Load(ctx, key)
		loaded[0]["n"] = 999
		_ = append(loaded, SessionStoreEntry{"n": 999})
		if again, _ := s.Load(ctx, key); !reflect.DeepEqual(again, []SessionStoreEntry{{"n": 1}}) {
			t.Errorf("Load = %v", again)
		}
	})

	t.Run("empty append creates the key", func(t *testing.T) {
		t.Parallel()
		s := NewInMemorySessionStore()
		if got, _ := s.Load(ctx, key); got != nil {
			t.Errorf("never written = %#v", got)
		}
		appendEntries(t, s, key)
		got, _ := s.Load(ctx, key)
		if got == nil || len(got) != 0 {
			t.Errorf("after empty append = %#v", got)
		}
		if ls, _ := s.ListSessions(ctx, "proj"); len(ls) != 1 {
			t.Errorf("listing = %v", ls)
		}
	})

	t.Run("zero value is usable", func(t *testing.T) {
		t.Parallel()
		var s InMemorySessionStore
		if got, _ := s.Load(ctx, key); got != nil {
			t.Error(got)
		}
		if err := s.Delete(ctx, key); err != nil {
			t.Error(err)
		}
		appendEntries(t, &s, key, SessionStoreEntry{"n": 1})
		if s.Len() != 1 {
			t.Errorf("Len = %d", s.Len())
		}
	})
}

func TestInMemorySessionStoreBehavior(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("listings keep creation order and monotonic mtimes", func(t *testing.T) {
		t.Parallel()
		s := NewInMemorySessionStore()
		ids := []string{"c", "a", "b"}
		for _, id := range ids {
			appendEntries(t, s, SessionKey{ProjectKey: "p", SessionID: id}, SessionStoreEntry{"type": "x"})
		}
		appendEntries(t, s, SessionKey{ProjectKey: "p", SessionID: "a"}, SessionStoreEntry{"type": "x"})
		for _, sub := range []string{"subagents/agent-2", "subagents/agent-1"} {
			appendEntries(t, s, SessionKey{ProjectKey: "p", SessionID: "a", Subpath: sub}, SessionStoreEntry{"type": "x"})
		}
		listing, _ := s.ListSessions(ctx, "p")
		var got []string
		mtimes := map[string]int64{}
		for _, e := range listing {
			got = append(got, e.SessionID)
			mtimes[e.SessionID] = e.MTime
		}
		if !slices.Equal(got, ids) {
			t.Errorf("order = %v", got)
		}
		if !(mtimes["c"] < mtimes["b"] && mtimes["b"] < mtimes["a"]) {
			t.Errorf("mtimes = %v", mtimes)
		}
		summaries, _ := s.ListSessionSummaries(ctx, "p")
		got = got[:0]
		for _, e := range summaries {
			got = append(got, e.SessionID)
			if e.MTime != mtimes[e.SessionID] {
				t.Errorf("summary %s mtime %d, listing %d", e.SessionID, e.MTime, mtimes[e.SessionID])
			}
		}
		if !slices.Equal(got, ids) {
			t.Errorf("summary order = %v", got)
		}
		subs, _ := s.ListSubkeys(ctx, SessionListSubkeysKey{ProjectKey: "p", SessionID: "a"})
		if !slices.Equal(subs, []string{"subagents/agent-2", "subagents/agent-1"}) {
			t.Errorf("subkeys = %v", subs)
		}
	})

	t.Run("summaries are folded per batch", func(t *testing.T) {
		t.Parallel()
		s := NewInMemorySessionStore()
		key := SessionKey{ProjectKey: "p", SessionID: "s"}
		b1 := []SessionStoreEntry{summaryUser("first prompt", "2024-01-01T00:00:00.000Z", "cwd", "/w")}
		b2 := []SessionStoreEntry{{"type": "custom-title", "customTitle": "T"}, {"type": "tag", "tag": "x"}}
		appendEntries(t, s, key, b1...)
		appendEntries(t, s, key, b2...)
		appendEntries(t, s, SessionKey{ProjectKey: "p", SessionID: "s", Subpath: "subagents/agent-1"}, SessionStoreEntry{"type": "custom-title", "customTitle": "sub"})

		want := FoldSessionSummary(nil, key, b1)
		want = FoldSessionSummary(&want, key, b2)
		got, _ := s.ListSessionSummaries(ctx, "p")
		if len(got) != 1 || !reflect.DeepEqual(got[0].Data, want.Data) {
			t.Fatalf("summaries = %+v, want data %v", got, want.Data)
		}
		got[0].Data["customTitle"] = "mutated"
		if again, _ := s.ListSessionSummaries(ctx, "p"); again[0].Data["customTitle"] != "T" {
			t.Error("summary data aliased")
		}
		if err := s.Delete(ctx, SessionKey{ProjectKey: "p", SessionID: "s", Subpath: "subagents/agent-1"}); err != nil {
			t.Fatal(err)
		}
		if again, _ := s.ListSessionSummaries(ctx, "p"); len(again) != 1 {
			t.Error("subpath delete removed the summary")
		}
		if err := s.Delete(ctx, key); err != nil {
			t.Fatal(err)
		}
		if again, _ := s.ListSessionSummaries(ctx, "p"); len(again) != 0 {
			t.Errorf("after delete = %v", again)
		}
	})

	t.Run("concurrent use", func(t *testing.T) {
		t.Parallel()
		s := NewInMemorySessionStore()
		var wg sync.WaitGroup
		for g := range 8 {
			wg.Go(func() {
				key := SessionKey{ProjectKey: "p", SessionID: fmt.Sprint(g % 4)}
				for i := range 50 {
					_ = s.Append(ctx, key, []SessionStoreEntry{{"type": "user", "n": i}})
					_, _ = s.Load(ctx, key)
					_, _ = s.ListSessions(ctx, "p")
					_, _ = s.ListSessionSummaries(ctx, "p")
					_, _ = s.ListSubkeys(ctx, SessionListSubkeysKey{ProjectKey: "p", SessionID: key.SessionID})
				}
			})
		}
		wg.Wait()
		total := 0
		for i := range 4 {
			total += len(s.Entries(SessionKey{ProjectKey: "p", SessionID: fmt.Sprint(i)}))
		}
		if total != 400 || s.Len() != 4 {
			t.Errorf("total = %d, Len = %d", total, s.Len())
		}
	})
}

// The session readers against the real InMemorySessionStore (the read-side
// tests use a test-only fake).
func TestInMemorySessionStoreWithReaders(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("ListSessionsFromStore", func(t *testing.T) {
		t.Parallel()
		s := NewInMemorySessionStore()
		var sids []string
		for range 3 {
			sid := newUUID(t)
			seedChain(t, s, sid, 1)
			sids = append(sids, sid)
		}
		side := newUUID(t)
		appendEntries(t, s, mainKey(side), SessionStoreEntry{"type": "user", "isSidechain": true, "uuid": newUUID(t),
			"message": map[string]any{"role": "user", "content": "side"}})
		newestFirst := []string{sids[2], sids[1], sids[0]}

		infos, err := ListSessionsFromStore(ctx, s, &ListSessionsOptions{Directory: storeTestDir})
		if err != nil || !slices.Equal(sessionIDs(infos), newestFirst) {
			t.Fatalf("fast path = %v, %v", sessionIDs(infos), err)
		}
		if infos[0].Summary != "prompt 0" || infos[0].Cwd != canonicalizePath(storeTestDir) || infos[0].CreatedAt == 0 {
			t.Errorf("info = %+v", infos[0])
		}
		// The slow path (ListSessions + Load) agrees.
		slow, err := ListSessionsFromStore(ctx, storeListOnly{s, s}, &ListSessionsOptions{Directory: storeTestDir})
		for i := range slow {
			if slow[i].FileSize == 0 {
				t.Errorf("slow path FileSize = 0: %+v", slow[i])
			}
			slow[i].FileSize = 0 // summary-backed rows have none
		}
		if err != nil || !reflect.DeepEqual(slow, infos) {
			t.Errorf("slow path = %+v, %v", slow, err)
		}
		page, _ := ListSessionsFromStore(ctx, s, &ListSessionsOptions{Directory: storeTestDir, Limit: 1, Offset: 1})
		if !slices.Equal(sessionIDs(page), newestFirst[1:2]) {
			t.Errorf("page = %v", sessionIDs(page))
		}
	})

	t.Run("GetSessionInfoFromStore and messages", func(t *testing.T) {
		t.Parallel()
		s := NewInMemorySessionStore()
		sid := newUUID(t)
		uuids := seedChain(t, s, sid, 2)
		appendEntries(t, s, mainKey(sid), SessionStoreEntry{"type": "custom-title", "customTitle": "Titled", "sessionId": sid})
		info, err := GetSessionInfoFromStore(ctx, s, sid, storeTestDir)
		if err != nil || info == nil || info.CustomTitle != "Titled" || info.FirstPrompt != "prompt 0" {
			t.Errorf("info = %+v, %v", info, err)
		}
		if info, _ := GetSessionInfoFromStore(ctx, s, newUUID(t), storeTestDir); info != nil {
			t.Errorf("unknown = %+v", info)
		}
		msgs, err := GetSessionMessagesFromStore(ctx, s, sid, &SessionMessagesOptions{Directory: storeTestDir})
		if err != nil || !slices.Equal(messageUUIDs(msgs), uuids) {
			t.Errorf("messages = %v, %v", messageUUIDs(msgs), err)
		}
		msgs, _ = GetSessionMessagesFromStore(ctx, s, sid, &SessionMessagesOptions{Directory: storeTestDir, Limit: 2, Offset: 1})
		if !slices.Equal(messageUUIDs(msgs), uuids[1:3]) {
			t.Errorf("page = %v", messageUUIDs(msgs))
		}
	})

	t.Run("subagents", func(t *testing.T) {
		t.Parallel()
		s := NewInMemorySessionStore()
		sid := newUUID(t)
		seedChain(t, s, sid, 1)
		u, a := newUUID(t), newUUID(t)
		sub := SessionKey{ProjectKey: storeTestKey, SessionID: sid, Subpath: "subagents/workflows/r1/agent-abc"}
		appendEntries(t, s, sub, storeUser("sub prompt", u, "", sid), storeAssistant("sub reply", a, u, sid),
			SessionStoreEntry{"type": "agent_metadata", "toolUseId": "toolu_1", "parentAgentId": "p"})
		ids, err := ListSubagentsFromStore(ctx, s, sid, storeTestDir)
		if err != nil || !slices.Equal(ids, []string{"abc"}) {
			t.Errorf("ids = %v, %v", ids, err)
		}
		msgs, err := GetSubagentMessagesFromStore(ctx, s, sid, "abc", &SessionMessagesOptions{Directory: storeTestDir})
		if err != nil || !slices.Equal(messageUUIDs(msgs), []string{u, a}) || msgs[0].ParentToolUseID != "toolu_1" || msgs[0].ParentAgentID != "p" {
			t.Errorf("messages = %+v, %v", msgs, err)
		}
	})
}
