package claude

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
)

// Session mutations: rename, tag, delete and fork, for local transcripts and
// for SessionStore-backed sessions. Ported from
// _internal/session_mutations.py.
//
// Rename and tag append typed metadata entries to the session's JSONL, as the
// CLI does; readers take the last such entry, so repeated calls are safe.
// Delete removes the transcript; fork writes a new session with remapped
// uuids.
//
// Concurrent writers: if the target session is open in a CLI process, the
// CLI tail-reads the transcript before re-appending its cached metadata. An
// SDK entry (such as a custom title) within its tail scan window is absorbed
// into the CLI's cache and re-appended, so the SDK value is not lost.

// forkDroppedKeys are removed from forked entries: they would leak state
// from the source session.
var forkDroppedKeys = []string{"teamName", "agentName", "slug", "sourceToolAssistantUUID"}

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

func invalidSessionIDError(sessionID string) error {
	return fmt.Errorf("%w: %q", ErrInvalidSessionID, sessionID)
}

func sessionNotFoundError(sessionID, directory string) error {
	if directory != "" {
		return fmt.Errorf("%w: %s in project directory for %s", ErrSessionNotFound, sessionID, directory)
	}
	return fmt.Errorf("%w: %s", ErrSessionNotFound, sessionID)
}

func noMessagesToForkError(sessionID string) error {
	return fmt.Errorf("claude: session %s has no messages to fork", sessionID)
}

// validateUpToMessageID checks ForkSessionOptions.UpToMessageID.
func validateUpToMessageID(id string) error {
	if id != "" && !validateUUID(id) {
		return fmt.Errorf("claude: invalid up-to message id %q: not a UUID", id)
	}
	return nil
}

var (
	errEmptySessionTitle = errors.New("claude: session title must be non-empty")
	errEmptySessionTag   = errors.New(`claude: session tag must be non-empty after sanitization (pass "" to clear the tag)`)
)

// normalizeSessionTitle strips a title and rejects an empty result.
func normalizeSessionTitle(title string) (string, error) {
	stripped := pyStrip(title)
	if stripped == "" {
		return "", errEmptySessionTitle
	}
	return stripped, nil
}

// normalizeSessionTag sanitizes and strips a tag. An empty tag clears it and
// is returned as-is; a non-empty tag that sanitizes to nothing is an error.
func normalizeSessionTag(tag string) (string, error) {
	if tag == "" {
		return "", nil
	}
	sanitized := pyStrip(sanitizeUnicode(tag))
	if sanitized == "" {
		return "", errEmptySessionTag
	}
	return sanitized, nil
}

// ---------------------------------------------------------------------------
// Rename and tag, shared by the filesystem and SessionStore paths
// ---------------------------------------------------------------------------

// metadataSink appends one typed metadata entry, given as ordered fields, to
// a session: to its local transcript or to a SessionStore.
type metadataSink interface {
	appendMetadata(sessionID string, fields []jsonField) error
}

// renameSessionTo validates and normalizes a rename and appends the
// custom-title entry to sink.
func renameSessionTo(sink metadataSink, sessionID, title string) error {
	return appendMetadataEntry(sink, sessionID, "custom-title", "customTitle", title, normalizeSessionTitle)
}

// tagSessionTo validates and normalizes a tag and appends the tag entry to
// sink.
func tagSessionTo(sink metadataSink, sessionID, tag string) error {
	return appendMetadataEntry(sink, sessionID, "tag", "tag", tag, normalizeSessionTag)
}

// appendMetadataEntry validates sessionID, normalizes value and appends a
// {"type":typ,key:value,"sessionId":sessionID} entry to sink.
func appendMetadataEntry(sink metadataSink, sessionID, typ, key, value string,
	normalize func(string) (string, error),
) error {
	if !validateUUID(sessionID) {
		return invalidSessionIDError(sessionID)
	}
	value, err := normalize(value)
	if err != nil {
		return err
	}
	return sink.appendMetadata(sessionID, []jsonField{
		{"type", typ},
		{key, value},
		{"sessionId", sessionID},
	})
}

// localMetadataSink appends metadata entries as JSONL lines to the local
// transcript of a session (see appendToSession).
type localMetadataSink struct {
	sessions  localSessions
	directory string
}

