package agy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ironpark/gelati/agy/internal/wire"
)

// fakeAgentConfig returns a config that launches the fake harness, with
// files the fake records the harness config and notable events to.
func fakeAgentConfig(t *testing.T) (cfg Options, recordPath, logPath string) {
	t.Helper()
	dir := t.TempDir()
	recordPath = filepath.Join(dir, "harness_config.json")
	logPath = filepath.Join(dir, "events.log")
	return Options{
		CLIPath: os.Args[0],
		Env: map[string]string{
			fakeHarnessEnv: "1",
			// The race runtime sleeps a second on exit by default.
			"GORACE":      "atexit_sleep_ms=0",
			fakeRecordEnv: recordPath,
			fakeLogEnv:    logPath,
		},
		APIKey:     "test-key",
		Workspaces: []string{dir},
		SaveDir:    filepath.Join(dir, "save"),
		AppDataDir: filepath.Join(dir, "app"),
		Logger:     quietLogger(),
	}, recordPath, logPath
}

func startAgent(t *testing.T, cfg Options) *Agent {
	t.Helper()
	agent, err := NewAgent(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	if err := agent.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agent.Close() })
	return agent
}

func readRecord(t *testing.T, path string) *wire.HarnessConfig {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var hc wire.HarnessConfig
	if err := wire.Unmarshal(b, &hc); err != nil {
		t.Fatal(err)
	}
	return &hc
}

func readLog(path string) []string {
	b, _ := os.ReadFile(path)
	return strings.Fields(string(b))
}

func chat(t *testing.T, agent *Agent, prompt string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	resp, err := agent.Chat(ctx, Text(prompt))
	if err != nil {
		return "", err
	}
	res, err := resp.Result(ctx)
	if res == nil {
		return "", err
	}
	return res.Text(), err
}

