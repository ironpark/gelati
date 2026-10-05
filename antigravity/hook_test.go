package antigravity

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/ironpark/gelati/antigravity/internal/wire"
)

func newTestRouter(t *testing.T, hooks ...Hook) *hookRouter {
	t.Helper()
	return &hookRouter{hooks: mustHooks(t, hooks...)}
}

func route(t *testing.T, r *hookRouter, typ wire.LifecycleHook, fill func(*wire.CallHookRequest)) *wire.CallHookResponse {
	t.Helper()
	return r.handle(t.Context(), r.hooks.newTurnContext(), hookRequest("req", typ, fill).CallHookRequest, quietLogger())
}

func TestHookRouterUnknownHook(t *testing.T) {
	resp := route(t, newTestRouter(t), wire.LifecycleHookUnspecified, nil)
	if resp.GetRequestID() != "req" || resp.EmptyResult == nil {
		t.Fatalf("response %+v", resp)
	}
}

func TestHookRouterPreTurnMultipart(t *testing.T) {
	var got []Content
	r := newTestRouter(t, PreTurnHook(func(_ context.Context, hc *HookContext, prompt []Content) (HookResult, error) {
		if hc.Scope() != ScopeTurn || hc.Parent().Scope() != ScopeSession {
			t.Errorf("pre-turn context scope %v", hc.Scope())
		}
		got = prompt
		return HookResult{}, nil
	}))
	route(t, r, wire.LifecycleHookPreTurn, func(req *wire.CallHookRequest) {
		req.PreTurnArgs = &wire.PreTurnArgs{UserInput: &wire.UserInput{Parts: []*wire.UserInputPart{{Text: new("hello")}, {Text: new("world")}}}}
	})
	if !reflect.DeepEqual(got, []Content{Text("hello"), Text("world")}) {
		t.Fatalf("prompt %#v", got)
	}
	// No arguments: the prompt is a single empty text.
	route(t, r, wire.LifecycleHookPreTurn, nil)
	if !reflect.DeepEqual(got, []Content{Text("")}) {
		t.Fatalf("empty prompt %#v", got)
	}
}

func TestHookRouterPostTool(t *testing.T) {
	var got *ToolResult
	r := newTestRouter(t, PostToolCallHook(func(_ context.Context, hc *HookContext, res *ToolResult) error {
		if hc.Scope() != ScopeOperation {
			t.Errorf("post-tool scope %v", hc.Scope())
		}
		got = res
		return nil
	}))
	post := func(pta *wire.PostToolArgs) *ToolResult {
		t.Helper()
		got = nil
		resp := route(t, r, wire.LifecycleHookPostTool, func(req *wire.CallHookRequest) { req.PostToolArgs = pta })
		if resp.EmptyResult == nil || got == nil {
			t.Fatalf("response %+v, result %+v", resp, got)
		}
		return got
	}
	res := post(&wire.PostToolArgs{ToolName: new("view_file"), Result: new("file content here"), TrajectoryID: new("traj-1"), StepIndex: new(uint32(5))})
	if res.Name != "view_file" || res.Result != "file content here" || res.StepID != "traj-1:5" || res.Failed() {
		t.Fatalf("view_file result %+v", res)
	}
	if res := post(&wire.PostToolArgs{ToolName: new("view_file"), Result: new("x"), StepIndex: new(uint32(7))}); res.StepID != "7" {
		t.Fatalf("step id %q", res.StepID)
	}
	res = post(&wire.PostToolArgs{ToolName: new("pirate_multiply"), ServerName: new("pirate_math"), Result: new("35"), CallID: new("call_post")})
	if res.ServerName != "pirate_math" || res.ID != "call_post" || res.Result != "35" || res.StepID != "" {
		t.Fatalf("mcp result %+v", res)
	}
	res = post(&wire.PostToolArgs{ToolName: new("run_command"), Error: new("command not found")})
	if res.Result != nil || res.Error != "command not found" || !res.Failed() {
		t.Fatalf("error result %+v", res)
	}
	// Structured extraction is skipped for failed calls.
	res = post(&wire.PostToolArgs{ToolName: new("list_directory"), Result: new(`{"entries": []}`), Error: new("Command failed")})
	if res.Result != nil || res.Error != "Command failed" {
		t.Fatalf("failed list result %+v", res)
	}
}

