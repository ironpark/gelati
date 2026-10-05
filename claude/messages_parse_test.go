package claude

import (
	"encoding/json/v2"
	"reflect"
	"testing"
)

// TestParseLenient checks that frames missing fields the TypeScript types mark
// as required (or carrying them with the wrong JSON type) still parse, as the
// TypeScript SDK never validates frames.
func TestParseLenient(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		line  string
		check func(t *testing.T, m Message)
	}{
		{"missingType", `{"foo":"bar"}`, func(t *testing.T, m Message) {
			if u, ok := m.(*UnknownMessage); !ok || u.Type != "" || u.Raw["foo"] != "bar" {
				t.Fatalf("got %#v, want *UnknownMessage", m)
			}
		}},
		{"userMissingMessage", `{"type":"user","uuid":"u1"}`, func(t *testing.T, m Message) {
			um := m.(*UserMessage)
			if um.UUID != "u1" || um.ContentText != nil || um.Content != nil {
				t.Fatalf("got %+v", um)
			}
		}},
		{"userBadBlockSkipped", `{"type":"user","message":{"content":["nope",{"type":"text","text":"ok"}]}}`, func(t *testing.T, m Message) {
			um := m.(*UserMessage)
			if len(um.Content) != 1 || um.Content[0].(*TextBlock).Text != "ok" {
				t.Fatalf("content = %#v", um.Content)
			}
		}},
		{"assistantMissingModel", `{"type":"assistant","message":{"content":[]}}`, func(t *testing.T, m Message) {
			am := m.(*AssistantMessage)
			if am.Model != "" || am.Content == nil || len(am.Content) != 0 {
				t.Fatalf("got %+v", am)
			}
		}},
		{"assistantStringContent", `{"type":"assistant","message":{"model":"m","content":"hi"}}`, func(t *testing.T, m Message) {
			am := m.(*AssistantMessage)
			if len(am.Content) != 1 || am.Content[0].(*TextBlock).Text != "hi" {
				t.Fatalf("content = %#v", am.Content)
			}
		}},
		{"assistantTextBlockMissingText", `{"type":"assistant","message":{"model":"m","content":[{"type":"text"}]}}`, func(t *testing.T, m Message) {
			if tb := m.(*AssistantMessage).Content[0].(*TextBlock); tb.Text != "" {
				t.Fatalf("text = %q", tb.Text)
			}
		}},
		{"systemMissingSubtype", `{"type":"system","x":1}`, func(t *testing.T, m Message) {
			if sm := m.(*SystemMessage); sm.Subtype != "" || sm.Data["x"] != float64(1) {
				t.Fatalf("got %+v", sm)
			}
		}},
		{"taskStartedMissingIDs", `{"type":"system","subtype":"task_started","description":"d"}`, func(t *testing.T, m Message) {
			if ts := m.(*TaskStartedMessage); ts.Description != "d" || ts.TaskID != "" {
				t.Fatalf("got %+v", ts)
			}
		}},
		{"taskProgressMissingUsage", `{"type":"system","subtype":"task_progress","task_id":"t1"}`, func(t *testing.T, m Message) {
			if tp := m.(*TaskProgressMessage); tp.TaskID != "t1" || tp.Usage != (TaskUsage{}) {
				t.Fatalf("got %+v", tp)
			}
		}},
		{"taskNotificationMissingOutput", `{"type":"system","subtype":"task_notification","task_id":"t1","status":"failed"}`, func(t *testing.T, m Message) {
			if tn := m.(*TaskNotificationMessage); tn.Status != "failed" || tn.OutputFile != "" {
				t.Fatalf("got %+v", tn)
			}
		}},
		{"wrongFieldType", `{"type":"system","subtype":"task_started","task_id":7,"description":"d","uuid":"u"}`, func(t *testing.T, m Message) {
			if ts := m.(*TaskStartedMessage); ts.TaskID != "" || ts.Description != "d" || ts.UUID != "u" {
				t.Fatalf("got %+v", ts)
			}
		}},
		{"resultMissingDurations", `{"type":"result","subtype":"success","is_error":false}`, func(t *testing.T, m Message) {
			rm := m.(*ResultMessage)
			if rm.Subtype != ResultSubtypeSuccess || rm.DurationMS != 0 || rm.SessionID != "" || rm.Data == nil {
				t.Fatalf("got %+v", rm)
			}
		}},
		{"resultPartialDeferred", `{"type":"result","subtype":"success","deferred_tool_use":{"id":"a","name":"b"}}`, func(t *testing.T, m Message) {
			d := m.(*ResultMessage).DeferredToolUse
			if d == nil || d.ID != "a" || d.Input != nil {
				t.Fatalf("deferred = %+v", d)
			}
		}},
		{"streamEventMissingEvent", `{"type":"stream_event","uuid":"u"}`, func(t *testing.T, m Message) {
			if se := m.(*StreamEvent); se.UUID != "u" || se.Event != nil {
				t.Fatalf("got %+v", se)
			}
		}},
		{"rateLimitMissingInfo", `{"type":"rate_limit_event","uuid":"u","session_id":"s"}`, func(t *testing.T, m Message) {
			if rl := m.(*RateLimitEvent); rl.UUID != "u" || rl.RateLimitInfo.Status != "" {
				t.Fatalf("got %+v", rl)
			}
		}},
		{"conversationResetMissingID", `{"type":"conversation_reset","uuid":"u"}`, func(t *testing.T, m Message) {
			if cr := m.(*ConversationResetMessage); cr.UUID != "u" || cr.NewConversationID != "" {
				t.Fatalf("got %+v", cr)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			msg, err := ParseMessage([]byte(tc.line))
			if err != nil {
				t.Fatalf("ParseMessage: %v", err)
			}
			tc.check(t, msg)
		})
	}
}

