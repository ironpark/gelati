package agy

import (
	"context"
	"encoding/json/v2"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ironpark/gelati/agy/internal/wire"
)

// clearModelEnv unsets the environment variables that affect model
// resolution for the duration of the test.
func clearModelEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"GEMINI_API_KEY", "GOOGLE_GENAI_USE_VERTEXAI", "GOOGLE_GENAI_USE_ENTERPRISE", "GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_LOCATION"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}

// harnessConfig builds the harness config of cfg.
func harnessConfig(t *testing.T, cfg Options, hooks ...Hook) *wire.HarnessConfig {
	t.Helper()
	cfg.Hooks = append(slices.Clone(cfg.Hooks), hooks...)
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	cc, err := cfg.compile()
	if err != nil {
		t.Fatal(err)
	}
	hc, err := buildHarnessConfig(cc, cfg.ResolvedModels())
	if err != nil {
		t.Fatal(err)
	}
	return hc
}

func TestConfigValidation(t *testing.T) {
	sub := SubagentConfig{Name: "researcher", Description: "researcher"}
	for name, tc := range map[string]struct {
		cfg     Options
		wantErr string
	}{
		"zero":                {Options{}, ""},
		"valid id":            {Options{ConversationID: "12345678901234567890123456789012"}, ""},
		"uuid id":             {Options{ConversationID: "12345678-1234-1234-1234-123456789012"}, ""},
		"short id":            {Options{ConversationID: "too-short"}, "must be at least 32 characters long"},
		"bad id chars":        {Options{ConversationID: "invalid_char_because_of_underscores_123"}, "must match [a-zA-Z0-9-]"},
		"resume without id":   {Options{SessionContinuationMode: SessionResume}, "must be specified when session_continuation_mode is RESUME"},
		"resume with id":      {Options{SessionContinuationMode: SessionResume, ConversationID: strings.Repeat("a", 32)}, ""},
		"bad mode":            {Options{SessionContinuationMode: "sometimes"}, "unknown session continuation mode"},
		"relative app dir":    {Options{AppDataDir: "relative/path"}, "app_data_dir must be an absolute path"},
		"schema string":       {Options{ResponseSchema: `{"type": "object"}`}, ""},
		"schema invalid":      {Options{ResponseSchema: "{not json"}, "response_schema string is not valid JSON."},
		"schema map":          {Options{ResponseSchema: map[string]any{"type": "object"}}, ""},
		"schema type":         {Options{ResponseSchema: struct{ A int }{}}, ""},
		"allowed known":       {Options{Subagents: []SubagentConfig{sub}, Capabilities: &CapabilitiesConfig{AllowedSubagents: []string{"researcher"}}}, ""},
		"allowed unknown":     {Options{Subagents: []SubagentConfig{sub}, Capabilities: &CapabilitiesConfig{AllowedSubagents: []string{"non_existent"}}}, "Unknown subagent name(s)"},
		"sub allowed unknown": {Options{Subagents: []SubagentConfig{{Name: "researcher", Capabilities: &SubagentCapabilities{EnabledTools: []BuiltinTool{BuiltinStartSubagent}, AllowedSubagents: []string{"ghost_agent"}}}}}, "ghost_agent"},
		"bad mcp":             {Options{MCPServers: []MCPServer{&MCPStdioServer{Name: "bad name"}}}, "must match"},
		"multiple auto":       {Options{Policies: []Policy{{Auto: true}, {Auto: true}}}, "Multiple AutoPolicy"},
		"nil hook":            {Options{Hooks: []Hook{nil}}, "nil hook"},
		"nil trigger":         {Options{Triggers: []Trigger{nil}}, "trigger 0 is nil"},
		"negative compaction": {Options{Compaction: &CompactionConfig{TokenThreshold: -1}}, "must be positive"},
		"bad budget":          {Options{Budget: &BudgetConfig{MaxModelCalls: -1}}, "max_model_calls"},
	} {
		err := tc.cfg.Validate()
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("%s: unexpected error %v", name, err)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("%s: error %v, want %q", name, err, tc.wantErr)
		}
	}
}

func TestCustomToolCollection(t *testing.T) {
	mk := func(name string) *Tool {
		return NewToolWithSchema(name, "", nil, func(context.Context, *ToolContext, map[string]any) (any, error) { return nil, nil })
	}
	shared, subOnly := mk("shared_tool"), mk("sub_only_tool")
	cfg := Options{
		Tools:     []*Tool{shared},
		ToolNames: []string{"view_file"},
		Subagents: []SubagentConfig{
			{Name: "sub1", Tools: []*Tool{shared, subOnly}, ToolNames: []string{"grep_search"}},
			{Name: "sub2", Tools: []*Tool{subOnly}},
		},
	}
	tools, err := cfg.allCustomTools()
	if err != nil || len(tools) != 2 || tools[0] != shared || tools[1] != subOnly {
		t.Fatalf("tools %v, %v", tools, err)
	}
	cfg = Options{Tools: []*Tool{mk("tool_fn")}, Subagents: []SubagentConfig{{Name: "sub1", Tools: []*Tool{mk("tool_fn")}}}}
	if _, err := cfg.allCustomTools(); err == nil || !strings.Contains(err.Error(), "Duplicate custom tool name 'tool_fn' detected across agent and subagent 'sub1' configurations.") {
		t.Fatalf("collision: %v", err)
	}
	if err := (&Options{Tools: []*Tool{nil}}).Validate(); err == nil {
		t.Fatal("nil tool accepted")
	}
}

