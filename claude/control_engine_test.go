package claude

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// respondBySubtype answers every control request the SDK sends: with the
// payload registered for its subtype, else with an empty success. A nil
// payload entry suppresses the answer.
func respondBySubtype(ft *fakeTransport, answers map[string]map[string]any) {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	ft.onWrite = func(frame map[string]any) {
		if frame["type"] != "control_request" {
			return
		}
		subtype := frame["request"].(map[string]any)["subtype"].(string)
		payload, ok := answers[subtype]
		if ok && payload == nil {
			return
		}
		if !ok {
			payload = map[string]any{}
		}
		ft.push(map[string]any{
			"type": "control_response",
			"response": map[string]any{
				"subtype":    "success",
				"request_id": frame["request_id"],
				"response":   payload,
			},
		})
	}
}

func errorResult(text string) map[string]any {
	return map[string]any{"type": "result", "subtype": "error_during_execution", "duration_ms": 1,
		"duration_api_ms": 1, "is_error": true, "num_turns": 1, "session_id": "s1", "errors": []any{text}}
}

func TestEngineErrorResultSurvivesSkippedFrames(t *testing.T) {
	t.Parallel()
	modelLess := map[string]any{"type": "result", "subtype": "success", "duration_ms": 1,
		"duration_api_ms": 1, "is_error": false, "num_turns": 0, "session_id": "s1", "result": ""}
	tests := []struct {
		name       string
		after      []map[string]any
		wantResult bool
	}{
		{"keep_alive", []map[string]any{{"type": "keep_alive"}}, true},
		{"summary frames", []map[string]any{
			{"type": "system", "subtype": "post_turn_summary"},
			{"type": "active_goal"},
			{"type": "autocompact_state"},
		}, true},
		{"session state", []map[string]any{{"type": "system", "subtype": "session_state_changed", "state": "idle"}}, true},
		{"model-less turn", []map[string]any{{"type": "system", "subtype": "init"}, modelLess}, true},
		{"assistant moves on", []map[string]any{assistantFrame("next")}, false},
		{"commands_changed moves on", []map[string]any{{"type": "system", "subtype": "commands_changed", "commands": []any{}}}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			eng, ft := startEngine(t, nil)
			ft.push(errorResult("boom"))
			for _, f := range tc.after {
				ft.push(f)
			}
			code := 1
			ft.finish(NewProcessError("Command failed", &code, ""))
			var last error
			for _, err := range eng.receive(context.Background()) {
				if err != nil {
					last = err
				}
			}
			var resErr *ResultError
			if got := errors.As(last, &resErr); got != tc.wantResult {
				t.Fatalf("error = %T (%v), want ResultError: %v", last, last, tc.wantResult)
			}
		})
	}
}

func TestEngineDuplicateInboundRequestRunsOnce(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	release := make(chan struct{})
	opts := &Options{CanUseTool: func(context.Context, string, map[string]any, ToolPermissionContext) (PermissionResult, error) {
		calls.Add(1)
		<-release
		return &PermissionResultAllow{}, nil
	}}
	_, ft := startEngine(t, opts)
	req := map[string]any{"type": "control_request", "request_id": "dup",
		"request": map[string]any{"subtype": "can_use_tool", "tool_name": "Read", "tool_use_id": "tu"}}
	ft.push(req)
	ft.push(req)
	time.Sleep(100 * time.Millisecond)
	close(release)
	resp := ft.nextResponse(t)
	if resp["request_id"] != "dup" {
		t.Fatalf("response = %#v", resp)
	}
	select {
	case raw := <-ft.writeCh:
		t.Fatalf("second reply written: %s", raw)
	case <-time.After(100 * time.Millisecond):
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("handler ran %d times", n)
	}
}

func TestEngineCancelledRequestSendsControlCancel(t *testing.T) {
	t.Parallel()
	eng, ft := startEngine(t, nil)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := eng.request(ctx, "reload_plugins", nil)
		done <- err
	}()
	req := ft.nextWrite(t)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
	cancelFrame := ft.nextWrite(t)
	if cancelFrame["type"] != "control_cancel_request" || cancelFrame["request_id"] != req["request_id"] || len(cancelFrame) != 2 {
		t.Fatalf("cancel frame = %#v, request = %#v", cancelFrame, req)
	}
	eng.mu.Lock()
	pending := len(eng.pending)
	eng.mu.Unlock()
	if pending != 0 {
		t.Fatalf("pending = %d", pending)
	}
}

