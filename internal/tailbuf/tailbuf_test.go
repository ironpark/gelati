package tailbuf

import (
	"io"
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
