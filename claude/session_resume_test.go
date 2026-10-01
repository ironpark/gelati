package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const (
	resumeSID  = "550e8400-e29b-41d4-a716-446655440000"
	resumeSID2 = "660e8400-e29b-41d4-a716-446655440000"
	resumeSID3 = "770e8400-e29b-41d4-a716-446655440000"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

// resumeStoreFake is a minimal SessionStore (Append and Load only). Listing
// order is first-append order; mtimes default to the append sequence number.
// loadHook, when set, replaces Load.
type resumeStoreFake struct {
	loadHook func(ctx context.Context, key SessionKey) ([]SessionStoreEntry, error)

	mu     sync.Mutex
	data   map[SessionKey][]SessionStoreEntry
	order  []SessionKey
	mtimes map[SessionKey]int64
	loads  []SessionKey
	seq    int64
}

func (s *resumeStoreFake) Append(_ context.Context, key SessionKey, entries []SessionStoreEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data == nil {
		s.data = map[SessionKey][]SessionStoreEntry{}
		s.mtimes = map[SessionKey]int64{}
	}
	if _, ok := s.data[key]; !ok {
		s.order = append(s.order, key)
	}
	s.seq++
	s.data[key] = append(s.data[key], entries...)
	if s.data[key] == nil {
		s.data[key] = []SessionStoreEntry{}
	}
	s.mtimes[key] = s.seq
	return nil
}

func (s *resumeStoreFake) Load(ctx context.Context, key SessionKey) ([]SessionStoreEntry, error) {
	s.mu.Lock()
	s.loads = append(s.loads, key)
	hook := s.loadHook
	s.mu.Unlock()
	if hook != nil {
		return hook(ctx, key)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.data[key]), nil
}

func (s *resumeStoreFake) put(t *testing.T, key SessionKey, entries ...SessionStoreEntry) {
	t.Helper()
	if err := s.Append(t.Context(), key, entries); err != nil {
		t.Fatal(err)
	}
}

func (s *resumeStoreFake) setMTime(key SessionKey, mtime int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mtimes[key] = mtime
}

func (s *resumeStoreFake) loadCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.loads)
}

func (s *resumeStoreFake) entries(key SessionKey) []SessionStoreEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.data[key])
}

func (s *resumeStoreFake) listSessions(projectKey string) []SessionStoreListEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []SessionStoreListEntry
	for _, k := range s.order {
		if k.ProjectKey == projectKey && k.Subpath == "" {
			out = append(out, SessionStoreListEntry{SessionID: k.SessionID, MTime: s.mtimes[k]})
		}
	}
	return out
}

func (s *resumeStoreFake) listSubkeys(key SessionListSubkeysKey) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, k := range s.order {
		if k.ProjectKey == key.ProjectKey && k.SessionID == key.SessionID && k.Subpath != "" {
			out = append(out, k.Subpath)
		}
	}
	return out
}

// resumeListingStore adds SessionLister and SessionSubkeyLister. The hooks,
// when set, replace the defaults.
type resumeListingStore struct {
	*resumeStoreFake
	listHook    func(ctx context.Context, projectKey string) ([]SessionStoreListEntry, error)
	subkeysHook func(ctx context.Context, key SessionListSubkeysKey) ([]string, error)
}

func (s *resumeListingStore) ListSessions(ctx context.Context, projectKey string) ([]SessionStoreListEntry, error) {
	if s.listHook != nil {
		return s.listHook(ctx, projectKey)
	}
	return s.listSessions(projectKey), nil
}

func (s *resumeListingStore) ListSubkeys(ctx context.Context, key SessionListSubkeysKey) ([]string, error) {
	if s.subkeysHook != nil {
		return s.subkeysHook(ctx, key)
	}
	return s.listSubkeys(key), nil
}

// resumeListOnlyStore implements SessionLister but not SessionSubkeyLister.
type resumeListOnlyStore struct{ *resumeStoreFake }

func (s resumeListOnlyStore) ListSessions(_ context.Context, projectKey string) ([]SessionStoreListEntry, error) {
	return s.listSessions(projectKey), nil
}

func newResumeListingStore() *resumeListingStore {
	return &resumeListingStore{resumeStoreFake: &resumeStoreFake{}}
}

// resumeFixture is an isolated project directory, home directory and temp
// root, with the process environment, Keychain and real home stubbed out.
type resumeFixture struct {
	cwd        string
	projectKey string
	home       string
	tempRoot   string
	env        resumeEnv

	mu            sync.Mutex
	procEnv       map[string]string
	keychain      string
	keychainCalls int
}

func newResumeFixture(t *testing.T) *resumeFixture {
	t.Helper()
	root := t.TempDir()
	f := &resumeFixture{
		cwd:      filepath.Join(root, "project"),
		home:     filepath.Join(root, "home"),
		tempRoot: filepath.Join(root, "tmp"),
		procEnv:  map[string]string{},
	}
	for _, d := range []string{f.cwd, f.home, f.tempRoot} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f.projectKey = ProjectKeyForDirectory(f.cwd)
	f.env = resumeEnv{
		lookupEnv: func(key string) (string, bool) {
			f.mu.Lock()
			defer f.mu.Unlock()
			v, ok := f.procEnv[key]
			return v, ok
		},
		homeDir: func() (string, error) { return f.home, nil },
		readKeychain: func(context.Context) (string, bool) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.keychainCalls++
			return f.keychain, f.keychain != ""
		},
		tempDir:    f.tempRoot,
		retryDelay: time.Millisecond,
	}
	return f
}

func (f *resumeFixture) key(sid string) SessionKey {
	return SessionKey{ProjectKey: f.projectKey, SessionID: sid}
}

func (f *resumeFixture) materialize(t *testing.T, opts *Options) *materializedResume {
	t.Helper()
	if opts.Cwd == "" {
		opts.Cwd = f.cwd
	}
	m, err := materializeResumeSession(t.Context(), opts, f.env)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if m != nil {
		t.Cleanup(m.cleanup)
	}
	return m
}

// leftovers lists what is left in the temp root.
func (f *resumeFixture) leftovers(t *testing.T) []string {
	t.Helper()
	ents, err := os.ReadDir(f.tempRoot)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

func (f *resumeFixture) writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readJSONLFile(t *testing.T, path string) []map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func readJSONFile(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return m
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %o, want %o", path, got, want)
	}
}

func assertNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s exists (err=%v)", path, err)
	}
}

// ---------------------------------------------------------------------------
// No materialization
// ---------------------------------------------------------------------------

func TestMaterializeResumeNothingToDo(t *testing.T) {
	t.Parallel()
	f := newResumeFixture(t)
	empty := newResumeListingStore()
	emptyEntries := newResumeListingStore()
	emptyEntries.put(t, f.key(resumeSID))

	cases := map[string]*Options{
		"no store":                {Resume: resumeSID},
		"no resume or continue":   {SessionStore: empty},
		"non-uuid resume":         {SessionStore: empty, Resume: "../../etc/passwd"},
		"session missing":         {SessionStore: empty, Resume: resumeSID},
		"session empty":           {SessionStore: emptyEntries, Resume: resumeSID},
		"continue with no listed": {SessionStore: empty, ContinueConversation: true},
	}
	for name, opts := range cases {
		if m := f.materialize(t, opts); m != nil {
			t.Errorf("%s: materialized %+v", name, m)
		}
	}
	if left := f.leftovers(t); len(left) != 0 {
		t.Fatalf("temp dirs created: %v", left)
	}
	// A non-UUID resume never reaches the store.
	if n := empty.loadCount(); n != 1 {
		t.Fatalf("loads on the empty store = %d, want 1 (the missing session)", n)
	}
}