func TestEngineNoDefaultControlTimeout(t *testing.T) {
	t.Parallel()
	eng, ft := startEngine(t, nil)
	done := make(chan error, 1)
	go func() {
		_, err := eng.request(t.Context(), "get_usage", nil)
		done <- err
	}()
	req := ft.nextWrite(t)
	select {
	case err := <-done:
		t.Fatalf("request ended early: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	ft.push(map[string]any{"type": "control_response", "response": map[string]any{
		"subtype": "success", "request_id": req["request_id"], "response": map[string]any{"a": 1}}})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestEngineSessionStateFrames(t *testing.T) {
	t.Parallel()
	eng, ft := startEngine(t, nil)
	ft.push(map[string]any{"type": "system", "subtype": "session_state_changed", "state": "running", "sdk_host_only": true})
	ft.push(map[string]any{"type": "system", "subtype": "session_state_changed", "state": "idle"})
	ft.push(map[string]any{"type": "keep_alive"})
	ft.finish(nil)
	var states []string
	for msg, err := range eng.receive(context.Background()) {
		if err != nil {
			t.Fatal(err)
		}
		if sm, ok := msg.(interface{ isMessage() }); ok && sm != nil {
			states = append(states, "msg")
		}
	}
	// Only the caller-visible idle frame is yielded.
	if len(states) != 1 {
		t.Fatalf("yielded %d messages, want 1", len(states))
	}
}

// streamOne runs StreamInput with one prompt in the background and returns a
// channel closed when it returns.
func streamOne(t *testing.T, eng *engine) <-chan struct{} {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = eng.streamInput(t.Context(), rawInputs(map[string]any{"type": "user", "message": map[string]any{"content": "hi"}}))
	}()
	return done
}

func expectOpen(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
		t.Fatal("input closed too early")
	case <-time.After(100 * time.Millisecond):
	}
}

func expectClosed(t *testing.T, done <-chan struct{}, ft *fakeTransport) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("input was never closed")
	}
	if !ft.endedInput() {
		t.Fatal("EndInput was not called")
	}
}

func allowAll(context.Context, string, map[string]any, ToolPermissionContext) (PermissionResult, error) {
	return &PermissionResultAllow{}, nil
}

func stateFrame(state string) map[string]any {
	return map[string]any{"type": "system", "subtype": "session_state_changed", "state": state}
}

func TestEngineRunEndWaitsForIdle(t *testing.T) {
	t.Parallel()
	eng, ft := startEngine(t, &Options{CanUseTool: allowAll})
	done := streamOne(t, eng)
	ft.nextWrite(t) // the prompt
	ft.push(stateFrame("running"))
	ft.push(resultFrame())
	// A result while the CLI still reports work ends one turn, not the run.
	expectOpen(t, done)
	ft.push(stateFrame("idle"))
	expectClosed(t, done, ft)
}

func TestEngineRunEndCeiling(t *testing.T) {
	t.Parallel()
	opts := &Options{CanUseTool: allowAll, Env: map[string]string{"CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS": "200"}}
	eng, ft := startEngine(t, opts)
	done := streamOne(t, eng)
	ft.nextWrite(t)
	ft.push(stateFrame("running"))
	ft.push(resultFrame())
	// requires_action clears the ceiling while this host answers a request.
	ft.push(stateFrame("requires_action"))
	select {
	case <-done:
		t.Fatal("ceiling fired during requires_action")
	case <-time.After(400 * time.Millisecond):
	}
	// Back to running: the ceiling is re-armed and ends the run.
	ft.push(stateFrame("running"))
	expectClosed(t, done, ft)
}

func TestRunEndCeilingParsing(t *testing.T) {
	t.Parallel()
	tests := []struct {
		env  map[string]string
		want time.Duration
	}{
		{map[string]string{"CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS": "1500"}, 1500 * time.Millisecond},
		{map[string]string{"CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS": "0"}, 0},
		{map[string]string{"CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS": "-3"}, defaultRunEndCeiling},
		{map[string]string{"CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS": "soon"}, defaultRunEndCeiling},
		{map[string]string{"CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS": "99999999999999"}, maxRunEndCeiling},
	}
	for _, tc := range tests {
		if got := runEndCeiling(tc.env); got != tc.want {
			t.Errorf("runEndCeiling(%v) = %v, want %v", tc.env, got, tc.want)
		}
	}
}