// TestParseSystemSubtypes checks one wire sample (shaped after sdk.d.ts) per
// typed system subtype.
func TestParseSystemSubtypes(t *testing.T) {
	t.Parallel()
	const env = `"uuid":"u1","session_id":"s1"`
	cases := []struct {
		subtype string
		body    string
		check   func(t *testing.T, m Message)
	}{
		{"init", `"agents":["general-purpose"],"apiKeySource":"none","betas":["b1"],
		  "claude_code_version":"2.1.0","cwd":"/w","tools":["Bash","Read"],
		  "mcp_servers":[{"name":"fs","status":"connected","source":"sdk"}],
		  "model":"claude-opus-4-7","permissionMode":"default","slash_commands":["clear","exit"],
		  "terminal_slash_commands":["exit"],"output_style":"default","skills":["pdf"],
		  "plugins":[{"name":"p","path":"/p","version":"1.0.0"}],
		  "plugin_errors":[{"plugin":"inline[0]","type":"path-not-found","message":"m","path":"/x"}],
		  "fast_mode_state":"off","fast_mode_disabled_reason":"free","effort":"high","view_mode":"focus",
		  "capabilities":["interrupt_receipt_v1","queued_notifications"]`,
			func(t *testing.T, m Message) {
				im := m.(*InitMessage)
				if im.Model != "claude-opus-4-7" || im.APIKeySource != APIKeySourceNone || im.ClaudeCodeVersion != "2.1.0" ||
					im.Cwd != "/w" || im.PermissionMode != PermissionModeDefault || im.OutputStyle != "default" ||
					im.FastModeState != FastModeOff || im.FastModeDisabledReason != FastModeDisabledFree ||
					im.Effort != "high" || im.ViewMode != "focus" {
					t.Fatalf("got %+v", im)
				}
				if !reflect.DeepEqual(im.Tools, []string{"Bash", "Read"}) || !reflect.DeepEqual(im.TerminalSlashCommands, []string{"exit"}) ||
					!reflect.DeepEqual(im.Agents, []string{"general-purpose"}) || !reflect.DeepEqual(im.Skills, []string{"pdf"}) ||
					!reflect.DeepEqual(im.Betas, []string{"b1"}) || len(im.SlashCommands) != 2 {
					t.Fatalf("lists = %+v", im)
				}
				if im.MCPServers[0] != (InitMCPServer{Name: "fs", Status: "connected", Source: "sdk"}) {
					t.Fatalf("mcp = %+v", im.MCPServers)
				}
				if im.Plugins[0] != (InitPlugin{Name: "p", Path: "/p", Version: "1.0.0"}) ||
					im.PluginErrors[0] != (InitPluginError{Plugin: "inline[0]", Type: "path-not-found", Message: "m", Path: "/x"}) {
					t.Fatalf("plugins = %+v / %+v", im.Plugins, im.PluginErrors)
				}
				if !im.HasCapability(CapabilityInterruptReceiptV1) || im.HasCapability(CapabilityInterruptCancelQueuedV1) {
					t.Fatalf("capabilities = %v", im.Capabilities)
				}
			}},
		{"compact_boundary", `"compact_metadata":{"trigger":"auto","pre_tokens":1000,"post_tokens":200,"duration_ms":30,
		  "preserved_segment":{"head_uuid":"h","anchor_uuid":"a","tail_uuid":"t"},
		  "preserved_messages":{"anchor_uuid":"a","uuids":["x","y"]}}`,
			func(t *testing.T, m Message) {
				md := m.(*CompactBoundaryMessage).CompactMetadata
				if md.Trigger != "auto" || md.PreTokens != 1000 || md.PostTokens != 200 || md.DurationMS != 30 ||
					*md.PreservedSegment != (PreservedSegment{HeadUUID: "h", AnchorUUID: "a", TailUUID: "t"}) ||
					!reflect.DeepEqual(md.PreservedMessages.UUIDs, []string{"x", "y"}) {
					t.Fatalf("got %+v", md)
				}
			}},
		{"status", `"status":"compacting","permissionMode":"plan","compact_result":"failed","compact_error":"boom"`,
			func(t *testing.T, m Message) {
				sm := m.(*StatusMessage)
				if sm.Status != StatusCompacting || sm.PermissionMode != PermissionModePlan || sm.CompactResult != "failed" || sm.CompactError != "boom" {
					t.Fatalf("got %+v", sm)
				}
			}},
		{"status", `"status":null`, func(t *testing.T, m Message) {
			if sm := m.(*StatusMessage); sm.Status != "" {
				t.Fatalf("got %+v", sm)
			}
		}},
		{"api_retry", `"attempt":2,"max_retries":10,"retry_delay_ms":500,"error_status":529,"error":"overloaded",
		  "no_response":{"waited_ms":30000,"retry_wait_ms":60000}`,
			func(t *testing.T, m Message) {
				ar := m.(*APIRetryMessage)
				if ar.Attempt != 2 || ar.MaxRetries != 10 || ar.RetryDelayMS != 500 || *ar.ErrorStatus != 529 ||
					ar.Error != AssistantErrorOverloaded || *ar.NoResponse != (APIRetryNoResponse{WaitedMS: 30000, RetryWaitMS: 60000}) {
					t.Fatalf("got %+v", ar)
				}
			}},
		{"api_retry", `"attempt":1,"max_retries":10,"retry_delay_ms":500,"error_status":null,"error":"unknown"`,
			func(t *testing.T, m Message) {
				if ar := m.(*APIRetryMessage); ar.ErrorStatus != nil || ar.NoResponse != nil {
					t.Fatalf("got %+v", ar)
				}
			}},
		{"control_request_progress", `"request_id":"r1","status":"api_retry","attempt":1,"max_retries":3,"retry_delay_ms":100,"error_status":500`,
			func(t *testing.T, m Message) {
				cp := m.(*ControlRequestProgressMessage)
				if cp.RequestID != "r1" || cp.Status != "api_retry" || cp.Attempt != 1 || *cp.ErrorStatus != 500 {
					t.Fatalf("got %+v", cp)
				}
			}},
		{"model_refusal_fallback", `"trigger":"refusal","direction":"retry","scope":"local","original_model":"a","fallback_model":"b",
		  "request_id":null,"api_refusal_category":"cyber","api_refusal_explanation":null,
		  "retracted_message_uuids":["m1"],"refused_user_message_uuid":"q1","content":"Switched"`,
			func(t *testing.T, m Message) {
				rf := m.(*ModelRefusalFallbackMessage)
				if rf.Trigger != "refusal" || rf.Direction != "retry" || rf.Scope != "local" || rf.OriginalModel != "a" ||
					rf.FallbackModel != "b" || rf.RequestID != "" || rf.APIRefusalCategory != "cyber" ||
					rf.RefusedUserMessageUUID != "q1" || rf.Content != "Switched" || rf.RetractedMessageUUIDs[0] != "m1" {
					t.Fatalf("got %+v", rf)
				}
			}},
		{"model_refusal_no_fallback", `"original_model":"a","request_id":"req","api_refusal_category":"bio","content":"Refused"`,
			func(t *testing.T, m Message) {
				rn := m.(*ModelRefusalNoFallbackMessage)
				if rn.OriginalModel != "a" || rn.RequestID != "req" || rn.APIRefusalCategory != "bio" || rn.Content != "Refused" {
					t.Fatalf("got %+v", rn)
				}
			}},
		{"local_command_output", `"content":"Total cost: $0"`, func(t *testing.T, m Message) {
			if lc := m.(*LocalCommandOutputMessage); lc.Content != "Total cost: $0" {
				t.Fatalf("got %+v", lc)
			}
		}},
		{"plugin_install", `"status":"failed","name":"mkt","error":"nope"`, func(t *testing.T, m Message) {
			if pi := m.(*PluginInstallMessage); pi.Status != "failed" || pi.Name != "mkt" || pi.Error != "nope" {
				t.Fatalf("got %+v", pi)
			}
		}},
		{"background_tasks_changed", `"tasks":[{"task_id":"t1","task_type":"local_bash","description":"d","ambient":true}]`,
			func(t *testing.T, m Message) {
				bt := m.(*BackgroundTasksChangedMessage)
				if len(bt.Tasks) != 1 || bt.Tasks[0] != (BackgroundTask{TaskID: "t1", TaskType: "local_bash", Description: "d", Ambient: true}) {
					t.Fatalf("got %+v", bt)
				}
			}},
		{"thinking_tokens", `"estimated_tokens":120,"estimated_tokens_delta":20,"user_message_uuid":"q1"`,
			func(t *testing.T, m Message) {
				tt := m.(*ThinkingTokensMessage)
				if tt.EstimatedTokens != 120 || tt.EstimatedTokensDelta != 20 || tt.UserMessageUUID != "q1" {
					t.Fatalf("got %+v", tt)
				}
			}},
		{"session_state_changed", `"state":"requires_action"`, func(t *testing.T, m Message) {
			if ss := m.(*SessionStateChangedMessage); ss.State != SessionStateRequiresAction {
				t.Fatalf("got %+v", ss)
			}
		}},
		{"worker_shutting_down", `"reason":"host_exit"`, func(t *testing.T, m Message) {
			if ws := m.(*WorkerShuttingDownMessage); ws.Reason != "host_exit" {
				t.Fatalf("got %+v", ws)
			}
		}},
		{"commands_changed", `"commands":[{"name":"usage","description":"Show usage","argumentHint":"","aliases":["cost"],"builtin":true}]`,
			func(t *testing.T, m Message) {
				cc := m.(*CommandsChangedMessage)
				if len(cc.Commands) != 1 || cc.Commands[0].Name != "usage" || !cc.Commands[0].Builtin || cc.Commands[0].Aliases[0] != "cost" {
					t.Fatalf("got %+v", cc)
				}
			}},
		{"notification", `"key":"k","text":"hello","priority":"high","color":"red","timeout_ms":5000`,
			func(t *testing.T, m Message) {
				nm := m.(*NotificationMessage)
				if nm.Key != "k" || nm.Text != "hello" || nm.Priority != NotificationPriorityHigh || nm.Color != "red" || nm.TimeoutMS != 5000 {
					t.Fatalf("got %+v", nm)
				}
			}},
		{"files_persisted", `"files":[{"filename":"a.txt","file_id":"f1"}],"failed":[{"filename":"b.txt","error":"too big"}],"processed_at":"2026-01-01T00:00:00Z"`,
			func(t *testing.T, m Message) {
				fp := m.(*FilesPersistedMessage)
				if fp.Files[0] != (PersistedFile{Filename: "a.txt", FileID: "f1"}) || fp.Failed[0] != (FailedFile{Filename: "b.txt", Error: "too big"}) ||
					fp.ProcessedAt != "2026-01-01T00:00:00Z" {
					t.Fatalf("got %+v", fp)
				}
			}},
		{"memory_recall", `"mode":"select","memories":[{"path":"/m.md","scope":"team"},{"path":"https://x","scope":"organization","content":"c"}]`,
			func(t *testing.T, m Message) {
				mr := m.(*MemoryRecallMessage)
				if mr.Mode != "select" || len(mr.Memories) != 2 || mr.Memories[1] != (RecalledMemory{Path: "https://x", Scope: "organization", Content: "c"}) {
					t.Fatalf("got %+v", mr)
				}
			}},
		{"elicitation_complete", `"mcp_server_name":"srv","elicitation_id":"e1"`, func(t *testing.T, m Message) {
			if ec := m.(*ElicitationCompleteMessage); ec.MCPServerName != "srv" || ec.ElicitationID != "e1" {
				t.Fatalf("got %+v", ec)
			}
		}},
		{"permission_denied", `"tool_name":"Bash","tool_use_id":"tu1","agent_id":"a1","decision_reason_type":"mode","decision_reason":"dontAsk","message":"denied"`,
			func(t *testing.T, m Message) {
				pd := m.(*PermissionDeniedMessage)
				if pd.ToolName != "Bash" || pd.ToolUseID != "tu1" || pd.AgentID != "a1" || pd.DecisionReasonType != "mode" ||
					pd.DecisionReason != "dontAsk" || pd.Message != "denied" {
					t.Fatalf("got %+v", pd)
				}
			}},
		{"informational", `"content":"blocked","level":"warning","tool_use_id":"tu1","prevent_continuation":true`,
			func(t *testing.T, m Message) {
				in := m.(*InformationalMessage)
				if in.Content != "blocked" || in.Level != InformationalWarning || in.ToolUseID != "tu1" || !in.PreventContinuation {
					t.Fatalf("got %+v", in)
				}
			}},
		{"task_started", `"task_id":"t1","tool_use_id":"tu1","description":"d","subagent_type":"Explore","is_backgrounded":true,
		  "spawn_depth":2,"task_type":"local_workflow","workflow_name":"spec","prompt":"go","skip_transcript":true,"ambient":true`,
			func(t *testing.T, m Message) {
				ts := m.(*TaskStartedMessage)
				if ts.SubagentType != "Explore" || !ts.IsBackgrounded || ts.SpawnDepth != 2 || ts.WorkflowName != "spec" ||
					ts.Prompt != "go" || !ts.SkipTranscript || !ts.Ambient || ts.TaskType != "local_workflow" {
					t.Fatalf("got %+v", ts)
				}
			}},
		{"task_progress", `"task_id":"t1","description":"d","subagent_type":"Plan","usage":{"total_tokens":1,"tool_uses":2,"duration_ms":3},
		  "last_tool_name":"Read","summary":"Reading files"`,
			func(t *testing.T, m Message) {
				tp := m.(*TaskProgressMessage)
				if tp.SubagentType != "Plan" || tp.Summary != "Reading files" || tp.LastToolName != "Read" || tp.Usage.ToolUses != 2 {
					t.Fatalf("got %+v", tp)
				}
			}},
		{"task_notification", `"task_id":"t1","status":"stopped","reason":"worker_restart","output_file":"/o","summary":"s",
		  "usage":{"total_tokens":1,"tool_uses":2,"duration_ms":3},
		  "resource_links":[{"uri":"file:///a","name":"a","mimeType":"text/plain","size":12}],"skip_transcript":true,"ambient":true`,
			func(t *testing.T, m Message) {
				tn := m.(*TaskNotificationMessage)
				if tn.Reason != "worker_restart" || tn.Usage == nil || tn.Usage.DurationMS != 3 || !tn.SkipTranscript || !tn.Ambient ||
					len(tn.ResourceLinks) != 1 || tn.ResourceLinks[0].URI != "file:///a" || tn.ResourceLinks[0].MimeType != "text/plain" ||
					*tn.ResourceLinks[0].Size != 12 {
					t.Fatalf("got %+v", tn)
				}
			}},
		{"task_updated", `"task_id":"t1","patch":{"status":"paused","description":"d","end_time":1700000000000,"total_paused_ms":5,"error":"e","is_backgrounded":false}`,
			func(t *testing.T, m Message) {
				tu := m.(*TaskUpdatedMessage)
				p := tu.Patch
				if tu.Status != "paused" || p.Description != "d" || p.EndTime != 1700000000000 || p.TotalPausedMS != 5 ||
					p.Error != "e" || p.IsBackgrounded == nil || *p.IsBackgrounded {
					t.Fatalf("got %+v", tu)
				}
				if _, ok := tu.Data["patch"].(map[string]any)["end_time"]; !ok {
					t.Fatal("raw patch should be kept in Data")
				}
			}},
		{"hook_progress", `"hook_id":"h1","hook_name":"PreToolUse:Bash","hook_event":"PreToolUse","stdout":"o","stderr":"e","output":"oe"`,
			func(t *testing.T, m Message) {
				hm := m.(*HookEventMessage)
				if hm.HookID != "h1" || hm.HookName != "PreToolUse:Bash" || hm.HookEventName != "PreToolUse" ||
					hm.Stdout != "o" || hm.Stderr != "e" || hm.Output != "oe" || hm.ExitCode != nil {
					t.Fatalf("got %+v", hm)
				}
			}},
		{"hook_response", `"hook_id":"h1","hook_name":"Stop","hook_event":"Stop","output":"","stdout":"","stderr":"x","exit_code":2,"outcome":"error"`,
			func(t *testing.T, m Message) {
				hm := m.(*HookEventMessage)
				if hm.Outcome != HookOutcomeError || hm.ExitCode == nil || *hm.ExitCode != 2 || hm.Stderr != "x" {
					t.Fatalf("got %+v", hm)
				}
			}},
		{"mirror_error", `"error":"boom","key":{"projectKey":"p","sessionId":"s","subpath":"subagents/a"}`,
			func(t *testing.T, m Message) {
				me := m.(*MirrorErrorMessage)
				if me.Error != "boom" || me.Key == nil || *me.Key != (SessionKey{ProjectKey: "p", SessionID: "s", Subpath: "subagents/a"}) {
					t.Fatalf("got %+v", me)
				}
			}},
	}
	for _, tc := range cases {
		t.Run(tc.subtype, func(t *testing.T) {
			t.Parallel()
			line := `{"type":"system","subtype":"` + tc.subtype + `",` + tc.body + `,` + env + `}`
			msg := mustParse(t, line)
			tc.check(t, msg)
			// Every typed subtype keeps the base view and the raw payload.
			sm := reflect.ValueOf(msg).Elem().FieldByName("SystemMessage").Interface().(SystemMessage)
			if sm.Subtype != tc.subtype || sm.Data["uuid"] != "u1" {
				t.Fatalf("base = %+v", sm)
			}
			if f := reflect.ValueOf(msg).Elem().FieldByName("UUID"); f.IsValid() && f.String() != "u1" {
				t.Fatalf("uuid = %q", f.String())
			}
			if f := reflect.ValueOf(msg).Elem().FieldByName("SessionID"); f.IsValid() && f.String() != "s1" {
				t.Fatalf("session = %q", f.String())
			}
		})
	}
}

