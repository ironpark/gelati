package agy

import (
	"encoding/json/jsontext"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/ironpark/gelati/agy/internal/wire"
	"github.com/ironpark/gelati/internal/jsonx"
	"github.com/ironpark/gelati/internal/logx"
)

// Options configures an Agent backed by the local harness (upstream
// LocalAgentConfig, which extends the abstract AgentConfig).
//
// The zero Options is usable: it runs DefaultModel against the Gemini API
// with the key from GEMINI_API_KEY, enables the default builtin tools,
// denies run_command (ConfirmRunCommand policies) and uses the current
// directory as the workspace.
//
// Where upstream distinguishes "not set" from "set to empty", nil and empty
// slices differ here: a nil Policies or Workspaces gets the default, while
// an empty non-nil one means none.
type Options struct {
	// SystemInstructions customize the agent's instructions.
	SystemInstructions SystemInstructions
	// Capabilities configure tools and behavior; nil means the defaults
	// (every DefaultTools tool, autonomous behavior, subagents enabled).
	Capabilities *CapabilitiesConfig
	// Tools are custom tools the agent can call.
	Tools []*Tool
	// ToolNames reference tools by name for the root agent: custom tools
	// declared on a subagent, or tools known to the harness.
	ToolNames []string
	// Policies govern which tool calls may run. Nil means
	// ConfirmRunCommand: run_command is denied and everything else allowed.
	// Use policy.AllowAll() for fully autonomous execution.
	Policies []Policy
	// Hooks observe and steer the agent's lifecycle.
	Hooks []Hook
	// Triggers run alongside the session and can push messages into it.
	Triggers []Trigger
	// MCPServers are Model Context Protocol servers whose tools the agent
	// can use.
	MCPServers []MCPServer
	// Workspaces are the directories the agent works in; file tools are
	// confined to them. Nil means the current directory. Paths may be
	// relative, start with "~", or be file:// or cns:// URIs.
	Workspaces []string
	// ConversationID resumes or names a session. It must be at least 32
	// characters of [a-zA-Z0-9-].
	ConversationID string
	// SessionContinuationMode says how ConversationID is used; SessionResume
	// requires ConversationID.
	SessionContinuationMode SessionContinuationMode
	// SaveDir is where the harness stores trajectories (needed to resume).
	// Empty uses a new temporary directory, which is left in place.
	SaveDir string
	// AppDataDir is the harness's app data directory; it must be absolute.
	// Empty uses ~/.gemini/antigravity.
	AppDataDir string
	// ResponseSchema asks the agent for structured output (read with
	// TurnResult.StructuredOutput). It may be a JSON schema as a string,
	// []byte, jsontext.Value or map[string]any; a reflect.Type; or any
	// other value, whose type's schema is derived as for tool parameters.
	ResponseSchema any
	// SkillsPaths are directories to load skills from.
	SkillsPaths []string
	// Subagents are static subagents the agent can delegate to.
	Subagents []SubagentConfig
	Retry     *RetryConfig
	Budget    *BudgetConfig
	// Compaction configures context compaction.
	Compaction *CompactionConfig

	// Model is shorthand for a text model target using the endpoint built
	// from APIKey, Vertex, Project and Location, or OpenAI when set.
	Model string
	// Models are explicit model targets. Model and the defaults
	// (DefaultModel for text, DefaultImageGenerationModel for images) are
	// added for the types they do not cover.
	Models []ModelTarget
	// APIKey authenticates the shorthand endpoint. Empty falls back to
	// GEMINI_API_KEY.
	APIKey string
	// Vertex routes the shorthand endpoint to Vertex AI. It is also enabled
	// by GOOGLE_GENAI_USE_VERTEXAI or GOOGLE_GENAI_USE_ENTERPRISE set to
	// "true" or "1".
	Vertex bool
	// Project and Location select the Vertex AI project; empty values fall
	// back to GOOGLE_CLOUD_PROJECT and GOOGLE_CLOUD_LOCATION.
	Project  string
	Location string
	// OpenAI, when set, runs the agent on an OpenAI-compatible chat
	// completions server (Ollama, LM Studio, vLLM, ...) instead of Gemini
	// (upstream LocalOpenAIAgentConfig): Model names the model on that
	// server, and no default Gemini models are added, so no Gemini
	// credentials are needed. APIKey, Vertex, Project and Location are
	// ignored. Explicit Models are still sent alongside.
	OpenAI *OpenAIEndpoint

	// Env holds extra environment variables for the harness process, merged
	// over this process's environment. The harness also receives them in its
	// input config, as upstream sends them.
	Env map[string]string
	// CLIPath is the localharness executable. Empty uses
	// ANTIGRAVITY_HARNESS_PATH (in Env, then the process environment), then
	// localharness on PATH.
	CLIPath string
	// Stderr, when set, receives the harness's stderr output.
	Stderr io.Writer
	// Logger receives the SDK's diagnostics. Nil passes warnings and errors
	// to slog's default logger, much as Python's logging does by default.
	Logger *slog.Logger

	// modelsResolved marks Models as already merged with the shorthand and
	// the defaults (set by Eval).
	modelsResolved bool
}