func TestEngineInboundRequests(t *testing.T) {
	t.Parallel()
	type want struct {
		reply    bool
		subtype  string
		response map[string]any
	}
	tests := []struct {
		name    string
		opts    *Options
		request map[string]any
		want    want
	}{
		{
			name:    "elicitation without handler declines",
			opts:    &Options{},
			request: map[string]any{"subtype": "elicitation", "mcp_server_name": "s", "message": "m"},
			want:    want{true, "success", map[string]any{"action": "decline"}},
		},
		{
			name: "elicitation handler",
			opts: &Options{OnElicitation: func(_ context.Context, req ElicitationRequest) (*ElicitationResult, error) {
				if req.ServerName != "s" || req.Mode != ElicitationModeForm || req.RequestID != "r1" ||
					req.RequestedSchema["type"] != "object" || req.Title != "T" || req.DisplayName != "D" {
					return nil, errors.New("bad request mapping")
				}
				return &ElicitationResult{Action: ElicitationAccept, Content: map[string]any{"name": "x"}}, nil
			}},
			request: map[string]any{"subtype": "elicitation", "mcp_server_name": "s", "message": "m",
				"mode": "form", "requested_schema": map[string]any{"type": "object"}, "title": "T", "display_name": "D"},
			want: want{true, "success", map[string]any{"action": "accept", "content": map[string]any{"name": "x"}}},
		},
		{
			name: "elicitation out of band",
			opts: &Options{OnElicitation: func(context.Context, ElicitationRequest) (*ElicitationResult, error) {
				return nil, ErrRespondedOutOfBand
			}},
			request: map[string]any{"subtype": "elicitation", "mcp_server_name": "s", "message": "m"},
			want:    want{reply: false},
		},
		{
			name:    "dialog without handler stays silent",
			opts:    &Options{},
			request: map[string]any{"subtype": "request_user_dialog", "dialog_kind": "k", "payload": map[string]any{}},
			want:    want{reply: false},
		},
		{
			name: "dialog completed",
			opts: &Options{SupportedDialogKinds: []string{"k"}, OnUserDialog: func(_ context.Context, req UserDialogRequest) (*UserDialogResult, error) {
				return &UserDialogResult{Result: map[string]any{"choice": req.Payload["n"], "tool": req.ToolUseID}}, nil
			}},
			request: map[string]any{"subtype": "request_user_dialog", "dialog_kind": "k",
				"payload": map[string]any{"n": float64(2)}, "tool_use_id": "tu"},
			want: want{true, "success", map[string]any{"behavior": "completed",
				"result": map[string]any{"choice": float64(2), "tool": "tu"}}},
		},
		{
			name: "dialog cancelled",
			opts: &Options{OnUserDialog: func(context.Context, UserDialogRequest) (*UserDialogResult, error) {
				return &UserDialogResult{Behavior: UserDialogCancelled}, nil
			}},
			request: map[string]any{"subtype": "request_user_dialog", "dialog_kind": "k"},
			want:    want{true, "success", map[string]any{"behavior": "cancelled"}},
		},
		{
			name: "undeclared dialog kind stays silent",
			opts: &Options{SupportedDialogKinds: []string{"other"}, OnUserDialog: func(context.Context, UserDialogRequest) (*UserDialogResult, error) {
				return &UserDialogResult{Behavior: UserDialogCancelled}, nil
			}},
			request: map[string]any{"subtype": "request_user_dialog", "dialog_kind": "k"},
			want:    want{reply: false},
		},
		{
			name:    "remote tool call is left for its machine",
			opts:    &Options{},
			request: map[string]any{"subtype": "remote_tool_call"},
			want:    want{reply: false},
		},
		{
			name:    "remote tools probe is left for its machine",
			opts:    &Options{},
			request: map[string]any{"subtype": "remote_tools_reannounce"},
			want:    want{reply: false},
		},
		{
			name:    "unknown subtype errors",
			opts:    &Options{},
			request: map[string]any{"subtype": "oauth_token_refresh"},
			want:    want{reply: true, subtype: "error"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, ft := startEngine(t, tc.opts)
			ft.push(map[string]any{"type": "control_request", "request_id": "r1", "request": tc.request})
			if !tc.want.reply {
				select {
				case raw := <-ft.writeCh:
					t.Fatalf("unexpected reply %s", raw)
				case <-time.After(150 * time.Millisecond):
				}
				return
			}
			resp := ft.nextResponse(t)
			if resp["subtype"] != tc.want.subtype || resp["request_id"] != "r1" {
				t.Fatalf("response = %#v", resp)
			}
			if tc.want.response != nil {
				assertJSONEqual(t, resp["response"], tc.want.response)
			}
		})
	}
}

