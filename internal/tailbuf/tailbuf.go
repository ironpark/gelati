// Package tailbuf keeps the last lines written to it, so the SDK packages
// can attach a subprocess's recent stderr to the error reporting its exit.
package tailbuf

import (
	"bytes"
	"io"
	"strings"
	"sync"
)

// maxPartial bounds an unterminated line before it is kept as a line.
const maxPartial = 64 << 10

// Buffer keeps the last Max lines written to it and copies all output to
// Tee. The zero value keeps nothing. It is safe for concurrent use.
type Buffer struct {
	Tee io.Writer
	Max int

	mu      sync.Mutex
	lines   []string // ring buffer of up to Max lines, oldest at start
	start   int
	partial []byte
}

// Write implements io.Writer. It never fails; Tee errors are ignored.
func (b *Buffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.Tee != nil {
		_, _ = b.Tee.Write(p)
	}
	b.partial = append(b.partial, p...)
	for {
		i := bytes.IndexByte(b.partial, '\n')
		if i < 0 {
			break
		}
		b.push(strings.TrimRight(string(b.partial[:i]), "\r"))
		b.partial = b.partial[i+1:]
	}
	if len(b.partial) > maxPartial {
		b.push(string(b.partial))
		b.partial = nil
	}
	return len(p), nil
}

func (b *Buffer) push(line string) {
	switch {
	case b.Max <= 0:
	case len(b.lines) < b.Max:
		b.lines = append(b.lines, line)
	default:
		b.lines[b.start] = line
		b.start = (b.start + 1) % b.Max
	}
}

// String returns the retained lines, including an unterminated last line.
func (b *Buffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	lines := make([]string, 0, len(b.lines)+1)
	lines = append(lines, b.lines[b.start:]...)
	lines = append(lines, b.lines[:b.start]...)
	if len(b.partial) > 0 {
		lines = append(lines, string(b.partial))
	}
	return strings.Join(lines, "\n")
}
