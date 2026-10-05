package claude

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"iter"
	"sync"
	"testing"
	"time"
)

func startEngine(t *testing.T, opts *Options) (*engine, *fakeTransport) {
	t.Helper()
	ft := newFakeTransport()
	if err := ft.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}
	eng := newTestEngine(t, ft, opts)
	eng.start(t.Context())
	t.Cleanup(func() { _ = eng.close() })
	return eng, ft
}

// newTestEngine builds an engine over transport for opts, nil meaning none,
// with its launch resolved.
func newTestEngine(t *testing.T, transport Transport, opts *Options) *engine {
	t.Helper()
	if opts == nil {
		opts = &Options{}
	}
	launch, err := resolveLaunch(opts)
	if err != nil {
		t.Fatal(err)
	}
	return newEngine(transport, opts, launch)
}

// interrupt sends an interrupt control request on eng.
func interrupt(ctx context.Context, eng *engine) error {
	_, err := eng.request(ctx, "interrupt", nil)
	return err
}

// rawInputs yields each frame as a UserInput sent verbatim.
func rawInputs(frames ...map[string]any) iter.Seq[UserInput] {
	return func(yield func(UserInput) bool) {
		for _, f := range frames {
			if !yield(UserInput{Raw: f}) {
				return
			}
		}
	}
}

func TestEngineMessageStream(t *testing.T) {
	t.Parallel()
	eng, ft := startEngine(t, nil)
	ft.push(map[string]any{"type": "system", "subtype": "init"})
	ft.push(map[string]any{"type": "brand_new", "x": 1})
	ft.push(nil)
	ft.push([]any{"not", "an", "object"})
	ft.push(map[string]any{"type": "assistant", "message": map[string]any{
		"model": "opus", "content": []any{map[string]any{"type": "text", "text": "hi"}}}})
	ft.push(map[string]any{"type": "result", "subtype": "success", "duration_ms": 1,
		"duration_api_ms": 1, "is_error": false, "num_turns": 1, "session_id": "s1"})
	ft.finish(nil)

	var kinds []string
	for msg, err := range eng.receive(context.Background()) {
		if err != nil {
			t.Fatalf("stream error: %v", err)
		}
		switch msg.(type) {
		case *SystemMessage, *InitMessage:
			kinds = append(kinds, "system")
		case *AssistantMessage:
			kinds = append(kinds, "assistant")
		case *ResultMessage:
			kinds = append(kinds, "result")
		case *UnknownMessage:
			kinds = append(kinds, "unknown")
		default:
			t.Fatalf("unexpected message %T", msg)
		}
	}
	// The unknown frame type is surfaced as is; the non-object frames are
	// skipped rather than treated as fatal.
	if len(kinds) != 4 || kinds[0] != "system" || kinds[1] != "unknown" || kinds[3] != "result" {
		t.Fatalf("kinds = %q", kinds)
	}
}

func TestEngineConcurrentControlRequests(t *testing.T) {
	t.Parallel()
	eng, ft := startEngine(t, nil)
	// Answer out of order, so responses must be matched by request id.
	var mu sync.Mutex
	var queued []map[string]any
	ft.mu.Lock()
	ft.onWrite = func(frame map[string]any) {
		if frame["type"] != "control_request" {
			return
		}
		mu.Lock()
		queued = append(queued, frame)
		flush := len(queued) == 8
		batch := queued
		mu.Unlock()
		if !flush {
			return
		}
		for i := len(batch) - 1; i >= 0; i-- {
			ft.push(map[string]any{
				"type": "control_response",
				"response": map[string]any{
					"subtype":    "success",
					"request_id": batch[i]["request_id"],
					"response":   map[string]any{"echo": batch[i]["request_id"]},
				},
			})
		}
	}
	ft.mu.Unlock()

	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = eng.request(t.Context(), "stop_task", map[string]any{"task_id": "task"})
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
}