// TestParseTopLevelTypes checks the top-level types that used to be dropped.
func TestParseTopLevelTypes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		line  string
		check func(t *testing.T, m Message)
	}{
		{"tool_progress", `{"type":"tool_progress","tool_use_id":"tu1","tool_name":"Agent","parent_tool_use_id":null,
		  "elapsed_time_seconds":12.5,"task_id":"t1","uuid":"u1","session_id":"s1","heartbeat":true,"subagent_type":"Explore",
		  "subagent_retry":{"agent_id":"a1","attempt":1,"max_retries":5,"retry_delay_ms":1000,"error_status":null,"error_category":"overloaded"}}`,
			func(t *testing.T, m Message) {
				tp := m.(*ToolProgressMessage)
				if tp.ToolUseID != "tu1" || tp.ToolName != "Agent" || tp.ParentToolUseID != "" || tp.ElapsedTimeSeconds != 12.5 ||
					tp.TaskID != "t1" || !tp.Heartbeat || tp.SubagentType != "Explore" || tp.UUID != "u1" || tp.SessionID != "s1" {
					t.Fatalf("got %+v", tp)
				}
				if r := tp.SubagentRetry; r == nil || r.AgentID != "a1" || r.MaxRetries != 5 || r.ErrorStatus != nil || r.ErrorCategory != "overloaded" {
					t.Fatalf("retry = %+v", tp.SubagentRetry)
				}
			}},
		{"tool_use_summary", `{"type":"tool_use_summary","summary":"Read 3 files","preceding_tool_use_ids":["a","b"],"uuid":"u1","session_id":"s1"}`,
			func(t *testing.T, m Message) {
				ts := m.(*ToolUseSummaryMessage)
				if ts.Summary != "Read 3 files" || !reflect.DeepEqual(ts.PrecedingToolUseIDs, []string{"a", "b"}) || ts.UUID != "u1" {
					t.Fatalf("got %+v", ts)
				}
			}},
		{"auth_status", `{"type":"auth_status","isAuthenticating":true,"output":["Opening browser"],"error":"x","uuid":"u1","session_id":"s1"}`,
			func(t *testing.T, m Message) {
				as := m.(*AuthStatusMessage)
				if !as.IsAuthenticating || as.Output[0] != "Opening browser" || as.Error != "x" || as.SessionID != "s1" {
					t.Fatalf("got %+v", as)
				}
			}},
		{"prompt_suggestion", `{"type":"prompt_suggestion","suggestion":"run the tests","uuid":"u1","session_id":"s1"}`,
			func(t *testing.T, m Message) {
				if ps := m.(*PromptSuggestionMessage); ps.Suggestion != "run the tests" || ps.UUID != "u1" {
					t.Fatalf("got %+v", ps)
				}
			}},
		{"active_goal", `{"type":"active_goal","value":{"condition":"tests pass","iterations":2,"set_at":1700000000000,
		  "tokens_at_start":100,"last_reason":"1 failing"},"uuid":"u1","session_id":"s1"}`,
			func(t *testing.T, m Message) {
				ag := m.(*ActiveGoalMessage)
				want := ActiveGoal{Condition: "tests pass", Iterations: 2, SetAt: 1700000000000, TokensAtStart: 100, LastReason: "1 failing"}
				if ag.Value == nil || *ag.Value != want || ag.UUID != "u1" {
					t.Fatalf("got %+v", ag)
				}
			}},
		{"active_goal_cleared", `{"type":"active_goal","value":null,"uuid":"u1","session_id":"s1"}`,
			func(t *testing.T, m Message) {
				if ag := m.(*ActiveGoalMessage); ag.Value != nil {
					t.Fatalf("got %+v", ag)
				}
			}},
		{"conversation_reset", `{"type":"conversation_reset","new_conversation_id":"c2","uuid":"u1","session_id":"s1",
		  "trigger":"clear","user_message_uuid":"q1","timestamp":"2026-01-01T00:00:00.000Z"}`,
			func(t *testing.T, m Message) {
				cr := m.(*ConversationResetMessage)
				if cr.NewConversationID != "c2" || cr.Trigger != ResetTriggerClear || cr.UserMessageUUID != "q1" || cr.Timestamp == "" {
					t.Fatalf("got %+v", cr)
				}
			}},
		{"stream_event", `{"type":"stream_event","event":{"type":"message_start"},"parent_tool_use_id":null,"uuid":"u1","session_id":"s1",
		  "ttft_ms":321.5,"user_message_uuid":"q2","user_message_uuids":["q1","q2"],"resume_reason":"interrupted_turn"}`,
			func(t *testing.T, m Message) {
				se := m.(*StreamEvent)
				if se.TTFTMS == nil || *se.TTFTMS != 321.5 || se.UserMessageUUID != "q2" || len(se.UserMessageUUIDs) != 2 ||
					se.ResumeReason != "interrupted_turn" || se.ParentToolUseID != "" {
					t.Fatalf("got %+v", se)
				}
			}},
		{"rate_limit_event", `{"type":"rate_limit_event","uuid":"u1","session_id":"s1","rate_limit_info":{"status":"rejected",
		  "resetsAt":1700000000,"rateLimitType":"seven_day_opus","utilization":1,"overageStatus":"rejected","overageResetsAt":1700000500,
		  "overageDisabledReason":"out_of_credits","isUsingOverage":true,"overageInUse":true,"surpassedThreshold":0.9,
		  "limitScope":"group_pool","errorCode":"credits_required","canUserPurchaseCredits":true,"hasChargeableSavedPaymentMethod":true}}`,
			func(t *testing.T, m Message) {
				ri := m.(*RateLimitEvent).RateLimitInfo
				if ri.Status != RateLimitRejected || *ri.ResetsAt != 1700000000 || ri.RateLimitType != RateLimitTypeSevenDayOpus ||
					*ri.Utilization != 1 || ri.OverageStatus != RateLimitRejected || *ri.OverageResetsAt != 1700000500 ||
					ri.OverageDisabledReason != OverageDisabledOutOfCredits || !ri.IsUsingOverage || !ri.OverageInUse ||
					*ri.SurpassedThreshold != 0.9 || ri.LimitScope != "group_pool" || ri.ErrorCode != "credits_required" ||
					!ri.CanUserPurchaseCredits || !ri.HasChargeableSavedPaymentMethod || ri.Raw["limitScope"] != "group_pool" {
					t.Fatalf("got %+v", ri)
				}
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.check(t, mustParse(t, tc.line))
		})
	}
}

