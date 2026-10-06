package codex

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/ironpark/gelati"
)

// fakeProvider returns a provider whose clients dial server. got receives
// the options each client was created with and client the last client.
func fakeProvider(t *testing.T, server *fakeServer, base Options, thread StartThreadParams) (p *provider, got *Options, client **Client) {
	t.Helper()
	got, client = new(Options), new(*Client)
	p = Provider(base, thread).(*provider)
	p.newClient = func(ctx context.Context, opts Options) (*Client, error) {
		*got = opts
		c, err := dial(ctx, opts, server.toClientR, server.clientW, func() error {
			server.close()
			return nil
		})
		if err == nil {
			*client = c
			t.Cleanup(func() { _ = c.Close() })
		}
		return c, err
	}
	return p, got, client
}

// serveOpen answers initialize and thread/start with thread thr_1 on a
// goroutine, passing the thread/start params to check.
func serveOpen(t *testing.T, server *fakeServer, check func(init InitializeParams, params StartThreadParams)) chan struct{} {
	t.Helper()
	return serve(t, func() {
		req := server.expect("initialize")
		var init InitializeParams
		if err := json.Unmarshal(req.Params, &init); err != nil {
			t.Errorf("initialize params: %v", err)
		}
		server.respond(req, defaultInitializeResult)
		server.expect("initialized")
		req = server.expect("thread/start")
		var params StartThreadParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			t.Errorf("thread/start params: %v", err)
		}
		if check != nil {
			check(init, params)
		}
		server.respond(req, map[string]any{"thread": map[string]any{"id": "thr_1"}})
	})
}

// openFake opens a provider session against a fake server.
func openFake(t *testing.T, base Options, thread StartThreadParams, cfg gelati.Config) (*conn, *fakeServer) {
	t.Helper()
	return openWith(t, base, thread, cfg, nil)
}

// serveTurn answers turn/start with turnID on a goroutine, passing the raw
// params to check, then emits notes scoped to thr_1/turnID and completes
// the turn.
func serveTurn(t *testing.T, server *fakeServer, turnID string, check func(params map[string]any), notes ...[2]any) chan struct{} {
	t.Helper()
	return serve(t, func() {
		req := server.expect("turn/start")
		if check != nil {
			var params map[string]any
			if err := json.Unmarshal(req.Params, &params); err != nil {
				t.Errorf("turn/start params: %v", err)
			}
			check(params)
		}
		server.respond(req, map[string]any{"turn": map[string]any{"id": turnID, "status": "inProgress"}})
		for _, note := range notes {
			params := note[1].(map[string]any)
			params["threadId"], params["turnId"] = "thr_1", turnID
			server.notify(note[0].(string), params)
		}
		server.notify(MethodTurnCompleted, map[string]any{"threadId": "thr_1",
			"turn": map[string]any{"id": turnID, "status": "completed"}})
	})
}

func note(method string, params map[string]any) [2]any { return [2]any{method, params} }

