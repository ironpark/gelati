package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
)

// importEntry mirrors the Python tests' _entry.
func importEntry(i int) SessionStoreEntry {
	return SessionStoreEntry{"type": "user", "uuid": fmt.Sprintf("u%d", i), "timestamp": fmt.Sprintf("2026-01-01T00:00:%02dZ", i)}
}

func importEntries(from, to int) []SessionStoreEntry {
	var out []SessionStoreEntry
	for i := from; i < to; i++ {
		out = append(out, importEntry(i))
	}
	return out
}

func writeImportJSONL(t *testing.T, path string, entries ...SessionStoreEntry) {
	t.Helper()
	var lines []string
	for _, e := range entries {
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(b))
	}
	writeJSONL(t, path, lines...)
}

// importFixture creates a project and its transcript directory, keyed like
// the Python tests' claude_dir fixture.
func importFixture(t *testing.T) (root, cwd, projectKey, projectDir string) {
	t.Helper()
	root = newProjectsRoot(t)
	cwd, canonical := newProject(t, "project")
	projectKey = sanitizePath(canonical)
	projectDir = filepath.Join(root, projectKey)
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return root, cwd, projectKey, projectDir
}

const importSID = "550e8400-e29b-41d4-a716-446655440000"

// recordingStore records Append calls on top of an InMemorySessionStore.
type recordingStore struct {
	*InMemorySessionStore
	mu    sync.Mutex
	calls []recordedAppend
}

type recordedAppend struct {
	key     SessionKey
	entries []SessionStoreEntry
}

func (s *recordingStore) Append(ctx context.Context, key SessionKey, entries []SessionStoreEntry) error {
	s.mu.Lock()
	s.calls = append(s.calls, recordedAppend{key, slices.Clone(entries)})
	s.mu.Unlock()
	return s.InMemorySessionStore.Append(ctx, key, entries)
}

