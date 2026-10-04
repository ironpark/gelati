package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// SessionStore-backed resume. When Options.Resume or
// Options.ContinueConversation is paired with Options.SessionStore, the
// session transcript almost certainly does not exist on local disk: it lives
// in the store, and the CLI can only resume from a local file. The session is
// therefore loaded from the store and written to a temporary directory laid
// out like ~/.claude/, and the subprocess is pointed at it through
// CLAUDE_CONFIG_DIR. Ported from _internal/session_resume.py and
// _internal/session_store_validation.py.

// keychainServiceName is the macOS Keychain service holding the CLI's OAuth
// credentials when CLAUDE_CONFIG_DIR is unset.
const keychainServiceName = "Claude Code-credentials"

// keychainReadTimeout bounds the `security` invocation.
const keychainReadTimeout = 5 * time.Second

// resumeSettingsStrippedKeys are user-settings keys that misbehave under the
// redirected CLAUDE_CONFIG_DIR: plugin declarations reconcile against the
// always-empty plugin cache of the temp dir and would network-install every
// declared marketplace on each resume.
var resumeSettingsStrippedKeys = []string{"enabledPlugins", "extraKnownMarketplaces"}

// rmtreeRetries and rmtreeRetryDelay shape the cleanup retry loop.
const (
	rmtreeRetries    = 4
	rmtreeRetryDelay = 100 * time.Millisecond
)

// resumeEnv holds the process-level lookups resume materialization depends
// on, so tests can run in parallel without touching the real environment,
// home directory or Keychain. The zero value uses the real ones.
type resumeEnv struct {
	// lookupEnv reads the parent process environment.
	lookupEnv func(key string) (string, bool)
	// homeDir returns the user's home directory.
	homeDir func() (string, error)
	// readKeychain returns the CLI's OAuth credentials JSON from the macOS
	// Keychain, or false.
	readKeychain func(ctx context.Context) (string, bool)
	// tempDir is the parent of the materialized directory; empty means
	// os.TempDir().
	tempDir string
	// removeAll and retryDelay drive cleanup.
	removeAll  func(path string) error
	retryDelay time.Duration
}

func (r resumeEnv) withDefaults() resumeEnv {
	if r.lookupEnv == nil {
		r.lookupEnv = os.LookupEnv
	}
	if r.homeDir == nil {
		r.homeDir = userHomeDir
	}
	if r.readKeychain == nil {
		r.readKeychain = readKeychainCredentials
	}
	if r.removeAll == nil {
		r.removeAll = os.RemoveAll
	}
	if r.retryDelay == 0 {
		r.retryDelay = rmtreeRetryDelay
	}
	return r
}

// materializedResume is a session written to a temporary config directory.
type materializedResume struct {
	// configDir is laid out like ~/.claude/; the subprocess runs with
	// CLAUDE_CONFIG_DIR pointing at it.
	configDir string
	// resumeSessionID is passed as --resume. For ContinueConversation it is
	// the most recent main session found through SessionLister.
	resumeSessionID string

	removeAll  func(string) error
	retryDelay time.Duration
}

// cleanup removes configDir, best-effort. It is nil-safe and idempotent.
// Call it only after the subprocess has exited.
func (m *materializedResume) cleanup() {
	if m == nil {
		return
	}
	rmtreeWithRetry(m.configDir, m.removeAll, m.retryDelay)
}

// projectsDir is the directory the subprocess writes its transcripts under.
func (m *materializedResume) projectsDir() string {
	return filepath.Join(m.configDir, "projects")
}

// applyMaterializedOptions returns a copy of opts pointed at the
// materialized config directory: CLAUDE_CONFIG_DIR is set in Env, Resume is
// the materialized session and ContinueConversation is cleared (it has
// already been resolved to a concrete session). opts is not modified.
func applyMaterializedOptions(opts *Options, m *materializedResume) *Options {
	out := *opts
	out.Env = make(map[string]string, len(opts.Env)+1)
	maps.Copy(out.Env, opts.Env)
	out.Env["CLAUDE_CONFIG_DIR"] = m.configDir
	out.Resume = m.resumeSessionID
	out.ContinueConversation = false
	return &out
}

