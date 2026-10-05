package unicodenorm

import (
	"testing"
)

func TestNFC(t *testing.T) {
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
		if got := NFC(tt.in); got != tt.want {
			t.Errorf("NFC(%+q) = %+q, want %+q", tt.in, got, tt.want)
		}
	}
}

func TestNFKC(t *testing.T) {
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
		if got := NFKC(tt.in); got != tt.want {
			t.Errorf("NFKC(%+q) = %+q, want %+q", tt.in, got, tt.want)
		}
	}
}