func TestProviderOpenOverlaysConfig(t *testing.T) {
	server := newFakeServer(t)
	baseEnv := map[string]string{"A": "1", "B": "1"}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	p, got, _ := fakeProvider(t, server, Options{
		CLIPath:    "/base/codex",
		Env:        baseEnv,
		ClientInfo: ClientInfo{Name: "app", Version: "1.0"},
	}, StartThreadParams{
		ThreadSettings: ThreadSettings{
			Model:                 "base-model",
			Cwd:                   "/base",
			Sandbox:               SandboxModeWorkspaceWrite,
			DeveloperInstructions: "Base rules.",
		},
		ServiceName: "svc",
	})
	if p.Name() != "codex" {
		t.Fatalf("Name = %q", p.Name())
	}

	done := serveOpen(t, server, func(init InitializeParams, params StartThreadParams) {
		if init.ClientInfo.Name != "app" {
			t.Errorf("clientInfo = %+v", init.ClientInfo)
		}
		if params.Model != "gpt-5.6-terra" || params.Cwd != "/repo" {
			t.Errorf("model, cwd = %q, %q", params.Model, params.Cwd)
		}
		if params.DeveloperInstructions != "Base rules.\n\nBe brief." {
			t.Errorf("developerInstructions = %q", params.DeveloperInstructions)
		}
		if params.Sandbox != SandboxModeWorkspaceWrite || params.ServiceName != "svc" {
			t.Errorf("base params lost: %+v", params)
		}
	})
	c, err := p.Open(context.Background(), gelati.Config{
		Model:        "gpt-5.6-terra",
		Dir:          "/repo",
		Instructions: "Be brief.",
		CLIPath:      "/usr/bin/codex",
		Env:          map[string]string{"B": "2", "C": "3"},
		Logger:       logger,
	})
	<-done
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer c.Close()

	if got.CLIPath != "/usr/bin/codex" || got.Logger != logger {
		t.Errorf("options = %+v", got)
	}
	if want := map[string]string{"A": "1", "B": "2", "C": "3"}; !reflect.DeepEqual(got.Env, want) {
		t.Errorf("env = %v, want %v", got.Env, want)
	}
	if want := map[string]string{"A": "1", "B": "1"}; !reflect.DeepEqual(baseEnv, want) {
		t.Errorf("base env changed: %v", baseEnv)
	}
	if c.ID() != "thr_1" {
		t.Errorf("ID = %q", c.ID())
	}
	thread, ok := c.Native().(*Thread)
	if !ok || thread.ID() != "thr_1" || thread.Client() == nil {
		t.Fatalf("Native = %#v", c.Native())
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-c.Done():
	case <-time.After(fakeTimeout):
		t.Fatal("Done not closed after Close")
	}
	if err := c.Err(); err != nil {
		t.Fatalf("Err after Close = %v", err)
	}
	if _, err := thread.Client().ListModels(context.Background(), ListModelsParams{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("call after Close = %v, want ErrClosed", err)
	}
}

func TestProviderInstructionsWithoutBase(t *testing.T) {
	server := newFakeServer(t)
	p, _, _ := fakeProvider(t, server, Options{}, StartThreadParams{})
	done := serveOpen(t, server, func(_ InitializeParams, params StartThreadParams) {
		if params.DeveloperInstructions != "Be brief." || params.Model != "" || params.Cwd != "" {
			t.Errorf("params = %+v", params)
		}
	})
	c, err := p.Open(context.Background(), gelati.Config{Instructions: "Be brief."})
	<-done
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_ = c.Close()
}

func TestProviderStartThreadFailureClosesClient(t *testing.T) {
	server := newFakeServer(t)
	p, _, client := fakeProvider(t, server, Options{}, StartThreadParams{})
	done := serve(t, func() {
		server.respond(server.expect("initialize"), defaultInitializeResult)
		server.expect("initialized")
		server.respondError(server.expect("thread/start"), -32600, "bad cwd")
	})
	c, err := p.Open(context.Background(), gelati.Config{Dir: "/nope"})
	<-done
	if _, ok := errors.AsType[*RPCError](err); !ok || c != nil {
		t.Fatalf("Open = %v, %v", c, err)
	}
	if *client == nil {
		t.Fatal("no client created")
	}
	select {
	case <-(*client).Done():
	case <-time.After(fakeTimeout):
		t.Fatal("client left running")
	}
}

func TestProviderStructuredOutput(t *testing.T) {
	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any{"answer": map[string]any{"type": "string"}},
	}
	c, server := openFake(t, Options{}, StartThreadParams{}, gelati.Config{OutputSchema: schema})

	for _, turnID := range []string{"turn_1", "turn_2"} {
		done := serveTurn(t, server, turnID, func(params map[string]any) {
			got, _ := params["outputSchema"].(map[string]any)
			if got["additionalProperties"] != false || !reflect.DeepEqual(got["required"], []any{"answer"}) {
				t.Errorf("%s: outputSchema = %v", turnID, params["outputSchema"])
			}
		}, note(MethodItemCompleted, completedMessage("m1", `{"answer":"42"}`, PhaseFinalAnswer)))
		tc, err := c.Send(context.Background(), []gelati.Input{gelati.Text("answer")})
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
		res, err := tc.Result(context.Background())
		<-done
		if err != nil {
			t.Fatalf("Result: %v", err)
		}
		var out struct {
			Answer string `json:"answer"`
		}
		if err := res.DecodeStructuredOutput(&out); err != nil || out.Answer != "42" {
			t.Fatalf("structured output = %s, %v", res.StructuredOutput, err)
		}
		if _, ok := res.Raw.(*TurnResult); !ok {
			t.Fatalf("Raw = %T", res.Raw)
		}
	}
	// The caller's schema is left as it was.
	if _, ok := schema["additionalProperties"]; ok {
		t.Fatal("OutputSchema was modified")
	}
}

