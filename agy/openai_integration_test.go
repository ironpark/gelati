package agy_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ironpark/gelati/agy"
	"github.com/ironpark/gelati/agy/policy"
)

// openAIAgent starts an agent on the real localharness binary named by
// GELATI_AGY_HARNESS, talking to the OpenAI-compatible server srv.
func openAIAgent(t *testing.T, ctx context.Context, srv *httptest.Server, tools ...*agy.Tool) (*agy.Agent, *syncBuffer) {
	t.Helper()
	bin := os.Getenv("GELATI_AGY_HARNESS")
	if bin == "" {
		t.Skip("set GELATI_AGY_HARNESS=/path/to/localharness to run")
	}
	clearGeminiEnv(t)
	stderr := &syncBuffer{}
	agent, err := agy.NewAgent(agy.Config{
		CLIPath:    bin,
		Model:      "test-model",
		OpenAI:     &agy.OpenAIEndpoint{BaseURL: srv.URL + "/v1"},
		Tools:      tools,
		Policies:   []agy.Policy{policy.AllowAll()},
		Workspaces: []string{t.TempDir()},
		SaveDir:    t.TempDir(),
		AppDataDir: t.TempDir(),
		Stderr:     stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.Start(ctx); err != nil {
		t.Fatalf("Start: %v\nstderr:\n%s", err, stderr.String())
	}
	t.Cleanup(func() { agent.Close() })
	return agent, stderr
}

func clearGeminiEnv(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "")
	os.Unsetenv("GEMINI_API_KEY")
}

type weatherArgs struct {
	Location string `json:"location"`
}

func weatherTool() *agy.Tool {
	return agy.NewTool("get_current_weather", "Get the current weather for a given location.",
		func(_ context.Context, _ *agy.ToolContext, in weatherArgs) (string, error) {
			return fmt.Sprintf("The weather in %s is 72°F and sunny.", in.Location), nil
		})
}

// TestRealHarnessOpenAI ports upstream's
// test_local_openai_agent_getting_started_e2e: the harness calls a strict
// OpenAI-compatible server, which asks for a custom tool and then answers
// with its result.
func TestRealHarnessOpenAI(t *testing.T) {
	var (
		mu       sync.Mutex
		problems []string
		params   map[string]any
	)
	sse := func(w http.ResponseWriter, chunks ...any) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range chunks {
			b, _ := json.Marshal(c)
			fmt.Fprintf(w, "data: %s\n\n", b)
		}
		io.WriteString(w, "data: [DONE]\n\n")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var body struct {
			Tools []struct {
				Function struct {
					Name       string         `json:"name"`
					Parameters map[string]any `json:"parameters"`
				} `json:"function"`
			} `json:"tools"`
			Messages []struct {
				Role string `json:"role"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		for _, tool := range body.Tools {
			p := tool.Function.Parameters
			if typ, _ := p["type"].(string); typ != "" && typ != strings.ToLower(typ) {
				problems = append(problems, fmt.Sprintf("tool %s: type %q", tool.Function.Name, typ))
			}
			if tool.Function.Name == "get_current_weather" {
				params = p
			}
		}
		mu.Unlock()
		hasToolResponse := false
		for _, m := range body.Messages {
			hasToolResponse = hasToolResponse || m.Role == "tool"
		}
		if !hasToolResponse {
			sse(w,
				map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{map[string]any{
					"index": 0, "id": "call_123", "type": "function",
					"function": map[string]any{"name": "get_current_weather", "arguments": `{"location": "Seattle"}`},
				}}}}}},
				map[string]any{"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "tool_calls"}}},
			)
			return
		}
		sse(w,
			map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "The weather in Seattle is 72°F and sunny."}}}},
			map[string]any{"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "stop"}}},
		)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	agent, stderr := openAIAgent(t, ctx, srv, weatherTool())
	resp, err := agent.Chat(ctx, agy.Text("What's the weather in Seattle?"))
	if err != nil {
		t.Fatal(err)
	}
	text, err := resp.WaitText(ctx)
	if err != nil {
		t.Fatalf("turn: %v\nstderr:\n%s", err, stderr.String())
	}
	if !strings.Contains(text, "Seattle") {
		t.Fatalf("answer %q", text)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(problems) > 0 {
		t.Fatalf("schema problems: %v", problems)
	}
	if params["type"] != "object" {
		t.Fatalf("tool parameters %v", params)
	}
	if props, _ := params["properties"].(map[string]any); props == nil || props["location"].(map[string]any)["type"] != "string" {
		t.Fatalf("tool parameters %v", params)
	}
}

// TestRealHarnessOpenAIBadRequest ports upstream's
// test_local_openai_agent_handles_http_400_error: a server rejecting every
// request ends the turn with an error instead of hanging.
func TestRealHarnessOpenAIBadRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"message":"Invalid request schema: Bad Request","type":"invalid_request_error","code":400}}`)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	agent, _ := openAIAgent(t, ctx, srv, weatherTool())
	resp, err := agent.Chat(ctx, agy.Text("What's the weather in Seattle?"))
	if err == nil {
		_, err = resp.WaitText(ctx)
	}
	var execErr *agy.ExecutionError
	var connErr *agy.ConnectionError
	if !errors.As(err, &execErr) && !errors.As(err, &connErr) {
		t.Fatalf("turn error = %v (%T), want an execution or connection error", err, err)
	}
	t.Logf("turn ended with %T: %.160s", err, err.Error())
}
