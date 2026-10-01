package claude

import (
	"context"
	"errors"
	"time"
)

// call sends one control request on the connected session.
func (c *Client) call(ctx context.Context, subtype string, fields map[string]any) (map[string]any, error) {
	eng, err := c.engineOrErr()
	if err != nil {
		return nil, err
	}
	return eng.request(ctx, subtype, fields)
}

// SendControlRequest sends a control request the Client has no method for and
// returns the CLI's response payload (never nil on success). subtype sets the
// request's "subtype"; fields are the rest of the request, sent as-is. It is
// an escape hatch for internal and newer CLI requests whose shapes this
// package does not model; prefer the typed methods where they exist.
//
// Cancelling ctx abandons the request and tells the CLI with a
// control_cancel_request.
func (c *Client) SendControlRequest(ctx context.Context, subtype string, fields map[string]any) (map[string]any, error) {
	if subtype == "" {
		return nil, errors.New("claude: SendControlRequest needs a subtype")
	}
	return c.call(ctx, subtype, fields)
}

// InterruptWithReceipt aborts the current turn like Interrupt and returns the
// CLI's receipt: the queued messages that survive the interrupt, or with
// opts.CancelQueued the ones it cancelled. The receipt is nil from a CLI that
// predates it (no interrupt_receipt_v1 capability). opts may be nil.
func (c *Client) InterruptWithReceipt(ctx context.Context, opts *InterruptOptions) (*InterruptReceipt, error) {
	eng, err := c.engineOrErr()
	if err != nil {
		return nil, err
	}
	return eng.InterruptWithReceipt(ctx, opts != nil && opts.CancelQueued)
}

// InitializationResult reports the typed initialize response: commands,
// agents, models, account and output styles. It is nil before Connect. After
// Reinitialize it reports the latest response.
func (c *Client) InitializationResult() *InitializeResult {
	if eng, err := c.engineOrErr(); err == nil {
		return eng.InitializeResult()
	}
	return nil
}

// SupportedCommands lists the session's slash commands and skills. The list
// from the initialize response is replaced whenever the CLI reports a change
// (a commands_changed system message). It is nil before Connect.
func (c *Client) SupportedCommands() []SlashCommand {
	if eng, err := c.engineOrErr(); err == nil {
		return eng.SupportedCommands()
	}
	return nil
}

// SupportedModels lists the models the session can use. It is nil before
// Connect.
func (c *Client) SupportedModels() []ModelInfo {
	if r := c.InitializationResult(); r != nil {
		return r.Models
	}
	return nil
}

// SupportedAgents lists the subagents the session can invoke. It is nil
// before Connect.
func (c *Client) SupportedAgents() []AgentInfo {
	if r := c.InitializationResult(); r != nil {
		return r.Agents
	}
	return nil
}

// AccountInfo describes the logged-in account. It is nil before Connect.
func (c *Client) AccountInfo() *AccountInfo {
	if r := c.InitializationResult(); r != nil {
		info := r.Account
		return &info
	}
	return nil
}

// Reinitialize re-sends the initialize handshake on the live session, for a
// host that lost track of it (a transport gap, a reattached client). The same
// hook registrations are sent again, and permission prompts and dialogs the
// CLI still has open are delivered again to CanUseTool and OnUserDialog; a
// request already being answered is not answered twice.
func (c *Client) Reinitialize(ctx context.Context) (*InitializeResult, error) {
	eng, err := c.engineOrErr()
	if err != nil {
		return nil, err
	}
	return eng.initialize(ctx)
}

// ApplyFlagSettings merges settings into the session's flag settings layer
// mid-session. A nil value clears that key.
func (c *Client) ApplyFlagSettings(ctx context.Context, settings map[string]any) error {
	if settings == nil {
		settings = map[string]any{}
	}
	_, err := c.call(ctx, "apply_flag_settings", map[string]any{"settings": settings})
	return err
}

// UpdateSettings merges settings into a settings file through the CLI's own
// writer, the path /config uses, and applies them live. source is
// DestinationLocalSettings or DestinationUserSettings.
func (c *Client) UpdateSettings(ctx context.Context, source PermissionUpdateDestination, settings map[string]any) error {
	if settings == nil {
		settings = map[string]any{}
	}
	_, err := c.call(ctx, "update_settings", map[string]any{"source": source, "settings": settings})
	return err
}

// SetMCPPermissionModeOverride pins a tighten-only permission mode for one MCP
// server: MCPPermissionModeDefault forces per-action prompts,
// MCPPermissionModeAuto routes calls through the auto-mode classifier, and an
// empty mode clears the override. The override only applies when the
// session mode would auto-allow, so it never widens privilege.
func (c *Client) SetMCPPermissionModeOverride(ctx context.Context, serverName string, mode MCPPermissionModeOverride) (*MCPPermissionModeOverrideResult, error) {
	var value any
	if mode != "" {
		value = mode
	}
	return decodeControl[MCPPermissionModeOverrideResult](c.call(ctx, "set_mcp_permission_mode_override",
		map[string]any{"serverName": serverName, "mode": value}))
}

