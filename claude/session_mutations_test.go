package claude

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/ironpark/gelati/internal/jsonx"
)

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// makeTranscriptSession mirrors the Python tests' _make_transcript_session:
// numTurns user/assistant pairs linked by parentUuid.
func makeTranscriptSession(t *testing.T, projectDir string, numTurns int) (sid, path string, uuids []string) {
	t.Helper()
	sid = randomUUID()
	var lines []string
	parent := ""
	for i := range numTurns {
		u, a := randomUUID(), randomUUID()
		lines = append(lines,
			transcriptEntry("user", u, parent, sid, fmt.Sprintf("Turn %d question", i+1), "timestamp", "2026-03-01T00:00:00Z"),
			transcriptEntry("assistant", a, u, sid, []any{map[string]any{"type": "text", "text": fmt.Sprintf("Turn %d answer", i+1)}}, "timestamp", "2026-03-01T00:00:00Z"),
		)
		uuids = append(uuids, u, a)
		parent = a
	}
	path = filepath.Join(projectDir, sid+".jsonl")
	writeJSONL(t, path, lines...)
	return sid, path, uuids
}

// readJSONLines returns the non-empty lines of a file.
func readJSONLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// readEntries parses every line of a JSONL file.
func readEntries(t *testing.T, path string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range readJSONLines(t, path) {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func lastEntry(t *testing.T, path string) map[string]any {
	t.Helper()
	entries := readEntries(t, path)
	return entries[len(entries)-1]
}

// mutationProject sets up a projects root and a real project directory with
// its transcript directory.
func mutationProject(t *testing.T) (root, project, projectDir string) {
	t.Helper()
	root = newProjectsRoot(t)
	project, canonical := newProject(t, "proj")
	return root, project, makeProjectDir(t, root, canonical)
}

func fileContent(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// ---------------------------------------------------------------------------
// tryAppend
// ---------------------------------------------------------------------------

func TestSessionTryAppend(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	f := filepath.Join(dir, "test.jsonl")
	writeFile(t, f, "line1\n")
	if ok, err := tryAppend(f, "line2\n"); !ok || err != nil {
		t.Fatalf("existing file: %v, %v", ok, err)
	}
	if ok, err := tryAppend(f, "line3\n"); !ok || err != nil {
		t.Fatalf("second append: %v, %v", ok, err)
	}
	if got := fileContent(t, f); got != "line1\nline2\nline3\n" {
		t.Errorf("content = %q", got)
	}

	missing := filepath.Join(dir, "nonexistent.jsonl")
	if ok, err := tryAppend(missing, "data\n"); ok || err != nil {
		t.Errorf("missing file: %v, %v", ok, err)
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing file was created: %v", err)
	}
	if ok, err := tryAppend(filepath.Join(dir, "nonexistent", "file.jsonl"), "data\n"); ok || err != nil {
		t.Errorf("missing parent: %v, %v", ok, err)
	}
	if ok, err := tryAppend(filepath.Join(f, "child.jsonl"), "data\n"); ok || err != nil {
		t.Errorf("parent is a file (ENOTDIR): %v, %v", ok, err)
	}

	stub := filepath.Join(dir, "stub.jsonl")
	writeFile(t, stub, "")
	if ok, err := tryAppend(stub, "data\n"); ok || err != nil {
		t.Errorf("0-byte stub: %v, %v", ok, err)
	}
	if got := fileContent(t, stub); got != "" {
		t.Errorf("stub modified: %q", got)
	}

	// Other errors surface.
	sub := filepath.Join(dir, "adir.jsonl")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if ok, err := tryAppend(sub, "data\n"); ok || err == nil {
		t.Errorf("directory: %v, %v", ok, err)
	}
}

// ---------------------------------------------------------------------------
// RenameSession
// ---------------------------------------------------------------------------

func TestSessionRename(t *testing.T) {
	t.Parallel()

	t.Run("invalid session id", func(t *testing.T) {
		t.Parallel()
		root := newProjectsRoot(t)
		for _, sid := range []string{"not-a-uuid", ""} {
			err := newLocalSessions(root).renameSession(sid, "title", "")
			if !errors.Is(err, ErrInvalidSessionID) {
				t.Errorf("%q: err = %v", sid, err)
			}
		}
	})

	t.Run("empty title", func(t *testing.T) {
		t.Parallel()
		root, project, dir := mutationProject(t)
		sid := makeSessionFile(t, dir, sessionFile{})
		before := fileContent(t, filepath.Join(dir, sid+".jsonl"))
		for _, title := range []string{"", "   ", "\n\t", "\u3000"} {
			err := newLocalSessions(root).renameSession(sid, title, project)
			if err == nil || !strings.Contains(err.Error(), "title must be non-empty") {
				t.Errorf("%q: err = %v", title, err)
			}
		}
		if fileContent(t, filepath.Join(dir, sid+".jsonl")) != before {
			t.Error("file modified")
		}
	})

	t.Run("not found", func(t *testing.T) {
		t.Parallel()
		root, project, _ := mutationProject(t)
		err := newLocalSessions(root).renameSession(randomUUID(), "title", project)
		if !errors.Is(err, ErrSessionNotFound) || !strings.Contains(err.Error(), "in project directory for") {
			t.Errorf("err = %v", err)
		}
		if err := newLocalSessions(root).renameSession(randomUUID(), "title", ""); !errors.Is(err, ErrSessionNotFound) {
			t.Errorf("all projects: err = %v", err)
		}
	})

	t.Run("no projects directory", func(t *testing.T) {
		t.Parallel()
		err := newLocalSessions(filepath.Join(t.TempDir(), "nonexistent")).renameSession(randomUUID(), "title", "")
		if !errors.Is(err, ErrSessionNotFound) || !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), "no projects directory") {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("appends compact custom-title entry", func(t *testing.T) {
		t.Parallel()
		root, project, dir := mutationProject(t)
		sid := makeSessionFile(t, dir, sessionFile{})
		path := filepath.Join(dir, sid+".jsonl")
		before := fileContent(t, path)
		if err := newLocalSessions(root).renameSession(sid, "My New Title", project); err != nil {
			t.Fatal(err)
		}
		want := `{"type":"custom-title","customTitle":"My New Title","sessionId":"` + sid + `"}` + "\n"
		if got := fileContent(t, path); got != before+want {
			t.Errorf("content = %q, want suffix %q", got, want)
		}
	})

	t.Run("trimmed and ASCII-escaped", func(t *testing.T) {
		t.Parallel()
		root, project, dir := mutationProject(t)
		sid := makeSessionFile(t, dir, sessionFile{})
		path := filepath.Join(dir, sid+".jsonl")
		if err := newLocalSessions(root).renameSession(sid, "  caf\u00e9 \U0001F600 <&>  ", project); err != nil {
			t.Fatal(err)
		}
		// Python: json.dumps(..., separators=(",", ":")) with ensure_ascii.
		want := `{"type":"custom-title","customTitle":"caf\u00e9 \ud83d\ude00 <&>","sessionId":"` + sid + `"}`
		if lines := readJSONLines(t, path); lines[len(lines)-1] != want {
			t.Errorf("line = %s\nwant   %s", lines[len(lines)-1], want)
		}
		if got := lastEntry(t, path)["customTitle"]; got != "caf\u00e9 \U0001F600 <&>" {
			t.Errorf("customTitle = %q", got)
		}
	})

	t.Run("last wins via ListSessions", func(t *testing.T) {
		t.Parallel()
		root, project, dir := mutationProject(t)
		sid := makeSessionFile(t, dir, sessionFile{firstPrompt: "original"})
		for _, title := range []string{"First Title", "Second Title", "Final Title"} {
			if err := newLocalSessions(root).renameSession(sid, title, project); err != nil {
				t.Fatal(err)
			}
		}
		infos := newLocalSessions(root).listSessions(&ListSessionsOptions{Directory: project, ExcludeWorktrees: true})
		if len(infos) != 1 || infos[0].CustomTitle != "Final Title" || infos[0].Summary != "Final Title" {
			t.Errorf("infos = %+v", infos)
		}
	})

	t.Run("searches all projects", func(t *testing.T) {
		t.Parallel()
		root := newProjectsRoot(t)
		dir := makeProjectDir(t, root, "/some/project")
		sid := makeSessionFile(t, dir, sessionFile{})
		if err := newLocalSessions(root).renameSession(sid, "Found Without Dir", ""); err != nil {
			t.Fatal(err)
		}
		if got := lastEntry(t, filepath.Join(dir, sid+".jsonl"))["customTitle"]; got != "Found Without Dir" {
			t.Errorf("customTitle = %v", got)
		}
	})

	t.Run("skips zero-byte stub", func(t *testing.T) {
		t.Parallel()
		root := newProjectsRoot(t)
		projA := makeProjectDir(t, root, "/aaa/project")
		projZ := makeProjectDir(t, root, "/zzz/project")
		sid := randomUUID()
		writeFile(t, filepath.Join(projA, sid+".jsonl"), "")
		makeSessionFile(t, projZ, sessionFile{id: sid, firstPrompt: "real"})
		if err := newLocalSessions(root).renameSession(sid, "New Title", ""); err != nil {
			t.Fatal(err)
		}
		if got := fileContent(t, filepath.Join(projA, sid+".jsonl")); got != "" {
			t.Errorf("stub = %q", got)
		}
		if got := fileContent(t, filepath.Join(projZ, sid+".jsonl")); !strings.Contains(got, `"customTitle":"New Title"`) {
			t.Errorf("real file = %q", got)
		}
	})
}

func TestSessionMutationsWorktreeFallback(t *testing.T) {
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

	root := newProjectsRoot(t)
	makeProjectDir(t, root, canonicalizePath(repo)) // exists, but without the session
	wtDir := makeProjectDir(t, root, canonicalizePath(wt))
	sid, path, _ := makeTranscriptSession(t, wtDir, 1)

	if err := newLocalSessions(root).renameSession(sid, "via worktree", repo); err != nil {
		t.Fatal(err)
	}
	if err := newLocalSessions(root).tagSession(sid, "wt", repo); err != nil {
		t.Fatal(err)
	}
	entries := readEntries(t, path)
	if n := len(entries); entries[n-2]["customTitle"] != "via worktree" || entries[n-1]["tag"] != "wt" {
		t.Errorf("entries = %v", entries[n-2:])
	}
	res, err := newLocalSessions(root).forkSession(sid, &ForkSessionOptions{Directory: repo})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(wtDir, res.SessionID+".jsonl")); err != nil {
		t.Errorf("fork not written next to the source: %v", err)
	}
	if err := newLocalSessions(root).deleteSession(sid, repo); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("not deleted: %v", err)
	}
}

// ---------------------------------------------------------------------------
// TagSession
// ---------------------------------------------------------------------------

func TestSessionTag(t *testing.T) {
	t.Parallel()

	t.Run("invalid session id", func(t *testing.T) {
		t.Parallel()
		root := newProjectsRoot(t)
		for _, sid := range []string{"not-a-uuid", ""} {
			if err := newLocalSessions(root).tagSession(sid, "tag", ""); !errors.Is(err, ErrInvalidSessionID) {
				t.Errorf("%q: err = %v", sid, err)
			}
		}
	})

	t.Run("blank or invisible tag rejected", func(t *testing.T) {
		t.Parallel()
		root, project, dir := mutationProject(t)
		sid := makeSessionFile(t, dir, sessionFile{})
		for _, tag := range []string{"   ", "\u200b\u200c\ufeff", " \u200b "} {
			if err := newLocalSessions(root).tagSession(sid, tag, project); err == nil || !strings.Contains(err.Error(), "tag must be non-empty") {
				t.Errorf("%q: err = %v", tag, err)
			}
		}
	})

	t.Run("not found", func(t *testing.T) {
		t.Parallel()
		root, project, _ := mutationProject(t)
		if err := newLocalSessions(root).tagSession(randomUUID(), "tag", project); !errors.Is(err, ErrSessionNotFound) {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("appends compact tag entries, last wins", func(t *testing.T) {
		t.Parallel()
		root, project, dir := mutationProject(t)
		sid := makeSessionFile(t, dir, sessionFile{})
		path := filepath.Join(dir, sid+".jsonl")
		if err := newLocalSessions(root).tagSession(sid, "mytag", project); err != nil {
			t.Fatal(err)
		}
		lines := readJSONLines(t, path)
		if want := `{"type":"tag","tag":"mytag","sessionId":"` + sid + `"}`; lines[len(lines)-1] != want {
			t.Errorf("line = %s", lines[len(lines)-1])
		}
		for _, tag := range []string{"first", "  second  ", "third"} {
			if err := newLocalSessions(root).tagSession(sid, tag, project); err != nil {
				t.Fatal(err)
			}
		}
		var tags []any
		for _, e := range readEntries(t, path) {
			if e["type"] == "tag" {
				tags = append(tags, e["tag"])
			}
		}
		if !slices.Equal(tags, []any{"mytag", "first", "second", "third"}) {
			t.Errorf("tags = %v", tags)
		}
		infos := newLocalSessions(root).listSessions(&ListSessionsOptions{Directory: project, ExcludeWorktrees: true})
		if len(infos) != 1 || infos[0].Tag != "third" {
			t.Errorf("infos = %+v", infos)
		}
	})

	t.Run("empty tag clears", func(t *testing.T) {
		t.Parallel()
		root, project, dir := mutationProject(t)
		sid := makeSessionFile(t, dir, sessionFile{})
		path := filepath.Join(dir, sid+".jsonl")
		if err := newLocalSessions(root).tagSession(sid, "original-tag", project); err != nil {
			t.Fatal(err)
		}
		if err := newLocalSessions(root).tagSession(sid, "", project); err != nil {
			t.Fatal(err)
		}
		lines := readJSONLines(t, path)
		if want := `{"type":"tag","tag":"","sessionId":"` + sid + `"}`; lines[len(lines)-1] != want {
			t.Errorf("line = %s", lines[len(lines)-1])
		}
		infos := newLocalSessions(root).listSessions(&ListSessionsOptions{Directory: project, ExcludeWorktrees: true})
		if len(infos) != 1 || infos[0].Tag != "" {
			t.Errorf("infos = %+v", infos)
		}
	})

	t.Run("sanitized", func(t *testing.T) {
		t.Parallel()
		root, project, dir := mutationProject(t)
		sid := makeSessionFile(t, dir, sessionFile{})
		if err := newLocalSessions(root).tagSession(sid, "clean\u200btag\ufeff", project); err != nil {
			t.Fatal(err)
		}
		if got := lastEntry(t, filepath.Join(dir, sid+".jsonl"))["tag"]; got != "cleantag" {
			t.Errorf("tag = %q", got)
		}
	})
}

// ---------------------------------------------------------------------------
// DeleteSession
// ---------------------------------------------------------------------------

func TestSessionDelete(t *testing.T) {
	t.Parallel()

	t.Run("invalid and missing", func(t *testing.T) {
		t.Parallel()
		root := newProjectsRoot(t)
		if err := newLocalSessions(root).deleteSession("not-a-uuid", ""); !errors.Is(err, ErrInvalidSessionID) {
			t.Errorf("invalid: %v", err)
		}
		if err := newLocalSessions(root).deleteSession(randomUUID(), ""); !errors.Is(err, ErrSessionNotFound) {
			t.Errorf("missing: %v", err)
		}
		dir := makeProjectDir(t, root, "/stub/project")
		stub := randomUUID()
		writeFile(t, filepath.Join(dir, stub+".jsonl"), "")
		if err := newLocalSessions(root).deleteSession(stub, ""); !errors.Is(err, ErrSessionNotFound) {
			t.Errorf("0-byte stub: %v", err)
		}
	})

	t.Run("deletes file and subagent directory", func(t *testing.T) {
		t.Parallel()
		root, project, dir := mutationProject(t)
		sid := makeSessionFile(t, dir, sessionFile{})
		path := filepath.Join(dir, sid+".jsonl")
		subDir := filepath.Join(dir, sid)
		writeFile(t, filepath.Join(subDir, "subagents", "agent-a.jsonl"), "{}\n")
		other := makeSessionFile(t, dir, sessionFile{})

		if infos := newLocalSessions(root).listSessions(&ListSessionsOptions{Directory: project}); len(infos) != 2 {
			t.Fatalf("before: %+v", infos)
		}
		if err := newLocalSessions(root).deleteSession(sid, project); err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{path, subDir} {
			if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("%s still exists: %v", p, err)
			}
		}
		if ids := sessionIDs(newLocalSessions(root).listSessions(&ListSessionsOptions{Directory: project})); !slices.Equal(ids, []string{other}) {
			t.Errorf("after: %v", ids)
		}
	})

	t.Run("without directory", func(t *testing.T) {
		t.Parallel()
		root := newProjectsRoot(t)
		dir := makeProjectDir(t, root, "/any/project")
		sid := makeSessionFile(t, dir, sessionFile{})
		if err := newLocalSessions(root).deleteSession(sid, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(dir, sid+".jsonl")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("still exists: %v", err)
		}
	})

	t.Run("only a real subagent directory is removed", func(t *testing.T) {
		t.Parallel()
		root := newProjectsRoot(t)
		dir := makeProjectDir(t, root, "/any/project")
		sid := makeSessionFile(t, dir, sessionFile{})
		sibling := filepath.Join(dir, sid)
		writeFile(t, sibling, "not a directory")
		if err := newLocalSessions(root).deleteSession(sid, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(sibling); err != nil {
			t.Errorf("file named like the session removed: %v", err)
		}

		sid2 := makeSessionFile(t, dir, sessionFile{})
		target := filepath.Join(t.TempDir(), "target")
		writeFile(t, filepath.Join(target, "keep.jsonl"), "{}\n")
		if err := os.Symlink(target, filepath.Join(dir, sid2)); err != nil {
			t.Skip("symlinks unsupported:", err)
		}
		if err := newLocalSessions(root).deleteSession(sid2, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(target, "keep.jsonl")); err != nil {
			t.Errorf("symlink target contents removed: %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// ForkSession
// ---------------------------------------------------------------------------

var uuidLike = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestSessionFork(t *testing.T) {
	t.Parallel()

	t.Run("validation", func(t *testing.T) {
		t.Parallel()
		root := newProjectsRoot(t)
		if _, err := newLocalSessions(root).forkSession("not-a-uuid", nil); !errors.Is(err, ErrInvalidSessionID) {
			t.Errorf("invalid id: %v", err)
		}
		if _, err := newLocalSessions(root).forkSession(randomUUID(), nil); !errors.Is(err, ErrSessionNotFound) {
			t.Errorf("missing: %v", err)
		}
		_, err := newLocalSessions(root).forkSession(randomUUID(), &ForkSessionOptions{UpToMessageID: "not-valid"})
		if err == nil || errors.Is(err, ErrInvalidSessionID) || errors.Is(err, ErrSessionNotFound) || !strings.Contains(err.Error(), "not-valid") {
			t.Errorf("invalid up-to: %v", err)
		}
	})

	t.Run("creates remapped copy", func(t *testing.T) {
		t.Parallel()
		root, project, dir := mutationProject(t)
		sid, srcPath, orig := makeTranscriptSession(t, dir, 3)
		srcBefore := fileContent(t, srcPath)

		res, err := newLocalSessions(root).forkSession(sid, &ForkSessionOptions{Directory: project})
		if err != nil {
			t.Fatal(err)
		}
		if res.SessionID == sid || !uuidLike.MatchString(res.SessionID) {
			t.Fatalf("fork id = %q", res.SessionID)
		}
		forkPath := filepath.Join(dir, res.SessionID+".jsonl")
		if fi, err := os.Stat(forkPath); err != nil || (fi.Mode().Perm() != 0o600 && os.PathSeparator == '/') {
			t.Errorf("fork file: %v, %v", fi, err)
		}
		if fileContent(t, srcPath) != srcBefore {
			t.Error("source modified")
		}

		entries := readEntries(t, forkPath)
		if len(entries) != 7 {
			t.Fatalf("entries = %d", len(entries))
		}
		var prev any
		for i, e := range entries[:6] {
			if slices.Contains(orig, e["uuid"].(string)) || e["parentUuid"] != prev {
				t.Errorf("entry %d uuid/parent = %v / %v", i, e["uuid"], e["parentUuid"])
			}
			prev = e["uuid"]
			if e["sessionId"] != res.SessionID || e["isSidechain"] != false {
				t.Errorf("entry %d = %v", i, e)
			}
			if v, ok := e["logicalParentUuid"]; !ok || v != nil {
				t.Errorf("entry %d logicalParentUuid = %v, %v", i, v, ok)
			}
			ff := e["forkedFrom"].(map[string]any)
			if ff["sessionId"] != sid || ff["messageUuid"] != orig[i] {
				t.Errorf("entry %d forkedFrom = %v", i, ff)
			}
			// Only the last message gets a fresh timestamp.
			if ts := e["timestamp"]; (i < 5) != (ts == "2026-03-01T00:00:00Z") {
				t.Errorf("entry %d timestamp = %v", i, ts)
			}
		}
		title := entries[6]
		if title["type"] != "custom-title" || title["sessionId"] != res.SessionID || title["customTitle"] != "Turn 1 question (fork)" ||
			!uuidLike.MatchString(str(title["uuid"])) || title["timestamp"] != entries[5]["timestamp"] {
			t.Errorf("title entry = %v", title)
		}

		origMsgs := newLocalSessions(root).getSessionMessages(sid, &SessionMessagesOptions{Directory: project})
		forkMsgs := newLocalSessions(root).getSessionMessages(res.SessionID, &SessionMessagesOptions{Directory: project})
		if len(forkMsgs) != len(origMsgs) || len(forkMsgs) != 6 {
			t.Errorf("messages: fork %d, source %d", len(forkMsgs), len(origMsgs))
		}
		var info *SessionInfo
		for _, s := range newLocalSessions(root).listSessions(&ListSessionsOptions{Directory: project}) {
			if s.SessionID == res.SessionID {
				info = &s
			}
		}
		if info == nil || info.CustomTitle != "Turn 1 question (fork)" {
			t.Errorf("listed fork = %+v", info)
		}
	})

	t.Run("up to message", func(t *testing.T) {
		t.Parallel()
		root, project, dir := mutationProject(t)
		sid, _, uuids := makeTranscriptSession(t, dir, 3)
		res, err := newLocalSessions(root).forkSession(sid, &ForkSessionOptions{Directory: project, UpToMessageID: uuids[1]})
		if err != nil {
			t.Fatal(err)
		}
		if msgs := newLocalSessions(root).getSessionMessages(res.SessionID, &SessionMessagesOptions{Directory: project}); len(msgs) != 2 {
			t.Errorf("messages = %d", len(msgs))
		}
		_, err = newLocalSessions(root).forkSession(sid, &ForkSessionOptions{Directory: project, UpToMessageID: randomUUID()})
		if err == nil || !strings.Contains(err.Error(), "not found in session") {
			t.Errorf("unknown up-to: %v", err)
		}
	})

	t.Run("titles", func(t *testing.T) {
		t.Parallel()
		root, project, dir := mutationProject(t)
		sid, path, _ := makeTranscriptSession(t, dir, 1)
		forkTitle := func(opts *ForkSessionOptions) string {
			t.Helper()
			opts.Directory = project
			res, err := newLocalSessions(root).forkSession(sid, opts)
			if err != nil {
				t.Fatal(err)
			}
			return str(lastEntry(t, filepath.Join(dir, res.SessionID+".jsonl"))["customTitle"])
		}
		if got := forkTitle(&ForkSessionOptions{Title: "  My Fork  "}); got != "My Fork" {
			t.Errorf("explicit = %q", got)
		}
		if got := forkTitle(&ForkSessionOptions{Title: "   "}); got != "Turn 1 question (fork)" {
			t.Errorf("blank = %q", got)
		}
		appendLine := func(line string) {
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if _, err := f.WriteString(line + "\n"); err != nil {
				t.Fatal(err)
			}
		}
		appendLine(jsonObj("type", "ai-title", "aiTitle", "Generated", "sessionId", sid))
		if got := forkTitle(&ForkSessionOptions{}); got != "Generated (fork)" {
			t.Errorf("ai title = %q", got)
		}
		appendLine(jsonObj("type", "custom-title", "customTitle", "Named", "sessionId", sid))
		if got := forkTitle(&ForkSessionOptions{}); got != "Named (fork)" {
			t.Errorf("custom title = %q", got)
		}
		if infos := newLocalSessions(root).listSessions(&ListSessionsOptions{Directory: project}); len(infos) != 5 {
			t.Errorf("sessions = %d", len(infos))
		}

		// No title source at all.
		sid2 := randomUUID()
		writeJSONL(t, filepath.Join(dir, sid2+".jsonl"), transcriptEntry("assistant", randomUUID(), "", sid2, "only an answer"))
		res, err := newLocalSessions(root).forkSession(sid2, &ForkSessionOptions{Directory: project})
		if err != nil {
			t.Fatal(err)
		}
		if got := lastEntry(t, filepath.Join(dir, res.SessionID+".jsonl"))["customTitle"]; got != "Forked session (fork)" {
			t.Errorf("fallback = %v", got)
		}
	})

	t.Run("without directory", func(t *testing.T) {
		t.Parallel()
		root := newProjectsRoot(t)
		dir := makeProjectDir(t, root, "/any/project")
		sid, _, _ := makeTranscriptSession(t, dir, 1)
		res, err := newLocalSessions(root).forkSession(sid, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(dir, res.SessionID+".jsonl")); err != nil {
			t.Error(err)
		}
	})

	t.Run("chain transform", func(t *testing.T) {
		t.Parallel()
		root := newProjectsRoot(t)
		dir := makeProjectDir(t, root, "/any/project")
		sid := randomUUID()
		other := randomUUID()
		u1, p1, p2, a1, side, b1, u2 := randomUUID(), randomUUID(), randomUUID(), randomUUID(), randomUUID(), randomUUID(), randomUUID()
		writeJSONL(t, filepath.Join(dir, sid+".jsonl"),
			jsonObj("type", "permission-mode", "permissionMode", "default", "sessionId", sid),
			transcriptEntry("user", u1, "", sid, "hello", "timestamp", "t-u1", "teamName", "x", "agentName", "y", "slug", "z", "sourceToolAssistantUUID", "w"),
			transcriptEntry("progress", p1, u1, sid, nil, "timestamp", "t-p1"),
			transcriptEntry("progress", p2, p1, sid, nil, "timestamp", "t-p2"),
			transcriptEntry("assistant", a1, p2, sid, "hi", "timestamp", "t-a1"),
			transcriptEntry("user", side, a1, sid, "sidechain", "isSidechain", true),
			"not json",
			"[1,2]",
			transcriptEntry("system", b1, "", sid, nil, "subtype", "compact_boundary", "logicalParentUuid", a1, "timestamp", "t-b1"),
			jsonObj("type", "file-history-snapshot", "messageId", u1),
			jsonObj("type", "content-replacement", "sessionId", sid, "replacements", []any{map[string]any{"toolUseId": "t1", "z": 1, "a": 2}}),
			jsonObj("type", "content-replacement", "sessionId", other, "replacements", []any{"foreign"}),
			jsonObj("type", "content-replacement", "sessionId", sid, "replacements", "not a list"),
			transcriptEntry("user", u2, b1, sid, "after compact", "timestamp", "t-u2", "logicalParentUuid", randomUUID()),
		)
		res, err := newLocalSessions(root).forkSession(sid, nil)
		if err != nil {
			t.Fatal(err)
		}
		entries := readEntries(t, filepath.Join(dir, res.SessionID+".jsonl"))
		var types []any
		for _, e := range entries {
			types = append(types, e["type"])
		}
		if want := []any{"user", "assistant", "system", "user", "content-replacement", "custom-title"}; !slices.Equal(types, want) {
			t.Fatalf("types = %v", types)
		}
		fu1, fa1, fb1, fu2, cr := entries[0], entries[1], entries[2], entries[3], entries[4]
		for _, k := range forkDroppedKeys {
			if _, ok := fu1[k]; ok {
				t.Errorf("%s kept", k)
			}
		}
		if fa1["parentUuid"] != fu1["uuid"] {
			t.Errorf("progress ancestors not skipped: %v", fa1["parentUuid"])
		}
		if fb1["parentUuid"] != nil || fb1["logicalParentUuid"] != fa1["uuid"] || fb1["subtype"] != "compact_boundary" {
			t.Errorf("compact boundary = %v", fb1)
		}
		if fu2["parentUuid"] != fb1["uuid"] || fu2["logicalParentUuid"] != nil {
			t.Errorf("after compact = %v", fu2)
		}
		if fu1["timestamp"] != "t-u1" || fb1["timestamp"] != "t-b1" || fu2["timestamp"] == "t-u2" {
			t.Errorf("timestamps = %v %v %v", fu1["timestamp"], fb1["timestamp"], fu2["timestamp"])
		}
		if cr["sessionId"] != res.SessionID || !uuidLike.MatchString(str(cr["uuid"])) || cr["timestamp"] != fu2["timestamp"] {
			t.Errorf("content replacement = %v", cr)
		}
		if got, _ := jsonx.Marshal(cr["replacements"]); string(got) != `[{"a":2,"toolUseId":"t1","z":1}]` {
			t.Errorf("replacements = %s", got)
		}
		if ff := fa1["forkedFrom"].(map[string]any); ff["messageUuid"] != a1 {
			t.Errorf("forkedFrom = %v", ff)
		}
	})

	t.Run("up to a progress entry and empty results", func(t *testing.T) {
		t.Parallel()
		root := newProjectsRoot(t)
		dir := makeProjectDir(t, root, "/any/project")

		sid := randomUUID()
		p1 := randomUUID()
		writeJSONL(t, filepath.Join(dir, sid+".jsonl"),
			transcriptEntry("progress", p1, "", sid, nil),
			transcriptEntry("user", randomUUID(), p1, sid, "hi"),
		)
		if _, err := newLocalSessions(root).forkSession(sid, &ForkSessionOptions{UpToMessageID: p1}); err == nil || !strings.Contains(err.Error(), "no messages to fork") {
			t.Errorf("only progress kept: %v", err)
		}

		meta := randomUUID()
		writeJSONL(t, filepath.Join(dir, meta+".jsonl"), jsonObj("type", "custom-title", "customTitle", "x", "sessionId", meta))
		if _, err := newLocalSessions(root).forkSession(meta, nil); err == nil || !strings.Contains(err.Error(), "no messages to fork") {
			t.Errorf("metadata only: %v", err)
		}

		sidechain := randomUUID()
		writeJSONL(t, filepath.Join(dir, sidechain+".jsonl"), transcriptEntry("user", randomUUID(), "", sidechain, "x", "isSidechain", true))
		if _, err := newLocalSessions(root).forkSession(sidechain, nil); err == nil || !strings.Contains(err.Error(), "no messages to fork") {
			t.Errorf("sidechain only: %v", err)
		}
	})

	t.Run("source formatting preserved", func(t *testing.T) {
		t.Parallel()
		root := newProjectsRoot(t)
		dir := makeProjectDir(t, root, "/any/project")
		sid, u := randomUUID(), randomUUID()
		// Key order and number literals are copied verbatim; only the
		// rewritten fields change, and new keys are appended.
		line := `{"parentUuid":null,"isSidechain":false,"type":"user","message":{"z":1.50,"a":12345678901234567890,"content":"caf` + "\u00e9" + `"},"uuid":"` + u + `","sessionId":"` + sid + `","timestamp":"t0"}`
		writeJSONL(t, filepath.Join(dir, sid+".jsonl"), line, transcriptEntry("assistant", randomUUID(), u, sid, "ok", "timestamp", "t1"))
		res, err := newLocalSessions(root).forkSession(sid, nil)
		if err != nil {
			t.Fatal(err)
		}
		got := readJSONLines(t, filepath.Join(dir, res.SessionID+".jsonl"))[0]
		re := regexp.MustCompile(`^\{"parentUuid":null,"isSidechain":false,"type":"user","message":\{"z":1\.50,"a":12345678901234567890,"content":"caf\\u00e9"\},"uuid":"[0-9a-f-]{36}","sessionId":"` + res.SessionID + `","timestamp":"t0","logicalParentUuid":null,"forkedFrom":\{"sessionId":"` + sid + `","messageUuid":"` + u + `"\}\}$`)
		if !re.MatchString(got) {
			t.Errorf("line = %s", got)
		}
	})

	t.Run("progress cycle terminates", func(t *testing.T) {
		t.Parallel()
		root := newProjectsRoot(t)
		dir := makeProjectDir(t, root, "/any/project")
		sid, p1, p2 := randomUUID(), randomUUID(), randomUUID()
		writeJSONL(t, filepath.Join(dir, sid+".jsonl"),
			transcriptEntry("progress", p1, p2, sid, nil),
			transcriptEntry("progress", p2, p1, sid, nil),
			transcriptEntry("user", randomUUID(), p2, sid, "hi"),
		)
		res, err := newLocalSessions(root).forkSession(sid, nil)
		if err != nil {
			t.Fatal(err)
		}
		if e := readEntries(t, filepath.Join(dir, res.SessionID+".jsonl"))[0]; e["parentUuid"] != nil {
			t.Errorf("parentUuid = %v", e["parentUuid"])
		}
	})
}

// ---------------------------------------------------------------------------
// SessionStore-backed mutations
// ---------------------------------------------------------------------------

// spyStore counts Append calls on top of an InMemorySessionStore.
type spyStore struct {
	*InMemorySessionStore
	appends int
}

func (s *spyStore) Append(ctx context.Context, key SessionKey, entries []SessionStoreEntry) error {
	s.appends++
	return s.InMemorySessionStore.Append(ctx, key, entries)
}

func TestSessionMutationsViaStore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("rename", func(t *testing.T) {
		t.Parallel()
		store := NewInMemorySessionStore()
		sid := randomUUID()
		seedChain(t, store, sid, 2)
		if err := RenameSessionViaStore(ctx, store, sid, "  New Title  ", storeTestDir); err != nil {
			t.Fatal(err)
		}
		entries := store.Entries(mainKey(sid))
		last := entries[len(entries)-1]
		if last["type"] != "custom-title" || last["customTitle"] != "New Title" || last["sessionId"] != sid ||
			!uuidLike.MatchString(str(last["uuid"])) || str(last["timestamp"]) == "" {
			t.Errorf("entry = %v", last)
		}
		if info, _ := GetSessionInfoFromStore(ctx, store, sid, storeTestDir); info == nil || info.CustomTitle != "New Title" {
			t.Errorf("info = %+v", info)
		}
		if err := RenameSessionViaStore(ctx, store, "not-a-uuid", "t", storeTestDir); !errors.Is(err, ErrInvalidSessionID) {
			t.Errorf("invalid id: %v", err)
		}
		if err := RenameSessionViaStore(ctx, store, sid, "  ", storeTestDir); err == nil || !strings.Contains(err.Error(), "title must be non-empty") {
			t.Errorf("blank title: %v", err)
		}
	})

	t.Run("tag", func(t *testing.T) {
		t.Parallel()
		store := NewInMemorySessionStore()
		sid := randomUUID()
		seedChain(t, store, sid, 1)
		if err := TagSessionViaStore(ctx, store, sid, "exp\u200b", storeTestDir); err != nil {
			t.Fatal(err)
		}
		entries := store.Entries(mainKey(sid))
		if last := entries[len(entries)-1]; last["type"] != "tag" || last["tag"] != "exp" || last["sessionId"] != sid || str(last["uuid"]) == "" {
			t.Errorf("entry = %v", last)
		}
		if info, _ := GetSessionInfoFromStore(ctx, store, sid, storeTestDir); info == nil || info.Tag != "exp" {
			t.Errorf("info = %+v", info)
		}
		if infos, _ := ListSessionsFromStore(ctx, store, &ListSessionsOptions{Directory: storeTestDir}); len(infos) != 1 || infos[0].Tag != "exp" {
			t.Errorf("listed = %+v", infos)
		}
		if err := TagSessionViaStore(ctx, store, sid, "", storeTestDir); err != nil {
			t.Fatal(err)
		}
		entries = store.Entries(mainKey(sid))
		if last := entries[len(entries)-1]; last["type"] != "tag" || last["tag"] != "" {
			t.Errorf("clear entry = %v", last)
		}
		if info, _ := GetSessionInfoFromStore(ctx, store, sid, storeTestDir); info == nil || info.Tag != "" {
			t.Errorf("cleared info = %+v", info)
		}
		if infos, _ := ListSessionsFromStore(ctx, store, &ListSessionsOptions{Directory: storeTestDir}); len(infos) != 1 || infos[0].Tag != "" {
			t.Errorf("cleared listing = %+v", infos)
		}
		if err := TagSessionViaStore(ctx, store, sid, " \ufeff ", storeTestDir); err == nil {
			t.Error("invisible tag accepted")
		}
	})

	t.Run("invalid ids never touch the store", func(t *testing.T) {
		t.Parallel()
		store := &spyStore{InMemorySessionStore: NewInMemorySessionStore()}
		if err := DeleteSessionViaStore(ctx, store, "not-a-uuid", storeTestDir); !errors.Is(err, ErrInvalidSessionID) || !strings.Contains(err.Error(), "not-a-uuid") {
			t.Errorf("delete: %v", err)
		}
		if err := TagSessionViaStore(ctx, store, "not-a-uuid", "tag", storeTestDir); !errors.Is(err, ErrInvalidSessionID) {
			t.Errorf("tag: %v", err)
		}
		if store.appends != 0 {
			t.Errorf("appends = %d", store.appends)
		}
	})

	t.Run("delete", func(t *testing.T) {
		t.Parallel()
		store := NewInMemorySessionStore()
		sid := randomUUID()
		seedChain(t, store, sid, 1)
		appendEntries(t, store, SessionKey{ProjectKey: storeTestKey, SessionID: sid, Subpath: "subagents/agent-a"}, storeUser("x", randomUUID(), "", sid))
		if store.Len() != 1 {
			t.Fatalf("Len = %d", store.Len())
		}
		if err := DeleteSessionViaStore(ctx, store, sid, storeTestDir); err != nil {
			t.Fatal(err)
		}
		if store.Len() != 0 {
			t.Errorf("Len = %d", store.Len())
		}
		if got, _ := store.Load(ctx, mainKey(sid)); got != nil {
			t.Errorf("Load = %v", got)
		}
		if subs, _ := store.ListSubkeys(ctx, SessionListSubkeysKey{ProjectKey: storeTestKey, SessionID: sid}); len(subs) != 0 {
			t.Errorf("subkeys = %v", subs)
		}
		// Without SessionDeleter, deletion is a no-op.
		if err := DeleteSessionViaStore(ctx, storeMinimal{store}, randomUUID(), storeTestDir); err != nil {
			t.Errorf("minimal store: %v", err)
		}
	})

	t.Run("fork round trip", func(t *testing.T) {
		t.Parallel()
		store := NewInMemorySessionStore()
		sid := randomUUID()
		src := seedChain(t, store, sid, 2)
		res, err := ForkSessionViaStore(ctx, store, sid, &ForkSessionOptions{Directory: storeTestDir})
		if err != nil {
			t.Fatal(err)
		}
		if res.SessionID == sid {
			t.Fatal("same id")
		}
		forked := store.Entries(mainKey(res.SessionID))
		var msgs []SessionStoreEntry
		for _, e := range forked {
			if e["type"] == "user" || e["type"] == "assistant" {
				msgs = append(msgs, e)
			}
		}
		if len(msgs) != 4 {
			t.Fatalf("messages = %d", len(msgs))
		}
		var prev any
		for _, e := range msgs {
			if e["sessionId"] != res.SessionID || slices.Contains(src, str(e["uuid"])) || e["parentUuid"] != prev ||
				e["forkedFrom"].(map[string]any)["sessionId"] != sid {
				t.Errorf("entry = %v", e)
			}
			prev = e["uuid"]
		}
		trailer := forked[len(forked)-1]
		if trailer["type"] != "custom-title" || trailer["customTitle"] != "prompt 0 (fork)" || str(trailer["uuid"]) == "" || str(trailer["timestamp"]) == "" {
			t.Errorf("trailer = %v", trailer)
		}
		got, err := GetSessionMessagesFromStore(ctx, store, res.SessionID, &SessionMessagesOptions{Directory: storeTestDir})
		if err != nil || len(got) != 4 {
			t.Errorf("readable: %d, %v", len(got), err)
		}
		// The fork shares no objects with the source.
		msgs[0]["message"].(map[string]any)["content"] = "mutated"
		if src0 := store.Entries(mainKey(sid))[0]; src0["message"].(map[string]any)["content"] != "prompt 0" {
			t.Error("fork aliases the source entries")
		}
	})

	t.Run("fork derives title from the original entries", func(t *testing.T) {
		t.Parallel()
		for _, tt := range []struct {
			extra []SessionStoreEntry
			want  string
		}{
			{[]SessionStoreEntry{{"type": "ai-title", "aiTitle": "Generated"}}, "Generated (fork)"},
			{[]SessionStoreEntry{{"type": "custom-title", "customTitle": "My Title"}, {"type": "ai-title", "aiTitle": "Later AI"}}, "My Title (fork)"},
			{[]SessionStoreEntry{{"type": "custom-title", "customTitle": "A"}, {"type": "custom-title", "customTitle": ""}}, "A (fork)"},
		} {
			store := NewInMemorySessionStore()
			sid := randomUUID()
			seedChain(t, store, sid, 1)
			appendEntries(t, store, mainKey(sid), tt.extra...)
			res, err := ForkSessionViaStore(ctx, store, sid, &ForkSessionOptions{Directory: storeTestDir})
			if err != nil {
				t.Fatal(err)
			}
			forked := store.Entries(mainKey(res.SessionID))
			if got := forked[len(forked)-1]["customTitle"]; got != tt.want {
				t.Errorf("title = %v, want %q", got, tt.want)
			}
		}
	})

	t.Run("fork preserves chain and stamps synthetic entries", func(t *testing.T) {
		t.Parallel()
		store := NewInMemorySessionStore()
		sid := randomUUID()
		u1 := storeUser("one", randomUUID(), "", sid)
		a1 := storeAssistant("two", randomUUID(), str(u1["uuid"]), sid)
		u2 := storeUser("three", randomUUID(), str(a1["uuid"]), sid)
		cr := SessionStoreEntry{"type": "content-replacement", "sessionId": sid,
			"replacements": []map[string]any{{"toolUseId": "tu_1", "newContent": "x"}}}
		appendEntries(t, store, mainKey(sid), u1, a1, u2, cr)

		res, err := ForkSessionViaStore(ctx, store, sid, &ForkSessionOptions{Directory: storeTestDir, UpToMessageID: str(a1["uuid"]), Title: "My Fork"})
		if err != nil {
			t.Fatal(err)
		}
		forked := store.Entries(mainKey(res.SessionID))
		if len(forked) != 4 {
			t.Fatalf("forked = %v", forked)
		}
		f0, f1, crOut, title := forked[0], forked[1], forked[2], forked[3]
		if f0["uuid"] == u1["uuid"] || f0["parentUuid"] != nil || f1["parentUuid"] != f0["uuid"] ||
			f0["sessionId"] != res.SessionID || f0["forkedFrom"].(map[string]any)["messageUuid"] != u1["uuid"] {
			t.Errorf("chain = %v / %v", f0, f1)
		}
		if title["type"] != "custom-title" || title["customTitle"] != "My Fork" || str(title["uuid"]) == "" || str(title["timestamp"]) == "" {
			t.Errorf("title = %v", title)
		}
		if crOut["type"] != "content-replacement" || crOut["sessionId"] != res.SessionID || str(crOut["uuid"]) == "" || str(crOut["timestamp"]) == "" {
			t.Errorf("content replacement = %v", crOut)
		}
		if reps, _ := crOut["replacements"].([]any); len(reps) != 1 || reps[0].(map[string]any)["toolUseId"] != "tu_1" {
			t.Errorf("replacements = %v", crOut["replacements"])
		}
	})

	t.Run("fork errors", func(t *testing.T) {
		t.Parallel()
		store := NewInMemorySessionStore()
		if _, err := ForkSessionViaStore(ctx, store, randomUUID(), &ForkSessionOptions{Directory: storeTestDir}); !errors.Is(err, ErrSessionNotFound) {
			t.Errorf("missing: %v", err)
		}
		if _, err := ForkSessionViaStore(ctx, store, "not-a-uuid", nil); !errors.Is(err, ErrInvalidSessionID) {
			t.Errorf("invalid id: %v", err)
		}
		sid := randomUUID()
		uuids := seedChain(t, store, sid, 3)
		if _, err := ForkSessionViaStore(ctx, store, sid, &ForkSessionOptions{Directory: storeTestDir, UpToMessageID: "not-a-uuid"}); err == nil || !strings.Contains(err.Error(), "up-to message id") {
			t.Errorf("invalid up-to: %v", err)
		}
		res, err := ForkSessionViaStore(ctx, store, sid, &ForkSessionOptions{Directory: storeTestDir, UpToMessageID: uuids[1]})
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, e := range store.Entries(mainKey(res.SessionID)) {
			if e["type"] == "user" || e["type"] == "assistant" {
				n++
			}
		}
		if n != 2 {
			t.Errorf("messages = %d", n)
		}
		failing := storeLoadErr{store, errors.New("boom")}
		if _, err := ForkSessionViaStore(ctx, failing, sid, &ForkSessionOptions{Directory: storeTestDir}); err == nil || err.Error() != "boom" {
			t.Errorf("load error: %v", err)
		}
	})
}

// storeLoadErr fails every Load.
type storeLoadErr struct {
	SessionStore
	err error
}

func (s storeLoadErr) Load(context.Context, SessionKey) ([]SessionStoreEntry, error) {
	return nil, s.err
}

// ---------------------------------------------------------------------------
// Public wrappers (process environment)
// ---------------------------------------------------------------------------

func TestSessionPublicMutationAPI(t *testing.T) {
	config := filepath.Join(t.TempDir(), "cfg")
	t.Setenv("CLAUDE_CONFIG_DIR", config)
	root := filepath.Join(config, "projects")
	project, canonical := newProject(t, "proj")
	dir := makeProjectDir(t, root, canonical)
	sid, path, _ := makeTranscriptSession(t, dir, 1)
	writeFile(t, filepath.Join(dir, sid, "subagents", "agent-a.jsonl"), jsonObj("type", "user", "uuid", "s1")+"\n")

	if err := RenameSession(sid, "Renamed", project); err != nil {
		t.Fatal(err)
	}
	if err := TagSession(sid, "t1", ""); err != nil {
		t.Fatal(err)
	}
	if info, _ := GetSessionInfo(sid, project); info == nil || info.CustomTitle != "Renamed" || info.Tag != "t1" {
		t.Errorf("info = %+v", info)
	}

	store := NewInMemorySessionStore()
	if err := ImportSessionToStore(context.Background(), sid, store, &ImportSessionOptions{Directory: project}); err != nil {
		t.Fatal(err)
	}
	key := SessionKey{ProjectKey: sanitizePath(canonical), SessionID: sid}
	if got := store.Entries(key); len(got) != 4 {
		t.Errorf("imported = %v", got)
	}
	if got := store.Entries(SessionKey{ProjectKey: key.ProjectKey, SessionID: sid, Subpath: "subagents/agent-a"}); len(got) != 1 {
		t.Errorf("imported subagent = %v", got)
	}

	res, err := ForkSession(sid, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := lastEntry(t, filepath.Join(dir, res.SessionID+".jsonl"))["customTitle"]; got != "Renamed (fork)" {
		t.Errorf("fork title = %v", got)
	}
	if err := DeleteSession(sid, project); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("not deleted: %v", err)
	}
	if err := DeleteSession(sid, project); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("second delete: %v", err)
	}
}