func TestResolvedModels(t *testing.T) {
	clearModelEnv(t)
	models := (&Options{}).ResolvedModels()
	if len(models) != 2 || models[0].Name != DefaultModel || !reflect.DeepEqual(models[0].Types, []ModelType{ModelTypeText}) ||
		models[1].Name != DefaultImageGenerationModel || !reflect.DeepEqual(models[1].Types, []ModelType{ModelTypeImage}) {
		t.Fatalf("defaults %+v", models)
	}
	if _, ok := models[0].Endpoint.(*GeminiAPIEndpoint); !ok {
		t.Fatalf("default endpoint %T", models[0].Endpoint)
	}

	models = (&Options{Model: "custom-text-model", APIKey: "my-key"}).ResolvedModels()
	if len(models) != 2 || models[0].Name != "custom-text-model" || models[1].Name != DefaultImageGenerationModel {
		t.Fatalf("shorthand %+v", models)
	}
	if e := models[0].Endpoint.(*GeminiAPIEndpoint); e.APIKey != "my-key" {
		t.Fatalf("shorthand endpoint %+v", e)
	}

	img := ModelTarget{Name: "custom-image-model", Types: []ModelType{ModelTypeImage}}
	models = (&Options{Models: []ModelTarget{img}}).ResolvedModels()
	if len(models) != 2 || models[0].Name != "custom-image-model" || models[1].Name != DefaultModel {
		t.Fatalf("explicit %+v", models)
	}
	models = (&Options{Model: "custom-text-model", Models: []ModelTarget{img}}).ResolvedModels()
	if len(models) != 2 || models[0].Name != "custom-image-model" || models[1].Name != "custom-text-model" {
		t.Fatalf("explicit + shorthand %+v", models)
	}
}

func TestVertexEnvironment(t *testing.T) {
	clearModelEnv(t)
	t.Setenv("GOOGLE_GENAI_USE_VERTEXAI", "True")
	t.Setenv("GOOGLE_CLOUD_PROJECT", "env-project")
	t.Setenv("GOOGLE_CLOUD_LOCATION", "env-location")
	models := (&Options{Model: "gemini-3.8-flash"}).ResolvedModels()
	v, ok := models[0].Endpoint.(*VertexEndpoint)
	if !ok || v.Project != "env-project" || v.Location != "env-location" {
		t.Fatalf("vertex endpoint %#v", models[0].Endpoint)
	}
	if err := v.Validate(); err != nil {
		t.Fatal(err)
	}
	// Explicit endpoints get the environment defaults too, except in
	// express mode or behind a custom base URL.
	if v := (&VertexEndpoint{}).withEnvDefaults(); v.Project != "env-project" {
		t.Fatalf("direct endpoint %+v", v)
	}
	if v := (&VertexEndpoint{APIKey: "express-key"}).withEnvDefaults(); v.Project != "" || v.Location != "" {
		t.Fatalf("express endpoint %+v", v)
	}
	if v := (&VertexEndpoint{BaseURL: "http://localhost:8080"}).withEnvDefaults(); v.Project != "" {
		t.Fatalf("base URL endpoint %+v", v)
	}

	clearModelEnv(t)
	t.Setenv("GOOGLE_GENAI_USE_ENTERPRISE", "1")
	if _, ok := (&Options{}).ResolvedModels()[0].Endpoint.(*VertexEndpoint); !ok {
		t.Fatal("GOOGLE_GENAI_USE_ENTERPRISE did not route to Vertex")
	}
	clearModelEnv(t)
	models = (&Options{Vertex: true, APIKey: "express-key"}).ResolvedModels()
	if v := models[0].Endpoint.(*VertexEndpoint); v.APIKey != "express-key" || v.Project != "" || v.Validate() != nil {
		t.Fatalf("express shorthand %+v", v)
	}
}

