package antigravity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/ironpark/gelati/antigravity/internal/harness"
	"github.com/ironpark/gelati/antigravity/internal/wire"
)

// connectLocal launches the localharness binary and opens a Connection to
// it (upstream LocalConnectionStrategy). It is the only backend: upstream's
// OpenAI-compatible strategy (LocalOpenAIConnectionStrategy) only swaps the
// model list, which here is the OpenAIEndpoint model endpoint
// (Config.OpenAI).
//
// Seam for further backends: the LiteRT strategy, not ported, would adjust
// the HarnessConfig between buildHarnessConfig and Initialize below, and run
// a side server for the session's lifetime.
func connectLocal(ctx context.Context, cc *compiledConfig) (*Connection, error) {
	cfg := cc.cfg
	models := cfg.ResolvedModels()
	if err := validateModels(models); err != nil {
		return nil, err
	}
	hc, err := buildHarnessConfig(cc, models)
	if err != nil {
		return nil, err
	}
	saveDir := cfg.SaveDir
	if saveDir == "" {
		if saveDir, err = os.MkdirTemp("", "antigravity_"); err != nil {
			return nil, err
		}
		cc.logger.Info("no SaveDir specified; using a temporary directory", "dir", saveDir)
	}
	opts := harness.Options{
		CLIPath:          cfg.CLIPath,
		Env:              cfg.Env,
		StorageDirectory: saveDir,
		Stderr:           cfg.Stderr,
	}
	h, err := harness.Start(ctx, opts)
	if err != nil {
		if errors.Is(err, harness.ErrBinaryNotFound) {
			return nil, err
		}
		return nil, connectionErrorFrom(err)
	}
	resp, err := h.Initialize(ctx, hc)
	if err != nil {
		return nil, connectionErrorFrom(err)
	}
	init := parseInitializeResponse(resp)
	warnIfSandboxUnavailable(cc.logger, hc.GetHarnessSideTools().GetRunCommand(), init.sandbox)
	return newConnection(h, connectionOptions{
		tools:          cc.tools,
		hooks:          cc.hooks,
		dynamic:        cc.dynamic,
		logger:         cc.logger,
		initialHistory: init.history,
		initialUsage:   init.cumulativeUsage,
		trajUsages:     init.trajectoryUsages,
		sandbox:        init.sandbox,
		conversationID: init.conversationID,
	}), nil
}