func TestEngineCanUseToolContextAndReply(t *testing.T) {
	t.Parallel()
	got := make(chan ToolPermissionContext, 1)
	opts := &Options{CanUseTool: func(_ context.Context, _ string, _ map[string]any, pc ToolPermissionContext) (PermissionResult, error) {
		got <- pc
		if pc.ToolUseID == "oob" {
			return nil, ErrRespondedOutOfBand
		}
		return &PermissionResultDeny{Message: "no", DecisionClassification: DecisionUserReject}, nil
	}}
	_, ft := startEngine(t, opts)
	ft.push(map[string]any{"type": "control_request", "request_id": "r9", "request": map[string]any{
		"subtype": "can_use_tool", "tool_name": "mcp__fs__read", "input": map[string]any{},
		"tool_use_id": "tu1", "mcp_server": map[string]any{"name": "fs", "source": "sdk"},
		"default_to_no": true, "suppress_always_allow_rule": true, "requires_user_interaction": false,
		"matched_ask_rule":       map[string]any{"source": "userSettings", "tool_name": "Bash", "rule_content": "rm:*"},
		"decision_reason_type":   "rule",
		"permission_suggestions": []any{map[string]any{"type": "addRules", "rules": []any{map[string]any{"toolName": "Read"}}, "behavior": "allow", "destination": "cliArg"}},
	}})
	pc := <-got
	if pc.MCPServer == nil || pc.MCPServer.Name != "fs" || pc.MCPServer.Source != "sdk" {
		t.Fatalf("mcp server = %#v", pc.MCPServer)
	}
	if !pc.DefaultToNo || !pc.SuppressAlwaysAllowRule || pc.RequestID != "r9" ||
		pc.RequiresUserInteraction == nil || *pc.RequiresUserInteraction {
		t.Fatalf("context = %#v", pc)
	}
	if r := pc.MatchedAskRule; r == nil || r.Source != "userSettings" || r.ToolName != "Bash" || r.RuleContent == nil || *r.RuleContent != "rm:*" {
		t.Fatalf("matched ask rule = %#v", pc.MatchedAskRule)
	}
	if pc.Raw["decision_reason_type"] != "rule" || len(pc.Suggestions) != 1 || pc.Suggestions[0].Destination != DestinationCLIArg {
		t.Fatalf("raw/suggestions = %#v %#v", pc.Raw, pc.Suggestions)
	}
	resp := ft.nextResponse(t)
	assertJSONEqual(t, resp["response"], map[string]any{
		"behavior": "deny", "message": "no", "toolUseID": "tu1", "decisionClassification": "user_reject"})

	// An out-of-band answer writes nothing.
	ft.push(map[string]any{"type": "control_request", "request_id": "r10", "request": map[string]any{
		"subtype": "can_use_tool", "tool_name": "Read", "input": map[string]any{}, "tool_use_id": "oob"}})
	<-got
	select {
	case raw := <-ft.writeCh:
		t.Fatalf("unexpected reply %s", raw)
	case <-time.After(150 * time.Millisecond):
	}
}

func TestEngineInitializeRequestFields(t *testing.T) {
	t.Parallel()
	opts := &Options{
		OnUserDialog:         func(context.Context, UserDialogRequest) (*UserDialogResult, error) { return nil, nil },
		SupportedDialogKinds: []string{"refusal_fallback_prompt"},
	}
	eng, ft := startEngine(t, opts)
	respondBySubtype(ft, nil)
	if _, err := eng.initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	req := ft.frames(t)[0]["request"].(map[string]any)
	if _, ok := req["hooks"]; ok {
		t.Fatalf("hooks sent without hooks: %#v", req)
	}
	assertJSONEqual(t, req["supportedDialogKinds"], []any{"refusal_fallback_prompt"})
}

