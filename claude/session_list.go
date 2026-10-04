package claude

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// Session listing and reading for the CLI's local transcript store.
//
// The CLI writes one JSONL transcript per session to
// <config>/projects/<sanitized-cwd>/<session-id>.jsonl. Listing extracts
// metadata from a stat plus a head/tail read of each file rather than a full
// JSONL parse (session_lite.go); reading a session rebuilds its conversation
// chain from the parentUuid links (session_messages.go, session_chain.go).
// Ported from _internal/sessions.py and aligned with the TypeScript SDK
// (v0.3.286), whose behavior wins where the two differ;
// sessions_parity_test.go cross-checks the results against the TS runtime.
//
// The local implementations are methods of localSessions (see
// session_paths.go), which carries the projects directory explicitly so they
// can be exercised without touching the process environment; the exported
// functions resolve it from CLAUDE_CONFIG_DIR.

// ---------------------------------------------------------------------------
// Ordering and pagination
// ---------------------------------------------------------------------------

// cmpMTimeDesc orders Unix-millisecond times newest first.
func cmpMTimeDesc(a, b int64) int {
	switch {
	case a > b:
		return -1
	case a < b:
		return 1
	}
	return 0
}

// dedupeBySessionID keeps the newest LastModified per session id. Each id
// keeps the position of its first occurrence.
func dedupeBySessionID(sessions []SessionInfo) []SessionInfo {
	index := make(map[string]int, len(sessions))
	var out []SessionInfo
	for _, s := range sessions {
		i, ok := index[s.SessionID]
		if !ok {
			index[s.SessionID] = len(out)
			out = append(out, s)
		} else if s.LastModified > out[i].LastModified {
			out[i] = s
		}
	}
	return out
}

// sortSessionsLocal sorts local listings newest first, breaking ties by
// descending session id.
func sortSessionsLocal(sessions []SessionInfo) {
	slices.SortStableFunc(sessions, func(a, b SessionInfo) int {
		if c := cmpMTimeDesc(a.LastModified, b.LastModified); c != 0 {
			return c
		}
		return strings.Compare(b.SessionID, a.SessionID)
	})
}

// paginate applies the listing options' paging: it skips offset elements
// (when positive) and then keeps at most limit (when positive).
func paginate[T any](s []T, limit, offset int) []T {
	if offset > 0 {
		s = s[min(offset, len(s)):]
	}
	if limit > 0 && limit < len(s) {
		s = s[:limit]
	}
	return s
}

// ---------------------------------------------------------------------------
// Listing
// ---------------------------------------------------------------------------

// sessionCandidate is a transcript found while listing.
type sessionCandidate struct {
	sessionID string
	path      string
	// projectPath is the Cwd fallback and collision-filter reference;
	// empty when listing every project.
	projectPath string
	// ownWorktrees are the repository's worktrees (worktree listings only).
	ownWorktrees []string
}

// sessionCandidatesIn returns the <uuid>.jsonl transcripts of projectDir.
func sessionCandidatesIn(projectDir, projectPath string, ownWorktrees []string) []sessionCandidate {
	entries, err := os.ReadDir(projectDir)
	if err != nil {
		return nil
	}
	var out []sessionCandidate
	for _, e := range entries {
		sessionID, ok := strings.CutSuffix(e.Name(), ".jsonl")
		if !ok || !validateUUID(sessionID) {
			continue
		}
		out = append(out, sessionCandidate{
			sessionID:    sessionID,
			path:         filepath.Join(projectDir, e.Name()),
			projectPath:  projectPath,
			ownWorktrees: ownWorktrees,
		})
	}
	return out
}