// ---------------------------------------------------------------------------
// Happy paths
// ---------------------------------------------------------------------------

func TestMaterializeResumeWritesTranscriptAndCleanupRemovesDir(t *testing.T) {
	t.Parallel()
	f := newResumeFixture(t)
	store := newResumeListingStore()
	entries := []SessionStoreEntry{
		{"type": "user", "uuid": "u1", "message": map[string]any{"role": "user", "content": "hi <b>&"}},
		{"type": "assistant", "uuid": "a1"},
	}
	store.put(t, f.key(resumeSID), entries...)

	m := f.materialize(t, &Options{SessionStore: store, Resume: resumeSID})
	if m == nil {
		t.Fatal("not materialized")
	}
	if m.resumeSessionID != resumeSID {
		t.Fatalf("session = %q", m.resumeSessionID)
	}
	if filepath.Dir(m.configDir) != f.tempRoot || !strings.HasPrefix(filepath.Base(m.configDir), "claude-resume-") {
		t.Fatalf("config dir = %q", m.configDir)
	}
	assertMode(t, m.configDir, 0o700)

	jsonl := filepath.Join(m.configDir, "projects", f.projectKey, resumeSID+".jsonl")
	assertMode(t, jsonl, 0o600)
	got := readJSONLFile(t, jsonl)
	if !reflect.DeepEqual(got, entries) {
		t.Fatalf("transcript = %v", got)
	}
	raw, _ := os.ReadFile(jsonl)
	if !strings.HasPrefix(string(raw), `{"type":"user",`) || !strings.Contains(string(raw), "<b>&") {
		t.Fatalf("raw transcript = %s", raw)
	}

	m.cleanup()
	assertNotExist(t, m.configDir)
	m.cleanup() // idempotent
	(*materializedResume)(nil).cleanup()
}

func TestMaterializeResumeCredentialsRedacted(t *testing.T) {
	t.Parallel()
	f := newResumeFixture(t)
	f.writeFile(t, filepath.Join(f.home, ".claude", ".credentials.json"),
		`{"claudeAiOauth":{"accessToken":"at","refreshToken":"SECRET","expiresAt":17000000000000000001},"other":1}`)
	f.writeFile(t, filepath.Join(f.home, ".claude.json"), `{"theme":"dark"}`)
	store := newResumeListingStore()
	store.put(t, f.key(resumeSID), SessionStoreEntry{"type": "user", "uuid": "u1"})

	m := f.materialize(t, &Options{SessionStore: store, Resume: resumeSID})
	credsPath := filepath.Join(m.configDir, ".credentials.json")
	assertMode(t, credsPath, 0o600)
	raw, _ := os.ReadFile(credsPath)
	if strings.Contains(string(raw), "SECRET") || !strings.Contains(string(raw), `"accessToken":"at"`) {
		t.Fatalf("credentials = %s", raw)
	}
	// Numbers survive unchanged.
	if !strings.Contains(string(raw), "17000000000000000001") {
		t.Fatalf("credentials lost precision: %s", raw)
	}
	// .claude.json is copied verbatim from ~ (not ~/.claude/).
	if b, _ := os.ReadFile(filepath.Join(m.configDir, ".claude.json")); string(b) != `{"theme":"dark"}` {
		t.Fatalf(".claude.json = %s", b)
	}
	assertMode(t, filepath.Join(m.configDir, ".claude.json"), 0o600)
	// Without a refresh token in the source, the file is copied as is.
	f.writeFile(t, filepath.Join(f.home, ".claude", ".credentials.json"), `{"claudeAiOauth": {"accessToken": "x"}}`)
	m2 := f.materialize(t, &Options{SessionStore: store, Resume: resumeSID})
	if b, _ := os.ReadFile(filepath.Join(m2.configDir, ".credentials.json")); string(b) != `{"claudeAiOauth": {"accessToken": "x"}}` {
		t.Fatalf("credentials = %s", b)
	}
}

func TestMaterializeResumeCallerConfigDir(t *testing.T) {
	t.Parallel()
	for _, via := range []string{"options env", "process env"} {
		t.Run(via, func(t *testing.T) {
			t.Parallel()
			f := newResumeFixture(t)
			custom := filepath.Join(f.home, "custom-config")
			f.writeFile(t, filepath.Join(custom, ".credentials.json"), `{"claudeAiOauth":{"accessToken":"fromenv"}}`)
			f.writeFile(t, filepath.Join(custom, ".claude.json"), `{"from":"custom"}`)
			f.writeFile(t, filepath.Join(custom, "settings.json"), `{"apiKeyHelper":"/from/env"}`)
			// The ~ files must not win over CLAUDE_CONFIG_DIR.
			f.writeFile(t, filepath.Join(f.home, ".claude", "settings.json"), `{"x":1}`)
			f.writeFile(t, filepath.Join(f.home, ".claude", ".credentials.json"), `{"claudeAiOauth":{"accessToken":"home"}}`)
			f.writeFile(t, filepath.Join(f.home, ".claude.json"), `{"from":"home"}`)
			f.keychain = `{"claudeAiOauth":{"accessToken":"kc"}}`
			store := newResumeListingStore()
			store.put(t, f.key(resumeSID), SessionStoreEntry{"type": "user"})
			opts := &Options{SessionStore: store, Resume: resumeSID}
			if via == "options env" {
				opts.Env = map[string]string{"CLAUDE_CONFIG_DIR": custom}
			} else {
				f.procEnv["CLAUDE_CONFIG_DIR"] = custom
			}

			m := f.materialize(t, opts)
			creds := readJSONFile(t, filepath.Join(m.configDir, ".credentials.json"))
			if creds["claudeAiOauth"].(map[string]any)["accessToken"] != "fromenv" {
				t.Fatalf("credentials = %v", creds)
			}
			if got := readJSONFile(t, filepath.Join(m.configDir, ".claude.json")); got["from"] != "custom" {
				t.Fatalf(".claude.json = %v", got)
			}
			if got := readJSONFile(t, filepath.Join(m.configDir, "settings.json")); !reflect.DeepEqual(got, map[string]any{"apiKeyHelper": "/from/env"}) {
				t.Fatalf("settings = %v", got)
			}
			if f.keychainCalls != 0 {
				t.Fatal("the Keychain must not be read with a custom config dir")
			}
		})
	}
}

