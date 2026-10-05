package agy_test

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ironpark/gelati/agy"
)

// syncBuffer is a goroutine-safe strings.Builder for harness stderr.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// realHarnessAgent starts an agent on the real localharness binary named by
// GELATI_AGY_HARNESS, with an invalid API key.
func realHarnessAgent(t *testing.T, ctx context.Context, hooks ...agy.Hook) (*agy.Agent, *syncBuffer) {
	t.Helper()
	bin := os.Getenv("GELATI_AGY_HARNESS")
	if bin == "" {
		t.Skip("set GELATI_AGY_HARNESS=/path/to/localharness to run")
	}
	stderr := &syncBuffer{}
	agent, err := agy.NewAgent(agy.Config{
		CLIPath:      bin,
		APIKey:       "invalid-key",
		Workspaces:   []string{t.TempDir()},
		SaveDir:      t.TempDir(),
		AppDataDir:   t.TempDir(),
		Capabilities: &agy.CapabilitiesConfig{EnabledTools: agy.ReadOnlyTools()},
		Stderr:       stderr,
		Hooks:        hooks,
		Logger:       slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.Start(ctx); err != nil {
		t.Fatalf("Start: %v\nstderr:\n%s", err, stderr.String())
	}
	return agent, stderr
}

// wantTurnError checks that a turn ended with a connection or execution
// error rather than succeeding or hanging.
func wantTurnError(t *testing.T, ctx context.Context, agent *agy.Agent, prompt string) {
	t.Helper()
	resp, err := agent.Chat(ctx, agy.Text(prompt))
	if err == nil {
		_, err = resp.WaitText(ctx)
	}
	var connErr *agy.ConnectionError
	var execErr *agy.ExecutionError
	if !errors.As(err, &connErr) && !errors.As(err, &execErr) {
		t.Fatalf("turn error = %v (%T), want a connection or execution error", err, err)
	}
	t.Logf("turn ended with %T: %.120s", err, err.Error())
}

// TestRealHarnessWithoutCredentials checks that a session starts and that a
// chat without valid credentials ends with an error instead of hanging.
func TestRealHarnessWithoutCredentials(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	agent, stderr := realHarnessAgent(t, ctx)
	t.Logf("sandbox status: %+v", agent.SandboxStatus())

	wantTurnError(t, ctx, agent, "Say hi")
	conv := agent.Conversation()
	for _, s := range conv.History() {
		t.Logf("step %s type=%s source=%s status=%s http=%d", s.ID, s.Type, s.Source, s.Status, s.HTTPCode)
	}
	if conv.ConversationID() == "" {
		t.Error("no conversation ID after a turn")
	}
	start := time.Now()
	if err := agent.Close(); err != nil {
		t.Fatalf("Close: %v\nstderr:\n%s", err, stderr.String())
	}
	t.Logf("closed in %v", time.Since(start))
}

// TestRealHarnessSessionEndHook checks the session end handshake: Close
// asks the harness to run the session end hooks, which call back into the
// SDK.
func TestRealHarnessSessionEndHook(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	var sessionEnded atomic.Bool
	agent, stderr := realHarnessAgent(t, ctx, agy.OnSessionEndHook(func(context.Context, *agy.HookContext) error {
		sessionEnded.Store(true)
		return nil
	}))
	if err := agent.Close(); err != nil {
		t.Fatalf("Close: %v\nstderr:\n%s", err, stderr.String())
	}
	if !sessionEnded.Load() {
		t.Error("session end hook did not run")
	}
}

// TestRealHarnessTurnAfterFatalError checks that a turn after a fatal model
// error also fails cleanly: the harness ends the session after an agent run
// fails, which surfaces as a connection error rather than a hang.
func TestRealHarnessTurnAfterFatalError(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	agent, _ := realHarnessAgent(t, ctx)
	wantTurnError(t, ctx, agent, "Say hi")
	wantTurnError(t, ctx, agent, "Say hi again")
	if got := agent.Conversation().TurnCount(); got != 2 {
		t.Errorf("TurnCount = %d, want 2", got)
	}
	if err := agent.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