func TestExtractToolResult(t *testing.T) {
	for _, tc := range []struct {
		tool, result string
		want         any
	}{
		{"run_command", `{"combined_output": "hi\n"}`, &RunCommandResult{Output: "hi\n"}},
		{"run_command", `{"output": "o"}`, &RunCommandResult{Output: "o"}},
		{"list_directory", `{"results": [{"name": "foo.py", "file_size": 100}]}`, &ListDirectoryResult{Entries: []ListDirectoryEntry{{Name: "foo.py", FileSize: 100}}}},
		{"find_file", `{"output": "/tmp/a.txt"}`, &FindFileResult{Output: "/tmp/a.txt"}},
		{"find_file", `plain text`, &FindFileResult{Output: "plain text"}},
		{"search_directory", `{"num_results": 5}`, &SearchDirectoryResult{NumResults: 5}},
		{"edit_file", `{"summary": "Edited file"}`, &EditFileResult{Summary: "Edited file"}},
		{"edit_file", `not json`, &EditFileResult{Summary: "not json"}},
		{"generate_image", `{"image_name": "cat_img"}`, &GenerateImageResult{ImageName: "cat_img"}},
		{"search_web", `{"summary": "news results"}`, &SearchWebResult{Summary: "news results"}},
		{"read_url_content", `{"title": "Example Domain", "summary": "s", "content_path": "/tmp/content.md"}`, &ReadURLContentResult{Title: "Example Domain", Summary: "s", ContentPath: "/tmp/content.md"}},
		{"view_file", "Viewed file", nil},
		{"list_directory", `{"results": [not valid json...`, nil},
		{"list_directory", `["just", "a", "list"]`, nil},
		{"list_directory", `{"results": null}`, nil},
		{"start_subagent", "", nil},
	} {
		got := extractToolResult(tc.tool, tc.result)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("extract(%s, %q) = %#v, want %#v", tc.tool, tc.result, got, tc.want)
		}
	}
	if s := (&GenerateImageResult{ImageName: "sunset", OutputPath: "/tmp/sunset_123.png"}).String(); s != "/tmp/sunset_123.png" {
		t.Fatal(s)
	}
	if s := (&GenerateImageResult{ImageName: "sunset"}).String(); s != "sunset" {
		t.Fatal(s)
	}
	if s := (&ListDirectoryResult{Entries: []ListDirectoryEntry{{Name: "d", IsDirectory: true}, {Name: "f", FileSize: 3}}}).String(); !strings.Contains(s, "d/ (dir)") || !strings.Contains(s, "f (3 bytes)") {
		t.Fatal(s)
	}
}

func TestHookRouterPostToolStructuredResult(t *testing.T) {
	var got *ToolResult
	r := newTestRouter(t, PostToolCallHook(func(_ context.Context, _ *HookContext, res *ToolResult) error { got = res; return nil }))
	route(t, r, wire.LifecycleHookPostTool, func(req *wire.CallHookRequest) {
		req.PostToolArgs = &wire.PostToolArgs{ToolName: new("invoke_subagent"), Result: new("")}
	})
	if got.Name != "start_subagent" || got.Result != "" {
		t.Fatalf("subagent result %+v", got)
	}
	route(t, r, wire.LifecycleHookPostTool, func(req *wire.CallHookRequest) {
		req.PostToolArgs = &wire.PostToolArgs{ToolName: new("run_command"), Result: new(`{"combined_output": "hi"}`)}
	})
	if rc, ok := got.Result.(*RunCommandResult); !ok || rc.Output != "hi" {
		t.Fatalf("run_command result %#v", got.Result)
	}
}

func TestHookRouterToolError(t *testing.T) {
	var got *ToolExecutionError
	recovery := ""
	r := newTestRouter(t, OnToolErrorHook(func(_ context.Context, _ *HookContext, err *ToolExecutionError) (string, error) {
		got = err
		return recovery, nil
	}))
	resp := route(t, r, wire.LifecycleHookOnToolError, func(req *wire.CallHookRequest) {
		req.OnToolErrorArgs = &wire.OnToolErrorArgs{ToolName: new("run_command"), ErrorMessage: new("command failed"), TrajectoryID: new("traj-1"), StepIndex: new(uint32(3))}
	})
	if resp.EmptyResult == nil || got.ToolName != "run_command" || got.StepID != "traj-1:3" || got.ServerName != "" || got.Error() != "command failed" {
		t.Fatalf("response %+v, error %+v", resp, got)
	}
	route(t, r, wire.LifecycleHookOnToolError, func(req *wire.CallHookRequest) {
		req.OnToolErrorArgs = &wire.OnToolErrorArgs{ToolName: new("mcp_tool"), ErrorMessage: new("mcp error"), ServerName: new("mcp_server"), CallID: new("call_err")}
	})
	if got.ServerName != "mcp_server" || got.CallID != "call_err" {
		t.Fatalf("mcp error %+v", got)
	}
	recovery = "  fallback value  "
	resp = route(t, r, wire.LifecycleHookOnToolError, func(req *wire.CallHookRequest) {
		req.OnToolErrorArgs = &wire.OnToolErrorArgs{ToolName: new("my_tool"), ErrorMessage: new("broken")}
	})
	if resp.GetOnToolErrorResult().GetCustomErrorMessage() != "fallback value" {
		t.Fatalf("recovery response %+v", resp)
	}
	// Without hooks the response is empty.
	resp = route(t, newTestRouter(t), wire.LifecycleHookOnToolError, nil)
	if resp.EmptyResult == nil {
		t.Fatalf("no-hook response %+v", resp)
	}
}