func TestParseUserMessageFields(t *testing.T) {
	t.Parallel()
	msg := mustParse(t, `{"type":"user","message":{"role":"user","content":[
	    {"type":"text","text":"look"},
	    {"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBO"}},
	    {"type":"document","source":{"type":"url","url":"https://x/a.pdf"},"title":"A"},
	    {"type":"search_result","source":"s","title":"t","content":[]}
	  ]},
	  "parent_tool_use_id":null,"isSynthetic":true,"priority":"next","shouldQuery":false,
	  "timestamp":"2026-01-01T00:00:00.000Z","client_composed":true,"uuid":"u1","session_id":"s1",
	  "isReplay":true,"file_attachments":[{"name":"f"}],"pasted_content":["p"],"inline_pastes":["i"],
	  "subagent_type":"Explore","task_description":"look around","tool_use_result":"plain string"}`)
	um := msg.(*UserMessage)
	if !um.IsReplay || !um.IsSynthetic || um.Priority != MessagePriorityNext || um.ShouldQuery == nil || *um.ShouldQuery ||
		um.Timestamp != "2026-01-01T00:00:00.000Z" || !um.ClientComposed || len(um.FileAttachments) != 1 ||
		um.PastedContent[0] != "p" || um.InlinePastes[0] != "i" || um.SubagentType != "Explore" || um.TaskDescription != "look around" {
		t.Fatalf("got %+v", um)
	}
	if um.ToolUseResult != "plain string" {
		t.Fatalf("tool_use_result = %#v", um.ToolUseResult)
	}
	if len(um.Content) != 4 {
		t.Fatalf("blocks = %#v", um.Content)
	}
	img := um.Content[1].(*ImageBlock)
	if img.Source != (BlockSource{Type: SourceBase64, MediaType: "image/png", Data: "iVBO"}) {
		t.Fatalf("image = %+v", img)
	}
	if doc := um.Content[2].(*DocumentBlock); doc.Source.URL != "https://x/a.pdf" || doc.Title != "A" {
		t.Fatalf("document = %+v", doc)
	}
	if ub := um.Content[3].(*UnknownBlock); ub.Type != "search_result" || ub.Raw["source"] != "s" {
		t.Fatalf("unknown = %+v", ub)
	}

	// An object tool_use_result stays a map.
	obj := mustParse(t, `{"type":"user","message":{"content":"x"},"tool_use_result":{"stdout":"ok"}}`).(*UserMessage)
	if m, ok := obj.ToolUseResult.(map[string]any); !ok || m["stdout"] != "ok" || obj.ShouldQuery != nil {
		t.Fatalf("got %+v", obj)
	}
}