func TestAgentChat(t *testing.T) {
	cfg, record, _ := fakeAgentConfig(t)
	cfg.SystemInstructions = TextSystemInstructions("Be brief.")
	agent := startAgent(t, cfg)
	if !agent.IsStarted() {
		t.Fatal("not started")
	}
	if s := agent.SandboxStatus(); s == nil || !s.Available {
		t.Fatalf("sandbox status %+v", s)
	}
	conv := agent.Conversation()
	if h := conv.History(); len(h) != 1 || h[0].Content != "earlier prompt" {
		t.Fatalf("restored history %+v", h)
	}

	ctx := t.Context()
	resp, err := agent.Chat(ctx, Text("hello"))
	if err != nil {
		t.Fatal(err)
	}
	var deltas []string
	for d, err := range resp.Text(ctx) {
		if err != nil {
			t.Fatal(err)
		}
		deltas = append(deltas, d)
	}
	if !reflect.DeepEqual(deltas, []string{"Hello", " there!"}) {
		t.Fatalf("deltas %q", deltas)
	}
	var thoughts []string
	for th, err := range resp.Thoughts(ctx) {
		if err != nil {
			t.Fatal(err)
		}
		thoughts = append(thoughts, th)
	}
	if !reflect.DeepEqual(thoughts, []string{"thinking"}) {
		t.Fatalf("thoughts %q", thoughts)
	}
	res, err := resp.Result(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text() != "Hello there!" || res.Thoughts() != "thinking" {
		t.Fatalf("result text %q thoughts %q", res.Text(), res.Thoughts())
	}
	if u := res.Usage; u == nil || val(u.TotalTokenCount) != 35 || val(u.PromptTokenCount) != 30 {
		t.Fatalf("turn usage %+v (cumulative started at 10)", u)
	}
	if agent.ConversationID() != fakeCascade || conv.LastResponse() != "Hello there!" || conv.TurnCount() != 1 || res.StopReason != StopReasonUnspecified {
		t.Fatalf("id %q last %q turns %d", agent.ConversationID(), conv.LastResponse(), conv.TurnCount())
	}

	hc := readRecord(t, record)
	if hc.GetSystemInstructions().GetAppended().GetAppendedSections()[0].GetContent() != "Be brief." {
		t.Fatalf("system instructions %+v", hc.GetSystemInstructions())
	}
	if len(hc.GetModels()) != 2 || hc.GetModels()[0].GetName() != DefaultModel || hc.GetModels()[0].GetGeminiApiEndpoint().GetApiKey() != "test-key" {
		t.Fatalf("models %+v", hc.GetModels())
	}
	if rules := hc.GetPolicyConfig().GetRules(); len(rules) != 2 || rules[0].GetTool() != "run_command" {
		t.Fatalf("policies %+v", rules)
	}
	if len(hc.GetWorkspaces()) != 1 || len(hc.GetEnabledHooks()) != 0 {
		t.Fatalf("workspaces %+v hooks %v", hc.GetWorkspaces(), hc.GetEnabledHooks())
	}

	if err := agent.Close(); err != nil {
		t.Fatal(err)
	}
	if agent.IsStarted() || agent.Conversation() != nil || agent.ConversationID() != "" || agent.SandboxStatus() != nil {
		t.Fatal("agent still started after Close")
	}
	if err := agent.Close(); err != nil {
		t.Fatal("second Close:", err)
	}
	if _, err := agent.Chat(ctx, Text("hello")); !errors.Is(err, ErrNotStarted) {
		t.Fatalf("Chat after Close = %v", err)
	}
}

func TestAgentCustomToolAndHooks(t *testing.T) {
	type echoArgs struct {
		Message string `json:"message"`
	}
	var convIDs []string
	var mu sync.Mutex
	echo := NewTool("echo", "Echoes a message.", func(_ context.Context, tc *ToolContext, in echoArgs) (map[string]any, error) {
		n := tc.UpdateState("calls", func(v any) any { return v.(int) + 1 }, 0)
		mu.Lock()
		convIDs = append(convIDs, tc.ConversationID())
		mu.Unlock()
		return map[string]any{"echo": in.Message, "calls": n}, nil
	})
	failing := NewTool("failing", "Always fails.", func(context.Context, *ToolContext, struct{}) (any, error) {
		return nil, errors.New("disk on fire")
	})
	var events []string
	var evMu sync.Mutex
	logEvent := func(s string) {
		evMu.Lock()
		events = append(events, s)
		evMu.Unlock()
	}
	cfg, record, _ := fakeAgentConfig(t)
	cfg.Tools = []*Tool{echo, failing}
	cfg.Hooks = []Hook{
		PreTurnHook(func(_ context.Context, hc *HookContext, prompt []Content) (HookResult, error) {
			hc.SetState("prompt", string(prompt[0].(Text)))
			logEvent("pre_turn")
			return HookResult{}, nil
		}),
		PreToolCallHook(func(_ context.Context, hc *HookContext, call *ToolCall) (HookResult, error) {
			p, _ := hc.GetState("prompt")
			logEvent("pre_tool:" + call.Name)
			if strings.Contains(p.(string), "forbidden") {
				return HookResult{Deny: true, Message: "not today"}, nil
			}
			if call.Name == "echo" {
				return HookResult{ModifiedArgs: map[string]any{"message": "sanitized"}}, nil
			}
			return HookResult{}, nil
		}),
		PostToolCallHook(func(_ context.Context, _ *HookContext, r *ToolResult) error {
			logEvent("post_tool:" + r.Name)
			return nil
		}),
		OnToolErrorHook(func(_ context.Context, _ *HookContext, err *ToolExecutionError) (string, error) {
			logEvent("tool_error:" + err.Message)
			return "please retry later", nil
		}),
		PostTurnHook(func(context.Context, *HookContext, string) error { logEvent("post_turn"); return nil }),
	}
	agent := startAgent(t, cfg)

	text, err := chat(t, agent, `tool echo {"message": "hi"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, `"echo":"sanitized"`) || !strings.Contains(text, `"calls":1`) {
		t.Fatalf("tool turn text %q", text)
	}
	text, err = chat(t, agent, `tool failing {}`)
	if err != nil {
		t.Fatal(err)
	}
	if text != "tool said: error please retry later" {
		t.Fatalf("failing tool turn %q", text)
	}
	text, err = chat(t, agent, `tool echo {"message": "forbidden"}`)
	if err != nil {
		t.Fatal(err)
	}
	if text != "denied: not today" {
		t.Fatalf("denied turn %q", text)
	}
	mu.Lock()
	if len(convIDs) != 1 || convIDs[0] != fakeCascade {
		t.Fatalf("tool context conversation ids %q", convIDs)
	}
	mu.Unlock()

	// The tool call is reported in the turn's chunks too.
	resp, err := agent.Chat(t.Context(), Text(`tool echo {"message": "again"}`))
	if err != nil {
		t.Fatal(err)
	}
	var calls []*ToolCall
	for c, err := range resp.ToolCalls(t.Context()) {
		if err != nil {
			t.Fatal(err)
		}
		calls = append(calls, c)
	}
	if len(calls) != 1 || calls[0].Name != "echo" || calls[0].ID != "call-1" || calls[0].Args["message"] != "sanitized" {
		t.Fatalf("tool calls %+v", calls)
	}

	evMu.Lock()
	want := []string{
		"pre_turn", "pre_tool:echo", "post_tool:echo", "post_turn",
		"pre_turn", "pre_tool:failing", "tool_error:disk on fire", "post_tool:failing", "post_turn",
		"pre_turn", "pre_tool:echo", "post_turn",
	}
	if !reflect.DeepEqual(events[:len(want)], want) {
		t.Fatalf("hook events\n got %q\nwant %q", events, want)
	}
	evMu.Unlock()

	hc := readRecord(t, record)
	var names []string
	for _, tl := range hc.GetTools() {
		names = append(names, tl.GetName())
	}
	if !reflect.DeepEqual(names, []string{"echo", "failing"}) || !strings.Contains(hc.GetTools()[0].GetParametersJsonSchema(), `"message"`) {
		t.Fatalf("declared tools %+v", hc.GetTools())
	}
	wantHooks := []wire.LifecycleHook{wire.LifecycleHook_LIFECYCLE_HOOK_PRE_TURN, wire.LifecycleHook_LIFECYCLE_HOOK_POST_TURN, wire.LifecycleHook_LIFECYCLE_HOOK_PRE_TOOL, wire.LifecycleHook_LIFECYCLE_HOOK_POST_TOOL, wire.LifecycleHook_LIFECYCLE_HOOK_ON_TOOL_ERROR}
	if !reflect.DeepEqual(hc.GetEnabledHooks(), wantHooks) {
		t.Fatalf("enabled hooks %v", hc.GetEnabledHooks())
	}
}

func TestAgentTurnErrors(t *testing.T) {
	cfg, _, _ := fakeAgentConfig(t)
	cfg.Hooks = []Hook{PreTurnHook(func(_ context.Context, _ *HookContext, prompt []Content) (HookResult, error) {
		if prompt[0].(Text) == "blocked" {
			return HookResult{Deny: true, Message: "Denied by hook"}, nil
		}
		return HookResult{}, nil
	})}
	agent := startAgent(t, cfg)

	_, err := chat(t, agent, "fail")
	if ce, ok := errors.AsType[*ConnectionError](err); !ok || ce.Message != "API key not valid." {
		t.Fatalf("fatal error turn: %v", err)
	}
	_, err = chat(t, agent, "blocked")
	if ee, ok := errors.AsType[*ExecutionError](err); !ok || ee.Message != "Denied by hook" {
		t.Fatalf("denied turn: %v", err)
	}
	// The session keeps working.
	if text, err := chat(t, agent, "hello"); err != nil || text != "Hello there!" {
		t.Fatalf("turn after errors %q %v", text, err)
	}
}

func TestAgentCancel(t *testing.T) {
	cfg, _, _ := fakeAgentConfig(t)
	agent := startAgent(t, cfg)
	ctx := t.Context()
	resp, err := agent.Chat(ctx, Text("slow"))
	if err != nil {
		t.Fatal(err)
	}
	for d, err := range resp.Text(ctx) {
		if err != nil {
			t.Fatal(err)
		}
		if d == "working" {
			break
		}
	}
	if err := resp.Cancel(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := resp.Result(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled turn: %v", err)
	}
	if text, err := chat(t, agent, "hello"); err != nil || text != "Hello there!" {
		t.Fatalf("turn after cancel %q %v", text, err)
	}
}

func TestAgentStructuredOutput(t *testing.T) {
	type report struct {
		Total float64 `json:"total_revenue"`
		Top   string  `json:"top_selling_product"`
	}
	cfg, record, _ := fakeAgentConfig(t)
	cfg.ResponseSchema = report{}
	agent := startAgent(t, cfg)
	resp, err := agent.Chat(t.Context(), Text(`structured {"total_revenue": 386.0, "top_selling_product": "Widget A"}`))
	if err != nil {
		t.Fatal(err)
	}
	res, err := resp.Result(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var got report
	if err := res.DecodeStructuredOutput(&got); err != nil || got.Total != 386 || got.Top != "Widget A" {
		t.Fatalf("structured output %+v %v", got, err)
	}
	if s := readRecord(t, record).GetFinishToolSchemaJson(); !strings.Contains(s, `"total_revenue"`) || !strings.Contains(s, `"number"`) {
		t.Fatalf("finish schema %s", s)
	}
}

func TestAgentDynamicPolicy(t *testing.T) {
	cfg, record, _ := fakeAgentConfig(t)
	var asked atomic.Int32
	cfg.Policies = []Policy{
		{Tool: "run_command", Decision: DecisionDeny, Name: "no-rm", When: func(_ context.Context, c ToolCall) (bool, error) {
			return strings.Contains(c.Args["CommandLine"].(string), "rm "), nil
		}},
		{Tool: "run_command", Decision: DecisionAskUser, AskUser: func(context.Context, ToolCall, string) (bool, error) {
			asked.Add(1)
			return true, nil
		}},
		AllowAllPolicy(),
	}
	agent := startAgent(t, cfg)
	text, err := chat(t, agent, "policy rule_0")
	if err != nil || text != "policy POLICY_EVALUATION_OUTCOME_DENY Denied by policy 'no-rm'." {
		t.Fatalf("rule_0 %q %v", text, err)
	}
	text, err = chat(t, agent, "policy rule_1")
	if err != nil || text != "policy POLICY_EVALUATION_OUTCOME_ALLOW " || asked.Load() != 1 {
		t.Fatalf("rule_1 %q %v asked=%d", text, err, asked.Load())
	}
	pc := readRecord(t, record).GetPolicyConfig()
	if len(pc.GetRules()) != 3 || !pc.GetRules()[0].GetIsDynamic() || pc.GetRules()[2].GetIsDynamic() ||
		pc.GetWorkspaceContainment() != wire.PolicyConfig_WORKSPACE_CONTAINMENT_DISABLED {
		t.Fatalf("policy config %+v", pc)
	}
}

func TestAgentTriggersAndSessionHooks(t *testing.T) {
	cfg, _, logPath := fakeAgentConfig(t)
	var started, ended atomic.Bool
	cfg.Hooks = []Hook{
		OnSessionStartHook(func(context.Context, *HookContext) error { started.Store(true); return nil }),
		OnSessionEndHook(func(context.Context, *HookContext) error { ended.Store(true); return nil }),
	}
	var fired atomic.Int32
	cfg.Triggers = []Trigger{Every(10*time.Millisecond, func(ctx context.Context, tc *TriggerContext) error {
		if fired.Add(1) > 2 {
			<-ctx.Done()
			return nil
		}
		return tc.Send(ctx, "tick")
	})}
	agent := startAgent(t, cfg)
	waitFor(t, "session start hook", started.Load)
	waitFor(t, "trigger notifications", func() bool {
		n := 0
		for _, l := range readLog(logPath) {
			if l == "trigger:tick" {
				n++
			}
		}
		return n == 2
	})
	if err := agent.Close(); err != nil {
		t.Fatal(err)
	}
	if !ended.Load() {
		t.Fatal("session end hook did not run")
	}
	if log := readLog(logPath); log[len(log)-1] != "session_end" {
		t.Fatalf("event log %q", log)
	}
}

func TestAgentRestart(t *testing.T) {
	cfg, _, _ := fakeAgentConfig(t)
	agent := startAgent(t, cfg)
	if err := agent.Start(t.Context()); err == nil {
		t.Fatal("second Start succeeded")
	}
	if err := agent.Close(); err != nil {
		t.Fatal(err)
	}
	if err := agent.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if text, err := chat(t, agent, "hello"); err != nil || text != "Hello there!" {
		t.Fatalf("after restart %q %v", text, err)
	}
}

func TestAgentPolicyGuard(t *testing.T) {
	guardErr := "Write tools or MCP servers are enabled without a safety policy"
	mcp := []MCPServer{&MCPStdioServer{Name: "test_server", Command: "node", Args: []string{"index.js"}}}
	decide := PreToolCallHook(func(context.Context, *HookContext, *ToolCall) (HookResult, error) { return HookResult{}, nil })
	for name, tc := range map[string]struct {
		cfg     Options
		blocked bool
	}{
		"default write tools, no policies":     {Options{Policies: []Policy{}}, true},
		"explicit write tool":                  {Options{Policies: []Policy{}, Capabilities: &CapabilitiesConfig{EnabledTools: []BuiltinTool{BuiltinRunCommand}}}, true},
		"all tools":                            {Options{Policies: []Policy{}, Capabilities: &CapabilitiesConfig{EnabledTools: AllTools()}}, true},
		"empty disabled list":                  {Options{Policies: []Policy{}, Capabilities: &CapabilitiesConfig{DisabledTools: []BuiltinTool{}}}, true},
		"mcp server, no policies":              {Options{Policies: []Policy{}, Capabilities: &CapabilitiesConfig{EnabledTools: ReadOnlyTools()}, MCPServers: mcp}, true},
		"read-only tools":                      {Options{Policies: []Policy{}, Capabilities: &CapabilitiesConfig{EnabledTools: ReadOnlyTools()}}, false},
		"deprecated read-only tools":           {Options{Policies: []Policy{}, Capabilities: &CapabilitiesConfig{EnabledTools: DeprecatedTools()}}, false},
		"all default write tools disabled":     {Options{Policies: []Policy{}, Capabilities: &CapabilitiesConfig{DisabledTools: writeTools()}}, false},
		"write tools with a policy":            {Options{Policies: []Policy{{Tool: "*", Decision: DecisionDeny}}}, false},
		"default policies":                     {Options{}, false},
		"mcp server with a policy":             {Options{Policies: []Policy{{Tool: "*", Decision: DecisionDeny}}, MCPServers: mcp}, false},
		"mcp server with a pre-tool-call hook": {Options{Policies: []Policy{}, MCPServers: mcp, Hooks: []Hook{decide}}, false},
		"auto policy":                          {Options{Policies: []Policy{{Auto: true}}}, false},
	} {
		base, _, _ := fakeAgentConfig(t)
		cfg := tc.cfg
		cfg.CLIPath, cfg.Env, cfg.APIKey, cfg.Workspaces, cfg.SaveDir, cfg.AppDataDir, cfg.Logger =
			base.CLIPath, base.Env, base.APIKey, base.Workspaces, base.SaveDir, base.AppDataDir, base.Logger
		agent, err := NewAgent(cfg)
		if tc.blocked {
			if _, ok := errors.AsType[*ValidationError](err); !ok || !strings.Contains(err.Error(), guardErr) {
				t.Errorf("%s: NewAgent = %v, want the policy guard error", name, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: NewAgent: %v", name, err)
		}
		if err := agent.Start(t.Context()); err != nil {
			t.Errorf("%s: Start = %v", name, err)
		}
		_ = agent.Close()
	}
}

// writeTools returns the default tools that are not read-only.
func writeTools() []BuiltinTool {
	return slices.DeleteFunc(DefaultTools(), func(t BuiltinTool) bool { return slices.Contains(PolicyFreeTools(), t) })
}

func TestAgentBeforeStartAndValidation(t *testing.T) {
	cfg, _, _ := fakeAgentConfig(t)
	agent, err := NewAgent(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if agent.IsStarted() || agent.Conversation() != nil || agent.ConversationID() != "" || agent.SandboxStatus() != nil {
		t.Fatal("unstarted agent reports a session")
	}
	if _, err := agent.Chat(t.Context(), Text("hello")); !errors.Is(err, ErrNotStarted) {
		t.Fatalf("Chat before Start = %v", err)
	}
	if err := agent.Close(); err != nil {
		t.Fatal(err)
	}
	for _, content := range [][]Content{nil, {Text("")}, {Text("   ")}, {Text(""), Text("  ")}} {
		if _, err := agent.Chat(t.Context(), content...); err == nil || !strings.Contains(err.Error(), "non-empty message") {
			t.Errorf("Chat(%q) = %v", content, err)
		}
	}
	if _, err := NewAgent(Options{ConversationID: "short"}); err == nil {
		t.Fatal("invalid config accepted")
	}
	clearModelEnv(t)
	cfg.APIKey = ""
	agent, _ = NewAgent(cfg)
	if err := agent.Start(t.Context()); err == nil || !strings.Contains(err.Error(), "API key is required") {
		t.Fatalf("Start without key = %v", err)
	}
	cfg.APIKey = "k"
	cfg.CLIPath = ""
	cfg.Env = map[string]string{}
	t.Setenv("ANTIGRAVITY_HARNESS_PATH", "")
	t.Setenv("PATH", t.TempDir())
	agent, _ = NewAgent(cfg)
	if err := agent.Start(t.Context()); !errors.Is(err, ErrCLINotFound) {
		t.Fatalf("Start without binary = %v", err)
	}
}

func TestAgentStartFailureIsConnectionError(t *testing.T) {
	cfg, _, _ := fakeAgentConfig(t)
	cfg.CLIPath = filepath.Join(t.TempDir(), "missing-binary")
	agent, _ := NewAgent(cfg)
	err := agent.Start(t.Context())
	if _, ok := errors.AsType[*ConnectionError](err); !ok || !errors.Is(err, ErrCLINotFound) {
		t.Fatalf("Start = %v (%T)", err, err)
	}
	if agent.IsStarted() {
		t.Fatal("started after failure")
	}
}

func TestAgentDoneAndErr(t *testing.T) {
	cfg, _, _ := fakeAgentConfig(t)
	agent, err := NewAgent(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Before Start: closed channel, ErrNotStarted.
	waitDone(t, agent.Done())
	if err := agent.Err(); !errors.Is(err, ErrNotStarted) {
		t.Fatalf("Err before Start = %v", err)
	}

	// Close: Done closes, Err is nil.
	if err := agent.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agent.Close() })
	done := agent.Done()
	select {
	case <-done:
		t.Fatal("Done closed while running")
	default:
	}
	if err := agent.Err(); err != nil {
		t.Fatalf("Err while running = %v", err)
	}
	if err := agent.Close(); err != nil {
		t.Fatal(err)
	}
	waitDone(t, done)
	if err := agent.Err(); err != nil {
		t.Fatalf("Err after Close = %v", err)
	}

	// Restart, then the harness process exits: Done closes and Err carries
	// its stderr.
	if err := agent.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	done = agent.Done()
	select {
	case <-done:
		t.Fatal("Done of the restarted session already closed")
	default:
	}
	text, err := chat(t, agent, "crash")
	if _, ok := errors.AsType[*ConnectionError](err); !ok {
		t.Fatalf("chat with a crashing harness: %q, %v", text, err)
	}
	if pe, ok := errors.AsType[*ProcessError](err); !ok || pe.ExitCode == nil || *pe.ExitCode != 3 {
		t.Fatalf("turn error of a crashing harness %v, want a *ProcessError with status 3", err)
	}
	waitDone(t, done)
	ce, ok := errors.AsType[*ConnectionError](agent.Err())
	if !ok || !strings.Contains(ce.Stderr, "crashing on purpose") {
		t.Fatalf("Err after crash = %v", agent.Err())
	}
	if pe, ok := errors.AsType[*ProcessError](agent.Err()); !ok || !strings.Contains(pe.Stderr, "crashing on purpose") {
		t.Fatalf("Err after crash = %v, want a *ProcessError", agent.Err())
	}
	if err := agent.Close(); err != nil {
		t.Logf("Close after crash: %v", err)
	}
	if _, ok := errors.AsType[*ConnectionError](agent.Err()); !ok {
		t.Fatalf("Err after crash and Close = %v", agent.Err())
	}
}