// validateSessionStoreOptions rejects SessionStore option combinations that
// cannot work, before anything is spawned.
func validateSessionStoreOptions(opts *Options) error {
	if opts == nil || opts.SessionStore == nil {
		return nil
	}
	if opts.ContinueConversation && opts.Resume == "" {
		// With Resume set, the session list is never consulted (Resume wins
		// over ContinueConversation), so a minimal store is fine.
		if _, ok := opts.SessionStore.(SessionLister); !ok {
			return errors.New("claude: Options.ContinueConversation with Options.SessionStore " +
				"requires the store to implement SessionLister (ListSessions)")
		}
	}
	if opts.EnableFileCheckpointing {
		return errors.New("claude: Options.SessionStore cannot be combined with " +
			"Options.EnableFileCheckpointing (checkpoints are local-disk only and " +
			"would diverge from the mirrored transcript)")
	}
	return nil
}

// materializeResumeSession loads the session to resume from opts.SessionStore
// and writes it to a temporary config directory.
//
// It returns nil when there is nothing to materialize: no store, neither
// Resume nor ContinueConversation, a Resume that is not a UUID, a session
// with no entries, or (for ContinueConversation) no main session at all. The
// caller then falls through to the normal resume or spawn path.
//
// Every store call is bounded by opts.LoadTimeout. A failing or timed-out
// call is returned as an error and no directory is left behind.
func materializeResumeSession(ctx context.Context, opts *Options, env resumeEnv) (*materializedResume, error) {
	if opts == nil || opts.SessionStore == nil {
		return nil, nil
	}
	if opts.Resume == "" && !opts.ContinueConversation {
		return nil, nil
	}
	env = env.withDefaults()
	store := opts.SessionStore
	timeout := opts.LoadTimeout
	if timeout <= 0 {
		timeout = DefaultSessionLoadTimeout
	}
	// The key honors CLAUDE_CODE_PROJECT_DIR_NAME like the CLI, reading
	// the environment the subprocess will see: Options.Env over the
	// parent's.
	projectKey := projectKeyForDirectory(opts.Cwd, func(k string) string {
		if v, ok := opts.Env[k]; ok {
			return v
		}
		v, _ := env.lookupEnv(k)
		return v
	})

	// An explicit Resume wins; otherwise pick the most recent main session.
	var (
		sessionID string
		entries   []SessionStoreEntry
		err       error
	)
	if opts.Resume != "" {
		// The id becomes a path component below: anything but a UUID is
		// passed to the CLI unchanged instead.
		if !validateUUID(opts.Resume) {
			return nil, nil
		}
		sessionID = opts.Resume
		entries, err = loadResumeCandidate(ctx, store, projectKey, sessionID, timeout)
	} else {
		sessionID, entries, err = resolveContinueCandidate(ctx, store, projectKey, timeout)
	}
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, nil
	}

	tmpBase, err := os.MkdirTemp(env.tempDir, "claude-resume-")
	if err != nil {
		return nil, fmt.Errorf("claude: creating resume directory: %w", err)
	}
	m := &materializedResume{
		configDir:       tmpBase,
		resumeSessionID: sessionID,
		removeAll:       env.removeAll,
		retryDelay:      env.retryDelay,
	}
	if err := populateResumeDir(ctx, m, store, opts, env, projectKey, entries, timeout); err != nil {
		// The directory may already hold a credentials copy, and the caller
		// has no handle on it: remove it before returning.
		m.cleanup()
		return nil, err
	}
	return m, nil
}

// populateResumeDir writes the transcript, auth files and subagent
// transcripts into m.configDir.
func populateResumeDir(ctx context.Context, m *materializedResume, store SessionStore, opts *Options,
	env resumeEnv, projectKey string, entries []SessionStoreEntry, timeout time.Duration,
) error {
	projectDir := filepath.Join(m.projectsDir(), projectKey)
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		return fmt.Errorf("claude: creating resume directory: %w", err)
	}
	if err := writeEntriesJSONL(filepath.Join(projectDir, m.resumeSessionID+".jsonl"), entries); err != nil {
		return err
	}

	// The subprocess runs with CLAUDE_CONFIG_DIR=configDir; seed it with the
	// caller's auth and user config so it can authenticate. Missing files are
	// fine (API-key auth and the like).
	if err := copyAuthFiles(ctx, m.configDir, opts.Env, env); err != nil {
		return err
	}

	// Subagent transcripts, if the store can enumerate them.
	if lister, ok := store.(SessionSubkeyLister); ok {
		return materializeSubkeys(ctx, store, lister, projectDir, projectKey, m.resumeSessionID, timeout)
	}
	return nil
}