func TestParseAssistantMessageFields(t *testing.T) {
	t.Parallel()
	msg := mustParse(t, `{"type":"assistant","message":{"id":"msg_1","type":"message","role":"assistant","model":"m",
	    "content":[{"type":"text","text":"no"}],"stop_reason":"refusal","stop_sequence":null,
	    "stop_details":{"type":"refusal","category":"cyber","explanation":null},
	    "usage":{"input_tokens":1},"container":{"id":"c1"},"context_management":{"applied_edits":[]}},
	  "parent_tool_use_id":null,"error":"max_output_tokens","uuid":"u1","session_id":"s1","request_id":"req_1",
	  "user_message_uuid":"q2","user_message_uuids":["q1","q2"],"resume_reason":"host_draining",
	  "resumed_from_incomplete_thinking":true,"supersedes":["old1"],"aborted":true,"subagent_type":"Plan",
	  "task_description":"plan it","timestamp":"2026-01-01T00:00:00.000Z",
	  "context_usage":{"model":"m","total_tokens":10,"raw_max_tokens":100,"percentage":10,
	    "over_limit":{"tokens_over":0,"kind":"hard_limit"},
	    "categories":[{"name":"Messages","tokens":10,"kind":"used"}],
	    "mcp_tools":[{"name":"mcp__a__b","server_name":"a","tokens":3}],
	    "memory_files":[{"path":"/CLAUDE.md","type":"Project","tokens":2}],
	    "agents":[{"agent_type":"x","source":"userSettings","tokens":1}],
	    "skills":[{"name":"pdf","source":"plugin","plugin_name":"docs","tokens":4}]},
	  "usage_report":{"session":{"total_cost_usd":1.5},"rate_limits":null}}`)
	am := msg.(*AssistantMessage)
	if am.Error != AssistantErrorMaxOutputTokens || am.RequestID != "req_1" || am.UserMessageUUID != "q2" ||
		len(am.UserMessageUUIDs) != 2 || am.ResumeReason != "host_draining" || !am.ResumedFromIncompleteThinking ||
		am.Supersedes[0] != "old1" || !am.Aborted || am.SubagentType != "Plan" || am.TaskDescription != "plan it" ||
		am.Timestamp == "" || am.StopReason != "refusal" || am.StopSequence != "" {
		t.Fatalf("got %+v", am)
	}
	if am.StopDetails == nil || *am.StopDetails != (StopDetails{Type: "refusal", Category: "cyber"}) {
		t.Fatalf("stop details = %+v", am.StopDetails)
	}
	if am.Container["id"] != "c1" || am.ContextManagement == nil {
		t.Fatalf("container = %v, cm = %v", am.Container, am.ContextManagement)
	}
	cu := am.ContextUsage
	if cu == nil || cu.TotalTokens != 10 || cu.OverLimit.Kind != "hard_limit" || cu.Categories[0].Kind != "used" ||
		cu.MCPTools[0].ServerName != "a" || cu.MemoryFiles[0].Type != "Project" || cu.Agents[0].AgentType != "x" ||
		cu.Skills[0].PluginName != "docs" {
		t.Fatalf("context usage = %+v", cu)
	}
	if s, _ := am.UsageReport["session"].(map[string]any); s["total_cost_usd"] != 1.5 {
		t.Fatalf("usage report = %v", am.UsageReport)
	}
}