func (k localMetadataSink) appendMetadata(sessionID string, fields []jsonField) error {
	line, err := pyJSONObject(fields)
	if err != nil {
		return err
	}
	return k.sessions.appendToSession(sessionID, line+"\n", k.directory)
}

// storeMetadataSink appends metadata entries to a SessionStore under the
// project key of directory. Each entry gets a fresh uuid and timestamp for
// the store's idempotency contract.
type storeMetadataSink struct {
	ctx       context.Context
	store     SessionStore
	directory string
}

func (k storeMetadataSink) appendMetadata(sessionID string, fields []jsonField) error {
	entry := make(SessionStoreEntry, len(fields)+2)
	for _, f := range fields {
		entry[f.key] = f.value
	}
	entry["uuid"] = randomUUID()
	entry["timestamp"] = pyISONowUTC()
	key := SessionKey{ProjectKey: ProjectKeyForDirectory(k.directory), SessionID: sessionID}
	return k.store.Append(k.ctx, key, []SessionStoreEntry{entry})
}

// ---------------------------------------------------------------------------
// Local transcripts
// ---------------------------------------------------------------------------

// RenameSession renames a session by appending a custom-title entry to its
// transcript. ListSessions reads the last custom title, so the most recent
// rename wins.
//
// title is stripped of leading and trailing whitespace and must not be empty
// afterwards. directory is the project path, with the same semantics as
// ListSessionsOptions.Directory (git worktrees of the repository are searched
// too); empty searches every project. Empty (0-byte) transcripts are skipped
// during the search.
//
// The entry is appended with a single O_APPEND write, so it lands atomically
// at the end of the file even while the CLI is writing to it. It returns an
// error wrapping ErrInvalidSessionID when sessionID is not a UUID and one
// wrapping ErrSessionNotFound when no transcript is found; I/O errors are
// returned as-is.
func RenameSession(sessionID, title, directory string) error {
	return localSessionsFromEnv().renameSession(sessionID, title, directory)
}

// renameSession implements RenameSession.
func (s localSessions) renameSession(sessionID, title, directory string) error {
	return renameSessionTo(localMetadataSink{s, directory}, sessionID, title)
}

// TagSession tags a session by appending a tag entry to its transcript;
// ListSessions reports the last one. An empty tag clears the tag (Python's
// tag=None): it appends an entry with an empty tag, which readers treat as
// no tag.
//
// A non-empty tag is Unicode-sanitized for compatibility with the CLI's
// filters (NFKC normalization; format, private-use and unassigned code
// points such as zero-width characters and directional marks are removed)
// and stripped of surrounding whitespace. If nothing is left, for example
// for a whitespace-only tag, an error is returned rather than clearing the
// tag. Invalid UTF-8 is replaced with U+FFFD.
//
// directory and the errors are as for RenameSession.
func TagSession(sessionID, tag, directory string) error {
	return localSessionsFromEnv().tagSession(sessionID, tag, directory)
}

// tagSession implements TagSession.
func (s localSessions) tagSession(sessionID, tag, directory string) error {
	return tagSessionTo(localMetadataSink{s, directory}, sessionID, tag)
}

// DeleteSession permanently deletes a session's transcript, together with
// the sibling <sessionID>/ directory holding its subagent transcripts when
// there is one. For a reversible delete, tag the session (for example
// "__hidden") and filter it out when listing instead.
//
// directory is as for RenameSession. Failure to remove the subagent
// directory is ignored. It returns an error wrapping ErrInvalidSessionID
// when sessionID is not a UUID and one wrapping ErrSessionNotFound when no
// non-empty transcript is found.
func DeleteSession(sessionID, directory string) error {
	return localSessionsFromEnv().deleteSession(sessionID, directory)
}

// deleteSession implements DeleteSession.
func (s localSessions) deleteSession(sessionID, directory string) error {
	if !validateUUID(sessionID) {
		return invalidSessionIDError(sessionID)
	}
	path := s.resolveSessionFilePath(sessionID, directory)
	if path == "" {
		return sessionNotFoundError(sessionID, directory)
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: %s: %w", ErrSessionNotFound, sessionID, err)
		}
		return err
	}
	// Subagent transcripts live in a sibling <sessionID>/ directory, which
	// is often absent. Like Python's rmtree, only a real directory is
	// removed: not a symlink, not a file of that name.
	subDir := filepath.Join(filepath.Dir(path), sessionID)
	if fi, err := os.Lstat(subDir); err == nil && fi.IsDir() {
		_ = os.RemoveAll(subDir)
	}
	return nil
}