func TestProviderNoStructuredOutputWithoutSchema(t *testing.T) {
	c, server := openFake(t, Options{}, StartThreadParams{}, gelati.Config{})
	done := serveTurn(t, server, "turn_1", func(params map[string]any) {
		if _, ok := params["outputSchema"]; ok {
			t.Errorf("outputSchema sent: %v", params["outputSchema"])
		}
	}, note(MethodItemCompleted, completedMessage("m1", `{"answer":"42"}`, "")))
	tc, err := c.Send(context.Background(), []gelati.Input{gelati.Text("hi")})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	res, err := tc.Result(context.Background())
	<-done
	if err != nil || res.Text != `{"answer":"42"}` || res.StructuredOutput != nil {
		t.Fatalf("Result = %+v, %v", res, err)
	}
}

func TestProviderEvents(t *testing.T) {
	c, server := openFake(t, Options{}, StartThreadParams{}, gelati.Config{})
	command := map[string]any{"type": "commandExecution", "id": "cmd_1", "command": "go test ./...",
		"cwd": "/repo", "status": "inProgress"}
	done := serveTurn(t, server, "turn_1", nil,
		note(MethodAgentMessageDelta, map[string]any{"itemId": "m1", "delta": "Hel"}),
		note(MethodReasoningSummaryPartAdded, map[string]any{"itemId": "r1", "summaryIndex": 1}),
		note(MethodReasoningSummaryTextDelta, map[string]any{"itemId": "r1", "delta": "hmm"}),
		note(MethodItemStarted, map[string]any{"item": command}),
		note(MethodCommandExecutionOutputDelta, map[string]any{"itemId": "cmd_1", "delta": "ok\n"}),
		note(MethodItemCompleted, map[string]any{"item": map[string]any{"type": "commandExecution",
			"id": "cmd_1", "command": "go test ./...", "status": "failed", "aggregatedOutput": "FAIL\n", "exitCode": 1}}),
		note(MethodAgentMessageDelta, map[string]any{"itemId": "m1", "delta": "lo"}),
	)
	tc, err := c.Send(context.Background(), []gelati.Input{gelati.Text("run")})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	var events []gelati.Event
	for ev, err := range tc.Events(context.Background()) {
		if err != nil {
			t.Fatalf("Events: %v", err)
		}
		if _, ok := ev.Raw.(Event); !ok {
			t.Fatalf("Raw = %T", ev.Raw)
		}
		events = append(events, ev)
	}
	<-done

	var kinds []gelati.EventKind
	for _, ev := range events {
		kinds = append(kinds, ev.Kind)
	}
	want := []gelati.EventKind{
		gelati.EventTextDelta, gelati.EventOther, gelati.EventThoughtDelta, gelati.EventToolCall,
		gelati.EventOther, gelati.EventToolResult, gelati.EventTextDelta, gelati.EventOther,
	}
	if !reflect.DeepEqual(kinds, want) {
		t.Fatalf("kinds = %v, want %v", kinds, want)
	}
	if events[0].Text != "Hel" || events[2].Text != "hmm" || events[6].Text != "lo" {
		t.Errorf("texts = %q %q %q", events[0].Text, events[2].Text, events[6].Text)
	}
	call := events[3].Tool
	if call.ID != "cmd_1" || call.Name != "command" ||
		!reflect.DeepEqual(call.Input, map[string]any{"command": "go test ./...", "cwd": "/repo"}) {
		t.Errorf("tool call = %+v", call)
	}
	result := events[5].Tool
	if result.ID != "cmd_1" || result.Name != "command" || result.Output != "FAIL\n" || !result.IsError {
		t.Errorf("tool result = %+v", result)
	}
	if _, err := tc.Result(context.Background()); err != nil {
		t.Fatalf("Result: %v", err)
	}
}

