package claude

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"slices"
	"sync"
	"time"
)

// mcpManifestProtocolVersion is the MCP protocol version the manifest
// capture initializes servers with. It must equal the version the CLI's MCP
// client requests, or the CLI ignores the manifest.
const mcpManifestProtocolVersion = "2025-11-25"

// mcpManifestCaptureTimeout bounds the whole manifest capture; servers that
// take longer handshake over the control channel instead.
const mcpManifestCaptureTimeout = 250 * time.Millisecond

// mcpManifestIDPrefix marks the JSON-RPC ids of capture requests.
const mcpManifestIDPrefix = "sdk-manifest-capture:"

// sdkMCPInitializeFields returns the initialize-request fields that declare
// in-process SDK MCP servers, matching the TypeScript SDK:
//
//   - sdkMcpServers: the server names;
//   - sdkMcpServerConfigs: {name: {timeout}} for servers with a timeout;
//   - sdkMcpServerManifests: {name: {initializeResult, toolsListResult?}},
//     each server's own handshake output, so the CLI connects to them
//     without mcp_message round trips. Setting
//     CLAUDE_AGENT_SDK_DISABLE_MCP_MANIFESTS turns this off.
//
// It returns nil when no in-process server is configured.
func sdkMCPInitializeFields(opts *Options) map[string]any {
	return sdkMCPDeclarationFields(sdkMCPServers(opts))
}

// sdkMCPDeclarationFields is sdkMCPInitializeFields for an explicit server
// set, such as a session's live registry.
func sdkMCPDeclarationFields(servers map[string]*MCPSDKServerConfig) map[string]any {
	if len(servers) == 0 {
		return nil
	}
	names := slices.Sorted(maps.Keys(servers))
	fields := map[string]any{"sdkMcpServers": names}

	configs := map[string]any{}
	for _, name := range names {
		if timeout := positiveTimeout(servers[name].Timeout); timeout > 0 {
			configs[name] = map[string]any{"timeout": timeout}
		}
	}
	if len(configs) > 0 {
		fields["sdkMcpServerConfigs"] = configs
	}

	if !envTruthy(os.Getenv("CLAUDE_AGENT_SDK_DISABLE_MCP_MANIFESTS")) {
		if manifests := captureSDKMCPManifests(servers); len(manifests) > 0 {
			fields["sdkMcpServerManifests"] = manifests
		}
	}
	return fields
}

// sdkMCPManifest is one server's captured handshake output. The results are
// kept verbatim, as the CLI requires.
type sdkMCPManifest struct {
	InitializeResult json.RawMessage `json:"initializeResult"`
	ToolsListResult  json.RawMessage `json:"toolsListResult,omitempty"`
}

// captureSDKMCPManifests performs the MCP handshake with every server
// in-process, within mcpManifestCaptureTimeout overall. Servers that fail or
// are too slow are left out; the CLI handshakes them over the control
// channel.
func captureSDKMCPManifests(servers map[string]*MCPSDKServerConfig) map[string]sdkMCPManifest {
	ctx, cancel := context.WithTimeout(context.Background(), mcpManifestCaptureTimeout)
	defer cancel()

	var (
		mu  sync.Mutex
		out = map[string]sdkMCPManifest{}
		wg  sync.WaitGroup
	)
	for name, cfg := range servers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			manifest, ok := captureSDKMCPManifest(ctx, cfg.Instance)
			if !ok {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if ctx.Err() == nil {
				out[name] = manifest
			}
		}()
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
	mu.Lock()
	defer mu.Unlock()
	// Late goroutines see the cancelled ctx and leave the map alone.
	cancel()
	return out
}

// captureSDKMCPManifest runs initialize, notifications/initialized and, when
// the server has tools, tools/list.
func captureSDKMCPManifest(ctx context.Context, handler MCPHandler) (manifest sdkMCPManifest, ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	initResult, ok := mcpCaptureCall(ctx, handler, map[string]any{
		"jsonrpc": "2.0",
		"id":      mcpManifestIDPrefix + "initialize",
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": mcpManifestProtocolVersion,
			"capabilities":    map[string]any{},
			"clientInfo": map[string]any{
				"name":        "claude-code",
				"title":       "Claude Code",
				"description": "Anthropic's agentic coding tool",
				"websiteUrl":  "https://claude.com/claude-code",
				"version":     Version,
			},
		},
	})
	if !ok {
		return manifest, false
	}
	manifest.InitializeResult = initResult
	notification, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	if _, err := handler.HandleMCPMessage(ctx, notification); err != nil {
		return manifest, false
	}

	var init struct {
		Capabilities map[string]json.RawMessage `json:"capabilities"`
	}
	_ = json.Unmarshal(initResult, &init)
	if tools, has := init.Capabilities["tools"]; has && string(tools) != "null" {
		listResult, listed := mcpCaptureCall(ctx, handler, map[string]any{
			"jsonrpc": "2.0",
			"id":      mcpManifestIDPrefix + "tools-list",
			"method":  "tools/list",
		})
		if !listed {
			return manifest, false
		}
		var page struct {
			NextCursor *json.RawMessage `json:"nextCursor"`
		}
		if json.Unmarshal(listResult, &page) == nil && page.NextCursor == nil {
			manifest.ToolsListResult = listResult
		}
	}
	return manifest, true
}

// mcpCaptureCall sends one request and returns its result object, or false
// when the server answered with an error or nothing usable.
func mcpCaptureCall(ctx context.Context, handler MCPHandler, request map[string]any) (json.RawMessage, bool) {
	raw, err := json.Marshal(request)
	if err != nil {
		return nil, false
	}
	reply, err := handler.HandleMCPMessage(ctx, raw)
	if err != nil || reply == nil {
		return nil, false
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
	}
	if json.Unmarshal(reply, &envelope) != nil || len(envelope.Result) == 0 || envelope.Result[0] != '{' {
		return nil, false
	}
	return envelope.Result, true
}
