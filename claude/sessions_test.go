package claude

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ironpark/gelati/internal/jsonx"
)

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// jsonObj renders a compact JSON object with keys in the given order
// (key, value, key, value, ...), matching the CLI's on-disk format.
func jsonObj(kv ...any) string {
	var sb strings.Builder
	sb.WriteByte('{')
	for i := 0; i < len(kv); i += 2 {
		if i > 0 {
			sb.WriteByte(',')
		}
		k, _ := jsonx.Marshal(kv[i])
		v, err := jsonx.Marshal(kv[i+1])
		if err != nil {
			panic(err)
		}
		sb.Write(k)
		sb.WriteByte(':')
		sb.Write(v)
	}
	sb.WriteByte('}')
	return sb.String()
}

func writeJSONL(t testing.TB, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t testing.TB, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func setMTime(t testing.TB, path string, unixSeconds float64) {
	t.Helper()
	mt := time.Unix(0, int64(unixSeconds*1e9))
	if err := os.Chtimes(path, mt, mt); err != nil {
		t.Fatal(err)
	}
}

// newProjectsRoot returns an empty projects directory.
func newProjectsRoot(t testing.TB) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "config", "projects")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

// newProject creates a real project directory and returns its path as
// given and its canonical form.
func newProject(t testing.TB, name string) (path, canonical string) {
	t.Helper()
	path = filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	return path, canonicalizePath(path)
}

// makeProjectDir creates the transcript directory for projectPath.
func makeProjectDir(t testing.TB, root, projectPath string) string {
	t.Helper()
	dir := filepath.Join(root, sanitizePath(projectPath))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

type sessionFile struct {
	id          string
	firstPrompt string
	summary     string
	customTitle string
	gitBranch   string
	cwd         string
	sidechain   bool
	metaOnly    bool
	mtime       float64 // Unix seconds; zero leaves the write time
}

// makeSessionFile mirrors the Python tests' _make_session_file: a user
// prompt, an assistant reply and a summary/title tail entry.
func makeSessionFile(t testing.TB, projectDir string, f sessionFile) string {
	t.Helper()
	if f.id == "" {
		f.id = randomUUID()
	}
	if f.firstPrompt == "" {
		f.firstPrompt = "Hello Claude"
	}
	first := []any{"type", "user", "message", map[string]any{"role": "user", "content": f.firstPrompt}}
	if f.cwd != "" {
		first = append(first, "cwd", f.cwd)
	}
	if f.gitBranch != "" {
		first = append(first, "gitBranch", f.gitBranch)
	}
	if f.sidechain {
		first = append(first, "isSidechain", true)
	}
	if f.metaOnly {
		first = append(first, "isMeta", true)
	}
	tail := []any{"type", "summary"}
	if f.summary != "" {
		tail = append(tail, "summary", f.summary)
	}
	if f.customTitle != "" {
		tail = append(tail, "customTitle", f.customTitle)
	}
	if f.gitBranch != "" {
		tail = append(tail, "gitBranch", f.gitBranch)
	}
	path := filepath.Join(projectDir, f.id+".jsonl")
	writeJSONL(t, path,
		jsonObj(first...),
		jsonObj("type", "assistant", "message", map[string]any{"role": "assistant", "content": "Hi there!"}),
		jsonObj(tail...),
	)
	if f.mtime != 0 {
		setMTime(t, path, f.mtime)
	}
	return f.id
}

// transcriptEntry mirrors the Python tests' _make_transcript_entry.
func transcriptEntry(typ, uid, parent, sid string, content any, extra ...any) string {
	var parentVal any
	if parent != "" {
		parentVal = parent
	}
	kv := []any{"type", typ, "uuid", uid, "parentUuid", parentVal, "sessionId", sid}
	if content != nil {
		role := "user"
		if typ == "assistant" {
			role = "assistant"
		}
		kv = append(kv, "message", map[string]any{"role": role, "content": content})
	}
	return jsonObj(append(kv, extra...)...)
}

func sessionIDs(infos []SessionInfo) []string {
	ids := make([]string, len(infos))
	for i, s := range infos {
		ids[i] = s.SessionID
	}
	return ids
}

func messageUUIDs(msgs []SessionMessage) []string {
	ids := make([]string, len(msgs))
	for i, m := range msgs {
		ids[i] = m.UUID
	}
	return ids
}

func sortedCopy(s []string) []string {
	s = slices.Clone(s)
	slices.Sort(s)
	return s
}

// ---------------------------------------------------------------------------
// Helpers (values computed by running the Python implementation)
// ---------------------------------------------------------------------------

func TestSessionValidateUUID(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"550e8400-e29b-41d4-a716-446655440000", "550E8400-E29B-41D4-A716-446655440000"} {
		if !validateUUID(s) {
			t.Errorf("validateUUID(%q) = false", s)
		}
	}
	for _, s := range []string{"not-a-uuid", "", "550e8400-e29b-41d4-a716", "550e8400-e29b-41d4-a716-446655440000\n", " 550e8400-e29b-41d4-a716-446655440000"} {
		if validateUUID(s) {
			t.Errorf("validateUUID(%q) = true", s)
		}
	}
}