func TestHookRouterPreTool(t *testing.T) {
	var got *ToolCall
	result := HookResult{}
	r := newTestRouter(t, PreToolCallHook(func(_ context.Context, _ *HookContext, call *ToolCall) (HookResult, error) {
		got = call
		return result, nil
	}))
	pre := func(pta *wire.PreToolArgs) *wire.PreToolResult {
		t.Helper()
		resp := route(t, r, wire.LifecycleHookPreTool, func(req *wire.CallHookRequest) { req.PreToolArgs = pta })
		return resp.GetPreToolResult()
	}
	res := pre(&wire.PreToolArgs{ToolName: new("run_command"), ArgumentsJSON: new(`{"cmd": "ls"}`), CallID: new("call_pre"), TrajectoryID: new("traj-1"), StepIndex: new(uint32(4))})
	if res.GetDecision() != wire.PreToolResultDecisionAllow || res.ModifiedArgs != nil {
		t.Fatalf("allow result %+v", res)
	}
	if got.Name != "run_command" || !reflect.DeepEqual(got.Args, map[string]any{"cmd": "ls"}) || got.ID != "call_pre" || got.StepID != "traj-1:4" {
		t.Fatalf("call %+v", got)
	}

	pre(&wire.PreToolArgs{ToolName: new("invoke_subagent"), ArgumentsJSON: new("{}")})
	if got.Name != "start_subagent" {
		t.Fatalf("name translation %q", got.Name)
	}
	pre(&wire.PreToolArgs{ToolName: new("view_file"), ArgumentsJSON: new(`{"file_path": "file:///home/user/foo.py"}`)})
	if got.Args["file_path"] != "/home/user/foo.py" || got.CanonicalPath != "/home/user/foo.py" {
		t.Fatalf("path normalization %+v", got)
	}
	pre(&wire.PreToolArgs{ToolName: new("replace_file_content"), ArgumentsJSON: new(`{"TargetFile": "file:///home/user/bar.py"}`)})
	if got.Args["TargetFile"] != "/home/user/bar.py" {
		t.Fatalf("TargetFile normalization %+v", got)
	}
	pre(&wire.PreToolArgs{ToolName: new("pirate_multiply"), ArgumentsJSON: new(`{"a": 5, "b": 7}`), ServerName: new("pirate_math")})
	if got.ServerName != "pirate_math" || !reflect.DeepEqual(got.Args, map[string]any{"a": 5.0, "b": 7.0}) {
		t.Fatalf("mcp call %+v", got)
	}

	// No arguments at all.
	resp := route(t, r, wire.LifecycleHookPreTool, nil)
	if resp.GetPreToolResult().GetDecision() != wire.PreToolResultDecisionAllow || got.Name != "" || len(got.Args) != 0 {
		t.Fatalf("no-args response %+v, call %+v", resp, got)
	}

	result = HookResult{ModifiedArgs: map[string]any{"cmd": "echo 'sanitized'"}}
	res = pre(&wire.PreToolArgs{ToolName: new("run_command"), ArgumentsJSON: new(`{"cmd": "rm -rf /"}`)})
	if res.GetDecision() != wire.PreToolResultDecisionAllow || res.ModifiedArgs.AsMap()["cmd"] != "echo 'sanitized'" {
		t.Fatalf("modified result %+v", res)
	}

	result = HookResult{Deny: true, Message: "blocked by policy"}
	res = pre(&wire.PreToolArgs{ToolName: new("run_command"), ArgumentsJSON: new(`{"cmd": "rm -rf /"}`)})
	if res.GetDecision() != wire.PreToolResultDecisionDeny || res.GetReason() != "blocked by policy" {
		t.Fatalf("deny result %+v", res)
	}

	// Invalid argument JSON fails the hook request.
	resp = route(t, r, wire.LifecycleHookPreTool, func(req *wire.CallHookRequest) {
		req.PreToolArgs = &wire.PreToolArgs{ArgumentsJSON: new("[1]")}
	})
	if !strings.HasPrefix(resp.GetErrorMessage(), "Hook failed") {
		t.Fatalf("invalid args response %+v", resp)
	}
}