// loadResumeCandidate loads the entries of one session; nil means it is
// empty or missing.
func loadResumeCandidate(ctx context.Context, store SessionStore, projectKey, sessionID string, timeout time.Duration) ([]SessionStoreEntry, error) {
	return callWithLoadTimeout(ctx, timeout, "SessionStore.Load() for session "+sessionID,
		func(ctx context.Context) ([]SessionStoreEntry, error) {
			return store.Load(ctx, SessionKey{ProjectKey: projectKey, SessionID: sessionID})
		})
}

// resolveContinueCandidate picks the most recently modified main session.
//
// Sidechain transcripts are mirrored as ordinary top-level keys and often
// have the newest mtime (their append lands after the main session's in the
// same flush). Candidates are walked newest first, loading each one (the load
// is needed anyway) and skipping sidechains, so ContinueConversation resumes
// the user's conversation rather than a subagent's, as the CLI's own
// --continue does. Equal mtimes keep the store's listing order.
func resolveContinueCandidate(ctx context.Context, store SessionStore, projectKey string, timeout time.Duration) (string, []SessionStoreEntry, error) {
	lister, ok := store.(SessionLister)
	if !ok {
		return "", nil, fmt.Errorf("claude: SessionStore.ListSessions() failed during resume materialization: %w",
			errors.ErrUnsupported)
	}
	sessions, err := callWithLoadTimeout(ctx, timeout, "SessionStore.ListSessions()",
		func(ctx context.Context) ([]SessionStoreListEntry, error) {
			return lister.ListSessions(ctx, projectKey)
		})
	if err != nil || len(sessions) == 0 {
		return "", nil, err
	}
	sorted := slices.Clone(sessions)
	slices.SortStableFunc(sorted, func(a, b SessionStoreListEntry) int { return cmpMTimeDesc(a.MTime, b.MTime) })
	for _, cand := range sorted {
		if !validateUUID(cand.SessionID) {
			continue
		}
		entries, err := loadResumeCandidate(ctx, store, projectKey, cand.SessionID, timeout)
		if err != nil {
			return "", nil, err
		}
		if len(entries) == 0 {
			continue
		}
		if first := entries[0]; first != nil && first["isSidechain"] == true {
			continue
		}
		return cand.SessionID, entries, nil
	}
	return "", nil, nil
}

// callWithLoadTimeout runs one store call bounded by timeout (see
// runStoreCall), so a store that ignores its context cannot hang the resume.
// Failures are wrapped with what and the materialization context.
func callWithLoadTimeout[T any](ctx context.Context, timeout time.Duration, what string, call func(context.Context) (T, error)) (T, error) {
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, fmt.Errorf("claude: %s during resume materialization: %w", what, err)
	}
	v, err := runStoreCall(ctx, timeout, call)
	switch {
	case err == nil:
	case errors.Is(err, errStoreTimeout):
		return zero, fmt.Errorf("claude: %s timed out after %dms during resume materialization: %w",
			what, timeout.Milliseconds(), context.DeadlineExceeded)
	case err == ctx.Err():
		return zero, fmt.Errorf("claude: %s during resume materialization: %w", what, err)
	case err != nil:
		return zero, fmt.Errorf("claude: %s failed during resume materialization: %w", what, err)
	}
	return v, nil
}

// ---------------------------------------------------------------------------
// Auth and user config
// ---------------------------------------------------------------------------