// ForkSession forks a session into a new session with fresh uuids, written
// next to the source transcript, and returns the new session's id.
//
// The transcript messages are copied with every uuid remapped and the
// parentUuid chain preserved (progress entries are skipped and their
// children re-linked to the nearest kept ancestor). Sidechain (subagent)
// entries and file-history snapshots are not copied, so the fork starts
// without undo history. Each copied entry records its origin in
// "forkedFrom"; only the last one gets a fresh timestamp. The fork ends with
// a custom-title entry.
//
// opts may be nil. opts.UpToMessageID, when set, must be a UUID and keeps
// the transcript up to and including that message. opts.Title is the fork's
// title; when empty or blank it is derived from the source's custom title,
// AI title or first prompt (in that order) plus " (fork)". opts.Directory is
// as for RenameSession.
//
// It returns an error wrapping ErrInvalidSessionID when sessionID is not a
// UUID and one wrapping ErrSessionNotFound when the transcript is not found;
// a source without forkable messages or without the UpToMessageID message
// is an error too.
func ForkSession(sessionID string, opts *ForkSessionOptions) (*ForkSessionResult, error) {
	return localSessionsFromEnv().forkSession(sessionID, opts)
}

// forkSession implements ForkSession.
func (s localSessions) forkSession(sessionID string, opts *ForkSessionOptions) (*ForkSessionResult, error) {
	if opts == nil {
		opts = &ForkSessionOptions{}
	}
	if !validateUUID(sessionID) {
		return nil, invalidSessionIDError(sessionID)
	}
	if err := validateUpToMessageID(opts.UpToMessageID); err != nil {
		return nil, err
	}
	filePath := s.resolveSessionFilePath(sessionID, opts.Directory)
	if filePath == "" {
		return nil, sessionNotFoundError(sessionID, opts.Directory)
	}
	content, err := os.ReadFile(filePath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s: %w", ErrSessionNotFound, sessionID, err)
		}
		return nil, err
	}
	if len(content) == 0 {
		return nil, noMessagesToForkError(sessionID)
	}

	transcript, replacements := parseForkTranscript(decodeUTF8Replace(content), sessionID)
	deriveTitle := func() string {
		lite := jsonlToLite(content, 0)
		return cmp.Or(liteTitle(lite, ""), extractFirstPromptFromHead(lite.head))
	}
	forkedID, lines, err := buildForkLines(transcript, replacements, sessionID, opts.UpToMessageID, opts.Title, deriveTitle)
	if err != nil {
		return nil, err
	}

	forkPath := filepath.Join(filepath.Dir(filePath), forkedID+".jsonl")
	f, err := os.OpenFile(forkPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	_, werr := f.WriteString(strings.Join(lines, "\n") + "\n")
	if err := errors.Join(werr, f.Close()); err != nil {
		// Do not leave a truncated session behind.
		_ = os.Remove(forkPath)
		return nil, err
	}
	return &ForkSessionResult{SessionID: forkedID}, nil
}

// appendToSession appends data to the transcript of sessionID, trying the
// candidate files in search order: the project directory of directory and
// its git worktrees when directory is set, otherwise every entry of the
// projects directory. There is no separate existence check; see tryAppend.
func (s localSessions) appendToSession(sessionID, data, directory string) error {
	fileName := sessionID + ".jsonl"
	if directory != "" {
		canonical := canonicalizePath(directory)
		if projectDir := s.findProjectDir(canonical); projectDir != "" {
			if ok, err := tryAppend(filepath.Join(projectDir, fileName), data); ok || err != nil {
				return err
			}
		}
		// Sessions may live under another worktree of the repository.
		for _, wt := range getWorktreePaths(canonical) {
			if wt == canonical {
				continue // already tried
			}
			if projectDir := s.findProjectDir(wt); projectDir != "" {
				if ok, err := tryAppend(filepath.Join(projectDir, fileName), data); ok || err != nil {
					return err
				}
			}
		}
		return sessionNotFoundError(sessionID, directory)
	}

	entries, err := os.ReadDir(s.root)
	if err != nil {
		return fmt.Errorf("%w: %s (no projects directory: %w)", ErrSessionNotFound, sessionID, err)
	}
	for _, e := range entries {
		if ok, err := tryAppend(filepath.Join(s.root, e.Name(), fileName), data); ok || err != nil {
			return err
		}
	}
	return fmt.Errorf("%w: %s in any project directory", ErrSessionNotFound, sessionID)
}