func TestMaterializeResumeKeychainFallback(t *testing.T) {
	t.Parallel()
	f := newResumeFixture(t)
	f.keychain = `{"claudeAiOauth":{"accessToken":"kc","refreshToken":"SECRET"}}`
	// The Keychain wins over a credentials file.
	f.writeFile(t, filepath.Join(f.home, ".claude", ".credentials.json"), `{"claudeAiOauth":{"accessToken":"file"}}`)
	store := newResumeListingStore()
	store.put(t, f.key(resumeSID), SessionStoreEntry{"type": "user"})

	m := f.materialize(t, &Options{SessionStore: store, Resume: resumeSID})
	oauth := readJSONFile(t, filepath.Join(m.configDir, ".credentials.json"))["claudeAiOauth"].(map[string]any)
	if oauth["accessToken"] != "kc" || oauth["refreshToken"] != nil {
		t.Fatalf("credentials = %v", oauth)
	}

	// Env-based auth skips the Keychain, from Options.Env or the process.
	for _, tc := range []struct {
		optEnv  map[string]string
		procEnv map[string]string
	}{
		{optEnv: map[string]string{"ANTHROPIC_API_KEY": "k"}},
		{optEnv: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "t"}},
		{procEnv: map[string]string{"ANTHROPIC_API_KEY": "k"}},
		{procEnv: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "t"}},
	} {
		f.mu.Lock()
		f.keychainCalls = 0
		f.procEnv = maps.Clone(tc.procEnv)
		if f.procEnv == nil {
			f.procEnv = map[string]string{}
		}
		f.mu.Unlock()
		m := f.materialize(t, &Options{SessionStore: store, Resume: resumeSID, Env: tc.optEnv})
		if f.keychainCalls != 0 {
			t.Fatalf("%+v: Keychain read", tc)
		}
		oauth := readJSONFile(t, filepath.Join(m.configDir, ".credentials.json"))["claudeAiOauth"].(map[string]any)
		if oauth["accessToken"] != "file" {
			t.Fatalf("%+v: credentials = %v", tc, oauth)
		}
	}

	// An empty value does not count as env-based auth.
	f.mu.Lock()
	f.keychainCalls = 0
	f.procEnv = map[string]string{"ANTHROPIC_API_KEY": ""}
	f.mu.Unlock()
	f.materialize(t, &Options{SessionStore: store, Resume: resumeSID, Env: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": ""}})
	if f.keychainCalls != 1 {
		t.Fatalf("Keychain calls = %d, want 1", f.keychainCalls)
	}
}

func TestMaterializeResumeUserSettings(t *testing.T) {
	t.Parallel()
	f := newResumeFixture(t)
	store := newResumeListingStore()
	store.put(t, f.key(resumeSID), SessionStoreEntry{"type": "user"})
	cfg := filepath.Join(f.home, ".claude")
	materialize := func() *materializedResume {
		return f.materialize(t, &Options{SessionStore: store, Resume: resumeSID})
	}
	readBack := func(m *materializedResume, name string) string {
		b, err := os.ReadFile(filepath.Join(m.configDir, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return string(b)
	}

	// Absent: nothing written.
	m := materialize()
	for _, name := range []string{"settings.json", "cowork_settings.json", ".claude.json", ".credentials.json"} {
		assertNotExist(t, filepath.Join(m.configDir, name))
	}

	// Nothing to strip: bytes copied through, 0600.
	settings := `{"apiKeyHelper": "/bin/print-key", "env": {"FOO": "bar"}}`
	f.writeFile(t, filepath.Join(cfg, "settings.json"), settings)
	f.writeFile(t, filepath.Join(cfg, "cowork_settings.json"), settings)
	m = materialize()
	for _, name := range []string{"settings.json", "cowork_settings.json"} {
		if got := readBack(m, name); got != settings {
			t.Fatalf("%s = %s", name, got)
		}
		assertMode(t, filepath.Join(m.configDir, name), 0o600)
	}

	// Plugin declarations and env.CLAUDE_CONFIG_DIR are stripped; a BOM is
	// tolerated.
	original := `{"apiKeyHelper":"/bin/print-key","enabledPlugins":{"p@m":true},` +
		`"extraKnownMarketplaces":{"m":{"source":"github","repo":"o/r"}},` +
		`"env":{"CLAUDE_CONFIG_DIR":"/elsewhere","KEEP":"1"},"permissions":{"allow":["Bash(ls)"]},"big":12345678901234567890123}`
	f.writeFile(t, filepath.Join(cfg, "settings.json"), "\xef\xbb\xbf"+original)
	f.writeFile(t, filepath.Join(cfg, "cowork_settings.json"), original)
	m = materialize()
	for _, name := range []string{"settings.json", "cowork_settings.json"} {
		got := readBack(m, name)
		if strings.HasPrefix(got, "\xef\xbb\xbf") || !strings.Contains(got, "12345678901234567890123") {
			t.Fatalf("%s = %s", name, got)
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(got), &parsed); err != nil {
			t.Fatal(err)
		}
		delete(parsed, "big")
		want := map[string]any{
			"apiKeyHelper": "/bin/print-key",
			"env":          map[string]any{"KEEP": "1"},
			"permissions":  map[string]any{"allow": []any{"Bash(ls)"}},
		}
		if !reflect.DeepEqual(parsed, want) {
			t.Fatalf("%s = %v", name, parsed)
		}
	}

	// Malformed, non-object, or non-object env: byte-for-byte.
	f.writeFile(t, filepath.Join(cfg, "settings.json"), "{not json")
	f.writeFile(t, filepath.Join(cfg, "cowork_settings.json"), `{"env": "nope", "a": 1}`)
	f.writeFile(t, filepath.Join(f.home, ".claude.json"), "[1, 2]")
	m = materialize()
	if readBack(m, "settings.json") != "{not json" || readBack(m, "cowork_settings.json") != `{"env": "nope", "a": 1}` ||
		readBack(m, ".claude.json") != "[1, 2]" {
		t.Fatal("malformed settings were rewritten")
	}

	// An overflowing float falls back to the original bytes.
	for _, raw := range []string{
		`{"enabledPlugins": {"p@m": true}, "threshold": 1e999}`,
		`{"enabledPlugins": {}} trailing`,
		`["enabledPlugins"]`,
		"\xff{\"enabledPlugins\": {}}",
	} {
		f.writeFile(t, filepath.Join(cfg, "settings.json"), raw)
		if got := readBack(materialize(), "settings.json"); got != raw {
			t.Fatalf("settings = %q, want %q", got, raw)
		}
	}
}

func TestStripSettingsForResume(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		`{"a":1}`:                                       `{"a":1}`,
		`{"enabledPlugins":{},"a":1.50}`:                `{"a":1.50}`,
		`{"env":{"CLAUDE_CONFIG_DIR":"x"}}`:             `{"env":{}}`,
		`{"enabledPlugins":{},"tiny":1e-999,"s":"<é>"}`: `{"s":"<é>","tiny":1e-999}`,
		`null`: `null`,
	}
	for in, want := range cases {
		if got := string(stripSettingsForResume([]byte(in))); got != want {
			t.Errorf("strip(%s) = %s, want %s", in, got, want)
		}
	}
}

func TestMaterializeResumeUnreadableSeedFilesSkipped(t *testing.T) {
	t.Parallel()
	f := newResumeFixture(t)
	// Directories where files are expected.
	for _, p := range []string{
		filepath.Join(f.home, ".claude", "settings.json"),
		filepath.Join(f.home, ".claude", ".credentials.json"),
		filepath.Join(f.home, ".claude.json"),
	} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	store := newResumeListingStore()
	store.put(t, f.key(resumeSID), SessionStoreEntry{"type": "user"})
	m := f.materialize(t, &Options{SessionStore: store, Resume: resumeSID})
	for _, name := range []string{"settings.json", ".credentials.json", ".claude.json"} {
		assertNotExist(t, filepath.Join(m.configDir, name))
	}
	if _, err := os.Stat(filepath.Join(m.configDir, "projects", f.projectKey, resumeSID+".jsonl")); err != nil {
		t.Fatal(err)
	}
}

func TestMaterializeResumeHomeUnavailable(t *testing.T) {
	t.Parallel()
	f := newResumeFixture(t)
	f.env.homeDir = func() (string, error) { return "", errors.New("no home") }
	store := newResumeListingStore()
	store.put(t, f.key(resumeSID), SessionStoreEntry{"type": "user"})
	if m := f.materialize(t, &Options{SessionStore: store, Resume: resumeSID}); m == nil {
		t.Fatal("not materialized")
	}
}

// ---------------------------------------------------------------------------
// ContinueConversation
// ---------------------------------------------------------------------------

func TestMaterializeResumeContinue(t *testing.T) {
	t.Parallel()

	t.Run("picks most recent", func(t *testing.T) {
		t.Parallel()
		f := newResumeFixture(t)
		store := newResumeListingStore()
		store.put(t, f.key(resumeSID2), SessionStoreEntry{"type": "user", "uuid": "new"})
		store.put(t, f.key(resumeSID), SessionStoreEntry{"type": "user", "uuid": "old"})
		store.setMTime(f.key(resumeSID2), 2000)
		store.setMTime(f.key(resumeSID), 1000)
		m := f.materialize(t, &Options{SessionStore: store, ContinueConversation: true})
		if m == nil || m.resumeSessionID != resumeSID2 {
			t.Fatalf("materialized = %+v", m)
		}
	})

	t.Run("skips sidechains and invalid ids", func(t *testing.T) {
		t.Parallel()
		f := newResumeFixture(t)
		store := newResumeListingStore()
		store.put(t, f.key(resumeSID), SessionStoreEntry{"type": "user", "uuid": "main"})
		store.put(t, f.key(resumeSID2), SessionStoreEntry{"type": "user", "uuid": "sc", "isSidechain": true})
		store.put(t, f.key("not-a-uuid"), SessionStoreEntry{"type": "user"})
		store.put(t, f.key(resumeSID3)) // empty
		store.setMTime(f.key(resumeSID), 1000)
		store.setMTime(f.key(resumeSID2), 2000)
		store.setMTime(f.key("not-a-uuid"), 3000)
		store.setMTime(f.key(resumeSID3), 4000)
		m := f.materialize(t, &Options{SessionStore: store, ContinueConversation: true})
		if m == nil || m.resumeSessionID != resumeSID {
			t.Fatalf("materialized = %+v", m)
		}
		for _, k := range store.loads {
			if k.SessionID == "not-a-uuid" {
				t.Fatal("an invalid session id was loaded")
			}
		}
	})

	t.Run("only sidechains", func(t *testing.T) {
		t.Parallel()
		f := newResumeFixture(t)
		store := newResumeListingStore()
		store.put(t, f.key(resumeSID), SessionStoreEntry{"type": "user", "isSidechain": true})
		if m := f.materialize(t, &Options{SessionStore: store, ContinueConversation: true}); m != nil {
			t.Fatalf("materialized = %+v", m)
		}
		if left := f.leftovers(t); len(left) != 0 {
			t.Fatalf("temp dirs created: %v", left)
		}
	})

	t.Run("tie break keeps listing order", func(t *testing.T) {
		t.Parallel()
		f := newResumeFixture(t)
		store := newResumeListingStore()
		store.put(t, f.key(resumeSID2), SessionStoreEntry{"type": "user"})
		store.put(t, f.key(resumeSID), SessionStoreEntry{"type": "user"})
		store.setMTime(f.key(resumeSID), 5000)
		store.setMTime(f.key(resumeSID2), 5000)
		for range 3 {
			m := f.materialize(t, &Options{SessionStore: store, ContinueConversation: true})
			if m.resumeSessionID != resumeSID2 {
				t.Fatalf("picked %s", m.resumeSessionID)
			}
		}
	})

	t.Run("resume wins over continue", func(t *testing.T) {
		t.Parallel()
		f := newResumeFixture(t)
		store := &resumeStoreFake{} // no SessionLister needed
		store.put(t, f.key(resumeSID), SessionStoreEntry{"type": "user"})
		m := f.materialize(t, &Options{SessionStore: store, Resume: resumeSID, ContinueConversation: true})
		if m == nil || m.resumeSessionID != resumeSID {
			t.Fatalf("materialized = %+v", m)
		}
	})

	t.Run("store without lister", func(t *testing.T) {
		t.Parallel()
		f := newResumeFixture(t)
		_, err := materializeResumeSession(t.Context(), &Options{Cwd: f.cwd, SessionStore: &resumeStoreFake{}, ContinueConversation: true}, f.env)
		if !errors.Is(err, errors.ErrUnsupported) {
			t.Fatalf("error = %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// Subagent transcripts
// ---------------------------------------------------------------------------

func TestMaterializeResumeSubagents(t *testing.T) {
	t.Parallel()
	f := newResumeFixture(t)
	store := newResumeListingStore()
	store.put(t, f.key(resumeSID), SessionStoreEntry{"type": "user", "uuid": "u1"})
	sub := SessionKey{ProjectKey: f.projectKey, SessionID: resumeSID, Subpath: "subagents/agent-abc"}
	store.put(t, sub,
		SessionStoreEntry{"type": "user", "uuid": "su1"},
		SessionStoreEntry{"type": "agent_metadata", "agentType": "old"},
		SessionStoreEntry{"type": "assistant", "uuid": "sa1"},
		SessionStoreEntry{"type": "agent_metadata", "agentType": "general", "ver": 1.0},
	)
	// Metadata only: no transcript file.
	store.put(t, SessionKey{ProjectKey: f.projectKey, SessionID: resumeSID, Subpath: "subagents/agent-meta"},
		SessionStoreEntry{"type": "agent_metadata", "agentType": "x"})
	// Empty: skipped.
	store.put(t, SessionKey{ProjectKey: f.projectKey, SessionID: resumeSID, Subpath: "subagents/agent-empty"})

	m := f.materialize(t, &Options{SessionStore: store, Resume: resumeSID})
	sessionDir := filepath.Join(m.configDir, "projects", f.projectKey, resumeSID)
	got := readJSONLFile(t, filepath.Join(sessionDir, "subagents", "agent-abc.jsonl"))
	want := []map[string]any{{"type": "user", "uuid": "su1"}, {"type": "assistant", "uuid": "sa1"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("subagent transcript = %v", got)
	}
	meta := filepath.Join(sessionDir, "subagents", "agent-abc.meta.json")
	if got := readJSONFile(t, meta); !reflect.DeepEqual(got, map[string]any{"agentType": "general", "ver": 1.0}) {
		t.Fatalf("meta = %v", got)
	}
	assertMode(t, meta, 0o600)
	assertNotExist(t, filepath.Join(sessionDir, "subagents", "agent-meta.jsonl"))
	if got := readJSONFile(t, filepath.Join(sessionDir, "subagents", "agent-meta.meta.json")); got["agentType"] != "x" {
		t.Fatalf("meta = %v", got)
	}
	assertNotExist(t, filepath.Join(sessionDir, "subagents", "agent-empty.jsonl"))
}

func TestMaterializeResumeSubpathTraversalGuards(t *testing.T) {
	t.Parallel()
	f := newResumeFixture(t)
	store := newResumeListingStore()
	store.subkeysHook = func(context.Context, SessionListSubkeysKey) ([]string, error) {
		return []string{
			"", ".", "./", "a/.", "subagents/.", "/etc/passwd", `\etc\passwd`, "../escape", "a/../b",
			`a\..\b`, "C:escape", `C:\abs`, "é:x", "subagents/agent\x00x", "subagents/agent-ok",
		}, nil
	}
	store.loadHook = func(_ context.Context, key SessionKey) ([]SessionStoreEntry, error) {
		switch key.Subpath {
		case "":
			return []SessionStoreEntry{{"type": "user", "uuid": "main"}}, nil
		case "subagents/agent-ok":
			return []SessionStoreEntry{{"type": "user", "uuid": "ok"}}, nil
		}
		return nil, fmt.Errorf("loaded unsafe subpath %q", key.Subpath)
	}
	m := f.materialize(t, &Options{SessionStore: store, Resume: resumeSID})
	projectDir := filepath.Join(m.configDir, "projects", f.projectKey)
	if got := readJSONLFile(t, filepath.Join(projectDir, resumeSID, "subagents", "agent-ok.jsonl")); got[0]["uuid"] != "ok" {
		t.Fatalf("ok transcript = %v", got)
	}
	// The main transcript was not overwritten by a subkey (subpath ".").
	if got := readJSONLFile(t, filepath.Join(projectDir, resumeSID+".jsonl")); len(got) != 1 || got[0]["uuid"] != "main" {
		t.Fatalf("main transcript = %v", got)
	}
	// Nothing escaped the temp dir.
	if left := f.leftovers(t); len(left) != 1 {
		t.Fatalf("temp root = %v", left)
	}
}

func TestIsSafeSubpath(t *testing.T) {
	t.Parallel()
	sessionDir := filepath.Join(t.TempDir(), "proj", resumeSID)
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	safe := []string{"subagents/agent-1", "subagents/nested/agent-2", "a//b", "subagents/agent-1/", "agent"}
	unsafe := []string{"", ".", "..", "./x", "x/./y", "../x", "x/..", "/abs", `\abs`, "C:x", "\x00", "a\x00b"}
	if runtime.GOOS != "windows" {
		// A symlinked directory that points outside the session dir.
		outside := t.TempDir()
		if err := os.Symlink(outside, filepath.Join(sessionDir, "link")); err != nil {
			t.Fatal(err)
		}
		unsafe = append(unsafe, "link/agent-1")
	}
	for _, s := range safe {
		if !isSafeSubpath(s, sessionDir) {
			t.Errorf("%q rejected", s)
		}
	}
	for _, s := range unsafe {
		if isSafeSubpath(s, sessionDir) {
			t.Errorf("%q accepted", s)
		}
	}
}

func TestMaterializeResumeWithoutSubkeyLister(t *testing.T) {
	t.Parallel()
	f := newResumeFixture(t)
	store := resumeListOnlyStore{&resumeStoreFake{}}
	store.put(t, f.key(resumeSID), SessionStoreEntry{"type": "user"})
	store.put(t, SessionKey{ProjectKey: f.projectKey, SessionID: resumeSID, Subpath: "subagents/agent-a"}, SessionStoreEntry{"type": "user"})
	m := f.materialize(t, &Options{SessionStore: store, Resume: resumeSID})
	projectDir := filepath.Join(m.configDir, "projects", f.projectKey)
	if _, err := os.Stat(filepath.Join(projectDir, resumeSID+".jsonl")); err != nil {
		t.Fatal(err)
	}
	assertNotExist(t, filepath.Join(projectDir, resumeSID))
}

// ---------------------------------------------------------------------------
// Timeouts and errors
// ---------------------------------------------------------------------------

func TestMaterializeResumeTimeouts(t *testing.T) {
	t.Parallel()
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	// A store that ignores its context still times out.
	hang := func(context.Context) { <-block }

	t.Run("load", func(t *testing.T) {
		t.Parallel()
		f := newResumeFixture(t)
		store := &resumeStoreFake{loadHook: func(ctx context.Context, _ SessionKey) ([]SessionStoreEntry, error) {
			hang(ctx)
			return nil, nil
		}}
		_, err := materializeResumeSession(t.Context(), &Options{Cwd: f.cwd, SessionStore: store, Resume: resumeSID, LoadTimeout: 50 * time.Millisecond}, f.env)
		if err == nil || !strings.Contains(err.Error(), "SessionStore.Load() for session "+resumeSID+" timed out after 50ms") ||
			!errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("list sessions", func(t *testing.T) {
		t.Parallel()
		f := newResumeFixture(t)
		store := newResumeListingStore()
		store.listHook = func(ctx context.Context, _ string) ([]SessionStoreListEntry, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		_, err := materializeResumeSession(t.Context(), &Options{Cwd: f.cwd, SessionStore: store, ContinueConversation: true, LoadTimeout: 50 * time.Millisecond}, f.env)
		if err == nil || !strings.Contains(err.Error(), "SessionStore.ListSessions() timed out") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("list subkeys cleans up", func(t *testing.T) {
		t.Parallel()
		f := newResumeFixture(t)
		f.writeFile(t, filepath.Join(f.home, ".claude", ".credentials.json"), `{}`)
		store := newResumeListingStore()
		store.put(t, f.key(resumeSID), SessionStoreEntry{"type": "user"})
		store.subkeysHook = func(ctx context.Context, _ SessionListSubkeysKey) ([]string, error) {
			hang(ctx)
			return nil, nil
		}
		_, err := materializeResumeSession(t.Context(), &Options{Cwd: f.cwd, SessionStore: store, Resume: resumeSID, LoadTimeout: 50 * time.Millisecond}, f.env)
		if err == nil || !strings.Contains(err.Error(), "SessionStore.ListSubkeys() for session "+resumeSID+" timed out") {
			t.Fatalf("error = %v", err)
		}
		if left := f.leftovers(t); len(left) != 0 {
			t.Fatalf("temp dir leaked: %v", left)
		}
	})

	t.Run("default", func(t *testing.T) {
		t.Parallel()
		f := newResumeFixture(t)
		var deadline time.Duration
		store := &resumeStoreFake{loadHook: func(ctx context.Context, _ SessionKey) ([]SessionStoreEntry, error) {
			d, _ := ctx.Deadline()
			deadline = time.Until(d)
			return nil, nil
		}}
		f.materialize(t, &Options{SessionStore: store, Resume: resumeSID})
		if deadline <= DefaultSessionLoadTimeout-10*time.Second || deadline > DefaultSessionLoadTimeout {
			t.Fatalf("deadline = %v", deadline)
		}
	})
}

func TestMaterializeResumeErrors(t *testing.T) {
	t.Parallel()

	t.Run("load error wrapped", func(t *testing.T) {
		t.Parallel()
		f := newResumeFixture(t)
		down := errors.New("network down")
		store := &resumeStoreFake{loadHook: func(context.Context, SessionKey) ([]SessionStoreEntry, error) { return nil, down }}
		_, err := materializeResumeSession(t.Context(), &Options{Cwd: f.cwd, SessionStore: store, Resume: resumeSID}, f.env)
		if !errors.Is(err, down) || !strings.Contains(err.Error(), "failed during resume materialization: network down") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("load panic", func(t *testing.T) {
		t.Parallel()
		f := newResumeFixture(t)
		store := &resumeStoreFake{loadHook: func(context.Context, SessionKey) ([]SessionStoreEntry, error) { panic("boom") }}
		_, err := materializeResumeSession(t.Context(), &Options{Cwd: f.cwd, SessionStore: store, Resume: resumeSID}, f.env)
		if err == nil || !strings.Contains(err.Error(), "panic: boom") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("subkeys error cleans up", func(t *testing.T) {
		t.Parallel()
		f := newResumeFixture(t)
		store := newResumeListingStore()
		store.put(t, f.key(resumeSID), SessionStoreEntry{"type": "user"})
		store.subkeysHook = func(context.Context, SessionListSubkeysKey) ([]string, error) { return nil, errors.New("boom") }
		_, err := materializeResumeSession(t.Context(), &Options{Cwd: f.cwd, SessionStore: store, Resume: resumeSID}, f.env)
		if err == nil || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("error = %v", err)
		}
		if left := f.leftovers(t); len(left) != 0 {
			t.Fatalf("temp dir leaked: %v", left)
		}
	})

	t.Run("unencodable entry cleans up", func(t *testing.T) {
		t.Parallel()
		f := newResumeFixture(t)
		store := &resumeStoreFake{loadHook: func(context.Context, SessionKey) ([]SessionStoreEntry, error) {
			return []SessionStoreEntry{{"type": "user", "blob": make(chan int)}}, nil
		}}
		_, err := materializeResumeSession(t.Context(), &Options{Cwd: f.cwd, SessionStore: store, Resume: resumeSID}, f.env)
		if err == nil || !strings.Contains(err.Error(), `"blob"`) {
			t.Fatalf("error = %v", err)
		}
		if left := f.leftovers(t); len(left) != 0 {
			t.Fatalf("temp dir leaked: %v", left)
		}
	})

	t.Run("cancelled after mkdtemp cleans up", func(t *testing.T) {
		t.Parallel()
		f := newResumeFixture(t)
		store := newResumeListingStore()
		store.put(t, f.key(resumeSID), SessionStoreEntry{"type": "user"})
		ctx, cancel := context.WithCancel(t.Context())
		store.subkeysHook = func(ctx context.Context, _ SessionListSubkeysKey) ([]string, error) {
			cancel()
			<-ctx.Done()
			return nil, ctx.Err()
		}
		_, err := materializeResumeSession(ctx, &Options{Cwd: f.cwd, SessionStore: store, Resume: resumeSID}, f.env)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v", err)
		}
		if left := f.leftovers(t); len(left) != 0 {
			t.Fatalf("temp dir leaked: %v", left)
		}
	})

	t.Run("cancelled before load", func(t *testing.T) {
		t.Parallel()
		f := newResumeFixture(t)
		store := newResumeListingStore()
		store.put(t, f.key(resumeSID), SessionStoreEntry{"type": "user"})
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := materializeResumeSession(ctx, &Options{Cwd: f.cwd, SessionStore: store, Resume: resumeSID}, f.env)
		if !errors.Is(err, context.Canceled) || store.loadCount() != 0 {
			t.Fatalf("error = %v, loads = %d", err, store.loadCount())
		}
	})
}

func TestWriteEntriesJSONLRoundTrip(t *testing.T) {
	t.Parallel()
	var entries []SessionStoreEntry
	for i := range 100 {
		entries = append(entries, SessionStoreEntry{
			"uuid":    fmt.Sprintf("uuid-%d", i),
			"type":    []string{"user", "assistant"}[i%2],
			"message": map[string]any{"role": "user", "content": fmt.Sprintf("line %d \"q\" \n nl", i)},
			"nested":  map[string]any{"a": []any{float64(i), float64(i + 1)}, "b": nil},
		})
	}
	entries = append(entries, SessionStoreEntry{"no_type": true})
	path := filepath.Join(t.TempDir(), "deep", "stream.jsonl")
	if err := writeEntriesJSONL(path, entries); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	lines := strings.Split(string(raw), "\n")
	if lines[len(lines)-1] != "" || len(lines) != len(entries)+1 {
		t.Fatalf("lines = %d", len(lines))
	}
	for i, line := range lines[:len(entries)-1] {
		if !strings.HasPrefix(line, `{"type":`) {
			t.Fatalf("line %d does not start with type: %s", i, line)
		}
	}
	if got := readJSONLFile(t, path); !reflect.DeepEqual(got, entries) {
		t.Fatal("round trip mismatch")
	}
	assertMode(t, path, 0o600)
}

// ---------------------------------------------------------------------------
// Cleanup
// ---------------------------------------------------------------------------

func TestRmtreeWithRetry(t *testing.T) {
	t.Parallel()
	mkdir := func(t *testing.T) string {
		d := filepath.Join(t.TempDir(), "claude-resume-x")
		if err := os.MkdirAll(filepath.Join(d, "projects"), 0o700); err != nil {
			t.Fatal(err)
		}
		return d
	}

	t.Run("retries transient errors", func(t *testing.T) {
		t.Parallel()
		d := mkdir(t)
		calls := 0
		rmtreeWithRetry(d, func(p string) error {
			calls++
			if calls <= 2 {
				return &os.PathError{Op: "unlinkat", Path: p, Err: syscall.EBUSY}
			}
			return os.RemoveAll(p)
		}, time.Millisecond)
		if calls != 3 {
			t.Fatalf("calls = %d", calls)
		}
		assertNotExist(t, d)
	})

	t.Run("final sweep after retries", func(t *testing.T) {
		t.Parallel()
		d := mkdir(t)
		calls := 0
		rmtreeWithRetry(d, func(p string) error {
			calls++
			if calls <= rmtreeRetries {
				return &os.PathError{Op: "unlinkat", Path: p, Err: syscall.EPERM}
			}
			return os.RemoveAll(p)
		}, time.Millisecond)
		if calls != rmtreeRetries+1 {
			t.Fatalf("calls = %d", calls)
		}
		assertNotExist(t, d)
	})

	t.Run("non-transient error stops retrying", func(t *testing.T) {
		t.Parallel()
		d := mkdir(t)
		calls := 0
		rmtreeWithRetry(d, func(p string) error {
			calls++
			if calls == 1 {
				return errors.New("weird")
			}
			return os.RemoveAll(p)
		}, time.Hour)
		if calls != 2 {
			t.Fatalf("calls = %d", calls)
		}
		assertNotExist(t, d)
	})

	t.Run("missing path", func(t *testing.T) {
		t.Parallel()
		rmtreeWithRetry(filepath.Join(t.TempDir(), "gone"), func(string) error {
			t.Fatal("removeAll called for a missing path")
			return nil
		}, 0)
	})

	t.Run("failure path retries", func(t *testing.T) {
		t.Parallel()
		f := newResumeFixture(t)
		calls := 0
		f.env.removeAll = func(p string) error {
			calls++
			if calls <= 2 {
				return &os.PathError{Op: "unlinkat", Path: p, Err: syscall.EACCES}
			}
			return os.RemoveAll(p)
		}
		store := newResumeListingStore()
		store.put(t, f.key(resumeSID), SessionStoreEntry{"type": "user"})
		store.subkeysHook = func(context.Context, SessionListSubkeysKey) ([]string, error) { return nil, errors.New("boom") }
		if _, err := materializeResumeSession(t.Context(), &Options{Cwd: f.cwd, SessionStore: store, Resume: resumeSID}, f.env); err == nil {
			t.Fatal("no error")
		}
		if calls != 3 {
			t.Fatalf("calls = %d", calls)
		}
		if left := f.leftovers(t); len(left) != 0 {
			t.Fatalf("temp dir leaked: %v", left)
		}
	})
}

// ---------------------------------------------------------------------------
// Options
// ---------------------------------------------------------------------------

func TestApplyMaterializedOptions(t *testing.T) {
	t.Parallel()
	orig := &Options{ContinueConversation: true, Env: map[string]string{"A": "1"}, Model: "m"}
	got := applyMaterializedOptions(orig, &materializedResume{configDir: "/tmp/x", resumeSessionID: resumeSID})
	if got.Resume != resumeSID || got.ContinueConversation || got.Model != "m" ||
		!reflect.DeepEqual(got.Env, map[string]string{"A": "1", "CLAUDE_CONFIG_DIR": "/tmp/x"}) {
		t.Fatalf("applied = %+v", got)
	}
	if !orig.ContinueConversation || orig.Resume != "" || len(orig.Env) != 1 {
		t.Fatal("the original options were mutated")
	}
	// A nil Env works too.
	if got := applyMaterializedOptions(&Options{}, &materializedResume{configDir: "/c"}); got.Env["CLAUDE_CONFIG_DIR"] != "/c" {
		t.Fatalf("env = %v", got.Env)
	}
}

func TestValidateSessionStoreOptions(t *testing.T) {
	t.Parallel()
	minimal := &resumeStoreFake{}
	listing := newResumeListingStore()
	ok := []*Options{
		nil,
		{},
		{ContinueConversation: true, EnableFileCheckpointing: true}, // no store
		{SessionStore: minimal},
		{SessionStore: minimal, Resume: resumeSID, ContinueConversation: true},
		{SessionStore: listing, ContinueConversation: true},
	}
	for _, opts := range ok {
		if err := validateSessionStoreOptions(opts); err != nil {
			t.Errorf("%+v: %v", opts, err)
		}
	}
	bad := map[string]*Options{
		"requires the store to implement SessionLister":           {SessionStore: minimal, ContinueConversation: true},
		"cannot be combined with Options.EnableFileCheckpointing": {SessionStore: listing, EnableFileCheckpointing: true},
	}
	for want, opts := range bad {
		if err := validateSessionStoreOptions(opts); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want %q", err, want)
		}
	}
}

func TestSessionStoreValidationFailsBeforeConnect(t *testing.T) {
	t.Parallel()
	for _, opts := range []Options{
		{SessionStore: &resumeStoreFake{}, ContinueConversation: true},
		{SessionStore: newResumeListingStore(), EnableFileCheckpointing: true},
	} {
		ft := newFakeTransport()
		opts.Transport = ft
		if err := NewClient(&opts).Connect(t.Context()); err == nil || !strings.Contains(err.Error(), "SessionStore") {
			t.Fatalf("connect error = %v", err)
		}
		var qerr error
		for _, err := range Query(t.Context(), "hi", &opts) {
			qerr = err
		}
		if qerr == nil || !strings.Contains(qerr.Error(), "SessionStore") {
			t.Fatalf("query error = %v", qerr)
		}
		if ft.Ready() {
			t.Fatal("the transport was connected")
		}
	}
}

// ---------------------------------------------------------------------------
// Client and Query wiring
// ---------------------------------------------------------------------------

// hookedTransport is a fakeTransport whose Connect can fail and whose Close
// can be observed.
type hookedTransport struct {
	*fakeTransport
	connectErr error
	onClose    func()
}

func (h *hookedTransport) Connect(ctx context.Context) error {
	if h.connectErr != nil {
		return h.connectErr
	}
	return h.fakeTransport.Connect(ctx)
}

func (h *hookedTransport) Close() error {
	if h.onClose != nil {
		h.onClose()
	}
	return h.fakeTransport.Close()
}

// capturingDeps returns session deps that hand out tr in place of the CLI
// subprocess and record the options it was built from.
func capturingDeps(f *resumeFixture, tr Transport) (*sessionDeps, func() *Options) {
	var mu sync.Mutex
	var captured *Options
	deps := &sessionDeps{
		newTransport: func(opts *Options) Transport {
			mu.Lock()
			defer mu.Unlock()
			captured = opts
			return tr
		},
		resume: f.env,
	}
	return deps, func() *Options {
		mu.Lock()
		defer mu.Unlock()
		return captured
	}
}

func TestClientConnectMaterializesResumeAndMirrors(t *testing.T) {
	t.Parallel()
	f := newResumeFixture(t)
	store := newResumeListingStore()
	store.put(t, f.key(resumeSID), SessionStoreEntry{"type": "user", "uuid": "u1"})

	ft := newFakeTransport()
	initResponder(ft, map[string]any{})
	deps, captured := capturingDeps(f, ft)
	opts := &Options{Cwd: f.cwd, SessionStore: store, ContinueConversation: true, CLIPath: "/usr/bin/claude"}
	client := NewClient(opts)
	client.deps = deps
	if err := client.Connect(t.Context()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Disconnect()

	got := captured()
	if got == nil {
		t.Fatal("no transport built")
	}
	configDir := got.Env["CLAUDE_CONFIG_DIR"]
	if got.Resume != resumeSID || got.ContinueConversation || filepath.Dir(configDir) != f.tempRoot {
		t.Fatalf("transport options: resume=%q continue=%v config=%q", got.Resume, got.ContinueConversation, configDir)
	}
	transcript := filepath.Join(configDir, "projects", f.projectKey, resumeSID+".jsonl")
	if _, err := os.Stat(transcript); err != nil {
		t.Fatal(err)
	}
	args, err := buildCommandArgs(got)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(args, "--resume="+resumeSID) || slices.Contains(args, "--continue") ||
		slices.Contains(args, "--resume") || !slices.Contains(args, "--session-mirror") {
		t.Fatalf("args = %v", args)
	}
	// The caller's options are untouched.
	if !opts.ContinueConversation || opts.Resume != "" || opts.Env != nil {
		t.Fatalf("caller options mutated: %+v", opts)
	}

	// Mirror frames for the materialized projects dir reach the store.
	if err := client.Query(t.Context(), "next", ""); err != nil {
		t.Fatal(err)
	}
	ft.push(mirrorFrame(transcript, map[string]any{"type": "user", "uuid": "u2"}))
	ft.push(resultFrame())
	for _, err := range client.ReceiveResponse(t.Context()) {
		if err != nil {
			t.Fatal(err)
		}
	}
	entries := store.entries(f.key(resumeSID))
	if len(entries) != 2 || entries[1]["uuid"] != "u2" {
		t.Fatalf("store entries = %v", entries)
	}

	if err := client.Disconnect(); err != nil {
		t.Fatal(err)
	}
	assertNotExist(t, configDir)
}

func TestClientConnectWithoutStorePassesOptionsThrough(t *testing.T) {
	t.Parallel()
	f := newResumeFixture(t)
	ft := newFakeTransport()
	initResponder(ft, map[string]any{})
	deps, captured := capturingDeps(f, ft)
	client := NewClient(&Options{Cwd: f.cwd, Resume: resumeSID})
	client.deps = deps
	if err := client.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer client.Disconnect()
	if got := captured(); got.Resume != resumeSID || got.Env["CLAUDE_CONFIG_DIR"] != "" {
		t.Fatalf("options = %+v", got)
	}
	if left := f.leftovers(t); len(left) != 0 {
		t.Fatalf("temp dirs = %v", left)
	}
}

func TestClientConnectFailureRemovesResumeDir(t *testing.T) {
	t.Parallel()

	t.Run("transport connect", func(t *testing.T) {
		t.Parallel()
		f := newResumeFixture(t)
		store := newResumeListingStore()
		store.put(t, f.key(resumeSID), SessionStoreEntry{"type": "user"})
		spawnErr := errors.New("spawn failed")
		deps, _ := capturingDeps(f, &hookedTransport{fakeTransport: newFakeTransport(), connectErr: spawnErr})
		client := NewClient(&Options{Cwd: f.cwd, SessionStore: store, Resume: resumeSID})
		client.deps = deps
		if err := client.Connect(t.Context()); !errors.Is(err, spawnErr) {
			t.Fatalf("error = %v", err)
		}
		if left := f.leftovers(t); len(left) != 0 {
			t.Fatalf("temp dir leaked: %v", left)
		}
		if store.loadCount() == 0 {
			t.Fatal("the session was never materialized")
		}
		_ = client.Disconnect()
	})

	t.Run("initialize closes the CLI before cleanup", func(t *testing.T) {
		t.Parallel()
		f := newResumeFixture(t)
		store := newResumeListingStore()
		store.put(t, f.key(resumeSID), SessionStoreEntry{"type": "user"})
		ft := newFakeTransport()
		ft.mu.Lock()
		ft.onWrite = func(frame map[string]any) {
			ft.push(map[string]any{"type": "control_response", "response": map[string]any{
				"subtype": "error", "request_id": frame["request_id"], "error": "control timeout"}})
		}
		ft.mu.Unlock()
		var dirAtClose []string
		ht := &hookedTransport{fakeTransport: ft}
		ht.onClose = func() { dirAtClose = f.leftovers(t) }
		deps, _ := capturingDeps(f, ht)
		client := NewClient(&Options{Cwd: f.cwd, SessionStore: store, Resume: resumeSID})
		client.deps = deps
		if err := client.Connect(t.Context()); err == nil || !strings.Contains(err.Error(), "control timeout") {
			t.Fatalf("error = %v", err)
		}
		if len(dirAtClose) != 1 {
			t.Fatalf("temp dir at transport close = %v; it must outlive the CLI", dirAtClose)
		}
		if left := f.leftovers(t); len(left) != 0 {
			t.Fatalf("temp dir leaked: %v", left)
		}
	})

	t.Run("cancelled during load", func(t *testing.T) {
		t.Parallel()
		f := newResumeFixture(t)
		ctx, cancel := context.WithCancel(t.Context())
		store := &resumeStoreFake{loadHook: func(ctx context.Context, _ SessionKey) ([]SessionStoreEntry, error) {
			cancel()
			<-ctx.Done()
			return nil, ctx.Err()
		}}
		deps := &sessionDeps{
			newTransport: func(*Options) Transport { t.Error("transport built after cancellation"); return newFakeTransport() },
			resume:       f.env,
		}
		client := NewClient(&Options{Cwd: f.cwd, SessionStore: store, Resume: resumeSID})
		client.deps = deps
		if err := client.Connect(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v", err)
		}
		if left := f.leftovers(t); len(left) != 0 {
			t.Fatalf("temp dir leaked: %v", left)
		}
		if err := client.Disconnect(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestCustomTransportSkipsMaterializationButMirrors(t *testing.T) {
	t.Parallel()
	f := newResumeFixture(t)
	configDir := t.TempDir()
	store := newResumeListingStore()
	store.put(t, f.key(resumeSID), SessionStoreEntry{"type": "user", "uuid": "u1"})
	path := filepath.Join(configDir, "projects", f.projectKey, resumeSID+".jsonl")

	// Client.
	ft := newFakeTransport()
	initResponder(ft, map[string]any{})
	client := NewClient(&Options{
		Cwd: f.cwd, SessionStore: store, Resume: resumeSID, Transport: ft,
		Env: map[string]string{"CLAUDE_CONFIG_DIR": configDir},
	})
	client.deps = &sessionDeps{resume: f.env}
	if err := client.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := store.loadCount(); n != 0 {
		t.Fatalf("store loads = %d; a custom transport must skip materialization", n)
	}
	ft.push(mirrorFrame(path, map[string]any{"type": "user", "uuid": "c1"}))
	ft.push(resultFrame())
	for _, err := range client.ReceiveResponse(t.Context()) {
		if err != nil {
			t.Fatal(err)
		}
	}
	_ = client.Disconnect()

	// Query.
	qt := scriptedCLI(t, mirrorFrame(path, map[string]any{"type": "user", "uuid": "q1"}), resultFrame())
	for _, err := range queryStream(t.Context(), func(yield func(UserInput) bool) { yield(UserInput{Content: "hi"}) },
		&Options{Cwd: f.cwd, SessionStore: store, Resume: resumeSID, Transport: qt, Env: map[string]string{"CLAUDE_CONFIG_DIR": configDir}},
		&sessionDeps{resume: f.env}) {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := store.loadCount(); n != 0 {
		t.Fatalf("store loads = %d", n)
	}
	var uuids []any
	for _, e := range store.entries(f.key(resumeSID)) {
		uuids = append(uuids, e["uuid"])
	}
	if !reflect.DeepEqual(uuids, []any{"u1", "c1", "q1"}) {
		t.Fatalf("store entries = %v", uuids)
	}
	if left := f.leftovers(t); len(left) != 0 {
		t.Fatalf("temp dirs = %v", left)
	}
}

func TestQueryMaterializesResume(t *testing.T) {
	t.Parallel()
	f := newResumeFixture(t)
	store := newResumeListingStore()
	store.put(t, f.key(resumeSID), SessionStoreEntry{"type": "user", "uuid": "u1"})

	var transcript string
	var dirAtClose []string
	ht := &hookedTransport{fakeTransport: newFakeTransport()}
	ht.onClose = func() { dirAtClose = f.leftovers(t) }
	ht.mu.Lock()
	ht.onWrite = func(frame map[string]any) {
		if frame["type"] != "control_request" {
			return
		}
		ht.push(map[string]any{"type": "control_response", "response": map[string]any{
			"subtype": "success", "request_id": frame["request_id"], "response": map[string]any{}}})
		ht.push(mirrorFrame(transcript, map[string]any{"type": "assistant", "uuid": "a1"}))
		ht.push(assistantFrame("one"))
		ht.push(assistantFrame("two"))
		ht.push(resultFrame())
	}
	ht.mu.Unlock()
	deps := &sessionDeps{
		newTransport: func(opts *Options) Transport {
			transcript = filepath.Join(opts.Env["CLAUDE_CONFIG_DIR"], "projects", f.projectKey, opts.Resume+".jsonl")
			return ht
		},
		resume: f.env,
	}
	opts := &Options{Cwd: f.cwd, SessionStore: store, Resume: resumeSID}
	// The consumer stops early: the CLI is closed before the dir is removed.
	for msg, err := range queryStream(t.Context(), func(yield func(UserInput) bool) { yield(UserInput{Content: "hi"}) }, opts, deps) {
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := msg.(*AssistantMessage); ok {
			break
		}
	}
	if len(dirAtClose) != 1 {
		t.Fatalf("temp dir at transport close = %v", dirAtClose)
	}
	if left := f.leftovers(t); len(left) != 0 {
		t.Fatalf("temp dir leaked: %v", left)
	}
	// The mirror frame was flushed to the store when the engine closed.
	entries := store.entries(f.key(resumeSID))
	if len(entries) != 2 || entries[1]["uuid"] != "a1" {
		t.Fatalf("store entries = %v", entries)
	}

	// A transport that fails to connect also leaves nothing behind.
	spawnErr := errors.New("spawn failed")
	deps.newTransport = func(*Options) Transport {
		return &hookedTransport{fakeTransport: newFakeTransport(), connectErr: spawnErr}
	}
	var got error
	for _, err := range queryStream(t.Context(), func(yield func(UserInput) bool) { yield(UserInput{Content: "hi"}) }, opts, deps) {
		got = err
	}
	if !errors.Is(got, spawnErr) {
		t.Fatalf("error = %v", got)
	}
	if left := f.leftovers(t); len(left) != 0 {
		t.Fatalf("temp dir leaked: %v", left)
	}
}