func (o Options) logger() *slog.Logger {
	return logx.Or(o.Logger)
}

// clone returns a copy of o whose slices and nested configs are its own.
// Tools, hooks, triggers and policies keep their identity.
func (o Options) clone() Options {
	o.Capabilities = o.Capabilities.clone()
	o.Tools = slices.Clone(o.Tools)
	o.ToolNames = slices.Clone(o.ToolNames)
	o.Policies = slices.Clone(o.Policies)
	o.Hooks = slices.Clone(o.Hooks)
	o.Triggers = slices.Clone(o.Triggers)
	o.MCPServers = slices.Clone(o.MCPServers)
	o.Workspaces = slices.Clone(o.Workspaces)
	o.SkillsPaths = slices.Clone(o.SkillsPaths)
	o.Subagents = slices.Clone(o.Subagents)
	for i := range o.Subagents {
		s := &o.Subagents[i]
		s.Tools = slices.Clone(s.Tools)
		s.ToolNames = slices.Clone(s.ToolNames)
		s.Capabilities = s.Capabilities.clone()
	}
	if o.Retry != nil {
		r := *o.Retry
		o.Retry = &r
	}
	if o.Budget != nil {
		b := *o.Budget
		o.Budget = &b
	}
	if o.Compaction != nil {
		cc := *o.Compaction
		o.Compaction = &cc
	}
	o.Models = slices.Clone(o.Models)
	for i := range o.Models {
		o.Models[i] = o.Models[i].clone()
	}
	if o.OpenAI != nil {
		e := *o.OpenAI
		o.OpenAI = &e
	}
	o.Env = maps.Clone(o.Env)
	return o
}

func (o Options) capabilities() *CapabilitiesConfig {
	if o.Capabilities == nil {
		return &CapabilitiesConfig{}
	}
	return o.Capabilities
}

func (o Options) policies() []Policy {
	if o.Policies == nil {
		return ConfirmRunCommandPolicies(nil)
	}
	return o.Policies
}

// vertex reports whether the shorthand endpoint routes to Vertex AI.
func (o Options) vertex() bool {
	if o.Vertex {
		return true
	}
	for _, name := range []string{"GOOGLE_GENAI_USE_VERTEXAI", "GOOGLE_GENAI_USE_ENTERPRISE"} {
		switch strings.ToLower(os.Getenv(name)) {
		case "true", "1":
			return true
		}
	}
	return false
}

// shorthandEndpoint builds the endpoint of the shorthand and default models.
func (o Options) shorthandEndpoint() ModelEndpoint {
	if o.vertex() {
		return (&VertexEndpoint{Project: o.Project, Location: o.Location, APIKey: o.APIKey}).withEnvDefaults()
	}
	return &GeminiAPIEndpoint{APIKey: o.APIKey}
}

