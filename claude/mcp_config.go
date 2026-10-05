package claude

import (
	"encoding/json/v2"
	"reflect"
)

// ---------------------------------------------------------------------------
// MCP server configuration
// ---------------------------------------------------------------------------

// MCPServerConfig configures one MCP server. Implementations are
// MCPStdioServerConfig, MCPSSEServerConfig, MCPHTTPServerConfig and
// MCPSDKServerConfig.
type MCPServerConfig interface {
	isMCPServerConfig()
}

// MCPStdioServerConfig launches an MCP server as a subprocess.
type MCPStdioServerConfig struct {
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	// Timeout is the per-call tool timeout in milliseconds, overriding
	// MCP_TOOL_TIMEOUT for this server. Zero leaves the default; the CLI
	// ignores values below 1000.
	Timeout int `json:"timeout,omitzero"`
	// AlwaysLoad keeps every tool of the server in the prompt instead of
	// deferring it behind tool search, and makes startup wait for the
	// server to connect.
	AlwaysLoad bool `json:"alwaysLoad,omitzero"`
}

func (*MCPStdioServerConfig) isMCPServerConfig() {}

// MarshalJSON emits the stdio server shape with its type discriminator.
func (c *MCPStdioServerConfig) MarshalJSON() ([]byte, error) {
	type alias MCPStdioServerConfig
	return json.Marshal(struct {
		Type string `json:"type"`
		*alias
	}{"stdio", (*alias)(c)}, marshalOpts)
}

// MCPSSEServerConfig connects to an MCP server over server-sent events.
type MCPSSEServerConfig struct {
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
	// Tools sets per-tool permission policies.
	Tools []MCPServerToolPolicy `json:"tools,omitempty"`
	// Timeout is the per-call tool timeout in milliseconds; see
	// MCPStdioServerConfig.Timeout.
	Timeout int `json:"timeout,omitzero"`
	// AlwaysLoad: see MCPStdioServerConfig.AlwaysLoad.
	AlwaysLoad bool `json:"alwaysLoad,omitzero"`
}

func (*MCPSSEServerConfig) isMCPServerConfig() {}

// MarshalJSON emits the SSE server shape with its type discriminator.
func (c *MCPSSEServerConfig) MarshalJSON() ([]byte, error) {
	type alias MCPSSEServerConfig
	return json.Marshal(struct {
		Type string `json:"type"`
		*alias
	}{"sse", (*alias)(c)}, marshalOpts)
}

// MCPHTTPServerConfig connects to an MCP server over streamable HTTP.
type MCPHTTPServerConfig struct {
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
	// Tools sets per-tool permission policies.
	Tools []MCPServerToolPolicy `json:"tools,omitempty"`
	// Timeout is the per-call tool timeout in milliseconds; see
	// MCPStdioServerConfig.Timeout.
	Timeout int `json:"timeout,omitzero"`
	// AlwaysLoad: see MCPStdioServerConfig.AlwaysLoad.
	AlwaysLoad bool `json:"alwaysLoad,omitzero"`
}

func (*MCPHTTPServerConfig) isMCPServerConfig() {}

// MarshalJSON emits the HTTP server shape with its type discriminator.
func (c *MCPHTTPServerConfig) MarshalJSON() ([]byte, error) {
	type alias MCPHTTPServerConfig
	return json.Marshal(struct {
		Type string `json:"type"`
		*alias
	}{"http", (*alias)(c)}, marshalOpts)
}

// MCPSDKServerConfig serves an in-process MCP server to the CLI over the
// control protocol. Build one with NewSDKMCPServer; the server is declared to
// the CLI in the initialize request (or by Client.SetMCPServers) and its
// instance stays in this process.
type MCPSDKServerConfig struct {
	Name string
	// Instance answers the server's mcp_message traffic: an *MCPServer, or
	// any MCPHandler (for example an adapter over another MCP library).
	Instance MCPHandler
	// Timeout is the per-call tool timeout in milliseconds, overriding
	// MCP_TOOL_TIMEOUT for this server. Zero leaves the default; the CLI
	// ignores values below 1000. It applies when the server is first
	// registered.
	Timeout int
}

func (*MCPSDKServerConfig) isMCPServerConfig() {}