// tryAppend appends data to the file at path. It reports false, without
// error, when the file (or a parent directory) does not exist or is empty:
// a 0-byte transcript is a "not here, keep searching" stub, as for the
// readers. Other errors are returned.
//
// The file is opened O_WRONLY|O_APPEND without O_CREATE, so a missing file
// is never created and there is no check-then-open race. With O_APPEND the
// kernel moves to end-of-file atomically on every write (FILE_APPEND_DATA
// on Windows), so concurrent appenders cannot overwrite each other.
func tryAppend(path, data string) (bool, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			return false, nil
		}
		return false, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return false, err
	}
	if fi.Size() == 0 {
		f.Close()
		return false, nil
	}
	if _, err := f.WriteString(data); err != nil {
		f.Close()
		return false, err
	}
	return true, f.Close()
}

// ---------------------------------------------------------------------------
// Fork transform, shared by the filesystem and SessionStore paths
// ---------------------------------------------------------------------------

// forkEntry is a source entry of a fork.
type forkEntry struct {
	// fields is the decoded entry, used for field access.
	fields map[string]any
	// keys is the source key order; nil when unknown (store entries).
	keys []string
	// raw holds the source JSON of each value so unchanged fields are
	// copied verbatim (number literals, nested key order); nil for store
	// entries.
	raw map[string]json.RawMessage
}

// value returns the value to serialize for key k.
func (e forkEntry) value(k string) any {
	if r, ok := e.raw[k]; ok {
		return r
	}
	return e.fields[k]
}

// sourceKeys returns the key order to serialize e in: the source order, or
// "type" first and the other keys sorted when it is unknown.
func (e forkEntry) sourceKeys() []string {
	if e.keys != nil {
		return e.keys
	}
	return typeFirstKeys(e.fields)
}

// marshal serializes e like Python's {**entry, **overrides} with the dropped
// keys popped: overridden keys keep their source position, new keys follow
// in override order.
func (e forkEntry) marshal(overrides []jsonField, drop []string) (string, error) {
	keys := e.sourceKeys()
	over := make(map[string]any, len(overrides))
	for _, o := range overrides {
		over[o.key] = o.value
	}
	fields := make([]jsonField, 0, len(keys)+len(overrides))
	seen := make(map[string]bool, len(keys))
	for _, k := range keys {
		seen[k] = true
		if slices.Contains(drop, k) {
			continue
		}
		if v, ok := over[k]; ok {
			fields = append(fields, jsonField{k, v})
		} else {
			fields = append(fields, jsonField{k, e.value(k)})
		}
	}
	for _, o := range overrides {
		if !seen[o.key] && !slices.Contains(drop, o.key) {
			fields = append(fields, o)
		}
	}
	return pyJSONObject(fields)
}

// parseForkLine parses one JSONL line, keeping its key order and raw values.
// It reports false for invalid JSON and for values that are not objects.
func parseForkLine(line string) (forkEntry, bool) {
	if !json.Valid([]byte(line)) {
		return forkEntry{}, false
	}
	dec := json.NewDecoder(strings.NewReader(line))
	dec.UseNumber()
	var fields map[string]any
	if dec.Decode(&fields) != nil || fields == nil {
		return forkEntry{}, false
	}
	e := forkEntry{fields: fields, raw: make(map[string]json.RawMessage, len(fields))}
	dec = json.NewDecoder(strings.NewReader(line))
	if _, err := dec.Token(); err != nil { // '{'
		return forkEntry{}, false
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return forkEntry{}, false
		}
		k, _ := tok.(string)
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return forkEntry{}, false
		}
		if _, dup := e.raw[k]; !dup {
			e.keys = append(e.keys, k)
		}
		e.raw[k] = v // duplicate keys: last value wins, first position kept
	}
	return e, true
}