// ResolvedModels returns the model targets the session uses: the explicit
// Models, then the Model shorthand, then the defaults for the model types
// not yet covered. With OpenAI set, the shorthand targets that endpoint
// and no defaults are added. Vertex endpoints have their environment
// defaults filled in.
func (o Options) ResolvedModels() []ModelTarget {
	var merged []ModelTarget
	for _, m := range o.Models {
		merged = append(merged, m.clone())
	}
	if !o.modelsResolved && o.OpenAI != nil {
		e := *o.OpenAI
		merged = append(merged, ModelTarget{Name: o.Model, Types: []ModelType{ModelTypeText}, Endpoint: &e})
	} else if !o.modelsResolved {
		endpoint := o.shorthandEndpoint()
		if o.Model != "" {
			merged = append(merged, ModelTarget{Name: o.Model, Types: []ModelType{ModelTypeText}, Endpoint: endpoint})
		}
		present := map[ModelType]bool{}
		for _, m := range merged {
			for _, t := range m.types() {
				present[t] = true
			}
		}
		for _, d := range []ModelTarget{
			{Name: DefaultModel, Types: []ModelType{ModelTypeText}, Endpoint: endpoint},
			{Name: DefaultImageGenerationModel, Types: []ModelType{ModelTypeImage}, Endpoint: endpoint},
		} {
			if !slices.ContainsFunc(d.Types, func(t ModelType) bool { return present[t] }) {
				merged = append(merged, d.clone())
			}
		}
	}
	for i := range merged {
		if v, ok := merged[i].Endpoint.(*VertexEndpoint); ok {
			merged[i].Endpoint = v.withEnvDefaults()
		}
	}
	return merged
}

// ResolvedWorkspaces returns the normalized workspace directories.
func (o Options) ResolvedWorkspaces() ([]string, error) {
	if o.Workspaces == nil {
		wd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		return []string{wd}, nil
	}
	out := make([]string, 0, len(o.Workspaces))
	for _, w := range o.Workspaces {
		n, err := normalizeWorkspacePath(w)
		if err != nil {
			return nil, validationErrorf("invalid workspace %q: %v", w, err)
		}
		out = append(out, n)
	}
	return out, nil
}

// responseSchemaJSON converts ResponseSchema to a JSON string.
func (o Options) responseSchemaJSON() (string, error) {
	switch s := o.ResponseSchema.(type) {
	case nil:
		return "", nil
	case string:
		if !jsontext.Value(s).IsValid() {
			return "", validationErrorf("response_schema string is not valid JSON.")
		}
		return s, nil
	case []byte:
		if !jsontext.Value(s).IsValid() {
			return "", validationErrorf("response_schema is not valid JSON.")
		}
		return string(s), nil
	case jsontext.Value:
		if !s.IsValid() {
			return "", validationErrorf("response_schema is not valid JSON.")
		}
		return string(s), nil
	case map[string]any:
		b, err := jsonx.Marshal(s)
		if err != nil {
			return "", &ValidationError{Message: "response_schema is not JSON-encodable: " + err.Error(), Err: err}
		}
		return string(b), nil
	case reflect.Type:
		b, err := jsonx.Marshal(schemaForType(s))
		return string(b), err
	}
	b, err := jsonx.Marshal(schemaForType(reflect.TypeOf(o.ResponseSchema)))
	return string(b), err
}

// Validate checks the configuration the way upstream's pydantic validators
// do: identifiers, mutually exclusive options, ranges, references between
// subagents, the custom tool set, policies and hooks. It also applies the
// safety policy guard: tools beyond PolicyFreeTools, or MCP servers, may
// not be enabled while Policies is empty (non-nil) and no PreToolCallHook
// is registered. Endpoint credentials are checked when the session starts.
func (o Options) Validate() error {
	logger := o.logger()
	if id := o.ConversationID; id != "" {
		if len(id) < 32 {
			return validationErrorf("conversation_id must be at least 32 characters long, got %d", len(id))
		}
		if !onlyChars(id, "-") {
			return validationErrorf("conversation_id must match [a-zA-Z0-9-], got %q", id)
		}
	}
	switch o.SessionContinuationMode {
	case "", SessionCreateOrResume, SessionCreateOnly:
	case SessionResume:
		if o.ConversationID == "" {
			return validationErrorf("conversation_id must be specified when session_continuation_mode is RESUME")
		}
	default:
		return validationErrorf("unknown session continuation mode %q", o.SessionContinuationMode)
	}
	if o.AppDataDir != "" && !filepath.IsAbs(o.AppDataDir) {
		return validationErrorf("app_data_dir must be an absolute path, got '%s'", o.AppDataDir)
	}
	if _, err := o.responseSchemaJSON(); err != nil {
		return err
	}
	if err := o.capabilities().validate(logger); err != nil {
		return err
	}
	if o.Compaction != nil && o.Compaction.TokenThreshold < 0 {
		return validationErrorf("compaction token_threshold must be positive, got %d", o.Compaction.TokenThreshold)
	}
	if err := o.Retry.validate(); err != nil {
		return err
	}
	if err := o.Budget.validate(); err != nil {
		return err
	}
	for _, s := range o.MCPServers {
		if err := validateMCPServer(s); err != nil {
			return err
		}
	}
	names := map[string]bool{}
	for _, s := range o.Subagents {
		if s.Name != "" {
			names[s.Name] = true
		}
		if s.Capabilities != nil {
			if err := s.Capabilities.validate(logger); err != nil {
				return err
			}
		}
	}
	valid := slices.Sorted(maps.Keys(names))
	checkAllowed := func(where string, allowed []string) error {
		var unknown []string
		for _, n := range allowed {
			if !names[n] {
				unknown = append(unknown, n)
			}
		}
		if len(unknown) > 0 {
			slices.Sort(unknown)
			return validationErrorf("Unknown subagent name(s) in %s: %q. Valid subagents are: %q", where, unknown, valid)
		}
		return nil
	}
	if err := checkAllowed("CapabilitiesConfig.AllowedSubagents", o.capabilities().AllowedSubagents); err != nil {
		return err
	}
	for _, s := range o.Subagents {
		if s.Capabilities != nil {
			if err := checkAllowed(fmt.Sprintf("SubagentConfig(%q).Capabilities.AllowedSubagents", s.Name), s.Capabilities.AllowedSubagents); err != nil {
				return err
			}
		}
	}
	if _, err := o.compile(); err != nil {
		return err
	}
	for i, t := range o.Triggers {
		if t == nil {
			return validationErrorf("trigger %d is nil", i)
		}
	}
	for i, m := range o.Models {
		switch m.Endpoint.(type) {
		case nil, *GeminiAPIEndpoint, *VertexEndpoint, *OpenAIEndpoint:
		default:
			return validationErrorf("model %d: unrecognized endpoint type %T", i, m.Endpoint)
		}
	}
	return nil
}

