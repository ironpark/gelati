package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"maps"
	"math"
	"math/big"
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
	"unicode"
	"unicode/utf8"
)

// Session listing and reading for the CLI's local transcript store.
//
// The CLI writes one JSONL transcript per session to
// <config>/projects/<sanitized-cwd>/<session-id>.jsonl. Listing extracts
// metadata from a stat plus a head/tail read of each file rather than a full
// JSONL parse; reading a session rebuilds its conversation chain from the
// parentUuid links (see sessions_chain.go). Ported from
// _internal/sessions.py and aligned with the TypeScript SDK (v0.3.286),
// whose behavior wins where the two differ; sessions_parity_test.go
// cross-checks the results against the TS runtime.
//
// Internal helpers take the projects directory as an explicit root argument
// (see projectsDir) so they can be exercised without touching the process
// environment; the exported functions resolve it from CLAUDE_CONFIG_DIR.

const (
	// liteReadBufSize is the size of the head and tail buffers read for
	// lite metadata extraction.
	liteReadBufSize = 65536

	// storeListLoadConcurrency bounds the concurrent SessionStore.Load calls
	// issued by ListSessionsFromStore, so large listings do not exhaust
	// adapter connection pools or trip backend rate limits.
	storeListLoadConcurrency = 16

	// maxSanitizedLength is the longest sanitized project directory name
	// kept verbatim. Most filesystems limit a path component to 255 bytes;
	// 200 leaves room for the hash suffix and separator.
	maxSanitizedLength = 200

	// worktreeListTimeout bounds `git worktree list`.
	worktreeListTimeout = 5 * time.Second

	// precompactSkipThreshold is the transcript size above which
	// GetSessionMessages reads only what follows the last compact boundary.
	precompactSkipThreshold = 5 << 20

	// precompactBoundaryWindow is how far into a line the
	// "compact_boundary" marker must start for the line to be parsed as a
	// boundary candidate during the pre-compact skip.
	precompactBoundaryWindow = 256

	// continuedInScanLimit bounds the scan of a continuation transcript
	// for a chain entry.
	continuedInScanLimit = 16 << 20
)

// programmaticEntrypoints are the CLAUDE_CODE_ENTRYPOINT values of SDK
// sessions, hidden by ListSessionsOptions.ExcludeProgrammatic. The TS SDK
// lists sdk-cli, sdk-ts and sdk-py; the Python client and this SDK add
// sdk-py-client, sdk-go and sdk-go-client.
var programmaticEntrypoints = map[string]bool{
	"sdk-cli": true, "sdk-ts": true, "sdk-py": true,
	"sdk-py-client": true, entrypoint: true, entrypointClient: true,
}

// jsSpaceClass matches the characters JavaScript's \s (and
// String.prototype.trim) treats as whitespace. Go's \s is ASCII-only.
const jsSpaceClass = `[\t\n\v\f\r \x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]`