// copyAuthFiles seeds tmpBase with the caller's auth and user config:
// .credentials.json (refresh token removed), .claude.json and the user
// settings.json / cowork_settings.json (plugin declarations removed).
//
// Sources are resolved as the CLI resolves them: .credentials.json and the
// settings files live in the config dir (CLAUDE_CONFIG_DIR, else ~/.claude),
// while .claude.json lives at $CLAUDE_CONFIG_DIR/.claude.json when that is
// set and at ~/.claude.json (not ~/.claude/.claude.json) otherwise. optEnv
// (Options.Env) is consulted before the process environment.
func copyAuthFiles(ctx context.Context, tmpBase string, optEnv map[string]string, env resumeEnv) error {
	callerConfigDir := optEnv["CLAUDE_CONFIG_DIR"]
	callerConfigDirSet := callerConfigDir != ""
	if !callerConfigDirSet {
		callerConfigDir, callerConfigDirSet = env.lookupEnv("CLAUDE_CONFIG_DIR")
	}
	// envSet reports whether key is set to a non-empty value.
	envSet := func(key string) bool {
		if optEnv[key] != "" {
			return true
		}
		v, _ := env.lookupEnv(key)
		return v != ""
	}
	home, homeErr := env.homeDir()
	homeOK := homeErr == nil && home != ""

	sourceConfigDir := callerConfigDir
	if sourceConfigDir == "" && homeOK {
		sourceConfigDir = filepath.Join(home, ".claude")
	}

	var creds []byte
	if sourceConfigDir != "" {
		creds = readIfPresent(filepath.Join(sourceConfigDir, ".credentials.json"))
	}

	// The default macOS setup keeps OAuth tokens in the Keychain, not a file.
	// Redirecting CLAUDE_CONFIG_DIR changes the Keychain service-name suffix,
	// so the subprocess's lookup would miss and fall back to
	// <configDir>/.credentials.json: populate that file from the parent's
	// Keychain. Skipped when env-based auth or a custom config dir is in play.
	if !callerConfigDirSet && !envSet("ANTHROPIC_API_KEY") && !envSet("CLAUDE_CODE_OAUTH_TOKEN") {
		if keychain, ok := env.readKeychain(ctx); ok {
			creds = []byte(keychain)
		}
	}
	if err := writeRedactedCredentials(creds, filepath.Join(tmpBase, ".credentials.json")); err != nil {
		return err
	}

	claudeJSON := ""
	switch {
	case callerConfigDir != "":
		claudeJSON = filepath.Join(callerConfigDir, ".claude.json")
	case homeOK:
		claudeJSON = filepath.Join(home, ".claude.json")
	}
	if claudeJSON != "" {
		copyIfPresent(claudeJSON, filepath.Join(tmpBase, ".claude.json"), nil)
	}

	// User settings carry apiKeyHelper (an auth mechanism of its own) plus
	// env, hooks and permissions; without them an apiKeyHelper-only host
	// fails with "Not logged in". cowork_settings.json is the file the CLI
	// reads instead in cowork-plugins mode.
	if sourceConfigDir != "" {
		for _, name := range []string{"settings.json", "cowork_settings.json"} {
			copyIfPresent(filepath.Join(sourceConfigDir, name), filepath.Join(tmpBase, name), stripSettingsForResume)
		}
	}
	return nil
}

// stripSettingsForResume drops the settings keys that misbehave under a
// redirected config dir: resumeSettingsStrippedKeys and env.CLAUDE_CONFIG_DIR
// (which would point the subprocess's config reads away from the temp dir).
// Content that is not a JSON object, or that holds a number too large for a
// double, is returned untouched so the subprocess reads exactly what the CLI
// would have read. A UTF-8 byte-order mark is tolerated.
func stripSettingsForResume(content []byte) []byte {
	text := bytes.TrimPrefix(content, []byte("\xef\xbb\xbf"))
	if !utf8.Valid(text) {
		return content
	}
	parsed, ok := decodeJSONObject(text)
	if !ok {
		return content
	}
	stripped := false
	for _, key := range resumeSettingsStrippedKeys {
		if _, ok := parsed[key]; ok {
			delete(parsed, key)
			stripped = true
		}
	}
	if envBlock, ok := parsed["env"].(map[string]any); ok {
		if _, ok := envBlock["CLAUDE_CONFIG_DIR"]; ok {
			delete(envBlock, "CLAUDE_CONFIG_DIR")
			stripped = true
		}
	}
	if !stripped || hasOverflowingNumber(parsed) {
		// An overflowing number would re-serialize as a value the CLI
		// rejects in Python; keep the original bytes as the Python SDK does.
		return content
	}
	out, err := newJSONAppender().append(nil, parsed)
	if err != nil {
		return content
	}
	return out
}

// decodeJSONObject parses b as exactly one JSON object, keeping numbers as
// json.Number so they re-serialize unchanged.
func decodeJSONObject(b []byte) (map[string]any, bool) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil || obj == nil {
		return nil, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, false // trailing data
	}
	return obj, true
}

// hasOverflowingNumber reports whether v holds a number outside the range of
// a double.
func hasOverflowingNumber(v any) bool {
	switch x := v.(type) {
	case json.Number:
		// Integers are arbitrary-precision in Python; only floats overflow.
		if strings.ContainsAny(string(x), ".eE") {
			if _, err := strconv.ParseFloat(string(x), 64); err != nil {
				return true
			}
		}
	case map[string]any:
		for _, item := range x {
			if hasOverflowingNumber(item) {
				return true
			}
		}
	case []any:
		for _, item := range x {
			if hasOverflowingNumber(item) {
				return true
			}
		}
	}
	return false
}