// compiledOptions is Options compiled for one session: the hook and tool
// runners and the harness policy config. New and connectLocal consume it.
type compiledOptions struct {
	opts   *Options
	logger *slog.Logger
	hooks  *hookRunner
	tools  *toolRunner
	// policy is nil when Policies is empty; dynamic holds its rules
	// evaluated in process, by rule ID.
	policy  *wire.PolicyConfig
	dynamic map[string]*Policy
}

// compile builds the session state of o, checking the custom tool set, the
// policies, the hooks and the safety policy guard (see Validate). The
// runners hold session state, so each session compiles afresh.
func (o Options) compile() (*compiledOptions, error) {
	custom, err := o.allCustomTools()
	if err != nil {
		return nil, err
	}
	tools, err := newToolRunner(custom)
	if err != nil {
		return nil, err
	}
	cc := &compiledOptions{opts: &o, logger: o.logger(), tools: tools}
	policies := o.policies()
	if len(policies) > 0 {
		if cc.policy, cc.dynamic, err = policyConfig(policies); err != nil {
			return nil, err
		}
	}
	if cc.hooks, err = newHookRunner(o.Hooks); err != nil {
		return nil, err
	}
	if len(policies) == 0 && !cc.hooks.has(hookPreToolCall) && (len(o.MCPServers) > 0 || o.hasPolicyBoundTools()) {
		return nil, validationErrorf("Write tools or MCP servers are enabled without a safety policy. " +
			"Set Policies to []agy.Policy{policy.AllowAll()} to approve all tool calls, " +
			"or to policy.DenyAll() plus policy.Allow(\"tool_name\") rules to selectively allow specific tools.")
	}
	return cc, nil
}

// hasPolicyBoundTools reports whether an active builtin tool is outside
// PolicyFreeTools.
func (o Options) hasPolicyBoundTools() bool {
	caps := o.capabilities()
	free := PolicyFreeTools()
	for t := range resolveActiveTools(caps.EnabledTools, caps.DisabledTools) {
		if !slices.Contains(free, t) {
			return true
		}
	}
	return false
}

