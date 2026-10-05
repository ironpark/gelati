// Package logx holds the logging default shared by the SDK packages.
package logx

import (
	"context"
	"log/slog"
)

// Or returns l, or, when l is nil, the default SDK logger: warnings and errors
// go to slog's default logger (stderr unless the program changed it), and
// debug and info records are dropped.
func Or(l *slog.Logger) *slog.Logger {
	if l != nil {
		return l
	}
	return slog.New(warnOnly{slog.Default().Handler()})
}

// warnOnly passes records at slog.LevelWarn and above to the wrapped handler.
type warnOnly struct{ slog.Handler }

func (h warnOnly) Enabled(ctx context.Context, level slog.Level) bool {
	return level >= slog.LevelWarn && h.Handler.Enabled(ctx, level)
}

func (h warnOnly) WithAttrs(attrs []slog.Attr) slog.Handler {
	return warnOnly{h.Handler.WithAttrs(attrs)}
}

func (h warnOnly) WithGroup(name string) slog.Handler {
	return warnOnly{h.Handler.WithGroup(name)}
}
