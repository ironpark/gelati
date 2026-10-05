package harness

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ironpark/gelati/agy/internal/wire"
)

// TestRealHarness drives the real localharness binary named by
// GELATI_AGY_HARNESS through the handshake and the initialize round
// trip. No model credentials are needed for that part.
func TestRealHarness(t *testing.T) {
	bin := os.Getenv("GELATI_AGY_HARNESS")
	if bin == "" {
		t.Skip("set GELATI_AGY_HARNESS=/path/to/localharness to run")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	var stderr strings.Builder
	h, err := Start(ctx, Options{
		CLIPath:          bin,
		Env:              map[string]string{"GEMINI_API_KEY": "dummy-key"},
		StorageDirectory: t.TempDir(),
		Stderr:           &lockedWriter{w: &stderr},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Logf("harness pid %d listening on port %d", h.PID(), h.Port())

	workspace := t.TempDir()
	resp, err := h.Initialize(ctx, wire.HarnessConfig_builder{
		CascadeId:               new(""),
		SessionContinuationMode: new(wire.HarnessConfig_SESSION_CONTINUATION_MODE_UNSPECIFIED),
		Workspaces: []*wire.Workspace{wire.Workspace_builder{
			FilesystemWorkspace: wire.FilesystemWorkspace_builder{Directory: new(workspace)}.Build(),
		}.Build()},
		HarnessSideTools: wire.HarnessSideTools_builder{
			Find:       wire.FindToolConfig_builder{Enabled: new(true)}.Build(),
			ViewFile:   wire.ViewFileToolConfig_builder{Enabled: new(true)}.Build(),
			RunCommand: wire.RunCommandToolConfig_builder{Enabled: new(false)}.Build(),
		}.Build(),
		// Upstream's default text model (models.DEFAULT_MODEL in v0.1.20).
		Models: []*wire.ModelConfig{wire.ModelConfig_builder{
			Name:              new("gemini-3.8-flash"),
			Types:             []wire.ModelType{wire.ModelType_MODEL_TYPE_TEXT},
			GeminiApiEndpoint: wire.GeminiAPIEndpoint_builder{ApiKey: new("dummy-key")}.Build(),
		}.Build()},
		AppDataDir:    new(t.TempDir()),
		AgentBehavior: new(wire.AgentBehavior_AGENT_BEHAVIOR_AUTONOMOUS),
	}.Build())
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	b, _ := wire.Marshal(resp)
	t.Logf("initialize response: %s", b)
	if resp.GetCascadeId() == "" {
		t.Errorf("initialize response has no cascade id")
	}

	// A prompt cannot reach a model with a dummy key, but the harness must
	// accept it and answer with events.
	if err := h.Send(ctx, wire.InputEvent_builder{UserInput: wire.UserInput_builder{
		Parts: []*wire.UserInput_Part{wire.UserInput_Part_builder{Text: new("Say hi")}.Build()},
	}.Build()}.Build()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	for i := 0; i < 50; i++ {
		ev, err := h.Receive(ctx)
		if err != nil {
			t.Fatalf("Receive: %v", err)
		}
		b, _ := wire.Marshal(ev)
		t.Logf("event: %s", truncate(b, 400))
		if tsu := ev.GetTrajectoryStateUpdate(); tsu != nil && tsu.GetState() == wire.TrajectoryStateUpdate_STATE_FULLY_IDLE {
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