func TestEngineControlErrorResponse(t *testing.T) {
	t.Parallel()
	eng, ft := startEngine(t, nil)
	ft.mu.Lock()
	ft.onWrite = func(frame map[string]any) {
		ft.push(map[string]any{
			"type": "control_response",
			"response": map[string]any{
				"subtype":    "error",
				"request_id": frame["request_id"],
				"error":      "not supported",
			},
		})
	}
	ft.mu.Unlock()

	err := interrupt(t.Context(), eng)
	var ctrlErr *ControlError
	if !errors.As(err, &ctrlErr) {
		t.Fatalf("error = %T (%v), want *ControlError", err, err)
	}
	if ctrlErr.Error() != "not supported" {
		t.Fatalf("message = %q", ctrlErr.Error())
	}
	var sdkErr Error
	if !errors.As(err, &sdkErr) {
		t.Fatal("ControlError should satisfy claude.Error")
	}
}

func TestEngineControlRequestTimeout(t *testing.T) {
	t.Parallel()
	eng, _ := startEngine(t, nil)
	ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond)
	defer cancel()
	_, err := eng.sendControlRequest(ctx, map[string]any{"subtype": "interrupt"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %T (%v)", err, err)
	}
	// A timed-out request must not leak its pending entry.
	eng.mu.Lock()
	pending := len(eng.pending)
	eng.mu.Unlock()
	if pending != 0 {
		t.Fatalf("pending = %d, want 0", pending)
	}
}