// parseForkTranscript parses a JSONL transcript into the transcript entries
// (chain-linked message types with a uuid) and the replacements of the
// session's content-replacement records. Corrupt lines are skipped.
func parseForkTranscript(content, sessionID string) ([]forkEntry, []any) {
	var transcript []forkEntry
	var replacements []any
	for line := range strings.Lines(content) {
		line = pyStrip(line)
		if line == "" {
			continue
		}
		e, ok := parseForkLine(line)
		if !ok {
			continue
		}
		if isTranscriptEntry(e.fields) {
			transcript = append(transcript, e)
		} else if isOwnContentReplacement(e.fields, sessionID) {
			var list []json.RawMessage
			if json.Unmarshal(e.raw["replacements"], &list) == nil {
				for _, r := range list {
					replacements = append(replacements, r)
				}
			}
		}
	}
	return transcript, replacements
}

// isOwnContentReplacement reports whether entry is a content-replacement
// record of sessionID with a list of replacements.
func isOwnContentReplacement(entry map[string]any, sessionID string) bool {
	if entry["type"] != "content-replacement" || entry["sessionId"] != sessionID {
		return false
	}
	v := reflect.ValueOf(entry["replacements"])
	return v.Kind() == reflect.Slice && v.Type().Elem().Kind() != reflect.Uint8
}

// sliceElements returns the elements of a slice value as []any.
func sliceElements(v any) []any {
	rv := reflect.ValueOf(v)
	out := make([]any, rv.Len())
	for i := range out {
		out[i] = rv.Index(i).Interface()
	}
	return out
}

// buildForkLines is the core fork transform: it remaps uuids and returns the
// new session id and its JSONL lines (without trailing newlines).
// deriveTitle is called only when title is blank.
func buildForkLines(transcript []forkEntry, replacements []any, sessionID, upToMessageID, title string,
	deriveTitle func() string,
) (string, []string, error) {
	// Sidechains (subagent sessions) have their own parentUuid graphs. Meta
	// entries are kept: they are interleaved in the main chain.
	var mainChain []forkEntry
	for _, e := range transcript {
		if !truthy(e.fields["isSidechain"]) {
			mainChain = append(mainChain, e)
		}
	}
	if len(mainChain) == 0 {
		return "", nil, noMessagesToForkError(sessionID)
	}

	if upToMessageID != "" {
		cutoff := slices.IndexFunc(mainChain, func(e forkEntry) bool { return e.fields["uuid"] == upToMessageID })
		if cutoff < 0 {
			return "", nil, fmt.Errorf("claude: message %s not found in session %s", upToMessageID, sessionID)
		}
		mainChain = mainChain[:cutoff+1]
	}

	// Progress entries are mapped too: the parentUuid walk passes through
	// them. As in Python, a duplicated uuid maps to its last occurrence.
	mapping := make(map[string]string, len(mainChain))
	byUUID := make(map[string]forkEntry, len(mainChain))
	for _, e := range mainChain {
		uid := e.fields["uuid"].(string)
		mapping[uid] = randomUUID()
		byUUID[uid] = e
	}

	// Progress entries are UI-only chain links, not needed in a fork.
	var writable []forkEntry
	for _, e := range mainChain {
		if e.fields["type"] != "progress" {
			writable = append(writable, e)
		}
	}
	if len(writable) == 0 {
		return "", nil, noMessagesToForkError(sessionID)
	}

	forkedID := randomUUID()
	now := pyISONowUTC()
	lines := make([]string, 0, len(writable)+2)

	for i, orig := range writable {
		origUUID := orig.fields["uuid"].(string)

		// Only the last message gets a fresh timestamp (leaf detection on
		// resume).
		var timestamp any = now
		if _, ok := orig.fields["timestamp"]; ok && i != len(writable)-1 {
			timestamp = orig.value("timestamp")
		}

		line, err := orig.marshal([]jsonField{
			{"uuid", mapping[origUUID]},
			{"parentUuid", forkParent(orig, byUUID, mapping)},
			{"logicalParentUuid", forkLogicalParent(orig, mapping)},
			{"sessionId", forkedID},
			{"timestamp", timestamp},
			{"isSidechain", false},
			{"forkedFrom", forkedFrom{SessionID: sessionID, MessageUUID: origUUID}},
		}, forkDroppedKeys)
		if err != nil {
			return "", nil, fmt.Errorf("claude: fork session %s: %w", sessionID, err)
		}
		lines = append(lines, line)
	}

	trailer, err := forkTrailerLines(sessionID, forkedID, now, replacements, title, deriveTitle)
	if err != nil {
		return "", nil, err
	}
	return forkedID, append(lines, trailer...), nil
}

