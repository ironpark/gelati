package claude

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Lite metadata extraction: SessionInfo is derived from a stat plus the
// first and last liteReadBufSize bytes of a transcript, scanned for string
// fields without a full JSONL parse.

const (
	// liteReadBufSize is the size of the head and tail buffers read for
	// lite metadata extraction.
	liteReadBufSize = 65536

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
)

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

// jsonlToLite builds the head/tail lite shape from an in-memory transcript,
// with the byte semantics of readSessionLite.
func jsonlToLite[T string | []byte](jsonl T, mtime int64) *liteSessionFile {
	size := len(jsonl)
	head := decodeUTF8Replace([]byte(jsonl[:min(size, liteReadBufSize)]))
	tail := head
	if size > liteReadBufSize {
		tail = decodeUTF8Replace([]byte(jsonl[size-liteReadBufSize:]))
	}
	return &liteSessionFile{mtime: mtime, size: int64(size), head: head, tail: tail}
}

// mtimeFromJSONLTail returns the last entry's timestamp in Unix epoch
// milliseconds, falling back to the current time when it is absent or
// unparseable.
func mtimeFromJSONLTail(jsonl string) int64 {
	trimmed := strings.TrimRightFunc(jsonl, pyIsSpace)
	lastLine := trimmed[strings.LastIndexByte(trimmed, '\n')+1:]
	var obj map[string]any
	if json.Unmarshal([]byte(lastLine), &obj) == nil {
		if ts, ok := obj["timestamp"].(string); ok {
			if ms, ok := isoToEpochMillis(ts); ok {
				return ms
			}
		}
	}
	return time.Now().UnixMilli()
}

// ---------------------------------------------------------------------------
// Metadata extraction shared by ListSessions and GetSessionInfo
// ---------------------------------------------------------------------------

// recordedSessionCwd returns the working directory a session was recorded
// in: the last relocation in the tail, else the first parseable cwd in the
// head.
func recordedSessionCwd(lite *liteSessionFile) (string, bool) {
	if cwd, ok := lastTypedStringField(lite.tail, "relocated", "relocatedCwd"); ok {
		return cwd, true
	}
	return firstLineStringField(lite.head, "cwd")
}

// liteTitle returns the title of a lite-read transcript. A user-set title
// (customTitle) wins over a generated one (aiTitle); the head fallbacks
// cover short sessions whose title entry is not in the tail. sidecarTitle,
// the custom-title.json title or "", ranks below a tail customTitle and
// above a head-only one.
func liteTitle(lite *liteSessionFile, sidecarTitle string) string {
	return firstNonEmpty(
		extractLastJSONStringField(lite.tail, "customTitle"),
		sidecarTitle,
		extractLastJSONStringField(lite.head, "customTitle"),
		extractLastJSONStringField(lite.tail, "aiTitle"),
		extractLastJSONStringField(lite.head, "aiTitle"),
	)
}

// liteSidecarTitle returns the custom-title.json title of the transcript at
// transcriptPath when its tail has no customTitle (the sidecar cannot win
// otherwise; see liteTitle), else "".
func liteSidecarTitle(lite *liteSessionFile, transcriptPath, sessionID string) string {
	if _, ok := lookupLastJSONStringField(lite.tail, "customTitle"); ok {
		return ""
	}
	return readCustomTitleSidecar(transcriptPath, sessionID)
}

// parseSessionInfoFromLite derives SessionInfo from a lite read. It returns
// nil for sidechain sessions and for metadata-only sessions with no
// extractable summary. projectPath is the Cwd fallback and may be empty;
// sidecarTitle is the custom-title.json title or "" (see liteTitle).
func parseSessionInfoFromLite(sessionID string, lite *liteSessionFile, projectPath, sidecarTitle string) *SessionInfo {
	head, tail := lite.head, lite.tail

	firstLine, _, _ := strings.Cut(head, "\n")
	if strings.Contains(firstLine, `"isSidechain":true`) || strings.Contains(firstLine, `"isSidechain": true`) {
		return nil
	}

	customTitle := liteTitle(lite, sidecarTitle)
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
