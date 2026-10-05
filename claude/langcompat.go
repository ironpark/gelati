package claude

import (
	"encoding/json/v2"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ironpark/gelati/internal/jsonx"
)

// Helpers reproducing Python and JavaScript semantics (whitespace, string
// lengths, truthiness, slicing, UTF-8 decoding) that the session code needs
// to match the reference SDKs byte for byte.

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
	case numberText:
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

// nilIfEmpty returns nil for an empty slice, so "no results" is always nil.
func nilIfEmpty[T any](s []T) []T {
	if len(s) == 0 {
		return nil
	}
	return s
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

// pyISONowUTC returns the current UTC time the way Python's
// datetime.now(timezone.utc).isoformat() does, with "Z" for "+00:00":
// microsecond precision, and no fraction when it is zero.
func pyISONowUTC() string {
	t := time.Now().UTC()
	if t.Nanosecond()/1000 == 0 {
		return t.Format("2006-01-02T15:04:05Z")
	}
	return t.Format("2006-01-02T15:04:05.000000Z")
}

// jsTruthy reports JavaScript truthiness for a decoded JSON value (unlike
// truthy, empty arrays and objects are truthy).
func jsTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x != 0 && x == x
	case int:
		return x != 0
	case int64:
		return x != 0
	}
	return true
}

// jsStrictEqual is JavaScript === for two possibly absent JSON values:
// absent (undefined) equals only absent, objects and arrays never compare
// equal (they are distinct references after parsing).
func jsStrictEqual(a any, aok bool, b any, bok bool) bool {
	if aok != bok {
		return false
	}
	if !aok {
		return true
	}
	switch a.(type) {
	case map[string]any, []any:
		return false
	}
	switch b.(type) {
	case map[string]any, []any:
		return false
	}
	return a == b
}

// jsJSONStringify serializes a decoded JSON value for equality checks.
// Keys are sorted, so (unlike JSON.stringify) key order is ignored.
func jsJSONStringify(v any) string {
	b, _ := json.Marshal(v, jsonx.LegacyEncode)
	return string(b)
}