func TestEngineInitializeResultAndRedelivery(t *testing.T) {
	t.Parallel()
	asked := make(chan string, 4)
	hook := func(context.Context, map[string]any, string, HookContext) (HookOutput, error) {
		return HookOutput{}, nil
	}
	opts := &Options{
		CanUseTool: func(_ context.Context, _ string, _ map[string]any, pc ToolPermissionContext) (PermissionResult, error) {
			asked <- pc.RequestID
			return &PermissionResultAllow{}, nil
		},
		Hooks: map[HookEvent][]HookMatcher{HookPreToolUse: {{Hooks: []HookCallback{hook}}}},
	}
	eng, ft := startEngine(t, opts)
	pending := []any{
		map[string]any{"type": "control_request", "request_id": "p1",
			"request": map[string]any{"subtype": "can_use_tool", "tool_name": "Read", "tool_use_id": "t"}},
		// Only can_use_tool entries count on this list.
		map[string]any{"type": "control_request", "request_id": "p2",
			"request": map[string]any{"subtype": "request_user_dialog", "dialog_kind": "k"}},
	}
	ft.mu.Lock()
	ft.onWrite = func(frame map[string]any) {
		if frame["type"] != "control_request" {
			return
		}
		ft.push(map[string]any{"type": "control_response", "response": map[string]any{
			"subtype": "success", "request_id": frame["request_id"],
			"pending_permission_requests": pending,
			"response": map[string]any{
				"commands":                []any{map[string]any{"name": "help", "description": "d", "argumentHint": "", "builtin": true}},
				"agents":                  []any{map[string]any{"name": "Explore", "description": "e"}},
				"models":                  []any{map[string]any{"value": "opus", "displayName": "Opus", "description": "o", "supportedEffortLevels": []any{"high"}}},
				"account":                 map[string]any{"email": "a@b.c", "apiProvider": "firstParty"},
				"output_style":            "default",
				"available_output_styles": []any{"default"},
				"hooks_applied":           true,
			},
		}})
	}
	ft.mu.Unlock()

	if _, err := eng.initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	if id := <-asked; id != "p1" {
		t.Fatalf("redelivered %q", id)
	}
	if resp := ft.nextResponse(t); resp["request_id"] != "p1" {
		t.Fatalf("response = %#v", resp)
	}
	res := eng.initializeResult()
	if res == nil || len(res.Commands) != 1 || !res.Commands[0].Builtin || res.Agents[0].Name != "Explore" ||
		res.Models[0].SupportedEffortLevels[0] != EffortHigh || res.Account.APIProvider != "firstParty" ||
		res.HooksApplied == nil || !*res.HooksApplied || res.Raw["output_style"] != "default" {
		t.Fatalf("result = %#v", res)
	}

	// Re-initializing sends the same hook registration.
	if _, err := eng.initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	var hookIDs []any
	for _, f := range ft.frames(t) {
		if req, ok := f["request"].(map[string]any); ok && req["subtype"] == "initialize" {
			hookIDs = append(hookIDs, req["hooks"].(map[string]any)[HookPreToolUse].([]any)[0].(map[string]any)["hookCallbackIds"])
		}
	}
	if len(hookIDs) != 2 {
		t.Fatalf("initialize frames = %d", len(hookIDs))
	}
	assertJSONEqual(t, hookIDs[0], hookIDs[1])

	// commands_changed replaces the command list.
	ft.push(map[string]any{"type": "system", "subtype": "commands_changed",
		"commands": []any{map[string]any{"name": "new", "description": "", "argumentHint": ""}}})
	deadline := time.Now().Add(5 * time.Second)
	for {
		cmds := eng.supportedCommands()
		if len(cmds) == 1 && cmds[0].Name == "new" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("commands = %#v", cmds)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestEnginePendingIgnoredOnOtherResponses(t *testing.T) {
	t.Parallel()
	asked := make(chan struct{}, 1)
	opts := &Options{CanUseTool: func(context.Context, string, map[string]any, ToolPermissionContext) (PermissionResult, error) {
		asked <- struct{}{}
		return &PermissionResultAllow{}, nil
	}}
	eng, ft := startEngine(t, opts)
	ft.mu.Lock()
	ft.onWrite = func(frame map[string]any) {
		if frame["type"] != "control_request" {
			return
		}
		ft.push(map[string]any{"type": "control_response", "response": map[string]any{
			"subtype": "success", "request_id": frame["request_id"], "response": map[string]any{},
			"pending_permission_requests": []any{map[string]any{"type": "control_request", "request_id": "p1",
				"request": map[string]any{"subtype": "can_use_tool", "tool_name": "Read"}}},
		}})
	}
	ft.mu.Unlock()
	if err := interrupt(t.Context(), eng); err != nil {
		t.Fatal(err)
	}
	select {
	case <-asked:
		t.Fatal("pending request redelivered from a non-initialize response")
	case <-time.After(150 * time.Millisecond):
	}
}

func TestValidateCallbackOptions(t *testing.T) {
	t.Parallel()
	if _, _, err := prepareOptions(&Options{SupportedDialogKinds: []string{"k"}}, entrypoint); err == nil {
		t.Fatal("SupportedDialogKinds without OnUserDialog should fail")
	}
	ok := &Options{SupportedDialogKinds: []string{"k"},
		OnUserDialog: func(context.Context, UserDialogRequest) (*UserDialogResult, error) { return nil, nil }}
	if _, _, err := prepareOptions(ok, entrypoint); err != nil {
		t.Fatal(err)
	}
}

// assertJSONEqual compares two values by their JSON encoding.
func assertJSONEqual(t *testing.T, got, want any) {
	t.Helper()
	g, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	w, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if string(g) != string(w) {
		t.Fatalf("got  %s\nwant %s", g, w)
	}
}
