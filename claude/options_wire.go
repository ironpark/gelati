package claude

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
)

// launchConfig is what Options contribute to starting a session, resolved
// once: the CLI's flags and environment, for the subprocess transport, and the
// option-derived fields of the initialize request, for the engine. A custom
// Transport receives only the latter.
//
// Several options reach the CLI on two channels (SystemPrompt, Plugins,
// OutputFormat, Skills); each is rendered by one helper that writes both
// halves.
type launchConfig struct {
	// args are the CLI flags, without the program or script path.
	args []string
	// env is the CLI's complete environment as KEY=VALUE pairs, sorted by
	// key. It is the inherited environment overlaid with Options.Env and the
	// SDK's own variables; see buildEnv.
	env []string
	// initFields are the option-derived fields of the initialize request,
	// following the TypeScript SDK's buildInitializeRequest. The engine adds
	// only the hook registrations and the in-process MCP server
	// declarations.
	initFields map[string]any
}

// resolveLaunch renders opts as a launchConfig. opts may be nil. The SDK
// always drives the CLI in streaming-json mode on both directions, matching
// the reference SDKs, so large configuration (agents, hooks, most system
// prompts) can ride on the initialize request instead of the command line.
//
// opts should have passed prepareOptions, which validates option
// combinations; resolveLaunch reports only encoding failures and the
// conflicts it cannot render.
func resolveLaunch(opts *Options) (*launchConfig, error) {
	if opts == nil {
		opts = &Options{}
	}
	l := &launchConfig{initFields: map[string]any{}}
	l.flag("--output-format", "stream-json", "--verbose")
	l.addSystemPrompt(opts.SystemPrompt)

	switch tools := opts.Tools.(type) {
	case nil:
	case ToolList:
		l.flag("--tools", strings.Join(tools, ","))
	case ToolsPreset:
		// The claude_code preset maps to the CLI's "default" tool set.
		l.flag("--tools", "default")
	}
	l.addAllowedToolsAndSkills(opts)

	// Zero turns is dropped, as in the TypeScript SDK.
	if opts.MaxTurns != nil && *opts.MaxTurns != 0 {
		l.flag("--max-turns", strconv.Itoa(*opts.MaxTurns))
	}
	if opts.MaxBudgetUSD != nil {
		l.flag("--max-budget-usd", strconv.FormatFloat(*opts.MaxBudgetUSD, 'g', -1, 64))
	}
	if len(opts.DisallowedTools) > 0 {
		l.flag("--disallowedTools", strings.Join(opts.DisallowedTools, ","))
	}
	if opts.TaskBudget != nil {
		l.flag("--task-budget", strconv.Itoa(opts.TaskBudget.Total))
	}
	l.flagIf(opts.Model != "", "--model", opts.Model)
	l.flagIf(opts.Agent != "", "--agent", opts.Agent)
	l.flagIf(opts.FallbackModel != "", "--fallback-model", opts.FallbackModel)
	if len(opts.Betas) > 0 {
		l.flag("--betas", strings.Join(opts.Betas, ","))
	}
	if opts.DebugFile != "" {
		l.flag("--debug-file", opts.DebugFile)
	} else if opts.Debug {
		l.flag("--debug")
	}
	l.flagIf(opts.PermissionPromptToolName != "", "--permission-prompt-tool", opts.PermissionPromptToolName)
	l.flagIf(opts.PermissionPrompts != "", "--permission-prompts", opts.PermissionPrompts)
	l.flagIf(opts.PermissionMode != "", "--permission-mode", opts.PermissionMode)
	l.flagIf(opts.AllowDangerouslySkipPermissions, "--allow-dangerously-skip-permissions")
	l.flagIf(opts.ContinueConversation, "--continue")
	// The equals form binds a dash-leading value to its flag; in the
	// two-token form the CLI would parse it as a separate flag, which lets an
	// untrusted session name inject arbitrary options.
	l.flagIf(opts.Resume != "", "--resume="+opts.Resume)
	l.flagIf(opts.SessionID != "", "--session-id="+opts.SessionID)

	settings, err := buildSettingsValue(opts)
	if err != nil {
		return nil, err
	}
	l.flagIf(settings != "", "--settings", settings)
	if opts.ManagedSettings != nil {
		payload, err := json.Marshal(opts.ManagedSettings)
		if err != nil {
			return nil, fmt.Errorf("claude: encoding managed settings: %w", err)
		}
		l.flag("--managed-settings", string(payload))
	}
	l.flagIf(opts.ProjectConfigRoot != "", "--project-config-root="+opts.ProjectConfigRoot)
	for _, dir := range opts.AddDirs {
		l.flag("--add-dir", dir)
	}
	if opts.MCPConfigPath != "" {
		l.flag("--mcp-config", opts.MCPConfigPath)
	} else if servers := processMCPServers(opts.MCPServers); len(servers) > 0 {
		// In-process SDK servers are declared in the initialize request
		// (sdkMcpServers), as the TypeScript SDK does, not here.
		payload, err := json.Marshal(map[string]any{"mcpServers": servers})
		if err != nil {
			return nil, fmt.Errorf("claude: encoding mcp servers: %w", err)
		}
		l.flag("--mcp-config", string(payload))
	}
	l.flagIf(opts.IncludePartialMessages, "--include-partial-messages")
	l.flagIf(opts.IncludeHookEvents, "--include-hook-events")
	l.flagIf(opts.StrictMCPConfig, "--strict-mcp-config")
	l.flagIf(opts.ForkSession, "--fork-session")
	l.flagIf(opts.ResumeSessionAt != "", "--resume-session-at="+opts.ResumeSessionAt)
	l.flagIf(opts.ResumeDropsTurn != "", "--resume-drops-turn="+opts.ResumeDropsTurn)
	l.flagIf(opts.NoSessionPersistence, "--no-session-persistence")
	// With a store the CLI emits transcript_mirror frames, which the engine
	// forwards to it.
	l.flagIf(opts.SessionStore != nil, "--session-mirror")
	if opts.SettingSources != nil {
		l.flag("--setting-sources=" + strings.Join(*opts.SettingSources, ","))
	}
	l.addPlugins(opts)
	for _, flag := range sortedKeys(opts.ExtraArgs) {
		value := opts.ExtraArgs[flag]
		switch {
		case value == nil:
			l.flag("--" + flag)
		case len(*value) > 1 && strings.HasPrefix(*value, "-"):
			l.flag("--" + flag + "=" + *value)
		default:
			l.flag("--"+flag, *value)
		}
	}
	l.args = appendThinkingArgs(l.args, opts)
	l.flagIf(opts.Effort != "", "--effort", opts.Effort)
	if err := l.addOutputFormat(opts); err != nil {
		return nil, err
	}
	l.flag("--input-format", "stream-json")

	l.addInitializeOnlyFields(opts)
	l.env = buildEnv(opts)
	return l, nil
}

