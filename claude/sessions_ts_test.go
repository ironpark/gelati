package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Unit tests for the behavior aligned with the TypeScript SDK. They run
// without node; sessions_parity_test.go cross-checks the same code paths
// against the TS runtime.

func TestSessionPromptFromUserEntry(t *testing.T) {
	t.Parallel()
	user := func(content any, extra ...any) map[string]any {
		e := map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": content}}
		for i := 0; i < len(extra); i += 2 {
			e[extra[i].(string)] = extra[i+1]
		}
		return e
	}
	text := func(s string) map[string]any { return map[string]any{"type": "text", "text": s} }
	tests := []struct {
		name         string
		entry        map[string]any
		prompt       string
		ok           bool
		fallbackFrom string
	}{
		{"plain", user("  hello\nworld  "), "hello world", true, ""},
		{"blocks", user([]any{text("<ide_opened_file>x</ide_opened_file>"), text("real")}), "real", true, ""},
		{"tool result", user([]any{text("hi"), map[string]any{"type": "tool_result"}}), "", false, ""},
		{"meta", user("hi", "isMeta", true), "", false, ""},
		{"compact summary", user("hi", "isCompactSummary", true), "", false, ""},
		{"not user", map[string]any{"type": "assistant", "message": map[string]any{"content": "hi"}}, "", false, ""},
		{"command", user("<command-name>/init</command-name>"), "", false, "/init"},
		{"bash", user("<bash-input>  ls -la </bash-input>"), "! ls -la", true, ""},
		{"any lowercase tag", user("  <system-reminder>x</system-reminder>"), "", false, ""},
		{"uppercase tag kept", user("<B>bold</B>"), "<B>bold</B>", true, ""},
		{"interrupt", user("[Request interrupted by user for tool use]"), "", false, ""},
		{"pasted", user("see:\n<pasted_content id=\"0a1b\">\nbody\n</pasted_content id=\"0a1b\">\nend"), "see:bodyend", true, ""}, // surrounding newlines are dropped
		{"truncated", user(strings.Repeat("é", 250)), strings.Repeat("é", 200) + "\u2026", true, ""},
		{"surrogate boundary", user(strings.Repeat("a", 199) + "😀" + strings.Repeat("b", 10)), strings.Repeat("a", 199) + "\u2026", true, ""},
	}
	for _, tt := range tests {
		fallback := ""
		prompt, ok := promptFromUserEntry(tt.entry, &fallback)
		if prompt != tt.prompt || ok != tt.ok || fallback != tt.fallbackFrom {
			t.Errorf("%s: got (%q, %v, fallback %q)", tt.name, prompt, ok, fallback)
		}
	}
}

