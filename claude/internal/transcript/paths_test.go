package transcript

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ironpark/gelati/claude/internal/unicodenorm"
)

func TestSimpleHashMatchesPython(t *testing.T) {
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

func TestSanitizePath(t *testing.T) {
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
		if got := SanitizePath(tt.in); got != tt.want {
			t.Errorf("SanitizePath(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
	// Long paths: 200-char prefix + "-" + hash (values from Python).
	long := []struct{ in, suffix string }{
		{strings.Repeat("/x", 150), "x-x-x-4y6yx2"},
		{"/tmp/" + strings.Repeat("deep/", 60), "deep--pqvxy1"},
		{strings.Repeat("/Users/é", 40), "ers---cibw94"},
	}
	for _, tt := range long {
		got := SanitizePath(tt.in)
		if len(got) != 207 || got[195:] != tt.suffix || got[200] != '-' {
			t.Errorf("SanitizePath(long %.12q) = ...%q (len %d), want suffix %q", tt.in, got[195:], len(got), tt.suffix)
		}
	}
	if got := SanitizePath(strings.Repeat("a", 200)); got != strings.Repeat("a", 200) {
		t.Errorf("200-char name was truncated: %q", got)
	}
}

func TestCanonicalizePath(t *testing.T) {
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
		if got := CanonicalizePath(tt.in); got != tt.want {
			t.Errorf("CanonicalizePath(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
	// Symlink loops stop resolution instead of hanging.
	loop := filepath.Join(base, "loop")
	if err := os.Symlink(loop, loop); err != nil {
		t.Fatal(err)
	}
	if got := CanonicalizePath(filepath.Join(loop, "x")); !strings.HasSuffix(got, "/loop/x") {
		t.Errorf("loop: got %q", got)
	}
	// Relative paths resolve against the working directory.
	wd, _ := os.Getwd()
	if got, want := CanonicalizePath("."), CanonicalizePath(wd); got != want {
		t.Errorf("CanonicalizePath(.) = %q, want %q", got, want)
	}
}

func TestProjectsDir(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "/from/env")
	if got := ProjectsDir(nil); got != filepath.Join("/from/env", "projects") {
		t.Errorf("ProjectsDir(nil) = %q", got)
	}
	if got := ProjectsDir(map[string]string{"CLAUDE_CONFIG_DIR": "/override-cafe\u0301"}); got != filepath.Join("/override-caf\u00e9", "projects") {
		t.Errorf("ProjectsDir(override) = %q", got)
	}
	if got := ProjectsDir(map[string]string{"OTHER": "x"}); got != filepath.Join("/from/env", "projects") {
		t.Errorf("ProjectsDir(unrelated override) = %q", got)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip(err)
	}
	if got := ProjectsDir(nil); got != unicodenorm.NFC(filepath.Join(home, ".claude", "projects")) {
		t.Errorf("projectsDir default = %q", got)
	}
}

func TestProjectDirNameOverrideAndKey(t *testing.T) {
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
		if got := ProjectDirNameOverride(tt.getenv); got != tt.want {
			t.Errorf("%d: override = %q", i, got)
		}
		wantKey := tt.want
		if wantKey == "" {
			wantKey = SanitizePath(CanonicalizePath("/some/dir"))
		}
		if got := ProjectKey("/some/dir", tt.getenv); got != wantKey {
			t.Errorf("%d: key = %q, want %q", i, got, wantKey)
		}
	}
}

func TestAgentMetadataHelpers(t *testing.T) {
	t.Parallel()
	if got := AgentMetadataSidecarPath(filepath.Join("a", "agent-x.jsonl")); got != filepath.Join("a", "agent-x.meta.json") {
		t.Errorf("sidecar path = %q", got)
	}
	meta, rest := SplitAgentMetadata([]map[string]any{
		{"type": "agent_metadata", "toolUseId": "old"},
		{"type": "user", "uuid": "u"},
		{"type": "agent_metadata", "toolUseId": "new"},
	})
	if meta["toolUseId"] != "new" || len(rest) != 1 || rest[0]["uuid"] != "u" {
		t.Errorf("split = %v, %v", meta, rest)
	}
	if meta, _ := SplitAgentMetadata([]map[string]any{{"type": "user"}}); meta != nil {
		t.Errorf("no metadata: %v", meta)
	}
}
