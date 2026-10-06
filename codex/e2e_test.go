package codex

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ironpark/gelati"
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

// TestE2EGelati drives the installed `codex` CLI through package gelati. It
// needs GELATI_CODEX_E2E=1, like TestE2E.
func TestE2EGelati(t *testing.T) {
	if os.Getenv("GELATI_CODEX_E2E") != "1" {
		t.Skip("set GELATI_CODEX_E2E=1 to run against the installed codex CLI")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	type answer struct {
		Answer int    `json:"answer"`
		Note   string `json:"note,omitempty"`
	}
	a, err := gelati.Open(ctx, Provider(Options{}, StartThreadParams{Ephemeral: true}), gelati.Config{
		Instructions: "Answer arithmetic questions only.",
		OutputSchema: gelati.SchemaFor[answer](),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = a.Close() }()
	if a.ID() == "" {
		t.Fatal("empty thread id")
	}
	turn, err := a.Send(ctx, gelati.Text("What is 2+2?"))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	var text strings.Builder
	for delta, err := range turn.Text(ctx) {
		if err != nil {
			t.Fatalf("Text: %v", err)
		}
		text.WriteString(delta)
	}
	res, err := turn.Result(ctx)
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	var got answer
	if err := res.DecodeStructuredOutput(&got); err != nil || got.Answer != 4 {
		t.Fatalf("structured output %q: %+v, %v", res.StructuredOutput, got, err)
	}
	if text.String() != res.Text || res.Usage.InputTokens == 0 || res.Usage.OutputTokens == 0 {
		t.Fatalf("streamed %q, result %+v", text.String(), res)
	}
}

// TestE2EGelatiToolsAndImages checks gelati tools, approvals and image input
// against the installed codex CLI.
func TestE2EGelatiToolsAndImages(t *testing.T) {
	if os.Getenv("GELATI_CODEX_E2E") != "1" {
		t.Skip("set GELATI_CODEX_E2E=1 to run against the installed codex CLI")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	type lookup struct {
		Key string `json:"key" description:"The key to look up"`
	}
	var toolCalls []string
	secret := gelati.NewTool("secret_value", "Returns the secret value stored under a key",
		func(_ context.Context, in lookup) (string, error) {
			toolCalls = append(toolCalls, in.Key)
			return "pineapple-" + in.Key, nil
		})
	var approvals []gelati.ToolRequest
	a, err := gelati.Open(ctx, Provider(Options{}, StartThreadParams{
		Ephemeral:      true,
		ThreadSettings: ThreadSettings{ApprovalPolicy: ApprovalUntrusted, Sandbox: SandboxModeReadOnly, Cwd: t.TempDir()},
	}), gelati.Config{
		Tools: []gelati.Tool{secret},
		Approve: func(_ context.Context, req gelati.ToolRequest) (gelati.Decision, error) {
			approvals = append(approvals, req)
			return gelati.Deny("not now"), nil
		},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = a.Close() }()

	res, err := a.Run(ctx, gelati.Text("Call the secret_value tool with key \"k7\" and reply with exactly the value it returns."))
	if err != nil {
		t.Fatalf("tool turn: %v", err)
	}
	if len(toolCalls) != 1 || toolCalls[0] != "k7" || !strings.Contains(res.Text, "pineapple-k7") {
		t.Fatalf("tool calls %v, answer %q", toolCalls, res.Text)
	}

	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	draw.Draw(img, img.Bounds(), &image.Uniform{color.RGBA{R: 255, A: 255}}, image.Point{}, draw.Src)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	res, err = a.Run(ctx, gelati.Text("What single color fills this image? Answer with one lowercase word."), gelati.Image(buf.Bytes(), ""))
	if err != nil {
		t.Fatalf("image turn: %v", err)
	}
	if !strings.Contains(strings.ToLower(res.Text), "red") {
		t.Fatalf("image answer %q", res.Text)
	}

	res, err = a.Run(ctx, gelati.Text("Run the shell command `touch approved.txt` in the working directory, then say done."))
	if err != nil {
		t.Fatalf("approval turn: %v", err)
	}
	if len(approvals) == 0 || approvals[0].Name != "command" {
		t.Fatalf("approval requests %+v; answer %q", approvals, res.Text)
	}
	t.Logf("approval requests: %+v", approvals)
}
