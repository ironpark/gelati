package claude

import (
	"context"
	"encoding/base64"
	"encoding/json/jsontext"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ironpark/gelati"
)

// openTestAgent opens an Agent through Provider on a fake transport that
// answers the initialize handshake.
func openTestAgent(t *testing.T, base Options, cfg gelati.Config) (*gelati.Agent, *fakeTransport) {
	t.Helper()
	ft := newFakeTransport()
	initResponder(ft, map[string]any{"commands": []any{}})
	base.Transport = ft
	a, err := gelati.Open(t.Context(), Provider(base), cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a, ft
}

// collectEvents reads a turn's events, failing the test on an error.
func collectEvents(t *testing.T, turn *gelati.Turn) []gelati.Event {
	t.Helper()
	var evs []gelati.Event
	for ev, err := range turn.Events(t.Context()) {
		if err != nil {
			t.Fatalf("events: %v", err)
		}
		if ev.Raw == nil {
			t.Fatalf("event %+v has no Raw message", ev)
		}
		evs = append(evs, ev)
	}
	return evs
}

// eventSummary renders the events other than EventOther as kind:text lines.
func eventSummary(evs []gelati.Event) string {
	var out []string
	for _, ev := range evs {
		switch ev.Kind {
		case gelati.EventOther:
		case gelati.EventToolCall, gelati.EventToolResult:
			out = append(out, string(ev.Kind)+":"+ev.Tool.ID)
		default:
			out = append(out, string(ev.Kind)+":"+ev.Text)
		}
	}
	return strings.Join(out, "|")
}

func thinkingDeltaFrame(text, parent string) map[string]any {
	return streamFrame(map[string]any{"type": "content_block_delta", "index": 0,
		"delta": map[string]any{"type": "thinking_delta", "thinking": text}}, parent)
}

func toolResultFrame(parent string, blocks ...any) map[string]any {
	frame := map[string]any{"type": "user", "session_id": "s1",
		"message": map[string]any{"role": "user", "content": blocks}}
	if parent != "" {
		frame["parent_tool_use_id"] = parent
	}
	return frame
}

func TestProviderOptions(t *testing.T) {
	t.Parallel()
	base := Options{
		Model:        "base-model",
		Cwd:          "/base",
		Env:          map[string]string{"A": "base", "B": "base"},
		SystemPrompt: &SystemPromptPreset{Append: "Be brief."},
		CLIPath:      "/bin/claude",
	}
	schema := map[string]any{"type": "object"}
	opts, err := providerOptions(base, gelati.Config{
		Model:        "m",
		Dir:          "/repo",
		Instructions: "Use Go.",
		OutputSchema: schema,
		Env:          map[string]string{"B": "cfg", "C": "cfg"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if opts.Model != "m" || opts.Cwd != "/repo" || opts.CLIPath != "/bin/claude" || !opts.IncludePartialMessages {
		t.Fatalf("opts = %+v", opts)
	}
	if sp, ok := opts.SystemPrompt.(*SystemPromptPreset); !ok || sp.Append != "Be brief.\n\nUse Go." {
		t.Fatalf("system prompt = %#v", opts.SystemPrompt)
	}
	if want := map[string]any{"type": "json_schema", "schema": schema}; !reflect.DeepEqual(opts.OutputFormat, want) {
		t.Fatalf("output format = %#v", opts.OutputFormat)
	}
	if want := map[string]string{"A": "base", "B": "cfg", "C": "cfg"}; !reflect.DeepEqual(opts.Env, want) {
		t.Fatalf("env = %#v", opts.Env)
	}
	// base is left alone.
	if base.Env["B"] != "base" || len(base.Env) != 2 || base.SystemPrompt.(*SystemPromptPreset).Append != "Be brief." {
		t.Fatalf("base mutated: %+v", base)
	}

	// The zero Config keeps base, apart from partial messages.
	opts, err = providerOptions(base, gelati.Config{})
	if err != nil || opts.Model != "base-model" || opts.Cwd != "/base" || opts.SystemPrompt != base.SystemPrompt ||
		opts.OutputFormat != nil || !reflect.DeepEqual(opts.Env, base.Env) {
		t.Fatalf("zero config: %+v, %v", opts, err)
	}
	// Without a system prompt, the session runs with Claude Code's default.
	opts, _ = providerOptions(Options{}, gelati.Config{})
	if !reflect.DeepEqual(opts.SystemPrompt, &SystemPromptPreset{}) {
		t.Fatalf("default system prompt = %#v", opts.SystemPrompt)
	}
}

func TestProviderInstructions(t *testing.T) {
	t.Parallel()
	blocks := SystemPromptBlocks{"one"}
	cases := []struct {
		base SystemPrompt
		want SystemPrompt
	}{
		{nil, &SystemPromptPreset{Append: "extra"}},
		{&SystemPromptPreset{ExcludeDynamicSections: true}, &SystemPromptPreset{ExcludeDynamicSections: true, Append: "extra"}},
		{SystemPromptText("You review code."), SystemPromptText("You review code.\n\nextra")},
		{blocks, SystemPromptBlocks{"one", "extra"}},
		{&SystemPromptCustom{Prompt: []string{"a"}}, &SystemPromptCustom{Prompt: []string{"a", "extra"}}},
	}
	for _, c := range cases {
		opts, err := providerOptions(Options{SystemPrompt: c.base}, gelati.Config{Instructions: "extra"})
		if err != nil || !reflect.DeepEqual(opts.SystemPrompt, c.want) {
			t.Errorf("%#v: got %#v, %v", c.base, opts.SystemPrompt, err)
		}
	}
	if len(blocks) != 1 {
		t.Fatalf("base blocks mutated: %q", blocks)
	}
	_, err := providerOptions(Options{SystemPrompt: &SystemPromptFile{Path: "p"}}, gelati.Config{Instructions: "extra"})
	if !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("file prompt: %v", err)
	}
}

func TestProviderOpenSendsConfig(t *testing.T) {
	t.Parallel()
	a, ft := openTestAgent(t, Options{}, gelati.Config{
		Instructions: "Use Go.",
		OutputSchema: map[string]any{"type": "object"},
	})
	if a.Provider() != "claude" {
		t.Fatalf("provider = %q", a.Provider())
	}
	if _, ok := a.Native().(*Client); !ok {
		t.Fatalf("native = %T", a.Native())
	}
	req := ft.frames(t)[0]["request"].(map[string]any)
	if req["appendSystemPrompt"] != "Use Go." || req["systemPrompt"] != nil {
		t.Fatalf("initialize = %#v", req)
	}
	if schema, _ := req["jsonSchema"].(map[string]any); schema["type"] != "object" {
		t.Fatalf("initialize = %#v", req)
	}
}

func TestProviderStreamedEvents(t *testing.T) {
	t.Parallel()
	a, ft := openTestAgent(t, Options{}, gelati.Config{})
	turn, err := a.Send(t.Context(), gelati.Text("hi"))
	if err != nil {
		t.Fatal(err)
	}
	ft.push(messageStartFrame("m1", ""))
	ft.push(thinkingDeltaFrame("hm", ""))
	ft.push(deltaFrame("Hel", ""))
	ft.push(deltaFrame("lo", ""))
	// The complete message repeats the deltas; only its tool call is new.
	ft.push(assistantBlocksFrame("m1", "",
		map[string]any{"type": "thinking", "thinking": "hm", "signature": "sig"},
		textBlock("Hello"),
		map[string]any{"type": "tool_use", "id": "tu1", "name": "Bash", "input": map[string]any{"command": "ls"}}))
	ft.push(toolResultFrame("", map[string]any{"type": "tool_result", "tool_use_id": "tu1",
		"content": []any{textBlock("a"), textBlock("b")}, "is_error": true}))
	// Subagent output is EventOther only.
	ft.push(messageStartFrame("m2", "tu1"))
	ft.push(deltaFrame("sub", "tu1"))
	ft.push(assistantBlocksFrame("m2", "tu1", textBlock("sub"),
		map[string]any{"type": "tool_use", "id": "tu2", "name": "Read", "input": map[string]any{}}))
	ft.push(toolResultFrame("tu1", map[string]any{"type": "tool_result", "tool_use_id": "tu2", "content": "x"}))
	// A message that was never streamed keeps its text and thinking.
	ft.push(assistantBlocksFrame("m3", "",
		map[string]any{"type": "thinking", "thinking": "more", "signature": "sig"}, textBlock("!")))
	ft.push(resultFrame())

	evs := collectEvents(t, turn)
	want := "thoughtDelta:hm|textDelta:Hel|textDelta:lo|toolCall:tu1|toolResult:tu1|thoughtDelta:more|textDelta:!"
	if got := eventSummary(evs); got != want {
		t.Fatalf("events = %s", got)
	}
	// Every message is an event, the result last.
	if len(evs) != 13 {
		t.Fatalf("got %d events", len(evs))
	}
	if _, ok := evs[len(evs)-1].Raw.(*ResultMessage); !ok || evs[len(evs)-1].Kind != gelati.EventOther {
		t.Fatalf("last event = %+v", evs[len(evs)-1])
	}
	for _, ev := range evs {
		switch ev.Kind {
		case gelati.EventToolCall:
			if ev.Tool.Name != "Bash" || ev.Tool.Input["command"] != "ls" {
				t.Fatalf("tool call = %+v", ev.Tool)
			}
			if _, ok := ev.Raw.(*AssistantMessage); !ok {
				t.Fatalf("raw = %T", ev.Raw)
			}
		case gelati.EventToolResult:
			if ev.Tool.Name != "Bash" || ev.Tool.Output != "a\nb" || !ev.Tool.IsError {
				t.Fatalf("tool result = %+v", ev.Tool)
			}
			if _, ok := ev.Raw.(*UserMessage); !ok {
				t.Fatalf("raw = %T", ev.Raw)
			}
		}
	}
}

func TestProviderEventsResumeMidMessage(t *testing.T) {
	t.Parallel()
	a, ft := openTestAgent(t, Options{}, gelati.Config{})
	turn, err := a.Send(t.Context(), gelati.Text("hi"))
	if err != nil {
		t.Fatal(err)
	}
	ft.push(assistantBlocksFrame("m1", "", textBlock("a"), textBlock("b")))
	ft.push(resultFrame())
	for ev, err := range turn.Events(t.Context()) {
		if err != nil || ev.Text != "a" {
			t.Fatalf("first = %+v, %v", ev, err)
		}
		break
	}
	// The rest of the message is not lost.
	var texts []string
	for text, err := range turn.Text(t.Context()) {
		if err != nil {
			t.Fatal(err)
		}
		texts = append(texts, text)
	}
	if strings.Join(texts, "|") != "b" {
		t.Fatalf("texts = %q", texts)
	}
}

func TestProviderResult(t *testing.T) {
	t.Parallel()
	a, ft := openTestAgent(t, Options{}, gelati.Config{OutputSchema: map[string]any{"type": "object"}})
	result := resultFrame()
	result["result"] = "done"
	result["total_cost_usd"] = 0.25
	result["structured_output"] = map[string]any{"ok": true}
	result["usage"] = map[string]any{"input_tokens": 10, "cache_read_input_tokens": 4,
		"cache_creation_input_tokens": 2, "output_tokens": 7}
	ft.push(result)
	res, err := a.Run(t.Context(), gelati.Text("go"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "done" || res.CostUSD == nil || *res.CostUSD != 0.25 {
		t.Fatalf("result = %+v", res)
	}
	if res.Usage != (gelati.Usage{InputTokens: 16, CachedInputTokens: 4, OutputTokens: 7}) {
		t.Fatalf("usage = %+v", res.Usage)
	}
	var out struct {
		OK bool `json:"ok"`
	}
	if err := res.DecodeStructuredOutput(&out); err != nil || !out.OK {
		t.Fatalf("structured output = %s, %v", res.StructuredOutput, err)
	}
	if raw, ok := res.Raw.(*ResultMessage); !ok || raw.Result != "done" {
		t.Fatalf("raw = %#v", res.Raw)
	}

	// An error result comes with its error.
	failed := resultFrame()
	failed["subtype"] = "error_during_execution"
	failed["is_error"] = true
	failed["result"] = "boom"
	ft.push(failed)
	res, err = a.Run(t.Context(), gelati.Text("fail"))
	var resErr *ResultError
	if !errors.As(err, &resErr) || res == nil || res.Text != "boom" {
		t.Fatalf("failed run = %+v, %v", res, err)
	}
}

func TestProviderSessionID(t *testing.T) {
	t.Parallel()
	const pinned = "123e4567-e89b-12d3-a456-426614174000"
	a, ft := openTestAgent(t, Options{SessionID: pinned}, gelati.Config{})
	if a.ID() != pinned {
		t.Fatalf("id = %q", a.ID())
	}
	turn, err := a.Send(t.Context(), gelati.Text("hi"))
	if err != nil {
		t.Fatal(err)
	}
	// The id is the latest one the CLI sent, so the result goes out only
	// once the init message has been read.
	ft.push(map[string]any{"type": "system", "subtype": "init", "session_id": "s-init", "uuid": "u"})
	for ev, err := range turn.Events(t.Context()) {
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := ev.Raw.(*InitMessage); ok {
			if a.ID() != "s-init" {
				t.Fatalf("id after init = %q", a.ID())
			}
			ft.push(resultFrame())
		}
	}
	if a.ID() != "s1" {
		t.Fatalf("id after result = %q", a.ID())
	}

	// Result alone tracks the id too.
	next := resultFrame()
	next["session_id"] = "s2"
	ft.push(next)
	if _, err := a.Run(t.Context(), gelati.Text("again")); err != nil || a.ID() != "s2" {
		t.Fatalf("id = %q, %v", a.ID(), err)
	}

	resumed, _ := openTestAgent(t, Options{Resume: "r1"}, gelati.Config{})
	forked, _ := openTestAgent(t, Options{Resume: "r1", ForkSession: true}, gelati.Config{})
	if resumed.ID() != "r1" || forked.ID() != "" {
		t.Fatalf("resumed = %q, forked = %q", resumed.ID(), forked.ID())
	}
}

func TestProviderInputs(t *testing.T) {
	t.Parallel()
	a, ft := openTestAgent(t, Options{}, gelati.Config{})
	if _, err := a.Send(t.Context(), gelati.Text("one")); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Send(t.Context(), gelati.Text("a"), gelati.Text("b")); err != nil {
		t.Fatal(err)
	}
	frames := ft.frames(t)
	single := frames[1]["message"].(map[string]any)
	if single["content"] != "one" {
		t.Fatalf("single = %#v", single)
	}
	multi := frames[2]["message"].(map[string]any)["content"].([]any)
	if len(multi) != 2 || multi[0].(map[string]any)["text"] != "a" || multi[1].(map[string]any)["text"] != "b" {
		t.Fatalf("multi = %#v", multi)
	}

	if _, err := a.Send(t.Context(), gelati.Text("a"), otherInput{}); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("unsupported input: %v", err)
	}
	if len(ft.frames(t)) != 3 {
		t.Fatal("an unsupported input must send nothing")
	}
}

func TestProviderImageInput(t *testing.T) {
	t.Parallel()
	a, ft := openTestAgent(t, Options{}, gelati.Config{})
	png := []byte("\x89PNG\r\n\x1a\n")
	if _, err := a.Send(t.Context(), gelati.Text("what is this?"), gelati.Image(png, "")); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Send(t.Context(), gelati.Image(png, "image/webp")); err != nil {
		t.Fatal(err)
	}
	frames := ft.frames(t)
	image := func(mime string) map[string]any {
		return map[string]any{"type": "image", "source": map[string]any{
			"type": "base64", "media_type": mime, "data": base64.StdEncoding.EncodeToString(png)}}
	}
	want := []any{map[string]any{"type": "text", "text": "what is this?"}, image("image/png")}
	if got := frames[1]["message"].(map[string]any)["content"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("text and image = %#v", got)
	}
	// A lone image is still sent as blocks.
	if got := frames[2]["message"].(map[string]any)["content"]; !reflect.DeepEqual(got, []any{image("image/webp")}) {
		t.Fatalf("image = %#v", got)
	}
}

// otherInput is an Input the provider does not know, standing in for a
// future kind.
type otherInput struct{ gelati.Input }

// answeringCLI is a fake transport that answers control requests with
// success and every user message with a streamed "4".
func answeringCLI() *fakeTransport {
	ft := newFakeTransport()
	ft.onWrite = func(frame map[string]any) {
		switch frame["type"] {
		case "control_request":
			ft.push(map[string]any{"type": "control_response", "response": map[string]any{
				"subtype": "success", "request_id": frame["request_id"], "response": map[string]any{}}})
		case "user":
			ft.push(messageStartFrame("m1", ""))
			ft.push(deltaFrame("4", ""))
			ft.push(assistantBlocksFrame("m1", "", textBlock("4")))
			r := resultFrame()
			r["result"] = "4"
			ft.push(r)
		}
	}
	return ft
}

func TestProviderRunEndToEnd(t *testing.T) {
	t.Parallel()
	res, err := gelati.Run(t.Context(), Provider(Options{Transport: answeringCLI()}), "What is 2+2?",
		gelati.Config{Model: "sonnet"})
	if err != nil || res.Text != "4" {
		t.Fatalf("run = %+v, %v", res, err)
	}

	// Streaming through Turn.Text yields the deltas once.
	a, err := gelati.Open(t.Context(), Provider(Options{Transport: answeringCLI()}), gelati.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	turn, err := a.Send(t.Context(), gelati.Text("What is 2+2?"))
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for text, err := range turn.Text(t.Context()) {
		if err != nil {
			t.Fatal(err)
		}
		texts = append(texts, text)
	}
	if strings.Join(texts, "|") != "4" {
		t.Fatalf("texts = %q", texts)
	}
	if res, err := a.Run(t.Context(), gelati.Text("again")); err != nil || res.Text != "4" {
		t.Fatalf("second run = %+v, %v", res, err)
	}
}

// echoTool is a gelati.Tool that returns its arguments, or fails when they
// are {"fail":true}.
func echoTool(name string) gelati.Tool {
	return gelati.Tool{
		Name:        name,
		Description: "Echo the arguments",
		InputSchema: map[string]any{"type": "object"},
		Run: func(_ context.Context, args jsontext.Value) (string, error) {
			if string(args) == `{"fail":true}` {
				return "", errors.New("echo failed: 100%")
			}
			return string(args) + " %s", nil
		},
	}
}

func TestProviderApprove(t *testing.T) {
	t.Parallel()
	var (
		got      gelati.ToolRequest
		decision gelati.Decision
		fail     error
	)
	approve := func(_ context.Context, req gelati.ToolRequest) (gelati.Decision, error) {
		got = req
		return decision, fail
	}
	base := Options{PermissionPromptToolName: "mcp__x__prompt"}
	opts, err := providerOptions(base, gelati.Config{Approve: approve, Tools: []gelati.Tool{echoTool("echo")}})
	if err != nil {
		t.Fatal(err)
	}
	if opts.CanUseTool == nil || opts.PermissionPromptToolName != "" || base.PermissionPromptToolName != "mcp__x__prompt" {
		t.Fatalf("opts = %+v", opts)
	}

	permCtx := ToolPermissionContext{ToolUseID: "tu1", DecisionReason: "not allowed in default mode"}
	input := map[string]any{"text": "hi"}
	decision = gelati.Allow()
	res, err := opts.CanUseTool(t.Context(), "mcp__gelati__echo", input, permCtx)
	if err != nil || !reflect.DeepEqual(res, &PermissionResultAllow{}) {
		t.Fatalf("allow = %#v, %v", res, err)
	}
	want := gelati.ToolRequest{ID: "tu1", Name: "echo", Input: input, Reason: "not allowed in default mode", Raw: permCtx}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("request = %#v", got)
	}

	cases := []struct {
		tool     string
		decision gelati.Decision
		err      error
		name     string
		message  string
	}{
		{"Bash", gelati.Deny("too risky"), nil, "Bash", "too risky"},
		{"mcp__other__echo", gelati.Deny(""), nil, "mcp__other__echo", deniedMessage},
		{"mcp__gelati__unknown", gelati.Allow(), errors.New("approver down"), "mcp__gelati__unknown", "approver down"},
	}
	for _, c := range cases {
		decision, fail = c.decision, c.err
		res, err := opts.CanUseTool(t.Context(), c.tool, nil, permCtx)
		if err != nil || !reflect.DeepEqual(res, &PermissionResultDeny{Message: c.message}) {
			t.Errorf("%s: %#v, %v", c.tool, res, err)
		}
		if got.Name != c.name {
			t.Errorf("%s: request name = %q", c.tool, got.Name)
		}
	}

	// Without Approve the base's answer stays.
	opts, _ = providerOptions(base, gelati.Config{})
	if opts.CanUseTool != nil || opts.PermissionPromptToolName != "mcp__x__prompt" {
		t.Fatalf("no approve: %+v", opts)
	}
}

func TestProviderTools(t *testing.T) {
	t.Parallel()
	other := NewSDKMCPServer("other", "")
	base := Options{
		MCPServers:   map[string]MCPServerConfig{"other": other},
		AllowedTools: make([]string, 1, 4),
	}
	base.AllowedTools[0] = "Read"
	opts, err := providerOptions(base, gelati.Config{Tools: []gelati.Tool{echoTool("echo"), echoTool("say")}})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Read", "mcp__gelati__echo", "mcp__gelati__say"}; !reflect.DeepEqual(opts.AllowedTools, want) {
		t.Fatalf("allowed tools = %q", opts.AllowedTools)
	}
	if len(base.MCPServers) != 1 || !reflect.DeepEqual(base.AllowedTools, []string{"Read"}) ||
		slices.Contains(base.AllowedTools[:cap(base.AllowedTools)], "mcp__gelati__echo") {
		t.Fatalf("base mutated: %+v", base)
	}
	if len(opts.MCPServers) != 2 || opts.MCPServers["other"] != other {
		t.Fatalf("servers = %#v", opts.MCPServers)
	}
	server, ok := opts.MCPServers["gelati"].(*MCPSDKServerConfig)
	if !ok || server.Name != "gelati" {
		t.Fatalf("gelati server = %#v", opts.MCPServers["gelati"])
	}
	echo := server.Instance.(*MCPServer).byName["echo"]
	if echo.Name != "echo" || echo.Description != "Echo the arguments" || echo.InputSchema["type"] != "object" {
		t.Fatalf("tool = %+v", echo)
	}
	// The text is not a format string.
	res, err := echo.Handler(t.Context(), jsontext.Value(`{"a":1}`))
	if err != nil || !reflect.DeepEqual(res, ToolResult{Content: []ToolContent{ToolText{Text: `{"a":1} %s`}}}) {
		t.Fatalf("success = %#v, %v", res, err)
	}
	res, err = echo.Handler(t.Context(), jsontext.Value(`{"fail":true}`))
	if err != nil || !reflect.DeepEqual(res, ToolResult{Content: []ToolContent{ToolText{Text: "echo failed: 100%"}}, IsError: true}) {
		t.Fatalf("failure = %#v, %v", res, err)
	}

	// The server name must be free.
	taken := Options{MCPServers: map[string]MCPServerConfig{"gelati": other}}
	if _, err := providerOptions(taken, gelati.Config{Tools: []gelati.Tool{echoTool("echo")}}); err == nil ||
		!strings.Contains(err.Error(), `"gelati"`) {
		t.Fatalf("taken name: %v", err)
	}
	if _, err := providerOptions(taken, gelati.Config{}); err != nil {
		t.Fatalf("taken name without tools: %v", err)
	}
}

func TestProviderToolsSession(t *testing.T) {
	t.Parallel()
	var got gelati.ToolRequest
	a, ft := openTestAgent(t, Options{}, gelati.Config{
		Tools: []gelati.Tool{echoTool("echo")},
		Approve: func(_ context.Context, req gelati.ToolRequest) (gelati.Decision, error) {
			got = req
			return gelati.Deny(""), nil
		},
	})
	req := ft.frames(t)[0]["request"].(map[string]any)
	if !reflect.DeepEqual(req["sdkMcpServers"], []any{"gelati"}) {
		t.Fatalf("initialize = %#v", req)
	}

	// Permission requests reach Approve.
	ft.push(map[string]any{"type": "control_request", "request_id": "r1", "request": map[string]any{
		"subtype": "can_use_tool", "tool_name": "mcp__gelati__echo", "input": map[string]any{"a": 1.0},
		"tool_use_id": "tu0"}})
	resp := ft.nextResponse(t)
	if reply, _ := resp["response"].(map[string]any); reply["behavior"] != "deny" || reply["message"] != deniedMessage {
		t.Fatalf("reply = %#v", resp)
	}
	if got.ID != "tu0" || got.Name != "echo" || got.Input["a"] != 1.0 {
		t.Fatalf("request = %#v", got)
	}

	// Events name the gelati tools without their prefix, and only them.
	turn, err := a.Send(t.Context(), gelati.Text("hi"))
	if err != nil {
		t.Fatal(err)
	}
	ft.push(assistantBlocksFrame("m1", "",
		map[string]any{"type": "tool_use", "id": "tu1", "name": "mcp__gelati__echo", "input": map[string]any{}},
		map[string]any{"type": "tool_use", "id": "tu2", "name": "mcp__other__x", "input": map[string]any{}},
		map[string]any{"type": "tool_use", "id": "tu3", "name": "mcp__gelati__nope", "input": map[string]any{}}))
	ft.push(toolResultFrame("",
		map[string]any{"type": "tool_result", "tool_use_id": "tu1", "content": "ok"},
		map[string]any{"type": "tool_result", "tool_use_id": "tu2", "content": "ok"},
		map[string]any{"type": "tool_result", "tool_use_id": "tu3", "content": "ok"}))
	ft.push(resultFrame())
	var names []string
	for _, ev := range collectEvents(t, turn) {
		if ev.Tool != nil {
			names = append(names, string(ev.Kind)+":"+ev.Tool.Name)
		}
	}
	want := "toolCall:echo|toolCall:mcp__other__x|toolCall:mcp__gelati__nope|" +
		"toolResult:echo|toolResult:mcp__other__x|toolResult:mcp__gelati__nope"
	if got := strings.Join(names, "|"); got != want {
		t.Fatalf("names = %s", got)
	}
}
