package claude

// MCPServerConnectionStatus is the connection state of an MCP server.
type MCPServerConnectionStatus = string

// Known MCP server connection states.
const (
	MCPStatusConnected MCPServerConnectionStatus = "connected"
	MCPStatusFailed    MCPServerConnectionStatus = "failed"
	MCPStatusNeedsAuth MCPServerConnectionStatus = "needs-auth"
	MCPStatusPending   MCPServerConnectionStatus = "pending"
	MCPStatusDisabled  MCPServerConnectionStatus = "disabled"
)

// MCPToolAnnotations are the tool hints reported in MCP server status.
type MCPToolAnnotations struct {
	ReadOnly    *bool `json:"readOnly,omitzero"`
	Destructive *bool `json:"destructive,omitzero"`
	OpenWorld   *bool `json:"openWorld,omitzero"`
}

// MCPToolInfo describes one tool provided by an MCP server.
type MCPToolInfo struct {
	Name        string              `json:"name"`
	Description string              `json:"description,omitempty"`
	Annotations *MCPToolAnnotations `json:"annotations,omitzero"`
	// Meta holds the MCP Apps members of the tool's _meta (ui,
	// ui/resourceUri), for hosts that render the tool's ui:// resource.
	Meta map[string]any `json:"_meta,omitempty"`
}

// MCPServerInfo is the server identity from the MCP initialize handshake.
type MCPServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// MCPServerStatus is the status of one MCP server connection.
type MCPServerStatus struct {
	// Name is the server name as configured.
	Name string `json:"name"`
	// Status is the current connection state.
	Status MCPServerConnectionStatus `json:"status"`
	// ServerInfo is set once the server is connected.
	ServerInfo *MCPServerInfo `json:"serverInfo,omitzero"`
	// Error is set when Status is "failed".
	Error string `json:"error,omitempty"`
	// Config is the server's configuration as the CLI reports it. Its "type"
	// field is one of stdio, sse, http, sdk or the output-only
	// claudeai-proxy.
	Config map[string]any `json:"config,omitempty"`
	// Scope is the configuration scope (project, user, local, claudeai,
	// managed, ...).
	Scope string `json:"scope,omitempty"`
	// Source is where the server definition came from: "sdk" for an
	// in-process server this host registered, "plugin", or the
	// configuration scope. Key trust decisions on it rather than on the
	// name. Empty on CLIs that predate the field.
	Source string `json:"source,omitempty"`
	// Tools lists the server's tools, when connected.
	Tools []MCPToolInfo `json:"tools,omitempty"`
}

// MCPStatusResponse is the result of [Client.MCPServerStatus].
type MCPStatusResponse struct {
	MCPServers []MCPServerStatus `json:"mcpServers"`
	// Raw is the full response payload, including fields not modeled above.
	Raw map[string]any `json:"-"`
}

func (r *MCPStatusResponse) setRaw(m map[string]any) { r.Raw = m }