func TestSessionSimpleHashMatchesPython(t *testing.T) {
	t.Parallel()
	tests := []struct{ in, want string }{
		{"", "0"},
		{"hello", "1n1e4y"},
		{"world", "1vgtci"},
		{"a", "2p"},
		{"/Users/foo/my-project", "jiemc7"},
		{strings.Repeat("/x", 150), "4y6yx2"},
		{"héllo wörld", "qxcvrt"},
		{"日本語のパス", "dwtq81"},
		{"emoji😀path", "f3ww9t"},
		{strings.Repeat("abc", 1000), "1r668"},
		{"/tmp/" + strings.Repeat("deep/", 60), "pqvxy1"},
	}
	for _, tt := range tests {
		if got := simpleHash(tt.in); got != tt.want {
			t.Errorf("simpleHash(%.20q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestSessionSanitizePath(t *testing.T) {
	t.Parallel()
	tests := []struct{ in, want string }{
		{"/Users/foo/my-project", "-Users-foo-my-project"},
		{"plugin:name:server", "plugin-name-server"},
		{"héllo wörld", "h-llo-w-rld"},
		{"日本語のパス", "------"},
		{"emoji😀path", "emoji-path"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := sanitizePath(tt.in); got != tt.want {
			t.Errorf("sanitizePath(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
	// Long paths: 200-char prefix + "-" + hash (values from Python).
	long := []struct{ in, suffix string }{
		{strings.Repeat("/x", 150), "x-x-x-4y6yx2"},
		{"/tmp/" + strings.Repeat("deep/", 60), "deep--pqvxy1"},
		{strings.Repeat("/Users/é", 40), "ers---cibw94"},
	}
	for _, tt := range long {
		got := sanitizePath(tt.in)
		if len(got) != 207 || got[195:] != tt.suffix || got[200] != '-' {
			t.Errorf("sanitizePath(long %.12q) = ...%q (len %d), want suffix %q", tt.in, got[195:], len(got), tt.suffix)
		}
	}
	if got := sanitizePath(strings.Repeat("a", 200)); got != strings.Repeat("a", 200) {
		t.Errorf("200-char name was truncated: %q", got)
	}
}

func TestSessionExtractJSONStringField(t *testing.T) {
	t.Parallel()
	tests := []struct {
		text, key, first, last string
	}{
		{`{"foo":"bar","baz":"qux"}`, "foo", "bar", "bar"},
		{`{"foo":"bar","baz":"qux"}`, "baz", "qux", "qux"},
		{`{"foo":"bar"}`, "missing", "", ""},
		{`{"foo": "bar"}`, "foo", "bar", "bar"},
		{`{"foo":"bar\"baz"}`, "foo", `bar"baz`, `bar"baz`},
		{`{"summary":"first"}` + "\n" + `{"summary":"second"}` + "\n" + `{"summary":"third"}`, "summary", "first", "third"},
		{`{"a":"x\u00e9y"}`, "a", "x\u00e9y", "x\u00e9y"},
		{`{"a":"bad\q"}`, "a", `bad\q`, `bad\q`},
		// "last" is the last occurrence by position, whatever the pattern
		// (TS Mt()); "first" prefers the compact pattern.
		{`{"a": "s1"} {"a":"c1"} {"a":"c2"}`, "a", "c1", "c2"},
		{`{"a":"c1"} {"a": "s1"}`, "a", "c1", "s1"},
		{`{"a":"c1"} {"a":"unterminated`, "a", "c1", "c1"},
		{`{"a":"x\\"}`, "a", `x\`, `x\`},
		{`{"a":"unterminated`, "a", "", ""},
		{`{"a":"\"q\""}`, "a", `"q"`, `"q"`},
	}
	for _, tt := range tests {
		if got := extractJSONStringField(tt.text, tt.key); got != tt.first {
			t.Errorf("extractJSONStringField(%q, %q) = %q, want %q", tt.text, tt.key, got, tt.first)
		}
		if got := extractLastJSONStringField(tt.text, tt.key); got != tt.last {
			t.Errorf("extractLastJSONStringField(%q, %q) = %q, want %q", tt.text, tt.key, got, tt.last)
		}
	}
}

func TestSessionExtractFirstPromptFromHead(t *testing.T) {
	t.Parallel()
	user := func(content any, extra ...any) string {
		return jsonObj(append([]any{"type", "user", "message", map[string]any{"content": content}}, extra...)...)
	}
	tests := []struct {
		name string
		head string
		want string
	}{
		{"simple", user("Hello!") + "\n", "Hello!"},
		{"skips meta", user("meta", "isMeta", true) + "\n" + user("real prompt") + "\n", "real prompt"},
		{"skips tool result", user([]any{map[string]any{"type": "tool_result", "content": "x"}}) + "\n" + user("actual prompt") + "\n", "actual prompt"},
		{"skips compact summary", user("summary", "isCompactSummary", true) + "\n" + user("next"), "next"},
		{"content blocks", user([]any{map[string]any{"type": "text", "text": "block prompt"}}) + "\n", "block prompt"},
		{"command fallback", user("<command-name>/help</command-name>stuff") + "\n", "/help"},
		{"first command wins", user("<command-name>/a</command-name>") + "\n" + user("<command-name>/b</command-name>") + "\n", "/a"},
		{"empty", "", ""},
		{"assistant only", `{"type":"assistant"}` + "\n", ""},
		{"ide opened file", user("  <ide_opened_file>x\ny</ide_opened_file>  ") + "\n" + user("after ide") + "\n", "after ide"},
		{"interrupt", user("[Request interrupted by user for tool use]") + "\n" + user("ok") + "\n", "ok"},
		{"tick and unicode strip", user("<tick>") + "\n" + user("\u00a0real\u00a0") + "\n", "real"},
		{"spaced json", `{"type": "user", "message": {"content": "line1\nline2"}}`, "line1 line2"},
		{"blank block skipped", user([]any{map[string]any{"type": "image"}, map[string]any{"type": "text", "text": "  "}, map[string]any{"type": "text", "text": "second block"}}), "second block"},
		{"truncates runes", user(strings.Repeat("é", 199) + " xxxxx"), strings.Repeat("é", 199) + "\u2026"},
		{"truncates and strips", user(strings.Repeat("a", 199) + "   bbb"), strings.Repeat("a", 199) + "\u2026"},
		{"corrupt line skipped", `{"type":"user", broken` + "\n" + user("good"), "good"},
	}
	for _, tt := range tests {
		if got := extractFirstPromptFromHead(tt.head); got != tt.want {
			t.Errorf("%s: got %q, want %q", tt.name, got, tt.want)
		}
	}
	got := extractFirstPromptFromHead(user(strings.Repeat("x", 300)))
	if n := len([]rune(got)); n > 201 || !strings.HasSuffix(got, "\u2026") {
		t.Errorf("truncation: %d runes, %q", n, got)
	}
}

func TestSessionDecodeUTF8Replace(t *testing.T) {
	t.Parallel()
	// Expected values from Python's bytes.decode("utf-8", errors="replace").
	tests := []struct {
		in   []byte
		want string
	}{
		{[]byte("plain é"), "plain é"},
		{[]byte("ab\xe2\x82"), "ab\ufffd"},
		{[]byte("\x82\x82ab"), "\ufffd\ufffdab"},
		{[]byte("a\xe2\x82\xe2\x82b"), "a\ufffd\ufffdb"},
		{[]byte("\xf0\x9f\x98"), "\ufffd"},
		{[]byte("\xed\xa0\x80x"), "\ufffd\ufffd\ufffdx"},
		{[]byte("\xc0\xaf"), "\ufffd\ufffd"},
		{[]byte("\xf4\x90\x80\x80"), "\ufffd\ufffd\ufffd\ufffd"},
		{[]byte("\xe0\x80\x80"), "\ufffd\ufffd\ufffd"},
		{[]byte("\xff\xfe"), "\ufffd\ufffd"},
	}
	for _, tt := range tests {
		if got := decodeUTF8Replace(tt.in); got != tt.want {
			t.Errorf("decodeUTF8Replace(%q) = %+q, want %+q", tt.in, got, tt.want)
		}
	}
}

func TestSessionISOToEpochMillis(t *testing.T) {
	t.Parallel()
	// Expected values from the Python implementation.
	tests := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"2026-01-15T10:30:00.000Z", 1768473000000, true},
		{"2026-01-15T10:30:00+00:00", 1768473000000, true},
		{"2026-01-15T10:30:00.123Z", 1768473000123, true},
		{"2026-01-15T10:30:00.123456Z", 1768473000123, true},
		{"2026-01-15T10:30:00.1234567Z", 1768473000123, true},
		{"2026-01-15T10:30:00.00012345678901Z", 1768473000000, true},
		{"2026-01-15T10:30:00,5Z", 1768473000500, true},
		{"2026-01-15 10:30:00Z", 1768473000000, true},
		{"2026-01-15x10:30:00Z", 1768473000000, true},
		{"2026-01-15T10:30:00+05:30", 1768453200000, true},
		{"2026-01-15T10:30:00-0800", 1768501800000, true},
		{"2026-01-15T10:30:00+05", 1768455000000, true},
		{"2026-01-15T10:30:00+05:30:00", 1768453200000, true},
		{"2026-01-15T10:30:00.5+00:00:30", 1768472970500, true},
		{"2026-01-15T10:30:00+00:00:30.5", 1768472969500, true},
		{"2026-01-15T10:30:00+23:59", 1768386660000, true},
		{"20260115T103000Z", 1768473000000, true},
		{"2026-01-15T103000Z", 1768473000000, true},
		{"2026-01-15T103000.5Z", 1768473000500, true},
		{"2026-01-15T24:00:00Z", 1768521600000, true},
		{"2026-01-15T10:30:00.123+0000", 1768473000123, true},
		{"1969-12-31T23:59:59.999Z", -1, true},
		{"0001-01-01T00:00:00Z", -62135596800000, true},
		{"2026-01-15T10:30:00.Z", 0, false},
		{"2026-02-30T10:00:00Z", 0, false},
		{"not-a-valid-iso-date", 0, false},
		{"2026-01-15T10:30:00Zjunk", 0, false},
		{"2026-01-15T10:30:60Z", 0, false},
		{"2026-1-15", 0, false},
		{"2026-01-15T1:30Z", 0, false},
		{"2026-01-15T10:30:00z", 0, false},
		{"2026-01-15T10:30:00+24:00", 0, false},
		{"2026-01-15T10:30.5Z", 0, false},
		{"2026-01-15T10:3000", 0, false},
		{"2026-01-15T10:30:00+5", 0, false},
		{"2026-01-15T10:30:00+05:3", 0, false},
		{"2026-01-15T", 0, false},
		{"0000-01-01", 0, false},
		{"", 0, false},
	}
	for _, tt := range tests {
		got, ok := isoToEpochMillis(tt.in)
		if ok != tt.ok || got != tt.want {
			t.Errorf("isoToEpochMillis(%q) = %d, %v; want %d, %v", tt.in, got, ok, tt.want, tt.ok)
		}
	}
	// Without an offset the timestamp is local time, as in Python.
	for _, tt := range []struct {
		in   string
		want time.Time
	}{
		{"2026-01-15", time.Date(2026, 1, 15, 0, 0, 0, 0, time.Local)},
		{"2026-01-15T10", time.Date(2026, 1, 15, 10, 0, 0, 0, time.Local)},
		{"2026-01-15T10:30", time.Date(2026, 1, 15, 10, 30, 0, 0, time.Local)},
		{"2026-01-15T10:30:00.250", time.Date(2026, 1, 15, 10, 30, 0, 250e6, time.Local)},
	} {
		if got, ok := isoToEpochMillis(tt.in); !ok || got != tt.want.UnixMilli() {
			t.Errorf("isoToEpochMillis(%q) = %d, %v; want %d", tt.in, got, ok, tt.want.UnixMilli())
		}
	}
}

func TestSessionPageSlice(t *testing.T) {
	t.Parallel()
	seq := []int{0, 1, 2, 3, 4, 5}
	tests := []struct {
		limit, offset int
		want          []int
	}{
		{0, 0, seq},
		{2, 0, []int{0, 1}},
		{2, 2, []int{2, 3}},
		{0, 4, []int{4, 5}},
		{0, 100, []int{}},
		{2, 100, []int{}},
		{-1, 0, seq},
		{0, -3, seq},
		{2, -1, []int{}},        // Python: seq[-1:1]
		{3, -4, []int{2, 3, 4}}, // Python: seq[-4:-1]
	}
	for _, tt := range tests {
		if got := pageSlice(seq, tt.limit, tt.offset); !slices.Equal(got, tt.want) {
			t.Errorf("pageSlice(limit=%d, offset=%d) = %v, want %v", tt.limit, tt.offset, got, tt.want)
		}
	}
}

func TestSessionCanonicalizePath(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(filepath.Join(real, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	realCanon, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct{ in, want string }{
		{real, realCanon},
		{link, realCanon},
		{filepath.Join(link, "sub"), filepath.Join(realCanon, "sub")},
		// ".." applies after resolving the symlink, like realpath.
		{filepath.Join(link, "sub", "..", ".."), filepath.Dir(realCanon)},
		// Missing components are kept unresolved.
		{filepath.Join(link, "missing", "x"), filepath.Join(realCanon, "missing", "x")},
		{"/definitely/not/here", "/definitely/not/here"},
		{"/a/./b//c/", "/a/b/c"},
		// NFC normalization.
		{"/tmp-nonexistent-cafe\u0301", "/tmp-nonexistent-caf\u00e9"},
	}
	for _, tt := range tests {
		if got := canonicalizePath(tt.in); got != tt.want {
			t.Errorf("canonicalizePath(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
	// Symlink loops stop resolution instead of hanging.
	loop := filepath.Join(base, "loop")
	if err := os.Symlink(loop, loop); err != nil {
		t.Fatal(err)
	}
	if got := canonicalizePath(filepath.Join(loop, "x")); !strings.HasSuffix(got, "/loop/x") {
		t.Errorf("loop: got %q", got)
	}
	// Relative paths resolve against the working directory.
	wd, _ := os.Getwd()
	if got, want := canonicalizePath("."), canonicalizePath(wd); got != want {
		t.Errorf("canonicalizePath(.) = %q, want %q", got, want)
	}
}

func TestSessionProjectsDir(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "/from/env")
	if got := projectsDir(nil); got != filepath.Join("/from/env", "projects") {
		t.Errorf("projectsDir(nil) = %q", got)
	}
	if got := projectsDir(map[string]string{"CLAUDE_CONFIG_DIR": "/override-cafe\u0301"}); got != filepath.Join("/override-caf\u00e9", "projects") {
		t.Errorf("projectsDir(override) = %q", got)
	}
	if got := projectsDir(map[string]string{"OTHER": "x"}); got != filepath.Join("/from/env", "projects") {
		t.Errorf("projectsDir(unrelated override) = %q", got)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip(err)
	}
	if got := projectsDir(nil); got != normalizeNFC(filepath.Join(home, ".claude", "projects")) {
		t.Errorf("projectsDir default = %q", got)
	}
}

func TestSessionFindProjectDir(t *testing.T) {
	t.Parallel()
	root := newProjectsRoot(t)
	short := "/some/project"
	if got := newLocalSessions(root).findProjectDir(short); got != "" {
		t.Errorf("missing short project: %q", got)
	}
	dir := makeProjectDir(t, root, short)
	if got := newLocalSessions(root).findProjectDir(short); got != dir {
		t.Errorf("exact: %q, want %q", got, dir)
	}

	// Long path whose directory was named with a different hash (Bun.hash
	// in the CLI): matched by prefix.
	long := "/" + strings.Repeat("segment/", 40)
	prefix := sanitizePath(long)[:maxSanitizedLength]
	if got := newLocalSessions(root).findProjectDir(long); got != "" {
		t.Errorf("missing long project: %q", got)
	}
	bunDir := filepath.Join(root, prefix+"-bunhash")
	if err := os.MkdirAll(bunDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The prefix match is confirmed by a session recorded in the path.
	if got := newLocalSessions(root).findProjectDir(long); got != "" {
		t.Errorf("unconfirmed prefix fallback: %q", got)
	}
	writeJSONL(t, filepath.Join(bunDir, randomUUID()+".jsonl"), jsonObj("type", "user", "cwd", long))
	otherDir := filepath.Join(root, prefix+"-other")
	writeJSONL(t, filepath.Join(otherDir, randomUUID()+".jsonl"), jsonObj("type", "user", "cwd", long+"x"))
	if got := newLocalSessions(root).findProjectDirs(long); !slices.Equal(got, []string{bunDir}) {
		t.Errorf("prefix fallback: %q, want %q", got, bunDir)
	}
	// Prefix matching never applies to short paths.
	if err := os.MkdirAll(filepath.Join(root, sanitizePath("/other")+"-x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := newLocalSessions(root).findProjectDir("/other"); got != "" {
		t.Errorf("short prefix matched: %q", got)
	}
	if got := newLocalSessions(filepath.Join(root, "nope")).findProjectDir(long); got != "" {
		t.Errorf("missing root: %q", got)
	}
}

func TestSessionReadSessionLite(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if readSessionLite(filepath.Join(dir, "missing.jsonl")) != nil {
		t.Error("missing file")
	}
	empty := filepath.Join(dir, "empty.jsonl")
	writeFile(t, empty, "")
	if readSessionLite(empty) != nil {
		t.Error("empty file")
	}
	if readSessionLite(dir) != nil {
		t.Error("directory")
	}

	small := filepath.Join(dir, "small.jsonl")
	writeFile(t, small, "abc\n")
	setMTime(t, small, 3000.0)
	lite := readSessionLite(small)
	if lite == nil || lite.head != "abc\n" || lite.tail != lite.head || lite.size != 4 || lite.mtime != 3_000_000 {
		t.Fatalf("small: %+v", lite)
	}

	// A large file: head and tail are distinct windows, and a multi-byte
	// character split by the tail boundary decodes like Python.
	big := filepath.Join(dir, "big.jsonl")
	content := "H" + strings.Repeat("x", liteReadBufSize) + "é" + strings.Repeat("y", liteReadBufSize-2) + "T"
	writeFile(t, big, content)
	setMTime(t, big, 1700000000.123)
	lite = readSessionLite(big)
	if lite == nil {
		t.Fatal("big: nil")
	}
	if lite.size != int64(len(content)) || len(lite.head) != liteReadBufSize || lite.head[0] != 'H' {
		t.Errorf("big head: size=%d len(head)=%d", lite.size, len(lite.head))
	}
	if !strings.HasPrefix(lite.tail, "\ufffdy") || !strings.HasSuffix(lite.tail, "T") {
		t.Errorf("big tail: %q...", lite.tail[:8])
	}
	if want := jsMTimeMillis(time.Unix(1700000000, 123000000)); lite.mtime != want {
		t.Errorf("mtime = %d, want %d", lite.mtime, want)
	}
}

func TestSessionJSMTimeMillis(t *testing.T) {
	t.Parallel()
	// Node: Math.round(sec * 1e3 + nsec / 1e6), as Stats.mtime.getTime()
	// (values checked against Node).
	tests := []struct {
		sec, nsec int64
		want      int64
	}{
		{3000, 0, 3_000_000},
		{1700000000, 123000000, 1700000000123},
		{1700000000, 123456789, 1700000000123},
		{1700000000, 999600000, 1700000001000},
		{1700000000, 999999999, 1700000001000},
		{1700000000, 500000, 1700000000001},
	}
	for _, tt := range tests {
		if got := jsMTimeMillis(time.Unix(tt.sec, tt.nsec)); got != tt.want {
			t.Errorf("jsMTimeMillis(%d.%09d) = %d, want %d", tt.sec, tt.nsec, got, tt.want)
		}
	}
}

func TestSessionParseSessionInfoFromLite(t *testing.T) {
	t.Parallel()
	sid := randomUUID()
	path := filepath.Join(t.TempDir(), sid+".jsonl")
	writeJSONL(t, path,
		jsonObj("type", "user", "message", map[string]any{"content": "test prompt"}, "cwd", "/workspace"),
		jsonObj("type", "tag", "tag", "experiment", "sessionId", sid),
	)
	lite := readSessionLite(path)
	info := parseSessionInfoFromLite(sid, lite, "/fallback", "")
	if info == nil || info.SessionID != sid || info.Summary != "test prompt" || info.Tag != "experiment" || info.Cwd != "/workspace" {
		t.Fatalf("info = %+v", info)
	}

	// Invalid timestamps leave CreatedAt unset; offsets are honoured.
	for ts, want := range map[string]int64{"not-a-valid-iso-date": 0, "2026-01-15T10:30:00+00:00": 1768473000000} {
		writeJSONL(t, path, jsonObj("type", "user", "message", map[string]any{"content": "hello"}, "timestamp", ts))
		info := parseSessionInfoFromLite(sid, readSessionLite(path), "", "")
		if info == nil || info.CreatedAt != want {
			t.Errorf("timestamp %q: %+v", ts, info)
		}
	}
}

// ---------------------------------------------------------------------------
// ListSessions
// ---------------------------------------------------------------------------

func TestSessionListSessions(t *testing.T) {
	t.Parallel()
	list := func(root, dir string, limit, offset int) []SessionInfo {
		return newLocalSessions(root).listSessions(&ListSessionsOptions{Directory: dir, Limit: limit, Offset: offset, ExcludeWorktrees: true})
	}

	t.Run("empty and missing roots", func(t *testing.T) {
		t.Parallel()
		if got := newLocalSessions(newProjectsRoot(t)).listSessions(nil); got != nil {
			t.Errorf("empty root: %v", got)
		}
		if got := newLocalSessions(filepath.Join(t.TempDir(), "nonexistent")).listSessions(nil); got != nil {
			t.Errorf("missing root: %v", got)
		}
	})

	t.Run("single session", func(t *testing.T) {
		t.Parallel()
		root := newProjectsRoot(t)
		path, canonical := newProject(t, "my-project")
		dir := makeProjectDir(t, root, canonical)
		sid := makeSessionFile(t, dir, sessionFile{firstPrompt: "What is 2+2?", gitBranch: "main", cwd: path})
		got := list(root, path, 0, 0)
		if len(got) != 1 {
			t.Fatalf("got %d sessions", len(got))
		}
		s := got[0]
		if s.SessionID != sid || s.FirstPrompt != "What is 2+2?" || s.Summary != "What is 2+2?" ||
			s.GitBranch != "main" || s.Cwd != path || s.FileSize <= 0 || s.LastModified <= 0 || s.CustomTitle != "" {
			t.Errorf("session = %+v", s)
		}
	})

	t.Run("title and summary precedence", func(t *testing.T) {
		t.Parallel()
		root := newProjectsRoot(t)
		path, canonical := newProject(t, "proj")
		dir := makeProjectDir(t, root, canonical)
		makeSessionFile(t, dir, sessionFile{firstPrompt: "original question", summary: "auto summary", customTitle: "My Custom Title", mtime: 2000})
		makeSessionFile(t, dir, sessionFile{firstPrompt: "question", summary: "better summary", mtime: 1000})
		got := list(root, path, 0, 0)
		if len(got) != 2 {
			t.Fatalf("got %d", len(got))
		}
		if got[0].Summary != "My Custom Title" || got[0].CustomTitle != "My Custom Title" || got[0].FirstPrompt != "original question" {
			t.Errorf("custom title session: %+v", got[0])
		}
		if got[1].Summary != "better summary" || got[1].CustomTitle != "" {
			t.Errorf("summary session: %+v", got[1])
		}
	})

	t.Run("sorting, limit and offset", func(t *testing.T) {
		t.Parallel()
		root := newProjectsRoot(t)
		path, canonical := newProject(t, "proj")
		dir := makeProjectDir(t, root, canonical)
		old := makeSessionFile(t, dir, sessionFile{firstPrompt: "old", mtime: 1000})
		newer := makeSessionFile(t, dir, sessionFile{firstPrompt: "new", mtime: 3000})
		mid := makeSessionFile(t, dir, sessionFile{firstPrompt: "mid", mtime: 2000})
		got := list(root, path, 0, 0)
		if ids := sessionIDs(got); !slices.Equal(ids, []string{newer, mid, old}) {
			t.Fatalf("order = %v", ids)
		}
		if got[0].LastModified != 3_000_000 || got[1].LastModified != 2_000_000 || got[2].LastModified != 1_000_000 {
			t.Errorf("mtimes = %d %d %d", got[0].LastModified, got[1].LastModified, got[2].LastModified)
		}
		if ids := sessionIDs(list(root, path, 2, 0)); !slices.Equal(ids, []string{newer, mid}) {
			t.Errorf("limit 2 = %v", ids)
		}
		if ids := sessionIDs(list(root, path, 2, 1)); !slices.Equal(ids, []string{mid, old}) {
			t.Errorf("limit 2 offset 1 = %v", ids)
		}
		if got := list(root, path, 0, 100); got != nil {
			t.Errorf("offset beyond end = %v", got)
		}
		if got := list(root, path, 0, 0); len(got) != 3 {
			t.Errorf("limit 0 = %d", len(got))
		}
		if got := list(root, path, -1, -1); len(got) != 3 {
			t.Errorf("negative limit/offset = %d", len(got))
		}
	})

	t.Run("filters", func(t *testing.T) {
		t.Parallel()
		root := newProjectsRoot(t)
		path, canonical := newProject(t, "proj")
		dir := makeProjectDir(t, root, canonical)
		makeSessionFile(t, dir, sessionFile{firstPrompt: "normal"})
		makeSessionFile(t, dir, sessionFile{firstPrompt: "sidechain", sidechain: true})
		makeSessionFile(t, dir, sessionFile{firstPrompt: "ignored meta", metaOnly: true})
		writeJSONL(t, filepath.Join(dir, "not-a-uuid.jsonl"), jsonObj("type", "user", "message", map[string]any{"content": "x"}))
		writeFile(t, filepath.Join(dir, "README.md"), "not a session")
		writeFile(t, filepath.Join(dir, randomUUID()+".jsonl"), "")
		got := list(root, path, 0, 0)
		if len(got) != 1 || got[0].FirstPrompt != "normal" {
			t.Errorf("got %+v", got)
		}
	})

	t.Run("all projects with dedupe", func(t *testing.T) {
		t.Parallel()
		root := newProjectsRoot(t)
		p1 := makeProjectDir(t, root, "/some/path/one")
		p2 := makeProjectDir(t, root, "/some/path/two")
		p3 := makeProjectDir(t, root, "/some/path/three")
		makeSessionFile(t, p1, sessionFile{firstPrompt: "from proj1", mtime: 1000})
		makeSessionFile(t, p2, sessionFile{firstPrompt: "from proj2", mtime: 2000})
		shared := randomUUID()
		makeSessionFile(t, p1, sessionFile{id: shared, firstPrompt: "older", mtime: 500})
		makeSessionFile(t, p3, sessionFile{id: shared, firstPrompt: "newer", mtime: 3000})
		writeFile(t, filepath.Join(root, "stray-file"), "x")
		got := newLocalSessions(root).listSessions(nil)
		if len(got) != 3 {
			t.Fatalf("got %d: %+v", len(got), got)
		}
		if got[0].FirstPrompt != "newer" || got[0].LastModified != 3_000_000 || got[0].Cwd != "" ||
			got[1].FirstPrompt != "from proj2" || got[2].FirstPrompt != "from proj1" {
			t.Errorf("got %+v", got)
		}
	})

	t.Run("nonexistent project and worktree isolation", func(t *testing.T) {
		t.Parallel()
		root := newProjectsRoot(t)
		path, canonical := newProject(t, "main-proj")
		if got := list(root, path, 0, 0); got != nil {
			t.Errorf("no project dir: %v", got)
		}
		makeSessionFile(t, makeProjectDir(t, root, canonical), sessionFile{firstPrompt: "main session"})
		makeSessionFile(t, makeProjectDir(t, root, canonical+"-worktree"), sessionFile{firstPrompt: "worktree session"})
		got := list(root, path, 0, 0)
		if len(got) != 1 || got[0].FirstPrompt != "main session" {
			t.Errorf("got %+v", got)
		}
	})

	t.Run("cwd and git branch", func(t *testing.T) {
		t.Parallel()
		root := newProjectsRoot(t)
		path, canonical := newProject(t, "proj")
		dir := makeProjectDir(t, root, canonical)
		makeSessionFile(t, dir, sessionFile{firstPrompt: "no cwd field"})
		got := list(root, path, 0, 0)
		if len(got) != 1 || got[0].Cwd != canonical {
			t.Errorf("cwd fallback: %+v", got)
		}

		root = newProjectsRoot(t)
		dir = makeProjectDir(t, root, canonical)
		writeJSONL(t, filepath.Join(dir, randomUUID()+".jsonl"),
			jsonObj("type", "user", "message", map[string]any{"content": "hello"}, "gitBranch", "old-branch"),
			jsonObj("type", "summary", "gitBranch", "new-branch"),
		)
		got = list(root, path, 0, 0)
		if len(got) != 1 || got[0].GitBranch != "new-branch" {
			t.Errorf("git branch: %+v", got)
		}
	})

	t.Run("ai title and last prompt", func(t *testing.T) {
		t.Parallel()
		root := newProjectsRoot(t)
		path, canonical := newProject(t, "proj")
		dir := makeProjectDir(t, root, canonical)
		writeJSONL(t, filepath.Join(dir, randomUUID()+".jsonl"),
			jsonObj("type", "user", "message", map[string]any{"content": "first"}),
			jsonObj("type", "last-prompt", "lastPrompt", "latest thing"),
		)
		writeJSONL(t, filepath.Join(dir, randomUUID()+".jsonl"),
			jsonObj("type", "user", "message", map[string]any{"content": "first"}),
			jsonObj("type", "ai-title", "aiTitle", "Generated"),
			jsonObj("type", "last-prompt", "lastPrompt", "latest thing"),
		)
		got := list(root, path, 0, 0)
		summaries := []string{got[0].Summary, got[1].Summary}
		slices.Sort(summaries)
		if !slices.Equal(summaries, []string{"Generated", "latest thing"}) {
			t.Errorf("summaries = %v", summaries)
		}
	})
}

func TestSessionTagExtraction(t *testing.T) {
	t.Parallel()
	user := jsonObj("type", "user", "message", map[string]any{"content": "hello"})
	toolUseTag := jsonObj("type", "assistant", "message", map[string]any{"content": []any{
		map[string]any{"type": "tool_use", "name": "mcp__docker__build", "input": map[string]any{"tag": "myapp:v2"}},
	}})
	tests := []struct {
		name  string
		lines []string
		want  string
	}{
		{"from tail", []string{user, jsonObj("type", "tag", "tag", "my-tag", "sessionId", "s")}, "my-tag"},
		{"last wins", []string{user, jsonObj("type", "tag", "tag", "first-tag"), jsonObj("type", "tag", "tag", "second-tag")}, "second-tag"},
		{"empty clears", []string{user, jsonObj("type", "tag", "tag", "old-tag"), jsonObj("type", "tag", "tag", "")}, ""},
		{"absent", []string{user}, ""},
		{"ignores tool_use inputs", []string{user, jsonObj("type", "tag", "tag", "real-tag"), toolUseTag}, "real-tag"},
		{"only tool_use tag", []string{user, toolUseTag}, ""},
		{"spaced tag line not matched", []string{user, `{"type": "tag", "tag": "spaced"}`}, ""},
	}
	for _, tt := range tests {
		root := newProjectsRoot(t)
		path, canonical := newProject(t, "proj")
		dir := makeProjectDir(t, root, canonical)
		sid := randomUUID()
		writeJSONL(t, filepath.Join(dir, sid+".jsonl"), tt.lines...)
		got := newLocalSessions(root).listSessions(&ListSessionsOptions{Directory: path, ExcludeWorktrees: true})
		if len(got) != 1 || got[0].Tag != tt.want {
			t.Errorf("%s: %+v", tt.name, got)
		}
		if info := newLocalSessions(root).getSessionInfo(sid, path); info == nil || info.Tag != tt.want {
			t.Errorf("%s: GetSessionInfo %+v", tt.name, info)
		}
	}
}

func TestSessionCreatedAt(t *testing.T) {
	t.Parallel()
	root := newProjectsRoot(t)
	path, canonical := newProject(t, "proj")
	dir := makeProjectDir(t, root, canonical)

	a := randomUUID()
	writeJSONL(t, filepath.Join(dir, a+".jsonl"),
		jsonObj("type", "user", "message", map[string]any{"content": "hello"}, "timestamp", "2026-01-15T10:30:00.000Z"),
		jsonObj("type", "assistant", "message", map[string]any{"content": "hi"}, "timestamp", "2026-01-15T10:35:00.000Z"),
	)
	b := randomUUID()
	writeJSONL(t, filepath.Join(dir, b+".jsonl"),
		jsonObj("type", "permission-mode", "permissionMode", "acceptEdits"),
		jsonObj("type", "user", "message", map[string]any{"content": "hello"}, "timestamp", "2026-01-15T10:30:00.000Z"),
	)
	c := makeSessionFile(t, dir, sessionFile{firstPrompt: "no timestamp"})
	d := randomUUID()
	writeJSONL(t, filepath.Join(dir, d+".jsonl"),
		jsonObj("type", "user", "message", map[string]any{"content": "hello"}, "timestamp", "2026-01-01T00:00:00.000Z"))
	setMTime(t, filepath.Join(dir, d+".jsonl"), 1769904000)

	byID := map[string]SessionInfo{}
	for _, s := range newLocalSessions(root).listSessions(&ListSessionsOptions{Directory: path, ExcludeWorktrees: true}) {
		byID[s.SessionID] = s
	}
	if byID[a].CreatedAt != 1768473000000 || byID[b].CreatedAt != 1768473000000 || byID[c].CreatedAt != 0 {
		t.Errorf("created_at: a=%d b=%d c=%d", byID[a].CreatedAt, byID[b].CreatedAt, byID[c].CreatedAt)
	}
	if s := byID[d]; s.CreatedAt == 0 || s.CreatedAt > s.LastModified {
		t.Errorf("created_at > last_modified: %+v", s)
	}
}

// ---------------------------------------------------------------------------
// GetSessionInfo
// ---------------------------------------------------------------------------

func TestSessionGetSessionInfo(t *testing.T) {
	t.Parallel()
	root := newProjectsRoot(t)
	for _, id := range []string{"not-a-uuid", "", randomUUID()} {
		if got := newLocalSessions(root).getSessionInfo(id, ""); got != nil {
			t.Errorf("getSessionInfo(%q) = %+v", id, got)
		}
	}
	if got := newLocalSessions(filepath.Join(t.TempDir(), "nonexistent")).getSessionInfo(randomUUID(), ""); got != nil {
		t.Errorf("missing root: %+v", got)
	}

	pathA, canonA := newProject(t, "proj-a")
	pathB, canonB := newProject(t, "proj-b")
	dirA := makeProjectDir(t, root, canonA)
	makeProjectDir(t, root, canonB)
	sid := makeSessionFile(t, dirA, sessionFile{firstPrompt: "hello", gitBranch: "main"})
	side := makeSessionFile(t, dirA, sessionFile{firstPrompt: "sidechain", sidechain: true})

	info := newLocalSessions(root).getSessionInfo(sid, pathA)
	if info == nil || info.SessionID != sid || info.Summary != "hello" || info.GitBranch != "main" || info.Cwd != canonA {
		t.Errorf("with directory: %+v", info)
	}
	if info := newLocalSessions(root).getSessionInfo(sid, ""); info == nil || info.Summary != "hello" || info.Cwd != "" {
		t.Errorf("without directory: %+v", info)
	}
	if info := newLocalSessions(root).getSessionInfo(sid, pathB); info != nil {
		t.Errorf("wrong directory: %+v", info)
	}
	if info := newLocalSessions(root).getSessionInfo(side, pathA); info != nil {
		t.Errorf("sidechain: %+v", info)
	}
}

// ---------------------------------------------------------------------------
// GetSessionMessages
// ---------------------------------------------------------------------------

func TestSessionGetSessionMessages(t *testing.T) {
	t.Parallel()

	setup := func(t *testing.T, lines ...string) (root, path, sid string) {
		t.Helper()
		root = newProjectsRoot(t)
		path, canonical := newProject(t, "proj")
		sid = randomUUID()
		dir := makeProjectDir(t, root, canonical)
		if lines != nil {
			writeJSONL(t, filepath.Join(dir, sid+".jsonl"), lines...)
		}
		return root, path, sid
	}
	get := func(root, path, sid string, limit, offset int) []SessionMessage {
		return newLocalSessions(root).getSessionMessages(sid, &SessionMessagesOptions{Directory: path, Limit: limit, Offset: offset})
	}

	t.Run("invalid and missing", func(t *testing.T) {
		t.Parallel()
		root := newProjectsRoot(t)
		for _, id := range []string{"not-a-uuid", "", randomUUID()} {
			if got := newLocalSessions(root).getSessionMessages(id, nil); got != nil {
				t.Errorf("%q: %v", id, got)
			}
		}
		if got := newLocalSessions(filepath.Join(t.TempDir(), "none")).getSessionMessages(randomUUID(), nil); got != nil {
			t.Errorf("missing root: %v", got)
		}
	})

	t.Run("simple chain", func(t *testing.T) {
		t.Parallel()
		u1, a1, u2, a2 := randomUUID(), randomUUID(), randomUUID(), randomUUID()
		root, path, sid := setup(t)
		dir := newLocalSessions(root).findProjectDir(canonicalizePath(path))
		writeJSONL(t, filepath.Join(dir, sid+".jsonl"),
			transcriptEntry("user", u1, "", sid, "hello"),
			transcriptEntry("assistant", a1, u1, sid, "hi!"),
			transcriptEntry("user", u2, a1, sid, "thanks"),
			transcriptEntry("assistant", a2, u2, sid, "welcome"),
		)
		msgs := get(root, path, sid, 0, 0)
		if ids := messageUUIDs(msgs); !slices.Equal(ids, []string{u1, a1, u2, a2}) {
			t.Fatalf("uuids = %v", ids)
		}
		m := msgs[0]
		if m.Type != "user" || m.SessionID != sid || m.ParentToolUseID != "" || m.ParentAgentID != "" ||
			!reflect.DeepEqual(m.Message, map[string]any{"role": "user", "content": "hello"}) {
			t.Errorf("first = %+v", m)
		}
		if msgs[1].Type != "assistant" || !reflect.DeepEqual(msgs[1].Message, map[string]any{"role": "assistant", "content": "hi!"}) {
			t.Errorf("second = %+v", msgs[1])
		}
	})

	t.Run("filters meta and non-message entries", func(t *testing.T) {
		t.Parallel()
		u1, meta, prog, a1 := randomUUID(), randomUUID(), randomUUID(), randomUUID()
		root, path, sid := setup(t, "")
		dir := newLocalSessions(root).findProjectDir(canonicalizePath(path))
		writeJSONL(t, filepath.Join(dir, sid+".jsonl"),
			transcriptEntry("user", u1, "", sid, "hello"),
			transcriptEntry("user", meta, u1, sid, "meta", "isMeta", true),
			transcriptEntry("progress", prog, meta, sid, nil),
			jsonObj("type", "summary", "summary", "A nice chat"),
			transcriptEntry("assistant", a1, prog, sid, "hi"),
		)
		if ids := messageUUIDs(get(root, path, sid, 0, 0)); !slices.Equal(ids, []string{u1, a1}) {
			t.Errorf("uuids = %v", ids)
		}
	})

	t.Run("keeps compact summary", func(t *testing.T) {
		t.Parallel()
		u1, a1 := randomUUID(), randomUUID()
		root, path, sid := setup(t, "")
		dir := newLocalSessions(root).findProjectDir(canonicalizePath(path))
		writeJSONL(t, filepath.Join(dir, sid+".jsonl"),
			transcriptEntry("user", u1, "", sid, "compact summary", "isCompactSummary", true),
			transcriptEntry("assistant", a1, u1, sid, "hi"),
		)
		if ids := messageUUIDs(get(root, path, sid, 0, 0)); !slices.Equal(ids, []string{u1, a1}) {
			t.Errorf("uuids = %v", ids)
		}
	})

	t.Run("limit and offset", func(t *testing.T) {
		t.Parallel()
		root, path, sid := setup(t, "")
		dir := newLocalSessions(root).findProjectDir(canonicalizePath(path))
		var uuids, lines []string
		for i := range 6 {
			uid := randomUUID()
			parent := ""
			if i > 0 {
				parent = uuids[i-1]
			}
			typ := "user"
			if i%2 == 1 {
				typ = "assistant"
			}
			uuids = append(uuids, uid)
			lines = append(lines, transcriptEntry(typ, uid, parent, sid, fmt.Sprintf("m%d", i)))
		}
		writeJSONL(t, filepath.Join(dir, sid+".jsonl"), lines...)
		tests := []struct {
			limit, offset int
			want          []string
		}{
			{0, 0, uuids},
			{2, 0, uuids[:2]},
			{2, 2, uuids[2:4]},
			{0, 4, uuids[4:]},
			{0, 100, nil},
		}
		for _, tt := range tests {
			if got := messageUUIDs(get(root, path, sid, tt.limit, tt.offset)); !slices.Equal(got, tt.want) && len(got)+len(tt.want) > 0 {
				t.Errorf("limit=%d offset=%d: %v, want %v", tt.limit, tt.offset, got, tt.want)
			}
		}
		if got := get(root, path, sid, 0, 100); got != nil {
			t.Errorf("offset beyond end should be nil: %v", got)
		}
	})

	t.Run("leaf selection", func(t *testing.T) {
		t.Parallel()
		root, path, sid := setup(t, "")
		dir := newLocalSessions(root).findProjectDir(canonicalizePath(path))
		r, mainLeaf, sideLeaf := randomUUID(), randomUUID(), randomUUID()
		writeJSONL(t, filepath.Join(dir, sid+".jsonl"),
			transcriptEntry("user", r, "", sid, "root"),
			transcriptEntry("assistant", mainLeaf, r, sid, "main"),
			transcriptEntry("assistant", sideLeaf, r, sid, "side", "isSidechain", true),
		)
		if ids := messageUUIDs(get(root, path, sid, 0, 0)); !slices.Equal(ids, []string{r, mainLeaf}) {
			t.Errorf("main over sidechain: %v", ids)
		}

		oldLeaf, newLeaf := randomUUID(), randomUUID()
		writeJSONL(t, filepath.Join(dir, sid+".jsonl"),
			transcriptEntry("user", r, "", sid, "root"),
			transcriptEntry("assistant", oldLeaf, r, sid, "old"),
			transcriptEntry("assistant", newLeaf, r, sid, "new"),
		)
		if ids := messageUUIDs(get(root, path, sid, 0, 0)); !slices.Equal(ids, []string{r, newLeaf}) {
			t.Errorf("latest leaf: %v", ids)
		}

		u1, a1, prog := randomUUID(), randomUUID(), randomUUID()
		writeJSONL(t, filepath.Join(dir, sid+".jsonl"),
			transcriptEntry("user", u1, "", sid, "hi"),
			transcriptEntry("assistant", a1, u1, sid, "hello"),
			transcriptEntry("progress", prog, a1, sid, nil),
		)
		if ids := messageUUIDs(get(root, path, sid, 0, 0)); !slices.Equal(ids, []string{u1, a1}) {
			t.Errorf("terminal progress walked back: %v", ids)
		}

		// Only sidechain/team leaves: fall back to all leaves.
		t1, t2 := randomUUID(), randomUUID()
		writeJSONL(t, filepath.Join(dir, sid+".jsonl"),
			transcriptEntry("user", t1, "", sid, "team", "teamName", "x"),
			transcriptEntry("assistant", t2, t1, sid, "team reply", "teamName", "x"),
		)
		if got := get(root, path, sid, 0, 0); got != nil {
			t.Errorf("team-only chain should yield no visible messages: %v", got)
		}
	})

	t.Run("corrupt lines, cycles and empty files", func(t *testing.T) {
		t.Parallel()
		root, path, sid := setup(t, "")
		dir := newLocalSessions(root).findProjectDir(canonicalizePath(path))
		u1, a1 := randomUUID(), randomUUID()
		writeJSONL(t, filepath.Join(dir, sid+".jsonl"),
			transcriptEntry("user", u1, "", sid, "hi"),
			"not valid json {{{",
			"",
			"[1,2,3]",
			"   "+transcriptEntry("assistant", a1, u1, sid, "hello")+"\t\r",
		)
		if ids := messageUUIDs(get(root, path, sid, 0, 0)); !slices.Equal(ids, []string{u1, a1}) {
			t.Errorf("corrupt lines: %v", ids)
		}
		// Trailing non-JSON whitespace (U+00A0) makes the line unparseable,
		// as for JSON.parse.
		writeJSONL(t, filepath.Join(dir, sid+".jsonl"),
			transcriptEntry("user", u1, "", sid, "hi"),
			transcriptEntry("assistant", a1, u1, sid, "hello")+"\u00a0",
		)
		if ids := messageUUIDs(get(root, path, sid, 0, 0)); !slices.Equal(ids, []string{u1}) {
			t.Errorf("nbsp line: %v", ids)
		}

		writeJSONL(t, filepath.Join(dir, sid+".jsonl"),
			transcriptEntry("user", u1, a1, sid, "hi"),
			transcriptEntry("assistant", a1, u1, sid, "hello"),
		)
		if got := get(root, path, sid, 0, 0); got != nil {
			t.Errorf("cycle: %v", got)
		}

		writeFile(t, filepath.Join(dir, sid+".jsonl"), "")
		if got := get(root, path, sid, 0, 0); got != nil {
			t.Errorf("empty: %v", got)
		}
	})

	t.Run("searches all projects without directory", func(t *testing.T) {
		t.Parallel()
		root := newProjectsRoot(t)
		makeProjectDir(t, root, "/path/one")
		p2 := makeProjectDir(t, root, "/path/two")
		sid, u1, a1 := randomUUID(), randomUUID(), randomUUID()
		// An empty copy in the first project is skipped.
		writeFile(t, filepath.Join(root, sanitizePath("/path/one"), sid+".jsonl"), "")
		writeJSONL(t, filepath.Join(p2, sid+".jsonl"),
			transcriptEntry("user", u1, "", sid, "hi"),
			transcriptEntry("assistant", a1, u1, sid, "hello"),
		)
		if ids := messageUUIDs(newLocalSessions(root).getSessionMessages(sid, nil)); !slices.Equal(ids, []string{u1, a1}) {
			t.Errorf("uuids = %v", ids)
		}
	})
}

func TestSessionBuildConversationChain(t *testing.T) {
	t.Parallel()
	if got := buildConversationChain(nil).chain; got != nil {
		t.Errorf("empty: %v", got)
	}
	single := map[string]any{"type": "user", "uuid": "a", "parentUuid": nil}
	if got := buildConversationChain([]map[string]any{single}).chain; len(got) != 1 || got[0]["uuid"] != "a" {
		t.Errorf("single: %v", got)
	}
	linear := []map[string]any{
		{"type": "user", "uuid": "a", "parentUuid": nil},
		{"type": "assistant", "uuid": "b", "parentUuid": "a"},
		{"type": "user", "uuid": "c", "parentUuid": "b"},
	}
	var ids []string
	for _, e := range buildConversationChain(linear).chain {
		ids = append(ids, e["uuid"].(string))
	}
	if !slices.Equal(ids, []string{"a", "b", "c"}) {
		t.Errorf("linear: %v", ids)
	}
	progress := []map[string]any{
		{"type": "progress", "uuid": "a", "parentUuid": nil},
		{"type": "progress", "uuid": "b", "parentUuid": "a"},
	}
	if got := buildConversationChain(progress).chain; got != nil {
		t.Errorf("progress only: %v", got)
	}
}

// ---------------------------------------------------------------------------
// Subagents
// ---------------------------------------------------------------------------

// makeSessionWithSubagents creates a session with a subagents directory.
func makeSessionWithSubagents(t *testing.T, root, canonical string, agentIDs ...string) (sid, subagentsDir string) {
	t.Helper()
	dir := makeProjectDir(t, root, canonical)
	sid = makeSessionFile(t, dir, sessionFile{})
	subagentsDir = filepath.Join(dir, sid, "subagents")
	if err := os.MkdirAll(subagentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, id := range agentIDs {
		writeJSONL(t, filepath.Join(subagentsDir, "agent-"+id+".jsonl"), jsonObj("type", "user", "uuid", "u", "parentUuid", nil))
	}
	return sid, subagentsDir
}

func TestSessionListSubagents(t *testing.T) {
	t.Parallel()
	root := newProjectsRoot(t)
	for _, id := range []string{"not-a-uuid", "", randomUUID()} {
		if got := newLocalSessions(root).listSubagents(id, ""); got != nil {
			t.Errorf("%q: %v", id, got)
		}
	}

	path, canonical := newProject(t, "proj")
	plain := makeSessionFile(t, makeProjectDir(t, root, canonical), sessionFile{})
	if got := newLocalSessions(root).listSubagents(plain, path); got != nil {
		t.Errorf("no subagents dir: %v", got)
	}
	empty, _ := makeSessionWithSubagents(t, root, canonical)
	if got := newLocalSessions(root).listSubagents(empty, path); got != nil {
		t.Errorf("empty subagents dir: %v", got)
	}

	sid, sub := makeSessionWithSubagents(t, root, canonical, "def456", "abc123")
	if got := newLocalSessions(root).listSubagents(sid, path); !slices.Equal(got, []string{"abc123", "def456"}) {
		t.Errorf("happy path: %v", got)
	}
	writeFile(t, filepath.Join(sub, "agent-abc123.meta.json"), "{}")
	writeFile(t, filepath.Join(sub, "other.jsonl"), "{}\n")
	writeFile(t, filepath.Join(sub, "agent-noext"), "{}")
	if err := os.MkdirAll(filepath.Join(sub, "agent-dir.jsonl"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(sub, "workflows", "run-1", "agent-nested.jsonl"), "{}\n")
	if got := newLocalSessions(root).listSubagents(sid, path); !slices.Equal(sortedCopy(got), []string{"abc123", "def456", "nested"}) {
		t.Errorf("filters and recursion: %v", got)
	}

	// Without a directory every project is searched.
	root2 := newProjectsRoot(t)
	dir := makeProjectDir(t, root2, "/some/project")
	sid2 := makeSessionFile(t, dir, sessionFile{})
	writeFile(t, filepath.Join(dir, sid2, "subagents", "agent-x.jsonl"), "{}\n")
	if got := newLocalSessions(root2).listSubagents(sid2, ""); !slices.Equal(got, []string{"x"}) {
		t.Errorf("all projects: %v", got)
	}
}

func TestSessionGetSubagentMessages(t *testing.T) {
	t.Parallel()
	root := newProjectsRoot(t)
	path, canonical := newProject(t, "proj")
	opts := &SessionMessagesOptions{Directory: path}

	for _, tt := range []struct{ sid, agent string }{{"not-a-uuid", "abc"}, {"", "abc"}, {randomUUID(), ""}, {randomUUID(), "abc"}} {
		if got := newLocalSessions(root).getSubagentMessages(tt.sid, tt.agent, nil); got != nil {
			t.Errorf("%q/%q: %v", tt.sid, tt.agent, got)
		}
	}
	other, _ := makeSessionWithSubagents(t, root, canonical, "other")
	if got := newLocalSessions(root).getSubagentMessages(other, "missing", opts); got != nil {
		t.Errorf("missing agent: %v", got)
	}

	writeAgent := func(dir, agentID, sid string, meta any) (string, string) {
		u1, a1 := randomUUID(), randomUUID()
		writeJSONL(t, filepath.Join(dir, "agent-"+agentID+".jsonl"),
			transcriptEntry("user", u1, "", sid, "hi"),
			transcriptEntry("assistant", a1, u1, sid, "hello"),
		)
		switch m := meta.(type) {
		case nil:
		case string:
			writeFile(t, filepath.Join(dir, "agent-"+agentID+".meta.json"), m)
		default:
			b, _ := jsonx.Marshal(m)
			writeFile(t, filepath.Join(dir, "agent-"+agentID+".meta.json"), string(b))
		}
		return u1, a1
	}

	t.Run("simple chain", func(t *testing.T) {
		sid, sub := makeSessionWithSubagents(t, root, canonical)
		var uuids, lines []string
		for i, typ := range []string{"user", "assistant", "user", "assistant"} {
			uid := randomUUID()
			parent := ""
			if i > 0 {
				parent = uuids[i-1]
			}
			uuids = append(uuids, uid)
			lines = append(lines, transcriptEntry(typ, uid, parent, sid, fmt.Sprintf("m%d", i)))
		}
		writeJSONL(t, filepath.Join(sub, "agent-abc.jsonl"), lines...)
		msgs := newLocalSessions(root).getSubagentMessages(sid, "abc", opts)
		if ids := messageUUIDs(msgs); !slices.Equal(ids, uuids) {
			t.Fatalf("uuids = %v", ids)
		}
		if msgs[0].Type != "user" || msgs[0].SessionID != sid || msgs[0].ParentToolUseID != "" || msgs[3].Type != "assistant" ||
			!reflect.DeepEqual(msgs[0].Message, map[string]any{"role": "user", "content": "m0"}) {
			t.Errorf("msgs = %+v", msgs)
		}
		for _, tt := range []struct {
			limit, offset int
			want          []string
		}{{2, 0, uuids[:2]}, {2, 2, uuids[2:]}, {0, 3, uuids[3:]}, {0, 0, uuids}} {
			got := newLocalSessions(root).getSubagentMessages(sid, "abc", &SessionMessagesOptions{Directory: path, Limit: tt.limit, Offset: tt.offset})
			if ids := messageUUIDs(got); !slices.Equal(ids, tt.want) {
				t.Errorf("limit=%d offset=%d: %v", tt.limit, tt.offset, ids)
			}
		}
	})

	t.Run("nested agent and sidecar", func(t *testing.T) {
		sid, sub := makeSessionWithSubagents(t, root, canonical)
		nested := filepath.Join(sub, "workflows", "run-1")
		u1, a1 := writeAgent(nested, "deep", sid, map[string]any{"toolUseId": "toolu_nested"})
		msgs := newLocalSessions(root).getSubagentMessages(sid, "deep", opts)
		if ids := messageUUIDs(msgs); !slices.Equal(ids, []string{u1, a1}) {
			t.Fatalf("uuids = %v", ids)
		}
		for _, m := range msgs {
			if m.ParentToolUseID != "toolu_nested" || m.ParentAgentID != "" {
				t.Errorf("parent ids: %+v", m)
			}
		}
		writeAgent(sub, "abc", sid, map[string]any{
			"agentType": "general-purpose", "toolUseId": "toolu_01ABC", "parentAgentId": "a-parent", "spawnDepth": 2,
		})
		for _, m := range newLocalSessions(root).getSubagentMessages(sid, "abc", opts) {
			if m.ParentToolUseID != "toolu_01ABC" || m.ParentAgentID != "a-parent" {
				t.Errorf("parent ids: %+v", m)
			}
		}
	})

	t.Run("unusable sidecars", func(t *testing.T) {
		for i, meta := range []any{nil, "not json {", map[string]any{"agentType": "gp"}, map[string]any{"toolUseId": 42, "parentAgentId": []any{"x"}}, "[1]", "\xff\xfe"} {
			sid, sub := makeSessionWithSubagents(t, root, canonical)
			writeAgent(sub, "x", sid, meta)
			msgs := newLocalSessions(root).getSubagentMessages(sid, "x", opts)
			if len(msgs) != 2 || msgs[0].ParentToolUseID != "" || msgs[0].ParentAgentID != "" {
				t.Errorf("case %d: %+v", i, msgs)
			}
		}
		// A directory in place of the sidecar is a read error, which the
		// best-effort reader swallows.
		sid, sub := makeSessionWithSubagents(t, root, canonical)
		writeAgent(sub, "x", sid, nil)
		if err := os.Mkdir(filepath.Join(sub, "agent-x.meta.json"), 0o755); err != nil {
			t.Fatal(err)
		}
		if msgs := newLocalSessions(root).getSubagentMessages(sid, "x", opts); len(msgs) != 2 || msgs[0].ParentToolUseID != "" {
			t.Errorf("directory sidecar: %+v", msgs)
		}
		if _, err := readAgentMetadataSidecar(filepath.Join(sub, "agent-x.jsonl")); err == nil {
			t.Error("readAgentMetadataSidecar should report a directory sidecar")
		}
	})

	t.Run("corrupt and empty transcripts", func(t *testing.T) {
		sid, sub := makeSessionWithSubagents(t, root, canonical)
		u1, a1 := randomUUID(), randomUUID()
		writeJSONL(t, filepath.Join(sub, "agent-x.jsonl"),
			transcriptEntry("user", u1, "", sid, "hi"), "not valid json {", "",
			transcriptEntry("assistant", a1, u1, sid, "ok"))
		if ids := messageUUIDs(newLocalSessions(root).getSubagentMessages(sid, "x", opts)); !slices.Equal(ids, []string{u1, a1}) {
			t.Errorf("corrupt: %v", ids)
		}
		writeFile(t, filepath.Join(sub, "agent-empty.jsonl"), "")
		if got := newLocalSessions(root).getSubagentMessages(sid, "empty", opts); got != nil {
			t.Errorf("empty: %v", got)
		}
	})
}

func TestSessionAgentMetadataHelpers(t *testing.T) {
	t.Parallel()
	if got := agentMetadataSidecarPath(filepath.Join("a", "agent-x.jsonl")); got != filepath.Join("a", "agent-x.meta.json") {
		t.Errorf("sidecar path = %q", got)
	}
	meta, transcript := splitAgentMetadata([]SessionStoreEntry{
		{"type": "agent_metadata", "toolUseId": "old"},
		{"type": "user", "uuid": "u"},
		{"type": "agent_metadata", "toolUseId": "new"},
	})
	if meta["toolUseId"] != "new" || len(transcript) != 1 || transcript[0]["uuid"] != "u" {
		t.Errorf("split = %v, %v", meta, transcript)
	}
	if meta, _ := splitAgentMetadata([]SessionStoreEntry{{"type": "user"}}); meta != nil {
		t.Errorf("no metadata: %v", meta)
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "agent-y.jsonl")
	if m, err := readAgentMetadataSidecar(p); m != nil || err != nil {
		t.Errorf("missing sidecar: %v, %v", m, err)
	}
	writeFile(t, agentMetadataSidecarPath(p), `{"toolUseId":"t"}`)
	if m, err := readAgentMetadataSidecar(p); err != nil || m["toolUseId"] != "t" {
		t.Errorf("sidecar: %v, %v", m, err)
	}
}

// ---------------------------------------------------------------------------
// Git worktrees
// ---------------------------------------------------------------------------

func TestSessionWorktrees(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	wt := filepath.Join(base, "repo-wt")
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "init.defaultBranch=main"}, args...)...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git(repo, "init", "-q")
	git(repo, "commit", "-q", "--allow-empty", "-m", "init")
	git(repo, "worktree", "add", "-q", "-b", "feature", wt)

	repoCanon, wtCanon := canonicalizePath(repo), canonicalizePath(wt)
	paths := getWorktreePaths(repoCanon)
	if !slices.Contains(paths, repoCanon) || !slices.Contains(paths, wtCanon) {
		t.Fatalf("worktrees = %v, want %s and %s", paths, repoCanon, wtCanon)
	}
	if got := getWorktreePaths(base); got != nil {
		t.Errorf("outside a repo: %v", got)
	}
	if got := getWorktreePaths(filepath.Join(base, "missing")); got != nil {
		t.Errorf("missing dir: %v", got)
	}

	root := newProjectsRoot(t)
	mainSID := makeSessionFile(t, makeProjectDir(t, root, repoCanon), sessionFile{firstPrompt: "main", mtime: 1000})
	wtSID := makeSessionFile(t, makeProjectDir(t, root, wtCanon), sessionFile{firstPrompt: "worktree", mtime: 2000})
	// A sibling whose name merely extends the repo's must not match.
	makeSessionFile(t, makeProjectDir(t, root, repoCanon+"-other"), sessionFile{firstPrompt: "unrelated"})
	// A subdirectory of the repo is always included.
	sub := filepath.Join(repo, "packages", "app")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	subSID := makeSessionFile(t, makeProjectDir(t, root, canonicalizePath(sub)), sessionFile{firstPrompt: "sub", mtime: 3000})

	got := newLocalSessions(root).listSessions(&ListSessionsOptions{Directory: repo})
	if ids := sessionIDs(got); !slices.Equal(ids, []string{wtSID, mainSID}) {
		t.Errorf("with worktrees = %v (want %v)", ids, []string{wtSID, mainSID})
	}
	if got[0].Cwd != wtCanon || got[1].Cwd != repoCanon {
		t.Errorf("cwd fallbacks = %q, %q", got[0].Cwd, got[1].Cwd)
	}
	got = newLocalSessions(root).listSessions(&ListSessionsOptions{Directory: sub})
	if ids := sessionIDs(got); !slices.Equal(ids, []string{subSID, wtSID, mainSID}) {
		t.Errorf("from subdirectory = %v", ids)
	}
	if ids := sessionIDs(newLocalSessions(root).listSessions(&ListSessionsOptions{Directory: repo, ExcludeWorktrees: true})); !slices.Equal(ids, []string{mainSID}) {
		t.Errorf("excluding worktrees = %v", ids)
	}

	// Single-session readers fall back to the other worktrees.
	if info := newLocalSessions(root).getSessionInfo(wtSID, repo); info == nil || info.Cwd != wtCanon {
		t.Errorf("GetSessionInfo via worktree: %+v", info)
	}
	if p := newLocalSessions(root).resolveSessionFilePath(wtSID, repo); p == "" {
		t.Error("resolveSessionFilePath via worktree")
	}
	chainSID, u1 := randomUUID(), randomUUID()
	writeJSONL(t, filepath.Join(root, sanitizePath(wtCanon), chainSID+".jsonl"), transcriptEntry("user", u1, "", chainSID, "in worktree"))
	if msgs := newLocalSessions(root).getSessionMessages(chainSID, &SessionMessagesOptions{Directory: repo}); len(msgs) != 1 || msgs[0].UUID != u1 {
		t.Errorf("GetSessionMessages via worktree: %+v", msgs)
	}
}

// ---------------------------------------------------------------------------
// Public wrappers (process environment)
// ---------------------------------------------------------------------------

func TestSessionPublicLocalAPI(t *testing.T) {
	config := filepath.Join(t.TempDir(), "cfg")
	t.Setenv("CLAUDE_CONFIG_DIR", config)
	root := filepath.Join(config, "projects")
	path, canonical := newProject(t, "proj")
	sid, sub := makeSessionWithSubagents(t, root, canonical, "a1")
	u1 := randomUUID()
	writeJSONL(t, filepath.Join(sub, "agent-a1.jsonl"), transcriptEntry("user", u1, "", sid, "sub hi"))
	main := filepath.Join(root, sanitizePath(canonical), sid+".jsonl")
	m1 := randomUUID()
	writeJSONL(t, main, transcriptEntry("user", m1, "", sid, "Hello Claude"))

	infos, err := ListSessions(nil)
	if err != nil || len(infos) != 1 || infos[0].SessionID != sid {
		t.Fatalf("ListSessions = %+v, %v", infos, err)
	}
	if infos, err := ListSessions(&ListSessionsOptions{Directory: path, ExcludeWorktrees: true}); err != nil || len(infos) != 1 {
		t.Errorf("ListSessions(dir) = %+v, %v", infos, err)
	}
	if info, err := GetSessionInfo(sid, path); err != nil || info == nil || info.Summary != "Hello Claude" {
		t.Errorf("GetSessionInfo = %+v, %v", info, err)
	}
	if info, err := GetSessionInfo(randomUUID(), ""); err != nil || info != nil {
		t.Errorf("GetSessionInfo(missing) = %+v, %v", info, err)
	}
	if msgs, err := GetSessionMessages(sid, nil); err != nil || len(msgs) != 1 || msgs[0].UUID != m1 {
		t.Errorf("GetSessionMessages = %+v, %v", msgs, err)
	}
	if ids, err := ListSubagents(sid, ""); err != nil || !slices.Equal(ids, []string{"a1"}) {
		t.Errorf("ListSubagents = %v, %v", ids, err)
	}
	if msgs, err := GetSubagentMessages(sid, "a1", &SessionMessagesOptions{Directory: path}); err != nil || len(msgs) != 1 || msgs[0].UUID != u1 {
		t.Errorf("GetSubagentMessages = %+v, %v", msgs, err)
	}
	if got := ProjectKeyForDirectory(path); got != sanitizePath(canonical) {
		t.Errorf("ProjectKeyForDirectory = %q", got)
	}
	wd, _ := os.Getwd()
	if got := ProjectKeyForDirectory(""); got != sanitizePath(canonicalizePath(wd)) {
		t.Errorf("ProjectKeyForDirectory(\"\") = %q", got)
	}
}
