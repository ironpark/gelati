package claude

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/ironpark/gelati/internal/jsonx"
)

// launchConfig is what Options contribute to starting a session, resolved
// once per session by resolveLaunch: the option-derived fields of the
// initialize request, for the engine, and the encoded values of the CLI flags
// whose encoding can fail, so those errors surface before anything is
// spawned, whatever the transport. Only the subprocess transport renders the
// full command line (commandArgs) and environment (buildEnv), when it starts
// the CLI.
//
// Several options reach the CLI on two channels (SystemPrompt, Plugins,
// OutputFormat, Skills): resolveLaunch writes their initialize half and
// commandArgs their flag half.
type launchConfig struct {
	// initFields are the option-derived fields of the initialize request,
	// following the TypeScript SDK's buildInitializeRequest. The engine adds
	// only the hook registrations and the in-process MCP server
	// declarations.
	initFields map[string]any

	// The encoded values of --settings, --managed-settings, an inline
	// --mcp-config and --json-schema; empty when the flag is not passed.
	settings, managedSettings, mcpConfig, jsonSchema string
}

// resolveLaunch resolves opts as a launchConfig. The SDK always drives the
// CLI in streaming-json mode on both directions, matching the reference SDKs,
// so large configuration (agents, hooks, most system prompts) can ride on the
// initialize request instead of the command line.
//
// prepareOptions calls it after validating option combinations; resolveLaunch
// reports the encoding failures and the conflicts it cannot render.
func resolveLaunch(opts *Options) (*launchConfig, error) {
	l := &launchConfig{initFields: map[string]any{}}
	l.addSystemPromptFields(opts.SystemPrompt)
	if skills, ok := opts.Skills.(SkillList); ok {
		// "all" and unset are the same on the wire, so only a list is sent.
		l.initFields["skills"] = []string(skills)
	}

	var err error
	if l.settings, err = buildSettingsValue(opts); err != nil {
		return nil, err
	}
	if opts.ManagedSettings != nil {
		payload, err := json.Marshal(opts.ManagedSettings, jsonx.LegacyEncode)
		if err != nil {
			return nil, fmt.Errorf("claude: encoding managed settings: %w", err)
		}
		l.managedSettings = string(payload)
	}
	if opts.MCPConfigPath == "" {
		if servers := processMCPServers(opts.MCPServers); len(servers) > 0 {
			// In-process SDK servers are declared in the initialize
			// request (sdkMcpServers), as the TypeScript SDK does, not
			// here.
			payload, err := json.Marshal(map[string]any{"mcpServers": servers}, jsonx.LegacyEncode)
			if err != nil {
				return nil, fmt.Errorf("claude: encoding mcp servers: %w", err)
			}
			l.mcpConfig = string(payload)
		}
	}
	l.addPluginFields(opts)
	if err := l.addOutputFormat(opts); err != nil {
		return nil, err
	}
	l.addInitializeOnlyFields(opts)
	return l, nil
}

