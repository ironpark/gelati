package codex

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestE2E drives the installed `codex` CLI. It needs GELATI_CODEX_E2E=1 and
// a signed-in account, and spends a few tokens.
func TestE2E(t *testing.T) {
	if os.Getenv("GELATI_CODEX_E2E") != "1" {
		t.Skip("set GELATI_CODEX_E2E=1 to run against the installed codex CLI")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	client, err := New(ctx, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = client.Close() }()
	if info := client.Info(); info.UserAgent == "" || info.CodexHome == "" {
		t.Fatalf("initialize result = %+v", info)
	}

	models, err := client.ListModels(ctx, ListModelsParams{})
	if err != nil || len(models.Data) == 0 {
		t.Fatalf("ListModels = %+v, %v", models, err)
	}

	policy, reviewer := ApprovalModeAutoReview.Settings()
	thread, err := client.StartThread(ctx, StartThreadParams{
		ThreadSettings: ThreadSettings{
			Cwd:               t.TempDir(),
			Sandbox:           SandboxModeReadOnly,
			ApprovalPolicy:    policy,
			ApprovalsReviewer: reviewer,
		},
		Ephemeral: true,
	})
	if err != nil {
		t.Fatalf("StartThread: %v", err)
	}

	result, err := client.Run(ctx, thread.ID, Text("Reply with exactly the word pong and nothing else."),
		&TurnOptions{
			ApprovalPolicy: ApprovalNever,
			SandboxPolicy:  SandboxModeReadOnly.Policy(),
			Effort:         "low",
			TurnTrigger:    "gelati-e2e",
		})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(strings.ToLower(result.FinalResponse), "pong") {
		t.Fatalf("final response = %q (items: %d)", result.FinalResponse, len(result.Items))
	}
	if result.Turn.Status != TurnCompleted || result.Usage == nil || result.Usage.Total.TotalTokens == 0 {
		t.Fatalf("turn = %+v, usage = %+v", result.Turn, result.Usage)
	}

	result, err = client.Run(ctx, thread.ID, Text(`Return {"answer": 4} for 2+2.`), &TurnOptions{
		OutputSchema: []byte(`{"type":"object","properties":{"answer":{"type":"integer"}},` +
			`"required":["answer"],"additionalProperties":false}`),
	})
	if err != nil {
		t.Fatalf("Run with output schema: %v", err)
	}
	if strings.ReplaceAll(result.FinalResponse, " ", "") != `{"answer":4}` {
		t.Fatalf("structured response = %q", result.FinalResponse)
	}
}
