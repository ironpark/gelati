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

	result, err := thread.RunTurn(ctx, TurnRequest{
		Input: []InputItem{Text("Reply with exactly the word pong and nothing else.")},
		TurnOptions: TurnOptions{
			ApprovalPolicy: ApprovalNever,
			SandboxPolicy:  SandboxModeReadOnly.Policy(),
			Effort:         "low",
			TurnTrigger:    "gelati-e2e",
		},
	})
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if !strings.Contains(strings.ToLower(result.Text()), "pong") {
		t.Fatalf("final response = %q (items: %d)", result.Text(), len(result.Items))
	}
	if result.Turn.Status != TurnCompleted || result.Usage == nil || result.Usage.Total.TotalTokens == 0 {
		t.Fatalf("turn = %+v, usage = %+v", result.Turn, result.Usage)
	}

	type answer struct {
		Answer int    `json:"answer" description:"The sum."`
		Note   string `json:"note,omitempty"`
	}
	result, err = thread.RunTurn(ctx, TurnRequest{
		Input:       []InputItem{Text("What is 2+2? Leave the note empty.")},
		TurnOptions: TurnOptions{OutputSchema: SchemaFor[answer]()},
	})
	if err != nil {
		t.Fatalf("RunTurn with output schema: %v", err)
	}
	var got answer
	if err := result.DecodeStructuredOutput(&got); err != nil || got.Answer != 4 {
		t.Fatalf("structured response = %q: %+v, %v", result.Text(), got, err)
	}
}