// candidateSessionInfo lite-reads one candidate. It returns nil for
// sidechain and summary-less sessions, programmatic sessions when
// excludeProgrammatic is set, sessions continued in another existing
// session, and sessions recorded in a different directory that collides
// with the candidate's project name.
func candidateSessionInfo(c sessionCandidate, excludeProgrammatic bool) *SessionInfo {
	lite := readSessionLite(c.path)
	if lite == nil {
		return nil
	}
	if excludeProgrammatic && isProgrammaticSession(lite.head, lite.tail) {
		return nil
	}
	if next := continuedInSessionID(lite.tail); next != "" &&
		hasChainEntries(filepath.Join(filepath.Dir(c.path), next+".jsonl")) {
		return nil
	}
	info := parseSessionInfoFromLite(c.sessionID, lite, c.projectPath, liteSidecarTitle(lite, c.path, c.sessionID))
	if info == nil {
		return nil
	}
	if c.projectPath != "" {
		if cwd, ok := recordedSessionCwd(lite); ok && isForeignSession(cwd, c.projectPath, c.ownWorktrees) {
			return nil
		}
	}
	return info
}

// directoryCandidates collects the transcripts of a directory listing: the
// project directory of directory and, when includeWorktrees is set and the
// repository has several worktrees, the project directories of the others.
func (s localSessions) directoryCandidates(directory string, includeWorktrees bool) []sessionCandidate {
	canonical := canonicalizePath(directory)
	var worktrees []string
	if includeWorktrees {
		worktrees = getWorktreePaths(canonical)
	}
	if len(worktrees) > 1 {
		if out, ok := s.worktreeCandidates(canonical, worktrees); ok {
			return out
		}
	}
	return s.projectCandidates(canonical)
}

// projectCandidates collects the transcripts of the project directories of
// canonical (see findProjectDirs).
func (s localSessions) projectCandidates(canonical string) []sessionCandidate {
	caseInsensitive := runtime.GOOS == "windows"
	var out []sessionCandidate
	seen := map[string]bool{}
	for _, dir := range s.findProjectDirs(canonical) {
		if key := foldPath(dir, caseInsensitive); !seen[key] {
			seen[key] = true
			out = append(out, sessionCandidatesIn(dir, canonical, nil)...)
		}
	}
	return out
}

// worktreeCandidates collects the transcripts of canonical's project
// directories and of every project directory of root that belongs to one of
// worktrees. ok is false when root cannot be read.
func (s localSessions) worktreeCandidates(canonical string, worktrees []string) (out []sessionCandidate, ok bool) {
	caseInsensitive := runtime.GOOS == "windows"

	// Longest name first so more specific worktrees win.
	type worktreeName struct{ path, exact, truncatedPrefix string }
	names := make([]worktreeName, 0, len(worktrees))
	for _, wt := range worktrees {
		name := projectDirName(wt, s.dirNameOverride)
		w := worktreeName{path: wt, exact: foldPath(name, caseInsensitive)}
		if len(name) > maxSanitizedLength {
			w.truncatedPrefix = w.exact[:maxSanitizedLength]
		}
		names = append(names, w)
	}
	slices.SortStableFunc(names, func(a, b worktreeName) int { return len(b.exact) - len(a.exact) })

	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, false
	}
	own := append([]string{canonical}, worktrees...)
	seen := map[string]bool{}
	// The requested directory itself comes first: a subdirectory such as
	// /repo/packages/app does not match any worktree root.
	for _, dir := range s.findProjectDirs(canonical) {
		if name := foldPath(filepath.Base(dir), caseInsensitive); !seen[name] {
			seen[name] = true
			out = append(out, sessionCandidatesIn(dir, canonical, own)...)
		}
	}
	for _, e := range entries {
		name := foldPath(e.Name(), caseInsensitive)
		if seen[name] || !direntIsDir(s.root, e) {
			continue
		}
		dir := filepath.Join(s.root, e.Name())
		for _, wt := range names {
			// Prefix matching only applies to truncated names, which carry
			// a hash suffix, and is confirmed by the sessions' recorded cwd;
			// short names must match exactly so that /root/project does not
			// claim /root/project-foo.
			if name == wt.exact || (wt.truncatedPrefix != "" && strings.HasPrefix(name, wt.truncatedPrefix+"-") &&
				dirHasSessionForPath(dir, wt.path, caseInsensitive)) {
				seen[name] = true
				out = append(out, sessionCandidatesIn(dir, wt.path, own)...)
				break
			}
		}
	}
	return out, true
}