func TestEndpointValidation(t *testing.T) {
	clearModelEnv(t)
	if err := (&GeminiAPIEndpoint{}).Validate(); err == nil || !strings.Contains(err.Error(), "A Gemini API key is required") {
		t.Fatalf("gemini without key: %v", err)
	}
	if err := (&GeminiAPIEndpoint{APIKey: "k"}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (&GeminiAPIEndpoint{BaseURL: "http://proxy"}).Validate(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GEMINI_API_KEY", "env-key")
	if err := (&GeminiAPIEndpoint{}).Validate(); err != nil {
		t.Fatal(err)
	}
	clearModelEnv(t)
	if err := (&VertexEndpoint{}).Validate(); err == nil || !strings.Contains(err.Error(), "either (project and location) or api_key must be set") {
		t.Fatalf("vertex without auth: %v", err)
	}
	for _, e := range []*VertexEndpoint{
		{Project: "p", Location: "l", APIKey: "k"},
		{Project: "p", APIKey: "k"},
		{Location: "l", APIKey: "k"},
	} {
		if err := e.Validate(); err == nil || !strings.Contains(err.Error(), "Cannot specify both api_key") {
			t.Errorf("%+v: %v", e, err)
		}
	}
	if err := (&VertexEndpoint{BaseURL: "https://gw", Project: "p", Location: "l", APIKey: "k"}).Validate(); err != nil {
		t.Fatal(err)
	}

	if err := validateModels((&Options{Models: []ModelTarget{{Name: "m"}}}).ResolvedModels()); err == nil || !strings.Contains(err.Error(), "must have an endpoint configured") {
		t.Fatalf("missing endpoint: %v", err)
	}
	cc, err := (&Options{}).compile()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connectLocal(t.Context(), cc); err == nil || !strings.Contains(err.Error(), "A Gemini API key is required") {
		t.Fatalf("connect without key: %v", err)
	}
}

func TestHarnessConfigDefaults(t *testing.T) {
	clearModelEnv(t)
	hc := harnessConfig(t, Options{Workspaces: []string{}})
	tools := hc.GetHarnessSideTools()
	if !tools.GetSubagents().GetEnabled() || tools.GetUserQuestions().GetEnabled() || !tools.GetRunCommand().GetEnabled() ||
		!tools.GetManageTask().GetEnabled() || !tools.GetSchedule().GetEnabled() || tools.GetFind().GetEnabled() ||
		tools.GetGrepSearch().GetEnabled() || !tools.GetGenerateImage().GetEnabled() {
		t.Fatalf("default tools %+v", tools)
	}
	if hc.GetSystemInstructions() != nil || len(hc.GetWorkspaces()) != 0 || hc.GetAgentBehavior() != wire.AgentBehavior_AGENT_BEHAVIOR_AUTONOMOUS ||
		hc.GetCascadeId() != "" || hc.GetSessionContinuationMode() != wire.HarnessConfig_SESSION_CONTINUATION_MODE_UNSPECIFIED ||
		hc.GetCompactionThreshold() != 0 || hc.GetCompactionConfig() != nil || hc.GetRetryConfig() != nil || hc.GetBudgetConfig() != nil {
		t.Fatalf("defaults %+v", hc)
	}
	if hc.GetAppDataDir() != defaultAppDataDir() || !strings.HasSuffix(hc.GetAppDataDir(), filepath.Join(".gemini", "antigravity")) {
		t.Fatalf("app data dir %q", hc.GetAppDataDir())
	}
	// Default policies: confirm_run_command.
	rules := hc.GetPolicyConfig().GetRules()
	if len(rules) != 2 || rules[0].GetTool() != "run_command" || rules[0].GetDecision() != wire.PolicyDecision_POLICY_DECISION_DENY || rules[1].GetTool() != "*" {
		t.Fatalf("default policies %+v", rules)
	}
	// Models: the default text and image models.
	if len(hc.GetModels()) != 2 || hc.GetModels()[0].GetName() != DefaultModel || hc.GetModels()[0].GetGeminiApiEndpoint() == nil {
		t.Fatalf("models %+v", hc.GetModels())
	}
	// A nil workspace list means the current directory.
	wd, _ := os.Getwd()
	hc = harnessConfig(t, Options{})
	if len(hc.GetWorkspaces()) != 1 || hc.GetWorkspaces()[0].GetFilesystemWorkspace().GetDirectory() != filepath.ToSlash(wd) {
		t.Fatalf("default workspace %+v", hc.GetWorkspaces())
	}
	// Explicit empty policies send no policy config.
	if hc := harnessConfig(t, Options{Policies: []Policy{}, Capabilities: &CapabilitiesConfig{EnabledTools: ReadOnlyTools()}}); hc.GetPolicyConfig() != nil {
		t.Fatalf("empty policies %+v", hc.GetPolicyConfig())
	}
}

func TestHarnessConfigCapabilities(t *testing.T) {
	hc := harnessConfig(t, Options{Capabilities: &CapabilitiesConfig{
		DisabledTools: []BuiltinTool{BuiltinRunCommand, BuiltinSchedule, BuiltinAskQuestion, BuiltinGenerateImage},
	}})
	tools := hc.GetHarnessSideTools()
	if tools.GetRunCommand().GetEnabled() || tools.GetSchedule().GetEnabled() || tools.GetManageTask().GetEnabled() ||
		tools.GetUserQuestions().GetEnabled() || tools.GetGenerateImage().GetEnabled() || !tools.GetSubagents().GetEnabled() ||
		tools.GetFind().GetEnabled() || !tools.GetFileEdit().GetEnabled() || !tools.GetViewFile().GetEnabled() ||
		!tools.GetWriteToFile().GetEnabled() || tools.GetListDir().GetEnabled() || !tools.GetSearchWeb().GetEnabled() {
		t.Fatalf("disabled tools %+v", tools)
	}

	hc = harnessConfig(t, Options{Capabilities: &CapabilitiesConfig{EnabledTools: []BuiltinTool{BuiltinViewFile}}})
	want := wire.HarnessSideTools_builder{
		ViewFile:       wire.ViewFileToolConfig_builder{Enabled: new(true)}.Build(),
		Subagents:      wire.SubagentsConfig_builder{Enabled: new(false)}.Build(),
		UserQuestions:  wire.UserQuestionsConfig_builder{Enabled: new(false)}.Build(),
		RunCommand:     wire.RunCommandToolConfig_builder{Enabled: new(false), EnableDaemonCommands: new(false), MaxTimeoutMs: new(uint32(0)), EnableSandbox: new(false)}.Build(),
		ManageTask:     wire.ManageTaskToolConfig_builder{Enabled: new(false)}.Build(),
		Schedule:       wire.ScheduleToolConfig_builder{Enabled: new(false)}.Build(),
		Find:           wire.FindToolConfig_builder{Enabled: new(false)}.Build(),
		GenerateImage:  wire.GenerateImageToolConfig_builder{Enabled: new(false)}.Build(),
		FileEdit:       wire.FileEditToolConfig_builder{Enabled: new(false)}.Build(),
		WriteToFile:    wire.WriteToFileToolConfig_builder{Enabled: new(false)}.Build(),
		GrepSearch:     wire.GrepSearchToolConfig_builder{Enabled: new(false)}.Build(),
		ListDir:        wire.ListDirToolConfig_builder{Enabled: new(false)}.Build(),
		SearchWeb:      wire.SearchWebToolConfig_builder{Enabled: new(false)}.Build(),
		ReadUrlContent: wire.ReadUrlContentToolConfig_builder{Enabled: new(false)}.Build(),
	}.Build()
	gb, _ := wire.Marshal(hc.GetHarnessSideTools())
	wb, _ := wire.Marshal(want)
	if string(gb) != string(wb) {
		t.Fatalf("enabled tools\n got %s\nwant %s", gb, wb)
	}

	hc = harnessConfig(t, Options{Capabilities: &CapabilitiesConfig{RunCommand: &RunCommandConfig{Timeout: 60 * time.Second, EnableSandbox: true}}})
	rc := hc.GetHarnessSideTools().GetRunCommand()
	if rc.GetMaxTimeoutMs() != 60000 || rc.GetEnableDaemonCommands() || !rc.GetEnableSandbox() || !rc.GetEnabled() {
		t.Fatalf("run command %+v", rc)
	}
	hc = harnessConfig(t, Options{Capabilities: &CapabilitiesConfig{RunCommand: &RunCommandConfig{EnableDaemons: true}}})
	if rc := hc.GetHarnessSideTools().GetRunCommand(); !rc.GetEnableDaemonCommands() || rc.GetMaxTimeoutMs() != 0 {
		t.Fatalf("daemons %+v", rc)
	}

	sub := SubagentConfig{Name: "researcher"}
	hc = harnessConfig(t, Options{
		Subagents:    []SubagentConfig{sub, {Name: "reviewer"}},
		Capabilities: &CapabilitiesConfig{MaxSubagentDepth: 3, AllowedSubagents: []string{"researcher", "reviewer"}},
	})
	sc := hc.GetHarnessSideTools().GetSubagents()
	if !sc.GetEnabled() || sc.GetMaxNestingDepth() != 3 || !reflect.DeepEqual(sc.GetAllowedSubagents(), []string{"researcher", "reviewer"}) {
		t.Fatalf("subagents %+v", sc)
	}
	for _, caps := range []*CapabilitiesConfig{{DisabledTools: []BuiltinTool{BuiltinStartSubagent}}, {DisableSubagents: true}} {
		if harnessConfig(t, Options{Capabilities: caps}).GetHarnessSideTools().GetSubagents().GetEnabled() {
			t.Errorf("subagents enabled with %+v", caps)
		}
	}
	hc = harnessConfig(t, Options{Capabilities: &CapabilitiesConfig{AgentBehavior: AgentBehaviorInteractive, EnabledTools: []BuiltinTool{BuiltinViewFile, BuiltinAskQuestion}}})
	if hc.GetAgentBehavior() != wire.AgentBehavior_AGENT_BEHAVIOR_INTERACTIVE || !hc.GetHarnessSideTools().GetUserQuestions().GetEnabled() || hc.GetHarnessSideTools().GetRunCommand().GetEnabled() {
		t.Fatalf("interactive %+v", hc.GetHarnessSideTools())
	}
	hc = harnessConfig(t, Options{Capabilities: &CapabilitiesConfig{EnabledTools: []BuiltinTool{BuiltinFindFile, BuiltinSearchDir}}})
	if !hc.GetHarnessSideTools().GetFind().GetEnabled() || !hc.GetHarnessSideTools().GetGrepSearch().GetEnabled() {
		t.Fatal("find/grep not enabled")
	}
}

func TestHarnessConfigCompactionRetryBudgetTruncation(t *testing.T) {
	hc := harnessConfig(t, Options{Capabilities: &CapabilitiesConfig{CompactionThreshold: 50000}})
	if hc.GetCompactionThreshold() != 50000 || hc.GetCompactionConfig().GetTokenThreshold() != 50000 {
		t.Fatalf("legacy compaction %+v", hc.GetCompactionConfig())
	}
	hc = harnessConfig(t, Options{Capabilities: &CapabilitiesConfig{CompactionThreshold: 50000}, Compaction: &CompactionConfig{TokenThreshold: 30000}})
	if hc.GetCompactionThreshold() != 30000 || hc.GetCompactionConfig().GetTokenThreshold() != 30000 {
		t.Fatalf("compaction precedence %+v", hc.GetCompactionConfig())
	}

	if harnessConfig(t, Options{Retry: &RetryConfig{}}).GetRetryConfig() != nil {
		t.Fatal("empty retry config sent")
	}
	hc = harnessConfig(t, Options{Retry: &RetryConfig{APIRetry: &ModelAPIRetryConfig{
		MaxRetries: new(uint32(5)), InitialSleepDurationMs: new(uint32(500)), ExponentialMultiplier: new(1.5), JitterRange: new(0.1),
	}}})
	if a := hc.GetRetryConfig().GetApiRetry(); a.GetMaxRetries() != 5 || a.GetInitialSleepDurationMs() != 500 || a.GetExponentialMultiplier() != 1.5 || a.GetJitterRange() != 0.1 || hc.GetRetryConfig().GetModelOutputRetry() != nil {
		t.Fatalf("api retry %+v", hc.GetRetryConfig())
	}
	hc = harnessConfig(t, Options{Retry: &RetryConfig{ModelOutputRetry: &ModelOutputRetryConfig{MaxRetries: new(uint32(math.MaxUint32))}, APIRetry: &ModelAPIRetryConfig{MaxRetries: new(uint32(0))}}})
	if hc.GetRetryConfig().GetModelOutputRetry().GetMaxRetries() != math.MaxUint32 || !hc.GetRetryConfig().GetApiRetry().HasMaxRetries() {
		t.Fatalf("retry edges %+v", hc.GetRetryConfig())
	}

	if harnessConfig(t, Options{Budget: &BudgetConfig{}}).GetBudgetConfig() != nil {
		t.Fatal("empty budget sent")
	}
	hc = harnessConfig(t, Options{Budget: &BudgetConfig{MaxModelCalls: 5, MaxToolCalls: 10, MaxInputTokens: 500, MaxOutputTokens: 200, MaxTotalTokens: 1000}})
	b := hc.GetBudgetConfig()
	if b.GetMaxModelCalls() != 5 || b.GetMaxToolCalls() != 10 || b.GetMaxInputTokens() != 500 || b.GetMaxOutputTokens() != 200 || b.GetMaxTotalTokens() != 1000 || b.GetScope() != wire.BudgetConfig_BUDGET_SCOPE_LIFETIME {
		t.Fatalf("budget %+v", b)
	}
	if hc := harnessConfig(t, Options{Budget: &BudgetConfig{MaxTotalTokens: 2000, Scope: BudgetScopeForwardLooking}}); hc.GetBudgetConfig().GetScope() != wire.BudgetConfig_BUDGET_SCOPE_FORWARD_LOOKING {
		t.Fatal("forward-looking scope")
	}

	if harnessConfig(t, Options{}).GetToolOutputTruncation() != nil {
		t.Fatal("truncation sent by default")
	}
	for _, n := range []int{2048, 0} {
		hc := harnessConfig(t, Options{Capabilities: &CapabilitiesConfig{ToolOutputTruncation: &ToolOutputTruncationConfig{MaxTokens: n}}})
		if tr := hc.GetToolOutputTruncation().GetTruncate(); tr == nil || tr.GetMaxTokens() != int32(n) {
			t.Fatalf("truncation %d: %+v", n, hc.GetToolOutputTruncation())
		}
	}
}

func TestHarnessConfigSessionModelsAndInstructions(t *testing.T) {
	clearModelEnv(t)
	id := "12345678901234567890123456789012"
	for mode, want := range map[SessionContinuationMode]wire.HarnessConfig_SessionContinuationMode{
		SessionResume:         wire.HarnessConfig_RESUME,
		SessionCreateOrResume: wire.HarnessConfig_CREATE_OR_RESUME,
		SessionCreateOnly:     wire.HarnessConfig_CREATE_ONLY,
	} {
		hc := harnessConfig(t, Options{ConversationID: id, SessionContinuationMode: mode})
		if hc.GetSessionContinuationMode() != want || hc.GetCascadeId() != id {
			t.Errorf("mode %s: %s %s", mode, hc.GetSessionContinuationMode(), hc.GetCascadeId())
		}
	}

	hc := harnessConfig(t, Options{Models: []ModelTarget{
		{Name: "gemini-2.5-pro", Endpoint: &GeminiAPIEndpoint{APIKey: "test-key", Options: &GeminiModelOptions{ThinkingLevel: ThinkingHigh}}},
		{Name: "imagen-3-custom", Types: []ModelType{ModelTypeImage}, Endpoint: &GeminiAPIEndpoint{Options: &GeminiModelOptions{}}},
	}})
	if len(hc.GetModels()) != 2 || hc.GetModels()[0].GetGeminiApiEndpoint().GetApiKey() != "test-key" ||
		hc.GetModels()[0].GetGeminiApiEndpoint().GetOptions().GetThinkingLevel() != "high" ||
		!reflect.DeepEqual(hc.GetModels()[1].GetTypes(), []wire.ModelType{wire.ModelType_MODEL_TYPE_IMAGE}) || hc.GetModels()[1].GetGeminiApiEndpoint().GetOptions() != nil {
		t.Fatalf("models %+v", hc.GetModels())
	}
	for _, tier := range []ServiceTier{ServiceTierStandard, ServiceTierPriority, ServiceTierFlex} {
		hc := harnessConfig(t, Options{Models: []ModelTarget{{Endpoint: &VertexEndpoint{Project: "p", Location: "l", Options: &GeminiModelOptions{ServiceTier: tier}}}}})
		if got := hc.GetModels()[0].GetVertexEndpoint().GetOptions().GetServiceTier(); got != string(tier) {
			t.Errorf("tier %s: %q", tier, got)
		}
		if hc.GetModels()[0].GetName() != "" || hc.GetModels()[0].GetVertexEndpoint().GetProject() != "p" {
			t.Errorf("vertex model %+v", hc.GetModels()[0])
		}
	}

	if si := harnessConfig(t, Options{SystemInstructions: TextSystemInstructions("Be concise.")}).GetSystemInstructions(); si.GetAppended().GetAppendedSections()[0].GetContent() != "Be concise." ||
		si.GetAppended().GetAppendedSections()[0].GetTitle() != "user_system_instructions" {
		t.Fatalf("text instructions %+v", si)
	}
	if si := harnessConfig(t, Options{SystemInstructions: CustomSystemInstructions{Text: "Override everything."}}).GetSystemInstructions(); si.GetCustom().GetPart()[0].GetText() != "Override everything." {
		t.Fatalf("custom instructions %+v", si)
	}
	si := harnessConfig(t, Options{SystemInstructions: TemplatedSystemInstructions{Identity: "New Identity", Sections: []SystemInstructionSection{{Title: "extra", Content: "More instructions"}}}}).GetSystemInstructions()
	if si.GetAppended().GetCustomIdentity() != "New Identity" || si.GetAppended().GetAppendedSections()[0].GetTitle() != "extra" {
		t.Fatalf("templated instructions %+v", si)
	}
	if si := harnessConfig(t, Options{SystemInstructions: &TemplatedSystemInstructions{Identity: "Only Identity"}}).GetSystemInstructions(); si.GetAppended().GetCustomIdentity() != "Only Identity" || len(si.GetAppended().GetAppendedSections()) != 0 {
		t.Fatalf("identity only %+v", si)
	}
	if si := harnessConfig(t, Options{SystemInstructions: TextSystemInstructions("")}).GetSystemInstructions(); si != nil {
		t.Fatalf("empty instructions %+v", si)
	}

	hc = harnessConfig(t, Options{SkillsPaths: []string{"/skills/a", "/skills/b"}, AppDataDir: "/custom/app/data", ResponseSchema: map[string]any{"properties": map[string]any{"field": map[string]any{"type": "string"}}}})
	if !reflect.DeepEqual(hc.GetSkillsPaths(), []string{"/skills/a", "/skills/b"}) || hc.GetAppDataDir() != "/custom/app/data" ||
		hc.GetFinishToolSchemaJson() != `{"properties":{"field":{"type":"string"}}}` {
		t.Fatalf("misc %+v", hc)
	}
	hc = harnessConfig(t, Options{Capabilities: &CapabilitiesConfig{FinishToolSchemaJSON: `{"type": "object"}`}})
	if hc.GetFinishToolSchemaJson() != `{"type": "object"}` {
		t.Fatalf("finish schema %q", hc.GetFinishToolSchemaJson())
	}
}

func TestHarnessConfigWorkspaces(t *testing.T) {
	tmp := resolvePath(os.TempDir())
	home, _ := os.UserHomeDir()
	wd, _ := os.Getwd()
	hc := harnessConfig(t, Options{Workspaces: []string{
		"file:///dev/shm/workspace", filepath.Join(tmp, "clean-path"), "ws", "~/my_project", "cns://el-d/home/user/project", "/cns/el-d/home/user/data",
	}})
	var got []string
	for _, w := range hc.GetWorkspaces() {
		got = append(got, w.GetFilesystemWorkspace().GetDirectory())
	}
	want := []string{
		resolvePath("/dev/shm/workspace"), filepath.Join(tmp, "clean-path"), filepath.Join(resolvePath(wd), "ws"),
		resolvePath(filepath.Join(home, "my_project")), "/cns/el-d/home/user/project", "/cns/el-d/home/user/data",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("workspaces\n got %q\nwant %q", got, want)
	}
}

func TestHarnessConfigMCPServers(t *testing.T) {
	hc := harnessConfig(t, Options{MCPServers: []MCPServer{
		&MCPStreamableHTTPServer{Name: "my_http_server", URL: "http://localhost:8080/mcp", Headers: map[string]string{"Authorization": "Bearer token123"}, TimeoutSeconds: 30},
		&MCPStdioServer{Name: "my_stdio_server", Command: "node", Args: []string{"server.js"}, Env: map[string]string{"NODE_ENV": "production"}, TimeoutSeconds: 10, EnabledTools: []string{"add", "sub"}},
	}})
	h, s := hc.GetMcpServers()[0], hc.GetMcpServers()[1]
	if h.GetName() != "my_http_server" || h.GetHttp().GetUrl() != "http://localhost:8080/mcp" || h.GetHttp().GetHeaders()["Authorization"] != "Bearer token123" || h.GetTimeoutSeconds() != 30 || h.GetStdio() != nil {
		t.Fatalf("http server %+v", h)
	}
	if s.GetStdio().GetCommand() != "node" || !reflect.DeepEqual(s.GetStdio().GetArgs(), []string{"server.js"}) ||
		s.GetStdio().GetEnv()["NODE_ENV"] != "production" || s.GetTimeoutSeconds() != 10 || !reflect.DeepEqual(s.GetEnabledTools(), []string{"add", "sub"}) {
		t.Fatalf("stdio server %+v", s)
	}
}

func TestHarnessConfigToolsAndSubagents(t *testing.T) {
	type q struct {
		Query string `json:"query"`
	}
	mk := func(name string) *Tool {
		return NewTool(name, name+" docs", func(context.Context, *ToolContext, q) (string, error) { return "", nil })
	}
	root, shared, subOnly := mk("root_tool"), mk("shared_tool"), mk("sub_tool")
	hc := harnessConfig(t, Options{
		Tools:     []*Tool{root, shared},
		ToolNames: []string{"sub_tool", "builtin_thing"},
		Subagents: []SubagentConfig{
			{
				Name: "helper", Description: "Helps.", Model: "gemini-2.5-flash",
				SystemInstructions: TextSystemInstructions("Help."),
				Tools:              []*Tool{shared, subOnly},
				ToolNames:          []string{"grep_search"},
				Capabilities: &SubagentCapabilities{
					AgentBehavior: AgentBehaviorMinimal,
					EnabledTools:  []BuiltinTool{BuiltinStartSubagent},
				},
			},
			{Name: "plain", Description: "Plain."},
		},
	})
	var rootNames []string
	for _, tl := range hc.GetTools() {
		rootNames = append(rootNames, tl.GetName())
	}
	if !reflect.DeepEqual(rootNames, []string{"root_tool", "shared_tool", "sub_tool", "builtin_thing"}) {
		t.Fatalf("root tools %v", rootNames)
	}
	if hc.GetTools()[2].GetParametersJsonSchema() == "" || hc.GetTools()[3].GetParametersJsonSchema() != "" {
		t.Fatalf("tool declarations %+v", hc.GetTools())
	}
	var schema map[string]any
	if err := json.Unmarshal([]byte(hc.GetTools()[0].GetParametersJsonSchema()), &schema); err != nil || schema["type"] != "object" || hc.GetTools()[0].GetDescription() != "root_tool docs" {
		t.Fatalf("schema %v %v", schema, err)
	}
	helper, plain := hc.GetCustomSubagents()[0], hc.GetCustomSubagents()[1]
	var subNames []string
	for _, tl := range helper.GetTools() {
		subNames = append(subNames, tl.GetName())
	}
	if !reflect.DeepEqual(subNames, []string{"shared_tool", "sub_tool", "grep_search"}) || helper.GetModel().GetName() != "gemini-2.5-flash" ||
		helper.GetAgentBehavior() != wire.AgentBehavior_AGENT_BEHAVIOR_MINIMAL || !helper.GetHarnessSideTools().GetSubagents().GetEnabled() ||
		helper.GetSystemInstructions().GetAppended() == nil || helper.GetDescription() != "Helps." {
		t.Fatalf("helper %+v", helper)
	}
	// Subagents default to read-only tools, without subagents.
	pt := plain.GetHarnessSideTools()
	if !pt.GetViewFile().GetEnabled() || pt.GetRunCommand().GetEnabled() || pt.GetSubagents().GetEnabled() || plain.GetModel() != nil ||
		plain.GetAgentBehavior() != wire.AgentBehavior_AGENT_BEHAVIOR_AUTONOMOUS {
		t.Fatalf("plain subagent %+v", plain)
	}
}

func TestLightweight(t *testing.T) {
	clearModelEnv(t)
	orig := Options{Model: "gemini-3.8-flash"}
	cfg := orig.Lightweight()
	caps := cfg.Capabilities
	if cfg.Model != "gemini-3.8-flash" || caps.AgentBehavior != AgentBehaviorMinimal || !reflect.DeepEqual(caps.EnabledTools, MinimalTools()) ||
		cfg.Compaction.TokenThreshold != 65536 || caps.CompactionThreshold != 0 || !caps.DisableSubagents {
		t.Fatalf("lightweight %+v %+v", cfg, caps)
	}
	if orig.Capabilities != nil || orig.Compaction != nil {
		t.Fatal("Lightweight modified the original")
	}
	hc := harnessConfig(t, cfg)
	tools := hc.GetHarnessSideTools()
	if hc.GetAgentBehavior() != wire.AgentBehavior_AGENT_BEHAVIOR_MINIMAL || hc.GetCompactionThreshold() != 65536 || tools.GetSubagents().GetEnabled() ||
		!tools.GetRunCommand().GetEnabled() || !tools.GetViewFile().GetEnabled() || !tools.GetWriteToFile().GetEnabled() ||
		!tools.GetFileEdit().GetEnabled() || tools.GetListDir().GetEnabled() || tools.GetUserQuestions().GetEnabled() {
		t.Fatalf("lightweight harness config %+v", tools)
	}

	cfg = Options{Capabilities: &CapabilitiesConfig{CompactionThreshold: 8000}}.Lightweight()
	if cfg.Capabilities.CompactionThreshold != 8000 || cfg.Compaction != nil {
		t.Fatalf("legacy compaction kept %+v", cfg)
	}
	cfg = Options{Compaction: &CompactionConfig{TokenThreshold: 12345}}.Lightweight()
	if cfg.Compaction.TokenThreshold != 12345 || harnessConfig(t, cfg).GetCompactionThreshold() != 12345 {
		t.Fatal("explicit compaction not kept")
	}
	cfg = Options{Capabilities: &CapabilitiesConfig{DisabledTools: []BuiltinTool{BuiltinRunCommand}, AgentBehavior: AgentBehaviorInteractive}}.Lightweight()
	if !reflect.DeepEqual(cfg.Capabilities.EnabledTools, []BuiltinTool{BuiltinViewFile, BuiltinCreateFile, BuiltinEditFile}) ||
		cfg.Capabilities.DisabledTools != nil || cfg.Capabilities.AgentBehavior != AgentBehaviorInteractive {
		t.Fatalf("lightweight with disabled tools %+v", cfg.Capabilities)
	}
}

func TestEval(t *testing.T) {
	clearModelEnv(t)
	orig := Options{Model: "gemini-3.1-pro-preview"}
	cfg, err := orig.Eval(ThinkingHigh)
	if err != nil {
		t.Fatal(err)
	}
	caps := cfg.Capabilities
	if cfg.Model != "gemini-3.1-pro-preview" || !caps.DisableSubagents || !reflect.DeepEqual(caps.DisabledTools, []BuiltinTool{BuiltinGenerateImage}) ||
		caps.EnabledTools != nil || !caps.RunCommand.EnableDaemons || len(cfg.Policies) != 1 || cfg.Policies[0].Name != "allow_all" ||
		*cfg.Retry.APIRetry.MaxRetries != math.MaxUint32 {
		t.Fatalf("eval %+v %+v", cfg, caps)
	}
	if orig.Capabilities != nil || orig.Policies != nil || orig.Models != nil {
		t.Fatal("Eval modified the original")
	}
	models := cfg.ResolvedModels()
	if len(models) != 2 || models[0].Name != "gemini-3.1-pro-preview" || models[0].Endpoint.(*GeminiAPIEndpoint).Options.ThinkingLevel != ThinkingHigh ||
		models[1].Endpoint.(*GeminiAPIEndpoint).Options != nil {
		t.Fatalf("eval models %+v", models)
	}
	hc := harnessConfig(t, cfg)
	if hc.GetHarnessSideTools().GetGenerateImage().GetEnabled() || !hc.GetHarnessSideTools().GetRunCommand().GetEnabled() ||
		hc.GetRetryConfig().GetApiRetry().GetInitialSleepDurationMs() != 1000 || hc.GetRetryConfig().GetModelOutputRetry() != nil ||
		len(hc.GetPolicyConfig().GetRules()) != 1 || hc.GetPolicyConfig().GetWorkspaceContainment() != wire.PolicyConfig_WORKSPACE_CONTAINMENT_DISABLED ||
		len(hc.GetModels()) != 2 || hc.GetModels()[0].GetGeminiApiEndpoint().GetOptions().GetThinkingLevel() != "high" {
		t.Fatalf("eval harness config %+v", hc)
	}

	custom := &RetryConfig{APIRetry: &ModelAPIRetryConfig{MaxRetries: new(uint32(3))}}
	cfg, err = Options{Retry: custom, Policies: ConfirmRunCommandPolicies(nil), Capabilities: &CapabilitiesConfig{DisabledTools: []BuiltinTool{BuiltinSearchWeb}}}.Eval("")
	if err != nil || *cfg.Retry.APIRetry.MaxRetries != 3 || len(cfg.Policies) != 2 || !reflect.DeepEqual(cfg.Capabilities.DisabledTools, []BuiltinTool{BuiltinSearchWeb}) {
		t.Fatalf("eval overrides %+v %v", cfg, err)
	}
	cfg, _ = Options{Capabilities: &CapabilitiesConfig{EnabledTools: []BuiltinTool{BuiltinViewFile}}}.Eval("")
	if cfg.Capabilities.DisabledTools != nil {
		t.Fatal("disabled tools added despite enabled tools")
	}

	if _, err := (Options{Models: []ModelTarget{{Name: "x", Endpoint: &GeminiAPIEndpoint{Options: &GeminiModelOptions{ThinkingLevel: ThinkingLow}}}}}).Eval(ThinkingHigh); err == nil || !strings.Contains(err.Error(), "already sets thinking_level") {
		t.Fatalf("existing thinking level: %v", err)
	}
	if _, err := (Options{Models: []ModelTarget{{Name: "x"}}}).Eval(ThinkingHigh); err == nil || !strings.Contains(err.Error(), "endpoint must be a GeminiAPIEndpoint or VertexEndpoint") {
		t.Fatalf("nil endpoint: %v", err)
	}
	cfg, err = (Options{Models: []ModelTarget{{Name: "x", Endpoint: &VertexEndpoint{Project: "p", Location: "l", Options: &GeminiModelOptions{ServiceTier: ServiceTierFlex}}}}}).Eval(ThinkingExtraHigh)
	if err != nil {
		t.Fatal(err)
	}
	if o := cfg.ResolvedModels()[0].Endpoint.(*VertexEndpoint).Options; o.ThinkingLevel != ThinkingExtraHigh || o.ServiceTier != ServiceTierFlex {
		t.Fatalf("vertex options %+v", o)
	}
	if _, err := cfg.Eval(ThinkingHigh); err == nil {
		t.Fatal("applying a thinking level twice succeeded")
	}
	// Explicit models resolve the same after Eval.
	before := len(cfg.ResolvedModels())
	if after := len(slices.Clone(cfg.ResolvedModels())); after != before {
		t.Fatalf("models changed %d -> %d", before, after)
	}
}

// TestOpenAI covers upstream's local_openai_connection_test: an
// OpenAI-compatible server replaces the Gemini models.
func TestOpenAI(t *testing.T) {
	clearModelEnv(t)
	cfg := Options{Model: "llama3.1", OpenAI: &OpenAIEndpoint{BaseURL: "http://localhost:11434/v1"}}
	hc := harnessConfig(t, cfg)
	if len(hc.GetModels()) != 1 {
		t.Fatalf("models %+v", hc.GetModels())
	}
	m := hc.GetModels()[0]
	if m.GetName() != "llama3.1" || !reflect.DeepEqual(m.GetTypes(), []wire.ModelType{wire.ModelType_MODEL_TYPE_TEXT}) ||
		m.GetGemmaEndpoint().GetBaseUrl() != "http://localhost:11434/v1" || m.GetGeminiApiEndpoint() != nil {
		t.Fatalf("model %+v", m)
	}
	// No Gemini credentials are needed.
	if err := validateModels(cfg.ResolvedModels()); err != nil {
		t.Fatal(err)
	}
	// Defaults match the local config: all default tools, confirm_run_command.
	if cfg.Capabilities != nil || len(cfg.policies()) != 2 || cfg.policies()[0].Tool != "run_command" || cfg.policies()[1].Tool != WildcardTool {
		t.Fatalf("defaults %+v", cfg.policies())
	}

	// An empty base URL fails at session start.
	empty := Options{Model: "test", OpenAI: &OpenAIEndpoint{}}
	err := validateModels(empty.ResolvedModels())
	var verr *ValidationError
	if !errors.As(err, &verr) || !strings.Contains(err.Error(), "non-empty BaseURL") {
		t.Fatalf("empty base URL: %v", err)
	}

	// Lightweight keeps the backend.
	lw := cfg.Lightweight()
	if lw.OpenAI == nil || lw.OpenAI == cfg.OpenAI || lw.OpenAI.BaseURL != cfg.OpenAI.BaseURL || lw.Model != "llama3.1" ||
		lw.Capabilities.AgentBehavior != AgentBehaviorMinimal || !reflect.DeepEqual(lw.Capabilities.EnabledTools, MinimalTools()) ||
		lw.Compaction.TokenThreshold != 65536 || !lw.Capabilities.DisableSubagents {
		t.Fatalf("lightweight %+v", lw)
	}
	lw = Options{Model: "m", OpenAI: cfg.OpenAI, Compaction: &CompactionConfig{TokenThreshold: 20000}}.Lightweight()
	if lw.Compaction.TokenThreshold != 20000 {
		t.Fatal("explicit compaction not kept")
	}

	// Eval cannot set a thinking level on an OpenAI-compatible model.
	if _, err := cfg.Eval(ThinkingHigh); err == nil || !strings.Contains(err.Error(), "only supported on Gemini or Vertex") {
		t.Fatalf("eval: %v", err)
	}
	ev, err := cfg.Eval("")
	if err != nil || len(harnessConfig(t, ev).GetModels()) != 1 {
		t.Fatalf("eval without thinking level: %v", err)
	}

	// MCP servers and subagents are passed through.
	cfg.MCPServers = []MCPServer{&MCPStdioServer{Name: "test_mcp", Command: "echo", Args: []string{"hello"}}}
	cfg.Subagents = []SubagentConfig{{Name: "test_subagent", Description: "A test subagent", SystemInstructions: TextSystemInstructions("You are a subagent")}}
	hc = harnessConfig(t, cfg)
	if len(hc.GetMcpServers()) != 1 || hc.GetMcpServers()[0].GetName() != "test_mcp" || len(hc.GetCustomSubagents()) != 1 || hc.GetCustomSubagents()[0].GetName() != "test_subagent" {
		t.Fatalf("mcp/subagents %+v %+v", hc.GetMcpServers(), hc.GetCustomSubagents())
	}

	// An OpenAIEndpoint can also be one target among others.
	mixed := Options{Models: []ModelTarget{{Name: "local", Endpoint: &OpenAIEndpoint{BaseURL: "http://x/v1"}}}, APIKey: "k"}
	hc = harnessConfig(t, mixed)
	if len(hc.GetModels()) != 2 || hc.GetModels()[0].GetGemmaEndpoint().GetBaseUrl() != "http://x/v1" || hc.GetModels()[1].GetName() != DefaultImageGenerationModel {
		t.Fatalf("mixed models %+v", hc.GetModels())
	}
}