func TestProviderToolItems(t *testing.T) {
	tests := []struct {
		name     string
		item     string
		call     gelati.ToolCall // started
		result   gelati.ToolCall // completed
		notATool bool
	}{
		{
			name: "fileChange",
			item: `{"type":"fileChange","id":"f1","status":"declined","changes":[{"path":"a.go","kind":{"type":"update","move_path":"b.go"},"diff":"@@"}]}`,
			call: gelati.ToolCall{ID: "f1", Name: "fileChange", Input: map[string]any{"changes": []any{
				map[string]any{"path": "a.go", "kind": "update", "diff": "@@", "movePath": "b.go"}}}},
			result: gelati.ToolCall{ID: "f1", Name: "fileChange", IsError: true},
		},
		{
			name:   "mcp",
			item:   `{"type":"mcpToolCall","id":"m1","server":"docs","tool":"search","status":"completed","arguments":{"q":"go"},"result":{"content":[{"type":"text","text":"one"},{"type":"text","text":"two"}]}}`,
			call:   gelati.ToolCall{ID: "m1", Name: "docs/search", Input: map[string]any{"q": "go"}},
			result: gelati.ToolCall{ID: "m1", Name: "docs/search", Output: "one\ntwo"},
		},
		{
			name:   "mcpError",
			item:   `{"type":"mcpToolCall","id":"m2","server":"docs","tool":"search","status":"failed","arguments":"x","error":{"message":"boom"}}`,
			call:   gelati.ToolCall{ID: "m2", Name: "docs/search", Input: map[string]any{"arguments": "x"}},
			result: gelati.ToolCall{ID: "m2", Name: "docs/search", Output: "boom", IsError: true},
		},
		{
			name:   "dynamic",
			item:   `{"type":"dynamicToolCall","id":"d1","tool":"lookup","status":"completed","arguments":{"id":7},"contentItems":[{"type":"inputText","text":"found"}],"success":false}`,
			call:   gelati.ToolCall{ID: "d1", Name: "lookup", Input: map[string]any{"id": float64(7)}},
			result: gelati.ToolCall{ID: "d1", Name: "lookup", Output: "found", IsError: true},
		},
		{
			name: "webSearch",
			item: `{"type":"webSearch","id":"w1","query":"golang","action":{"type":"search","query":"golang"}}`,
			call: gelati.ToolCall{ID: "w1", Name: "webSearch", Input: map[string]any{"query": "golang",
				"action": map[string]any{"type": "search", "query": "golang"}}},
			result: gelati.ToolCall{ID: "w1", Name: "webSearch"},
		},
		{
			name: "collab",
			item: `{"type":"collabAgentToolCall","id":"c1","tool":"spawn_agent","status":"completed","senderThreadId":"thr_1","receiverThreadIds":["thr_2"],"prompt":"go"}`,
			call: gelati.ToolCall{ID: "c1", Name: "spawn_agent", Input: map[string]any{"prompt": "go",
				"receiverThreadIds": []string{"thr_2"}}},
			result: gelati.ToolCall{ID: "c1", Name: "spawn_agent"},
		},
		{name: "agentMessage", item: `{"type":"agentMessage","id":"a1","text":"hi"}`, notATool: true},
		{name: "unknown", item: `{"type":"somethingNew","id":"u1"}`, notATool: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var item ThreadItem
			if err := json.Unmarshal([]byte(tt.item), &item); err != nil {
				t.Fatal(err)
			}
			started := mapEvent(Event{Kind: EventItemStarted, Item: &item})
			completed := mapEvent(Event{Kind: EventItemCompleted, Item: &item})
			if tt.notATool {
				if started.Kind != gelati.EventOther || completed.Kind != gelati.EventOther {
					t.Fatalf("kinds = %s, %s", started.Kind, completed.Kind)
				}
				return
			}
			if started.Kind != gelati.EventToolCall || !reflect.DeepEqual(*started.Tool, tt.call) {
				t.Errorf("call = %s %+v, want %+v", started.Kind, started.Tool, tt.call)
			}
			if completed.Kind != gelati.EventToolResult || !reflect.DeepEqual(*completed.Tool, tt.result) {
				t.Errorf("result = %s %+v, want %+v", completed.Kind, completed.Tool, tt.result)
			}
		})
	}
}

