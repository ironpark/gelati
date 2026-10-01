package claude

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMCPToolMetaAndInstructions(t *testing.T) {
	t.Parallel()
	size := 10
	cfg := NewSDKMCPServerWithOptions(SDKMCPServerOptions{
		Name:         "srv",
		Instructions: "Use srv for math.",
		AlwaysLoad:   true,
		Timeout:      5000,
		Tools: []ToolDef{
			{Name: "plain"},
			{Name: "hinted", SearchHint: "add numbers", Annotations: &ToolAnnotations{MaxResultSizeChars: &size}},
			{Name: "optOut", Meta: map[string]any{"anthropic/alwaysLoad": false, "ui": map[string]any{"resourceUri": "ui://x"}}},
		},
	})
	if cfg.Timeout != 5000 || cfg.Name != "srv" {
		t.Fatalf("config = %+v", cfg)
	}
	s := cfg.Instance.(*MCPServer)

	init := rpc(t, s, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize"})["result"].(map[string]any)
	if init["instructions"] != "Use srv for math." {
		t.Fatalf("initialize = %#v", init)
	}
	if caps, _ := json.Marshal(init["capabilities"]); string(caps) != `{"tools":{"listChanged":true}}` {
		t.Fatalf("capabilities = %s", caps)
	}

	tools := rpc(t, s, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"})["result"].(map[string]any)["tools"].([]any)
	want := []string{
		`{"anthropic/alwaysLoad":true}`,
		`{"anthropic/alwaysLoad":true,"anthropic/maxResultSizeChars":10,"anthropic/searchHint":"add numbers"}`,
		// A tool's own _meta overrides the server-wide alwaysLoad.
		`{"anthropic/alwaysLoad":false,"ui":{"resourceUri":"ui://x"}}`,
	}
	for i, tool := range tools {
		meta, _ := json.Marshal(tool.(map[string]any)["_meta"])
		if string(meta) != want[i] {
			t.Fatalf("tool %d _meta = %s, want %s", i, meta, want[i])
		}
	}

	// Without hints there is no _meta at all.
	plain := NewSDKMCPServer("p", "", ToolDef{Name: "t"}).Instance.(*MCPServer)
	tool := rpc(t, plain, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})["result"].(map[string]any)["tools"].([]any)[0]
	if _, ok := tool.(map[string]any)["_meta"]; ok {
		t.Fatalf("tool = %#v", tool)
	}
}

func TestToolResultWire(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		result ToolResult
		want   string
	}{
		{"audio", ToolResult{Content: []ToolContent{ToolAudio{Data: "UklG", MimeType: "audio/wav"}}},
			`{"content":[{"data":"UklG","mimeType":"audio/wav","type":"audio"}],"isError":false}`},
		{"structuredAndMeta", ToolResult{
			Content:           []ToolContent{ToolText{Text: "{}"}},
			StructuredContent: map[string]any{"sum": 3},
			Meta:              map[string]any{"claude/endTurn": true},
		}, `{"_meta":{"claude/endTurn":true},"content":[{"text":"{}","type":"text"}],"isError":false,"structuredContent":{"sum":3}}`},
		{"resourceLinkFull", ToolResult{Content: []ToolContent{ToolResourceLink{
			Name: "n", URI: "file:///x", Title: "T", Description: "d", MimeType: "text/plain"}}},
			`{"content":[{"description":"d","mimeType":"text/plain","name":"n","title":"T","type":"resource_link","uri":"file:///x"}],"isError":false}`},
		{"blobResource", ToolResult{Content: []ToolContent{ToolResource{URI: "file:///b", MimeType: "image/png", Blob: "AAAA"}}},
			`{"content":[{"resource":{"blob":"AAAA","mimeType":"image/png","uri":"file:///b"},"type":"resource"}],"isError":false}`},
		{"nilBlockSkipped", ToolResult{Content: []ToolContent{nil}, IsError: true},
			`{"content":[],"isError":true}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := json.Marshal(tc.result.wire())
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Fatalf("wire = %s\nwant   %s", got, tc.want)
			}
		})
	}
}

func TestMCPServerConfigJSONTSFields(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  MCPServerConfig
		want string
	}{
		{"stdio", &MCPStdioServerConfig{Command: "node", Timeout: 30000, AlwaysLoad: true},
			`{"type":"stdio","command":"node","timeout":30000,"alwaysLoad":true}`},
		{"sse", &MCPSSEServerConfig{URL: "https://x/sse", Tools: []MCPServerToolPolicy{
			{Name: "read", PermissionPolicy: MCPToolPolicyAlwaysAllow},
			{Name: "rm", PermissionPolicy: MCPToolPolicyAlwaysDeny, OrgMaxPermission: MCPToolOrgMaxBlocked}}},
			`{"type":"sse","url":"https://x/sse","tools":[{"name":"read","permission_policy":"always_allow"},{"name":"rm","permission_policy":"always_deny","org_max_permission":"blocked"}]}`},
		{"http", &MCPHTTPServerConfig{URL: "https://x/mcp", Timeout: 2000, AlwaysLoad: true},
			`{"type":"http","url":"https://x/mcp","timeout":2000,"alwaysLoad":true}`},
		{"sdkTimeout", &MCPSDKServerConfig{Name: "calc", Instance: &MCPServer{}, Timeout: 9000},
			`{"name":"calc","timeout":9000,"type":"sdk"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := json.Marshal(tc.cfg)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Fatalf("marshal = %s\nwant      %s", got, tc.want)
			}
		})
	}
}