// commandArgs renders the CLI flags for opts, the options l was resolved
// from or a copy differing only in single-channel flags (a materialized
// resume's Resume and ContinueConversation). They exclude the program or
// script path.
func (l *launchConfig) commandArgs(opts *Options) []string {
	var a cliArgs
	a.flag("--output-format", "stream-json", "--verbose")
	if sp, ok := opts.SystemPrompt.(*SystemPromptFile); ok {
		// The only system prompt form that is not sent in the initialize
		// request; see addSystemPromptFields.
		a.flag("--system-prompt-file", sp.Path)
	}

	switch tools := opts.Tools.(type) {
	case nil:
	case ToolList:
		a.flag("--tools", strings.Join(tools, ","))
	case ToolsPreset:
		// The claude_code preset maps to the CLI's "default" tool set.
		a.flag("--tools", "default")
	}
	a.addAllowedTools(opts)

	// Zero turns is dropped, as in the TypeScript SDK.
	if opts.MaxTurns != nil && *opts.MaxTurns != 0 {
		a.flag("--max-turns", strconv.Itoa(*opts.MaxTurns))
	}
	if opts.MaxBudgetUSD != nil {
		a.flag("--max-budget-usd", strconv.FormatFloat(*opts.MaxBudgetUSD, 'g', -1, 64))
	}
	if len(opts.DisallowedTools) > 0 {
		a.flag("--disallowedTools", strings.Join(opts.DisallowedTools, ","))
	}
	if opts.TaskBudget != nil {
		a.flag("--task-budget", strconv.Itoa(opts.TaskBudget.Total))
	}
	a.flagIf(opts.Model != "", "--model", opts.Model)
	a.flagIf(opts.Agent != "", "--agent", opts.Agent)
	a.flagIf(opts.FallbackModel != "", "--fallback-model", opts.FallbackModel)
	if len(opts.Betas) > 0 {
		a.flag("--betas", strings.Join(opts.Betas, ","))
	}
	if opts.DebugFile != "" {
		a.flag("--debug-file", opts.DebugFile)
	} else if opts.Debug {
		a.flag("--debug")
	}
	a.flagIf(opts.PermissionPromptToolName != "", "--permission-prompt-tool", opts.PermissionPromptToolName)
	a.flagIf(opts.PermissionPrompts != "", "--permission-prompts", opts.PermissionPrompts)
	a.flagIf(opts.PermissionMode != "", "--permission-mode", opts.PermissionMode)
	a.flagIf(opts.AllowDangerouslySkipPermissions, "--allow-dangerously-skip-permissions")
	a.flagIf(opts.ContinueConversation, "--continue")
	// The equals form binds a dash-leading value to its flag; in the
	// two-token form the CLI would parse it as a separate flag, which lets an
	// untrusted session name inject arbitrary options.
	a.flagIf(opts.Resume != "", "--resume="+opts.Resume)
	a.flagIf(opts.SessionID != "", "--session-id="+opts.SessionID)

	a.flagIf(l.settings != "", "--settings", l.settings)
	a.flagIf(opts.ManagedSettings != nil, "--managed-settings", l.managedSettings)
	a.flagIf(opts.ProjectConfigRoot != "", "--project-config-root="+opts.ProjectConfigRoot)
	for _, dir := range opts.AddDirs {
		a.flag("--add-dir", dir)
	}
	if opts.MCPConfigPath != "" {
		a.flag("--mcp-config", opts.MCPConfigPath)
	} else {
		a.flagIf(l.mcpConfig != "", "--mcp-config", l.mcpConfig)
	}
	a.flagIf(opts.IncludePartialMessages, "--include-partial-messages")
	a.flagIf(opts.IncludeHookEvents, "--include-hook-events")
	a.flagIf(opts.StrictMCPConfig, "--strict-mcp-config")
	a.flagIf(opts.ForkSession, "--fork-session")
	a.flagIf(opts.ResumeSessionAt != "", "--resume-session-at="+opts.ResumeSessionAt)
	a.flagIf(opts.ResumeDropsTurn != "", "--resume-drops-turn="+opts.ResumeDropsTurn)
	a.flagIf(opts.NoSessionPersistence, "--no-session-persistence")
	// With a store the CLI emits transcript_mirror frames, which the engine
	// forwards to it.
	a.flagIf(opts.SessionStore != nil, "--session-mirror")
	if opts.SettingSources != nil {
		a.flag("--setting-sources=" + strings.Join(*opts.SettingSources, ","))
	}
	a.addPluginFlags(opts)
	for _, flag := range sortedKeys(opts.ExtraArgs) {
		value := opts.ExtraArgs[flag]
		switch {
		case value == nil:
			a.flag("--" + flag)
		case len(*value) > 1 && strings.HasPrefix(*value, "-"):
			a.flag("--" + flag + "=" + *value)
		default:
			a.flag("--"+flag, *value)
		}
	}
	a.addThinking(opts)
	a.flagIf(opts.Effort != "", "--effort", opts.Effort)
	a.flagIf(l.jsonSchema != "", "--json-schema", l.jsonSchema)
	a.flag("--input-format", "stream-json")
	return a
}

// cliArgs accumulates CLI flags.
type cliArgs []string

// flag appends CLI arguments.
func (a *cliArgs) flag(args ...string) {
	*a = append(*a, args...)
}

// flagIf appends CLI arguments when cond holds.
func (a *cliArgs) flagIf(cond bool, args ...string) {
	if cond {
		a.flag(args...)
	}
}

// ---------------------------------------------------------------------------
// Two-channel options
// ---------------------------------------------------------------------------