// writeRedactedCredentials writes creds to dst (0600) without
// claudeAiOauth.refreshToken. The resumed subprocess runs under a redirected
// config dir: if it refreshed, the single-use refresh token would be
// consumed server-side and the new tokens written where the parent never
// reads them, revoking the parent's stored credentials. Without a refresh
// token the subprocess never tries. Unparseable content is written through.
// nil creds writes nothing.
func writeRedactedCredentials(creds []byte, dst string) error {
	if creds == nil {
		return nil
	}
	out := creds
	if data, ok := decodeJSONObject(creds); ok {
		if oauth, ok := data["claudeAiOauth"].(map[string]any); ok {
			if _, ok := oauth["refreshToken"]; ok {
				delete(oauth, "refreshToken")
				if b, err := json.Marshal(data); err == nil {
					out = b
				}
			}
		}
	}
	if err := os.WriteFile(dst, out, 0o600); err != nil {
		return fmt.Errorf("claude: writing %s: %w", dst, err)
	}
	_ = os.Chmod(dst, 0o600)
	return nil
}

// readIfPresent reads a regular file. A missing file, and anything that
// cannot be read as a regular file (permission denied, a directory or FIFO in
// its place), yields nil: these files only enrich the temp config dir, so an
// unreadable one must not abort (or, for a FIFO, hang) the resume.
func readIfPresent(src string) []byte {
	info, err := os.Stat(src)
	if err != nil || !info.Mode().IsRegular() {
		return nil
	}
	b, err := os.ReadFile(src)
	if err != nil {
		return nil
	}
	return b
}

// copyIfPresent copies src to dst (0600) through an optional transform when
// src is a readable regular file. A failed write removes dst rather than
// leaving a truncated file for the subprocess to misparse.
func copyIfPresent(src, dst string, transform func([]byte) []byte) {
	content := readIfPresent(src)
	if content == nil {
		return
	}
	if transform != nil {
		content = transform(content)
	}
	if err := os.WriteFile(dst, content, 0o600); err != nil {
		_ = os.Remove(dst)
		return
	}
	_ = os.Chmod(dst, 0o600)
}

// readKeychainCredentials reads the CLI's OAuth credentials JSON from the
// macOS Keychain under the default service name. It is best-effort: any
// failure, and every other platform, reports false.
func readKeychainCredentials(ctx context.Context) (string, bool) {
	if runtime.GOOS != "darwin" {
		return "", false
	}
	account := os.Getenv("USER")
	if account == "" {
		if u, err := user.Current(); err == nil && u.Username != "" {
			account = u.Username
		} else {
			account = "claude-code-user"
		}
	}
	ctx, cancel := context.WithTimeout(ctx, keychainReadTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "security", "find-generic-password",
		"-a", account, "-w", "-s", keychainServiceName).Output()
	if err != nil {
		return "", false
	}
	creds := strings.TrimSpace(string(out))
	return creds, creds != ""
}

// ---------------------------------------------------------------------------
// Subagent transcripts
// ---------------------------------------------------------------------------

