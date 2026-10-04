package claude

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Locating transcripts on disk: config and projects directories, project
// directory naming (sanitization, the CLAUDE_CODE_PROJECT_DIR_NAME override,
// long-name hash suffixes), git worktrees, and session and subagent files.

const (
	// maxSanitizedLength is the longest sanitized project directory name
	// kept verbatim. Most filesystems limit a path component to 255 bytes;
	// 200 leaves room for the hash suffix and separator.
	maxSanitizedLength = 200

	// worktreeListTimeout bounds `git worktree list`.
	worktreeListTimeout = 5 * time.Second
)

var (
	uuidRE = regexp.MustCompile(
		`^(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

	sanitizeRE = regexp.MustCompile(`[^a-zA-Z0-9]`)
)

// ---------------------------------------------------------------------------
// Local transcript backend
// ---------------------------------------------------------------------------

// localSessions is the backend of the session functions that work on the
// CLI's local transcripts: the projects directory they read and write, plus
// the settings the environment applies to it. The exported functions use
// localSessionsFromEnv; tests build one over a temporary root.
type localSessions struct {
	// root is the projects directory (see projectsDir).
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
	return newLocalSessions(projectsDir(nil))
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
// UUID validation and path sanitization
// ---------------------------------------------------------------------------

// validateUUID reports whether s is a UUID (any case). Session ids are used
// as path components, so only UUIDs are accepted.
func validateUUID(s string) bool { return uuidRE.MatchString(s) }

// simpleHash is the CLI's 32-bit djb2-style string hash rendered in base 36,
// used to suffix truncated project directory names. It walks code points as
// Python does; bytes that are not valid UTF-8 hash as Python's
// surrogateescape code points (U+DC80..U+DCFF).
func simpleHash(s string) string {
	var h int32
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size <= 1 {
			r = 0xDC00 + rune(s[i])
			size = 1
		}
		h = h<<5 - h + int32(r) // wraps like JS `hash |= 0`
		i += size
	}
	return strconv.FormatInt(max(int64(h), -int64(h)), 36)
}

// sanitizeFull replaces every character of name other than an ASCII letter
// or digit with '-', without sanitizePath's truncation.
func sanitizeFull(name string) string {
	return sanitizeRE.ReplaceAllLiteralString(name, "-")
}

// sanitizePath makes name safe for use as a directory name: every
// character other than an ASCII letter or digit becomes '-'. Results longer
// than maxSanitizedLength are truncated and suffixed with "-<hash>".
func sanitizePath(name string) string {
	sanitized := sanitizeFull(name)
	if len(sanitized) <= maxSanitizedLength {
		return sanitized
	}
	return sanitized[:maxSanitizedLength] + "-" + simpleHash(name)
}

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
		return normalizeNFC(s)
	}
	return s
}

// ---------------------------------------------------------------------------
// Config directories
// ---------------------------------------------------------------------------

// userHomeDir is Python's Path.home(): $HOME, then the password database.
func userHomeDir() (string, error) {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return home, nil
	}
	u, err := user.Current()
	if err != nil {
		return "", err
	}
	return u.HomeDir, nil
}

// claudeConfigHomeDir returns the Claude config directory: CLAUDE_CONFIG_DIR
// when set, else ~/.claude, NFC-normalized.
func claudeConfigHomeDir() string {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return filepath.Clean(normalizeNFC(dir))
	}
	home, _ := userHomeDir()
	return normalizeNFC(filepath.Join(home, ".claude"))
}

// projectsDir returns the directory holding the per-project transcript
// directories. env is consulted before the process environment, so callers
// that pass CLAUDE_CONFIG_DIR to the CLI subprocess through Options.Env
// resolve the directory that subprocess will write to. env may be nil.
func projectsDir(env map[string]string) string {
	if dir := env["CLAUDE_CONFIG_DIR"]; dir != "" {
		return filepath.Join(normalizeNFC(dir), "projects")
	}
	return filepath.Join(claudeConfigHomeDir(), "projects")
}

// canonicalizePath resolves a directory to its canonical form: absolute,
// symlinks resolved (Python's non-strict os.path.realpath, so missing
// components are kept) and NFC-normalized.
func canonicalizePath(d string) string {
	resolved, err := realpath(d)
	if err != nil {
		return normalizeNFC(d)
	}
	return normalizeNFC(resolved)
}

// realpath ports Python's non-strict posixpath.realpath: symlinks are
// resolved component by component, ".." is applied to the resolved prefix,
// and components that do not exist are appended unresolved. Symlink loops
// stop resolution at the looping link.
func realpath(name string) (string, error) {
	if runtime.GOOS == "windows" {
		abs, err := filepath.Abs(name)
		if err != nil {
			return "", err
		}
		if resolved, err := filepath.EvalSymlinks(abs); err == nil {
			return resolved, nil
		}
		return abs, nil
	}
	if !strings.HasPrefix(name, "/") {
		// Python starts from the physical getcwd(); Go's Getwd may return
		// a logical $PWD, which the component walk below resolves.
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		name = wd + "/" + name
	}
	type part struct {
		name     string
		resolved bool   // marker: the symlink below has been resolved
		link     string // symlink path for resolved markers
	}
	var rest []part // stack; the next part is at the end
	pushParts := func(p string) int {
		fields := strings.Split(p, "/")
		for i := len(fields) - 1; i >= 0; i-- {
			rest = append(rest, part{name: fields[i]})
		}
		return len(fields)
	}
	count := pushParts(name)
	path := "/"
	seen := map[string]*string{}
	for count > 0 {
		p := rest[len(rest)-1]
		rest = rest[:len(rest)-1]
		if p.resolved {
			resolved := path
			seen[p.link] = &resolved
			continue
		}
		count--
		switch p.name {
		case "", ".":
			continue
		case "..":
			if i := strings.LastIndex(path, "/"); i > 0 {
				path = path[:i]
			} else {
				path = "/"
			}
			continue
		}
		newpath := path + "/" + p.name
		if path == "/" {
			newpath = "/" + p.name
		}
		fi, err := os.Lstat(newpath)
		if err != nil || fi.Mode()&fs.ModeSymlink == 0 {
			path = newpath
			continue
		}
		if cached, ok := seen[newpath]; ok {
			if cached != nil {
				path = *cached
			} else {
				path = newpath // symlink loop
			}
			continue
		}
		target, err := os.Readlink(newpath)
		if err != nil {
			path = newpath
			continue
		}
		if strings.HasPrefix(target, "/") {
			path = "/"
		}
		seen[newpath] = nil
		rest = append(rest, part{resolved: true, link: newpath})
		count += pushParts(target)
	}
	return path, nil
}

// ---------------------------------------------------------------------------
// Project directories
// ---------------------------------------------------------------------------

// projectDirNameRE and reservedDirNameRE validate the
// CLAUDE_CODE_PROJECT_DIR_NAME override.
var (
	projectDirNameRE  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	reservedDirNameRE = regexp.MustCompile(`(?i)^(?:con|prn|aux|nul|com[0-9]|lpt[0-9])$`)
)

// validProjectDirName returns name when it is a usable
// CLAUDE_CODE_PROJECT_DIR_NAME: 1-64 ASCII letters, digits, '_' or '-' and
// not a reserved Windows device name. Otherwise it returns "".
func validProjectDirName(name string) string {
	if !projectDirNameRE.MatchString(name) || reservedDirNameRE.MatchString(name) {
		return ""
	}
	return name
}

// projectDirNameOverrideFromEnv returns the project directory name override
// of an environment: CLAUDE_CODE_PROJECT_DIR_NAME, honored only when
// CLAUDE_CONFIG_DIR is set too (the CLI applies it to every working
// directory under that config directory).
func projectDirNameOverrideFromEnv(getenv func(string) string) string {
	if getenv("CLAUDE_CONFIG_DIR") == "" {
		return ""
	}
	return validProjectDirName(getenv("CLAUDE_CODE_PROJECT_DIR_NAME"))
}

// projectDirNameOverride returns the project directory name override that
// applies to the transcripts under root: the process's
// CLAUDE_CODE_PROJECT_DIR_NAME when root is the projects directory of its
// CLAUDE_CONFIG_DIR, else "".
func projectDirNameOverride(root string) string {
	override := projectDirNameOverrideFromEnv(os.Getenv)
	if override == "" || filepath.Clean(root) != projectsDir(nil) {
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
	return sanitizePath(projectPath)
}

// ProjectKeyForDirectory returns the SessionStore project key for a
// directory; empty means the current working directory.
//
// It applies the same realpath, NFC normalization and djb2-hashed
// sanitization the CLI uses to name project directories, so keys match
// between local transcripts and store-mirrored ones, even on filesystems
// that decompose Unicode (macOS HFS+). When CLAUDE_CONFIG_DIR and a valid
// CLAUDE_CODE_PROJECT_DIR_NAME are both set in the process environment,
// the key is that name for every directory, as in the CLI.
func ProjectKeyForDirectory(directory string) string {
	return projectKeyForDirectory(directory, os.Getenv)
}

// projectKeyForDirectory is ProjectKeyForDirectory against an explicit
// environment.
func projectKeyForDirectory(directory string, getenv func(string) string) string {
	if override := projectDirNameOverrideFromEnv(getenv); override != "" {
		return override
	}
	return sanitizePath(storeProjectPath(directory))
}

// storeProjectPath is the canonical project path for a store call's
// directory argument; empty means the current working directory.
func storeProjectPath(directory string) string {
	if directory == "" {
		directory = "."
	}
	return canonicalizePath(directory)
}

// findProjectDirs returns the existing transcript directories for
// projectPath: the directory named by projectDirName and, when the override
// differs from the sanitized name, the sanitized one too. For paths whose
// sanitized name exceeds maxSanitizedLength (and no override), it adds every
// directory sharing the truncated prefix whose sessions were recorded in
// projectPath, since the CLI (Bun.hash) and the SDKs (simpleHash) produce
// different hash suffixes.
func (s localSessions) findProjectDirs(projectPath string) []string {
	var dirs []string
	override := s.dirNameOverride
	primary := filepath.Join(s.root, projectDirName(projectPath, override))
	if isDirFollow(primary) {
		dirs = append(dirs, primary)
	}
	sanitized := sanitizePath(projectPath)
	if override != "" {
		if sanitized != override {
			if p := filepath.Join(s.root, sanitized); isDirFollow(p) {
				dirs = append(dirs, p)
			}
		}
		return dirs
	}
	if len(sanitized) <= maxSanitizedLength {
		return dirs
	}
	caseInsensitive := runtime.GOOS == "windows"
	prefix := foldPath(sanitized[:maxSanitizedLength]+"-", caseInsensitive)
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
	want := sanitizeFull(projectPath)
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
		got := sanitizeFull(darwinNFC(cwd))
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
			paths = append(paths, normalizeNFC(p))
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
	if foldPath(sanitizeFull(a), caseInsensitive) != foldPath(sanitizeFull(b), caseInsensitive) {
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
	return sanitizedNamesCollide(real, canonicalizePath(projectPath), caseInsensitive)
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
		canonical := canonicalizePath(directory)
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

// agentMetadataSidecarPath maps agent-<id>.jsonl to agent-<id>.meta.json in
// the same directory. It is the single definition of the sidecar naming
// convention, shared by reading, import and resume.
func agentMetadataSidecarPath(transcriptPath string) string {
	return strings.TrimSuffix(transcriptPath, ".jsonl") + ".meta.json"
}