// Server returns the built-in server behind the config, or nil when Instance
// is not an *MCPServer (another MCPHandler, or none).
func (c *MCPSDKServerConfig) Server() *MCPServer {
	if c == nil {
		return nil
	}
	server, _ := c.Instance.(*MCPServer)
	return server
}

// MarshalJSON emits only the serializable fields; the instance is not sent.
func (c *MCPSDKServerConfig) MarshalJSON() ([]byte, error) {
	out := map[string]any{"type": "sdk", "name": c.Name}
	if c.Timeout > 0 {
		out["timeout"] = c.Timeout
	}
	return json.Marshal(out, marshalOpts)
}

// MCPServerToolPolicy sets the permission policy of one tool of an SSE or HTTP
// MCP server.
type MCPServerToolPolicy struct {
	// Name is the tool name as the server lists it.
	Name string `json:"name"`
	// PermissionPolicy is one of the MCPToolPolicy* constants; empty
	// leaves the normal permission flow.
	PermissionPolicy MCPToolPermissionPolicy `json:"permission_policy,omitempty"`
	// OrgMaxPermission is an org admin's ceiling for the tool, one of the
	// MCPToolOrgMax* constants. An "ask" ceiling forces a prompt even in
	// auto mode.
	OrgMaxPermission MCPToolOrgMaxPermission `json:"org_max_permission,omitempty"`
}

// MCPToolPermissionPolicy is a per-tool permission policy.
type MCPToolPermissionPolicy = string

// Per-tool permission policies.
const (
	MCPToolPolicyAlwaysAllow MCPToolPermissionPolicy = "always_allow"
	MCPToolPolicyAlwaysAsk   MCPToolPermissionPolicy = "always_ask"
	MCPToolPolicyAlwaysDeny  MCPToolPermissionPolicy = "always_deny"
)

// MCPToolOrgMaxPermission is an org admin's per-tool permission ceiling.
type MCPToolOrgMaxPermission = string

// Org permission ceilings.
const (
	MCPToolOrgMaxAllow   MCPToolOrgMaxPermission = "allow"
	MCPToolOrgMaxAsk     MCPToolOrgMaxPermission = "ask"
	MCPToolOrgMaxBlocked MCPToolOrgMaxPermission = "blocked"
)

// ---------------------------------------------------------------------------
// Option classification
// ---------------------------------------------------------------------------

// sdkMCPServers returns the in-process MCP servers configured on opts, keyed
// by the name they are registered under. Servers without an instance are not
// in-process servers and are left to --mcp-config.
func sdkMCPServers(opts *Options) map[string]*MCPSDKServerConfig {
	if opts == nil {
		return nil
	}
	var out map[string]*MCPSDKServerConfig
	for name, cfg := range opts.MCPServers {
		sdk, ok := asSDKServer(cfg)
		if !ok {
			continue
		}
		if out == nil {
			out = map[string]*MCPSDKServerConfig{}
		}
		out[name] = sdk
	}
	return out
}

// processMCPServers returns the servers the CLI runs itself, which go to
// --mcp-config. In-process SDK servers are declared in the initialize
// request instead.
func processMCPServers(servers map[string]MCPServerConfig) map[string]MCPServerConfig {
	out := make(map[string]MCPServerConfig, len(servers))
	for name, cfg := range servers {
		if _, ok := asSDKServer(cfg); ok {
			continue
		}
		out[name] = cfg
	}
	return out
}

// asSDKServer reports whether cfg is an in-process SDK server, one with a
// live handler. An SDK config without an instance is left to --mcp-config.
func asSDKServer(cfg MCPServerConfig) (*MCPSDKServerConfig, bool) {
	sdk, ok := cfg.(*MCPSDKServerConfig)
	if !ok || sdk == nil || isNilHandler(sdk.Instance) {
		return nil, false
	}
	return sdk, true
}

// isNilHandler reports whether h is nil or a typed nil pointer.
func isNilHandler(h MCPHandler) bool {
	if h == nil {
		return true
	}
	v := reflect.ValueOf(h)
	switch v.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Func, reflect.Interface, reflect.Slice, reflect.Chan:
		return v.IsNil()
	}
	return false
}
