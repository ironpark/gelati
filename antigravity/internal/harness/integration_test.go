package harness

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ironpark/gelati/antigravity/internal/wire"
)

// TestRealHarness drives the real localharness binary named by
// GELATI_ANTIGRAVITY_HARNESS through the handshake and the initialize round
// trip. No model credentials are needed for that part.
func TestRealHarness(t *testing.T) {
	bin := os.Getenv("GELATI_ANTIGRAVITY_HARNESS")
	if bin == "" {
		t.Skip("set GELATI_ANTIGRAVITY_HARNESS=/path/to/localharness to run")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	var stderr strings.Builder
	h, err := Start(ctx, Options{
		BinaryPath:       bin,
		Env:              map[string]string{"GEMINI_API_KEY": "dummy-key"},
		StorageDirectory: t.TempDir(),
		Stderr:           &lockedWriter{w: &stderr},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Logf("harness pid %d listening on port %d", h.PID(), h.Port())

	workspace := t.TempDir()
	resp, err := h.Initialize(ctx, &wire.HarnessConfig{
		CascadeID:               new(""),
		SessionContinuationMode: new(wire.HarnessConfigSessionContinuationModeUnspecified),
		Workspaces: []*wire.Workspace{{
			FilesystemWorkspace: &wire.FilesystemWorkspace{Directory: new(workspace)},
		}},
		HarnessSideTools: &wire.HarnessSideTools{
			Find:       &wire.FindToolConfig{Enabled: new(true)},
			ViewFile:   &wire.ViewFileToolConfig{Enabled: new(true)},
			RunCommand: &wire.RunCommandToolConfig{Enabled: new(false)},
		},
		// Upstream's default text model (models.DEFAULT_MODEL in v0.1.20).
		Models: []*wire.ModelConfig{{
			Name:              new("gemini-3.8-flash"),
			Types:             []wire.ModelType{wire.ModelTypeText},
			GeminiAPIEndpoint: &wire.GeminiAPIEndpoint{APIKey: new("dummy-key")},
		}},
		AppDataDir:    new(t.TempDir()),
		AgentBehavior: new(wire.AgentBehaviorAutonomous),
	})
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	b, _ := wire.Marshal(resp)
	t.Logf("initialize response: %s", b)
	if resp.GetCascadeID() == "" {
		t.Errorf("initialize response has no cascade id")
	}

	// A prompt cannot reach a model with a dummy key, but the harness must
	// accept it and answer with events.
	if err := h.Send(ctx, &wire.InputEvent{UserInput: &wire.UserInput{
		Parts: []*wire.UserInputPart{{Text: new("Say hi")}},
	}}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	for i := 0; i < 50; i++ {
		ev, err := h.Receive(ctx)
		if err != nil {
			t.Fatalf("Receive: %v", err)
		}
		b, _ := wire.Marshal(ev)
		t.Logf("event: %s", truncate(b, 400))
		if tsu := ev.GetTrajectoryStateUpdate(); tsu != nil && tsu.GetState() == wire.TrajectoryStateUpdateStateFullyIdle {
			break
		}
	}

	start := time.Now()
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-h.Exited():
	default:
		t.Fatal("process still running after Close")
	}
	t.Logf("closed in %v, exit err %v", time.Since(start), h.ExitErr())
	if _, err := h.Receive(ctx); err != ErrClosed {
		t.Errorf("Receive after Close = %v, want ErrClosed", err)
	}
}