// materializeSubkeys loads every subagent transcript and metadata sidecar
// stored under sessionID and writes them beneath the session directory.
func materializeSubkeys(ctx context.Context, store SessionStore, lister SessionSubkeyLister,
	projectDir, projectKey, sessionID string, timeout time.Duration,
) error {
	sessionDir := filepath.Join(projectDir, sessionID)
	subkeys, err := callWithLoadTimeout(ctx, timeout, "SessionStore.ListSubkeys() for session "+sessionID,
		func(ctx context.Context) ([]string, error) {
			return lister.ListSubkeys(ctx, SessionListSubkeysKey{ProjectKey: projectKey, SessionID: sessionID})
		})
	if err != nil {
		return err
	}
	for _, subpath := range subkeys {
		// Subpaths come from an external store and become path components:
		// anything that could escape the session directory is skipped.
		if !isSafeSubpath(subpath, sessionDir) {
			continue
		}
		key := SessionKey{ProjectKey: projectKey, SessionID: sessionID, Subpath: subpath}
		entries, err := callWithLoadTimeout(ctx, timeout,
			fmt.Sprintf("SessionStore.Load() for session %s subpath %s", sessionID, subpath),
			func(ctx context.Context) ([]SessionStoreEntry, error) { return store.Load(ctx, key) })
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			continue
		}

		// agent_metadata entries stand for the .meta.json sidecar (the last
		// one wins); everything else is a transcript line.
		metadata, transcript := splitAgentMetadata(entries)
		subFile := subpathTranscriptFile(sessionDir, subpath)
		if len(transcript) > 0 {
			if err := writeEntriesJSONL(subFile, transcript); err != nil {
				return err
			}
		}
		if metadata != nil {
			delete(metadata, "type") // the synthetic discriminator
			b, err := json.Marshal(metadata)
			if err != nil {
				return fmt.Errorf("claude: encoding agent metadata for %s: %w", subpath, err)
			}
			metaFile := agentMetadataSidecarPath(subFile)
			if err := os.MkdirAll(filepath.Dir(metaFile), 0o700); err != nil {
				return fmt.Errorf("claude: writing %s: %w", metaFile, err)
			}
			if err := os.WriteFile(metaFile, b, 0o600); err != nil {
				return fmt.Errorf("claude: writing %s: %w", metaFile, err)
			}
			_ = os.Chmod(metaFile, 0o600)
		}
	}
	return nil
}

// subpathTranscriptFile is the .jsonl file a subpath is written to. It is the
// single expression shared by the writer and isSafeSubpath, so the validated
// path cannot drift from the written one.
func subpathTranscriptFile(sessionDir, subpath string) string {
	return filepath.Join(sessionDir, filepath.FromSlash(subpath)) + ".jsonl"
}

// isSafeSubpath rejects subpaths that are empty, absolute, drive-prefixed,
// contain a "." or ".." component (under either separator) or a NUL byte, or
// whose transcript file resolves outside sessionDir.
func isSafeSubpath(subpath, sessionDir string) bool {
	if subpath == "" {
		return false
	}
	if filepath.IsAbs(subpath) || strings.HasPrefix(subpath, "/") || strings.HasPrefix(subpath, `\`) {
		return false
	}
	// Drive-prefixed ("C:foo") subpaths are never legitimate store keys; they
	// are rejected on every OS so a Windows consumer is protected even if
	// the store was populated elsewhere. Like ntpath.splitdrive, any
	// character before the colon counts.
	if r := []rune(subpath); len(r) >= 2 && r[1] == ':' {
		return false
	}
	for _, part := range strings.FieldsFunc(subpath, func(r rune) bool { return r == '/' || r == '\\' }) {
		if part == "." || part == ".." {
			return false
		}
	}
	if strings.ContainsRune(subpath, 0) {
		return false
	}
	target, err := realpath(subpathTranscriptFile(sessionDir, subpath))
	if err != nil {
		return false
	}
	base, err := realpath(sessionDir)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(base, target)
	if err != nil || rel == "." || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// Cleanup
// ---------------------------------------------------------------------------

// rmtreeWithRetry removes path, retrying on errors that indicate a briefly
// held handle (on Windows an AV scanner or indexer can lock a freshly
// written .credentials.json) so the access token copy does not leak. After
// the retries, or on any other error, a final best-effort removal runs. It
// never fails.
func rmtreeWithRetry(path string, removeAll func(string) error, delay time.Duration) {
	if path == "" {
		return
	}
	if removeAll == nil {
		removeAll = os.RemoveAll
	}
	if _, err := os.Lstat(path); err != nil {
		return
	}
	for range rmtreeRetries {
		err := removeAll(path)
		if err == nil {
			return
		}
		if !isRetryableRemoveError(err) {
			break
		}
		time.Sleep(delay)
	}
	_ = removeAll(path)
}

// isRetryableRemoveError reports whether a removal failure looks transient.
func isRetryableRemoveError(err error) bool {
	if errors.Is(err, fs.ErrPermission) {
		return true
	}
	for _, errno := range []syscall.Errno{syscall.EBUSY, syscall.EMFILE, syscall.ENFILE, syscall.ENOTEMPTY, syscall.EPERM, syscall.EACCES} {
		if errors.Is(err, errno) {
			return true
		}
	}
	if runtime.GOOS == "windows" {
		// ERROR_SHARING_VIOLATION and ERROR_LOCK_VIOLATION, which Python
		// maps to EACCES.
		var errno syscall.Errno
		if errors.As(err, &errno) && (errno == 32 || errno == 33) {
			return true
		}
	}
	return false
}
