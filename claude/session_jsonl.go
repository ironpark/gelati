package claude

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"
)

// JSONL serialization of transcript entries, shared by the store readers,
// resume materialization and the session mutations. Every writer emits
// compact JSON without HTML escaping; entries whose key order is unknown
// (decoded maps) are written with "type" first, where the CLI writes it, and
// the other keys sorted: adapters may reorder keys (Postgres JSONB does).

// jsonField is one key/value pair of an ordered JSON object.
type jsonField struct {
	key   string
	value any
}

// typeFirstKeys returns the keys of e with "type" (when present) first and
// the others sorted.
func typeFirstKeys(e map[string]any) []string {
	keys := make([]string, 0, len(e))
	for k := range e {
		if k != "type" {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	if _, ok := e["type"]; ok {
		keys = append([]string{"type"}, keys...)
	}
	return keys
}

// appendJSONValue appends v to dst as compact JSON without HTML escaping or
// a trailing newline. On error dst is returned unchanged.
func appendJSONValue(dst []byte, v any) ([]byte, error) {
	buf := bytes.NewBuffer(dst)
	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return dst, err
	}
	b := buf.Bytes()
	return b[:len(b)-1], nil // drop Encode's trailing newline
}

// appendEntryJSON appends entry e to dst as a JSON object with its keys in
// typeFirstKeys order, without a trailing newline; a nil entry is written as
// null. When strict is set, a value that cannot be encoded is an error;
// otherwise it is written as null.
func appendEntryJSON(dst []byte, e map[string]any, strict bool) ([]byte, error) {
	if e == nil {
		return append(dst, "null"...), nil
	}
	dst = append(dst, '{')
	for i, k := range typeFirstKeys(e) {
		if i > 0 {
			dst = append(dst, ',')
		}
		var err error
		if dst, err = appendJSONValue(dst, k); err != nil {
			return dst, err
		}
		dst = append(dst, ':')
		next, err := appendJSONValue(dst, e[k])
		if err != nil {
			if strict {
				return dst, fmt.Errorf("entry field %q: %w", k, err)
			}
			next = append(dst, "null"...)
		}
		dst = next
	}
	return append(dst, '}'), nil
}

// entriesToJSONL serializes store entries to JSONL the way the Python SDK
// does (json.dumps with compact separators and ensure_ascii), hoisting
// "type" to the front of each object, where the CLI writes it too: adapters
// may reorder keys (Postgres JSONB does).
// The remaining keys are written in sorted order.
func entriesToJSONL(entries []SessionStoreEntry) string {
	return asciiEscapeJSON(compactJSONL(entries))
}

// compactJSONL is entriesToJSONL before the ASCII escaping: compact JSON
// lines with "type" first, as JSON.stringify writes them except that U+2028
// and U+2029 are escaped.
func compactJSONL(entries []SessionStoreEntry) []byte {
	var buf []byte
	for _, e := range entries {
		buf, _ = appendEntryJSON(buf, e, false)
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
// every non-ASCII character escaped (ensure_ascii). json.RawMessage values
// are copied compacted.
func pyJSONObject(fields []jsonField) (string, error) {
	buf := []byte{'{'}
	for i, f := range fields {
		if i > 0 {
			buf = append(buf, ',')
		}
		var err error
		if buf, err = appendJSONValue(buf, f.key); err != nil {
			return "", err
		}
		buf = append(buf, ':')
		if buf, err = appendJSONValue(buf, f.value); err != nil {
			return "", fmt.Errorf("field %q: %w", f.key, err)
		}
	}
	buf = append(buf, '}')
	return asciiEscapeJSON(buf), nil
}

// writeEntriesJSONL streams entries to path as one compact JSON object per
// line, with "type" first as in entriesToJSONL, and makes the file 0600.
// Unlike entriesToJSONL it fails on a value that cannot be encoded instead
// of writing null for it.
func writeEntriesJSONL(path string, entries []SessionStoreEntry) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("claude: writing %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("claude: writing %s: %w", path, err)
	}
	w := bufio.NewWriter(f)
	var line []byte
	for _, e := range entries {
		if line, err = appendEntryJSON(line[:0], e, true); err != nil {
			break
		}
		if _, err = w.Write(append(line, '\n')); err != nil {
			break
		}
	}
	if err == nil {
		err = w.Flush()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("claude: writing %s: %w", path, err)
	}
	_ = os.Chmod(path, 0o600)
	return nil
}