func TestParseResultMessageFields(t *testing.T) {
	t.Parallel()
	msg := mustParse(t, `{"type":"result","subtype":"success","duration_ms":10,"duration_api_ms":8,"is_error":false,
	  "num_turns":1,"result":"ok","stop_reason":"end_turn","total_cost_usd":0.5,"usage":{"input_tokens":1},
	  "modelUsage":{"m":{"inputTokens":1,"outputTokens":5,"thinkingTokens":3,"cacheReadInputTokens":0,
	    "cacheCreationInputTokens":0,"webSearchRequests":0,"costUSD":0.5,"contextWindow":200000,
	    "maxOutputTokens":32000,"costBasis":"managed"}},
	  "permission_denials":[{"tool_name":"Bash","tool_use_id":"tu1","tool_input":{"command":"rm -rf /"}}],
	  "queued_turn_count":2,"structured_output":null,"terminal_reason":"completed","result_index":4,
	  "fast_mode_state":"cooldown","fast_mode_disabled_reason":"pending","user_message_uuid":"q1",
	  "user_message_uuids":["q1"],"resume_reason":"interrupted_turn","local_command":"cost",
	  "ttft_ms":120.5,"time_to_request_ms":40,"first_stream_post_queued_behind":"hold","warm_spare_claimed":true,
	  "uuid":"u1","session_id":"s1"}`)
	rm := msg.(*ResultMessage)
	if *rm.QueuedTurnCount != 2 || *rm.ResultIndex != 4 || rm.TerminalReason != TerminalReasonCompleted ||
		rm.FastModeState != FastModeCooldown || rm.FastModeDisabledReason != FastModeDisabledPending ||
		rm.UserMessageUUID != "q1" || rm.UserMessageUUIDs[0] != "q1" || rm.ResumeReason != "interrupted_turn" ||
		rm.LocalCommand != "cost" || rm.StructuredOutput != nil {
		t.Fatalf("got %+v", rm)
	}
	if mu := rm.ModelUsage["m"]; mu.ThinkingTokens != 3 || mu.CostBasis != "managed" || mu.OutputTokens != 5 {
		t.Fatalf("model usage = %+v", mu)
	}
	pd := rm.PermissionDenials
	if len(pd) != 1 || pd[0].ToolName != "Bash" || pd[0].ToolUseID != "tu1" || pd[0].ToolInput["command"] != "rm -rf /" {
		t.Fatalf("denials = %+v", pd)
	}
	want := ResultTiming{TTFTMS: 120.5, TimeToRequestMS: 40, FirstStreamPostQueuedBehind: "hold", WarmSpareClaimed: true}
	if rm.Timing == nil || *rm.Timing != want {
		t.Fatalf("timing = %+v", rm.Timing)
	}

	errRes := mustParse(t, `{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["boom"],
	  "startup_failure_reason":"cwd_unavailable","uuid":"u1","session_id":"s1"}`).(*ResultMessage)
	if errRes.Subtype != ResultSubtypeErrorDuringExecution || errRes.StartupFailureReason != StartupFailureCwdUnavailable ||
		errRes.Timing != nil || errRes.Errors[0] != "boom" {
		t.Fatalf("got %+v", errRes)
	}
}