func TestProviderUsage(t *testing.T) {
	c, server := openFake(t, Options{}, StartThreadParams{}, gelati.Config{})
	usage := func(total, last [3]int) [2]any {
		tokens := func(u [3]int) map[string]any {
			return map[string]any{"inputTokens": u[0], "cachedInputTokens": u[1], "outputTokens": u[2]}
		}
		return note(MethodTokenUsageUpdated, map[string]any{"tokenUsage": map[string]any{
			"total": tokens(total), "last": tokens(last)}})
	}
	turns := []struct {
		id    string
		notes [][2]any
		want  gelati.Usage
	}{
		{"turn_1", [][2]any{usage([3]int{10, 4, 5}, [3]int{10, 4, 5})},
			gelati.Usage{InputTokens: 10, CachedInputTokens: 4, OutputTokens: 5}},
		// Two model requests: the turn's usage is the change in the total,
		// not the last request's.
		{"turn_2", [][2]any{usage([3]int{18, 7, 7}, [3]int{8, 3, 2}), usage([3]int{30, 10, 9}, [3]int{12, 3, 2})},
			gelati.Usage{InputTokens: 20, CachedInputTokens: 6, OutputTokens: 4}},
	}
	for _, turn := range turns {
		done := serveTurn(t, server, turn.id, nil, append(turn.notes,
			note(MethodItemCompleted, completedMessage("m", "done", "")))...)
		tc, err := c.Send(context.Background(), []gelati.Input{gelati.Text("go")})
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
		res, err := tc.Result(context.Background())
		<-done
		if err != nil {
			t.Fatalf("Result: %v", err)
		}
		if res.Usage != turn.want || res.Text != "done" {
			t.Fatalf("%s: usage = %+v, text = %q", turn.id, res.Usage, res.Text)
		}
	}
}

func TestProviderFailedTurnReturnsResult(t *testing.T) {
	c, server := openFake(t, Options{}, StartThreadParams{}, gelati.Config{})
	done := serve(t, func() {
		req := server.expect("turn/start")
		server.respond(req, map[string]any{"turn": map[string]any{"id": "turn_1", "status": "inProgress"}})
		notifyTurn(server, MethodItemCompleted, completedMessage("m", "partial", ""))
		server.notify(MethodTurnCompleted, map[string]any{"threadId": "thr_1", "turn": map[string]any{
			"id": "turn_1", "status": "failed", "error": map[string]any{"message": "quota"}}})
	})
	tc, err := c.Send(context.Background(), []gelati.Input{gelati.Text("go")})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	res, err := tc.Result(context.Background())
	<-done
	if _, ok := errors.AsType[*TurnError](err); !ok || res == nil || res.Text != "partial" {
		t.Fatalf("Result = %+v, %v", res, err)
	}
}

type otherInput struct{ gelati.TextInput }

func TestProviderUnsupportedInput(t *testing.T) {
	c, _ := openFake(t, Options{}, StartThreadParams{}, gelati.Config{})
	_, err := c.Send(context.Background(), []gelati.Input{gelati.Text("look"), otherInput{}})
	if !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("Send = %v", err)
	}
}

func TestProviderAgentRun(t *testing.T) {
	server := newFakeServer(t)
	userNotes := make(chan string, 64)
	p, _, _ := fakeProvider(t, server, Options{
		OnNotification: func(method string, _ jsontext.Value) { userNotes <- method },
	}, StartThreadParams{})
	done := serveOpen(t, server, nil)
	a, err := gelati.Open(context.Background(), p, gelati.Config{Model: "gpt-5.6-terra"})
	<-done
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer a.Close()
	if a.Provider() != "codex" || a.ID() != "thr_1" {
		t.Fatalf("agent = %q %q", a.Provider(), a.ID())
	}

	done = serveTurn(t, server, "turn_1", func(params map[string]any) {
		input, _ := params["input"].([]any)
		if len(input) != 2 {
			t.Errorf("input = %v", params["input"])
		}
	},
		note(MethodAgentMessageDelta, map[string]any{"itemId": "m", "delta": "Hi"}),
		note(MethodItemCompleted, completedMessage("m", "Hi there", PhaseFinalAnswer)),
		note(MethodTokenUsageUpdated, map[string]any{"tokenUsage": map[string]any{
			"total": map[string]any{"inputTokens": 3, "outputTokens": 2}, "last": map[string]any{}}}),
	)
	res, err := a.Run(context.Background(), gelati.Text("Say"), gelati.Text("hi"))
	<-done
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "Hi there" || res.Usage != (gelati.Usage{InputTokens: 3, OutputTokens: 2}) {
		t.Fatalf("result = %+v", res)
	}
	// The user's OnNotification still sees every notification.
	select {
	case <-userNotes:
	default:
		t.Fatal("OnNotification not called")
	}
	if err := a.Close(); err != nil || a.Err() != nil {
		t.Fatalf("Close = %v, Err = %v", err, a.Err())
	}
}

