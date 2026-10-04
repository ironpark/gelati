package claude

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestHookEventsMatchTypeScript(t *testing.T) {
	t.Parallel()
	// HOOK_EVENTS from sdk.d.ts, in order.
	want := strings.Fields(`PreToolUse PostToolUse PostToolUseFailure PostToolBatch
		Notification UserPromptSubmit UserPromptExpansion SessionStart SessionEnd
		Stop StopFailure SubagentStart SubagentStop PreCompact PostCompact
		PreModelSwitch PostModelSwitch PermissionRequest PermissionDenied Setup
		TeammateIdle TaskCreated TaskCompleted Elicitation ElicitationResult
		ConfigChange WorktreeCreate WorktreeRemove InstructionsLoaded CwdChanged
		FileChanged DirectoryAdded MessageDisplay`)
	if !reflect.DeepEqual(HookEvents, want) {
		t.Fatalf("HookEvents = %v\nwant %v", HookEvents, want)
	}
	// Every event decodes to its own typed input.
	for _, event := range HookEvents {
		in, err := DecodeHookInput(map[string]any{"hook_event_name": event})
		if err != nil {
			t.Fatalf("%s: %v", event, err)
		}
		if _, unknown := in.(*UnknownHookInput); unknown {
			t.Fatalf("%s decoded as unknown", event)
		}
		if in.Base().HookEventName != event {
			t.Fatalf("%s: base = %+v", event, in.Base())
		}
	}
}