var (
	uuidRE = regexp.MustCompile(
		`^(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

	// skipFirstPromptRE matches auto-generated or system messages that are
	// skipped when looking for the first meaningful user prompt: anything
	// starting with a lowercase XML-style tag, and interrupt markers.
	skipFirstPromptRE = regexp.MustCompile(
		`^(?:` + jsSpaceClass + `*<[a-z][A-Za-z0-9_-]*(?:` + jsSpaceClass + `|>)|` +
			`\[Request interrupted by user[^\]]*\])`)

	// commandNameRE extracts a slash command's name; like a JS ".", the
	// name does not span line terminators.
	commandNameRE = regexp.MustCompile(`<command-name>([^\n\r\x{2028}\x{2029}]*?)</command-name>`)

	bashInputRE = regexp.MustCompile(`<bash-input>([\s\S]*?)</bash-input>`)

	// titleControlRE and titleC1RE sanitize custom-title.json titles.
	titleControlRE = regexp.MustCompile(`[\p{Cc}\p{Cf}\x{2028}\x{2029}]+`)
	titleC1RE      = regexp.MustCompile(`[\x00-\x1f\x7f-\x9f]`)

	sanitizeRE = regexp.MustCompile(`[^a-zA-Z0-9]`)
)

// ---------------------------------------------------------------------------
// Small Python-semantics helpers
// ---------------------------------------------------------------------------

// pyIsSpace reports whether Python's str.isspace would accept r.
func pyIsSpace(r rune) bool {
	return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f)
}

// pyStrip is Python's str.strip() with no arguments.
func pyStrip(s string) string { return strings.TrimFunc(s, pyIsSpace) }

// jsIsSpace reports whether JavaScript's String.prototype.trim strips r.
func jsIsSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

// jsTrim is JavaScript's String.prototype.trim.
func jsTrim(s string) string { return strings.TrimFunc(s, jsIsSpace) }

// utf16Len returns the length of s in UTF-16 code units (a JS string's
// length).
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n++
		if r > 0xFFFF {
			n++
		}
	}
	return n
}

// truncateUTF16 returns the longest prefix of s spanning at most n UTF-16
// code units without splitting a surrogate pair.
func truncateUTF16(s string, n int) string {
	units := 0
	for i, r := range s {
		w := 1
		if r > 0xFFFF {
			w = 2
		}
		if units+w > n {
			return s[:i]
		}
		units += w
	}
	return s
}

// truthy reports Python truthiness for a decoded JSON value.
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x != 0
	case int:
		return x != 0
	case int64:
		return x != 0
	case json.Number:
		f, err := x.Float64()
		return err != nil || f != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

// pySliceBounds converts Python slice bounds seq[start:stop] over a
// sequence of length n into valid Go bounds.
func pySliceBounds(n, start, stop int) (int, int) {
	clamp := func(i int) int {
		if i < 0 {
			i += n
			if i < 0 {
				return 0
			}
		}
		return min(i, n)
	}
	start, stop = clamp(start), clamp(stop)
	if stop < start {
		stop = start
	}
	return start, stop
}

// pageSlice applies the limit/offset paging shared by the message readers:
// a positive limit selects seq[offset:offset+limit] (Python slice
// semantics), otherwise a positive offset selects seq[offset:].
func pageSlice[T any](seq []T, limit, offset int) []T {
	if limit > 0 {
		lo, hi := pySliceBounds(len(seq), offset, offset+limit)
		return seq[lo:hi]
	}
	if offset > 0 {
		lo, hi := pySliceBounds(len(seq), offset, len(seq))
		return seq[lo:hi]
	}
	return seq
}

// truncateRunes returns the first n runes of s.
func truncateRunes(s string, n int) string {
	i := 0
	for ; n > 0 && i < len(s); n-- {
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
	}
	return s[:i]
}

// decodeUTF8Replace decodes b like Python's bytes.decode("utf-8",
// errors="replace"): every maximal ill-formed subsequence becomes one
// U+FFFD (Unicode's "maximal subpart" practice).
func decodeUTF8Replace(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	var sb strings.Builder
	sb.Grow(len(b) + 8)
	for i := 0; i < len(b); {
		r, size := utf8.DecodeRune(b[i:])
		if r != utf8.RuneError || size > 1 {
			sb.Write(b[i : i+size])
			i += size
			continue
		}
		sb.WriteRune(utf8.RuneError)
		i += utf8MaximalSubpart(b[i:])
	}
	return sb.String()
}

// utf8MaximalSubpart returns the length (>= 1) of the ill-formed sequence
// at the start of b: a valid lead byte plus however many valid
// continuation bytes follow it before the sequence breaks.
func utf8MaximalSubpart(b []byte) int {
	lead := b[0]
	var need int
	lo, hi := byte(0x80), byte(0xBF)
	switch {
	case lead >= 0xC2 && lead <= 0xDF:
		need = 1
	case lead == 0xE0:
		need, lo = 2, 0xA0
	case lead >= 0xE1 && lead <= 0xEC, lead == 0xEE, lead == 0xEF:
		need = 2
	case lead == 0xED:
		need, hi = 2, 0x9F
	case lead == 0xF0:
		need, lo = 3, 0x90
	case lead >= 0xF1 && lead <= 0xF3:
		need = 3
	case lead == 0xF4:
		need, hi = 3, 0x8F
	default:
		return 1
	}
	n := 1
	for ; n <= need && n < len(b); n++ {
		c := b[n]
		if c < lo || c > hi {
			break
		}
		lo, hi = 0x80, 0xBF
	}
	return n
}

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

// sanitizePath makes name safe for use as a directory name: every
// character other than an ASCII letter or digit becomes '-'. Results longer
// than maxSanitizedLength are truncated and suffixed with "-<hash>".
func sanitizePath(name string) string {
	sanitized := sanitizeRE.ReplaceAllLiteralString(name, "-")
	if len(sanitized) <= maxSanitizedLength {
		return sanitized
	}
	return sanitized[:maxSanitizedLength] + "-" + simpleHash(name)
}

// ---------------------------------------------------------------------------
// Config directories
// ---------------------------------------------------------------------------

// claudeConfigHomeDir returns the Claude config directory: CLAUDE_CONFIG_DIR
// when set, else ~/.claude, NFC-normalized.
func claudeConfigHomeDir() string {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return filepath.Clean(normalizeNFC(dir))
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		if u, uerr := user.Current(); uerr == nil {
			home = u.HomeDir
		}
	}
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

// findProjectDirs returns the transcript directories for projectPath under
// root (see projectsDir), using the directory name override of root.
func findProjectDirs(root, projectPath string) []string {
	return findProjectDirsWith(root, projectPath, projectDirNameOverride(root))
}

// findProjectDirsWith returns the existing transcript directories for
// projectPath under root: the directory named by projectDirName and, when
// override differs from the sanitized name, the sanitized one too. For
// paths whose sanitized name exceeds maxSanitizedLength (and no override),
// it adds every directory sharing the truncated prefix whose sessions were
// recorded in projectPath, since the CLI (Bun.hash) and the SDKs
// (simpleHash) produce different hash suffixes.
func findProjectDirsWith(root, projectPath, override string) []string {
	var dirs []string
	primary := filepath.Join(root, projectDirName(projectPath, override))
	if isDirFollow(primary) {
		dirs = append(dirs, primary)
	}
	sanitized := sanitizePath(projectPath)
	if override != "" {
		if sanitized != override {
			if p := filepath.Join(root, sanitized); isDirFollow(p) {
				dirs = append(dirs, p)
			}
		}
		return dirs
	}
	if len(sanitized) <= maxSanitizedLength {
		return dirs
	}
	caseInsensitive := runtime.GOOS == "windows"
	fold := func(s string) string {
		if caseInsensitive {
			return strings.ToLower(s)
		}
		return s
	}
	prefix := fold(sanitized[:maxSanitizedLength] + "-")
	entries, err := os.ReadDir(root)
	if err != nil {
		return dirs
	}
	for _, e := range entries {
		if !strings.HasPrefix(fold(e.Name()), prefix) || !direntIsDir(root, e) {
			continue
		}
		p := filepath.Join(root, e.Name())
		if fold(p) != fold(primary) && dirHasSessionForPath(p, projectPath, caseInsensitive) {
			dirs = append(dirs, p)
		}
	}
	return dirs
}

// findProjectDir returns the first of findProjectDirs, or "".
func findProjectDir(root, projectPath string) string {
	if dirs := findProjectDirs(root, projectPath); len(dirs) > 0 {
		return dirs[0]
	}
	return ""
}

// recordedSessionCwd returns the working directory a session was recorded
// in: the last relocation in the tail, else the first parseable cwd in the
// head.
func recordedSessionCwd(lite *liteSessionFile) (string, bool) {
	if cwd, ok := lastTypedStringField(lite.tail, "relocated", "relocatedCwd"); ok {
		return cwd, true
	}
	return firstLineStringField(lite.head, "cwd")
}

// dirHasSessionForPath reports whether some transcript directly in dir was
// recorded in a working directory whose (untruncated) sanitized form is
// that of projectPath.
func dirHasSessionForPath(dir, projectPath string, caseInsensitive bool) bool {
	want := sanitizeRE.ReplaceAllLiteralString(projectPath, "-")
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
		got := sanitizeRE.ReplaceAllLiteralString(darwinNFC(cwd), "-")
		if got == want || (caseInsensitive && strings.EqualFold(got, want)) {
			return true
		}
	}
	return false
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
// JSON string field extraction without a full parse (works on truncated
// lines)
// ---------------------------------------------------------------------------

// unescapeJSONString decodes the escapes of a JSON string body extracted as
// raw text, returning raw unchanged when it is not a valid string body.
func unescapeJSONString(raw string) string {
	if !strings.Contains(raw, `\`) {
		return raw
	}
	var s string
	if err := json.Unmarshal([]byte(`"`+raw+`"`), &s); err != nil {
		return raw
	}
	return s
}

// scanJSONStringValue returns the end index of the string value starting
// at start (the index of its closing quote), or -1 if it is unterminated.
func scanJSONStringValue(text string, start int) int {
	for i := start; i < len(text); {
		switch text[i] {
		case '\\':
			i += 2
		case '"':
			return i
		default:
			i++
		}
	}
	return -1
}

// lookupJSONStringField returns the value of the first `"key":"value"`
// (or `"key": "value"`) occurrence in text without parsing it as JSON. The
// compact pattern is searched first. ok is false when there is none.
func lookupJSONStringField(text, key string) (value string, ok bool) {
	for _, pattern := range [...]string{`"` + key + `":"`, `"` + key + `": "`} {
		idx := strings.Index(text, pattern)
		if idx < 0 {
			continue
		}
		start := idx + len(pattern)
		if end := scanJSONStringValue(text, start); end >= 0 {
			return unescapeJSONString(text[start:end]), true
		}
	}
	return "", false
}

// extractJSONStringField is lookupJSONStringField without the found flag.
func extractJSONStringField(text, key string) string {
	v, _ := lookupJSONStringField(text, key)
	return v
}

// lookupLastJSONStringField returns the value of the last (by position)
// `"key":"value"` or `"key": "value"` occurrence in text. ok is false when
// there is none.
func lookupLastJSONStringField(text, key string) (value string, ok bool) {
	lastIdx := -1
	for _, pattern := range [...]string{`"` + key + `":"`, `"` + key + `": "`} {
		from := 0
		for {
			idx := strings.Index(text[from:], pattern)
			if idx < 0 {
				break
			}
			idx += from
			start := idx + len(pattern)
			end := scanJSONStringValue(text, start)
			if end < 0 {
				break // unterminated: the scan ran to the end of text
			}
			if idx > lastIdx {
				value, lastIdx, ok = unescapeJSONString(text[start:end]), idx, true
			}
			from = end + 1
		}
	}
	return value, ok
}

// extractLastJSONStringField is lookupLastJSONStringField without the found
// flag.
func extractLastJSONStringField(text, key string) string {
	v, _ := lookupLastJSONStringField(text, key)
	return v
}

// lastTypedStringField scans text's lines from the end for a JSON object
// of the given type whose field is a string, and returns that string.
func lastTypedStringField(text, typ, field string) (string, bool) {
	fieldNeedle := `"` + field + `":`
	typeNeedle := `"type":"` + typ + `"`
	for end := len(text); end > 0; {
		i := strings.LastIndexByte(text[:end], '\n')
		line := text[i+1 : end]
		end = i
		if strings.Contains(line, fieldNeedle) && strings.Contains(line, typeNeedle) {
			var obj map[string]any
			if json.Unmarshal([]byte(line), &obj) == nil && obj["type"] == typ {
				if v, ok := obj[field].(string); ok {
					return v, true
				}
			}
		}
		if i < 0 {
			break
		}
	}
	return "", false
}

// firstLineStringField returns field of the first line of text that
// parses as a JSON object with a string value for it.
func firstLineStringField(text, field string) (string, bool) {
	needle := `"` + field + `":`
	for line := range strings.SplitSeq(text, "\n") {
		if !strings.Contains(line, needle) {
			continue
		}
		var obj map[string]any
		if json.Unmarshal([]byte(line), &obj) == nil {
			if v, ok := obj[field].(string); ok {
				return v, true
			}
		}
	}
	return "", false
}

// ---------------------------------------------------------------------------
// First prompt extraction
// ---------------------------------------------------------------------------

// unwrapPastedContent replaces each well-formed
// <pasted_content id="xxxx">...</pasted_content id="xxxx"> block of text
// with its body, dropping up to two newlines around it.
func unwrapPastedContent(text string) string {
	const open = `<pasted_content id="`
	type segment struct {
		text  string
		block bool
	}
	var segs []segment
	textStart, from := 0, 0
	for {
		o := strings.Index(text[from:], open)
		if o < 0 {
			break
		}
		o += from
		s := o + len(open)
		id := text[s:min(s+4, len(text))]
		if !isLowerHex4(id) || !strings.HasPrefix(text[s+4:], "\">\n") {
			from = s
			continue
		}
		bodyStart := s + 4 + 3
		closing := `</pasted_content id="` + id + `">`
		d := strings.Index(text[bodyStart-1:], "\n"+closing)
		if d < 0 {
			break
		}
		d += bodyStart - 1 + 1 // index just past the newline before closing
		p := o
		for f := 0; f < 2 && p > textStart && text[p-1] == '\n'; f++ {
			p--
		}
		if p > textStart {
			segs = append(segs, segment{text: text[textStart:p]})
		}
		textStart = d + len(closing)
		for f := 0; f < 2 && textStart < len(text) && text[textStart] == '\n'; f++ {
			textStart++
		}
		body := ""
		if d-1 > bodyStart {
			body = text[bodyStart : d-1]
		}
		segs = append(segs, segment{text: body, block: true})
		from = textStart
	}
	if textStart < len(text) {
		segs = append(segs, segment{text: text[textStart:]})
	}
	if len(segs) == 1 && !segs[0].block {
		return text
	}
	var sb strings.Builder
	for _, seg := range segs {
		sb.WriteString(seg.text)
	}
	return sb.String()
}