// listSessions implements ListSessions.
func (s localSessions) listSessions(opts *ListSessionsOptions) []SessionInfo {
	if opts == nil {
		opts = &ListSessionsOptions{}
	}
	var candidates []sessionCandidate
	if opts.Directory != "" {
		candidates = s.directoryCandidates(opts.Directory, !opts.ExcludeWorktrees)
	} else {
		for _, dir := range subdirs(s.root) {
			candidates = append(candidates, sessionCandidatesIn(dir, "", nil)...)
		}
	}
	var infos []SessionInfo
	for _, c := range candidates {
		if info := candidateSessionInfo(c, opts.ExcludeProgrammatic); info != nil {
			infos = append(infos, *info)
		}
	}
	infos = dedupeBySessionID(infos)
	sortSessionsLocal(infos)
	return nilIfEmpty(paginate(infos, opts.Limit, opts.Offset))
}

// ListSessions lists sessions with metadata extracted from a stat plus a
// head/tail read of each transcript; transcripts are never fully parsed.
//
// With opts.Directory set, it returns the sessions of that project
// directory and, unless opts.ExcludeWorktrees is set, of every other git
// worktree of its repository. Without it, it returns the sessions of every
// project. Left out are sidechain sessions, sessions with no title, summary
// or prompt, sessions continued in another session (a trailing
// "continued-in" entry whose target transcript exists), sessions recorded
// in a different directory whose sanitized name collides with the
// requested one, and with opts.ExcludeProgrammatic, SDK and daemon
// sessions. A session id found in several projects is reported once, with
// its newest copy.
//
// The title comes from the transcript or, when its tail has none, from a
// custom-title.json sidecar in the session's directory. When
// CLAUDE_CONFIG_DIR and CLAUDE_CODE_PROJECT_DIR_NAME are both set, the
// latter names the project directory of every working directory, as in the
// CLI.
//
// Results are sorted by LastModified, newest first (ties by descending
// session id); opts.Offset and opts.Limit then paginate them. opts may be
// nil. The transcript root is $CLAUDE_CONFIG_DIR/projects, defaulting to
// ~/.claude/projects. Unreadable files and directories are skipped, so the
// error is currently always nil.
func ListSessions(opts *ListSessionsOptions) ([]SessionInfo, error) {
	return localSessionsFromEnv().listSessions(opts), nil
}

// ---------------------------------------------------------------------------
// GetSessionInfo
// ---------------------------------------------------------------------------

// getSessionInfo implements GetSessionInfo.
func (s localSessions) getSessionInfo(sessionID, directory string) *SessionInfo {
	if !validateUUID(sessionID) {
		return nil
	}
	found, ok := s.findSessionFile(sessionID, directory)
	if !ok {
		return nil
	}
	lite := readSessionLite(found.path)
	if lite == nil {
		return nil
	}
	return parseSessionInfoFromLite(sessionID, lite, found.projectPath, liteSidecarTitle(lite, found.path, sessionID))
}

// GetSessionInfo reads the metadata of one session with a single head/tail
// read, without scanning the project. The title falls back to the
// session's custom-title.json sidecar and Cwd prefers the last relocation,
// as in ListSessions; unlike ListSessions, continued, programmatic and
// colliding sessions are not filtered out.
//
// directory is the project path, with the same semantics as
// ListSessionsOptions.Directory (git worktrees of the repository are
// searched too); empty searches every project. It returns (nil, nil) when
// sessionID is not a UUID, the transcript is not found, or the session is a
// sidechain or has no extractable summary. The error is currently always
// nil.
func GetSessionInfo(sessionID, directory string) (*SessionInfo, error) {
	return localSessionsFromEnv().getSessionInfo(sessionID, directory), nil
}