// openWith opens a provider session against a fake server, passing the
// thread/start params to check when it is not nil.
func openWith(t *testing.T, base Options, thread StartThreadParams, cfg gelati.Config, check func(StartThreadParams)) (*conn, *fakeServer) {
	t.Helper()
	server := newFakeServer(t)
	p, _, _ := fakeProvider(t, server, base, thread)
	done := serveOpen(t, server, func(_ InitializeParams, params StartThreadParams) {
		if check != nil {
			check(params)
		}
	})
	c, err := p.Open(context.Background(), cfg)
	<-done
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c.(*conn), server
}

func TestProviderTools(t *testing.T) {
	type addArgs struct {
		A int `json:"a"`
		B int `json:"b"`
	}
	add := gelati.NewTool("add", "Add two numbers", func(_ context.Context, in addArgs) (int, error) {
		return in.A + in.B, nil
	})
	fail := gelati.NewTool("fail", "Fails", func(context.Context, struct{}) (string, error) {
		return "", errors.New("boom")
	})
	own := DynamicToolSpec{Name: "own", Description: "Base tool"}
	var otherCalls []string
	server := newFakeServer(t)
	p, _, _ := fakeProvider(t, server, Options{Approvals: ApprovalFuncs{
		DynamicTool: func(_ context.Context, req *DynamicToolCallRequest) (*DynamicToolCallResponse, error) {
			otherCalls = append(otherCalls, req.Tool)
			return DynamicToolText("base", true), nil
		},
	}}, StartThreadParams{DynamicTools: []DynamicToolSpec{own}})
	done := serveOpen(t, server, func(_ InitializeParams, params StartThreadParams) {
		var names []string
		for _, s := range params.DynamicTools {
			names = append(names, s.Name)
		}
		if !reflect.DeepEqual(names, []string{"own", "add", "fail"}) {
			t.Errorf("dynamic tools = %v", names)
		}
		if got := params.DynamicTools[1].InputSchema["required"]; !reflect.DeepEqual(got, []any{"a", "b"}) {
			t.Errorf("add schema required = %v", got)
		}
	})
	c, err := p.Open(context.Background(), gelati.Config{Tools: []gelati.Tool{add, fail}})
	<-done
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	call := func(tool string, args any) string {
		t.Helper()
		reply := server.awaitReply(server.request("sr-"+tool, MethodDynamicToolCall, map[string]any{
			"threadId": "thr_1", "turnId": "turn_1", "callId": "call_1", "tool": tool, "arguments": args}))
		if reply.Error != nil {
			t.Fatalf("%s: %+v", tool, reply.Error)
		}
		return string(reply.Result)
	}
	if got := call("add", map[string]any{"a": 2, "b": 3}); got != `{"contentItems":[{"type":"inputText","text":"5"}],"success":true}` {
		t.Errorf("add = %s", got)
	}
	if got := call("fail", map[string]any{}); got != `{"contentItems":[{"type":"inputText","text":"boom"}],"success":false}` {
		t.Errorf("fail = %s", got)
	}
	if got := call("own", map[string]any{}); got != `{"contentItems":[{"type":"inputText","text":"base"}],"success":true}` || !reflect.DeepEqual(otherCalls, []string{"own"}) {
		t.Errorf("own = %s, base calls %v", got, otherCalls)
	}

	// A gelati tool may not shadow one of the thread's.
	p2, _, _ := fakeProvider(t, newFakeServer(t), Options{}, StartThreadParams{DynamicTools: []DynamicToolSpec{{Name: "add"}}})
	if _, err := p2.Open(context.Background(), gelati.Config{Tools: []gelati.Tool{add}}); err == nil {
		t.Error("Open with a duplicate tool succeeded")
	}
}

