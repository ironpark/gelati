package claude

import (
	"context"
	"encoding/json"
	"reflect"
	"sync"
	"testing"
)

func TestClientSetMCPServers(t *testing.T) {
	t.Parallel()
	initial := NewSDKMCPServer("initial", "", ToolDef{Name: "t"})
	added := NewSDKMCPServerWithOptions(SDKMCPServerOptions{Name: "added", Timeout: 5000,
		Tools: []ToolDef{{Name: "x", Handler: func(_ context.Context, _ json.RawMessage) (ToolResult, error) {
			return TextResult("from added"), nil
		}}}})

	ft := newFakeTransport()
	var (
		mu       sync.Mutex
		requests []map[string]any
	)
	ft.mu.Lock()
	ft.onWrite = func(frame map[string]any) {
		if frame["type"] != "control_request" {
			return
		}
		req, _ := frame["request"].(map[string]any)
		payload := map[string]any{}
		if req["subtype"] == "mcp_set_servers" {
			mu.Lock()
			requests = append(requests, req)
			mu.Unlock()
			payload = map[string]any{"added": []any{"added", "fs"}, "removed": []any{"initial"},
				"errors": map[string]any{"fs": "spawn failed"}, "extra": true}
		}
		ft.push(map[string]any{"type": "control_response", "response": map[string]any{
			"subtype": "success", "request_id": frame["request_id"], "response": payload}})
	}
	ft.mu.Unlock()
	client := NewClient(&Options{Transport: ft, MCPServers: map[string]MCPServerConfig{"initial": initial}})
	if err := client.Connect(t.Context()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Disconnect() })

	result, err := client.SetMCPServers(t.Context(), map[string]MCPServerConfig{
		"added": added,
		"fs":    &MCPStdioServerConfig{Command: "node", Timeout: 2000},
	})
	if err != nil {
		t.Fatalf("SetMCPServers: %v", err)
	}
	if !reflect.DeepEqual(result.Added, []string{"added", "fs"}) || !reflect.DeepEqual(result.Removed, []string{"initial"}) ||
		result.Errors["fs"] != "spawn failed" || result.Raw["extra"] != true {
		t.Fatalf("result = %+v", result)
	}

	mu.Lock()
	got, _ := json.Marshal(requests[0])
	mu.Unlock()
	want := `{"servers":{"added":{"name":"added","timeout":5000,"type":"sdk"},"fs":{"command":"node","timeout":2000,"type":"stdio"}},"subtype":"mcp_set_servers"}`
	if string(got) != want {
		t.Fatalf("request = %s\nwant      %s", got, want)
	}

	// The new server is routed; the removed one is gone.
	pushMCP(ft, "m1", "added", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "x"}})
	reply := ft.nextResponse(t)["response"].(map[string]any)["mcp_response"].(map[string]any)
	if text := reply["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"]; text != "from added" {
		t.Fatalf("reply = %#v", reply)
	}
	pushMCP(ft, "m2", "initial", map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
	rpcErr := ft.nextResponse(t)["response"].(map[string]any)["mcp_response"].(map[string]any)["error"].(map[string]any)
	if rpcErr["message"] != "Server 'initial' not found" {
		t.Fatalf("error = %#v", rpcErr)
	}
	initial.Instance.(*MCPServer).mu.RLock()
	peers := len(initial.Instance.(*MCPServer).peers)
	initial.Instance.(*MCPServer).mu.RUnlock()
	if peers != 0 {
		t.Fatal("a removed server must be detached")
	}

	// Re-registering a name keeps the original timeout, as in the TS SDK.
	if _, err := client.SetMCPServers(t.Context(), map[string]MCPServerConfig{
		"added": &MCPSDKServerConfig{Name: "added", Instance: added.Instance, Timeout: 1},
	}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got, _ = json.Marshal(requests[1]["servers"])
	mu.Unlock()
	if string(got) != `{"added":{"name":"added","timeout":5000,"type":"sdk"}}` {
		t.Fatalf("servers = %s", got)
	}

	// Clearing everything sends an empty map.
	if _, err := client.SetMCPServers(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got, _ = json.Marshal(requests[2]["servers"])
	mu.Unlock()
	if string(got) != `{}` {
		t.Fatalf("servers = %s", got)
	}
}

func TestClientSetMCPServersNotConnected(t *testing.T) {
	t.Parallel()
	if _, err := NewClient(nil).SetMCPServers(t.Context(), nil); err == nil {
		t.Fatal("want an error before Connect")
	}
}

// TestClientReinitializeDeclaresLiveMCPServers checks that a re-initialize
// after SetMCPServers announces the live in-process servers, not the ones
// the session was configured with.
func TestClientReinitializeDeclaresLiveMCPServers(t *testing.T) {
	t.Parallel()
	initial := NewSDKMCPServer("initial", "", ToolDef{Name: "t"})
	added := NewSDKMCPServerWithOptions(SDKMCPServerOptions{Name: "added", Timeout: 5000, Tools: []ToolDef{{Name: "x"}}})

	ft := newFakeTransport()
	var (
		mu    sync.Mutex
		inits []map[string]any
	)
	ft.mu.Lock()
	ft.onWrite = func(frame map[string]any) {
		if frame["type"] != "control_request" {
			return
		}
		req, _ := frame["request"].(map[string]any)
		if req["subtype"] == "initialize" {
			mu.Lock()
			inits = append(inits, req)
			mu.Unlock()
		}
		ft.push(map[string]any{"type": "control_response", "response": map[string]any{
			"subtype": "success", "request_id": frame["request_id"], "response": map[string]any{}}})
	}
	ft.mu.Unlock()
	client := NewClient(&Options{Transport: ft, MCPServers: map[string]MCPServerConfig{"initial": initial}})
	if err := client.Connect(t.Context()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Disconnect() })
	if _, err := client.SetMCPServers(t.Context(), map[string]MCPServerConfig{"added": added}); err != nil {
		t.Fatalf("SetMCPServers: %v", err)
	}
	if _, err := client.Reinitialize(t.Context()); err != nil {
		t.Fatalf("Reinitialize: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(inits) != 2 {
		t.Fatalf("initialize requests = %d, want 2", len(inits))
	}
	if got := inits[0]["sdkMcpServers"]; !reflect.DeepEqual(got, []any{"initial"}) {
		t.Fatalf("first initialize sdkMcpServers = %#v", got)
	}
	if got := inits[1]["sdkMcpServers"]; !reflect.DeepEqual(got, []any{"added"}) {
		t.Fatalf("re-initialize sdkMcpServers = %#v, want the live set", got)
	}
	want := map[string]any{"added": map[string]any{"timeout": float64(5000)}}
	if got := inits[1]["sdkMcpServerConfigs"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("re-initialize sdkMcpServerConfigs = %#v", got)
	}
}