// allCustomTools returns the custom tools of the agent and its subagents,
// once each. Two different tools with the same name are an error.
func (o Options) allCustomTools() ([]*Tool, error) {
	var out []*Tool
	seen := map[string]*Tool{}
	add := func(t *Tool, where string) error {
		if t == nil {
			return validationErrorf("nil tool in %s", where)
		}
		if t.name == "" {
			return validationErrorf("tool with empty name in %s", where)
		}
		if prev, ok := seen[t.name]; ok {
			if prev != t {
				return validationErrorf("Duplicate custom tool name '%s' detected across %s configurations.", t.name, where)
			}
			return nil
		}
		seen[t.name] = t
		out = append(out, t)
		return nil
	}
	for _, t := range o.Tools {
		if err := add(t, "agent and subagent"); err != nil {
			return nil, err
		}
	}
	for _, s := range o.Subagents {
		for _, t := range s.Tools {
			if err := add(t, fmt.Sprintf("agent and subagent '%s'", s.Name)); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// Lightweight returns a copy of o with the preset for small-context and
// on-device models: the MinimalTools set, AgentBehaviorMinimal, subagents
// disabled, and compaction at 65536 tokens. Capabilities fields already set
// on o are kept (a DisabledTools list is subtracted from MinimalTools), as
// is an explicit Compaction or CapabilitiesConfig.CompactionThreshold.
func (o Options) Lightweight() Options {
	out := o.clone()
	caps := out.capabilities().clone()
	if caps.EnabledTools == nil {
		if caps.DisabledTools != nil {
			caps.EnabledTools = slices.DeleteFunc(MinimalTools(), func(t BuiltinTool) bool {
				return slices.Contains(caps.DisabledTools, t)
			})
			caps.DisabledTools = nil
		} else {
			caps.EnabledTools = MinimalTools()
		}
	}
	if caps.AgentBehavior == "" {
		caps.AgentBehavior = AgentBehaviorMinimal
	}
	caps.DisableSubagents = true
	out.Capabilities = caps
	if out.Compaction == nil && caps.CompactionThreshold == 0 {
		out.Compaction = &CompactionConfig{TokenThreshold: 65536}
	}
	return out
}

// Eval returns a copy of o with the product-agnostic evaluation preset:
// generate_image disabled (unless EnabledTools is set), subagents disabled,
// daemon commands enabled, AllowAll policies and BenchmarkRetryConfig unless
// o sets Policies or Retry, and thinkingLevel on every text model target.
// Pass ThinkingHigh for upstream's default, or "" to leave the targets'
// thinking levels alone.
//
// It fails when thinkingLevel is set and a text target already sets one, or
// uses an endpoint other than Gemini or Vertex.
func (o Options) Eval(thinkingLevel ThinkingLevel) (Options, error) {
	out := o.clone()
	caps := out.capabilities().clone()
	if caps.EnabledTools == nil && caps.DisabledTools == nil {
		caps.DisabledTools = []BuiltinTool{BuiltinGenerateImage}
	}
	caps.DisableSubagents = true
	if caps.RunCommand == nil {
		caps.RunCommand = &RunCommandConfig{}
	}
	caps.RunCommand.EnableDaemons = true
	out.Capabilities = caps
	if out.Policies == nil {
		out.Policies = []Policy{AllowAllPolicy()}
	}
	if out.Retry == nil {
		out.Retry = BenchmarkRetryConfig()
	}
	if thinkingLevel == "" {
		return out, nil
	}
	if out.OpenAI != nil {
		return Options{}, validationErrorf("Cannot apply thinking_level in Eval() with an OpenAI-compatible endpoint: thinking_level is only supported on Gemini or Vertex model targets (pass \"\" to disable).")
	}
	models := out.ResolvedModels()
	if !slices.ContainsFunc(models, func(m ModelTarget) bool { return m.hasType(ModelTypeText) }) {
		return Options{}, validationErrorf("Cannot apply thinking_level in Eval(): no text model target found in models.")
	}
	for i := range models {
		m := &models[i]
		if !m.hasType(ModelTypeText) {
			continue
		}
		opts := endpointOptions(m.Endpoint)
		if opts == nil {
			return Options{}, validationErrorf("Cannot apply thinking_level to model target '%s': endpoint must be a GeminiAPIEndpoint or VertexEndpoint, got %T.", m.Name, m.Endpoint)
		}
		if *opts != nil && (*opts).ThinkingLevel != "" {
			return Options{}, validationErrorf("Model target '%s' already sets thinking_level=%q; remove it from the target and pass it to Eval, or pass \"\" to keep the target's setting.", m.Name, (*opts).ThinkingLevel)
		}
		if *opts == nil {
			*opts = &GeminiModelOptions{}
		}
		(*opts).ThinkingLevel = thinkingLevel
	}
	out.Models = models
	out.modelsResolved = true
	return out, nil
}