// forkParent returns the forked parentUuid of orig: the new uuid of its
// nearest ancestor that is not a progress entry, or nil when there is none
// in the fork. The seen set guards against progress cycles, which Python
// would loop on.
func forkParent(orig forkEntry, byUUID map[string]forkEntry, mapping map[string]string) any {
	seen := map[string]bool{}
	for parentID := str(orig.fields["parentUuid"]); parentID != "" && !seen[parentID]; {
		seen[parentID] = true
		parent, ok := byUUID[parentID]
		if !ok {
			break
		}
		if parent.fields["type"] != "progress" {
			return mapping[parentID]
		}
		parentID = str(parent.fields["parentUuid"])
	}
	return nil
}

// forkLogicalParent returns the forked logicalParentUuid of orig, the
// compact-boundary back-pointer: a target outside the fork becomes null, a
// falsy value is kept.
func forkLogicalParent(orig forkEntry, mapping map[string]string) any {
	lp, present := orig.fields["logicalParentUuid"]
	if !truthy(lp) {
		if present {
			return orig.value("logicalParentUuid")
		}
		return nil
	}
	if s, ok := lp.(string); ok {
		if m, ok := mapping[s]; ok {
			return m
		}
	}
	return nil
}

// forkTrailerLines returns the lines that end a fork of sessionID: a
// content-replacement record carrying the source's replacements, when there
// are any, and the custom-title entry. The title is title, else the source's
// custom title, AI title or first prompt (deriveTitle) plus " (fork)".
// Readers take the last custom-title entry, so this one is what surfaces.
func forkTrailerLines(sessionID, forkedID, now string, replacements []any, title string, deriveTitle func() string) ([]string, error) {
	var lines []string
	if len(replacements) > 0 {
		line, err := pyJSONObject([]jsonField{
			{"type", "content-replacement"},
			{"sessionId", forkedID},
			{"replacements", replacements},
			{"uuid", randomUUID()},
			{"timestamp", now},
		})
		if err != nil {
			return nil, fmt.Errorf("claude: fork session %s: %w", sessionID, err)
		}
		lines = append(lines, line)
	}

	forkTitle := pyStrip(title)
	if forkTitle == "" {
		forkTitle = cmp.Or(deriveTitle(), "Forked session") + " (fork)"
	}
	line, err := pyJSONObject([]jsonField{
		{"type", "custom-title"},
		{"sessionId", forkedID},
		{"customTitle", forkTitle},
		{"uuid", randomUUID()},
		{"timestamp", now},
	})
	if err != nil {
		return nil, err
	}
	return append(lines, line), nil
}

// forkedFrom is the "forkedFrom" value of forked entries.
type forkedFrom struct {
	SessionID   string `json:"sessionId"`
	MessageUUID string `json:"messageUuid"`
}

// deriveTitleFromEntries mirrors the filesystem title scan over store
// entries: the last non-empty customTitle, else the last non-empty aiTitle,
// else the first prompt.
func deriveTitleFromEntries(entries []SessionStoreEntry) string {
	var custom, ai string
	for _, e := range entries {
		if s, ok := e["customTitle"].(string); ok && s != "" {
			custom = s
		}
		if s, ok := e["aiTitle"].(string); ok && s != "" {
			ai = s
		}
	}
	if custom != "" {
		return custom
	}
	if ai != "" {
		return ai
	}
	// Reuse the head extractor over re-serialized JSONL so the skip rules
	// and truncation match the filesystem path exactly.
	return extractFirstPromptFromHead(entriesToJSONL(entries))
}

// ---------------------------------------------------------------------------
// SessionStore-backed mutations
// ---------------------------------------------------------------------------

// RenameSessionViaStore renames a session in a SessionStore by appending a
// custom-title entry, the store-backed counterpart of RenameSession.
// directory selects the project key (see ProjectKeyForDirectory) and
// defaults to the current directory.
//
// The title rules are as for RenameSession. The entry carries a fresh uuid
// and timestamp for the store's idempotency contract. The session's
// existence is not checked. It returns an error wrapping
// ErrInvalidSessionID when sessionID is not a UUID; store errors are
// returned.
func RenameSessionViaStore(ctx context.Context, store SessionStore, sessionID, title, directory string) error {
	return renameSessionTo(storeMetadataSink{ctx, store, directory}, sessionID, title)
}

