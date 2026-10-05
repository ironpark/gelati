package claude

import "testing"

func TestDecodeMCPStatusResponse(t *testing.T) {
	t.Parallel()
	data := map[string]any{
		"mcpServers": []any{
			map[string]any{
				"name":       "fs",
				"status":     "connected",
				"serverInfo": map[string]any{"name": "fs-server", "version": "1.2.0"},
				"config":     map[string]any{"type": "stdio", "command": "fs"},
				"scope":      "project",
				"tools": []any{map[string]any{
					"name": "read", "description": "Read a file",
					"annotations": map[string]any{"readOnly": true},
				}},
			},
			map[string]any{"name": "web", "status": "failed", "error": "boom"},
		},
		"extra": 1.0,
	}
	resp := &MCPStatusResponse{Raw: data}
	if err := decodeResponse(data, resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.MCPServers) != 2 {
		t.Fatalf("servers = %#v", resp.MCPServers)
	}
	fs := resp.MCPServers[0]
	if fs.Status != MCPStatusConnected || fs.ServerInfo == nil || fs.ServerInfo.Version != "1.2.0" ||
		fs.Config["type"] != "stdio" || fs.Scope != "project" || len(fs.Tools) != 1 ||
		fs.Tools[0].Annotations == nil || fs.Tools[0].Annotations.ReadOnly == nil || !*fs.Tools[0].Annotations.ReadOnly {
		t.Fatalf("fs = %#v", fs)
	}
	if web := resp.MCPServers[1]; web.Status != MCPStatusFailed || web.Error != "boom" {
		t.Fatalf("web = %#v", web)
	}
	if resp.Raw["extra"] != 1.0 {
		t.Fatalf("raw = %#v", resp.Raw)
	}
}

func TestDecodeContextUsageResponse(t *testing.T) {
	t.Parallel()
	data := map[string]any{
		"categories": []any{
			map[string]any{"name": "System prompt", "tokens": 3000.0, "color": "gray"},
			map[string]any{"name": "MCP tools", "tokens": 120.0, "color": "cyan", "isDeferred": true},
		},
		"totalTokens": 3120.0, "maxTokens": 180000.0, "rawMaxTokens": 200000.0, "percentage": 1.7,
	}
	var resp ContextUsageResponse
	if err := decodeResponse(data, &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Categories) != 2 || !resp.Categories[1].IsDeferred || resp.TotalTokens != 3120 ||
		resp.MaxTokens != 180000 || resp.RawMaxTokens != 200000 || resp.Percentage != 1.7 {
		t.Fatalf("resp = %#v", resp)
	}
}

func TestStampUserMessage(t *testing.T) {
	t.Parallel()
	raw := map[string]any{"type": "user", "client_composed": false}
	if got := stampUserMessage(raw, false); got["client_composed"] != false {
		t.Fatalf("verbatim off changed the frame: %#v", got)
	}
	got := stampUserMessage(raw, true)
	if got["client_composed"] != true {
		t.Fatalf("verbatim on: %#v", got)
	}
	if raw["client_composed"] != false {
		t.Fatal("caller's frame was mutated")
	}
}

func TestClientVerbatimPrompts(t *testing.T) {
	t.Parallel()
	client, ft := testClient(t, &Options{VerbatimPrompts: true}, nil)
	if _, err := client.Send(t.Context(), Text("read @/etc/passwd")); err != nil {
		t.Fatal(err)
	}
	frames := ft.frames(t)
	if last := frames[len(frames)-1]; last["type"] != "user" || last["client_composed"] != true {
		t.Fatalf("frame = %#v", last)
	}
}

func TestQueryVerbatimPrompts(t *testing.T) {
	t.Parallel()
	ft := newFakeTransport()
	initResponder(ft, map[string]any{})
	ack := ft.onWrite
	ft.onWrite = func(frame map[string]any) {
		ack(frame)
		if frame["type"] == "user" {
			ft.push(map[string]any{"type": "result", "subtype": "success", "duration_ms": 1.0,
				"duration_api_ms": 1.0, "is_error": false, "num_turns": 1.0, "session_id": "s"})
			ft.finish(nil)
		}
	}
	for _, err := range Query(t.Context(), "hi", Options{Transport: ft, VerbatimPrompts: true}) {
		if err != nil {
			t.Fatal(err)
		}
	}
	var stamped bool
	for _, frame := range ft.frames(t) {
		if frame["type"] == "user" {
			stamped = frame["client_composed"] == true
		}
	}
	if !stamped {
		t.Fatal("user frame was not marked client_composed")
	}
}