func TestProviderApprove(t *testing.T) {
	var reqs []gelati.ToolRequest
	approve := func(_ context.Context, req gelati.ToolRequest) (gelati.Decision, error) {
		reqs = append(reqs, req)
		switch req.Name {
		case "command":
			return gelati.Deny("no"), nil
		case "permissions":
			return gelati.Decision{}, errors.New("broken UI")
		}
		return gelati.Allow(), nil
	}
	c, server := openWith(t, Options{}, StartThreadParams{}, gelati.Config{Approve: approve}, nil)

	reply := server.awaitReply(server.request("sr-c", MethodCommandApproval, map[string]any{
		"threadId": "thr_1", "turnId": "turn_1", "itemId": "i1", "command": "rm -rf /", "cwd": "/repo", "reason": "risky"}))
	if string(reply.Result) != `{"decision":"decline"}` {
		t.Errorf("command reply = %s, %+v", reply.Result, reply.Error)
	}

	server.notify(MethodItemStarted, map[string]any{"threadId": "thr_1", "turnId": "turn_1", "item": map[string]any{
		"type": "fileChange", "id": "f1", "status": "inProgress",
		"changes": []any{map[string]any{"path": "a.go", "kind": map[string]any{"type": "add"}, "diff": "+x"}}}})
	reply = server.awaitReply(server.request("sr-f", MethodFileChangeApproval, map[string]any{
		"threadId": "thr_1", "turnId": "turn_1", "itemId": "f1", "grantRoot": "/repo"}))
	if string(reply.Result) != `{"decision":"accept"}` {
		t.Errorf("file change reply = %s, %+v", reply.Result, reply.Error)
	}
	server.notify(MethodItemCompleted, map[string]any{"threadId": "thr_1", "turnId": "turn_1", "item": map[string]any{
		"type": "fileChange", "id": "f1", "status": "completed", "changes": []any{}}})

	reply = server.awaitReply(server.request("sr-p", MethodPermissionsApproval, map[string]any{
		"threadId": "thr_1", "turnId": "turn_1", "itemId": "p1", "permissions": map[string]any{"network": map[string]any{"enabled": true}}}))
	if string(reply.Result) != `{"permissions":{}}` {
		t.Errorf("permissions reply = %s, %+v", reply.Result, reply.Error)
	}

	if len(reqs) != 3 {
		t.Fatalf("requests = %+v", reqs)
	}
	if r := reqs[0]; r.ID != "i1" || r.Name != "command" || r.Reason != "risky" ||
		!reflect.DeepEqual(r.Input, map[string]any{"command": "rm -rf /", "cwd": "/repo"}) {
		t.Errorf("command request = %+v", r)
	} else if raw, ok := r.Raw.(*CommandApprovalRequest); !ok || raw.ItemID != "i1" {
		t.Errorf("command raw = %#v", r.Raw)
	}
	wantChanges := map[string]any{"grantRoot": "/repo", "changes": []any{map[string]any{"path": "a.go", "kind": "add", "diff": "+x"}}}
	if r := reqs[1]; r.ID != "f1" || r.Name != "fileChange" || !reflect.DeepEqual(r.Input, wantChanges) {
		t.Errorf("file change request = %+v", r)
	}
	if r := reqs[2]; r.Name != "permissions" || !reflect.DeepEqual(r.Input, map[string]any{"network": map[string]any{"enabled": true}}) {
		t.Errorf("permissions request = %+v", r)
	}
	if got := c.client.fileChanges.get("thr_1", "turn_1", "f1"); got != nil {
		t.Errorf("file changes kept after completion: %v", got)
	}
}