func TestEngineInitializeRegistersHooks(t *testing.T) {
	t.Parallel()
	called := make(chan map[string]any, 1)
	opts := &Options{
		Hooks: map[HookEvent][]HookMatcher{
			HookPreToolUse: {{
				Matcher: "Bash",
				Timeout: 30,
				Hooks: []HookCallback{func(_ context.Context, input map[string]any, toolUseID string, _ HookContext) (HookOutput, error) {
					called <- input
					return HookOutput{
						Decision:           "block",
						Reason:             "denied by policy",
						HookSpecificOutput: map[string]any{"hookEventName": "PreToolUse"},
					}, nil
				}},
			}},
		},
		Agents:              map[string]AgentDefinition{"rev": {Description: "d", Prompt: "p"}},
		ForwardSubagentText: true,
		Skills:              SkillList{"pdf"},
		SystemPrompt:        &SystemPromptPreset{ExcludeDynamicSections: true},
	}
	eng, ft := startEngine(t, opts)
	ft.respondSuccess(map[string]any{"commands": []any{"/help"}, "output_style": "default"})

	info, err := eng.initialize(t.Context())
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if info.Raw["output_style"] != "default" {
		t.Fatalf("server info = %#v", info.Raw)
	}
	if eng.initializeResult().Raw["output_style"] != "default" {
		t.Fatal("server info should be retained")
	}

	frames := ft.frames(t)
	req := frames[0]["request"].(map[string]any)
	if req["subtype"] != "initialize" {
		t.Fatalf("first frame = %#v", frames[0])
	}
	hooks := req["hooks"].(map[string]any)[HookPreToolUse].([]any)
	matcher := hooks[0].(map[string]any)
	if matcher["matcher"] != "Bash" || matcher["timeout"] != float64(30) {
		t.Fatalf("matcher = %#v", matcher)
	}
	ids := matcher["hookCallbackIds"].([]any)
	if len(ids) != 1 {
		t.Fatalf("callback ids = %#v", ids)
	}
	if req["forwardSubagentText"] != true || req["excludeDynamicSections"] != true {
		t.Fatalf("request = %#v", req)
	}
	if skills := req["skills"].([]any); len(skills) != 1 || skills[0] != "pdf" {
		t.Fatalf("skills = %#v", req["skills"])
	}
	if _, ok := req["agents"].(map[string]any)["rev"]; !ok {
		t.Fatalf("agents = %#v", req["agents"])
	}

	// The CLI now invokes the registered callback.
	ft.mu.Lock()
	ft.onWrite = nil
	ft.mu.Unlock()
	ft.push(map[string]any{
		"type":       "control_request",
		"request_id": "cli_1",
		"request": map[string]any{
			"subtype":     "hook_callback",
			"callback_id": ids[0],
			"input":       map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Bash"},
			"tool_use_id": "tu1",
		},
	})
	select {
	case input := <-called:
		if input["tool_name"] != "Bash" {
			t.Fatalf("hook input = %#v", input)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("hook was not invoked")
	}
	response := ft.nextResponse(t)
	if response["subtype"] != "success" || response["request_id"] != "cli_1" {
		t.Fatalf("reply = %#v", response)
	}
	out := response["response"].(map[string]any)
	if out["decision"] != "block" || out["reason"] != "denied by policy" {
		t.Fatalf("hook output = %#v", out)
	}
	if hso := out["hookSpecificOutput"].(map[string]any); hso["hookEventName"] != "PreToolUse" {
		t.Fatalf("hook specific output = %#v", hso)
	}
}

func TestEngineHookErrors(t *testing.T) {
	t.Parallel()
	opts := &Options{
		Hooks: map[HookEvent][]HookMatcher{
			HookStop: {{Hooks: []HookCallback{
				func(context.Context, map[string]any, string, HookContext) (HookOutput, error) {
					panic("boom")
				},
			}}},
		},
	}
	eng, ft := startEngine(t, opts)
	ft.respondSuccess(nil)
	if _, err := eng.initialize(t.Context()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	ft.mu.Lock()
	ft.onWrite = nil
	ft.mu.Unlock()

	// An unknown callback id is an error response, not a crash.
	ft.push(map[string]any{"type": "control_request", "request_id": "c1",
		"request": map[string]any{"subtype": "hook_callback", "callback_id": "nope"}})
	reply := ft.nextResponse(t)
	if reply["subtype"] != "error" || reply["error"] != "No hook callback found for ID: nope" {
		t.Fatalf("reply = %#v", reply)
	}

	// A panicking callback becomes an error response too.
	ft.push(map[string]any{"type": "control_request", "request_id": "c2",
		"request": map[string]any{"subtype": "hook_callback", "callback_id": "hook_0"}})
	reply = ft.nextResponse(t)
	if reply["subtype"] != "error" || reply["error"] == "" {
		t.Fatalf("reply = %#v", reply)
	}
}

func TestEngineCanUseTool(t *testing.T) {
	t.Parallel()
	var seen ToolPermissionContext
	opts := &Options{
		CanUseTool: func(_ context.Context, tool string, input map[string]any, permCtx ToolPermissionContext) (PermissionResult, error) {
			seen = permCtx
			if tool == "Bash" {
				return &PermissionResultDeny{Message: "no shell", Interrupt: true}, nil
			}
			return &PermissionResultAllow{
				UpdatedInput:       map[string]any{"file_path": "/safe"},
				UpdatedPermissions: []PermissionUpdate{{Type: PermissionUpdateSetMode, Mode: PermissionModePlan}},
			}, nil
		},
	}
	_, ft := startEngine(t, opts)

	ft.push(map[string]any{"type": "control_request", "request_id": "p1", "request": map[string]any{
		"subtype":     "can_use_tool",
		"tool_name":   "Read",
		"input":       map[string]any{"file_path": "/etc/passwd"},
		"tool_use_id": "tu1",
		"agent_id":    "a1",
		"title":       "Claude wants to read",
		"permission_suggestions": []any{map[string]any{
			"type": "addRules", "behavior": "allow",
			"rules": []any{map[string]any{"toolName": "Read"}},
		}},
	}})
	response := ft.nextResponse(t)
	if response["request_id"] != "p1" || response["subtype"] != "success" {
		t.Fatalf("response = %#v", response)
	}
	out := response["response"].(map[string]any)
	if out["behavior"] != "allow" {
		t.Fatalf("out = %#v", out)
	}
	if out["updatedInput"].(map[string]any)["file_path"] != "/safe" {
		t.Fatalf("updated input = %#v", out["updatedInput"])
	}
	perms := out["updatedPermissions"].([]any)
	if perms[0].(map[string]any)["mode"] != "plan" {
		t.Fatalf("updated permissions = %#v", perms)
	}
	if seen.ToolUseID != "tu1" || seen.AgentID != "a1" || seen.Title != "Claude wants to read" {
		t.Fatalf("context = %+v", seen)
	}
	if len(seen.Suggestions) != 1 || seen.Suggestions[0].Type != PermissionUpdateAddRules {
		t.Fatalf("suggestions = %#v", seen.Suggestions)
	}

	// Deny, with the interrupt flag.
	ft.push(map[string]any{"type": "control_request", "request_id": "p2", "request": map[string]any{
		"subtype": "can_use_tool", "tool_name": "Bash", "input": map[string]any{}, "tool_use_id": "tu2"}})
	out = ft.nextResponse(t)["response"].(map[string]any)
	if out["behavior"] != "deny" || out["message"] != "no shell" || out["interrupt"] != true {
		t.Fatalf("out = %#v", out)
	}
}

func TestEngineCanUseToolKeepsOriginalInput(t *testing.T) {
	t.Parallel()
	opts := &Options{
		CanUseTool: func(context.Context, string, map[string]any, ToolPermissionContext) (PermissionResult, error) {
			return &PermissionResultAllow{}, nil
		},
	}
	_, ft := startEngine(t, opts)
	ft.push(map[string]any{"type": "control_request", "request_id": "p1", "request": map[string]any{
		"subtype": "can_use_tool", "tool_name": "Read",
		"input": map[string]any{"file_path": "/x"}, "tool_use_id": "tu1"}})
	out := ft.nextResponse(t)["response"].(map[string]any)
	if out["updatedInput"].(map[string]any)["file_path"] != "/x" {
		t.Fatalf("out = %#v", out)
	}
}

func TestEngineCanUseToolErrors(t *testing.T) {
	t.Parallel()
	// No callback configured.
	_, ft := startEngine(t, nil)
	ft.push(map[string]any{"type": "control_request", "request_id": "p1",
		"request": map[string]any{"subtype": "can_use_tool", "tool_name": "Read"}})
	reply := ft.nextResponse(t)
	if reply["subtype"] != "error" || reply["error"] != "canUseTool callback is not provided" {
		t.Fatalf("reply = %#v", reply)
	}

	// A callback that fails.
	opts := &Options{CanUseTool: func(context.Context, string, map[string]any, ToolPermissionContext) (PermissionResult, error) {
		return nil, errors.New("policy service down")
	}}
	_, ft2 := startEngine(t, opts)
	ft2.push(map[string]any{"type": "control_request", "request_id": "p2",
		"request": map[string]any{"subtype": "can_use_tool", "tool_name": "Read"}})
	reply = ft2.nextResponse(t)
	if reply["error"] != "policy service down" {
		t.Fatalf("reply = %#v", reply)
	}
}

func TestEngineUnsupportedControlRequest(t *testing.T) {
	t.Parallel()
	_, ft := startEngine(t, nil)
	ft.push(map[string]any{"type": "control_request", "request_id": "x1",
		"request": map[string]any{"subtype": "who_knows"}})
	reply := ft.nextResponse(t)
	if reply["subtype"] != "error" || reply["error"] != "Unsupported control request subtype: who_knows" {
		t.Fatalf("reply = %#v", reply)
	}
}

func TestEngineMCPMessageUnknownServer(t *testing.T) {
	t.Parallel()
	_, ft := startEngine(t, nil)
	ft.push(map[string]any{"type": "control_request", "request_id": "m1", "request": map[string]any{
		"subtype": "mcp_message", "server_name": "calc",
		"message": map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"}}})
	out := ft.nextResponse(t)["response"].(map[string]any)
	rpc := out["mcp_response"].(map[string]any)
	rpcErr := rpc["error"].(map[string]any)
	if rpcErr["code"] != float64(-32601) || rpcErr["message"] != "Server 'calc' not found" {
		t.Fatalf("mcp response = %#v", rpc)
	}
	if rpc["id"] != float64(1) {
		t.Fatalf("mcp response should echo the request id: %#v", rpc)
	}
}

func TestEngineMCPMessageRouted(t *testing.T) {
	t.Parallel()
	eng, ft := startEngine(t, nil)
	eng.mcpServers.connect("calc", &MCPSDKServerConfig{Name: "calc", Instance: mcpHandlerFunc(func(_ context.Context, message jsontext.Value) (jsontext.Value, error) {
		var req map[string]any
		if err := json.Unmarshal(message, &req); err != nil {
			return nil, err
		}
		switch req["method"] {
		case "notifications/initialized":
			return nil, nil
		case "boom":
			return nil, errors.New("handler failed")
		}
		return jsontext.Value(`{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`), nil
	})})

	ft.push(map[string]any{"type": "control_request", "request_id": "m1", "request": map[string]any{
		"subtype": "mcp_message", "server_name": "calc",
		"message": map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"}}})
	out := ft.nextResponse(t)["response"].(map[string]any)
	if _, ok := out["mcp_response"].(map[string]any)["result"]; !ok {
		t.Fatalf("mcp response = %#v", out)
	}

	// A notification still gets an acknowledgement.
	ft.push(map[string]any{"type": "control_request", "request_id": "m2", "request": map[string]any{
		"subtype": "mcp_message", "server_name": "calc",
		"message": map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}}})
	out = ft.nextResponse(t)["response"].(map[string]any)
	rpc := out["mcp_response"].(map[string]any)
	if rpc["jsonrpc"] != "2.0" {
		t.Fatalf("ack = %#v", rpc)
	}

	// Handler failures and unknown servers become JSON-RPC errors, not
	// protocol errors.
	for _, tc := range []struct {
		server, method string
		code           float64
	}{
		{"calc", "boom", -32603},
		{"other", "tools/list", -32601},
	} {
		ft.push(map[string]any{"type": "control_request", "request_id": "m-" + tc.server, "request": map[string]any{
			"subtype": "mcp_message", "server_name": tc.server,
			"message": map[string]any{"jsonrpc": "2.0", "id": 2, "method": tc.method}}})
		out = ft.nextResponse(t)["response"].(map[string]any)
		rpcErr := out["mcp_response"].(map[string]any)["error"].(map[string]any)
		if rpcErr["code"] != tc.code {
			t.Fatalf("%s/%s: mcp error = %#v", tc.server, tc.method, rpcErr)
		}
	}
}

func TestEngineControlCancelRequest(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	opts := &Options{
		CanUseTool: func(ctx context.Context, _ string, _ map[string]any, _ ToolPermissionContext) (PermissionResult, error) {
			close(started)
			<-ctx.Done()
			return &PermissionResultAllow{}, nil
		},
	}
	_, ft := startEngine(t, opts)
	ft.push(map[string]any{"type": "control_request", "request_id": "p1",
		"request": map[string]any{"subtype": "can_use_tool", "tool_name": "Read"}})
	<-started
	ft.push(map[string]any{"type": "control_cancel_request", "request_id": "p1"})

	// A cancelled request gets no reply.
	select {
	case raw := <-ft.writeCh:
		t.Fatalf("unexpected write after cancel: %s", raw)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestEngineProcessErrorBecomesResultError(t *testing.T) {
	t.Parallel()
	eng, ft := startEngine(t, nil)
	ft.push(map[string]any{"type": "result", "subtype": "error_max_turns", "duration_ms": 1,
		"duration_api_ms": 1, "is_error": true, "num_turns": 5, "session_id": "s1",
		"errors": []any{"turn limit reached"}})
	code := 1
	ft.finish(NewProcessError("Command failed with exit code 1", &code, ""))

	var last error
	for _, err := range eng.receive(context.Background()) {
		if err != nil {
			last = err
		}
	}
	var resErr *ResultError
	if !errors.As(last, &resErr) {
		t.Fatalf("error = %T (%v), want *ResultError", last, last)
	}
	if resErr.Subtype != "error_max_turns" || resErr.SessionID != "s1" {
		t.Fatalf("result error = %+v", resErr)
	}
	if resErr.Error() != "Claude Code returned an error result: turn limit reached (exit code: 1)" {
		t.Fatalf("message = %q", resErr.Error())
	}
}

func TestEngineProcessErrorKeptAfterOtherMessages(t *testing.T) {
	t.Parallel()
	eng, ft := startEngine(t, nil)
	ft.push(map[string]any{"type": "result", "subtype": "error_during_execution", "duration_ms": 1,
		"duration_api_ms": 1, "is_error": true, "num_turns": 1, "session_id": "s1"})
	// A later message means the conversation moved on, so the exit is a
	// fresh failure rather than the expected exit after the error result.
	ft.push(map[string]any{"type": "system", "subtype": "init"})
	code := 1
	ft.finish(NewProcessError("Command failed with exit code 1", &code, "stderr tail"))

	var last error
	for _, err := range eng.receive(context.Background()) {
		if err != nil {
			last = err
		}
	}
	var procErr *ProcessError
	if !errors.As(last, &procErr) {
		t.Fatalf("error = %T (%v)", last, last)
	}
	var resErr *ResultError
	if errors.As(last, &resErr) {
		t.Fatal("should not have been converted to a ResultError")
	}
}

func TestEngineReadErrorFailsPendingRequests(t *testing.T) {
	t.Parallel()
	eng, ft := startEngine(t, nil)
	done := make(chan error, 1)
	go func() { done <- interrupt(t.Context(), eng) }()
	ft.nextWrite(t) // the control request

	ft.finish(errors.New("stream broke"))
	select {
	case err := <-done:
		if err == nil || err.Error() != "stream broke" {
			t.Fatalf("error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pending request was not failed")
	}
}

func TestEngineStreamInput(t *testing.T) {
	t.Parallel()
	eng, ft := startEngine(t, nil)
	if err := eng.streamInput(t.Context(), rawInputs(
		map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": "one"}},
		map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": "two"}},
	)); err != nil {
		t.Fatalf("stream input: %v", err)
	}
	frames := ft.frames(t)
	if len(frames) != 2 {
		t.Fatalf("frames = %#v", frames)
	}
	if frames[1]["message"].(map[string]any)["content"] != "two" {
		t.Fatalf("frame = %#v", frames[1])
	}
	// With no bidirectional needs the input stream closes immediately.
	if !ft.endedInput() {
		t.Fatal("input should have been ended")
	}
}

func TestEngineStreamInputWaitsForResult(t *testing.T) {
	t.Parallel()
	opts := &Options{CanUseTool: func(context.Context, string, map[string]any, ToolPermissionContext) (PermissionResult, error) {
		return &PermissionResultAllow{}, nil
	}}
	eng, ft := startEngine(t, opts)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = eng.streamInput(t.Context(), rawInputs(map[string]any{"type": "user", "message": map[string]any{"content": "hi"}}))
	}()

	// The hold is released only by a run-ending result.
	select {
	case <-done:
		t.Fatal("input was closed before the result arrived")
	case <-time.After(100 * time.Millisecond):
	}
	if ft.endedInput() {
		t.Fatal("input should still be open")
	}
	ft.push(map[string]any{"type": "result", "subtype": "success", "duration_ms": 1,
		"duration_api_ms": 1, "is_error": false, "num_turns": 1, "session_id": "s1"})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("input was never closed")
	}
	if !ft.endedInput() {
		t.Fatal("input should have been ended")
	}
}

func TestEngineStreamInputHeldByInflightTask(t *testing.T) {
	t.Parallel()
	opts := &Options{CanUseTool: func(context.Context, string, map[string]any, ToolPermissionContext) (PermissionResult, error) {
		return &PermissionResultAllow{}, nil
	}}
	eng, ft := startEngine(t, opts)

	// A delegated task that is still running keeps the input open past the
	// turn's result frame.
	ft.push(map[string]any{"type": "system", "subtype": "task_started", "task_id": "t1",
		"task_type": "local_agent", "description": "d", "uuid": "u1", "session_id": "s1"})
	ft.push(map[string]any{"type": "result", "subtype": "success", "duration_ms": 1,
		"duration_api_ms": 1, "is_error": false, "num_turns": 1, "session_id": "s1"})

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = eng.streamInput(t.Context(), rawInputs(map[string]any{"type": "user", "message": map[string]any{"content": "hi"}}))
	}()
	select {
	case <-done:
		t.Fatal("input closed while a task was in flight")
	case <-time.After(100 * time.Millisecond):
	}

	ft.push(map[string]any{"type": "system", "subtype": "task_notification", "task_id": "t1",
		"status": "completed", "output_file": "/o", "summary": "s", "uuid": "u2", "session_id": "s1"})
	ft.push(map[string]any{"type": "result", "subtype": "success", "duration_ms": 1,
		"duration_api_ms": 1, "is_error": false, "num_turns": 2, "session_id": "s1"})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("input was never closed")
	}
}

func TestEngineCloseJoinsHandlers(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	running := make(chan struct{})
	finished := make(chan struct{})
	opts := &Options{
		CanUseTool: func(context.Context, string, map[string]any, ToolPermissionContext) (PermissionResult, error) {
			close(running)
			<-release
			close(finished)
			return &PermissionResultAllow{}, nil
		},
	}
	eng, ft := startEngine(t, opts)
	ft.push(map[string]any{"type": "control_request", "request_id": "p1",
		"request": map[string]any{"subtype": "can_use_tool", "tool_name": "Read"}})
	<-running

	closed := make(chan error, 1)
	go func() { closed <- eng.close() }()
	select {
	case <-closed:
		t.Fatal("Close returned while a handler was still running")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return")
	}
	select {
	case <-finished:
	default:
		t.Fatal("handler did not finish before Close returned")
	}
	// The message stream terminates once the engine is closed.
	for range eng.receive(context.Background()) {
	}
}

func TestEngineCloseUnblocksControlRequests(t *testing.T) {
	t.Parallel()
	eng, _ := startEngine(t, nil)
	done := make(chan error, 1)
	go func() { done <- interrupt(t.Context(), eng) }()
	time.Sleep(50 * time.Millisecond)
	_ = eng.close()
	select {
	case err := <-done:
		var connErr *ConnectionError
		if !errors.As(err, &connErr) {
			t.Fatalf("error = %T (%v)", err, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not unblock the pending request")
	}
}

func TestEngineContextCancelsControlRequest(t *testing.T) {
	t.Parallel()
	eng, _ := startEngine(t, nil)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- interrupt(ctx, eng) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelling the context did not unblock the request")
	}
}

// mcpHandlerFunc adapts a function to MCPHandler.
type mcpHandlerFunc func(ctx context.Context, message jsontext.Value) (jsontext.Value, error)

func (f mcpHandlerFunc) HandleMCPMessage(ctx context.Context, message jsontext.Value) (jsontext.Value, error) {
	return f(ctx, message)
}