// addSystemPromptFields renders Options.SystemPrompt the way the TypeScript
// SDK does: every form travels in the initialize request, so a long prompt
// does not count against the OS command-line limit, except SystemPromptFile,
// which commandArgs passes as --system-prompt-file. nil becomes an empty
// custom prompt.
func (l *launchConfig) addSystemPromptFields(prompt SystemPrompt) {
	fields := l.initFields
	switch sp := prompt.(type) {
	case nil:
		fields["systemPrompt"] = []string{""}
	case SystemPromptText:
		fields["systemPrompt"] = []string{string(sp)}
	case SystemPromptBlocks:
		fields["systemPrompt"] = nonNilStrings(sp)
	case *SystemPromptCustom:
		fields["systemPrompt"] = nonNilStrings(sp.Prompt)
		if sp.Snapshot != nil {
			fields["systemPromptSnapshot"] = *sp.Snapshot
		}
	case *SystemPromptPreset:
		if sp.Append != "" {
			fields["appendSystemPrompt"] = sp.Append
		}
		if sp.ExcludeDynamicSections {
			fields["excludeDynamicSections"] = true
		}
		if sp.Snapshot != nil {
			fields["systemPromptSnapshot"] = *sp.Snapshot
		}
	}
}

// addAllowedTools renders Options.AllowedTools and the flag half of
// Options.Skills. Enabling skills implies the Skill tool, so --allowedTools
// gains the matching Skill rules and callers do not have to allow it by hand;
// an explicit skill list also filters the session's skills through the
// initialize request (see resolveLaunch). Like the TypeScript SDK, the
// setting sources are left alone. Skill names were checked by
// validateOptions.
func (a *cliArgs) addAllowedTools(opts *Options) {
	allowed := slices.Clone(opts.AllowedTools)
	switch skills := opts.Skills.(type) {
	case SkillsAll:
		if !slices.Contains(allowed, "Skill") {
			allowed = append(allowed, "Skill")
		}
	case SkillList:
		for _, name := range skills {
			rule := "Skill(" + name + ")"
			if !slices.Contains(allowed, rule) {
				allowed = append(allowed, rule)
			}
		}
	}
	if len(allowed) > 0 {
		a.flag("--allowedTools", strings.Join(allowed, ","))
	}
}

// addPluginFields renders the initialize half of Options.Plugins: with
// PluginDeliveryInitialize the list travels in the initialize request, so the
// command line does not grow with it.
func (l *launchConfig) addPluginFields(opts *Options) {
	if len(opts.Plugins) == 0 || opts.PluginDelivery != PluginDeliveryInitialize {
		return
	}
	plugins := make([]map[string]any, 0, len(opts.Plugins))
	for _, p := range opts.Plugins {
		plugin := map[string]any{"type": "local", "path": p.Path}
		if p.SkipMCPDiscovery {
			plugin["skipMcpDiscovery"] = true
		}
		plugins = append(plugins, plugin)
	}
	l.initFields["plugins"] = plugins
}

// addPluginFlags renders the flag half of Options.Plugins: one --plugin-dir
// flag per plugin, or --await-initialize when the list is in the initialize
// request.
func (a *cliArgs) addPluginFlags(opts *Options) {
	if len(opts.Plugins) == 0 {
		return
	}
	if opts.PluginDelivery == PluginDeliveryInitialize {
		a.flag("--await-initialize")
		return
	}
	for _, p := range opts.Plugins {
		flag := "--plugin-dir"
		if p.SkipMCPDiscovery {
			flag = "--plugin-dir-no-mcp"
		}
		a.flag(flag, p.Path)
	}
}

// addOutputFormat sends the schema of a json_schema Options.OutputFormat both
// in the initialize request and, encoded here, as --json-schema.
func (l *launchConfig) addOutputFormat(opts *Options) error {
	schema, ok := outputSchema(opts)
	if !ok {
		return nil
	}
	payload, err := json.Marshal(schema, jsonx.LegacyEncode)
	if err != nil {
		return fmt.Errorf("claude: encoding output schema: %w", err)
	}
	l.jsonSchema = string(payload)
	l.initFields["jsonSchema"] = schema
	return nil
}

// outputSchema returns the JSON schema of a json_schema OutputFormat.
func outputSchema(opts *Options) (any, bool) {
	if opts.OutputFormat == nil || opts.OutputFormat["type"] != "json_schema" {
		return nil, false
	}
	schema, ok := opts.OutputFormat["schema"]
	return schema, ok && schema != nil
}

