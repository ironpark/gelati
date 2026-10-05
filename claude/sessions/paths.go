package sessions

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ironpark/gelati/claude/internal/transcript"
	"github.com/ironpark/gelati/claude/internal/unicodenorm"
)

// Locating transcripts on disk: project directories (the
// CLAUDE_CODE_PROJECT_DIR_NAME override, long-name hash suffixes), git
// worktrees, and session and subagent files. The config directories and
// project directory naming shared with package claude live in
// internal/transcript.

// worktreeListTimeout bounds `git worktree list`.
const worktreeListTimeout = 5 * time.Second

// ---------------------------------------------------------------------------
// Local transcript backend
// ---------------------------------------------------------------------------

// localSessions is the backend of the session functions that work on the
// CLI's local transcripts: the projects directory they read and write, plus
// the settings the environment applies to it. The exported functions use
// localSessionsFromEnv; tests build one over a temporary root.
type localSessions struct {
	// root is the projects directory (see transcript.ProjectsDir).
	root string
	// dirNameOverride is the CLAUDE_CODE_PROJECT_DIR_NAME override that
	// names the project directory of every working directory under root,
	// or "" (see projectDirNameOverride).
	dirNameOverride string
	// skipPrecompact enables the large-transcript pre-compact skip of
	// getSessionMessages (see precompactSkipEnabled).
	skipPrecompact bool
}

// newLocalSessions returns the backend for the transcripts under root,
// configured from the process environment: the project directory name
// override applies only when root is the projects directory of
// CLAUDE_CONFIG_DIR.
func newLocalSessions(root string) localSessions {
	return localSessions{
		root:            root,
		dirNameOverride: projectDirNameOverride(root),
		skipPrecompact:  precompactSkipEnabled(os.Getenv),
	}
}

// localSessionsFromEnv returns the backend for the process's transcript
// root, $CLAUDE_CONFIG_DIR/projects (default ~/.claude/projects).
func localSessionsFromEnv() localSessions {
	return newLocalSessions(transcript.ProjectsDir(nil))
}

// ---------------------------------------------------------------------------
// Directory entries
// ---------------------------------------------------------------------------

// isDirFollow reports whether path is a directory, following symlinks
// (Python's Path.is_dir).
func isDirFollow(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// direntIsDir is Path.is_dir for a directory entry: symlinks are followed.
func direntIsDir(parent string, e fs.DirEntry) bool {
	if e.Type()&fs.ModeSymlink != 0 {
		return isDirFollow(filepath.Join(parent, e.Name()))
	}
	return e.IsDir()
}

// direntIsFile is Path.is_file for a directory entry: symlinks are followed.
func direntIsFile(parent string, e fs.DirEntry) bool {
	if e.Type()&fs.ModeSymlink != 0 {
		fi, err := os.Stat(filepath.Join(parent, e.Name()))
		return err == nil && fi.Mode().IsRegular()
	}
	return e.Type().IsRegular()
}

// subdirs returns the paths of the directories directly inside root,
// following symlinks, or nil when root cannot be read.
func subdirs(root string) []string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if direntIsDir(root, e) {
			out = append(out, filepath.Join(root, e.Name()))
		}
	}
	return out
}