// funcHandler is an MCPHandler backed by a function, standing in for an
// adapter over another MCP library.
type funcHandler struct {
	handle func(ctx context.Context, msg map[string]any) (any, error)

	mu   sync.Mutex
	send MCPSendFunc
	seen []map[string]any
}

func (h *funcHandler) HandleMCPMessage(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	var msg map[string]any
	if err := json.Unmarshal(raw, &msg); err != nil {
		return nil, err
	}
	h.mu.Lock()
	h.seen = append(h.seen, msg)
	h.mu.Unlock()
	out, err := h.handle(ctx, msg)
	if err != nil || out == nil {
		return nil, err
	}
	return json.Marshal(out)
}

func (h *funcHandler) ConnectMCP(send MCPSendFunc) func() {
	h.mu.Lock()
	h.send = send
	h.mu.Unlock()
	return func() {
		h.mu.Lock()
		h.send = nil
		h.mu.Unlock()
	}
}

func (h *funcHandler) sender() MCPSendFunc {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.send
}

func TestSDKMCPInitializeFields(t *testing.T) {
	t.Parallel()
	calc := NewSDKMCPServerWithOptions(SDKMCPServerOptions{Name: "calc", Version: "2.0.0", Instructions: "hi",
		Timeout: 5000, Tools: []ToolDef{{Name: "add"}}})
	slow := &funcHandler{handle: func(ctx context.Context, _ map[string]any) (any, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	failing := &funcHandler{handle: func(context.Context, map[string]any) (any, error) {
		return map[string]any{"jsonrpc": "2.0", "id": 1, "error": map[string]any{"code": -1, "message": "no"}}, nil
	}}
	noTools := &funcHandler{handle: func(_ context.Context, msg map[string]any) (any, error) {
		if msg["method"] == "initialize" {
			return map[string]any{"jsonrpc": "2.0", "id": msg["id"], "result": map[string]any{
				"protocolVersion": "2025-11-25", "capabilities": map[string]any{}, "serverInfo": map[string]any{"name": "x", "version": "1"}}}, nil
		}
		return nil, nil
	}}
	opts := &Options{MCPServers: map[string]MCPServerConfig{
		"calc":    calc,
		"slow":    &MCPSDKServerConfig{Name: "slow", Instance: slow},
		"failing": &MCPSDKServerConfig{Name: "failing", Instance: failing, Timeout: -1},
		"noTools": &MCPSDKServerConfig{Name: "noTools", Instance: noTools},
		"fs":      &MCPStdioServerConfig{Command: "node"},
		"bare":    &MCPSDKServerConfig{Name: "bare"},
	}}
	fields := sdkMCPInitializeFields(opts)

	names, _ := json.Marshal(fields["sdkMcpServers"])
	if string(names) != `["calc","failing","noTools","slow"]` {
		t.Fatalf("sdkMcpServers = %s", names)
	}
	configs, _ := json.Marshal(fields["sdkMcpServerConfigs"])
	if string(configs) != `{"calc":{"timeout":5000}}` {
		t.Fatalf("sdkMcpServerConfigs = %s", configs)
	}
	manifests, _ := json.Marshal(fields["sdkMcpServerManifests"])
	want := `{"calc":{"initializeResult":{"capabilities":{"tools":{"listChanged":true}},"instructions":"hi","protocolVersion":"2025-11-25","serverInfo":{"name":"calc","version":"2.0.0"}},` +
		`"toolsListResult":{"tools":[{"description":"","inputSchema":{"properties":{},"type":"object"},"name":"add"}]}},` +
		`"noTools":{"initializeResult":{"capabilities":{},"protocolVersion":"2025-11-25","serverInfo":{"name":"x","version":"1"}}}}`
	if string(manifests) != want {
		t.Fatalf("sdkMcpServerManifests = %s\nwant %s", manifests, want)
	}
	// The capture runs the full handshake, ids prefixed as in the TS SDK.
	noTools.mu.Lock()
	seen := noTools.seen
	noTools.mu.Unlock()
	if len(seen) != 2 || seen[0]["id"] != "sdk-manifest-capture:initialize" || seen[1]["method"] != "notifications/initialized" {
		t.Fatalf("handshake = %#v", seen)
	}
	if info := seen[0]["params"].(map[string]any)["clientInfo"].(map[string]any); info["name"] != "claude-code" {
		t.Fatalf("clientInfo = %#v", info)
	}

	if sdkMCPInitializeFields(&Options{MCPServers: map[string]MCPServerConfig{"fs": &MCPStdioServerConfig{}}}) != nil {
		t.Fatal("no in-process servers means no fields")
	}
}

func TestSDKMCPInitializeFieldsManifestOptOut(t *testing.T) {
	// Not parallel: it sets an environment variable.
	t.Setenv("CLAUDE_AGENT_SDK_DISABLE_MCP_MANIFESTS", "true")
	fields := sdkMCPInitializeFields(&Options{MCPServers: map[string]MCPServerConfig{"calc": NewSDKMCPServer("calc", "")}})
	if _, ok := fields["sdkMcpServerManifests"]; ok {
		t.Fatalf("fields = %#v", fields)
	}
	if fields["sdkMcpServers"] == nil {
		t.Fatalf("fields = %#v", fields)
	}
}

// mcpEngine starts an engine wired with the given in-process servers.
func mcpEngine(t *testing.T, servers map[string]MCPServerConfig) (*engine, *fakeTransport) {
	t.Helper()
	opts := &Options{MCPServers: servers}
	eng, ft := startEngine(t, opts)
	attachSDKMCPServers(eng, opts)
	return eng, ft
}

func pushMCP(ft *fakeTransport, requestID, server string, message map[string]any) {
	ft.push(map[string]any{"type": "control_request", "request_id": requestID, "request": map[string]any{
		"subtype": "mcp_message", "server_name": server, "message": message}})
}

func TestEngineMCPNotificationAckShape(t *testing.T) {
	t.Parallel()
	_, ft := mcpEngine(t, map[string]MCPServerConfig{"calc": NewSDKMCPServer("calc", "")})
	pushMCP(ft, "n1", "calc", map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	got, _ := json.Marshal(ft.nextResponse(t)["response"])
	if string(got) != `{"mcp_response":{"id":0,"jsonrpc":"2.0","result":{}}}` {
		t.Fatalf("ack = %s", got)
	}

	// An unregistered server is a JSON-RPC method-not-found error.
	pushMCP(ft, "n2", "nope", map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/list"})
	rpcErr := ft.nextResponse(t)["response"].(map[string]any)["mcp_response"].(map[string]any)["error"].(map[string]any)
	if rpcErr["code"] != float64(jsonRPCMethodNotFound) || rpcErr["message"] != "Server 'nope' not found" {
		t.Fatalf("error = %#v", rpcErr)
	}
}

func TestEngineMCPCancelledNotificationCancelsHandler(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	cfg := NewSDKMCPServer("calc", "", ToolDef{
		Name: "wait",
		Handler: func(ctx context.Context, _ json.RawMessage) (ToolResult, error) {
			close(started)
			select {
			case <-ctx.Done():
				return TextResult("cancelled"), nil
			case <-time.After(5 * time.Second):
				return TextResult("not cancelled"), nil
			}
		},
	})
	_, ft := mcpEngine(t, map[string]MCPServerConfig{"calc": cfg})
	pushMCP(ft, "c1", "calc", map[string]any{"jsonrpc": "2.0", "id": "call-7", "method": "tools/call",
		"params": map[string]any{"name": "wait"}})
	<-started
	pushMCP(ft, "c2", "calc", map[string]any{"jsonrpc": "2.0", "method": "notifications/cancelled",
		"params": map[string]any{"requestId": "call-7", "reason": "user"}})

	var call map[string]any
	for range 2 {
		resp := ft.nextResponse(t)
		if resp["request_id"] == "c1" {
			call = resp
		}
	}
	if call == nil {
		t.Fatal("no reply to the cancelled call")
	}
	result := call["response"].(map[string]any)["mcp_response"].(map[string]any)["result"].(map[string]any)
	if text := result["content"].([]any)[0].(map[string]any)["text"]; text != "cancelled" {
		t.Fatalf("handler was not cancelled: %v", text)
	}
}

func TestMCPServerNotifiesConnectedSessions(t *testing.T) {
	t.Parallel()
	cfg := NewSDKMCPServer("calc", "")
	server := cfg.Instance.(*MCPServer)
	eng, ft := mcpEngine(t, map[string]MCPServerConfig{"calc-key": cfg})

	server.AddTools(ToolDef{Name: "a"}, ToolDef{Name: "b"})
	frame := ft.nextWrite(t)
	request := frame["request"].(map[string]any)
	if frame["type"] != "control_request" || request["subtype"] != "mcp_message" || request["server_name"] != "calc-key" {
		t.Fatalf("frame = %#v", frame)
	}
	if id, _ := frame["request_id"].(string); len(id) != 36 {
		t.Fatalf("request_id = %v, want a UUID", frame["request_id"])
	}
	msg, _ := json.Marshal(request["message"])
	if string(msg) != `{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}` {
		t.Fatalf("message = %s", msg)
	}

	// Replacing a tool keeps its position; removing an absent tool is silent.
	server.AddTools(ToolDef{Name: "a", Description: "new"})
	ft.nextWrite(t)
	server.RemoveTools("missing")
	server.RemoveTools("b")
	ft.nextWrite(t)
	tools := rpc(t, server, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["description"] != "new" {
		t.Fatalf("tools = %#v", tools)
	}

	if err := server.Notify(t.Context(), "notifications/message", map[string]any{"level": "info", "data": "x"}); err != nil {
		t.Fatalf("notify: %v", err)
	}
	msg, _ = json.Marshal(ft.nextWrite(t)["request"].(map[string]any)["message"])
	if string(msg) != `{"jsonrpc":"2.0","method":"notifications/message","params":{"data":"x","level":"info"}}` {
		t.Fatalf("message = %s", msg)
	}

	// Closing the session detaches the server.
	_ = eng.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		server.mu.RLock()
		peers := len(server.peers)
		server.mu.RUnlock()
		if peers == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server still attached after Close")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestCustomMCPHandler(t *testing.T) {
	t.Parallel()
	handler := &funcHandler{handle: func(ctx context.Context, msg map[string]any) (any, error) {
		switch msg["method"] {
		case "resources/list":
			return map[string]any{"jsonrpc": "2.0", "id": msg["id"], "result": map[string]any{"resources": []any{}}}, nil
		case "boom":
			return nil, errors.New("exploded")
		case "panic":
			panic("kaboom")
		}
		return nil, nil
	}}
	_, ft := mcpEngine(t, map[string]MCPServerConfig{"lib": &MCPSDKServerConfig{Name: "lib", Instance: handler}})

	pushMCP(ft, "r1", "lib", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "resources/list"})
	if _, ok := ft.nextResponse(t)["response"].(map[string]any)["mcp_response"].(map[string]any)["result"]; !ok {
		t.Fatal("resources/list was not answered by the custom handler")
	}

	for _, method := range []string{"boom", "panic"} {
		pushMCP(ft, "r-"+method, "lib", map[string]any{"jsonrpc": "2.0", "id": 2, "method": method})
		rpcErr := ft.nextResponse(t)["response"].(map[string]any)["mcp_response"].(map[string]any)["error"].(map[string]any)
		if rpcErr["code"] != float64(jsonRPCInternalError) {
			t.Fatalf("%s: error = %#v", method, rpcErr)
		}
	}

	// A server-initiated request goes out as an mcp_message ...
	send := handler.sender()
	if send == nil {
		t.Fatal("handler was not connected")
	}
	if err := send(t.Context(), json.RawMessage(`{"jsonrpc":"2.0","id":"s1","method":"elicitation/create","params":{}}`)); err != nil {
		t.Fatal(err)
	}
	out := ft.nextWrite(t)["request"].(map[string]any)
	if out["subtype"] != "mcp_message" || out["message"].(map[string]any)["method"] != "elicitation/create" {
		t.Fatalf("outbound = %#v", out)
	}
	// ... and the CLI's response comes back through HandleMCPMessage.
	pushMCP(ft, "r3", "lib", map[string]any{"jsonrpc": "2.0", "id": "s1", "result": map[string]any{"action": "decline"}})
	ft.nextResponse(t)
	handler.mu.Lock()
	last := handler.seen[len(handler.seen)-1]
	handler.mu.Unlock()
	if last["id"] != "s1" || last["result"] == nil {
		t.Fatalf("response not delivered: %#v", last)
	}

	if err := send(t.Context(), json.RawMessage(`not json`)); err == nil || !strings.Contains(err.Error(), "invalid JSON") {
		t.Fatalf("err = %v", err)
	}
}

func TestMCPServerStatusSourceAndToolMeta(t *testing.T) {
	t.Parallel()
	var status MCPServerStatus
	raw := `{"name":"calc","status":"connected","source":"sdk","tools":[{"name":"add","_meta":{"ui":{"resourceUri":"ui://calc/app"}}}]}`
	if err := json.Unmarshal([]byte(raw), &status); err != nil {
		t.Fatal(err)
	}
	if status.Source != "sdk" || status.Tools[0].Meta["ui"].(map[string]any)["resourceUri"] != "ui://calc/app" {
		t.Fatalf("status = %+v", status)
	}
}