func TestHookRouterStop(t *testing.T) {
	var got StopArgs
	var result StopHookResult
	var hookErr error
	r := newTestRouter(t, StopHook(func(_ context.Context, _ *HookContext, a StopArgs) (StopHookResult, error) {
		got = a
		return result, hookErr
	}))
	stop := func(sa *wire.StopArgs) *wire.CallHookResponse {
		return route(t, r, wire.LifecycleHookStop, func(req *wire.CallHookRequest) { req.StopArgs = sa })
	}
	result = StopHookResult{Decision: StopDecisionAllowStop}
	if d := stop(&wire.StopArgs{ResponseText: new("all done")}).GetStopResult().GetDecision(); d != wire.StopResultDecisionAllowStop {
		t.Fatalf("allow stop decision %s", d)
	}
	result = StopHookResult{Decision: StopDecisionContinue, Reason: "Keep working on the task"}
	resp := stop(&wire.StopArgs{
		ResponseText: new("partial answer"), TrajectoryID: new("traj-2"), ContinuationCount: new(int32(1)),
		StopReason: new(wire.TrajectoryStateUpdateStopReasonMaxModelCallsExceeded), ErrorMessage: new("stopped due to error"),
	})
	want := StopArgs{ResponseText: "partial answer", TrajectoryID: "traj-2", ContinuationCount: 1, StopReason: StopReasonMaxModelCallsExceeded, ErrorMessage: "stopped due to error"}
	if got != want || resp.GetStopResult().GetDecision() != wire.StopResultDecisionContinue || resp.GetStopResult().GetReason() != "Keep working on the task" {
		t.Fatalf("args %+v, response %+v", got, resp)
	}
	result = StopHookResult{}
	if d := stop(nil).GetStopResult().GetDecision(); d != wire.StopResultDecisionAllowStop || got != (StopArgs{StopReason: StopReasonUnspecified}) {
		t.Fatalf("no-args decision %s, args %+v", d, got)
	}
	result = StopHookResult{Decision: StopDecisionContinue}
	if !strings.Contains(stop(nil).GetErrorMessage(), "non-empty reason") {
		t.Fatal("CONTINUE without reason accepted")
	}
	hookErr = errors.New("boom")
	if !strings.Contains(stop(nil).GetErrorMessage(), "boom") {
		t.Fatal("hook error not reported")
	}
}

func TestTurnContextSharedWithinTurn(t *testing.T) {
	var mu sync.Mutex
	var seen []*HookContext
	add := func(hc *HookContext) {
		mu.Lock()
		seen = append(seen, hc)
		mu.Unlock()
	}
	c, tr := newTestConnection(t, connectionOptions{hooks: mustHooks(t,
		PreTurnHook(func(_ context.Context, hc *HookContext, _ []Content) (HookResult, error) {
			add(hc)
			return HookResult{}, nil
		}),
		PostTurnHook(func(_ context.Context, hc *HookContext, _ string) error { add(hc); return nil }),
	)})
	turn := func() {
		if err := c.Send(t.Context(), Text("q")); err != nil {
			t.Fatal(err)
		}
		tr.next(t)
		tr.emit(hookRequest("pre", wire.LifecycleHookPreTurn, nil))
		tr.next(t)
		tr.emit(hookRequest("post", wire.LifecycleHookPostTurn, nil))
		tr.next(t)
		tr.emit(idleEvent("t", ""))
		waitFor(t, "idle", c.IsIdle)
	}
	turn()
	// A turn the harness starts by itself gets a context of its own.
	tr.emit(hookRequest("pre", wire.LifecycleHookPreTurn, nil))
	tr.next(t)
	turn()
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 5 || seen[0] != seen[1] || seen[3] != seen[4] || seen[1] == seen[2] || seen[2] == seen[3] || seen[0].Scope() != ScopeTurn {
		t.Fatalf("turn contexts %p", seen)
	}
}

