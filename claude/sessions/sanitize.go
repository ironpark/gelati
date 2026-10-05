package sessions

import (
	"strings"
	"unicode"

	"github.com/ironpark/gelati/claude/internal/unicodenorm"
)

// ---------------------------------------------------------------------------
// Unicode sanitization
// ---------------------------------------------------------------------------

// sanitizeUnicode removes characters that are invisible or can be abused
// for spoofing: it repeatedly applies NFKC normalization and strips format
// (Cf), private-use (Co) and unassigned (Cn) code points until the result
// is stable, at most 10 times. Invalid UTF-8 becomes U+FFFD.
//
// The categories come from Go's unicode tables and NFKC from tables
// generated from Python's unicodedata, so the result can differ from the
// Python SDK's for code points whose status differs between the Unicode
// versions involved (for example characters assigned after the Python
// runtime's Unicode version, which Python strips as unassigned).
func sanitizeUnicode(s string) string {
	current := string([]rune(s))
	for range 10 {
		previous := current
		current = unicodenorm.NFKC(current)
		current = strings.Map(func(r rune) rune {
			if isStrippedRune(r) {
				return -1
			}
			return r
		}, current)
		if current == previous {
			break
		}
	}
	return current
}

// isStrippedRune reports whether sanitizeUnicode removes r.
func isStrippedRune(r rune) bool {
	// Explicit ranges, redundant with the category checks but kept to
	// match the reference implementations: zero-width spaces and LTR/RTL
	// marks, directional formatting, directional isolates, BOM, and the
	// BMP private use area.
	switch {
	case r >= 0x200b && r <= 0x200f, r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069,
		r == 0xfeff, r >= 0xe000 && r <= 0xf8ff:
		return true
	}
	return unicode.In(r, unicode.Cf, unicode.Co, unicode.Cn)
}
