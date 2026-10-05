package claude

// Typed request options and responses of the outbound control requests. Each
// response keeps the full payload in Raw, so fields a newer CLI adds stay
// reachable without an SDK release.

// ---------------------------------------------------------------------------
// Initialize
// ---------------------------------------------------------------------------

// SlashCommand describes one slash command or skill.
type SlashCommand struct {
	// Name is the command name without the leading slash.
	Name        string `json:"name"`
	Description string `json:"description"`
	// ArgumentHint describes the arguments, e.g. "<file>".
	ArgumentHint string `json:"argumentHint"`
	// Aliases are alternate names that resolve to this command.
	Aliases []string `json:"aliases,omitempty"`
	// Builtin is true for Claude Code's own commands; false for commands
	// defined by a user, project, plugin or MCP server.
	Builtin bool `json:"builtin,omitzero"`
}

// ModelInfo describes one model the session can use.
type ModelInfo struct {
	// Value is the identifier to pass to SetModel or Options.Model.
	Value string `json:"value"`
	// ResolvedModel is the canonical model ID Value resolves to.
	ResolvedModel string `json:"resolvedModel,omitempty"`
	DisplayName   string `json:"displayName"`
	Description   string `json:"description"`
	// SupportsEffort reports whether the model supports effort levels,
	// listed in SupportedEffortLevels.
	SupportsEffort           bool          `json:"supportsEffort,omitzero"`
	SupportedEffortLevels    []EffortLevel `json:"supportedEffortLevels,omitempty"`
	SupportsAdaptiveThinking bool          `json:"supportsAdaptiveThinking,omitzero"`
	SupportsFastMode         bool          `json:"supportsFastMode,omitzero"`
	SupportsAutoMode         bool          `json:"supportsAutoMode,omitzero"`
}

// AgentInfo describes a subagent invokable through the Agent tool.
type AgentInfo struct {
	// Name is the agent type, e.g. "Explore".
	Name        string `json:"name"`
	Description string `json:"description"`
	// Model is an alias, a model ID or "inherit"; empty uses the default
	// subagent model.
	Model string `json:"model,omitempty"`
}

// AccountInfo describes the logged-in account.
type AccountInfo struct {
	Email            string `json:"email,omitempty"`
	Organization     string `json:"organization,omitempty"`
	SubscriptionType string `json:"subscriptionType,omitempty"`
	TokenSource      string `json:"tokenSource,omitempty"`
	APIKeySource     string `json:"apiKeySource,omitempty"`
	// APIProvider is the active API backend: firstParty, bedrock, vertex,
	// foundry, anthropicAws, anthropicGoogleCloud, mantle or gateway.
	APIProvider string `json:"apiProvider,omitempty"`
}

// InitializeResult is the CLI's answer to the initialize handshake.
type InitializeResult struct {
	Commands []SlashCommand `json:"commands"`
	Agents   []AgentInfo    `json:"agents"`
	// OutputStyle is the active output style.
	OutputStyle           string      `json:"output_style"`
	AvailableOutputStyles []string    `json:"available_output_styles"`
	Models                []ModelInfo `json:"models"`
	Account               AccountInfo `json:"account"`
	// HooksApplied reports whether the hooks this initialize carried were
	// registered; nil when it carried none or the CLI predates the field.
	HooksApplied *bool `json:"hooks_applied,omitzero"`
	// PluginsApplied reports whether every plugin the request listed is
	// loaded; nil when it listed none or the CLI predates the field.
	PluginsApplied *bool `json:"plugins_applied,omitzero"`
	// SDKMCPManifestsParked reports, per in-process server, what became of
	// its pre-captured MCP manifest (parked, already_connected,
	// protocol_version_mismatch, malformed or not_honoured).
	SDKMCPManifestsParked map[string]string `json:"sdk_mcp_manifests_parked,omitempty"`
	// FastModeState is off, cooldown or on.
	FastModeState          string `json:"fast_mode_state,omitempty"`
	FastModeDisabledReason string `json:"fast_mode_disabled_reason,omitempty"`
	// Raw is the full response payload.
	Raw map[string]any `json:"-"`
}

// ---------------------------------------------------------------------------
// Interrupt and rewind
// ---------------------------------------------------------------------------

// InterruptOptions tunes Client.InterruptWithReceipt.
type InterruptOptions struct {
	// CancelQueued also cancels every queued main-thread message, listing
	// them in InterruptReceipt.Cancelled. Older CLIs ignore it.
	CancelQueued bool
}