// TestParseContentBlocks checks each Anthropic beta block kind, using samples
// shaped after @anthropic-ai/sdk 0.131 BetaContentBlock.
func TestParseContentBlocks(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		block string
		want  ContentBlock
	}{
		{"text_citations", `{"type":"text","text":"x","citations":[
		    {"type":"char_location","cited_text":"c","document_index":1,"document_title":"T","start_char_index":2,"end_char_index":5,"file_id":null},
		    {"type":"web_search_result_location","cited_text":"w","url":"https://u","title":null,"encrypted_index":"ei"}]}`,
			&TextBlock{Text: "x", Citations: []TextCitation{
				{Type: "char_location", CitedText: "c", DocumentIndex: 1, DocumentTitle: "T", StartCharIndex: 2, EndCharIndex: 5},
				{Type: "web_search_result_location", CitedText: "w", URL: "https://u", EncryptedIndex: "ei"},
			}}},
		{"text_null_citations", `{"type":"text","text":"x","citations":null}`, &TextBlock{Text: "x"}},
		{"redacted_thinking", `{"type":"redacted_thinking","data":"enc"}`, &RedactedThinkingBlock{Data: "enc"}},
		{"tool_use_caller", `{"type":"tool_use","id":"t","name":"n","input":{},"caller":{"type":"direct"},"toolset_name":"ts"}`,
			&ToolUseBlock{ID: "t", Name: "n", Input: map[string]any{}, Caller: map[string]any{"type": "direct"}, ToolsetName: "ts"}},
		{"web_search_tool_result", `{"type":"web_search_tool_result","tool_use_id":"s1","content":[
		    {"type":"web_search_result","url":"https://a","title":"A","encrypted_content":"e","page_age":null}]}`,
			&ServerToolResultBlock{Type: BlockWebSearchToolResult, ToolUseID: "s1", ContentList: []map[string]any{
				{"type": "web_search_result", "url": "https://a", "title": "A", "encrypted_content": "e", "page_age": nil}}}},
		{"web_search_tool_result_error", `{"type":"web_search_tool_result","tool_use_id":"s1",
		    "content":{"type":"web_search_tool_result_error","error_code":"max_uses_exceeded"}}`,
			&ServerToolResultBlock{Type: BlockWebSearchToolResult, ToolUseID: "s1",
				Content: map[string]any{"type": "web_search_tool_result_error", "error_code": "max_uses_exceeded"}}},
		{"web_fetch_tool_result", `{"type":"web_fetch_tool_result","tool_use_id":"s2","content":{"type":"web_fetch_result","url":"u"},"caller":{"type":"direct"}}`,
			&ServerToolResultBlock{Type: BlockWebFetchToolResult, ToolUseID: "s2",
				Content: map[string]any{"type": "web_fetch_result", "url": "u"}, Caller: map[string]any{"type": "direct"}}},
		{"code_execution_tool_result", `{"type":"code_execution_tool_result","tool_use_id":"s3","content":{"type":"code_execution_result","stdout":"1","stderr":"","return_code":0,"content":[]}}`,
			&ServerToolResultBlock{Type: BlockCodeExecutionToolResult, ToolUseID: "s3",
				Content: map[string]any{"type": "code_execution_result", "stdout": "1", "stderr": "", "return_code": float64(0), "content": []any{}}}},
		{"bash_code_execution_tool_result", `{"type":"bash_code_execution_tool_result","tool_use_id":"s4","content":{"type":"bash_code_execution_result"}}`,
			&ServerToolResultBlock{Type: BlockBashCodeExecutionToolResult, ToolUseID: "s4", Content: map[string]any{"type": "bash_code_execution_result"}}},
		{"text_editor_code_execution_tool_result", `{"type":"text_editor_code_execution_tool_result","tool_use_id":"s5","content":{"type":"text_editor_code_execution_view_result"}}`,
			&ServerToolResultBlock{Type: BlockTextEditorCodeExecutionToolResult, ToolUseID: "s5", Content: map[string]any{"type": "text_editor_code_execution_view_result"}}},
		{"tool_search_tool_result", `{"type":"tool_search_tool_result","tool_use_id":"s6","content":{"type":"tool_search_tool_search_result","tool_references":[]}}`,
			&ServerToolResultBlock{Type: BlockToolSearchToolResult, ToolUseID: "s6", Content: map[string]any{"type": "tool_search_tool_search_result", "tool_references": []any{}}}},
		{"advisor_tool_result", `{"type":"advisor_tool_result","tool_use_id":"s7","content":{"type":"advisor_result","text":"t"}}`,
			&ServerToolResultBlock{Type: BlockAdvisorToolResult, ToolUseID: "s7", Content: map[string]any{"type": "advisor_result", "text": "t"}}},
		{"mcp_tool_use", `{"type":"mcp_tool_use","id":"m1","name":"search","server_name":"srv","input":{"q":"x"}}`,
			&MCPToolUseBlock{ID: "m1", Name: "search", ServerName: "srv", Input: map[string]any{"q": "x"}}},
		{"mcp_tool_result_text", `{"type":"mcp_tool_result","tool_use_id":"m1","is_error":false,"content":"done"}`,
			&MCPToolResultBlock{ToolUseID: "m1", ContentText: msgPtr("done")}},
		{"mcp_tool_result_list", `{"type":"mcp_tool_result","tool_use_id":"m1","is_error":true,"content":[{"type":"text","text":"bad","citations":null}]}`,
			&MCPToolResultBlock{ToolUseID: "m1", IsError: true, ContentList: []map[string]any{{"type": "text", "text": "bad", "citations": nil}}}},
		{"mcp_tool_listing", `{"type":"mcp_tool_listing","mcp_server_name":"srv","tools":[{"name":"search"}]}`,
			&MCPToolListingBlock{MCPServerName: "srv", Tools: []map[string]any{{"name": "search"}}}},
		{"container_upload", `{"type":"container_upload","file_id":"file_1"}`, &ContainerUploadBlock{FileID: "file_1"}},
		{"compaction", `{"type":"compaction","content":"summary","encrypted_content":null,"signature":"sig","tool_changes":null}`,
			&CompactionBlock{Content: "summary", Signature: "sig"}},
		{"fallback", `{"type":"fallback","from":{"model":"a"},"to":{"model":"b"},"trigger":{"type":"refusal","category":null}}`,
			&FallbackBlock{From: FallbackInfo{Model: "a"}, To: FallbackInfo{Model: "b"}, Trigger: FallbackTrigger{Type: "refusal"}}},
		{"unknown", `{"type":"brand_new","x":[1]}`, &UnknownBlock{Type: "brand_new", Raw: map[string]any{"type": "brand_new", "x": []any{float64(1)}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			line := `{"type":"assistant","message":{"model":"m","content":[` + tc.block + `]}}`
			got := mustParse(t, line).(*AssistantMessage).Content
			if len(got) != 1 || !reflect.DeepEqual(got[0], tc.want) {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
			if got[0].BlockType() != tc.want.BlockType() {
				t.Fatalf("BlockType = %q", got[0].BlockType())
			}
		})
	}
}

// TestContentBlockWireRoundTrip checks that every block survives JSON
// encoding and that its wire form carries the right discriminator.
func TestContentBlockWireRoundTrip(t *testing.T) {
	t.Parallel()
	isErr := false
	blocks := []ContentBlock{
		&TextBlock{Text: "t", Citations: []TextCitation{{Type: "page_location", CitedText: "c", StartPageNumber: 1, EndPageNumber: 2}}},
		&ThinkingBlock{Thinking: "th", Signature: "s"},
		&RedactedThinkingBlock{Data: "d"},
		&ToolUseBlock{ID: "i", Name: "n", Input: map[string]any{"a": "b"}},
		&ToolResultBlock{ToolUseID: "i", ContentText: msgPtr("out"), IsError: &isErr},
		&ToolResultBlock{ToolUseID: "i", ContentList: []map[string]any{{"type": "text", "text": "x"}}},
		&ServerToolUseBlock{ID: "s", Name: ServerToolWebFetch, Input: map[string]any{"url": "u"}},
		&ServerToolResultBlock{Type: BlockWebSearchToolResult, ToolUseID: "s", ContentList: []map[string]any{{"type": "web_search_result"}}},
		&ServerToolResultBlock{Type: BlockCodeExecutionToolResult, ToolUseID: "s", Content: map[string]any{"type": "code_execution_result"}},
		&MCPToolUseBlock{ID: "m", Name: "n", ServerName: "srv", Input: map[string]any{}},
		&MCPToolResultBlock{ToolUseID: "m", ContentText: msgPtr("r"), IsError: true},
		&MCPToolListingBlock{MCPServerName: "srv", Tools: []map[string]any{{"name": "x"}}},
		&ContainerUploadBlock{FileID: "f"},
		&CompactionBlock{Content: "c", EncryptedContent: "e"},
		&FallbackBlock{From: FallbackInfo{Model: "a"}, To: FallbackInfo{Model: "b"}, Trigger: FallbackTrigger{Type: "refusal", Category: "bio"}},
		&ImageBlock{Source: BlockSource{Type: SourceURL, URL: "https://x/a.png"}},
		&DocumentBlock{Source: BlockSource{Type: SourceText, MediaType: "text/plain", Data: "hello"}, Title: "T", Citations: map[string]any{"enabled": true}},
		&UnknownBlock{Type: "search_result", Raw: map[string]any{"type": "search_result", "source": "s"}},
	}
	for _, b := range blocks {
		t.Run(b.BlockType(), func(t *testing.T) {
			t.Parallel()
			// Struct JSON round trip.
			data, err := json.Marshal(b, json.Deterministic(true))
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			back := reflect.New(reflect.TypeOf(b).Elem()).Interface().(ContentBlock)
			if err := json.Unmarshal(data, back); err != nil {
				t.Fatalf("unmarshal %s: %v", data, err)
			}
			if !reflect.DeepEqual(back, b) {
				t.Fatalf("round trip = %#v, want %#v (json %s)", back, b, data)
			}
			// Wire form parses back to the same block.
			wire, err := json.Marshal(wireBlock{b}, json.Deterministic(true))
			if err != nil {
				t.Fatalf("wire: %v", err)
			}
			var head struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal(wire, &head)
			if head.Type != b.BlockType() {
				t.Fatalf("wire type = %q, want %q (%s)", head.Type, b.BlockType(), wire)
			}
			if got := parseBlock(wire); !reflect.DeepEqual(got, b) {
				t.Fatalf("parse(wire) = %#v, want %#v (%s)", got, b, wire)
			}
		})
	}
}

func msgPtr[T any](v T) *T { return &v }

func TestParseResultFractionalCounters(t *testing.T) {
	t.Parallel()
	rm := mustParse(t, `{"type":"result","subtype":"success","duration_ms":12.7,"duration_api_ms":3.2,"num_turns":1}`).(*ResultMessage)
	if rm.DurationMS != 12 || rm.DurationAPIMS != 3 || rm.NumTurns != 1 {
		t.Fatalf("got %+v", rm)
	}
}

// encoding/json allocates a pointer field before it finds a type mismatch;
// lenient decoding must still leave a wrong-typed optional member nil.
func TestLenientWrongTypedPointersAreNil(t *testing.T) {
	t.Parallel()
	rl := mustParse(t, `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed",`+
		`"utilization":"high","surpassedThreshold":0.75,"resetsAt":"soon"}}`).(*RateLimitEvent)
	info := rl.RateLimitInfo
	if info.Utilization != nil || info.ResetsAt != nil || info.Status != "allowed" ||
		info.SurpassedThreshold == nil || *info.SurpassedThreshold != 0.75 {
		t.Fatalf("rate limit info = %+v", info)
	}

	retry := mustParse(t, `{"type":"system","subtype":"api_retry","attempt":2,"error_status":"x"}`).(*APIRetryMessage)
	if retry.ErrorStatus != nil || retry.Attempt != 2 {
		t.Fatalf("api retry = %+v", retry)
	}
	retry = mustParse(t, `{"type":"system","subtype":"api_retry","error_status":529,"no_response":true}`).(*APIRetryMessage)
	if retry.ErrorStatus == nil || *retry.ErrorStatus != 529 || retry.NoResponse != nil {
		t.Fatalf("api retry = %+v", retry)
	}

	hook := mustParse(t, `{"type":"system","subtype":"hook_response","hook_event":"Stop","exit_code":[1]}`).(*HookEventMessage)
	if hook.ExitCode != nil || hook.HookEventName != "Stop" {
		t.Fatalf("hook event = %+v", hook)
	}

	type response struct {
		N    *int    `json:"n"`
		S    *string `json:"s"`
		Name string  `json:"name"`
	}
	data := map[string]any{"n": "x", "s": "ok", "name": "v"}
	got, err := decodeControl[response](data, nil)
	if err != nil || got.N != nil || got.S == nil || *got.S != "ok" || got.Name != "v" {
		t.Fatalf("decodeControl = %+v, %v", got, err)
	}
	var value response
	decodeValue(data, &value)
	if value.N != nil || value.S == nil || value.Name != "v" {
		t.Fatalf("decodeValue = %+v", value)
	}
}
