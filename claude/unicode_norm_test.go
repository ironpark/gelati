package claude

import (
	"strings"
	"testing"
)

func TestSessionNormalizeNFC(t *testing.T) {
	t.Parallel()
	// Expected values from Python's unicodedata.normalize("NFC", ...). The
	// tables were also checked exhaustively against Python for every code
	// point and its NFD form when they were generated.
	tests := []struct{ in, want string }{
		{"", ""},
		{"/plain/ascii-path", "/plain/ascii-path"},
		{"/Users/zo\u00eb/caf\u00e9", "/Users/zo\u00eb/caf\u00e9"},
		{"cafe\u0301", "caf\u00e9"},
		{"\u1100\u1161\u11a8", "\uac01"},
		{"A\u0323\u0307", "\u1ea0\u0307"},
		{"A\u0307\u0323", "\u1ea0\u0307"}, // reordered by combining class
		{"\u212b", "\u00c5"},
		{"e\u0301\u0301", "\u00e9\u0301"},
		{"\u0301e", "\u0301e"},
		{"\uac00\u11a8", "\uac01"},
		{"\uf900", "\u8c48"},
		{"\u0344", "\u0308\u0301"},
		{"\ud55c\uad6d\uc5b4", "\ud55c\uad6d\uc5b4"},
		{"\xffcafe\u0301\xfe", "\xffcaf\u00e9\xfe"}, // invalid bytes preserved
	}
	for _, tt := range tests {
		if got := normalizeNFC(tt.in); got != tt.want {
			t.Errorf("normalizeNFC(%+q) = %+q, want %+q", tt.in, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Unicode sanitization
// ---------------------------------------------------------------------------

func TestSessionSanitizeUnicode(t *testing.T) {
	t.Parallel()
	// Expected values from the Python SDK's _sanitize_unicode.
	tests := []struct{ in, want string }{
		{"hello", "hello"},
		{"tag-with-dashes_123", "tag-with-dashes_123"},
		{"a\u200bb", "ab"},
		{"a\u200cb", "ab"},
		{"a\u200db", "ab"},
		{"\ufeffhello", "hello"},
		{"a\u202ab\u202cc", "abc"},
		{"a\u2066b\u2069c", "abc"},
		{"a\ue000b", "ab"},
		{"a\uf8ffb", "ab"},
		{"\uff21", "A"},
		{"a" + strings.Repeat("\u200b", 20) + "b", "ab"},
		{"\uff48\uff45\uff4c\uff4c\uff4f\u200b", "hello"},
		{"\ufb01le", "file"},
		{"\u2460\u2461", "12"},
		{"\uff21\u0308", "\u00c4"},
		{"a\U000E0001b", "ab"}, // language tag (Cf)
		{"a\U000F0000b", "ab"}, // supplementary private use (Co)
		{"a\U0010FFFFb", "ab"}, // noncharacter (Cn)
		{"x\u0378y", "xy"},     // unassigned (Cn)
		{"\u00a0tag\u00a0", " tag "},
		{"\u337f", "\u682a\u5f0f\u4f1a\u793e"},
		{"e\u0301", "\u00e9"},
		{"\u1100\u1161\u11a8", "\uac01"},
		{"\u3131\u314f", "\uac00"},
		{"\u1e9b\u0323", "\u1e69"},
		{"a\u2028b", "a\u2028b"},
		{"  \u200b  ", "    "},
		{"bad\xffbyte", "bad\ufffdbyte"},
	}
	for _, tt := range tests {
		if got := sanitizeUnicode(tt.in); got != tt.want {
			t.Errorf("sanitizeUnicode(%+q) = %+q, want %+q", tt.in, got, tt.want)
		}
	}
}

func TestSessionNormalizeNFKC(t *testing.T) {
	t.Parallel()
	// Expected values from Python's unicodedata.normalize("NFKC", ...). The
	// tables were also checked against Python for every code point and for
	// 20000 random sequences when they were generated.
	tests := []struct{ in, want string }{
		{"", ""},
		{"plain ascii", "plain ascii"},
		{"\uff48\uff45\uff4c\uff4c\uff4f\u200b", "hello\u200b"},
		{"\ufb01", "fi"},
		{"\u00bd", "1\u20442"},
		{"\u2126", "\u03a9"},
		{"\u1e9b\u0323", "\u1e69"},
		{"\u3131\u314f", "\uac00"},
		{"\uac00\u11a8", "\uac01"},
		{"A\u0307\u0323", "\u1ea0\u0307"},
		{"\ufdfa", "\u0635\u0644\u0649 \u0627\u0644\u0644\u0647 \u0639\u0644\u064a\u0647 \u0648\u0633\u0644\u0645"},
	}
	for _, tt := range tests {
		if got := normalizeNFKC(tt.in); got != tt.want {
			t.Errorf("normalizeNFKC(%+q) = %+q, want %+q", tt.in, got, tt.want)
		}
	}
}
