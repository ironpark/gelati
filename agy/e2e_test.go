package agy_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ironpark/gelati/agy"
)

// TestE2EHelloWorld chats with a real model through the real harness. It is
// skipped unless GELATI_AGY_E2E=1, since it needs GEMINI_API_KEY and
// the localharness binary (ANTIGRAVITY_HARNESS_PATH or PATH) and costs
// money.
func TestE2EHelloWorld(t *testing.T) {
	if os.Getenv("GELATI_AGY_E2E") != "1" {
		t.Skip("set GELATI_AGY_E2E=1 (with GEMINI_API_KEY and the harness binary) to run")
	}
	if os.Getenv("GEMINI_API_KEY") == "" {
		t.Fatal("GEMINI_API_KEY is not set")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()

	var stderr syncBuffer
	agent, err := agy.NewAgent(agy.Config{
		SystemInstructions: agy.TextSystemInstructions("Answer with a single word."),
		Capabilities:       &agy.CapabilitiesConfig{EnabledTools: agy.ReadOnlyTools()},
		Workspaces:         []string{t.TempDir()},
		SaveDir:            t.TempDir(),
		Stderr:             &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.Start(ctx); err != nil {
		t.Fatalf("Start: %v\nstderr:\n%s", err, stderr.String())
	}
	defer agent.Close()

	resp, err := agent.Chat(ctx, agy.Text("What is the capital of France?"))
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	for delta, err := range resp.Text(ctx) {
		if err != nil {
			t.Fatalf("stream: %v\nstderr:\n%s", err, stderr.String())
		}
		text.WriteString(delta)
	}
	if !strings.Contains(strings.ToLower(text.String()), "paris") {
		t.Fatalf("answer = %q", text.String())
	}
	if u := resp.UsageMetadata(); u == nil || u.TotalTokenCount == nil || *u.TotalTokenCount == 0 {
		t.Errorf("usage = %+v", u)
	}
	if agent.ConversationID() == "" {
		t.Error("no conversation ID")
	}
	t.Logf("answer %q, conversation %s, stop reason %s", text.String(), agent.ConversationID(), resp.StopReason())
	if err := agent.Close(); err != nil {
		t.Fatal(err)
	}
}
