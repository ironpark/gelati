package claude

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