func TestHookOutputWireFormatTS(t *testing.T) {
	t.Parallel()
	timeout := 100
	cases := []struct {
		name string
		out  HookOutput
		want string
	}{
		{"terminalSequence", HookOutput{TerminalSequence: "\x1b]0;done\x07"},
			`{"terminalSequence":"\u001b]0;done\u0007"}`},
		{"approve", HookOutput{Decision: HookDecisionApprove}, `{"decision":"approve"}`},
		{"extraMergedModeledWins",
			HookOutput{Reason: "r", Extra: map[string]any{"reason": "lost", "newKey": 1}},
			`{"reason":"r","newKey":1}`},
		{"asyncKeepsExtra",
			HookOutput{Async: true, AsyncTimeout: &timeout, Reason: "ignored", Extra: map[string]any{"x": true}},
			`{"async":true,"asyncTimeout":100,"x":true}`},
		{"typedPreToolUse",
			HookOutput{Specific: &PreToolUseHookSpecificOutput{
				PermissionDecision:       PermissionDecisionDefer,
				PermissionDecisionReason: "host decides",
				UpdatedInput:             map[string]any{"command": "ls"},
			}},
			`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"defer","permissionDecisionReason":"host decides","updatedInput":{"command":"ls"}}}`},
		{"typedOverMap",
			HookOutput{
				HookSpecificOutput: map[string]any{"hookEventName": "Stop", "additionalContext": "old", "custom": "kept"},
				Specific:           &SessionStartHookSpecificOutput{AdditionalContext: "new", WatchPaths: []string{"/a"}},
			},
			`{"hookSpecificOutput":{"additionalContext":"new","custom":"kept","hookEventName":"SessionStart","watchPaths":["/a"]}}`},
		{"typedNilPointer",
			HookOutput{HookSpecificOutput: map[string]any{"hookEventName": "Stop"}, Specific: (*StopHookSpecificOutput)(nil)},
			`{"hookSpecificOutput":{"hookEventName":"Stop"}}`},
		{"permissionRequestAllow",
			HookOutput{Specific: &PermissionRequestHookSpecificOutput{Decision: &PermissionResultAllow{
				UpdatedInput: map[string]any{"a": 1}}}},
			`{"hookSpecificOutput":{"decision":{"behavior":"allow","updatedInput":{"a":1}},"hookEventName":"PermissionRequest"}}`},
		{"permissionRequestDeny",
			HookOutput{Specific: &PermissionRequestHookSpecificOutput{Decision: &PermissionResultDeny{
				Message: "no", Interrupt: true}}},
			`{"hookSpecificOutput":{"decision":{"behavior":"deny","interrupt":true,"message":"no"},"hookEventName":"PermissionRequest"}}`},
		{"worktreePathRequired",
			HookOutput{Specific: &WorktreeCreateHookSpecificOutput{}},
			`{"hookSpecificOutput":{"hookEventName":"WorktreeCreate","worktreePath":""}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := json.Marshal(tc.out)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("marshal = %s\nwant      %s", got, tc.want)
			}
		})
	}
}

func TestHookOutputUnmarshalKeepsUnknownKeys(t *testing.T) {
	t.Parallel()
	var out HookOutput
	if err := json.Unmarshal([]byte(`{"decision":"block","terminalSequence":"\u0007","future":{"a":1}}`), &out); err != nil {
		t.Fatal(err)
	}
	if out.Decision != HookDecisionBlock || out.TerminalSequence != "\a" {
		t.Fatalf("out = %+v", out)
	}
	if !reflect.DeepEqual(out.Extra, map[string]any{"future": map[string]any{"a": 1.0}}) {
		t.Fatalf("extra = %#v", out.Extra)
	}
}

func TestHookOutputBadPermissionRequestDecision(t *testing.T) {
	t.Parallel()
	type otherResult struct{ PermissionResult }
	_, err := json.Marshal(HookOutput{Specific: &PermissionRequestHookSpecificOutput{Decision: otherResult{}}})
	if err == nil {
		t.Fatal("want an error for an unknown decision type")
	}
}

func TestDecodeHookInput(t *testing.T) {
	t.Parallel()
	base := func(event string, extra map[string]any) map[string]any {
		m := map[string]any{"hook_event_name": event, "session_id": "s", "transcript_path": "/t",
			"cwd": "/w", "permission_mode": "default", "effort": map[string]any{"level": "high"}}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	cases := []struct {
		name  string
		raw   map[string]any
		check func(t *testing.T, in HookInput)
	}{
		{"preToolUse", base("PreToolUse", map[string]any{"tool_name": "mcp__calc__add",
			"tool_input": map[string]any{"a": 1.0}, "tool_use_id": "tu1",
			"mcp_server": map[string]any{"name": "calc", "source": "sdk"}}),
			func(t *testing.T, in HookInput) {
				p := in.(*PreToolUseHookInput)
				if p.ToolName != "mcp__calc__add" || p.ToolUseID != "tu1" || p.ToolInput["a"] != 1.0 {
					t.Fatalf("input = %+v", p)
				}
				if p.MCPServer == nil || p.MCPServer.Source != "sdk" || p.MCPServer.Name != "calc" {
					t.Fatalf("mcp server = %+v", p.MCPServer)
				}
				if p.SessionID != "s" || p.Cwd != "/w" || p.Effort == nil || p.Effort.Level != "high" {
					t.Fatalf("base = %+v", p.BaseHookInput)
				}
			}},
		{"preModelSwitch", base("PreModelSwitch", map[string]any{"from_model": "a", "to_model": "b",
			"requested_model": nil, "source": "sdk", "context_tokens": 12.0, "prompt_cache_warm": true,
			"cache_ttl": "1h", "estimated_cache_write_usd": 0.5, "pricing": "catalog"}),
			func(t *testing.T, in HookInput) {
				p := in.(*PreModelSwitchHookInput)
				if p.FromModel != "a" || p.ToModel != "b" || p.RequestedModel != "" || p.ContextTokens != 12 ||
					!p.PromptCacheWarm || p.CacheTTL != "1h" || p.EstimatedCacheWriteUSD != 0.5 {
					t.Fatalf("input = %+v", p)
				}
			}},
		{"postToolBatch", base("PostToolBatch", map[string]any{"tool_calls": []any{
			map[string]any{"tool_name": "Read", "tool_input": map[string]any{}, "tool_use_id": "x", "tool_response": "ok"}}}),
			func(t *testing.T, in HookInput) {
				p := in.(*PostToolBatchHookInput)
				if len(p.ToolCalls) != 1 || p.ToolCalls[0].ToolResponse != "ok" {
					t.Fatalf("input = %+v", p)
				}
			}},
		{"stop", base("Stop", map[string]any{"stop_hook_active": true,
			"background_tasks": []any{map[string]any{"id": "b1", "type": "shell", "status": "running", "description": "d"}},
			"session_crons":    []any{map[string]any{"id": "c", "schedule": "* * * * *", "recurring": true, "prompt": "p"}}}),
			func(t *testing.T, in HookInput) {
				p := in.(*StopHookInput)
				if !p.StopHookActive || p.BackgroundTasks[0].ID != "b1" || !p.SessionCrons[0].Recurring {
					t.Fatalf("input = %+v", p)
				}
			}},
		{"subagentStart", base("SubagentStart", map[string]any{"agent_id": "a1", "agent_type": "Explore"}),
			func(t *testing.T, in HookInput) {
				p := in.(*SubagentStartHookInput)
				if p.AgentID != "a1" || p.AgentType != "Explore" {
					t.Fatalf("input = %+v", p)
				}
			}},
		{"sessionEnd", base("SessionEnd", map[string]any{"reason": "logout"}),
			func(t *testing.T, in HookInput) {
				if in.(*SessionEndHookInput).Reason != ExitReasonLogout {
					t.Fatalf("input = %+v", in)
				}
			}},
		{"unknownEvent", base("FutureEvent", map[string]any{"novel": 1.0}),
			func(t *testing.T, in HookInput) {
				u := in.(*UnknownHookInput)
				if u.HookEventName != "FutureEvent" || u.Raw["novel"] != 1.0 || u.SessionID != "s" {
					t.Fatalf("input = %+v", u)
				}
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in, err := DecodeHookInput(tc.raw)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			tc.check(t, in)
		})
	}

	if _, err := DecodeHookInput(base("PreToolUse", map[string]any{"tool_name": 7})); err == nil {
		t.Fatal("a mistyped field should fail to decode")
	}
}

func TestTypedHook(t *testing.T) {
	t.Parallel()
	raw := map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Bash", "extra_field": "x"}
	typed := TypedHook(func(_ context.Context, in *PreToolUseHookInput, toolUseID string, hc HookContext) (HookOutput, error) {
		if hc.Raw["extra_field"] != "x" {
			return HookOutput{}, errors.New("raw input not passed through")
		}
		return HookOutput{Reason: in.ToolName + "/" + toolUseID}, nil
	})
	cases := []struct {
		name    string
		cb      HookCallback
		input   map[string]any
		want    string
		wantErr string
	}{
		{"matching", typed, raw, "Bash/tu", ""},
		{"wrongEvent", typed, map[string]any{"hook_event_name": "Stop"}, "", `received a "Stop" event`},
		{"badInput", typed, map[string]any{"hook_event_name": "PreToolUse", "tool_name": 1}, "", "decoding PreToolUse"},
		{"anyEvent", TypedHook(func(_ context.Context, in HookInput, _ string, _ HookContext) (HookOutput, error) {
			return HookOutput{Reason: in.Base().HookEventName}, nil
		}), map[string]any{"hook_event_name": "Stop"}, "Stop", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, err := tc.cb(t.Context(), tc.input, "tu", HookContext{})
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || out.Reason != tc.want {
				t.Fatalf("out = %+v, err = %v", out, err)
			}
		})
	}
}

func TestEngineHookCallbackTypedAndVerbatim(t *testing.T) {
	t.Parallel()
	eng, ft := startEngine(t, nil)
	eng.cfg.hookCallbacks["hook_0"] = TypedHook(func(_ context.Context, in *PreToolUseHookInput, _ string, hc HookContext) (HookOutput, error) {
		return HookOutput{
			TerminalSequence: "\a",
			Specific:         &PreToolUseHookSpecificOutput{PermissionDecision: PermissionDecisionAsk},
			Extra:            map[string]any{"futureField": hc.Raw["tool_name"]},
		}, nil
	})
	ft.push(map[string]any{"type": "control_request", "request_id": "h1", "request": map[string]any{
		"subtype": "hook_callback", "callback_id": "hook_0", "tool_use_id": "tu",
		"input": map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Bash"}}})
	out := ft.nextResponse(t)
	got, _ := json.Marshal(out["response"])
	want := `{"futureField":"Bash","hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"ask"},"terminalSequence":"\u0007"}`
	if out["subtype"] != "success" || string(got) != want {
		t.Fatalf("response = %#v\n got %s\nwant %s", out, got, want)
	}
}