// InterruptReceipt is the CLI's answer to an interrupt, on CLIs that
// advertise the interrupt_receipt_v1 capability.
type InterruptReceipt struct {
	// StillQueued are the UUIDs of queued user messages that survive the
	// interrupt and will still run unless cancelled. Only uuid-stamped
	// main-thread messages are listed, and the list may include UUIDs this
	// client never sent.
	StillQueued []string `json:"still_queued"`
	// Cancelled lists the messages cancelled because CancelQueued was set.
	Cancelled []string `json:"cancelled,omitempty"`
}

// RewindFilesOptions tunes Client.RewindFiles.
type RewindFilesOptions struct {
	// DryRun previews the rewind without changing files.
	DryRun bool
}

// RewindFilesResult reports what a rewind changed, or would change.
type RewindFilesResult struct {
	CanRewind    bool     `json:"canRewind"`
	Error        string   `json:"error,omitempty"`
	FilesChanged []string `json:"filesChanged,omitempty"`
	Insertions   int      `json:"insertions,omitzero"`
	Deletions    int      `json:"deletions,omitzero"`
	// SkippedLinks counts tracked files left alone because a link or other
	// non-regular file sat at their path. Set by real rewinds only.
	SkippedLinks int `json:"skippedLinks,omitzero"`
	// Raw is the full response payload.
	Raw map[string]any `json:"-"`
}

// ---------------------------------------------------------------------------
// Context usage
// ---------------------------------------------------------------------------

// ContextUsageDetail selects how Client.ContextUsage counts tokens.
type ContextUsageDetail = string

// Context usage detail levels.
const (
	// ContextUsageFull counts each category with the token-count API (the
	// CLI default).
	ContextUsageFull ContextUsageDetail = "full"
	// ContextUsageSummary answers from the last response's usage and local
	// estimates, without per-category token-count calls.
	ContextUsageSummary ContextUsageDetail = "summary"
)

// ContextUsageOptions tunes Client.ContextUsage.
type ContextUsageOptions struct {
	// Detail is ContextUsageFull, ContextUsageSummary or empty for the CLI
	// default.
	Detail ContextUsageDetail
}

// ---------------------------------------------------------------------------
// Settings, plugins, skills, output styles
// ---------------------------------------------------------------------------

// MCPPermissionModeOverride is a tighten-only per-server permission mode.
type MCPPermissionModeOverride = string

// MCP permission mode overrides. An empty mode clears the override.
const (
	// MCPPermissionModeDefault forces per-action prompts.
	MCPPermissionModeDefault MCPPermissionModeOverride = "default"
	// MCPPermissionModeAuto routes the server's calls through the
	// auto-mode classifier.
	MCPPermissionModeAuto MCPPermissionModeOverride = "auto"
)

// MCPPermissionModeOverrideResult is the answer to
// Client.SetMCPPermissionModeOverride.
type MCPPermissionModeOverrideResult struct {
	// Warning is set when the server name matches no known MCP server. The
	// override is stored anyway and applies once such a server connects.
	Warning string `json:"warning,omitempty"`
}

// ReloadPluginsOptions tunes Client.ReloadPlugins.
type ReloadPluginsOptions struct {
	// HoldOnCacheImpact skips the reload when it would change the tool list
	// the conversation's prompt cache depends on; the result then has
	// Held set and describes the impact.
	HoldOnCacheImpact bool
}

// PluginInfo describes one loaded plugin.
type PluginInfo struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Source string `json:"source,omitempty"`
	// Version is the plugin manifest's version, verbatim
	// (plugin-controlled).
	Version string `json:"version,omitempty"`
}

// PluginCacheImpact is what applying a held plugin reload would change.
type PluginCacheImpact struct {
	MCPServersAdded   []string `json:"mcp_servers_added"`
	MCPServersRemoved []string `json:"mcp_servers_removed"`
	// LSPToolChange is adds, may-add, removes, may-remove or empty.
	LSPToolChange string `json:"lsp_tool_change,omitempty"`
}

// ReloadPluginsResult is the session's components after a plugin reload.
type ReloadPluginsResult struct {
	Commands   []SlashCommand    `json:"commands"`
	Agents     []AgentInfo       `json:"agents"`
	Plugins    []PluginInfo      `json:"plugins"`
	MCPServers []MCPServerStatus `json:"mcpServers"`
	ErrorCount int               `json:"error_count"`
	// Held is set when HoldOnCacheImpact was requested: true means the
	// reload was not applied and CacheImpact says why.
	Held        *bool              `json:"held,omitzero"`
	CacheImpact *PluginCacheImpact `json:"cache_impact,omitzero"`
	// Raw is the full response payload.
	Raw map[string]any `json:"-"`
}

