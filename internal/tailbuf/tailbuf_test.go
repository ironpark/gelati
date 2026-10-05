package tailbuf

import (
	"io"
	"strings"
	"testing"
)

func TestBuffer(t *testing.T) {
	b := &Buffer{Max: 3}
	io.WriteString(b, "one\ntwo\r\nthr")
	io.WriteString(b, "ee\nfour\nfive")
	if got := b.String(); got != "two\nthree\nfour\nfive" {
		t.Fatalf("tail = %q, want the last 3 complete lines plus the partial one", got)
	}
}

func TestBufferAddLineCapsLength(t *testing.T) {
	b := &Buffer{Max: 2}
	b.AddLine("a")
	b.AddLine(strings.Repeat("x", maxLine+10))
	got := b.String()
	if !strings.HasPrefix(got, "a\nxxx") || len(got) != len("a\n")+maxLine+len("…") {
		t.Fatalf("tail length = %d", len(got))
	}
}