// collectJSONLFiles returns every *.jsonl file under baseDir, recursively,
// sorted by name within each directory, following symlinks. It returns nil
// when baseDir cannot be read.
func collectJSONLFiles(baseDir string) []string {
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		switch {
		case direntIsDir(baseDir, e):
			out = append(out, collectJSONLFiles(filepath.Join(baseDir, e.Name()))...)
		case direntIsFile(baseDir, e) && strings.HasSuffix(e.Name(), ".jsonl"):
			out = append(out, filepath.Join(baseDir, e.Name()))
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Path comparison
// ---------------------------------------------------------------------------

// foldPath lowercases s when caseInsensitive is set, for comparing paths and
// directory names as a case-insensitive filesystem would.
func foldPath(s string, caseInsensitive bool) string {
	if caseInsensitive {
		return strings.ToLower(s)
	}
	return s
}

// darwinNFC NFC-normalizes s on macOS, whose filesystems may hand back
// decomposed names, and returns it unchanged elsewhere.
func darwinNFC(s string) string {
	if runtime.GOOS == "darwin" {
		return unicodenorm.NFC(s)
	}
	return s
}

// ---------------------------------------------------------------------------
// Project directories
// ---------------------------------------------------------------------------

// projectDirNameOverride returns the project directory name override that
// applies to the transcripts under root: the process's
// CLAUDE_CODE_PROJECT_DIR_NAME when root is the projects directory of its
// CLAUDE_CONFIG_DIR, else "".
func projectDirNameOverride(root string) string {
	override := transcript.ProjectDirNameOverride(os.Getenv)
	if override == "" || filepath.Clean(root) != transcript.ProjectsDir(nil) {
		return ""
	}
	return override
}

// projectDirName is the name of the transcript directory of projectPath:
// the override when set, else the sanitized path.
func projectDirName(projectPath, override string) string {
	if override != "" {
		return override
	}
	return transcript.SanitizePath(projectPath)
}

// ProjectKey returns the Store project key for a directory; empty means the
// current working directory.
//
// It applies the same realpath, NFC normalization and djb2-hashed
// sanitization the CLI uses to name project directories, so keys match
// between local transcripts and store-mirrored ones, even on filesystems
// that decompose Unicode (macOS HFS+). When CLAUDE_CONFIG_DIR and a valid
// CLAUDE_CODE_PROJECT_DIR_NAME are both set in the process environment,
// the key is that name for every directory, as in the CLI.
func ProjectKey(directory string) string {
	return transcript.ProjectKey(directory, os.Getenv)
}

// findProjectDirs returns the existing transcript directories for
// projectPath: the directory named by projectDirName and, when the override
// differs from the sanitized name, the sanitized one too. For paths whose
// sanitized name exceeds transcript.MaxSanitizedLength (and no override), it
// adds every directory sharing the truncated prefix whose sessions were
// recorded in projectPath, since the CLI (Bun.hash) and the SDKs
// (transcript.SanitizePath) produce different hash suffixes.
func (s localSessions) findProjectDirs(projectPath string) []string {
	var dirs []string
	override := s.dirNameOverride
	primary := filepath.Join(s.root, projectDirName(projectPath, override))
	if isDirFollow(primary) {
		dirs = append(dirs, primary)
	}
	sanitized := transcript.SanitizePath(projectPath)
	if override != "" {
		if sanitized != override {
			if p := filepath.Join(s.root, sanitized); isDirFollow(p) {
				dirs = append(dirs, p)
			}
		}
		return dirs
	}
	if len(sanitized) <= transcript.MaxSanitizedLength {
		return dirs
	}
	caseInsensitive := runtime.GOOS == "windows"
	prefix := foldPath(sanitized[:transcript.MaxSanitizedLength]+"-", caseInsensitive)
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return dirs
	}
	for _, e := range entries {
		if !strings.HasPrefix(foldPath(e.Name(), caseInsensitive), prefix) || !direntIsDir(s.root, e) {
			continue
		}
		p := filepath.Join(s.root, e.Name())
		if foldPath(p, caseInsensitive) != foldPath(primary, caseInsensitive) &&
			dirHasSessionForPath(p, projectPath, caseInsensitive) {
			dirs = append(dirs, p)
		}
	}
	return dirs
}

// findProjectDir returns the first of findProjectDirs, or "".
func (s localSessions) findProjectDir(projectPath string) string {
	if dirs := s.findProjectDirs(projectPath); len(dirs) > 0 {
		return dirs[0]
	}
	return ""
}

// dirHasSessionForPath reports whether some transcript directly in dir was
// recorded in a working directory whose (untruncated) sanitized form is
// that of projectPath.
func dirHasSessionForPath(dir, projectPath string, caseInsensitive bool) bool {
	want := transcript.SanitizeFull(projectPath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.Type().IsRegular() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		lite := readSessionLite(filepath.Join(dir, e.Name()))
		if lite == nil {
			continue
		}
		cwd, ok := recordedSessionCwd(lite)
		if !ok {
			continue
		}
		got := transcript.SanitizeFull(darwinNFC(cwd))
		if got == want || (caseInsensitive && strings.EqualFold(got, want)) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Git worktrees
// ---------------------------------------------------------------------------

// getWorktreePaths returns the NFC-normalized absolute paths of the git
// worktrees of the repository containing cwd, or nil when git is
// unavailable, cwd is not in a repository, or the command fails or takes
// longer than five seconds.
func getWorktreePaths(cwd string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), worktreeListTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "worktree", "list", "--porcelain")
	cmd.Dir = cwd
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil || len(out) == 0 || !utf8.Valid(out) {
		return nil
	}
	text := strings.ReplaceAll(strings.ReplaceAll(string(out), "\r\n", "\n"), "\r", "\n")
	var paths []string
	for line := range strings.SplitSeq(text, "\n") {
		if p, ok := strings.CutPrefix(line, "worktree "); ok {
			paths = append(paths, unicodenorm.NFC(p))
		}
	}
	return paths
}

// ---------------------------------------------------------------------------
// Directory-name collisions
// ---------------------------------------------------------------------------

// unsafeRealpathTarget reports paths the collision filter must not resolve:
// UNC paths, paths with ".." components and network mounts (/net,
// /Network/Servers).
func unsafeRealpathTarget(p string) bool {
	if strings.HasPrefix(p, "//") || strings.HasPrefix(p, `\\`) {
		return true
	}
	parts := strings.FieldsFunc(filepath.ToSlash(p), func(r rune) bool { return r == '/' })
	if slices.Contains(parts, "..") {
		return true
	}
	if !strings.HasPrefix(p, "/") || len(parts) == 0 {
		return false
	}
	return strings.EqualFold(parts[0], "net") ||
		(len(parts) >= 2 && strings.EqualFold(parts[0], "network") && strings.EqualFold(parts[1], "servers"))
}

// sanitizedNamesCollide reports whether two paths differ but share a
// sanitized project directory name.
func sanitizedNamesCollide(a, b string, caseInsensitive bool) bool {
	a, b = darwinNFC(a), darwinNFC(b)
	if foldPath(transcript.SanitizeFull(a), caseInsensitive) != foldPath(transcript.SanitizeFull(b), caseInsensitive) {
		return false
	}
	return foldPath(strings.ReplaceAll(a, `\`, "/"), caseInsensitive) !=
		foldPath(strings.ReplaceAll(b, `\`, "/"), caseInsensitive)
}

// isForeignSession reports whether a session found in projectPath's
// transcript directory was recorded in a different real directory that
// merely sanitizes to the same name (e.g. /a-b and /a/b), so that it does
// not belong to projectPath. Sessions recorded inside one of ownWorktrees
// always belong.
func isForeignSession(cwd, projectPath string, ownWorktrees []string) bool {
	caseInsensitive := runtime.GOOS == "windows" || runtime.GOOS == "darwin"
	fold := func(s string) string {
		return foldPath(strings.ReplaceAll(darwinNFC(s), `\`, "/"), caseInsensitive)
	}
	c := fold(cwd)
	for _, wt := range ownWorktrees {
		w := fold(wt)
		if !strings.HasSuffix(w, "/") {
			w += "/"
		}
		if c+"/" == w || strings.HasPrefix(c, w) {
			return false
		}
	}
	if !sanitizedNamesCollide(cwd, projectPath, caseInsensitive) ||
		unsafeRealpathTarget(cwd) || unsafeRealpathTarget(projectPath) {
		return false
	}
	real, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return false
	}
	return sanitizedNamesCollide(real, transcript.CanonicalizePath(projectPath), caseInsensitive)
}

// ---------------------------------------------------------------------------
// Session and subagent files
// ---------------------------------------------------------------------------

// foundSession is a located transcript.
type foundSession struct {
	path        string
	projectPath string // the searched project or worktree path; empty when every project was searched
	size        int64
}

// findSessionFile finds the first non-empty transcript of sessionID: in the
// project directories of directory and then of its other git worktrees
// when directory is set, otherwise in every project directory.
func (s localSessions) findSessionFile(sessionID, directory string) (foundSession, bool) {
	fileName := sessionID + ".jsonl"
	check := func(projectDir, projectPath string) (foundSession, bool) {
		p := filepath.Join(projectDir, fileName)
		if fi, err := os.Stat(p); err == nil && fi.Size() > 0 {
			return foundSession{path: p, projectPath: projectPath, size: fi.Size()}, true
		}
		return foundSession{}, false
	}
	if directory != "" {
		canonical := transcript.CanonicalizePath(directory)
		for _, dir := range s.findProjectDirs(canonical) {
			if f, ok := check(dir, canonical); ok {
				return f, true
			}
		}
		for _, wt := range getWorktreePaths(canonical) {
			if wt == canonical {
				continue
			}
			for _, dir := range s.findProjectDirs(wt) {
				if f, ok := check(dir, wt); ok {
					return f, true
				}
			}
		}
		return foundSession{}, false
	}
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return foundSession{}, false
	}
	for _, e := range entries {
		if f, ok := check(filepath.Join(s.root, e.Name()), ""); ok {
			return f, true
		}
	}
	return foundSession{}, false
}

// resolveSessionFilePath returns the path of the first non-empty transcript
// of sessionID (see findSessionFile), or "" when none exists.
func (s localSessions) resolveSessionFilePath(sessionID, directory string) string {
	found, _ := s.findSessionFile(sessionID, directory)
	return found.path
}

// resolveSubagentsDir returns <projectDir>/<sessionID>/subagents for the
// session's transcript at <projectDir>/<sessionID>.jsonl, or "" when the
// session is not found. The directory itself may not exist.
func (s localSessions) resolveSubagentsDir(sessionID, directory string) string {
	p := s.resolveSessionFilePath(sessionID, directory)
	if p == "" {
		return ""
	}
	return filepath.Join(strings.TrimSuffix(p, ".jsonl"), "subagents")
}

// agentFile is a subagent transcript found on disk.
type agentFile struct {
	agentID string
	path    string
}

// collectAgentFiles recursively collects agent-<id>.jsonl files under
// baseDir in name order. Transcripts may sit directly in subagents/ or in
// nested directories such as subagents/workflows/<runId>/.
func collectAgentFiles(baseDir string) []agentFile {
	var results []agentFile
	for _, path := range collectJSONLFiles(baseDir) {
		if id, ok := strings.CutPrefix(filepath.Base(path), "agent-"); ok {
			results = append(results, agentFile{agentID: strings.TrimSuffix(id, ".jsonl"), path: path})
		}
	}
	return results
}