// ReloadSkillsResult is the refreshed skill list.
type ReloadSkillsResult struct {
	Skills []SlashCommand `json:"skills"`
	// Raw is the full response payload.
	Raw map[string]any `json:"-"`
}

// ReloadOutputStylesResult is the refreshed list of output styles.
type ReloadOutputStylesResult struct {
	AvailableOutputStyles []string `json:"available_output_styles"`
	// Raw is the full response payload.
	Raw map[string]any `json:"-"`
}

// ---------------------------------------------------------------------------
// Files and MCP resources
// ---------------------------------------------------------------------------

// ReadFileEncoding selects how Client.ReadFile encodes the file contents.
type ReadFileEncoding = string

// File content encodings.
const (
	ReadFileUTF8   ReadFileEncoding = "utf-8"
	ReadFileBase64 ReadFileEncoding = "base64"
)

// ReadFileOptions tunes Client.ReadFile.
type ReadFileOptions struct {
	// MaxBytes caps the bytes read; zero uses the CLI default.
	MaxBytes int
	// Encoding is ReadFileUTF8 (the default, lossy for binary files) or
	// ReadFileBase64.
	Encoding ReadFileEncoding
}

// ReadFileResult is a file read through the CLI's permission gate.
type ReadFileResult struct {
	Contents  string `json:"contents"`
	AbsPath   string `json:"absPath"`
	Truncated bool   `json:"truncated,omitzero"`
	// Encoding is "base64" when requested and honored; empty means utf-8.
	Encoding ReadFileEncoding `json:"encoding,omitempty"`
}

// MCPResourceContent is one item of an MCP resources/read result.
type MCPResourceContent struct {
	URI      string `json:"uri"`
	MIMEType string `json:"mimeType,omitempty"`
	Text     string `json:"text,omitempty"`
	// Blob is base64 data for a binary item.
	Blob string         `json:"blob,omitempty"`
	Meta map[string]any `json:"_meta,omitempty"`
}

// MCPReadResourceResult is the server's resources/read result.
type MCPReadResourceResult struct {
	Contents []MCPResourceContent `json:"contents"`
	// Raw is the full response payload.
	Raw map[string]any `json:"-"`
}

// ---------------------------------------------------------------------------
// Usage (experimental)
// ---------------------------------------------------------------------------

// UsageOptions tunes Client.UsageExperimental.
type UsageOptions struct {
	// SkipBehaviors skips the local transcript scan behind
	// UsageReport.Behaviors.
	SkipBehaviors bool
}

// UsageWindow is one plan rate-limit window.
type UsageWindow struct {
	// Utilization is the share of the window used, 0-100.
	Utilization *float64 `json:"utilization"`
	// ResetsAt is an ISO 8601 timestamp.
	ResetsAt *string `json:"resets_at"`
}

// UsageModelWindow is a per-model weekly rate-limit window.
type UsageModelWindow struct {
	DisplayName string   `json:"display_name"`
	Utilization *float64 `json:"utilization"`
	ResetsAt    *string  `json:"resets_at"`
}

// UsageExtraUsage describes pay-as-you-go usage beyond the plan.
type UsageExtraUsage struct {
	IsEnabled    bool     `json:"is_enabled"`
	MonthlyLimit *float64 `json:"monthly_limit"`
	UsedCredits  *float64 `json:"used_credits"`
	Utilization  *float64 `json:"utilization"`
	Currency     *string  `json:"currency,omitzero"`
}

// UsageRateLimits holds the plan's rate-limit windows.
type UsageRateLimits struct {
	FiveHour          *UsageWindow       `json:"five_hour,omitzero"`
	SevenDay          *UsageWindow       `json:"seven_day,omitzero"`
	SevenDayOAuthApps *UsageWindow       `json:"seven_day_oauth_apps,omitzero"`
	SevenDayOpus      *UsageWindow       `json:"seven_day_opus,omitzero"`
	SevenDaySonnet    *UsageWindow       `json:"seven_day_sonnet,omitzero"`
	ModelScoped       []UsageModelWindow `json:"model_scoped,omitempty"`
	ExtraUsage        *UsageExtraUsage   `json:"extra_usage,omitzero"`
}