func TestSessionUnwrapPastedContent(t *testing.T) {
	t.Parallel()
	tests := []struct{ in, want string }{
		{"no paste", "no paste"},
		{"", ""},
		{"a\n\n<pasted_content id=\"abcd\">\nX\nY\n</pasted_content id=\"abcd\">\n\nb", "aX\nYb"},
		{"<pasted_content id=\"abcd\">\n\n</pasted_content id=\"abcd\">", ""},
		{"<pasted_content id=\"ABCD\">\nX\n</pasted_content id=\"ABCD\">", "<pasted_content id=\"ABCD\">\nX\n</pasted_content id=\"ABCD\">"},
		{"<pasted_content id=\"abcd\">\nunclosed", "<pasted_content id=\"abcd\">\nunclosed"},
	}
	for _, tt := range tests {
		if got := unwrapPastedContent(tt.in); got != tt.want {
			t.Errorf("unwrapPastedContent(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestSessionProgrammaticDetection(t *testing.T) {
	t.Parallel()
	tests := []struct {
		head, tail string
		want       bool
	}{
		{`{"type":"user","entrypoint":"cli"}`, "", false},
		{`{"type":"user","entrypoint":"sdk-ts"}`, "", true},
		{`{"type":"user","entrypoint":"sdk-go"}`, "", true},
		{`{"type":"user","entrypoint":"sdk-go-client"}`, "", true},
		{`{"type":"user","entrypoint":"sdk-py-client"}`, "", true},
		{`{"type":"user"}`, `{"entrypoint":"sdk-cli"}`, true},
		{`{"type":"user","entrypoint":"cli"}`, `{"entrypoint":"sdk-cli"}`, false}, // the head wins
		{`{"type":"mode","sessionKind":"daemon"}` + "\n" + `{"parentUuid":null,"sessionKind":"interactive"}`, "", false},
		{`{"parentUuid":null,"sessionKind":"daemon-worker"}`, "", true},
	}
	for _, tt := range tests {
		if got := isProgrammaticSession(tt.head, tt.tail); got != tt.want {
			t.Errorf("isProgrammaticSession(%q, %q) = %v", tt.head, tt.tail, got)
		}
	}
}

func TestSessionContinuedInSessionID(t *testing.T) {
	t.Parallel()
	next := "11111111-1111-4111-8111-111111111111"
	marker := `{"type":"continued-in","continuedInSessionId":"` + next + `"}`
	tests := []struct {
		name, tail, want string
	}{
		{"none", `{"type":"user"}`, ""},
		{"last", `{"type":"assistant","message":{"stop_reason":"end_turn"}}` + "\n" + marker, next},
		{"reply after", marker + "\n" + `{"type":"assistant","message":{"stop_reason":"end_turn"}}`, ""},
		{"prompt after", marker + "\n" + `{"type":"user","message":{"content":"more"}}`, ""},
		{"unfinished reply after", marker + "\n" + `{"type":"assistant","message":{"stop_reason":null}}`, next},
		{"api error after", marker + "\n" + `{"type":"assistant","isApiErrorMessage":true,"message":{"stop_reason":"x"}}`, next},
		{"not a uuid", `{"type":"continued-in","continuedInSessionId":"nope"}`, ""},
	}
	for _, tt := range tests {
		if got := continuedInSessionID(tt.tail); got != tt.want {
			t.Errorf("%s: %q", tt.name, got)
		}
	}
}

func TestSessionSidecarTitle(t *testing.T) {
	t.Parallel()
	tests := []struct{ in, want string }{
		{"  plain  ", "plain"},
		{"a\u0007\u0008b\tc\u200bd", "a b c d"},
		{strings.Repeat("x", 250), strings.Repeat("x", 200)},
		{"\u2028", ""},
	}
	for _, tt := range tests {
		if got := sanitizeSidecarTitle(tt.in); got != tt.want {
			t.Errorf("sanitizeSidecarTitle(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
	dir := t.TempDir()
	sid := "11111111-1111-4111-8111-111111111111"
	transcript := filepath.Join(dir, sid+".jsonl")
	for _, tt := range []struct{ content, want string }{
		{`{"customTitle":" Named "}`, "Named"},
		{`{"customTitle":5}`, ""},
		{`not json`, ""},
	} {
		writeFile(t, filepath.Join(dir, sid, "custom-title.json"), tt.content)
		if got := readCustomTitleSidecar(transcript, sid); got != tt.want {
			t.Errorf("sidecar %s = %q", tt.content, got)
		}
	}
}

func TestSessionProjectDirNameOverride(t *testing.T) {
	t.Parallel()
	env := func(kv ...string) func(string) string {
		m := map[string]string{}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return func(k string) string { return m[k] }
	}
	tests := []struct {
		getenv func(string) string
		want   string
	}{
		{env(), ""},
		{env("CLAUDE_CODE_PROJECT_DIR_NAME", "proj"), ""}, // needs CLAUDE_CONFIG_DIR
		{env("CLAUDE_CONFIG_DIR", "/c", "CLAUDE_CODE_PROJECT_DIR_NAME", "proj_1-x"), "proj_1-x"},
		{env("CLAUDE_CONFIG_DIR", "/c", "CLAUDE_CODE_PROJECT_DIR_NAME", "a/b"), ""},
		{env("CLAUDE_CONFIG_DIR", "/c", "CLAUDE_CODE_PROJECT_DIR_NAME", "COM1"), ""},
		{env("CLAUDE_CONFIG_DIR", "/c", "CLAUDE_CODE_PROJECT_DIR_NAME", strings.Repeat("a", 65)), ""},
	}
	for i, tt := range tests {
		if got := projectDirNameOverrideFromEnv(tt.getenv); got != tt.want {
			t.Errorf("%d: override = %q", i, got)
		}
		wantKey := tt.want
		if wantKey == "" {
			wantKey = sanitizePath(canonicalizePath("/some/dir"))
		}
		if got := projectKeyForDirectory("/some/dir", tt.getenv); got != wantKey {
			t.Errorf("%d: key = %q, want %q", i, got, wantKey)
		}
	}

	root := t.TempDir()
	project := "/work/app"
	if dirs := findProjectDirsWith(root, project, "named"); dirs != nil {
		t.Errorf("none exist: %v", dirs)
	}
	named := filepath.Join(root, "named")
	sanitized := filepath.Join(root, sanitizePath(project))
	for _, d := range []string{named, sanitized} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if dirs := findProjectDirsWith(root, project, "named"); !slices.Equal(dirs, []string{named, sanitized}) {
		t.Errorf("override dirs = %v", dirs)
	}
	if dirs := findProjectDirsWith(root, project, ""); !slices.Equal(dirs, []string{sanitized}) {
		t.Errorf("default dirs = %v", dirs)
	}
}

func TestSessionSkipPrecompactLines(t *testing.T) {
	t.Parallel()
	boundary := func(meta map[string]any) string {
		return jsonObj("type", "system", "subtype", "compact_boundary", "uuid", "b", "compactMetadata", meta)
	}
	snap := func(id string) string { return jsonObj("type", "attribution-snapshot", "id", id) }
	lateMarker := jsonObj("type", "system", "pad", strings.Repeat("p", 300), "subtype", "compact_boundary")
	tests := []struct {
		name  string
		lines []string
		want  []string
	}{
		{"no boundary", []string{"a", snap("1"), "b"}, []string{"a", "b", snap("1")}},
		{"cut", []string{"a", snap("1"), boundary(map[string]any{}), "c", snap("2"), "d"},
			[]string{boundary(map[string]any{}), "c", "d", snap("2")}},
		{"snapshot before cut dropped", []string{snap("1"), boundary(nil), "c"}, []string{boundary(nil), "c"}},
		{"preserving boundary kept whole", []string{"a", boundary(map[string]any{"preservedSegment": map[string]any{"headUuid": "x"}}), "c"},
			[]string{"a", boundary(map[string]any{"preservedSegment": map[string]any{"headUuid": "x"}}), "c"}},
		{"marker beyond 256 bytes", []string{"a", lateMarker, "c"}, []string{"a", lateMarker, "c"}},
	}
	for _, tt := range tests {
		in := strings.Join(tt.lines, "\n") + "\n"
		want := strings.Join(tt.want, "\n") + "\n"
		if got := string(skipPrecompactLines([]byte(in))); got != want {
			t.Errorf("%s:\ngot  %q\nwant %q", tt.name, got, want)
		}
	}
	// A final unterminated boundary line is not detected.
	in := "a\n" + boundary(nil)
	if got := string(skipPrecompactLines([]byte(in))); got != in {
		t.Errorf("unterminated: %q", got)
	}
}

func TestSessionEnvTruthy(t *testing.T) {
	t.Parallel()
	for v, want := range map[string]bool{"1": true, "true": true, " YES ": true, "on": true, "0": false, "": false, "off": false, "2": false} {
		if got := envTruthy(v); got != want {
			t.Errorf("envTruthy(%q) = %v", v, got)
		}
		if got := precompactSkipEnabled(func(string) string { return v }); got == want {
			t.Errorf("precompactSkipEnabled(%q) = %v", v, got)
		}
	}
}

func TestSessionReadTranscriptPrecompactSkip(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "s.jsonl")
	head := jsonObj("type", "user", "pad", strings.Repeat("x", precompactSkipThreshold)) + "\n"
	tail := jsonObj("type", "system", "subtype", "compact_boundary") + "\nafter\n"
	writeFile(t, path, head+tail)
	size := int64(len(head + tail))
	if got := string(readTranscript(path, size, true)); got != tail {
		t.Errorf("skip: %.80q", got)
	}
	if got := readTranscript(path, size, false); len(got) != int(size) {
		t.Errorf("opt-out: %d bytes", len(got))
	}
	if got := readTranscript(path, 10, true); string(got) != (head + tail)[:10] {
		t.Errorf("size bound: %q", got)
	}
}

// chainEntry builds a transcript entry for chain tests.
func chainEntry(typ, uuid, parent string, extra ...any) map[string]any {
	e := map[string]any{"type": typ, "uuid": uuid, "sessionId": "s", "timestamp": "2025-01-01T00:00:0" + uuid[len(uuid)-1:] + "Z"}
	if parent != "" {
		e["parentUuid"] = parent
	} else {
		e["parentUuid"] = nil
	}
	if typ == "user" || typ == "assistant" {
		e["message"] = map[string]any{"role": typ, "content": uuid}
	}
	for i := 0; i < len(extra); i += 2 {
		e[extra[i].(string)] = extra[i+1]
	}
	return e
}

func messageKinds(msgs []SessionMessage) []string {
	var out []string
	for _, m := range msgs {
		k := m.Type + ":" + m.UUID
		if m.IsQueuedCommand {
			k += "+q"
		}
		if m.IsMeta {
			k += "+meta"
		}
		if m.IsCompletedLocalCommand {
			k += "+local"
		}
		out = append(out, k)
	}
	return out
}

func TestSessionMessagesFromParsed(t *testing.T) {
	t.Parallel()
	q := func(prompt string, extra ...any) map[string]any {
		m := map[string]any{"type": "queued_command", "prompt": prompt}
		for i := 0; i < len(extra); i += 2 {
			m[extra[i].(string)] = extra[i+1]
		}
		return m
	}
	tests := []struct {
		name          string
		entries       []map[string]any
		includeSystem bool
		want          []string
	}{
		{
			name: "preserved segment relinked",
			entries: []map[string]any{
				chainEntry("user", "u1", ""), chainEntry("assistant", "a1", "u1"),
				chainEntry("system", "b1", "", "subtype", "compact_boundary",
					"compactMetadata", map[string]any{"preservedSegment": map[string]any{"headUuid": "u1", "anchorUuid": "s1", "tailUuid": "a1"}}),
				chainEntry("user", "s1", "b1", "isCompactSummary", true),
				chainEntry("user", "u2", "s1"), chainEntry("assistant", "a2", "u2"),
			},
			includeSystem: true,
			want:          []string{"system:b1", "user:s1+meta", "user:u1", "assistant:a1", "user:u2", "assistant:a2"},
		},
		{
			name: "preserved messages relinked, system hidden",
			entries: []map[string]any{
				chainEntry("user", "u1", ""), chainEntry("assistant", "a1", "u1"),
				chainEntry("system", "b1", "", "subtype", "compact_boundary",
					"compactMetadata", map[string]any{"preservedMessages": map[string]any{"anchorUuid": "s1", "uuids": []any{"u1", "a1"}}}),
				chainEntry("user", "s1", "b1", "isCompactSummary", true),
				chainEntry("user", "u2", "s1"),
			},
			want: []string{"user:s1+meta", "user:u1", "assistant:a1", "user:u2"},
		},
		{
			name: "queued commands",
			entries: []map[string]any{
				chainEntry("user", "u1", ""),
				chainEntry("attachment", "q1", "u1", "attachment", q("mid")),
				chainEntry("assistant", "a1", "q1"),
				chainEntry("attachment", "q2", "a1", "attachment", q("trailing")),
				chainEntry("attachment", "q3", "a1", "attachment", q("meta", "isMeta", true)),
				chainEntry("attachment", "q4", "a1", "attachment", q("peer", "isMeta", true, "origin", map[string]any{"kind": "peer"})),
			},
			want: []string{"user:u1", "user:q1+q", "assistant:a1", "user:q2+q", "user:q4+q+meta"},
		},
		{
			name: "absorbed copies render once",
			entries: []map[string]any{
				chainEntry("user", "u1", ""),
				chainEntry("attachment", "q1", "u1", "attachment", q("x", "source_uuid", "src")),
				chainEntry("attachment", "q2", "q1", "attachment", q("x", "source_uuid", "src")),
				{"type": "queue-operation", "operation": "remove", "reason": "absorbed_mid_turn", "commandUuid": "src"},
				chainEntry("assistant", "a1", "q2"),
			},
			want: []string{"user:u1", "user:src+q", "assistant:a1"},
		},
		{
			name: "local command and meta",
			entries: []map[string]any{
				chainEntry("user", "c1", "", "isMeta", true, "message", map[string]any{"content": "<local-command-caveat>c</local-command-caveat>"}),
				chainEntry("user", "c2", "c1", "message", map[string]any{"content": "<command-name>/x</command-name>"}),
				chainEntry("user", "c3", "c2", "message", map[string]any{"content": "<local-command-stdout>o</local-command-stdout>"}),
				chainEntry("user", "m1", "c3", "isMeta", true),
				chainEntry("user", "m2", "m1", "isMeta", true, "origin", map[string]any{"kind": "channel"}),
			},
			want: []string{"user:c2+local", "user:c3+local", "user:m2+meta"},
		},
	}
	for _, tt := range tests {
		got := messageKinds(sessionMessagesFromParsed(tt.entries, tt.includeSystem))
		if !slices.Equal(got, tt.want) {
			t.Errorf("%s:\ngot  %v\nwant %v", tt.name, got, tt.want)
		}
	}
}

func TestSessionListExcludeProgrammatic(t *testing.T) {
	t.Parallel()
	root := newProjectsRoot(t)
	_, canonical := newProject(t, "proj")
	dir := makeProjectDir(t, root, canonical)
	var ids []string
	for _, ep := range []string{"cli", "sdk-go", "sdk-go-client", "sdk-ts"} {
		sid := newUUID(t)
		writeJSONL(t, filepath.Join(dir, sid+".jsonl"), jsonObj("type", "user", "entrypoint", ep, "message", map[string]any{"content": ep}))
		ids = append(ids, sid)
	}
	all := listSessionsIn(root, &ListSessionsOptions{Directory: canonical})
	interactive := listSessionsIn(root, &ListSessionsOptions{Directory: canonical, ExcludeProgrammatic: true})
	if len(all) != 4 || len(interactive) != 1 || interactive[0].SessionID != ids[0] {
		t.Errorf("all = %v, interactive = %v", sessionIDs(all), sessionIDs(interactive))
	}
}

func TestSessionJSONLByteSize(t *testing.T) {
	t.Parallel()
	entries := []SessionStoreEntry{
		{"type": "user", "text": "<a> & é \u2028"},
		{"n": 1.5},
	}
	var want int64
	for _, e := range entries {
		b, _ := json.Marshal(e)
		want += int64(len(b)) + 1
	}
	// Go escapes <, >, & (3 x 5 extra bytes) and U+2028 (3 extra bytes);
	// JSON.stringify writes them raw.
	want -= 3*5 + 3
	if got := jsonlByteSize(entries); got != want {
		t.Errorf("jsonlByteSize = %d, want %d", got, want)
	}
}

func TestSessionIsForeignSession(t *testing.T) {
	t.Parallel()
	work := canonicalizePath(t.TempDir())
	project := filepath.Join(work, "a-b")
	other := filepath.Join(work, "a", "b")
	for _, d := range []string{project, other} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		name, cwd string
		own       []string
		want      bool
	}{
		{"same dir", project, nil, false},
		{"colliding dir", other, nil, true},
		{"inside own worktree", other, []string{filepath.Join(work, "a")}, false},
		{"missing dir", filepath.Join(work, "a", "b-c", ".."), nil, false},
		{"unrelated", filepath.Join(work, "zzz"), nil, false},
	}
	for _, tt := range tests {
		if got := isForeignSession(tt.cwd, project, tt.own); got != tt.want {
			t.Errorf("%s: %v", tt.name, got)
		}
	}
}