func TestHookRunnerPreTurn(t *testing.T) {
	r := mustHooks(t, PreTurnHook(func(context.Context, *HookContext, []Content) (HookResult, error) {
		return HookResult{Deny: true, Message: "no"}, nil
	}))
	res, err := r.dispatchPreTurn(t.Context(), r.newTurnContext(), nil)
	if err != nil || !res.Deny || res.Message != "no" {
		t.Fatalf("pre turn %+v %v", res, err)
	}
	if res, _ := mustHooks(t).dispatchPreTurn(t.Context(), r.newTurnContext(), []Content{Text("x"), &Image{}}); res.Deny {
		t.Fatal("no hooks denied")
	}
}

func TestHookRunnerPreToolCallChaining(t *testing.T) {
	var seen []map[string]any
	modify := func(k, v string) Hook {
		return PreToolCallHook(func(_ context.Context, _ *HookContext, call *ToolCall) (HookResult, error) {
			seen = append(seen, call.Args)
			return HookResult{ModifiedArgs: map[string]any{k: v}}, nil
		})
	}
	allow := PreToolCallHook(func(_ context.Context, _ *HookContext, call *ToolCall) (HookResult, error) {
		seen = append(seen, call.Args)
		return HookResult{}, nil
	})
	r := mustHooks(t, modify("a", "1"), modify("b", "2"), allow)
	call := &ToolCall{Name: "t", Args: map[string]any{"a": "0", "c": "3"}}
	res, err := r.dispatchPreToolCall(t.Context(), r.newTurnContext(), call)
	if err != nil || res.Deny {
		t.Fatalf("dispatch %+v %v", res, err)
	}
	want := map[string]any{"a": "1", "b": "2", "c": "3"}
	if !reflect.DeepEqual(res.ModifiedArgs, want) || !reflect.DeepEqual(call.Args, map[string]any{"a": "0", "c": "3"}) {
		t.Fatalf("merged %v, original %v", res.ModifiedArgs, call.Args)
	}
	if !reflect.DeepEqual(seen[1], map[string]any{"a": "1", "c": "3"}) || !reflect.DeepEqual(seen[2], want) {
		t.Fatalf("hooks saw %v", seen)
	}

	// A denial stops the chain and carries the arguments modified so far.
	deny := PreToolCallHook(func(context.Context, *HookContext, *ToolCall) (HookResult, error) {
		return HookResult{Deny: true, Message: "denied"}, nil
	})
	called := false
	after := PreToolCallHook(func(context.Context, *HookContext, *ToolCall) (HookResult, error) {
		called = true
		return HookResult{}, nil
	})
	r = mustHooks(t, modify("a", "1"), deny, after)
	res, _ = r.dispatchPreToolCall(t.Context(), r.newTurnContext(), &ToolCall{Name: "t"})
	if !res.Deny || res.Message != "denied" || called {
		t.Fatalf("deny chain %+v called=%v", res, called)
	}

	// Without modifications the result carries no arguments.
	r = mustHooks(t, allow)
	if res, _ := r.dispatchPreToolCall(t.Context(), r.newTurnContext(), &ToolCall{Name: "t"}); res.ModifiedArgs != nil {
		t.Fatalf("unmodified %+v", res)
	}

	// Errors and panics propagate.
	r = mustHooks(t, PreToolCallHook(func(context.Context, *HookContext, *ToolCall) (HookResult, error) { panic("oops") }))
	if _, err := r.dispatchPreToolCall(t.Context(), r.newTurnContext(), &ToolCall{}); err == nil || !strings.Contains(err.Error(), "oops") {
		t.Fatalf("panic err %v", err)
	}
}

