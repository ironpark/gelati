package sessions

import (
	"bytes"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/ironpark/gelati/claude/internal/transcript"
)

// JSONL rendering of transcript entries for the store readers and the
// session mutations, built on transcript.JSONAppender (compact JSON without
// HTML escaping, "type" first and the other keys sorted).

// jsonField is one key/value pair of an ordered JSON object.
type jsonField struct {
	key   string
	value any
}

// entriesToJSONL serializes store entries to JSONL the way the Python SDK
// does (json.dumps with compact separators and ensure_ascii), hoisting
// "type" to the front of each object, where the CLI writes it too: adapters
// may reorder keys (Postgres JSONB does).
// The remaining keys are written in sorted order.
func entriesToJSONL(entries []Entry) string {
	return asciiEscapeJSON(compactJSONL(entries))
}

// compactJSONL is entriesToJSONL before the ASCII escaping: compact JSON
// lines with "type" first, as JSON.stringify writes them except that U+2028
// and U+2029 are escaped.
func compactJSONL(entries []Entry) []byte {
	var a transcript.JSONAppender
	var buf []byte
	for _, e := range entries {
		buf, _ = a.AppendEntry(buf, e, false)
		buf = append(buf, '\n')
	}
	if len(entries) == 0 {
		buf = append(buf, '\n')
	}
	return buf
}

// stringifySize converts the length of compactJSONL output to the length
// JSON.stringify would produce: Go escapes U+2028 and U+2029 (6 bytes) where
// JSON.stringify writes them raw (3 bytes).
func stringifySize(raw []byte) int64 {
	return int64(len(raw) - 3*(bytes.Count(raw, []byte(`\u2028`))+bytes.Count(raw, []byte(`\u2029`))))
}

// asciiEscapeJSON rewrites every non-ASCII character (and DEL) of
// serialized JSON as a \uXXXX escape, as Python's ensure_ascii does. Such
// characters only occur inside strings, where the escape is equivalent.
func asciiEscapeJSON(b []byte) string {
	var sb strings.Builder
	sb.Grow(len(b))
	for i := 0; i < len(b); {
		c := b[i]
		if c < utf8.RuneSelf && c != 0x7f {
			sb.WriteByte(c)
			i++
			continue
		}
		r, size := utf8.DecodeRune(b[i:])
		i += size
		if r > 0xFFFF {
			r -= 0x10000
			fmt.Fprintf(&sb, `\u%04x\u%04x`, 0xD800+(r>>10), 0xDC00+(r&0x3FF))
		} else {
			fmt.Fprintf(&sb, `\u%04x`, r)
		}
	}
	return sb.String()
}

// pyJSONObject serializes fields, in order, as a compact JSON object the way
// Python's json.dumps with compact separators does: no HTML escaping and
// every non-ASCII character escaped (ensure_ascii). jsontext.Value values
// are copied compacted.
func pyJSONObject(fields []jsonField) (string, error) {
	var a transcript.JSONAppender
	buf, err := a.AppendObject(nil, len(fields), func(i int) (string, any) {
		return fields[i].key, fields[i].value
	}, true, "field")
	if err != nil {
		return "", err
	}
	return asciiEscapeJSON(buf), nil
}