// flag appends CLI arguments.
func (l *launchConfig) flag(args ...string) {
	l.args = append(l.args, args...)
}

// flagIf appends CLI arguments when cond holds.
func (l *launchConfig) flagIf(cond bool, args ...string) {
	if cond {
		l.flag(args...)
	}
}

// ---------------------------------------------------------------------------
// Two-channel options
// ---------------------------------------------------------------------------

// addSystemPrompt renders Options.SystemPrompt the way the TypeScript SDK
// does: every form travels in the initialize request, so a long prompt does
// not count against the OS command-line limit, except SystemPromptFile, which
// is --system-prompt-file. nil becomes an empty custom prompt.
func (l *launchConfig) addSystemPrompt(prompt SystemPrompt) {
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
	case *SystemPromptFile:
		l.flag("--system-prompt-file", sp.Path)
	}
}

// addAllowedToolsAndSkills renders Options.AllowedTools and Options.Skills.
// Enabling skills implies the Skill tool, so --allowedTools gains the
// matching Skill rules and callers do not have to allow it by hand; an
// explicit skill list also filters the session's skills through the
// initialize request ("all" and unset are the same on the wire, so only a
// list is sent). Like the TypeScript SDK, the setting sources are left alone.
// Skill names were checked by validateOptions.
func (l *launchConfig) addAllowedToolsAndSkills(opts *Options) {
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
		l.initFields["skills"] = []string(skills)
	}
	if len(allowed) > 0 {
		l.flag("--allowedTools", strings.Join(allowed, ","))
	}
}

// addPlugins renders Options.Plugins: one --plugin-dir flag per plugin, or
// with PluginDeliveryInitialize the list in the initialize request and
// --await-initialize, so the command line does not grow with it.
func (l *launchConfig) addPlugins(opts *Options) {
	if len(opts.Plugins) == 0 {
		return
	}
	if opts.PluginDelivery != PluginDeliveryInitialize {
		for _, p := range opts.Plugins {
			flag := "--plugin-dir"
			if p.SkipMCPDiscovery {
				flag = "--plugin-dir-no-mcp"
			}
			l.flag(flag, p.Path)
		}
		return
	}
	l.flag("--await-initialize")
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

// addOutputFormat sends the schema of a json_schema Options.OutputFormat both
// as --json-schema and in the initialize request.
func (l *launchConfig) addOutputFormat(opts *Options) error {
	schema, ok := outputSchema(opts)
	if !ok {
		return nil
	}
	payload, err := json.Marshal(schema)
	if err != nil {
		return fmt.Errorf("claude: encoding output schema: %w", err)
	}
	l.flag("--json-schema", string(payload))
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

// appendThinkingArgs renders Options.Thinking, or the deprecated
// MaxThinkingTokens, as the TypeScript SDK does.
func appendThinkingArgs(args []string, opts *Options) []string {
	thinking := opts.Thinking
	if thinking == nil && opts.MaxThinkingTokens != nil {
		if *opts.MaxThinkingTokens == 0 {
			thinking = &ThinkingConfig{Type: ThinkingDisabled}
		} else {
			thinking = &ThinkingConfig{Type: ThinkingEnabled, BudgetTokens: opts.MaxThinkingTokens}
		}
	}
	if thinking == nil {
		return args
	}
	switch thinking.Type {
	case ThinkingAdaptive:
		args = append(args, "--thinking", "adaptive")
	case ThinkingEnabled:
		if thinking.BudgetTokens == nil {
			args = append(args, "--thinking", "adaptive")
		} else {
			args = append(args, "--max-thinking-tokens", strconv.Itoa(*thinking.BudgetTokens))
		}
	case ThinkingDisabled:
		args = append(args, "--thinking", "disabled")
	}
	if thinking.Type != ThinkingDisabled && thinking.Display != "" {
		args = append(args, "--thinking-display", thinking.Display)
	}
	return args
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
			encoded, err := json.Marshal(obj)
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
		if err := json.Unmarshal([]byte(text), &settings); err != nil {
			return "", fmt.Errorf("claude: parsing inline settings: %w", err)
		}
	}
	settings["sandbox"] = sandbox
	payload, err := json.Marshal(settings)
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
	case json.RawMessage:
		raw = value
	case []byte:
		raw = value
	default:
		encoded, err := json.Marshal(value)
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
	if err := json.Unmarshal(raw, &obj); err != nil {
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
	env["CLAUDE_AGENT_SDK_VERSION"] = Version
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