// validateModels checks that every resolved model target has a usable
// endpoint.
func validateModels(models []ModelTarget) error {
	for _, m := range models {
		if m.Endpoint == nil {
			return validationErrorf("Model '%s' must have an endpoint configured.", m.Name)
		}
		if err := m.Endpoint.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// warnIfSandboxUnavailable logs once when the sandbox was requested for
// run_command but the harness cannot enforce it.
func warnIfSandboxUnavailable(logger *slog.Logger, rc *wire.RunCommandToolConfig, status *SandboxStatus) {
	if rc.GetEnabled() && rc.GetEnableSandbox() && status != nil && !status.Available {
		reason := status.UnavailableReason
		if reason == "" {
			reason = "reason unknown"
		}
		logger.Warn("EnableSandbox is set but the OS sandbox is unavailable in this harness environment; run_command will execute UNSANDBOXED", "reason", reason)
	}
}

// buildHarnessConfig translates a compiled configuration and its resolved
// models into the HarnessConfig sent at initialization.
func buildHarnessConfig(cc *compiledConfig, resolvedModels []ModelTarget) (*wire.HarnessConfig, error) {
	cfg := cc.cfg
	caps := cfg.capabilities().clone()
	schema, err := cfg.responseSchemaJSON()
	if err != nil {
		return nil, err
	}
	if schema != "" {
		caps.FinishToolSchemaJSON = schema
	}

	allTools := map[string]*wire.Tool{}
	if cc.tools != nil {
		for _, t := range cc.tools.list() {
			p, err := toolProto(t)
			if err != nil {
				return nil, err
			}
			allTools[t.name] = p
		}
	}
	rootTools, err := resolveToolRefs(cfg.Tools, cfg.ToolNames, allTools)
	if err != nil {
		return nil, err
	}

	workspaces, err := cfg.ResolvedWorkspaces()
	if err != nil {
		return nil, err
	}
	var workspaceProtos []*wire.Workspace
	for _, w := range workspaces {
		workspaceProtos = append(workspaceProtos, &wire.Workspace{FilesystemWorkspace: &wire.FilesystemWorkspace{Directory: new(filepath.ToSlash(w))}})
	}

	models, err := modelsProto(resolvedModels)
	if err != nil {
		return nil, err
	}

	var mcp []*wire.MCPServerConfig
	for _, srv := range cfg.MCPServers {
		mcp = append(mcp, mcpServerProto(srv))
	}

	var subagents []*wire.CustomAgent
	for _, sub := range cfg.Subagents {
		subCaps := sub.Capabilities
		if subCaps == nil {
			subCaps = &SubagentCapabilities{EnabledTools: ReadOnlyTools()}
		}
		tools, err := resolveToolRefs(sub.Tools, sub.ToolNames, allTools)
		if err != nil {
			return nil, err
		}
		ca := &wire.CustomAgent{
			Name:               new(sub.Name),
			Description:        new(sub.Description),
			SystemInstructions: systemInstructionsProto(sub.SystemInstructions),
			HarnessSideTools:   subagentToolsProto(subCaps),
			Tools:              tools,
			AgentBehavior:      new(agentBehaviorProto(subCaps.behavior())),
		}
		if sub.Model != "" {
			ca.Model = &wire.ModelConfig{Name: new(sub.Model)}
		}
		subagents = append(subagents, ca)
	}

	compaction, legacyThreshold := compactionProto(cfg.Compaction, caps)
	appDataDir := cfg.AppDataDir
	if appDataDir == "" {
		appDataDir = defaultAppDataDir()
	}
	hc := &wire.HarnessConfig{
		Tools:                   rootTools,
		SystemInstructions:      systemInstructionsProto(cfg.SystemInstructions),
		CascadeID:               new(cfg.ConversationID),
		SessionContinuationMode: new(sessionModeProto(cfg.SessionContinuationMode)),
		Models:                  models,
		Workspaces:              workspaceProtos,
		SkillsPaths:             slices.Clone(cfg.SkillsPaths),
		HarnessSideTools:        rootToolsProto(caps),
		CompactionThreshold:     new(legacyThreshold),
		CompactionConfig:        compaction,
		FinishToolSchemaJSON:    new(caps.FinishToolSchemaJSON),
		AppDataDir:              new(appDataDir),
		MCPServers:              mcp,
		EnabledHooks:            enabledHooks(cc.hooks),
		CustomSubagents:         subagents,
		AgentBehavior:           new(agentBehaviorProto(caps.behavior())),
		RetryConfig:             retryProto(cfg.Retry),
		BudgetConfig:            budgetProto(cfg.Budget),
		PolicyConfig:            cc.policy,
	}
	if t := caps.ToolOutputTruncation; t != nil {
		hc.ToolOutputTruncation = &wire.ToolOutputTruncation{Truncate: &wire.ToolOutputTruncationTruncateStrategy{MaxTokens: new(int32(t.MaxTokens))}}
	}
	return hc, nil
}

// toolProto declares a custom tool to the harness.
func toolProto(t *Tool) (*wire.Tool, error) {
	b, err := json.Marshal(t.Schema())
	if err != nil {
		return nil, fmt.Errorf("antigravity: encode schema of tool %q: %w", t.name, err)
	}
	return &wire.Tool{Name: new(t.name), Description: new(t.description), ParametersJSONSchema: new(string(b))}, nil
}

// resolveToolRefs builds the tool declarations of an agent: its custom
// tools, then tools referenced by name (a known custom tool's declaration,
// or a bare name for the harness to resolve).
func resolveToolRefs(tools []*Tool, names []string, all map[string]*wire.Tool) ([]*wire.Tool, error) {
	var out []*wire.Tool
	for _, t := range tools {
		p, ok := all[t.name]
		if !ok {
			var err error
			if p, err = toolProto(t); err != nil {
				return nil, err
			}
			all[t.name] = p
		}
		out = append(out, p)
	}
	for _, n := range names {
		if p, ok := all[n]; ok {
			out = append(out, p)
		} else {
			out = append(out, &wire.Tool{Name: new(n)})
		}
	}
	return out, nil
}

func agentBehaviorProto(b AgentBehavior) wire.AgentBehavior {
	switch b {
	case AgentBehaviorInteractive:
		return wire.AgentBehaviorInteractive
	case AgentBehaviorMinimal:
		return wire.AgentBehaviorMinimal
	}
	return wire.AgentBehaviorAutonomous
}

func sessionModeProto(m SessionContinuationMode) wire.HarnessConfigSessionContinuationMode {
	switch m {
	case SessionResume:
		return wire.HarnessConfigSessionContinuationModeResume
	case SessionCreateOrResume:
		return wire.HarnessConfigSessionContinuationModeCreateOrResume
	case SessionCreateOnly:
		return wire.HarnessConfigSessionContinuationModeCreateOnly
	}
	return wire.HarnessConfigSessionContinuationModeUnspecified
}

// systemInstructionsProto converts system instructions; nil and empty text
// yield nil.
func systemInstructionsProto(si SystemInstructions) *wire.SystemInstructions {
	switch v := si.(type) {
	case nil:
		return nil
	case TextSystemInstructions:
		if v == "" {
			return nil
		}
		return systemInstructionsProto(TemplatedSystemInstructions{Sections: []SystemInstructionSection{{Content: string(v)}}})
	case CustomSystemInstructions:
		return &wire.SystemInstructions{Custom: &wire.CustomSystemInstructions{Part: []*wire.CustomSystemInstructionsPart{{Text: new(v.Text)}}}}
	case *CustomSystemInstructions:
		if v == nil {
			return nil
		}
		return systemInstructionsProto(*v)
	case TemplatedSystemInstructions:
		appended := &wire.AppendedSystemInstructions{}
		if v.Identity != "" {
			appended.CustomIdentity = new(v.Identity)
		}
		for _, sec := range v.Sections {
			title := sec.Title
			if title == "" {
				title = "user_system_instructions"
			}
			appended.AppendedSections = append(appended.AppendedSections, &wire.AppendedSystemInstructionsSection{Title: new(title), Content: new(sec.Content)})
		}
		return &wire.SystemInstructions{Appended: appended}
	case *TemplatedSystemInstructions:
		if v == nil {
			return nil
		}
		return systemInstructionsProto(*v)
	}
	return nil
}

func enabled(set map[BuiltinTool]bool, t BuiltinTool) *bool { return new(set[t]) }

func runCommandProto(active map[BuiltinTool]bool, rc *RunCommandConfig) *wire.RunCommandToolConfig {
	out := &wire.RunCommandToolConfig{
		Enabled:              enabled(active, BuiltinRunCommand),
		EnableDaemonCommands: new(false),
		MaxTimeoutMs:         new(uint32(0)),
		EnableSandbox:        new(false),
	}
	if rc != nil {
		out.EnableDaemonCommands = new(rc.EnableDaemons)
		out.EnableSandbox = new(rc.EnableSandbox)
		if rc.Timeout > 0 {
			ms := math.RoundToEven(float64(rc.Timeout) / float64(time.Millisecond))
			out.MaxTimeoutMs = new(uint32(min(ms, math.MaxUint32)))
		}
	}
	return out
}

// sideToolsProto builds the toggles of the builtin tools.
func sideToolsProto(active map[BuiltinTool]bool, subagents *wire.SubagentsConfig, rc *RunCommandConfig) *wire.HarnessSideTools {
	return &wire.HarnessSideTools{
		Subagents:      subagents,
		Find:           &wire.FindToolConfig{Enabled: enabled(active, BuiltinFindFile)},
		UserQuestions:  &wire.UserQuestionsConfig{Enabled: enabled(active, BuiltinAskQuestion)},
		RunCommand:     runCommandProto(active, rc),
		ManageTask:     &wire.ManageTaskToolConfig{Enabled: new(active[BuiltinRunCommand] || active[BuiltinSchedule])},
		Schedule:       &wire.ScheduleToolConfig{Enabled: enabled(active, BuiltinSchedule)},
		FileEdit:       &wire.FileEditToolConfig{Enabled: enabled(active, BuiltinEditFile)},
		ViewFile:       &wire.ViewFileToolConfig{Enabled: enabled(active, BuiltinViewFile)},
		WriteToFile:    &wire.WriteToFileToolConfig{Enabled: enabled(active, BuiltinCreateFile)},
		GrepSearch:     &wire.GrepSearchToolConfig{Enabled: enabled(active, BuiltinSearchDir)},
		ListDir:        &wire.ListDirToolConfig{Enabled: enabled(active, BuiltinListDir)},
		GenerateImage:  &wire.GenerateImageToolConfig{Enabled: enabled(active, BuiltinGenerateImage)},
		SearchWeb:      &wire.SearchWebToolConfig{Enabled: enabled(active, BuiltinSearchWeb)},
		ReadURLContent: &wire.ReadURLContentToolConfig{Enabled: enabled(active, BuiltinReadURLContent)},
	}
}

func rootToolsProto(caps *CapabilitiesConfig) *wire.HarnessSideTools {
	active := resolveActiveTools(caps.EnabledTools, caps.DisabledTools)
	sub := &wire.SubagentsConfig{
		Enabled:          new(!caps.DisableSubagents && active[BuiltinStartSubagent]),
		AllowedSubagents: slices.Clone(caps.AllowedSubagents),
	}
	if caps.MaxSubagentDepth > 0 {
		sub.MaxNestingDepth = new(int32(caps.MaxSubagentDepth))
	}
	return sideToolsProto(active, sub, caps.RunCommand)
}

func subagentToolsProto(caps *SubagentCapabilities) *wire.HarnessSideTools {
	active := resolveActiveTools(caps.EnabledTools, caps.DisabledTools)
	sub := &wire.SubagentsConfig{
		Enabled:          new(active[BuiltinStartSubagent]),
		AllowedSubagents: slices.Clone(caps.AllowedSubagents),
	}
	return sideToolsProto(active, sub, caps.RunCommand)
}

// compactionProto returns the compaction config and the legacy threshold
// field; Compaction takes precedence over the deprecated
// CapabilitiesConfig.CompactionThreshold.
func compactionProto(cc *CompactionConfig, caps *CapabilitiesConfig) (*wire.CompactionConfig, uint32) {
	threshold := 0
	switch {
	case cc != nil:
		threshold = cc.TokenThreshold
	case caps.CompactionThreshold > 0:
		threshold = caps.CompactionThreshold
	default:
		return nil, 0
	}
	t := uint32(min(max(threshold, 0), math.MaxUint32))
	return &wire.CompactionConfig{TokenThreshold: new(t)}, t
}

func geminiOptionsProto(o *GeminiModelOptions) *wire.GeminiModelOptions {
	if o.isEmpty() {
		return nil
	}
	p := &wire.GeminiModelOptions{}
	if o.ThinkingLevel != "" {
		p.ThinkingLevel = new(string(o.ThinkingLevel))
	}
	if o.ServiceTier != "" {
		p.ServiceTier = new(string(o.ServiceTier))
	}
	return p
}

var modelTypeProtos = map[ModelType]wire.ModelType{ModelTypeText: wire.ModelTypeText, ModelTypeImage: wire.ModelTypeImage}

func modelsProto(models []ModelTarget) ([]*wire.ModelConfig, error) {
	var out []*wire.ModelConfig
	for _, m := range models {
		mc := &wire.ModelConfig{Name: new(m.Name)}
		for _, t := range m.types() {
			wt, ok := modelTypeProtos[t]
			if !ok {
				wt = wire.ModelTypeUnspecified
			}
			mc.Types = append(mc.Types, wt)
		}
		switch e := m.Endpoint.(type) {
		case *GeminiAPIEndpoint:
			mc.GeminiAPIEndpoint = &wire.GeminiAPIEndpoint{
				BaseURL:     new(e.BaseURL),
				HTTPHeaders: maps.Clone(e.HTTPHeaders),
				APIKey:      new(e.APIKey),
				Options:     geminiOptionsProto(e.Options),
			}
		case *VertexEndpoint:
			mc.VertexEndpoint = &wire.VertexEndpoint{
				BaseURL:     new(e.BaseURL),
				HTTPHeaders: maps.Clone(e.HTTPHeaders),
				Project:     new(e.Project),
				Location:    new(e.Location),
				APIKey:      new(e.APIKey),
				Options:     geminiOptionsProto(e.Options),
			}
		case *OpenAIEndpoint:
			// Upstream's LocalOpenAIConnectionStrategy sends the server as a
			// GemmaEndpoint, the harness's OpenAI-compatible client.
			mc.GemmaEndpoint = &wire.GemmaEndpoint{BaseURL: new(e.BaseURL)}
		default:
			return nil, validationErrorf("Unrecognized endpoint type: %T", m.Endpoint)
		}
		out = append(out, mc)
	}
	return out, nil
}

func mcpServerProto(s MCPServer) *wire.MCPServerConfig {
	c := s.mcpServer()
	out := &wire.MCPServerConfig{
		Name:           new(c.name),
		EnabledTools:   slices.Clone(c.enabledTools),
		DisabledTools:  slices.Clone(c.disabledTools),
		TimeoutSeconds: new(int32(c.timeoutSeconds)),
	}
	switch s := s.(type) {
	case *MCPStdioServer:
		out.Stdio = &wire.MCPStdioTransport{Command: new(s.Command), Args: slices.Clone(s.Args), Env: maps.Clone(s.Env)}
	case *MCPStreamableHTTPServer:
		out.HTTP = &wire.MCPHTTPTransport{URL: new(s.URL), Headers: maps.Clone(s.Headers)}
	}
	return out
}

func retryProto(r *RetryConfig) *wire.RetryConfig {
	if r == nil {
		return nil
	}
	out := &wire.RetryConfig{}
	if a := r.APIRetry; a != nil && (a.MaxRetries != nil || a.InitialSleepDurationMs != nil || a.ExponentialMultiplier != nil || a.JitterRange != nil) {
		out.APIRetry = &wire.ModelAPIRetryConfig{
			MaxRetries:             a.MaxRetries,
			InitialSleepDurationMs: a.InitialSleepDurationMs,
			ExponentialMultiplier:  a.ExponentialMultiplier,
			JitterRange:            a.JitterRange,
		}
	}
	if o := r.ModelOutputRetry; o != nil && o.MaxRetries != nil {
		out.ModelOutputRetry = &wire.ModelOutputRetryConfig{MaxRetries: o.MaxRetries}
	}
	if out.APIRetry == nil && out.ModelOutputRetry == nil {
		return nil
	}
	return out
}

func budgetProto(b *BudgetConfig) *wire.BudgetConfig {
	if b == nil || (b.MaxModelCalls == 0 && b.MaxToolCalls == 0 && b.MaxInputTokens == 0 && b.MaxOutputTokens == 0 && b.MaxTotalTokens == 0) {
		return nil
	}
	out := &wire.BudgetConfig{}
	if b.MaxModelCalls > 0 {
		out.MaxModelCalls = new(int32(b.MaxModelCalls))
	}
	if b.MaxToolCalls > 0 {
		out.MaxToolCalls = new(int32(b.MaxToolCalls))
	}
	if b.MaxInputTokens > 0 {
		out.MaxInputTokens = new(wire.Int64(b.MaxInputTokens))
	}
	if b.MaxOutputTokens > 0 {
		out.MaxOutputTokens = new(wire.Int64(b.MaxOutputTokens))
	}
	if b.MaxTotalTokens > 0 {
		out.MaxTotalTokens = new(wire.Int64(b.MaxTotalTokens))
	}
	scope := wire.BudgetConfigBudgetScopeLifetime
	if b.Scope == BudgetScopeForwardLooking {
		scope = wire.BudgetConfigBudgetScopeForwardLooking
	}
	out.Scope = &scope
	return out
}

// enabledHooks lists the lifecycle hooks the harness should call back for:
// the kinds with at least one registered hook.
func enabledHooks(h *hookRunner) []wire.LifecycleHook {
	if h == nil {
		return nil
	}
	var out []wire.LifecycleHook
	for kind, lh := range lifecycleHooks {
		if lh != "" && h.has(hookKind(kind)) {
			out = append(out, lh)
		}
	}
	return out
}