// ---------------------------------------------------------------------------
// Single-channel options
// ---------------------------------------------------------------------------

// addInitializeOnlyFields renders the options that reach the CLI only through
// the initialize request: agents, dialog kinds, plan-mode instructions, tool
// aliases, the session title and the opt-in UX features.
func (l *launchConfig) addInitializeOnlyFields(opts *Options) {
	fields := l.initFields
	if len(opts.Agents) > 0 {
		fields["agents"] = opts.Agents
	}
	if opts.ForwardSubagentText {
		fields["forwardSubagentText"] = true
	}
	if len(opts.SupportedDialogKinds) > 0 {
		fields["supportedDialogKinds"] = opts.SupportedDialogKinds
	}
	if opts.PlanModeInstructions != "" {
		fields["planModeInstructions"] = opts.PlanModeInstructions
	}
	if len(opts.ToolAliases) > 0 {
		fields["toolAliases"] = opts.ToolAliases
	}
	if opts.Title != "" {
		fields["title"] = opts.Title
	}
	if opts.PromptSuggestions {
		fields["promptSuggestions"] = true
	}
	if opts.AgentProgressSummaries {
		fields["agentProgressSummaries"] = true
	}
	if opts.PerTaskStopAffordance {
		fields["perTaskStopAffordance"] = true
	}
}

// addThinking renders Options.Thinking, or the deprecated MaxThinkingTokens,
// as the TypeScript SDK does.
func (a *cliArgs) addThinking(opts *Options) {
	thinking := opts.Thinking
	if thinking == nil && opts.MaxThinkingTokens != nil {
		if *opts.MaxThinkingTokens == 0 {
			thinking = &ThinkingConfig{Type: ThinkingDisabled}
		} else {
			thinking = &ThinkingConfig{Type: ThinkingEnabled, BudgetTokens: opts.MaxThinkingTokens}
		}
	}
	if thinking == nil {
		return
	}
	switch thinking.Type {
	case ThinkingAdaptive:
		a.flag("--thinking", "adaptive")
	case ThinkingEnabled:
		if thinking.BudgetTokens == nil {
			a.flag("--thinking", "adaptive")
		} else {
			a.flag("--max-thinking-tokens", strconv.Itoa(*thinking.BudgetTokens))
		}
	case ThinkingDisabled:
		a.flag("--thinking", "disabled")
	}
	a.flagIf(thinking.Type != ThinkingDisabled && thinking.Display != "", "--thinking-display", thinking.Display)
}

// nonNilStrings keeps an empty list encoding as [] rather than null.
func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// ---------------------------------------------------------------------------
// Settings
// ---------------------------------------------------------------------------

// buildSettingsValue renders --settings: Options.Settings, with
// Options.Sandbox merged in as its "sandbox" key. A sandbox that is enabled
// without failIfUnavailable gets failIfUnavailable: true.
func buildSettingsValue(opts *Options) (string, error) {
	var (
		text   string
		isPath bool
	)
	switch value := opts.Settings.(type) {
	case nil:
	case string:
		text = value
		isPath = isSettingsPath(value)
	default:
		obj, ok, err := encodeJSONObject(value, "Options.Settings")
		if err != nil {
			return "", err
		}
		if ok {
			encoded, err := json.Marshal(obj, jsonx.LegacyEncode)
			if err != nil {
				return "", fmt.Errorf("claude: encoding Options.Settings: %w", err)
			}
			text = string(encoded)
		}
	}

	sandbox, ok, err := encodeJSONObject(opts.Sandbox, "Options.Sandbox")
	if err != nil {
		return "", err
	}
	if !ok {
		return text, nil
	}
	if isPath {
		return "", errSettingsPathWithSandbox
	}
	if sandbox["enabled"] == true {
		if _, set := sandbox["failIfUnavailable"]; !set {
			sandbox["failIfUnavailable"] = true
		}
	}
	settings := map[string]any{}
	if strings.TrimSpace(text) != "" {
		if err := jsonx.Unmarshal([]byte(text), &settings); err != nil {
			return "", fmt.Errorf("claude: parsing inline settings: %w", err)
		}
	}
	settings["sandbox"] = sandbox
	payload, err := json.Marshal(settings, jsonx.LegacyEncode)
	if err != nil {
		return "", fmt.Errorf("claude: encoding settings: %w", err)
	}
	return string(payload), nil
}