// UsageSession is the cost and usage accumulated by the current session.
type UsageSession struct {
	TotalCostUSD       float64               `json:"total_cost_usd"`
	TotalAPIDurationMS int64                 `json:"total_api_duration_ms"`
	TotalDurationMS    int64                 `json:"total_duration_ms"`
	TotalLinesAdded    int                   `json:"total_lines_added"`
	TotalLinesRemoved  int                   `json:"total_lines_removed"`
	ModelUsage         map[string]ModelUsage `json:"model_usage"`
}

// UsageReport is the /usage data. Experimental: the shape may change.
type UsageReport struct {
	Session UsageSession `json:"session"`
	// SubscriptionType is pro, max, team, enterprise, or nil for API-key and
	// third-party provider sessions.
	SubscriptionType *string `json:"subscription_type"`
	// RateLimitsAvailable is false when plan rate limits do not apply;
	// RateLimits is then nil.
	RateLimitsAvailable bool             `json:"rate_limits_available"`
	RateLimits          *UsageRateLimits `json:"rate_limits"`
	// Behaviors is the local transcript scan ("day" and "week" windows),
	// passed through as-is; nil when unavailable or skipped.
	Behaviors map[string]any `json:"behaviors"`
	// Raw is the full response payload.
	Raw map[string]any `json:"-"`
}

// ---------------------------------------------------------------------------
// Permission rules
// ---------------------------------------------------------------------------

// PermissionRuleDescription is a plain-language reading of a rule, split
// for display with an emphasized middle part.
type PermissionRuleDescription struct {
	Prefix   string `json:"prefix"`
	Emphasis string `json:"emphasis,omitempty"`
	Suffix   string `json:"suffix,omitempty"`
}

// PermissionRuleEntry is one live permission rule with its provenance.
type PermissionRuleEntry struct {
	Behavior PermissionBehavior `json:"behavior"`
	// Source is where the rule comes from: userSettings, projectSettings,
	// localSettings, flagSettings, policySettings, cliArg, command,
	// session, toolsNarrowing, mcpServerPolicy or hostCredential.
	Source string `json:"source"`
	// Rule is the stored rule string verbatim. It may carry control
	// characters: escape it before display.
	Rule        string                     `json:"rule"`
	Description *PermissionRuleDescription `json:"description,omitzero"`
	// Editability is persistent, session or readonly.
	Editability string `json:"editability"`
	// NotInEffect marks a rule ignored because managed settings allow
	// policy rules only.
	NotInEffect bool `json:"notInEffect,omitzero"`
}

// PermissionWorkspaceDirectory is one additional working directory in the
// permission scope.
type PermissionWorkspaceDirectory struct {
	Path string `json:"path"`
	// Source is a settings source, cliArg (--add-dir) or session.
	Source string `json:"source"`
}

// SettingsParseError reports a settings file that failed to parse or
// validate.
type SettingsParseError struct {
	File string `json:"file,omitempty"`
	// Path is the dot-notation field path, empty for whole-file errors.
	Path    string `json:"path"`
	Message string `json:"message"`
}

// PermissionRulesState is the session's live permission rules, as the
// CLI's /permissions command lists them.
type PermissionRulesState struct {
	Rules                []PermissionRuleEntry          `json:"rules"`
	WorkspaceDirectories []PermissionWorkspaceDirectory `json:"workspaceDirectories"`
	OriginalCwd          string                         `json:"originalCwd"`
	// ManagedOnly is true when managed settings allow policy rules only.
	ManagedOnly bool `json:"managedOnly"`
	// Errors lists settings files that were skipped.
	Errors []SettingsParseError `json:"errors,omitempty"`
	// Raw is the "state" object of the response.
	Raw map[string]any `json:"-"`
}

func (r *RewindFilesResult) setRaw(m map[string]any)        { r.Raw = m }
func (r *ReloadPluginsResult) setRaw(m map[string]any)      { r.Raw = m }
func (r *ReloadSkillsResult) setRaw(m map[string]any)       { r.Raw = m }
func (r *ReloadOutputStylesResult) setRaw(m map[string]any) { r.Raw = m }
func (r *MCPReadResourceResult) setRaw(m map[string]any)    { r.Raw = m }
func (r *UsageReport) setRaw(m map[string]any)              { r.Raw = m }
func (r *PermissionRulesState) setRaw(m map[string]any)     { r.Raw = m }