func TestProviderApproveAllows(t *testing.T) {
	approve := func(context.Context, gelati.ToolRequest) (gelati.Decision, error) { return gelati.Allow(), nil }
	_, server := openWith(t, Options{}, StartThreadParams{}, gelati.Config{Approve: approve}, nil)
	reply := server.awaitReply(server.request("sr-c", MethodCommandApproval, map[string]any{
		"threadId": "thr_1", "turnId": "turn_1", "itemId": "i1", "command": "ls"}))
	if string(reply.Result) != `{"decision":"accept"}` {
		t.Errorf("command reply = %s", reply.Result)
	}
	perms := map[string]any{"fileSystem": map[string]any{"write": []any{"/tmp"}}}
	reply = server.awaitReply(server.request("sr-p", MethodPermissionsApproval, map[string]any{
		"threadId": "thr_1", "turnId": "turn_1", "itemId": "p1", "permissions": perms}))
	if string(reply.Result) != `{"permissions":{"fileSystem":{"write":["/tmp"]}}}` {
		t.Errorf("permissions reply = %s", reply.Result)
	}
}

func TestProviderApprovalsKeepBase(t *testing.T) {
	// Without Approve, base.Approvals still answers approvals when tools
	// wrap it.
	tool := gelati.Tool{Name: "t", Run: func(context.Context, jsontext.Value) (string, error) { return "", nil }}
	_, server := openFake(t, Options{Approvals: ApprovalFuncs{
		Command: func(context.Context, *CommandApprovalRequest) (Decision, error) { return DecisionAcceptForSession, nil },
		Other: func(_ context.Context, method string, _ jsontext.Value) (any, error) {
			return map[string]string{"method": method}, nil
		},
	}}, StartThreadParams{}, gelati.Config{Tools: []gelati.Tool{tool}})
	reply := server.awaitReply(server.request("sr-c", MethodCommandApproval, map[string]any{"threadId": "thr_1", "itemId": "i1"}))
	if string(reply.Result) != `{"decision":"acceptForSession"}` {
		t.Errorf("command reply = %s", reply.Result)
	}
	reply = server.awaitReply(server.request("sr-o", "attestation/generate", map[string]any{"threadId": "thr_1"}))
	if string(reply.Result) != `{"method":"attestation/generate"}` {
		t.Errorf("other reply = %s", reply.Result)
	}
}

func TestProviderImageInput(t *testing.T) {
	c, server := openWith(t, Options{}, StartThreadParams{}, gelati.Config{}, nil)
	done := serveTurn(t, server, "turn_1", func(params map[string]any) {
		want := []any{
			map[string]any{"type": "text", "text": "what is this?"},
			map[string]any{"type": "image", "url": "data:image/png;base64,iVBO"},
		}
		if !reflect.DeepEqual(params["input"], want) {
			t.Errorf("input = %v", params["input"])
		}
	})
	tc, err := c.Send(context.Background(), []gelati.Input{gelati.Text("what is this?"), gelati.Image([]byte{0x89, 'P', 'N'}, "image/png")})
	if err != nil {
		t.Fatal(err)
	}
	<-done
	if _, err := tc.Result(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestTurnUsageOfResumedThread(t *testing.T) {
	client, server := connect(t, Options{})
	done := serve(t, func() {
		server.respond(server.expect("thread/resume"), map[string]any{"thread": map[string]any{"id": "thr_1"}})
	})
	thread, err := client.ResumeThread(context.Background(), ResumeThreadParams{ThreadID: "thr_1"})
	<-done
	if err != nil {
		t.Fatal(err)
	}
	usage := func(input int) [2]any {
		return note(MethodTokenUsageUpdated, map[string]any{"tokenUsage": map[string]any{
			"total": map[string]any{"inputTokens": input}, "last": map[string]any{"inputTokens": 1}}})
	}
	// The first turn cannot tell its own usage from the history's; the
	// second measures from the first's total.
	for i, want := range []*TokenUsage{nil, {InputTokens: 30}} {
		turnID := fmt.Sprint("turn_", i)
		done := serveTurn(t, server, turnID, nil, usage(100+30*i))
		r, err := thread.Run(context.Background(), Text("go"))
		<-done
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(r.TurnUsage, want) {
			t.Fatalf("%s: TurnUsage = %+v, want %+v", turnID, r.TurnUsage, want)
		}
	}
}