// TagSessionViaStore tags a session in a SessionStore by appending a tag
// entry, the store-backed counterpart of TagSession; an empty tag clears
// the tag. The tag rules are as for TagSession and directory, the entry and
// the errors as for RenameSessionViaStore.
func TagSessionViaStore(ctx context.Context, store SessionStore, sessionID, tag, directory string) error {
	return tagSessionTo(storeMetadataSink{ctx, store, directory}, sessionID, tag)
}

// DeleteSessionViaStore deletes a session from a SessionStore, the
// store-backed counterpart of DeleteSession. directory selects the project
// key and defaults to the current directory.
//
// It calls SessionDeleter.Delete for the session's main key; whether
// subagent transcripts go too depends on the store (the SessionDeleter
// contract requires the cascade, and InMemorySessionStore implements it).
// When the store does not implement SessionDeleter, deletion is a no-op, as
// suits write-once backends. It returns an error wrapping
// ErrInvalidSessionID when sessionID is not a UUID; store errors are
// returned.
func DeleteSessionViaStore(ctx context.Context, store SessionStore, sessionID, directory string) error {
	if !validateUUID(sessionID) {
		return invalidSessionIDError(sessionID)
	}
	deleter, ok := store.(SessionDeleter)
	if !ok {
		return nil
	}
	return deleter.Delete(ctx, SessionKey{ProjectKey: ProjectKeyForDirectory(directory), SessionID: sessionID})
}

// ForkSessionViaStore forks a session within a SessionStore, the
// store-backed counterpart of ForkSession. The source is loaded from the
// project key of opts.Directory (default: the current directory) and the
// fork is appended under the same project key in a single Append call.
//
// A storage-level copy would not do: every uuid is remapped and each entry
// is rewritten, so the transcript passes through this process once. The
// transform, options and title derivation are as for ForkSession; the
// appended entries are fresh JSON-decoded objects sharing nothing with the
// loaded ones.
//
// It returns an error wrapping ErrInvalidSessionID when sessionID is not a
// UUID and one wrapping ErrSessionNotFound when the store has no entries
// for it; store errors are returned.
func ForkSessionViaStore(ctx context.Context, store SessionStore, sessionID string, opts *ForkSessionOptions) (*ForkSessionResult, error) {
	if opts == nil {
		opts = &ForkSessionOptions{}
	}
	if !validateUUID(sessionID) {
		return nil, invalidSessionIDError(sessionID)
	}
	if err := validateUpToMessageID(opts.UpToMessageID); err != nil {
		return nil, err
	}
	projectKey := ProjectKeyForDirectory(opts.Directory)
	loaded, err := store.Load(ctx, SessionKey{ProjectKey: projectKey, SessionID: sessionID})
	if err != nil {
		return nil, err
	}
	if len(loaded) == 0 {
		return nil, sessionNotFoundError(sessionID, "")
	}

	var transcript []forkEntry
	var replacements []any
	for _, e := range loaded {
		if e == nil {
			continue
		}
		if isTranscriptEntry(e) {
			transcript = append(transcript, forkEntry{fields: e})
		} else if isOwnContentReplacement(e, sessionID) {
			replacements = append(replacements, sliceElements(e["replacements"])...)
		}
	}

	forkedID, lines, err := buildForkLines(transcript, replacements, sessionID, opts.UpToMessageID, opts.Title,
		func() string { return deriveTitleFromEntries(loaded) })
	if err != nil {
		return nil, err
	}
	// Re-parse the serialized lines so the store receives the same shape as
	// from the transcript mirror.
	entries := make([]SessionStoreEntry, len(lines))
	for i, line := range lines {
		if err := json.Unmarshal([]byte(line), &entries[i]); err != nil {
			return nil, fmt.Errorf("claude: fork session %s: %w", sessionID, err)
		}
	}
	if err := store.Append(ctx, SessionKey{ProjectKey: projectKey, SessionID: forkedID}, entries); err != nil {
		return nil, err
	}
	return &ForkSessionResult{SessionID: forkedID}, nil
}