// ReloadPlugins reloads plugins from disk and returns the refreshed commands,
// agents, plugins and MCP servers. opts may be nil.
func (c *Client) ReloadPlugins(ctx context.Context, opts *ReloadPluginsOptions) (*ReloadPluginsResult, error) {
	fields := map[string]any{}
	if opts != nil && opts.HoldOnCacheImpact {
		fields["hold_on_cache_impact"] = true
	}
	return decodeControl[ReloadPluginsResult](c.call(ctx, "reload_plugins", fields))
}

// ReloadSkills reloads skills from disk and returns the refreshed list.
func (c *Client) ReloadSkills(ctx context.Context) (*ReloadSkillsResult, error) {
	return decodeControl[ReloadSkillsResult](c.call(ctx, "reload_skills", nil))
}

// ReloadOutputStyles re-reads the output style directories and returns the
// refreshed style names.
func (c *Client) ReloadOutputStyles(ctx context.Context) (*ReloadOutputStylesResult, error) {
	return decodeControl[ReloadOutputStylesResult](c.call(ctx, "reload_output_styles", nil))
}

// BackgroundTasks moves running foreground work to the background, as Ctrl+B
// does in the terminal: all of it, or with a non-empty toolUseID only that
// tool call. It reports whether anything was backgrounded (true when the CLI
// does not say).
func (c *Client) BackgroundTasks(ctx context.Context, toolUseID string) (bool, error) {
	fields := map[string]any{}
	if toolUseID != "" {
		fields["tool_use_id"] = toolUseID
	}
	data, err := c.call(ctx, "background_tasks", fields)
	if err != nil {
		return false, err
	}
	if b, ok := data["backgrounded"].(bool); ok {
		return b, nil
	}
	return true, nil
}

// SeedReadState records that path was read when it had modification time
// mtime, so a later Edit does not fail with "file not read yet" after the
// Read left the context. The seed is skipped if the file changed since.
func (c *Client) SeedReadState(ctx context.Context, path string, mtime time.Time) error {
	_, err := c.call(ctx, "seed_read_state", map[string]any{"path": path, "mtime": mtime.UnixMilli()})
	return err
}

// ReadFile reads a file through the CLI, resolved against its working
// directory and gated by the Read tool's permission rules. opts may be nil.
// Unlike the TypeScript SDK, which returns null on any failure, the error is
// returned.
func (c *Client) ReadFile(ctx context.Context, path string, opts *ReadFileOptions) (*ReadFileResult, error) {
	fields := map[string]any{"path": path}
	if opts != nil {
		if opts.MaxBytes > 0 {
			fields["max_bytes"] = opts.MaxBytes
		}
		if opts.Encoding != "" {
			fields["encoding"] = opts.Encoding
		}
	}
	return decodeControl[ReadFileResult](c.call(ctx, "read_file", fields))
}

// ReadMCPResource reads a ui:// MCP Apps resource from a connected MCP server
// the CLI dialed (not an in-process SDK server). The contents are untrusted
// third-party data: render them sandboxed.
//
// Alpha: this API may change.
func (c *Client) ReadMCPResource(ctx context.Context, serverName, uri string) (*MCPReadResourceResult, error) {
	return decodeControl[MCPReadResourceResult](c.call(ctx, "mcp_read_resource",
		map[string]any{"serverName": serverName, "uri": uri}))
}

// UsageExperimental reports the /usage data: session cost and usage totals,
// plan rate-limit utilization and what contributes to it. opts may be nil.
//
// Experimental: the TypeScript SDK names this
// usage_EXPERIMENTAL_MAY_CHANGE_DO_NOT_RELY_ON_THIS_API_YET; the shape may
// change without notice.
func (c *Client) UsageExperimental(ctx context.Context, opts *UsageOptions) (*UsageReport, error) {
	fields := map[string]any{}
	if opts != nil && opts.SkipBehaviors {
		fields["skip_behaviors"] = true
	}
	return decodeControl[UsageReport](c.call(ctx, "get_usage", fields))
}

// PermissionRules lists the session's live permission rules and workspace
// directories, as the CLI's /permissions command does. It never changes
// rules.
func (c *Client) PermissionRules(ctx context.Context) (*PermissionRulesState, error) {
	data, err := c.call(ctx, "list_permission_rules", nil)
	if err != nil {
		return nil, err
	}
	state, _ := data["state"].(map[string]any)
	if state == nil {
		state = map[string]any{}
	}
	return decodeControl[PermissionRulesState](state, nil)
}