// errSettingsPathWithSandbox rejects a sandbox that has no settings object to
// be merged into.
var errSettingsPathWithSandbox = errors.New("claude: cannot use both a settings file path and Options.Sandbox; " +
	"include the sandbox configuration in the settings file instead")

// isSettingsPath reports whether a string Options.Settings names a settings
// file rather than holding inline JSON.
func isSettingsPath(s string) bool {
	return strings.TrimSpace(s) != "" && !isInlineJSONObject(s)
}

// encodeJSONObject encodes v and decodes it back as a JSON object. A nil
// value, or one that encodes to null, reports ok == false.
func encodeJSONObject(v any, what string) (map[string]any, bool, error) {
	var raw []byte
	switch value := v.(type) {
	case nil:
		return nil, false, nil
	case jsontext.Value:
		raw = value
	case []byte:
		raw = value
	default:
		encoded, err := json.Marshal(value, jsonx.LegacyEncode)
		if err != nil {
			return nil, false, fmt.Errorf("claude: encoding %s: %w", what, err)
		}
		raw = encoded
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, false, nil
	}
	var obj map[string]any
	if err := jsonx.Unmarshal(raw, &obj); err != nil {
		return nil, false, fmt.Errorf("claude: %s must be a JSON object: %w", what, err)
	}
	return obj, true, nil
}

// isInlineJSONObject reports whether a --settings string is inline JSON
// rather than a file path, using the TypeScript SDK's test.
func isInlineJSONObject(s string) bool {
	t := strings.TrimSpace(s)
	return strings.HasPrefix(t, "{") && strings.HasSuffix(t, "}")
}

// ---------------------------------------------------------------------------
// Environment
// ---------------------------------------------------------------------------

// buildEnv renders the child process environment: this process's
// environment, minus CLAUDECODE, overlaid with Options.Env (which
// prepareOptions gives a CLAUDE_CODE_ENTRYPOINT) and the SDK's own variables.
func buildEnv(opts *Options) []string {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "CLAUDECODE" {
			// CLAUDECODE is dropped so an SDK-spawned CLI does not think
			// it is running inside a Claude Code parent.
			continue
		}
		env[k] = v
	}
	maps.Copy(env, opts.Env)
	env["CLAUDE_AGENT_SDK_VERSION"] = Version()
	// The engine reads the CLI's session_state_changed frames to tell when
	// a run has ended; a caller-chosen value (any case) is kept.
	if !hasKeyFold(env, "CLAUDE_CODE_SDK_READS_SESSION_STATE") {
		env["CLAUDE_CODE_SDK_READS_SESSION_STATE"] = "1"
	}
	if opts.EnableFileCheckpointing {
		env["CLAUDE_CODE_ENABLE_SDK_FILE_CHECKPOINTING"] = "true"
	}
	if tc := opts.ToolConfig; tc != nil && tc.AskUserQuestion != nil && tc.AskUserQuestion.PreviewFormat != "" {
		env["CLAUDE_CODE_QUESTION_PREVIEW_FORMAT"] = tc.AskUserQuestion.PreviewFormat
	}
	// As in the TypeScript SDK: NODE_OPTIONS from the parent must not alter
	// the CLI's runtime, and DEBUG (used by many Node libraries) is only
	// passed on as DEBUG=1 when SDK debugging is requested.
	delete(env, "NODE_OPTIONS")
	if envTruthy(env["DEBUG_CLAUDE_AGENT_SDK"]) {
		env["DEBUG"] = "1"
	} else {
		delete(env, "DEBUG")
	}
	if opts.Cwd != "" {
		env["PWD"] = opts.Cwd
	}
	out := make([]string, 0, len(env))
	for _, k := range sortedKeys(env) {
		out = append(out, k+"="+env[k])
	}
	return out
}

// hasKeyFold reports whether env has key, ignoring case.
func hasKeyFold(env map[string]string, key string) bool {
	for k := range env {
		if strings.EqualFold(k, key) {
			return true
		}
	}
	return false
}

// sortedKeys returns m's keys in order, so rendered command lines and
// environments are deterministic.
func sortedKeys[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}