func isLowerHex4(s string) bool {
	if len(s) != 4 {
		return false
	}
	for i := 0; i < 4; i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// promptFromUserEntry inspects one parsed transcript entry for a first
// prompt. It reports the display prompt (whitespace-collapsed, truncated to
// 200 UTF-16 units) when the entry is a meaningful user prompt; bash-mode
// input is shown as "! <command>". For a slash command, the command name is
// stored in *fallback when that is still empty. Tool results, meta and
// compact-summary messages and auto-generated text (anything starting with
// a lowercase XML-style tag, interrupt markers) yield nothing.
func promptFromUserEntry(entry map[string]any, fallback *string) (string, bool) {
	if entry["type"] != "user" || entry["isMeta"] == true || entry["isCompactSummary"] == true {
		return "", false
	}
	if !jsTruthy(entry["message"]) {
		return "", false
	}
	message, _ := entry["message"].(map[string]any)
	var texts []string
	switch content := message["content"].(type) {
	case string:
		texts = append(texts, content)
	case []any:
		for _, b := range content {
			block, ok := b.(map[string]any)
			if !ok {
				continue
			}
			if block["type"] == "tool_result" {
				return "", false
			}
			if text, ok := block["text"].(string); ok && block["type"] == "text" {
				texts = append(texts, text)
			}
		}
	}
	for _, raw := range texts {
		text := jsTrim(strings.ReplaceAll(unwrapPastedContent(raw), "\n", " "))
		if text == "" {
			continue
		}
		if m := commandNameRE.FindStringSubmatch(text); m != nil {
			if *fallback == "" {
				*fallback = m[1]
			}
			continue
		}
		if m := bashInputRE.FindStringSubmatch(text); m != nil {
			return "! " + jsTrim(m[1]), true
		}
		if skipFirstPromptRE.MatchString(text) {
			continue
		}
		if utf16Len(text) > 200 {
			text = jsTrim(truncateUTF16(text, 200)) + "\u2026"
		}
		return text, true
	}
	return "", false
}

// isCandidateUserLine is the cheap line filter applied before parsing a
// head line as a possible prompt.
func isCandidateUserLine(line string) bool {
	if !strings.Contains(line, `"type":"user"`) && !strings.Contains(line, `"type": "user"`) {
		return false
	}
	return !strings.Contains(line, `"tool_result"`) &&
		!strings.Contains(line, `"isMeta":true`) && !strings.Contains(line, `"isMeta": true`)
}

// extractFirstPromptFromHead returns the first meaningful user prompt in a
// JSONL head chunk (see promptFromUserEntry); when only slash commands are
// found, the first command name is returned. It returns "" when nothing
// qualifies.
func extractFirstPromptFromHead(head string) string {
	commandFallback := ""
	for line := range strings.SplitSeq(head, "\n") {
		if !isCandidateUserLine(line) ||
			strings.Contains(line, `"isCompactSummary":true`) || strings.Contains(line, `"isCompactSummary": true`) {
			continue
		}
		var entry map[string]any
		if json.Unmarshal([]byte(line), &entry) != nil || entry == nil {
			continue
		}
		if prompt, ok := promptFromUserEntry(entry, &commandFallback); ok {
			return prompt
		}
	}
	return commandFallback
}

// extractMediaPromptFromHead returns "Image" or "Document" for the first
// user line of a head chunk that carries such a block, the summary of last
// resort for sessions whose prompts are attachments only.
func extractMediaPromptFromHead(head string) string {
	for line := range strings.SplitSeq(head, "\n") {
		if !isCandidateUserLine(line) {
			continue
		}
		if strings.Contains(line, `"type":"image"`) || strings.Contains(line, `"type": "image"`) {
			return "Image"
		}
		if strings.Contains(line, `"type":"document"`) || strings.Contains(line, `"type": "document"`) {
			return "Document"
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Head/tail reads
// ---------------------------------------------------------------------------

// liteSessionFile is the result of reading a session file's head, tail,
// mtime and size.
type liteSessionFile struct {
	mtime int64 // Unix epoch milliseconds
	size  int64
	head  string
	tail  string
}

// jsMTimeMillis converts a modification time to Unix epoch milliseconds the
// way Node's Stats.mtime.getTime() does: mtimeMs computed in float64, then
// Math.round.
func jsMTimeMillis(t time.Time) int64 {
	return int64(math.Floor(float64(t.Unix())*1e3 + float64(t.Nanosecond())/1e6 + 0.5))
}

// readSessionLite stats path and reads its first and last liteReadBufSize
// bytes. It returns nil on any error or when the file is empty.
func readSessionLite(path string) *liteSessionFile {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil
	}
	size := fi.Size()
	buf := make([]byte, liteReadBufSize)
	n, err := io.ReadFull(f, buf)
	if n == 0 || (err != nil && !errors.Is(err, io.ErrUnexpectedEOF)) {
		return nil
	}
	head := decodeUTF8Replace(buf[:n])
	tail := head
	if off := size - liteReadBufSize; off > 0 {
		n, err := f.ReadAt(buf, off)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil
		}
		tail = decodeUTF8Replace(buf[:n])
	}
	return &liteSessionFile{mtime: jsMTimeMillis(fi.ModTime()), size: size, head: head, tail: tail}
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
// Metadata extraction shared by ListSessions and GetSessionInfo
// ---------------------------------------------------------------------------

// firstNonEmpty returns the first non-empty string, mirroring a Python
// `a or b or c` chain over optional strings.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// parseSessionInfoFromLite derives SessionInfo from a lite read; see
// parseSessionInfoFromLiteTitle.
func parseSessionInfoFromLite(sessionID string, lite *liteSessionFile, projectPath string) *SessionInfo {
	return parseSessionInfoFromLiteTitle(sessionID, lite, projectPath, "")
}

// parseSessionInfoFromLiteTitle derives SessionInfo from a lite read. It
// returns nil for sidechain sessions and for metadata-only sessions with no
// extractable summary. projectPath is the Cwd fallback and may be empty;
// sidecarTitle is the custom-title.json title, ranked below a tail
// customTitle and above a head-only one.
func parseSessionInfoFromLiteTitle(sessionID string, lite *liteSessionFile, projectPath, sidecarTitle string) *SessionInfo {
	head, tail := lite.head, lite.tail

	firstLine, _, _ := strings.Cut(head, "\n")
	if strings.Contains(firstLine, `"isSidechain":true`) || strings.Contains(firstLine, `"isSidechain": true`) {
		return nil
	}

	// A user-set title (customTitle) wins over a generated one (aiTitle);
	// the head fallback covers short sessions whose title entry is not in
	// the tail.
	customTitle := firstNonEmpty(
		extractLastJSONStringField(tail, "customTitle"),
		sidecarTitle,
		extractLastJSONStringField(head, "customTitle"),
		extractLastJSONStringField(tail, "aiTitle"),
		extractLastJSONStringField(head, "aiTitle"),
	)
	firstPrompt := extractFirstPromptFromHead(head)
	// lastPrompt shows what the user was most recently doing.
	summary := firstNonEmpty(
		customTitle,
		extractLastJSONStringField(tail, "lastPrompt"),
		extractLastJSONStringField(tail, "summary"),
		firstPrompt,
		extractMediaPromptFromHead(head),
	)
	if summary == "" {
		return nil
	}

	relocated, _ := lastTypedStringField(tail, "relocated", "relocatedCwd")
	info := &SessionInfo{
		SessionID:    sessionID,
		Summary:      summary,
		LastModified: lite.mtime,
		FileSize:     lite.size,
		CustomTitle:  customTitle,
		FirstPrompt:  firstPrompt,
		GitBranch: firstNonEmpty(
			extractLastJSONStringField(tail, "gitBranch"),
			extractJSONStringField(head, "gitBranch"),
		),
		Cwd: firstNonEmpty(relocated, extractJSONStringField(head, "cwd"), projectPath),
	}

	// Tags are read only from tag lines: a bare scan for "tag" would match
	// tool_use inputs (git tags, Docker tags, ...).
	lines := strings.Split(tail, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.Contains(lines[i], `"type":"tag"`) && strings.Contains(lines[i], `"tag":"`) {
			info.Tag = extractLastJSONStringField(lines[i], "tag")
			break
		}
	}

	// CreatedAt comes from the first timestamp anywhere in the head: the
	// first record may be metadata-only (e.g. permission-mode) without one.
	if ts := extractJSONStringField(head, "timestamp"); ts != "" {
		if ms, ok := isoToEpochMillis(ts); ok {
			info.CreatedAt = ms
		}
	}
	return info
}

// isProgrammaticSession reports whether a session was started through an
// SDK (see programmaticEntrypoints) or is a daemon or daemon-worker
// session.
func isProgrammaticSession(head, tail string) bool {
	ep, ok := lookupJSONStringField(head, "entrypoint")
	if !ok {
		ep, _ = lookupLastJSONStringField(tail, "entrypoint")
	}
	if programmaticEntrypoints[ep] {
		return true
	}
	scope := head
	for line := range strings.SplitSeq(head, "\n") {
		if strings.Contains(line, `"parentUuid":`) {
			scope = line
			break
		}
	}
	kind, _ := lookupJSONStringField(scope, "sessionKind")
	return kind == "daemon" || kind == "daemon-worker"
}

// readCustomTitleSidecar returns the sanitized title of the
// <projectDir>/<sessionID>/custom-title.json sidecar beside a transcript,
// or "" when it is missing or unusable.
func readCustomTitleSidecar(transcriptPath, sessionID string) string {
	b, err := os.ReadFile(filepath.Join(filepath.Dir(transcriptPath), sessionID, "custom-title.json"))
	if err != nil {
		return ""
	}
	var obj map[string]any
	if json.Unmarshal(b, &obj) != nil {
		return ""
	}
	title, _ := obj["customTitle"].(string)
	return sanitizeSidecarTitle(title)
}

// sanitizeSidecarTitle collapses control and format characters to spaces,
// keeps at most 200 code points and trims.
func sanitizeSidecarTitle(title string) string {
	title = titleControlRE.ReplaceAllLiteralString(jsTrim(title), " ")
	title = titleC1RE.ReplaceAllLiteralString(title, "")
	return jsTrim(truncateRunes(title, 200))
}

// continuedInSessionID returns the session a transcript was continued in:
// the continuedInSessionId of a {"type":"continued-in"} line that no later
// completed assistant reply or user prompt follows. It returns "" when there
// is none.
func continuedInSessionID(tail string) string {
	const marker = `"type":"continued-in"`
	if !strings.Contains(tail, marker) {
		return ""
	}
	for end := len(tail); end > 0; {
		i := strings.LastIndexByte(tail[:end], '\n')
		line := tail[i+1 : end]
		end = i
		isMarker := strings.Contains(line, marker)
		isMessage := strings.Contains(line, `"type":"user"`) || strings.Contains(line, `"type":"assistant"`)
		if isMarker || isMessage {
			var obj any
			if json.Unmarshal([]byte(line), &obj) == nil {
				m, _ := obj.(map[string]any)
				if isMarker && m["type"] == "continued-in" {
					if id, ok := m["continuedInSessionId"].(string); ok {
						if validateUUID(id) {
							return id
						}
						return ""
					}
				}
				if isMessage && m != nil && continuesConversation(m) {
					return ""
				}
			}
		}
		if i < 0 {
			break
		}
	}
	return ""
}

// continuesConversation reports whether a transcript line is a completed
// assistant reply or a user prompt.
func continuesConversation(m map[string]any) bool {
	if m["type"] == "assistant" {
		shapeOK := true
		if v, ok := m["isApiErrorMessage"]; ok {
			if _, isBool := v.(bool); !isBool {
				shapeOK = false
			}
		}
		var stopReason any
		if v, ok := m["message"]; ok {
			msg, isObj := v.(map[string]any)
			if !isObj {
				shapeOK = false
			} else if sr, ok := msg["stop_reason"]; ok {
				if _, isStr := sr.(string); !isStr && sr != nil {
					shapeOK = false
				}
				stopReason = sr
			}
		}
		if shapeOK {
			_, isStr := stopReason.(string)
			return m["isApiErrorMessage"] != true && isStr
		}
	}
	var fallback string
	_, ok := promptFromUserEntry(m, &fallback)
	return ok
}

// hasChainEntries reports whether the transcript at path exists, is
// non-empty and holds a chain entry (a "parentUuid" key). Files are scanned
// up to continuedInScanLimit bytes; larger ones are assumed to.
func hasChainEntries(path string) bool {
	lite := readSessionLite(path)
	if lite == nil {
		return false
	}
	const needle = `"parentUuid":`
	if strings.Contains(lite.head, needle) || strings.Contains(lite.tail, needle) {
		return true
	}
	if lite.size <= liteReadBufSize {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, 1<<20+len(needle))
	carry, read := 0, 0
	for read < continuedInScanLimit {
		n, err := f.Read(buf[carry : carry+1<<20])
		if n == 0 {
			return false
		}
		if bytes.Contains(buf[:carry+n], []byte(needle)) {
			return true
		}
		read += n
		keep := min(len(needle), carry+n)
		copy(buf, buf[carry+n-keep:carry+n])
		carry = keep
		if err != nil {
			return false
		}
	}
	return true
}

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
	fold := func(s string) string {
		if caseInsensitive {
			return strings.ToLower(s)
		}
		return s
	}
	if fold(sanitizeRE.ReplaceAllLiteralString(a, "-")) != fold(sanitizeRE.ReplaceAllLiteralString(b, "-")) {
		return false
	}
	return fold(strings.ReplaceAll(a, `\`, "/")) != fold(strings.ReplaceAll(b, `\`, "/"))
}

// isForeignSession reports whether a session found in projectPath's
// transcript directory was recorded in a different real directory that
// merely sanitizes to the same name (e.g. /a-b and /a/b), so that it does
// not belong to projectPath. Sessions recorded inside one of ownWorktrees
// always belong.
func isForeignSession(cwd, projectPath string, ownWorktrees []string) bool {
	caseInsensitive := runtime.GOOS == "windows" || runtime.GOOS == "darwin"
	fold := func(s string) string {
		s = strings.ReplaceAll(darwinNFC(s), `\`, "/")
		if caseInsensitive {
			return strings.ToLower(s)
		}
		return s
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
// Listing
// ---------------------------------------------------------------------------

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

// sortSessionsByMTime stable-sorts sessions newest first.
func sortSessionsByMTime(sessions []SessionInfo) {
	slices.SortStableFunc(sessions, func(a, b SessionInfo) int { return cmpMTimeDesc(a.LastModified, b.LastModified) })
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

// applySortLimitOffset sorts newest first, then skips offset sessions
// (when positive) and keeps at most limit (when positive).
func applySortLimitOffset(sessions []SessionInfo, limit, offset int) []SessionInfo {
	sortSessionsByMTime(sessions)
	if offset > 0 {
		sessions = sessions[min(offset, len(sessions)):]
	}
	if limit > 0 && limit < len(sessions) {
		sessions = sessions[:limit]
	}
	if len(sessions) == 0 {
		return nil
	}
	return sessions
}

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
	sidecarTitle := ""
	if _, ok := lookupLastJSONStringField(lite.tail, "customTitle"); !ok {
		sidecarTitle = readCustomTitleSidecar(c.path, c.sessionID)
	}
	info := parseSessionInfoFromLiteTitle(c.sessionID, lite, c.projectPath, sidecarTitle)
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
func directoryCandidates(root, directory string, includeWorktrees bool) []sessionCandidate {
	canonical := canonicalizePath(directory)
	override := projectDirNameOverride(root)
	caseInsensitive := runtime.GOOS == "windows"
	fold := func(s string) string {
		if caseInsensitive {
			return strings.ToLower(s)
		}
		return s
	}

	var worktrees []string
	if includeWorktrees {
		worktrees = getWorktreePaths(canonical)
	}
	single := func() []sessionCandidate {
		var out []sessionCandidate
		seen := map[string]bool{}
		for _, dir := range findProjectDirsWith(root, canonical, override) {
			if !seen[fold(dir)] {
				seen[fold(dir)] = true
				out = append(out, sessionCandidatesIn(dir, canonical, nil)...)
			}
		}
		return out
	}
	if len(worktrees) <= 1 {
		return single()
	}

	// Longest name first so more specific worktrees win.
	type worktreeName struct{ path, exact, truncatedPrefix string }
	names := make([]worktreeName, 0, len(worktrees))
	for _, wt := range worktrees {
		name := projectDirName(wt, override)
		w := worktreeName{path: wt, exact: fold(name)}
		if len(name) > maxSanitizedLength {
			w.truncatedPrefix = w.exact[:maxSanitizedLength]
		}
		names = append(names, w)
	}
	slices.SortStableFunc(names, func(a, b worktreeName) int { return len(b.exact) - len(a.exact) })

	entries, err := os.ReadDir(root)
	if err != nil {
		return single()
	}
	own := append([]string{canonical}, worktrees...)
	var out []sessionCandidate
	seen := map[string]bool{}
	// The requested directory itself comes first: a subdirectory such as
	// /repo/packages/app does not match any worktree root.
	for _, dir := range findProjectDirsWith(root, canonical, override) {
		if name := fold(filepath.Base(dir)); !seen[name] {
			seen[name] = true
			out = append(out, sessionCandidatesIn(dir, canonical, own)...)
		}
	}
	for _, e := range entries {
		name := fold(e.Name())
		if seen[name] || !direntIsDir(root, e) {
			continue
		}
		dir := filepath.Join(root, e.Name())
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
	return out
}

// listSessionsIn implements ListSessions against an explicit projects root.
func listSessionsIn(root string, opts *ListSessionsOptions) []SessionInfo {
	if opts == nil {
		opts = &ListSessionsOptions{}
	}
	var candidates []sessionCandidate
	if opts.Directory != "" {
		candidates = directoryCandidates(root, opts.Directory, !opts.ExcludeWorktrees)
	} else {
		for _, dir := range subdirs(root) {
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
	if opts.Offset > 0 {
		infos = infos[min(opts.Offset, len(infos)):]
	}
	if opts.Limit > 0 && opts.Limit < len(infos) {
		infos = infos[:opts.Limit]
	}
	return nilIfEmpty(infos)
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
	return listSessionsIn(projectsDir(nil), opts), nil
}

// ---------------------------------------------------------------------------
// GetSessionInfo
// ---------------------------------------------------------------------------

// getSessionInfoIn implements GetSessionInfo against an explicit root.
func getSessionInfoIn(root, sessionID, directory string) *SessionInfo {
	if !validateUUID(sessionID) {
		return nil
	}
	found, ok := findSessionFile(root, sessionID, directory)
	if !ok {
		return nil
	}
	lite := readSessionLite(found.path)
	if lite == nil {
		return nil
	}
	sidecarTitle := ""
	if _, ok := lookupLastJSONStringField(lite.tail, "customTitle"); !ok {
		sidecarTitle = readCustomTitleSidecar(found.path, sessionID)
	}
	return parseSessionInfoFromLiteTitle(sessionID, lite, found.projectPath, sidecarTitle)
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
	return getSessionInfoIn(projectsDir(nil), sessionID, directory), nil
}

// ---------------------------------------------------------------------------
// GetSessionMessages: transcript reconstruction
// ---------------------------------------------------------------------------

// transcriptEntryTypes are the entry types that carry uuid + parentUuid
// chain links.
var transcriptEntryTypes = map[string]bool{
	"user": true, "assistant": true, "progress": true, "system": true, "attachment": true,
}

// isTranscriptEntry reports whether entry is a chain-linked transcript
// message: a transcript entry type with a string uuid.
func isTranscriptEntry(entry map[string]any) bool {
	t, _ := entry["type"].(string)
	_, ok := entry["uuid"].(string)
	return ok && transcriptEntryTypes[t]
}

// foundSession is a located transcript.
type foundSession struct {
	path        string
	projectPath string // the searched project or worktree path; empty when every project was searched
	size        int64
}

// findSessionFile finds the first non-empty transcript of sessionID: in the
// project directories of directory and then of its other git worktrees
// when directory is set, otherwise in every project directory.
func findSessionFile(root, sessionID, directory string) (foundSession, bool) {
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
		for _, dir := range findProjectDirs(root, canonical) {
			if f, ok := check(dir, canonical); ok {
				return f, true
			}
		}
		for _, wt := range getWorktreePaths(canonical) {
			if wt == canonical {
				continue
			}
			for _, dir := range findProjectDirs(root, wt) {
				if f, ok := check(dir, wt); ok {
					return f, true
				}
			}
		}
		return foundSession{}, false
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return foundSession{}, false
	}
	for _, e := range entries {
		if f, ok := check(filepath.Join(root, e.Name()), ""); ok {
			return f, true
		}
	}
	return foundSession{}, false
}

// readTranscript reads the first size bytes of a transcript for message
// reconstruction. When size exceeds precompactSkipThreshold and
// skipPrecompact is set, only the part from the last compact boundary on
// is returned (see skipPrecompactLines). It returns nil on error.
func readTranscript(path string, size int64, skipPrecompact bool) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, size))
	if err != nil {
		return nil
	}
	if size > precompactSkipThreshold && skipPrecompact {
		return skipPrecompactLines(b)
	}
	return b
}

// skipPrecompactLines drops everything before the last compact boundary
// that did not preserve messages (a system compact_boundary line whose
// "compact_boundary" marker starts within its first 256 bytes and whose
// compactMetadata has neither preservedSegment nor preservedMessages).
// Attribution snapshots are dropped too, except the last one after that
// boundary, which is moved to the end. A final line without a newline is
// never treated as a boundary.
func skipPrecompactLines(b []byte) []byte {
	const snapshotPrefix = `{"type":"attribution-snapshot"`
	marker := []byte(`"compact_boundary"`)
	var out, lastSnap []byte
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		terminated := i >= 0
		if !terminated {
			i = len(b) - 1
		}
		line := b[:i+1]
		b = b[i+1:]
		if bytes.HasPrefix(line, []byte(snapshotPrefix)) {
			lastSnap = line
			continue
		}
		if idx := bytes.Index(line, marker); terminated && idx >= 0 && idx < precompactBoundaryWindow {
			var entry map[string]any
			if json.Unmarshal(line, &entry) == nil && entry["type"] == "system" && entry["subtype"] == "compact_boundary" {
				meta, _ := entry["compactMetadata"].(map[string]any)
				if !jsTruthy(meta["preservedSegment"]) && !jsTruthy(meta["preservedMessages"]) {
					out, lastSnap = out[:0], nil
				}
			}
		}
		out = append(out, line...)
	}
	if lastSnap != nil {
		if len(out) > 0 && out[len(out)-1] != '\n' {
			out = append(out, '\n')
		}
		out = append(out, lastSnap...)
	}
	return out
}

// precompactSkipEnabled reports whether the large-transcript pre-compact
// skip applies: CLAUDE_CODE_DISABLE_PRECOMPACT_SKIP=1/true/yes/on turns it
// off.
func precompactSkipEnabled(getenv func(string) string) bool {
	return !envTruthy(getenv("CLAUDE_CODE_DISABLE_PRECOMPACT_SKIP"))
}

// envTruthy reports whether an environment value is 1, true, yes or on
// (case-insensitive, surrounding whitespace ignored).
func envTruthy(v string) bool {
	switch strings.ToLower(jsTrim(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// parseJSONLObjects parses JSONL content into its JSON object lines,
// skipping blank and corrupt lines. Invalid UTF-8 degrades to U+FFFD.
func parseJSONLObjects(content []byte) []map[string]any {
	var out []map[string]any
	for len(content) > 0 {
		i := bytes.IndexByte(content, '\n')
		if i < 0 {
			i = len(content)
		}
		line := bytes.TrimLeft(content[:i], "\x00\x01\x02\x03\x04\x05\x06\x07\x08\t\n\v\f\r\x0e\x0f\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f ")
		content = content[min(i+1, len(content)):]
		if len(line) == 0 {
			continue
		}
		var entry map[string]any
		if json.Unmarshal(line, &entry) == nil && entry != nil {
			out = append(out, entry)
		}
	}
	return out
}

// entriesToSessionMessages reconstructs the visible conversation from every
// parsed line of a main transcript and applies paging. It is shared by the
// filesystem and SessionStore paths.
func entriesToSessionMessages(parsed []map[string]any, opts *SessionMessagesOptions) []SessionMessage {
	messages := sessionMessagesFromParsed(parsed, opts.IncludeSystemMessages)
	return nilIfEmpty(pageSlice(messages, opts.Limit, opts.Offset))
}

func nilIfEmpty[T any](s []T) []T {
	if len(s) == 0 {
		return nil
	}
	return s
}

// getSessionMessagesIn implements GetSessionMessages against an explicit
// root.
func getSessionMessagesIn(root, sessionID string, opts *SessionMessagesOptions) []SessionMessage {
	return getSessionMessagesInEnv(root, sessionID, opts, precompactSkipEnabled(os.Getenv))
}

func getSessionMessagesInEnv(root, sessionID string, opts *SessionMessagesOptions, skipPrecompact bool) []SessionMessage {
	if opts == nil {
		opts = &SessionMessagesOptions{}
	}
	if !validateUUID(sessionID) {
		return nil
	}
	found, ok := findSessionFile(root, sessionID, opts.Directory)
	if !ok {
		return nil
	}
	content := readTranscript(found.path, found.size, skipPrecompact)
	if len(content) == 0 {
		return nil
	}
	return entriesToSessionMessages(parseJSONLObjects(content), opts)
}

// GetSessionMessages reads a session's conversation from its JSONL
// transcript. It rebuilds the conversation chain from the parentUuid links
// (following the relinking that compactions with preserved messages
// record) and returns its messages in chronological order: user and
// assistant messages, plus system messages when opts.IncludeSystemMessages
// is set. Sidechain, team and meta messages are left out, except meta
// messages from channel, observer and peer origins; compact summaries are
// kept. Prompts queued during a turn (queued_command attachments) are
// returned as user messages with IsQueuedCommand set once a turn reached
// them, and when they trail the conversation.
//
// For transcripts over 5 MiB, only the part after the last compact
// boundary is read, unless CLAUDE_CODE_DISABLE_PRECOMPACT_SKIP is set to
// 1, true, yes or on.
//
// opts.Directory is the project directory (its git worktrees are searched
// too); empty searches every project. opts.Offset skips messages from the
// start and a positive opts.Limit caps the count. opts may be nil.
//
// It returns nil when sessionID is not a UUID, the transcript is not found,
// or it has no visible messages. Corrupt lines are skipped. The error is
// currently always nil.
func GetSessionMessages(sessionID string, opts *SessionMessagesOptions) ([]SessionMessage, error) {
	return getSessionMessagesIn(projectsDir(nil), sessionID, opts), nil
}

// ---------------------------------------------------------------------------
// Subagent transcripts
// ---------------------------------------------------------------------------

// resolveSessionFilePath returns the path of the first non-empty transcript
// of sessionID (see findSessionFile), or "" when none exists.
func resolveSessionFilePath(root, sessionID, directory string) string {
	found, _ := findSessionFile(root, sessionID, directory)
	return found.path
}

// resolveSubagentsDir returns <projectDir>/<sessionID>/subagents for the
// session's transcript at <projectDir>/<sessionID>.jsonl, or "" when the
// session is not found. The directory itself may not exist.
func resolveSubagentsDir(root, sessionID, directory string) string {
	p := resolveSessionFilePath(root, sessionID, directory)
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
	var walk func(dir string)
	walk = func(dir string) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			name := e.Name()
			if strings.HasPrefix(name, "agent-") && strings.HasSuffix(name, ".jsonl") && direntIsFile(dir, e) {
				results = append(results, agentFile{
					agentID: name[len("agent-") : len(name)-len(".jsonl")],
					path:    filepath.Join(dir, name),
				})
			} else if direntIsDir(dir, e) {
				walk(filepath.Join(dir, name))
			}
		}
	}
	walk(baseDir)
	return results
}

// agentMetadataSidecarPath maps agent-<id>.jsonl to agent-<id>.meta.json in
// the same directory. It is the single definition of the sidecar naming
// convention, shared by reading, import and resume.
func agentMetadataSidecarPath(transcriptPath string) string {
	return strings.TrimSuffix(transcriptPath, ".jsonl") + ".meta.json"
}

// readAgentMetadataSidecar reads the .meta.json sidecar beside a subagent
// transcript. It returns (nil, nil) when the sidecar is missing, is not
// valid JSON or is not a JSON object; other read errors (permission denied,
// a directory in its place) are returned.
func readAgentMetadataSidecar(transcriptPath string) (map[string]any, error) {
	b, err := os.ReadFile(agentMetadataSidecarPath(transcriptPath))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	if !utf8.Valid(b) {
		return nil, nil // Python's UnicodeDecodeError is a ValueError
	}
	var meta map[string]any
	if json.Unmarshal(b, &meta) != nil {
		return nil, nil
	}
	return meta, nil
}

// splitAgentMetadata separates the synthetic agent_metadata entries a
// subagent's SessionStore stream carries in place of the .meta.json sidecar
// from its transcript lines. The last metadata entry wins (it is rewritten
// on resume) and is returned as a copy; metadata is nil when there is none.
func splitAgentMetadata(entries []SessionStoreEntry) (metadata map[string]any, transcript []SessionStoreEntry) {
	for _, e := range entries {
		if e != nil && e["type"] == "agent_metadata" {
			metadata = maps.Clone(e)
		} else {
			transcript = append(transcript, e)
		}
	}
	return metadata, transcript
}

// toolUseIDRE and agentIDRE validate the parent ids taken from agent
// metadata; malformed values count as absent.
var (
	toolUseIDRE = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)
	agentIDRE   = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
)

// parentIDsFromAgentMetadata extracts toolUseId and parentAgentId from a
// sidecar or agent_metadata entry; non-string or malformed values count as
// absent.
func parentIDsFromAgentMetadata(meta map[string]any) (toolUseID, parentAgentID string) {
	toolUseID, parentAgentID = str(meta["toolUseId"]), str(meta["parentAgentId"])
	if !toolUseIDRE.MatchString(toolUseID) {
		toolUseID = ""
	}
	if !agentIDRE.MatchString(parentAgentID) {
		parentAgentID = ""
	}
	return toolUseID, parentAgentID
}

// entriesToSubagentMessages builds the subagent chain from parsed lines and
// applies paging. Every message carries the same parent ids.
func entriesToSubagentMessages(parsed []map[string]any, limit, offset int, parentToolUseID, parentAgentID string) []SessionMessage {
	return nilIfEmpty(pageSlice(subagentMessagesFromParsed(parsed, parentToolUseID, parentAgentID), limit, offset))
}

// listSubagentsIn implements ListSubagents against an explicit root.
func listSubagentsIn(root, sessionID, directory string) []string {
	if !validateUUID(sessionID) {
		return nil
	}
	dir := resolveSubagentsDir(root, sessionID, directory)
	if dir == "" {
		return nil
	}
	var ids []string
	for _, f := range collectAgentFiles(dir) {
		ids = append(ids, f.agentID)
	}
	return ids
}

// ListSubagents lists the ids of a session's subagents by scanning
// <project>/<sessionID>/subagents/ (including nested directories such as
// workflows/<runId>/) for agent-<id>.jsonl transcripts.
//
// directory is the project directory (its git worktrees are searched too);
// empty searches every project. It returns nil when sessionID is not a
// UUID, the session is not found, or it has no subagents. The error is
// currently always nil.
func ListSubagents(sessionID, directory string) ([]string, error) {
	return listSubagentsIn(projectsDir(nil), sessionID, directory), nil
}

// getSubagentMessagesIn implements GetSubagentMessages against an explicit
// root.
func getSubagentMessagesIn(root, sessionID, agentID string, opts *SessionMessagesOptions) []SessionMessage {
	if opts == nil {
		opts = &SessionMessagesOptions{}
	}
	if !validateUUID(sessionID) || agentID == "" {
		return nil
	}
	dir := resolveSubagentsDir(root, sessionID, opts.Directory)
	if dir == "" {
		return nil
	}
	var match string
	for _, f := range collectAgentFiles(dir) {
		if f.agentID == agentID {
			match = f.path
			break
		}
	}
	if match == "" {
		return nil
	}
	content, err := os.ReadFile(match)
	if err != nil || len(content) == 0 {
		return nil
	}
	// The sidecar records which Agent tool_use spawned the subagent. Any
	// failure to read it degrades to "no metadata".
	meta, _ := readAgentMetadataSidecar(match)
	toolUseID, parentAgentID := parentIDsFromAgentMetadata(meta)
	return entriesToSubagentMessages(parseJSONLObjects(content), opts.Limit, opts.Offset, toolUseID, parentAgentID)
}

// GetSubagentMessages reads a subagent's conversation from its JSONL
// transcript and returns its user and assistant messages in chronological
// order. agentID is an id returned by ListSubagents.
//
// Every message's ParentToolUseID is the id of the Agent tool_use in the
// parent session that spawned the subagent, and ParentAgentID the spawning
// subagent for nested subagents; both come from the agent-<id>.meta.json
// sidecar beside the transcript and are empty when it is missing or
// unusable.
//
// opts.Directory is the project directory (its git worktrees are searched
// too); empty searches every project. opts.Offset and a positive
// opts.Limit page the result. opts may be nil. It returns nil when
// sessionID is not a UUID, agentID is empty, the session or subagent is not
// found, or the transcript has no messages. The error is currently always
// nil.
func GetSubagentMessages(sessionID, agentID string, opts *SessionMessagesOptions) ([]SessionMessage, error) {
	return getSubagentMessagesIn(projectsDir(nil), sessionID, agentID, opts), nil
}

// ---------------------------------------------------------------------------
// ISO-8601 timestamps
// ---------------------------------------------------------------------------

// isoToEpochMillis parses an ISO-8601 timestamp to Unix epoch milliseconds
// the way the Python SDK does (datetime.fromisoformat, then
// int(timestamp() * 1000)). It accepts extended and basic dates
// (YYYY-MM-DD, YYYYMMDD), any single separator character, times of the form
// HH[:MM[:SS[.f+]]] or HHMM[SS[.f+]] ("," also accepted as the decimal mark,
// digits beyond microseconds ignored), and offsets Z, ±HH, ±HHMM and
// ±HH:MM[:SS[.ffffff]]. Timestamps without an offset are local time, as in
// Python. Week dates are not supported.
func isoToEpochMillis(ts string) (int64, bool) {
	if strings.HasSuffix(ts, "Z") {
		ts = strings.ReplaceAll(ts, "Z", "+00:00")
	}
	var year, month, day int
	var rest string
	switch {
	case len(ts) >= 10 && ts[4] == '-' && ts[7] == '-':
		var ok1, ok2, ok3 bool
		year, ok1 = atoiDigits(ts[0:4])
		month, ok2 = atoiDigits(ts[5:7])
		day, ok3 = atoiDigits(ts[8:10])
		if !ok1 || !ok2 || !ok3 {
			return 0, false
		}
		rest = ts[10:]
	case len(ts) >= 8:
		var ok1, ok2, ok3 bool
		year, ok1 = atoiDigits(ts[0:4])
		month, ok2 = atoiDigits(ts[4:6])
		day, ok3 = atoiDigits(ts[6:8])
		if !ok1 || !ok2 || !ok3 {
			return 0, false
		}
		rest = ts[8:]
	default:
		return 0, false
	}
	if year < 1 || month < 1 || month > 12 || day < 1 || day > daysIn(year, month) {
		return 0, false
	}

	var hour, minute, sec, micro, offset int
	aware := false
	if rest != "" {
		_, size := utf8.DecodeRuneInString(rest) // any separator character
		rest = rest[size:]
		timePart, tzPart, sign := rest, "", 0
		if i := strings.IndexByte(rest, '-'); i >= 0 {
			timePart, tzPart, sign = rest[:i], rest[i+1:], -1
		} else if i := strings.IndexByte(rest, '+'); i >= 0 {
			timePart, tzPart, sign = rest[:i], rest[i+1:], 1
		}
		var ok bool
		if hour, minute, sec, micro, ok = parseISOTime(timePart); !ok {
			return 0, false
		}
		if sign != 0 {
			if n := len(tzPart); n == 0 || n == 1 || n == 3 {
				return 0, false
			}
			oh, om, os_, ous, ok := parseISOTime(tzPart)
			if !ok || oh > 23 {
				return 0, false
			}
			offset = sign * ((oh*3600+om*60+os_)*1_000_000 + ous)
			aware = true
		}
	}
	extraDay := 0
	if hour == 24 {
		if minute != 0 || sec != 0 || micro != 0 {
			return 0, false
		}
		hour, extraDay = 0, 1
	}

	if aware {
		t := time.Date(year, time.Month(month), day+extraDay, hour, minute, sec, 0, time.UTC)
		totalMicros := t.Unix()*1_000_000 + int64(micro) - int64(offset)
		// Python: timedelta.total_seconds() is an exactly rounded
		// integer division by 10**6.
		secs, _ := new(big.Rat).SetFrac64(totalMicros, 1_000_000).Float64()
		return int64(secs * 1000), true
	}
	t := time.Date(year, time.Month(month), day+extraDay, hour, minute, sec, 0, time.Local)
	secs := float64(t.Unix()) + float64(float64(micro)/1e6)
	return int64(secs * 1000), true
}

// parseISOTime parses HH[:MM[:SS[.f+]]] or HHMM[SS[.f+]]; a fraction is
// only accepted after seconds. Hour 24 is returned as-is for the caller to
// validate.
func parseISOTime(s string) (hour, minute, sec, micro int, ok bool) {
	if len(s) < 2 {
		return 0, 0, 0, 0, false
	}
	if hour, ok = atoiDigits(s[:2]); !ok || hour > 24 {
		return 0, 0, 0, 0, false
	}
	s = s[2:]
	extended := strings.HasPrefix(s, ":")
	parsed := 0
	for _, field := range []*int{&minute, &sec} {
		if s == "" || s[0] == '.' || s[0] == ',' {
			break
		}
		if extended {
			if s[0] != ':' {
				return 0, 0, 0, 0, false
			}
			s = s[1:]
		}
		if len(s) < 2 {
			return 0, 0, 0, 0, false
		}
		if *field, ok = atoiDigits(s[:2]); !ok {
			return 0, 0, 0, 0, false
		}
		s = s[2:]
		parsed++
	}
	if minute > 59 || sec > 59 {
		return 0, 0, 0, 0, false
	}
	if s != "" {
		if parsed < 2 || (s[0] != '.' && s[0] != ',') {
			return 0, 0, 0, 0, false
		}
		frac := s[1:]
		if _, ok := atoiDigits(frac); !ok {
			return 0, 0, 0, 0, false
		}
		micro, _ = strconv.Atoi((frac + "000000")[:6])
	}
	return hour, minute, sec, micro, true
}

// atoiDigits parses a string of ASCII digits.
func atoiDigits(s string) (int, bool) {
	n := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, s != ""
}

// daysIn returns the number of days in month of year (proleptic Gregorian).
func daysIn(year, month int) int {
	return time.Date(year, time.Month(month)+1, 0, 0, 0, 0, 0, time.UTC).Day()
}
