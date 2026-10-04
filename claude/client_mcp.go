package claude

import (
	"context"
	"fmt"
	"maps"
	"slices"
)

// MCPSetServersResult is the result of [Client.SetMCPServers].
type MCPSetServersResult struct {
	// Added names the servers that were added.
	Added []string `json:"added"`
	// Removed names the servers that were removed.
	Removed []string `json:"removed"`
	// Errors maps a server name to the reason it failed to connect.
	Errors map[string]string `json:"errors"`
	// Raw is the full response payload, including fields not modeled above.
	Raw map[string]any `json:"-"`
}

// SetMCPServers replaces the session's dynamically managed MCP servers with
// servers, keyed by name. Servers missing from the map are removed, new ones
// are added and connected.
//
// In-process servers (*MCPSDKServerConfig with an Instance) are registered
// with this client before the request is sent, and removed ones stop
// receiving traffic. As in the TypeScript SDK, a name that is already
// registered keeps its original instance and Timeout until it is removed and
// added again. Every other configuration is passed to the CLI as is.
func (c *Client) SetMCPServers(ctx context.Context, servers map[string]MCPServerConfig) (*MCPSetServersResult, error) {
	eng, err := c.engineOrErr()
	if err != nil {
		return nil, err
	}
	registry := eng.mcpServers
	sdk := map[string]*MCPSDKServerConfig{}
	wire := map[string]any{}
	for name, cfg := range servers {
		if cfg == nil {
			return nil, fmt.Errorf("claude: MCP server %q has a nil config", name)
		}
		if s, ok := asSDKServer(cfg); ok {
			sdk[name] = s
			continue
		}
		wire[name] = cfg
	}

	for _, name := range registry.names() {
		if _, keep := sdk[name]; !keep {
			registry.remove(name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(sdk)) {
		registry.connect(name, sdk[name])
		if entry := registry.get(name); entry != nil {
			// The registered config, which encodes as the CLI's
			// {"type":"sdk",...} declaration.
			wire[name] = entry.config
		}
	}

	response, err := eng.request(ctx, "mcp_set_servers", map[string]any{"servers": wire})
	if err != nil {
		return nil, err
	}
	result := &MCPSetServersResult{}
	if err := decodeResponse(response, result); err != nil {
		return nil, err
	}
	if result.Added == nil {
		result.Added = []string{}
	}
	if result.Removed == nil {
		result.Removed = []string{}
	}
	if result.Errors == nil {
		result.Errors = map[string]string{}
	}
	result.Raw = response
	return result, nil
}