func TestHookRunnerToolErrorAndInteraction(t *testing.T) {
	none := OnToolErrorHook(func(context.Context, *HookContext, *ToolExecutionError) (string, error) { return "", nil })
	recovering := OnToolErrorHook(func(context.Context, *HookContext, *ToolExecutionError) (string, error) { return "fallback", nil })
	failing := OnToolErrorHook(func(context.Context, *HookContext, *ToolExecutionError) (string, error) {
		return "", errors.New("bad")
	})
	op := newHookContext(ScopeOperation, nil)
	r := mustHooks(t, none, recovering)
	if msg := r.dispatchToolError(t.Context(), op, &ToolExecutionError{}); msg != "fallback" {
		t.Fatalf("recovery %q", msg)
	}
	r = mustHooks(t, failing, recovering)
	if msg := r.dispatchToolError(t.Context(), op, &ToolExecutionError{}); msg != "" {
		t.Fatalf("failing %q", msg)
	}
	r = mustHooks(t, none)
	if msg := r.dispatchToolError(t.Context(), op, &ToolExecutionError{}); msg != "" {
		t.Fatalf("fall through %q", msg)
	}

	skip := OnInteractionHook(func(context.Context, *HookContext, AskQuestionInteractionSpec) (*QuestionHookResult, error) {
		return nil, nil
	})
	answer := OnInteractionHook(func(context.Context, *HookContext, AskQuestionInteractionSpec) (*QuestionHookResult, error) {
		return &QuestionHookResult{Responses: []QuestionResponse{{Skipped: true}}}, nil
	})
	r = mustHooks(t, skip, answer)
	if q, err := r.dispatchInteraction(t.Context(), r.newTurnContext(), AskQuestionInteractionSpec{}); err != nil || q == nil || !q.Responses[0].Skipped {
		t.Fatalf("interaction %+v %v", q, err)
	}
	r = mustHooks(t, skip)
	if q, err := r.dispatchInteraction(t.Context(), r.newTurnContext(), AskQuestionInteractionSpec{}); err != nil || q != nil {
		t.Fatalf("unhandled interaction %+v %v", q, err)
	}
}

func TestHookRunnerStop(t *testing.T) {
	calls := 0
	allow := StopHook(func(context.Context, *HookContext, StopArgs) (StopHookResult, error) {
		calls++
		return StopHookResult{}, nil
	})
	cont := StopHook(func(context.Context, *HookContext, StopArgs) (StopHookResult, error) {
		calls++
		return StopHookResult{Decision: StopDecisionContinue, Reason: "more"}, nil
	})
	r := mustHooks(t, allow, cont, cont)
	res, err := r.dispatchStop(t.Context(), r.newTurnContext(), StopArgs{})
	if err != nil || res.Decision != StopDecisionContinue || calls != 2 {
		t.Fatalf("stop %+v %v calls=%d", res, err, calls)
	}
	if res, _ := mustHooks(t).dispatchStop(t.Context(), r.newTurnContext(), StopArgs{}); res.Decision != StopDecisionAllowStop {
		t.Fatalf("no hooks %+v", res)
	}
}

func TestHookRunnerRegistration(t *testing.T) {
	r := mustHooks(t,
		OnSessionStartHook(func(context.Context, *HookContext) error { return nil }),
		OnCompactionHook(func(context.Context, *HookContext, *Step) error { return nil }),
		StopHook(func(context.Context, *HookContext, StopArgs) (StopHookResult, error) { return StopHookResult{}, nil }),
	)
	if len(r.hooks[hookSessionStart]) != 1 || len(r.hooks[hookCompaction]) != 1 || len(r.hooks[hookStop]) != 1 || r.has(hookPreTurn) {
		t.Fatalf("runner %+v", r)
	}
	got := enabledHooks(r)
	want := []wire.LifecycleHook{wire.LifecycleHookOnSessionStart, wire.LifecycleHookOnCompaction, wire.LifecycleHookStop}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("enabled hooks %v", got)
	}
	if enabledHooks(mustHooks(t)) != nil {
		t.Fatal("empty runner enables hooks")
	}
	if _, err := newHookRunner([]Hook{PreTurnHook(nil)}); err == nil {
		t.Fatal("nil hook accepted")
	}
	if _, err := newHookRunner([]Hook{nil}); err == nil {
		t.Fatal("nil interface accepted")
	}
}

func TestHookContextScoping(t *testing.T) {
	r := mustHooks(t)
	r.session.SetState("s", 1)
	turn := r.newTurnContext()
	turn.SetState("t", 2)
	op := newHookContext(ScopeOperation, turn)
	op.SetState("s", 3)
	if v, _ := op.GetState("t"); v != 2 {
		t.Fatalf("op sees turn state %v", v)
	}
	if v, _ := op.GetState("s"); v != 3 {
		t.Fatalf("op shadows session state %v", v)
	}
	if v, _ := r.session.GetState("s"); v != 1 {
		t.Fatalf("session state modified: %v", v)
	}
	if _, ok := r.session.GetState("t"); ok {
		t.Fatal("session sees turn state")
	}
	if op.Parent() != turn || turn.Parent() != r.session || r.session.Parent() != nil {
		t.Fatal("parents")
	}
}
