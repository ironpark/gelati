package sessions

import (
	"strings"
	"testing"
)

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
