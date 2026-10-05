package logx

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestOrDefaultPassesWarnings(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	l := Or(nil).With("pkg", "test")
	l.Debug("debug")
	l.Info("info")
	l.Warn("warn")
	l.Error("error")
	out := buf.String()
	if strings.Contains(out, "debug") || strings.Contains(out, "msg=info") ||
		!strings.Contains(out, "msg=warn pkg=test") || !strings.Contains(out, "msg=error") {
		t.Fatalf("output = %q", out)
	}
}

func TestOrKeepsLogger(t *testing.T) {
	l := slog.New(slog.DiscardHandler)
	if Or(l) != l {
		t.Fatal("Or replaced a non-nil logger")
	}
}