func TestSessionImportMainTranscript(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("imports main transcript", func(t *testing.T) {
		t.Parallel()
		root, cwd, pk, dir := importFixture(t)
		entries := importEntries(0, 7)
		writeImportJSONL(t, filepath.Join(dir, importSID+".jsonl"), entries...)
		store := NewInMemorySessionStore()
		if err := importSessionIn(ctx, root, importSID, store, &ImportSessionOptions{Directory: cwd}, noEnv); err != nil {
			t.Fatal(err)
		}
		if got := store.Entries(SessionKey{ProjectKey: pk, SessionID: importSID}); !reflect.DeepEqual(got, entries) {
			t.Errorf("entries = %v", got)
		}
	})

	t.Run("batching", func(t *testing.T) {
		t.Parallel()
		root, cwd, pk, dir := importFixture(t)
		entries := importEntries(0, 5)
		writeImportJSONL(t, filepath.Join(dir, importSID+".jsonl"), entries...)
		store := &recordingStore{InMemorySessionStore: NewInMemorySessionStore()}
		if err := importSessionIn(ctx, root, importSID, store, &ImportSessionOptions{Directory: cwd, BatchSize: 2}, noEnv); err != nil {
			t.Fatal(err)
		}
		key := SessionKey{ProjectKey: pk, SessionID: importSID}
		want := []recordedAppend{{key, entries[0:2]}, {key, entries[2:4]}, {key, entries[4:5]}}
		if !reflect.DeepEqual(store.calls, want) {
			t.Errorf("calls = %v", store.calls)
		}
		if got := store.Entries(key); !reflect.DeepEqual(got, entries) {
			t.Errorf("entries = %v", got)
		}
	})

	t.Run("skips blank lines and accepts CRLF", func(t *testing.T) {
		t.Parallel()
		root, cwd, pk, dir := importFixture(t)
		b0, _ := json.Marshal(importEntry(0))
		b1, _ := json.Marshal(importEntry(1))
		b2, _ := json.Marshal(importEntry(2))
		writeFile(t, filepath.Join(dir, importSID+".jsonl"), string(b0)+"\n\n"+string(b1)+"\r\n\r\n"+string(b2)) // no final newline
		store := NewInMemorySessionStore()
		if err := importSessionIn(ctx, root, importSID, store, &ImportSessionOptions{Directory: cwd}, noEnv); err != nil {
			t.Fatal(err)
		}
		if got := store.Entries(SessionKey{ProjectKey: pk, SessionID: importSID}); !reflect.DeepEqual(got, importEntries(0, 3)) {
			t.Errorf("entries = %v", got)
		}
	})

	t.Run("non-positive batch size uses the default", func(t *testing.T) {
		t.Parallel()
		root, cwd, _, dir := importFixture(t)
		writeImportJSONL(t, filepath.Join(dir, importSID+".jsonl"), importEntries(0, 3)...)
		for _, size := range []int{0, -1} {
			store := &recordingStore{InMemorySessionStore: NewInMemorySessionStore()}
			if err := importSessionIn(ctx, root, importSID, store, &ImportSessionOptions{Directory: cwd, BatchSize: size}, noEnv); err != nil {
				t.Fatal(err)
			}
			if len(store.calls) != 1 {
				t.Errorf("size %d: %d calls", size, len(store.calls))
			}
		}
	})

	t.Run("byte threshold", func(t *testing.T) {
		t.Parallel()
		root, cwd, _, dir := importFixture(t)
		big := strings.Repeat("x", 600*1024)
		var entries []SessionStoreEntry
		for i := range 3 {
			entries = append(entries, SessionStoreEntry{"type": "user", "uuid": fmt.Sprint(i), "pad": big})
		}
		writeImportJSONL(t, filepath.Join(dir, importSID+".jsonl"), entries...)
		store := &recordingStore{InMemorySessionStore: NewInMemorySessionStore()}
		if err := importSessionIn(ctx, root, importSID, store, &ImportSessionOptions{Directory: cwd}, noEnv); err != nil {
			t.Fatal(err)
		}
		if len(store.calls) != 2 || len(store.calls[0].entries) != 2 || len(store.calls[1].entries) != 1 {
			t.Errorf("calls = %d", len(store.calls))
		}
	})

	t.Run("unparseable lines are skipped", func(t *testing.T) {
		t.Parallel()
		for _, bad := range []string{"not json", "[1,2]", "42", "null", "   "} {
			root, cwd, pk, dir := importFixture(t)
			b0, _ := json.Marshal(importEntry(0))
			b1, _ := json.Marshal(importEntry(1))
			writeFile(t, filepath.Join(dir, importSID+".jsonl"), string(b0)+"\n"+bad+"\n"+string(b1)+"\n")
			store := NewInMemorySessionStore()
			if err := importSessionIn(ctx, root, importSID, store, &ImportSessionOptions{Directory: cwd}, noEnv); err != nil {
				t.Fatalf("%q: %v", bad, err)
			}
			if got := store.Entries(SessionKey{ProjectKey: pk, SessionID: importSID}); !reflect.DeepEqual(got, importEntries(0, 2)) {
				t.Errorf("%q: entries = %v", bad, got)
			}
		}
		// Invalid UTF-8 inside a string decodes to U+FFFD.
		root, cwd, pk, dir := importFixture(t)
		writeFile(t, filepath.Join(dir, importSID+".jsonl"), "{\"type\":\"user\",\"x\":\"\xff\"}\n")
		store := NewInMemorySessionStore()
		if err := importSessionIn(ctx, root, importSID, store, &ImportSessionOptions{Directory: cwd}, noEnv); err != nil {
			t.Fatal(err)
		}
		if got := store.Entries(SessionKey{ProjectKey: pk, SessionID: importSID}); len(got) != 1 || got[0]["x"] != "\ufffd" {
			t.Errorf("invalid UTF-8: %v", got)
		}
	})

	t.Run("store errors and cancellation", func(t *testing.T) {
		t.Parallel()
		root, cwd, _, dir := importFixture(t)
		writeImportJSONL(t, filepath.Join(dir, importSID+".jsonl"), importEntries(0, 2)...)
		boom := errors.New("boom")
		if err := importSessionIn(ctx, root, importSID, appendErrStore{boom}, &ImportSessionOptions{Directory: cwd}, noEnv); !errors.Is(err, boom) {
			t.Errorf("append error: %v", err)
		}
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		if err := importSessionIn(cctx, root, importSID, NewInMemorySessionStore(), &ImportSessionOptions{Directory: cwd}, noEnv); !errors.Is(err, context.Canceled) {
			t.Errorf("cancelled: %v", err)
		}
	})
}

