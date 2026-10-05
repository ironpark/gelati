package claude

import (
	"context"
	"errors"
	"testing"
	"time"
)

// connectedClientWith connects a client whose fake CLI answers each control
// request subtype with the given payload (initialize included).
func connectedClientWith(t *testing.T, opts *Options, answers map[string]map[string]any) (*Client, *fakeTransport) {
	t.Helper()
	ft := newFakeTransport()
	respondBySubtype(ft, answers)
	if opts == nil {
		opts = &Options{}
	}
	opts.Transport = ft
	client := NewClient(*opts)
	if err := client.Connect(t.Context()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Disconnect() })
	return client, ft
}

// lastRequest returns the request body of the last control request written.
func lastRequest(t *testing.T, ft *fakeTransport) map[string]any {
	t.Helper()
	frames := ft.frames(t)
	for i := len(frames) - 1; i >= 0; i-- {
		if frames[i]["type"] == "control_request" {
			return frames[i]["request"].(map[string]any)
		}
	}
	t.Fatal("no control request written")
	return nil
}

func TestClientControlRequestShapes(t *testing.T) {
	t.Parallel()
	mtime := time.UnixMilli(1700000000123)
	tests := []struct {
		name    string
		subtype string
		answer  map[string]any
		call    func(context.Context, *Client) (any, error)
		want    map[string]any
		check   func(t *testing.T, result any)
	}{
		{
			name: "interrupt receipt", subtype: "interrupt",
			answer: map[string]any{"still_queued": []any{"u1", 3}, "cancelled": []any{"u2"}},
			call: func(ctx context.Context, c *Client) (any, error) {
				return c.InterruptWithReceipt(ctx, &InterruptOptions{CancelQueued: true})
			},
			want: map[string]any{"subtype": "interrupt", "cancel_queued": true},
			check: func(t *testing.T, r any) {
				assertJSONEqual(t, r, &InterruptReceipt{StillQueued: []string{"u1"}, Cancelled: []string{"u2"}})
			},
		},
		{
			name: "interrupt without receipt", subtype: "interrupt", answer: map[string]any{},
			call: func(ctx context.Context, c *Client) (any, error) { return c.InterruptWithReceipt(ctx, nil) },
			want: map[string]any{"subtype": "interrupt"},
			check: func(t *testing.T, r any) {
				if r.(*InterruptReceipt) != nil {
					t.Fatalf("receipt = %#v", r)
				}
			},
		},
		{
			name: "rewind dry run", subtype: "rewind_files",
			answer: map[string]any{"canRewind": true, "filesChanged": []any{"a.go"}, "insertions": 3},
			call: func(ctx context.Context, c *Client) (any, error) {
				return c.RewindFiles(ctx, "u1", &RewindFilesOptions{DryRun: true})
			},
			want: map[string]any{"subtype": "rewind_files", "user_message_id": "u1", "dry_run": true},
			check: func(t *testing.T, r any) {
				res := r.(*RewindFilesResult)
				if !res.CanRewind || res.FilesChanged[0] != "a.go" || res.Insertions != 3 {
					t.Fatalf("result = %#v", res)
				}
			},
		},
		{
			name: "context usage detail", subtype: "get_context_usage",
			answer: map[string]any{"totalTokens": 10, "isAutoCompactEnabled": true, "model": "opus",
				"categories": []any{map[string]any{"name": "Free", "tokens": 5, "color": "c", "kind": "free"}},
				"apiUsage":   map[string]any{"input_tokens": 4}},
			call: func(ctx context.Context, c *Client) (any, error) {
				return c.ContextUsage(ctx, &ContextUsageOptions{Detail: ContextUsageSummary})
			},
			want: map[string]any{"subtype": "get_context_usage", "detail": "summary"},
			check: func(t *testing.T, r any) {
				res := r.(*ContextUsageResponse)
				if res.TotalTokens != 10 || !res.IsAutoCompactEnabled || res.Model != "opus" ||
					res.Categories[0].Kind != "free" || res.APIUsage.InputTokens != 4 {
					t.Fatalf("result = %#v", res)
				}
			},
		},
		{
			name: "apply flag settings", subtype: "apply_flag_settings",
			call: func(ctx context.Context, c *Client) (any, error) {
				return nil, c.ApplyFlagSettings(ctx, map[string]any{"fastMode": true, "effortLevel": nil})
			},
			want: map[string]any{"subtype": "apply_flag_settings", "settings": map[string]any{"fastMode": true, "effortLevel": nil}},
		},
		{
			name: "update settings", subtype: "update_settings",
			call: func(ctx context.Context, c *Client) (any, error) {
				return nil, c.UpdateSettings(ctx, DestinationLocalSettings, map[string]any{"theme": "dark"})
			},
			want: map[string]any{"subtype": "update_settings", "source": "localSettings", "settings": map[string]any{"theme": "dark"}},
		},
		{
			name: "mcp permission override clear", subtype: "set_mcp_permission_mode_override",
			answer: map[string]any{"warning": "unknown server"},
			call: func(ctx context.Context, c *Client) (any, error) {
				return c.SetMCPPermissionModeOverride(ctx, "fs", "")
			},
			want: map[string]any{"subtype": "set_mcp_permission_mode_override", "serverName": "fs", "mode": nil},
			check: func(t *testing.T, r any) {
				if r.(*MCPPermissionModeOverrideResult).Warning != "unknown server" {
					t.Fatalf("result = %#v", r)
				}
			},
		},
		{
			name: "reload plugins", subtype: "reload_plugins",
			answer: map[string]any{"commands": []any{}, "agents": []any{}, "error_count": 1, "held": true,
				"plugins":      []any{map[string]any{"name": "p", "path": "/p", "version": "1.0"}},
				"mcpServers":   []any{map[string]any{"name": "fs", "status": "connected", "source": "plugin"}},
				"cache_impact": map[string]any{"mcp_servers_added": []any{"x"}, "mcp_servers_removed": []any{}, "lsp_tool_change": nil}},
			call: func(ctx context.Context, c *Client) (any, error) {
				return c.ReloadPlugins(ctx, &ReloadPluginsOptions{HoldOnCacheImpact: true})
			},
			want: map[string]any{"subtype": "reload_plugins", "hold_on_cache_impact": true},
			check: func(t *testing.T, r any) {
				res := r.(*ReloadPluginsResult)
				if res.ErrorCount != 1 || res.Held == nil || !*res.Held || res.Plugins[0].Version != "1.0" ||
					res.MCPServers[0].Source != "plugin" || res.CacheImpact.MCPServersAdded[0] != "x" {
					t.Fatalf("result = %#v", res)
				}
			},
		},
		{
			name: "reload skills", subtype: "reload_skills",
			answer: map[string]any{"skills": []any{map[string]any{"name": "pdf", "description": "d", "argumentHint": ""}}},
			call:   func(ctx context.Context, c *Client) (any, error) { return c.ReloadSkills(ctx) },
			want:   map[string]any{"subtype": "reload_skills"},
			check: func(t *testing.T, r any) {
				if r.(*ReloadSkillsResult).Skills[0].Name != "pdf" {
					t.Fatalf("result = %#v", r)
				}
			},
		},
		{
			name: "reload output styles", subtype: "reload_output_styles",
			answer: map[string]any{"available_output_styles": []any{"default", "x"}},
			call:   func(ctx context.Context, c *Client) (any, error) { return c.ReloadOutputStyles(ctx) },
			want:   map[string]any{"subtype": "reload_output_styles"},
			check: func(t *testing.T, r any) {
				if len(r.(*ReloadOutputStylesResult).AvailableOutputStyles) != 2 {
					t.Fatalf("result = %#v", r)
				}
			},
		},
		{
			name: "background tasks default true", subtype: "background_tasks", answer: map[string]any{},
			call: func(ctx context.Context, c *Client) (any, error) { return c.BackgroundTasks(ctx, "tu1") },
			want: map[string]any{"subtype": "background_tasks", "tool_use_id": "tu1"},
			check: func(t *testing.T, r any) {
				if r != true {
					t.Fatalf("backgrounded = %v", r)
				}
			},
		},
		{
			name: "seed read state", subtype: "seed_read_state",
			call: func(ctx context.Context, c *Client) (any, error) { return nil, c.SeedReadState(ctx, "/a", mtime) },
			want: map[string]any{"subtype": "seed_read_state", "path": "/a", "mtime": 1700000000123},
		},
		{
			name: "read file", subtype: "read_file",
			answer: map[string]any{"contents": "aGk=", "absPath": "/w/a", "encoding": "base64", "truncated": true},
			call: func(ctx context.Context, c *Client) (any, error) {
				return c.ReadFile(ctx, "a", &ReadFileOptions{MaxBytes: 10, Encoding: ReadFileBase64})
			},
			want: map[string]any{"subtype": "read_file", "path": "a", "max_bytes": 10, "encoding": "base64"},
			check: func(t *testing.T, r any) {
				assertJSONEqual(t, r, &ReadFileResult{Contents: "aGk=", AbsPath: "/w/a", Truncated: true, Encoding: "base64"})
			},
		},
		{
			name: "read mcp resource", subtype: "mcp_read_resource",
			answer: map[string]any{"contents": []any{map[string]any{"uri": "ui://x", "mimeType": "text/html", "text": "<p>", "_meta": map[string]any{"ui": 1}}}},
			call: func(ctx context.Context, c *Client) (any, error) {
				return c.ReadMCPResource(ctx, "fs", "ui://x")
			},
			want: map[string]any{"subtype": "mcp_read_resource", "serverName": "fs", "uri": "ui://x"},
			check: func(t *testing.T, r any) {
				c := r.(*MCPReadResourceResult).Contents[0]
				if c.MIMEType != "text/html" || c.Meta["ui"] != float64(1) {
					t.Fatalf("result = %#v", r)
				}
			},
		},
		{
			name: "usage", subtype: "get_usage",
			answer: map[string]any{"session": map[string]any{"total_cost_usd": 1.5,
				"model_usage": map[string]any{"opus": map[string]any{"inputTokens": 3}}},
				"subscription_type": "max", "rate_limits_available": true,
				"rate_limits": map[string]any{"five_hour": map[string]any{"utilization": 12.5, "resets_at": nil}},
				"behaviors":   nil},
			call: func(ctx context.Context, c *Client) (any, error) {
				return c.UsageExperimental(ctx, &UsageOptions{SkipBehaviors: true})
			},
			want: map[string]any{"subtype": "get_usage", "skip_behaviors": true},
			check: func(t *testing.T, r any) {
				u := r.(*UsageReport)
				if u.Session.TotalCostUSD != 1.5 || u.Session.ModelUsage["opus"].InputTokens != 3 ||
					*u.SubscriptionType != "max" || *u.RateLimits.FiveHour.Utilization != 12.5 || u.Behaviors != nil {
					t.Fatalf("usage = %#v", u)
				}
			},
		},
		{
			name: "permission rules", subtype: "list_permission_rules",
			answer: map[string]any{"state": map[string]any{
				"rules": []any{map[string]any{"behavior": "allow", "source": "cliArg", "rule": "Bash(ls:*)",
					"editability": "session", "description": map[string]any{"prefix": "Run "}}},
				"workspaceDirectories": []any{map[string]any{"path": "/w", "source": "cliArg"}},
				"originalCwd":          "/w", "managedOnly": false,
				"errors": []any{map[string]any{"path": "x", "message": "bad"}}}},
			call: func(ctx context.Context, c *Client) (any, error) { return c.PermissionRules(ctx) },
			want: map[string]any{"subtype": "list_permission_rules"},
			check: func(t *testing.T, r any) {
				s := r.(*PermissionRulesState)
				if s.Rules[0].Rule != "Bash(ls:*)" || s.Rules[0].Description.Prefix != "Run " ||
					s.WorkspaceDirectories[0].Path != "/w" || s.Errors[0].Message != "bad" || s.Raw["originalCwd"] != "/w" {
					t.Fatalf("state = %#v", s)
				}
			},
		},
		{
			name: "generic escape hatch", subtype: "get_settings", answer: map[string]any{"effective": map[string]any{}},
			call: func(ctx context.Context, c *Client) (any, error) {
				return c.SendControlRequest(ctx, "get_settings", map[string]any{"subtype": "ignored", "x": 1})
			},
			want: map[string]any{"subtype": "get_settings", "x": 1},
			check: func(t *testing.T, r any) {
				if _, ok := r.(map[string]any)["effective"]; !ok {
					t.Fatalf("response = %#v", r)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			answers := map[string]map[string]any{}
			if tc.answer != nil {
				answers[tc.subtype] = tc.answer
			}
			client, ft := connectedClientWith(t, nil, answers)
			result, err := tc.call(t.Context(), client)
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			assertJSONEqual(t, lastRequest(t, ft), tc.want)
			if tc.check != nil {
				tc.check(t, result)
			}
		})
	}
}

func TestClientInitializeAccessors(t *testing.T) {
	t.Parallel()
	client := NewClient(Options{})
	if client.InitializationResult() != nil || client.SupportedCommands() != nil ||
		client.SupportedModels() != nil || client.SupportedAgents() != nil || client.AccountInfo() != nil {
		t.Fatal("accessors should be nil before Connect")
	}
	if _, err := client.Reinitialize(t.Context()); err == nil {
		t.Fatal("Reinitialize should fail before Connect")
	}
	var connErr *ConnectionError
	if _, err := client.SendControlRequest(t.Context(), "x", nil); !errors.As(err, &connErr) {
		t.Fatalf("error = %v", err)
	}

	client, ft := connectedClientWith(t, nil, map[string]map[string]any{"initialize": {
		"commands": []any{map[string]any{"name": "help", "description": "", "argumentHint": ""}},
		"models":   []any{map[string]any{"value": "opus", "displayName": "Opus", "description": ""}},
		"agents":   []any{map[string]any{"name": "Explore", "description": ""}},
		"account":  map[string]any{"email": "me@x"},
	}})
	if client.SupportedCommands()[0].Name != "help" || client.SupportedModels()[0].Value != "opus" ||
		client.SupportedAgents()[0].Name != "Explore" || client.AccountInfo().Email != "me@x" ||
		client.InitializationResult().Raw["account"] == nil {
		t.Fatalf("accessors = %#v", client.InitializationResult())
	}
	res, err := client.Reinitialize(t.Context())
	if err != nil || res.Models[0].Value != "opus" {
		t.Fatalf("reinitialize = %#v, %v", res, err)
	}
	if lastRequest(t, ft)["subtype"] != "initialize" {
		t.Fatal("Reinitialize did not send initialize")
	}
}

func TestClientControlRequestRoundTrip(t *testing.T) {
	t.Parallel()
	client, ft := connectedClientWith(t, nil, nil)
	ctx := t.Context()
	calls := []struct {
		name string
		call func() error
		want map[string]any
	}{
		{"interrupt", func() error { return client.Interrupt(ctx) }, map[string]any{"subtype": "interrupt"}},
		{"permission mode", func() error { return client.SetPermissionMode(ctx, PermissionModeAcceptEdits) },
			map[string]any{"subtype": "set_permission_mode", "mode": "acceptEdits"}},
		{"model", func() error { return client.SetModel(ctx, "opus") }, map[string]any{"subtype": "set_model", "model": "opus"}},
		{"default model", func() error { return client.SetModel(ctx, "") }, map[string]any{"subtype": "set_model", "model": nil}},
		{"rewind", func() error {
			_, err := client.RewindFiles(ctx, "u1", &RewindFilesOptions{DryRun: true})
			return err
		}, map[string]any{"subtype": "rewind_files", "user_message_id": "u1", "dry_run": true}},
		{"reconnect", func() error { return client.ReconnectMCPServer(ctx, "fs") },
			map[string]any{"subtype": "mcp_reconnect", "serverName": "fs"}},
		{"toggle", func() error { return client.ToggleMCPServer(ctx, "fs", false) },
			map[string]any{"subtype": "mcp_toggle", "serverName": "fs", "enabled": false}},
		{"stop task", func() error { return client.StopTask(ctx, "t1") }, map[string]any{"subtype": "stop_task", "task_id": "t1"}},
		{"mcp status", func() error {
			_, err := client.MCPServerStatus(ctx)
			return err
		}, map[string]any{"subtype": "mcp_status"}},
		{"context usage", func() error {
			_, err := client.ContextUsage(ctx, &ContextUsageOptions{Detail: "full"})
			return err
		}, map[string]any{"subtype": "get_context_usage", "detail": "full"}},
	}
	for _, c := range calls {
		if err := c.call(); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		assertJSONEqual(t, lastRequest(t, ft), c.want)
	}

	ids := map[string]bool{}
	for _, frame := range ft.frames(t) {
		if frame["type"] != "control_request" {
			continue
		}
		id := frame["request_id"].(string)
		if ids[id] {
			t.Fatalf("duplicate request id %q", id)
		}
		ids[id] = true
	}
}