type appendErrStore struct{ err error }

func (s appendErrStore) Append(context.Context, SessionKey, []SessionStoreEntry) error { return s.err }
func (s appendErrStore) Load(context.Context, SessionKey) ([]SessionStoreEntry, error) {
	return nil, nil
}

func TestSessionImportSubagents(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	subKey := func(pk, subpath string) SessionKey {
		return SessionKey{ProjectKey: pk, SessionID: importSID, Subpath: subpath}
	}

	t.Run("subpaths", func(t *testing.T) {
		t.Parallel()
		root, cwd, pk, dir := importFixture(t)
		writeImportJSONL(t, filepath.Join(dir, importSID+".jsonl"), importEntry(0))
		writeImportJSONL(t, filepath.Join(dir, importSID, "subagents", "agent-abc.jsonl"), importEntries(10, 12)...)
		writeImportJSONL(t, filepath.Join(dir, importSID, "subagents", "workflows", "run-1", "agent-def.jsonl"), importEntry(20))
		writeFile(t, filepath.Join(dir, importSID, "subagents", "notes.txt"), "ignored")
		store := NewInMemorySessionStore()
		if err := importSessionIn(ctx, root, importSID, store, &ImportSessionOptions{Directory: cwd}, noEnv); err != nil {
			t.Fatal(err)
		}
		if got := store.Entries(subKey(pk, "subagents/agent-abc")); !reflect.DeepEqual(got, importEntries(10, 12)) {
			t.Errorf("agent-abc = %v", got)
		}
		if got := store.Entries(subKey(pk, "subagents/workflows/run-1/agent-def")); !reflect.DeepEqual(got, importEntries(20, 21)) {
			t.Errorf("agent-def = %v", got)
		}
		subs, _ := store.ListSubkeys(ctx, SessionListSubkeysKey{ProjectKey: pk, SessionID: importSID})
		if !slices.Equal(subs, []string{"subagents/agent-abc", "subagents/workflows/run-1/agent-def"}) {
			t.Errorf("subkeys = %v", subs)
		}
		// Readable through the store readers.
		if ids, _ := ListSubagentsFromStore(ctx, store, importSID, cwd); !slices.Equal(ids, []string{"abc", "def"}) {
			t.Errorf("ListSubagentsFromStore = %v", ids)
		}
	})

	t.Run("meta sidecar", func(t *testing.T) {
		t.Parallel()
		root, cwd, pk, dir := importFixture(t)
		writeImportJSONL(t, filepath.Join(dir, importSID+".jsonl"), importEntry(0))
		sub := filepath.Join(dir, importSID, "subagents")
		writeImportJSONL(t, filepath.Join(sub, "agent-abc.jsonl"), importEntry(10))
		writeFile(t, filepath.Join(sub, "agent-abc.meta.json"), `{"agentType": "coder", "worktreePath": "/tmp/wt"}`)
		writeImportJSONL(t, filepath.Join(sub, "agent-shadow.jsonl"), importEntry(11))
		writeFile(t, filepath.Join(sub, "agent-shadow.meta.json"), `{"type": "something-else", "toolUseId": "toolu_1"}`)
		store := NewInMemorySessionStore()
		if err := importSessionIn(ctx, root, importSID, store, &ImportSessionOptions{Directory: cwd}, noEnv); err != nil {
			t.Fatal(err)
		}
		want := []SessionStoreEntry{importEntry(10), {"type": "agent_metadata", "agentType": "coder", "worktreePath": "/tmp/wt"}}
		if got := store.Entries(subKey(pk, "subagents/agent-abc")); !reflect.DeepEqual(got, want) {
			t.Errorf("agent-abc = %v", got)
		}
		want = []SessionStoreEntry{importEntry(11), {"type": "agent_metadata", "toolUseId": "toolu_1"}}
		if got := store.Entries(subKey(pk, "subagents/agent-shadow")); !reflect.DeepEqual(got, want) {
			t.Errorf("agent-shadow = %v", got)
		}
	})

	t.Run("unusable sidecar treated as absent", func(t *testing.T) {
		t.Parallel()
		for _, sidecar := range []string{"not json {", "[1, 2]", "42", "null"} {
			root, cwd, pk, dir := importFixture(t)
			writeImportJSONL(t, filepath.Join(dir, importSID+".jsonl"), importEntry(0))
			sub := filepath.Join(dir, importSID, "subagents")
			writeImportJSONL(t, filepath.Join(sub, "agent-abc.jsonl"), importEntry(10))
			writeFile(t, filepath.Join(sub, "agent-abc.meta.json"), sidecar)
			store := NewInMemorySessionStore()
			if err := importSessionIn(ctx, root, importSID, store, &ImportSessionOptions{Directory: cwd}, noEnv); err != nil {
				t.Fatalf("%q: %v", sidecar, err)
			}
			if got := store.Entries(subKey(pk, "subagents/agent-abc")); !reflect.DeepEqual(got, importEntries(10, 11)) {
				t.Errorf("%q: entries = %v", sidecar, got)
			}
		}
	})

	t.Run("unreadable sidecar is an error", func(t *testing.T) {
		t.Parallel()
		root, cwd, _, dir := importFixture(t)
		writeImportJSONL(t, filepath.Join(dir, importSID+".jsonl"), importEntry(0))
		sub := filepath.Join(dir, importSID, "subagents")
		writeImportJSONL(t, filepath.Join(sub, "agent-abc.jsonl"), importEntry(10))
		if err := os.MkdirAll(filepath.Join(sub, "agent-abc.meta.json"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := importSessionIn(ctx, root, importSID, NewInMemorySessionStore(), &ImportSessionOptions{Directory: cwd}, noEnv); err == nil {
			t.Error("directory sidecar accepted")
		}
	})

	t.Run("ExcludeSubagents and missing directory", func(t *testing.T) {
		t.Parallel()
		root, cwd, pk, dir := importFixture(t)
		writeImportJSONL(t, filepath.Join(dir, importSID+".jsonl"), importEntry(0))
		writeImportJSONL(t, filepath.Join(dir, importSID, "subagents", "agent-abc.jsonl"), importEntry(10))
		store := NewInMemorySessionStore()
		if err := importSessionIn(ctx, root, importSID, store, &ImportSessionOptions{Directory: cwd, ExcludeSubagents: true}, noEnv); err != nil {
			t.Fatal(err)
		}
		if subs, _ := store.ListSubkeys(ctx, SessionListSubkeysKey{ProjectKey: pk, SessionID: importSID}); len(subs) != 0 {
			t.Errorf("subkeys = %v", subs)
		}

		root2, cwd2, pk2, dir2 := importFixture(t)
		writeImportJSONL(t, filepath.Join(dir2, importSID+".jsonl"), importEntry(0))
		store = NewInMemorySessionStore()
		if err := importSessionIn(ctx, root2, importSID, store, &ImportSessionOptions{Directory: cwd2}, noEnv); err != nil {
			t.Fatal(err)
		}
		if got := store.Entries(SessionKey{ProjectKey: pk2, SessionID: importSID}); !reflect.DeepEqual(got, importEntries(0, 1)) {
			t.Errorf("entries = %v", got)
		}
	})
}

func TestSessionImportValidationAndKeys(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("validation", func(t *testing.T) {
		t.Parallel()
		root, cwd, _, _ := importFixture(t)
		if err := importSessionIn(ctx, root, "../../etc/passwd", NewInMemorySessionStore(), nil, noEnv); !errors.Is(err, ErrInvalidSessionID) {
			t.Errorf("invalid: %v", err)
		}
		if err := importSessionIn(ctx, root, importSID, NewInMemorySessionStore(), &ImportSessionOptions{Directory: cwd}, noEnv); !errors.Is(err, ErrSessionNotFound) {
			t.Errorf("missing: %v", err)
		}
	})

	t.Run("keys match filePathToSessionKey", func(t *testing.T) {
		t.Parallel()
		root, cwd, _, dir := importFixture(t)
		mainPath := filepath.Join(dir, importSID+".jsonl")
		subPath := filepath.Join(dir, importSID, "subagents", "agent-xyz.jsonl")
		writeImportJSONL(t, mainPath, importEntry(0))
		writeImportJSONL(t, subPath, importEntry(1))
		store := NewInMemorySessionStore()
		if err := importSessionIn(ctx, root, importSID, store, &ImportSessionOptions{Directory: cwd}, noEnv); err != nil {
			t.Fatal(err)
		}
		mainKey, ok1 := filePathToSessionKey(mainPath, root)
		subKey, ok2 := filePathToSessionKey(subPath, root)
		if !ok1 || !ok2 {
			t.Fatal("unmapped paths")
		}
		if got := store.Entries(mainKey); !reflect.DeepEqual(got, importEntries(0, 1)) {
			t.Errorf("main = %v", got)
		}
		if got := store.Entries(subKey); !reflect.DeepEqual(got, importEntries(1, 2)) {
			t.Errorf("sub = %v", got)
		}
	})

	t.Run("without directory the key is the current directory's", func(t *testing.T) {
		t.Parallel()
		root, _, pk, dir := importFixture(t)
		writeImportJSONL(t, filepath.Join(dir, importSID+".jsonl"), importEntry(0))
		cwdKey := projectKeyForDirectory("", noEnv)
		if cwdKey == pk {
			t.Fatal("precondition: cwd maps to the fixture project")
		}
		store := NewInMemorySessionStore()
		if err := importSessionIn(ctx, root, importSID, store, nil, noEnv); err != nil {
			t.Fatal(err)
		}
		if got := store.Entries(SessionKey{ProjectKey: cwdKey, SessionID: importSID}); !reflect.DeepEqual(got, importEntries(0, 1)) {
			t.Errorf("entries = %v", got)
		}
		// CLAUDE_CODE_PROJECT_DIR_NAME (with CLAUDE_CONFIG_DIR) names the key.
		env := func(k string) string {
			return map[string]string{"CLAUDE_CONFIG_DIR": "/cfg", "CLAUDE_CODE_PROJECT_DIR_NAME": "my-proj"}[k]
		}
		store = NewInMemorySessionStore()
		if err := importSessionIn(ctx, root, importSID, store, nil, env); err != nil {
			t.Fatal(err)
		}
		if got := store.Entries(SessionKey{ProjectKey: "my-proj", SessionID: importSID}); len(got) != 1 {
			t.Errorf("override entries = %v", got)
		}
	})

	t.Run("round trip into a resumable store session", func(t *testing.T) {
		t.Parallel()
		root, cwd, _, dir := importFixture(t)
		sid, _, uuids := makeTranscriptSession(t, dir, 2)
		store := NewInMemorySessionStore()
		if err := importSessionIn(ctx, root, sid, store, &ImportSessionOptions{Directory: cwd}, noEnv); err != nil {
			t.Fatal(err)
		}
		msgs, err := GetSessionMessagesFromStore(ctx, store, sid, &SessionMessagesOptions{Directory: cwd})
		if err != nil || !slices.Equal(messageUUIDs(msgs), uuids) {
			t.Errorf("messages = %v, %v", messageUUIDs(msgs), err)
		}
		local := getSessionMessagesIn(root, sid, &SessionMessagesOptions{Directory: cwd})
		if !reflect.DeepEqual(local, msgs) {
			t.Errorf("local %+v\nstore %+v", local, msgs)
		}
	})
}

// noEnv is an empty environment for the internal session helpers.
func noEnv(string) string { return "" }
