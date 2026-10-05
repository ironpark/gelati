package antigravity

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ironpark/gelati/antigravity/internal/wire"
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
func harnessConfig(t *testing.T, cfg Config, hooks ...Hook) *wire.HarnessConfig {
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
		cfg     Config
		wantErr string
	}{
		"zero":                {Config{}, ""},
		"valid id":            {Config{ConversationID: "12345678901234567890123456789012"}, ""},
		"uuid id":             {Config{ConversationID: "12345678-1234-1234-1234-123456789012"}, ""},
		"short id":            {Config{ConversationID: "too-short"}, "must be at least 32 characters long"},
		"bad id chars":        {Config{ConversationID: "invalid_char_because_of_underscores_123"}, "must match [a-zA-Z0-9-]"},
		"resume without id":   {Config{SessionContinuationMode: SessionResume}, "must be specified when session_continuation_mode is RESUME"},
		"resume with id":      {Config{SessionContinuationMode: SessionResume, ConversationID: strings.Repeat("a", 32)}, ""},
		"bad mode":            {Config{SessionContinuationMode: "sometimes"}, "unknown session continuation mode"},
		"relative app dir":    {Config{AppDataDir: "relative/path"}, "app_data_dir must be an absolute path"},
		"schema string":       {Config{ResponseSchema: `{"type": "object"}`}, ""},
		"schema invalid":      {Config{ResponseSchema: "{not json"}, "response_schema string is not valid JSON."},
		"schema map":          {Config{ResponseSchema: map[string]any{"type": "object"}}, ""},
		"schema type":         {Config{ResponseSchema: struct{ A int }{}}, ""},
		"allowed known":       {Config{Subagents: []SubagentConfig{sub}, Capabilities: &CapabilitiesConfig{AllowedSubagents: []string{"researcher"}}}, ""},
		"allowed unknown":     {Config{Subagents: []SubagentConfig{sub}, Capabilities: &CapabilitiesConfig{AllowedSubagents: []string{"non_existent"}}}, "Unknown subagent name(s)"},
		"sub allowed unknown": {Config{Subagents: []SubagentConfig{{Name: "researcher", Capabilities: &SubagentCapabilities{EnabledTools: []BuiltinTool{BuiltinStartSubagent}, AllowedSubagents: []string{"ghost_agent"}}}}}, "ghost_agent"},
		"bad mcp":             {Config{MCPServers: []MCPServer{&MCPStdioServer{Name: "bad name"}}}, "must match"},
		"multiple auto":       {Config{Policies: []Policy{{Auto: true}, {Auto: true}}}, "Multiple AutoPolicy"},
		"nil hook":            {Config{Hooks: []Hook{nil}}, "nil hook"},
		"nil trigger":         {Config{Triggers: []Trigger{nil}}, "trigger 0 is nil"},
		"negative compaction": {Config{Compaction: &CompactionConfig{TokenThreshold: -1}}, "must be positive"},
		"bad budget":          {Config{Budget: &BudgetConfig{MaxModelCalls: -1}}, "max_model_calls"},
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
	cfg := Config{
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
	cfg = Config{Tools: []*Tool{mk("tool_fn")}, Subagents: []SubagentConfig{{Name: "sub1", Tools: []*Tool{mk("tool_fn")}}}}
	if _, err := cfg.allCustomTools(); err == nil || !strings.Contains(err.Error(), "Duplicate custom tool name 'tool_fn' detected across agent and subagent 'sub1' configurations.") {
		t.Fatalf("collision: %v", err)
	}
	if err := (&Config{Tools: []*Tool{nil}}).Validate(); err == nil {
		t.Fatal("nil tool accepted")
	}
}

func TestResolvedModels(t *testing.T) {
	clearModelEnv(t)
	models := (&Config{}).ResolvedModels()
	if len(models) != 2 || models[0].Name != DefaultModel || !reflect.DeepEqual(models[0].Types, []ModelType{ModelTypeText}) ||
		models[1].Name != DefaultImageGenerationModel || !reflect.DeepEqual(models[1].Types, []ModelType{ModelTypeImage}) {
		t.Fatalf("defaults %+v", models)
	}
	if _, ok := models[0].Endpoint.(*GeminiAPIEndpoint); !ok {
		t.Fatalf("default endpoint %T", models[0].Endpoint)
	}

	models = (&Config{Model: "custom-text-model", APIKey: "my-key"}).ResolvedModels()
	if len(models) != 2 || models[0].Name != "custom-text-model" || models[1].Name != DefaultImageGenerationModel {
		t.Fatalf("shorthand %+v", models)
	}
	if e := models[0].Endpoint.(*GeminiAPIEndpoint); e.APIKey != "my-key" {
		t.Fatalf("shorthand endpoint %+v", e)
	}

	img := ModelTarget{Name: "custom-image-model", Types: []ModelType{ModelTypeImage}}
	models = (&Config{Models: []ModelTarget{img}}).ResolvedModels()
	if len(models) != 2 || models[0].Name != "custom-image-model" || models[1].Name != DefaultModel {
		t.Fatalf("explicit %+v", models)
	}
	models = (&Config{Model: "custom-text-model", Models: []ModelTarget{img}}).ResolvedModels()
	if len(models) != 2 || models[0].Name != "custom-image-model" || models[1].Name != "custom-text-model" {
		t.Fatalf("explicit + shorthand %+v", models)
	}
}

func TestVertexEnvironment(t *testing.T) {
	clearModelEnv(t)
	t.Setenv("GOOGLE_GENAI_USE_VERTEXAI", "True")
	t.Setenv("GOOGLE_CLOUD_PROJECT", "env-project")
	t.Setenv("GOOGLE_CLOUD_LOCATION", "env-location")
	models := (&Config{Model: "gemini-3.8-flash"}).ResolvedModels()
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
	if _, ok := (&Config{}).ResolvedModels()[0].Endpoint.(*VertexEndpoint); !ok {
		t.Fatal("GOOGLE_GENAI_USE_ENTERPRISE did not route to Vertex")
	}
	clearModelEnv(t)
	models = (&Config{Vertex: true, APIKey: "express-key"}).ResolvedModels()
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

	if err := validateModels((&Config{Models: []ModelTarget{{Name: "m"}}}).ResolvedModels()); err == nil || !strings.Contains(err.Error(), "must have an endpoint configured") {
		t.Fatalf("missing endpoint: %v", err)
	}
	cc, err := (&Config{}).compile()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connectLocal(t.Context(), cc); err == nil || !strings.Contains(err.Error(), "A Gemini API key is required") {
		t.Fatalf("connect without key: %v", err)
	}
}

func TestHarnessConfigDefaults(t *testing.T) {
	clearModelEnv(t)
	hc := harnessConfig(t, Config{Workspaces: []string{}})
	tools := hc.GetHarnessSideTools()
	if !tools.GetSubagents().GetEnabled() || tools.GetUserQuestions().GetEnabled() || !tools.GetRunCommand().GetEnabled() ||
		!tools.GetManageTask().GetEnabled() || !tools.GetSchedule().GetEnabled() || tools.GetFind().GetEnabled() ||
		tools.GetGrepSearch().GetEnabled() || !tools.GetGenerateImage().GetEnabled() {
		t.Fatalf("default tools %+v", tools)
	}
	if hc.SystemInstructions != nil || len(hc.Workspaces) != 0 || hc.GetAgentBehavior() != wire.AgentBehaviorAutonomous ||
		hc.GetCascadeID() != "" || hc.GetSessionContinuationMode() != wire.HarnessConfigSessionContinuationModeUnspecified ||
		hc.GetCompactionThreshold() != 0 || hc.CompactionConfig != nil || hc.RetryConfig != nil || hc.BudgetConfig != nil {
		t.Fatalf("defaults %+v", hc)
	}
	if hc.GetAppDataDir() != defaultAppDataDir() || !strings.HasSuffix(hc.GetAppDataDir(), filepath.Join(".gemini", "antigravity")) {
		t.Fatalf("app data dir %q", hc.GetAppDataDir())
	}
	// Default policies: confirm_run_command.
	rules := hc.GetPolicyConfig().GetRules()
	if len(rules) != 2 || rules[0].GetTool() != "run_command" || rules[0].GetDecision() != wire.PolicyDecisionDeny || rules[1].GetTool() != "*" {
		t.Fatalf("default policies %+v", rules)
	}
	// Models: the default text and image models.
	if len(hc.Models) != 2 || hc.Models[0].GetName() != DefaultModel || hc.Models[0].GetGeminiAPIEndpoint() == nil {
		t.Fatalf("models %+v", hc.Models)
	}
	// A nil workspace list means the current directory.
	wd, _ := os.Getwd()
	hc = harnessConfig(t, Config{})
	if len(hc.Workspaces) != 1 || hc.Workspaces[0].GetFilesystemWorkspace().GetDirectory() != filepath.ToSlash(wd) {
		t.Fatalf("default workspace %+v", hc.Workspaces)
	}
	// Explicit empty policies send no policy config.
	if hc := harnessConfig(t, Config{Policies: []Policy{}, Capabilities: &CapabilitiesConfig{EnabledTools: ReadOnlyTools()}}); hc.PolicyConfig != nil {
		t.Fatalf("empty policies %+v", hc.PolicyConfig)
	}
}

func TestHarnessConfigCapabilities(t *testing.T) {
	hc := harnessConfig(t, Config{Capabilities: &CapabilitiesConfig{
		DisabledTools: []BuiltinTool{BuiltinRunCommand, BuiltinSchedule, BuiltinAskQuestion, BuiltinGenerateImage},
	}})
	tools := hc.GetHarnessSideTools()
	if tools.GetRunCommand().GetEnabled() || tools.GetSchedule().GetEnabled() || tools.GetManageTask().GetEnabled() ||
		tools.GetUserQuestions().GetEnabled() || tools.GetGenerateImage().GetEnabled() || !tools.GetSubagents().GetEnabled() ||
		tools.GetFind().GetEnabled() || !tools.GetFileEdit().GetEnabled() || !tools.GetViewFile().GetEnabled() ||
		!tools.GetWriteToFile().GetEnabled() || tools.GetListDir().GetEnabled() || !tools.GetSearchWeb().GetEnabled() {
		t.Fatalf("disabled tools %+v", tools)
	}

	hc = harnessConfig(t, Config{Capabilities: &CapabilitiesConfig{EnabledTools: []BuiltinTool{BuiltinViewFile}}})
	want := &wire.HarnessSideTools{
		ViewFile:       &wire.ViewFileToolConfig{Enabled: new(true)},
		Subagents:      &wire.SubagentsConfig{Enabled: new(false)},
		UserQuestions:  &wire.UserQuestionsConfig{Enabled: new(false)},
		RunCommand:     &wire.RunCommandToolConfig{Enabled: new(false), EnableDaemonCommands: new(false), MaxTimeoutMs: new(uint32(0)), EnableSandbox: new(false)},
		ManageTask:     &wire.ManageTaskToolConfig{Enabled: new(false)},
		Schedule:       &wire.ScheduleToolConfig{Enabled: new(false)},
		Find:           &wire.FindToolConfig{Enabled: new(false)},
		GenerateImage:  &wire.GenerateImageToolConfig{Enabled: new(false)},
		FileEdit:       &wire.FileEditToolConfig{Enabled: new(false)},
		WriteToFile:    &wire.WriteToFileToolConfig{Enabled: new(false)},
		GrepSearch:     &wire.GrepSearchToolConfig{Enabled: new(false)},
		ListDir:        &wire.ListDirToolConfig{Enabled: new(false)},
		SearchWeb:      &wire.SearchWebToolConfig{Enabled: new(false)},
		ReadURLContent: &wire.ReadURLContentToolConfig{Enabled: new(false)},
	}
	gb, _ := wire.Marshal(hc.GetHarnessSideTools())
	wb, _ := wire.Marshal(want)
	if string(gb) != string(wb) {
		t.Fatalf("enabled tools\n got %s\nwant %s", gb, wb)
	}

	hc = harnessConfig(t, Config{Capabilities: &CapabilitiesConfig{RunCommand: &RunCommandConfig{Timeout: 60 * time.Second, EnableSandbox: true}}})
	rc := hc.GetHarnessSideTools().GetRunCommand()
	if rc.GetMaxTimeoutMs() != 60000 || rc.GetEnableDaemonCommands() || !rc.GetEnableSandbox() || !rc.GetEnabled() {
		t.Fatalf("run command %+v", rc)
	}
	hc = harnessConfig(t, Config{Capabilities: &CapabilitiesConfig{RunCommand: &RunCommandConfig{EnableDaemons: true}}})
	if rc := hc.GetHarnessSideTools().GetRunCommand(); !rc.GetEnableDaemonCommands() || rc.GetMaxTimeoutMs() != 0 {
		t.Fatalf("daemons %+v", rc)
	}

	sub := SubagentConfig{Name: "researcher"}
	hc = harnessConfig(t, Config{
		Subagents:    []SubagentConfig{sub, {Name: "reviewer"}},
		Capabilities: &CapabilitiesConfig{MaxSubagentDepth: 3, AllowedSubagents: []string{"researcher", "reviewer"}},
	})
	sc := hc.GetHarnessSideTools().GetSubagents()
	if !sc.GetEnabled() || sc.GetMaxNestingDepth() != 3 || !reflect.DeepEqual(sc.GetAllowedSubagents(), []string{"researcher", "reviewer"}) {
		t.Fatalf("subagents %+v", sc)
	}
	for _, caps := range []*CapabilitiesConfig{{DisabledTools: []BuiltinTool{BuiltinStartSubagent}}, {DisableSubagents: true}} {
		if harnessConfig(t, Config{Capabilities: caps}).GetHarnessSideTools().GetSubagents().GetEnabled() {
			t.Errorf("subagents enabled with %+v", caps)
		}
	}
	hc = harnessConfig(t, Config{Capabilities: &CapabilitiesConfig{AgentBehavior: AgentBehaviorInteractive, EnabledTools: []BuiltinTool{BuiltinViewFile, BuiltinAskQuestion}}})
	if hc.GetAgentBehavior() != wire.AgentBehaviorInteractive || !hc.GetHarnessSideTools().GetUserQuestions().GetEnabled() || hc.GetHarnessSideTools().GetRunCommand().GetEnabled() {
		t.Fatalf("interactive %+v", hc.GetHarnessSideTools())
	}
	hc = harnessConfig(t, Config{Capabilities: &CapabilitiesConfig{EnabledTools: []BuiltinTool{BuiltinFindFile, BuiltinSearchDir}}})
	if !hc.GetHarnessSideTools().GetFind().GetEnabled() || !hc.GetHarnessSideTools().GetGrepSearch().GetEnabled() {
		t.Fatal("find/grep not enabled")
	}
}

func TestHarnessConfigCompactionRetryBudgetTruncation(t *testing.T) {
	hc := harnessConfig(t, Config{Capabilities: &CapabilitiesConfig{CompactionThreshold: 50000}})
	if hc.GetCompactionThreshold() != 50000 || hc.GetCompactionConfig().GetTokenThreshold() != 50000 {
		t.Fatalf("legacy compaction %+v", hc.CompactionConfig)
	}
	hc = harnessConfig(t, Config{Capabilities: &CapabilitiesConfig{CompactionThreshold: 50000}, Compaction: &CompactionConfig{TokenThreshold: 30000}})
	if hc.GetCompactionThreshold() != 30000 || hc.GetCompactionConfig().GetTokenThreshold() != 30000 {
		t.Fatalf("compaction precedence %+v", hc.CompactionConfig)
	}

	if harnessConfig(t, Config{Retry: &RetryConfig{}}).RetryConfig != nil {
		t.Fatal("empty retry config sent")
	}
	hc = harnessConfig(t, Config{Retry: &RetryConfig{APIRetry: &ModelAPIRetryConfig{
		MaxRetries: new(uint32(5)), InitialSleepDurationMs: new(uint32(500)), ExponentialMultiplier: new(1.5), JitterRange: new(0.1),
	}}})
	if a := hc.GetRetryConfig().GetAPIRetry(); a.GetMaxRetries() != 5 || a.GetInitialSleepDurationMs() != 500 || a.GetExponentialMultiplier() != 1.5 || a.GetJitterRange() != 0.1 || hc.GetRetryConfig().ModelOutputRetry != nil {
		t.Fatalf("api retry %+v", hc.RetryConfig)
	}
	hc = harnessConfig(t, Config{Retry: &RetryConfig{ModelOutputRetry: &ModelOutputRetryConfig{MaxRetries: new(uint32(math.MaxUint32))}, APIRetry: &ModelAPIRetryConfig{MaxRetries: new(uint32(0))}}})
	if hc.GetRetryConfig().GetModelOutputRetry().GetMaxRetries() != math.MaxUint32 || hc.GetRetryConfig().GetAPIRetry().MaxRetries == nil {
		t.Fatalf("retry edges %+v", hc.RetryConfig)
	}

	if harnessConfig(t, Config{Budget: &BudgetConfig{}}).BudgetConfig != nil {
		t.Fatal("empty budget sent")
	}
	hc = harnessConfig(t, Config{Budget: &BudgetConfig{MaxModelCalls: 5, MaxToolCalls: 10, MaxInputTokens: 500, MaxOutputTokens: 200, MaxTotalTokens: 1000}})
	b := hc.GetBudgetConfig()
	if b.GetMaxModelCalls() != 5 || b.GetMaxToolCalls() != 10 || b.GetMaxInputTokens() != 500 || b.GetMaxOutputTokens() != 200 || b.GetMaxTotalTokens() != 1000 || b.GetScope() != wire.BudgetConfigBudgetScopeLifetime {
		t.Fatalf("budget %+v", b)
	}
	if hc := harnessConfig(t, Config{Budget: &BudgetConfig{MaxTotalTokens: 2000, Scope: BudgetScopeForwardLooking}}); hc.GetBudgetConfig().GetScope() != wire.BudgetConfigBudgetScopeForwardLooking {
		t.Fatal("forward-looking scope")
	}

	if harnessConfig(t, Config{}).ToolOutputTruncation != nil {
		t.Fatal("truncation sent by default")
	}
	for _, n := range []int{2048, 0} {
		hc := harnessConfig(t, Config{Capabilities: &CapabilitiesConfig{ToolOutputTruncation: &ToolOutputTruncationConfig{MaxTokens: n}}})
		if tr := hc.GetToolOutputTruncation().GetTruncate(); tr == nil || tr.GetMaxTokens() != int32(n) {
			t.Fatalf("truncation %d: %+v", n, hc.ToolOutputTruncation)
		}
	}
}

func TestHarnessConfigSessionModelsAndInstructions(t *testing.T) {
	clearModelEnv(t)
	id := "12345678901234567890123456789012"
	for mode, want := range map[SessionContinuationMode]wire.HarnessConfigSessionContinuationMode{
		SessionResume:         wire.HarnessConfigSessionContinuationModeResume,
		SessionCreateOrResume: wire.HarnessConfigSessionContinuationModeCreateOrResume,
		SessionCreateOnly:     wire.HarnessConfigSessionContinuationModeCreateOnly,
	} {
		hc := harnessConfig(t, Config{ConversationID: id, SessionContinuationMode: mode})
		if hc.GetSessionContinuationMode() != want || hc.GetCascadeID() != id {
			t.Errorf("mode %s: %s %s", mode, hc.GetSessionContinuationMode(), hc.GetCascadeID())
		}
	}

	hc := harnessConfig(t, Config{Models: []ModelTarget{
		{Name: "gemini-2.5-pro", Endpoint: &GeminiAPIEndpoint{APIKey: "test-key", Options: &GeminiModelOptions{ThinkingLevel: ThinkingHigh}}},
		{Name: "imagen-3-custom", Types: []ModelType{ModelTypeImage}, Endpoint: &GeminiAPIEndpoint{Options: &GeminiModelOptions{}}},
	}})
	if len(hc.Models) != 2 || hc.Models[0].GetGeminiAPIEndpoint().GetAPIKey() != "test-key" ||
		hc.Models[0].GetGeminiAPIEndpoint().GetOptions().GetThinkingLevel() != "high" ||
		!reflect.DeepEqual(hc.Models[1].GetTypes(), []wire.ModelType{wire.ModelTypeImage}) || hc.Models[1].GetGeminiAPIEndpoint().Options != nil {
		t.Fatalf("models %+v", hc.Models)
	}
	for _, tier := range []ServiceTier{ServiceTierStandard, ServiceTierPriority, ServiceTierFlex} {
		hc := harnessConfig(t, Config{Models: []ModelTarget{{Endpoint: &VertexEndpoint{Project: "p", Location: "l", Options: &GeminiModelOptions{ServiceTier: tier}}}}})
		if got := hc.Models[0].GetVertexEndpoint().GetOptions().GetServiceTier(); got != string(tier) {
			t.Errorf("tier %s: %q", tier, got)
		}
		if hc.Models[0].GetName() != "" || hc.Models[0].GetVertexEndpoint().GetProject() != "p" {
			t.Errorf("vertex model %+v", hc.Models[0])
		}
	}

	if si := harnessConfig(t, Config{SystemInstructions: TextSystemInstructions("Be concise.")}).GetSystemInstructions(); si.GetAppended().GetAppendedSections()[0].GetContent() != "Be concise." ||
		si.GetAppended().GetAppendedSections()[0].GetTitle() != "user_system_instructions" {
		t.Fatalf("text instructions %+v", si)
	}
	if si := harnessConfig(t, Config{SystemInstructions: CustomSystemInstructions{Text: "Override everything."}}).GetSystemInstructions(); si.GetCustom().GetPart()[0].GetText() != "Override everything." {
		t.Fatalf("custom instructions %+v", si)
	}
	si := harnessConfig(t, Config{SystemInstructions: TemplatedSystemInstructions{Identity: "New Identity", Sections: []SystemInstructionSection{{Title: "extra", Content: "More instructions"}}}}).GetSystemInstructions()
	if si.GetAppended().GetCustomIdentity() != "New Identity" || si.GetAppended().GetAppendedSections()[0].GetTitle() != "extra" {
		t.Fatalf("templated instructions %+v", si)
	}
	if si := harnessConfig(t, Config{SystemInstructions: &TemplatedSystemInstructions{Identity: "Only Identity"}}).GetSystemInstructions(); si.GetAppended().GetCustomIdentity() != "Only Identity" || len(si.GetAppended().GetAppendedSections()) != 0 {
		t.Fatalf("identity only %+v", si)
	}
	if si := harnessConfig(t, Config{SystemInstructions: TextSystemInstructions("")}).SystemInstructions; si != nil {
		t.Fatalf("empty instructions %+v", si)
	}

	hc = harnessConfig(t, Config{SkillsPaths: []string{"/skills/a", "/skills/b"}, AppDataDir: "/custom/app/data", ResponseSchema: map[string]any{"properties": map[string]any{"field": map[string]any{"type": "string"}}}})
	if !reflect.DeepEqual(hc.GetSkillsPaths(), []string{"/skills/a", "/skills/b"}) || hc.GetAppDataDir() != "/custom/app/data" ||
		hc.GetFinishToolSchemaJSON() != `{"properties":{"field":{"type":"string"}}}` {
		t.Fatalf("misc %+v", hc)
	}
	hc = harnessConfig(t, Config{Capabilities: &CapabilitiesConfig{FinishToolSchemaJSON: `{"type": "object"}`}})
	if hc.GetFinishToolSchemaJSON() != `{"type": "object"}` {
		t.Fatalf("finish schema %q", hc.GetFinishToolSchemaJSON())
	}
}

func TestHarnessConfigWorkspaces(t *testing.T) {
	tmp := resolvePath(os.TempDir())
	home, _ := os.UserHomeDir()
	wd, _ := os.Getwd()
	hc := harnessConfig(t, Config{Workspaces: []string{
		"file:///dev/shm/workspace", filepath.Join(tmp, "clean-path"), "ws", "~/my_project", "cns://el-d/home/user/project", "/cns/el-d/home/user/data",
	}})
	var got []string
	for _, w := range hc.Workspaces {
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
	hc := harnessConfig(t, Config{MCPServers: []MCPServer{
		&MCPStreamableHTTPServer{Name: "my_http_server", URL: "http://localhost:8080/mcp", Headers: map[string]string{"Authorization": "Bearer token123"}, TimeoutSeconds: 30},
		&MCPStdioServer{Name: "my_stdio_server", Command: "node", Args: []string{"server.js"}, Env: map[string]string{"NODE_ENV": "production"}, TimeoutSeconds: 10, EnabledTools: []string{"add", "sub"}},
	}})
	h, s := hc.MCPServers[0], hc.MCPServers[1]
	if h.GetName() != "my_http_server" || h.GetHTTP().GetURL() != "http://localhost:8080/mcp" || h.GetHTTP().GetHeaders()["Authorization"] != "Bearer token123" || h.GetTimeoutSeconds() != 30 || h.Stdio != nil {
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
	hc := harnessConfig(t, Config{
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
	for _, tl := range hc.Tools {
		rootNames = append(rootNames, tl.GetName())
	}
	if !reflect.DeepEqual(rootNames, []string{"root_tool", "shared_tool", "sub_tool", "builtin_thing"}) {
		t.Fatalf("root tools %v", rootNames)
	}
	if hc.Tools[2].GetParametersJSONSchema() == "" || hc.Tools[3].GetParametersJSONSchema() != "" {
		t.Fatalf("tool declarations %+v", hc.Tools)
	}
	var schema map[string]any
	if err := json.Unmarshal([]byte(hc.Tools[0].GetParametersJSONSchema()), &schema); err != nil || schema["type"] != "object" || hc.Tools[0].GetDescription() != "root_tool docs" {
		t.Fatalf("schema %v %v", schema, err)
	}
	helper, plain := hc.CustomSubagents[0], hc.CustomSubagents[1]
	var subNames []string
	for _, tl := range helper.GetTools() {
		subNames = append(subNames, tl.GetName())
	}
	if !reflect.DeepEqual(subNames, []string{"shared_tool", "sub_tool", "grep_search"}) || helper.GetModel().GetName() != "gemini-2.5-flash" ||
		helper.GetAgentBehavior() != wire.AgentBehaviorMinimal || !helper.GetHarnessSideTools().GetSubagents().GetEnabled() ||
		helper.GetSystemInstructions().GetAppended() == nil || helper.GetDescription() != "Helps." {
		t.Fatalf("helper %+v", helper)
	}
	// Subagents default to read-only tools, without subagents.
	pt := plain.GetHarnessSideTools()
	if !pt.GetViewFile().GetEnabled() || pt.GetRunCommand().GetEnabled() || pt.GetSubagents().GetEnabled() || plain.Model != nil ||
		plain.GetAgentBehavior() != wire.AgentBehaviorAutonomous {
		t.Fatalf("plain subagent %+v", plain)
	}
}

func TestLightweight(t *testing.T) {
	clearModelEnv(t)
	orig := Config{Model: "gemini-3.8-flash"}
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
	if hc.GetAgentBehavior() != wire.AgentBehaviorMinimal || hc.GetCompactionThreshold() != 65536 || tools.GetSubagents().GetEnabled() ||
		!tools.GetRunCommand().GetEnabled() || !tools.GetViewFile().GetEnabled() || !tools.GetWriteToFile().GetEnabled() ||
		!tools.GetFileEdit().GetEnabled() || tools.GetListDir().GetEnabled() || tools.GetUserQuestions().GetEnabled() {
		t.Fatalf("lightweight harness config %+v", tools)
	}

	cfg = Config{Capabilities: &CapabilitiesConfig{CompactionThreshold: 8000}}.Lightweight()
	if cfg.Capabilities.CompactionThreshold != 8000 || cfg.Compaction != nil {
		t.Fatalf("legacy compaction kept %+v", cfg)
	}
	cfg = Config{Compaction: &CompactionConfig{TokenThreshold: 12345}}.Lightweight()
	if cfg.Compaction.TokenThreshold != 12345 || harnessConfig(t, cfg).GetCompactionThreshold() != 12345 {
		t.Fatal("explicit compaction not kept")
	}
	cfg = Config{Capabilities: &CapabilitiesConfig{DisabledTools: []BuiltinTool{BuiltinRunCommand}, AgentBehavior: AgentBehaviorInteractive}}.Lightweight()
	if !reflect.DeepEqual(cfg.Capabilities.EnabledTools, []BuiltinTool{BuiltinViewFile, BuiltinCreateFile, BuiltinEditFile}) ||
		cfg.Capabilities.DisabledTools != nil || cfg.Capabilities.AgentBehavior != AgentBehaviorInteractive {
		t.Fatalf("lightweight with disabled tools %+v", cfg.Capabilities)
	}
}

func TestEval(t *testing.T) {
	clearModelEnv(t)
	orig := Config{Model: "gemini-3.1-pro-preview"}
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
		hc.GetRetryConfig().GetAPIRetry().GetInitialSleepDurationMs() != 1000 || hc.GetRetryConfig().ModelOutputRetry != nil ||
		len(hc.GetPolicyConfig().GetRules()) != 1 || hc.GetPolicyConfig().GetWorkspaceContainment() != wire.PolicyConfigWorkspaceContainmentDisabled ||
		len(hc.Models) != 2 || hc.Models[0].GetGeminiAPIEndpoint().GetOptions().GetThinkingLevel() != "high" {
		t.Fatalf("eval harness config %+v", hc)
	}

	custom := &RetryConfig{APIRetry: &ModelAPIRetryConfig{MaxRetries: new(uint32(3))}}
	cfg, err = Config{Retry: custom, Policies: ConfirmRunCommandPolicies(nil), Capabilities: &CapabilitiesConfig{DisabledTools: []BuiltinTool{BuiltinSearchWeb}}}.Eval("")
	if err != nil || *cfg.Retry.APIRetry.MaxRetries != 3 || len(cfg.Policies) != 2 || !reflect.DeepEqual(cfg.Capabilities.DisabledTools, []BuiltinTool{BuiltinSearchWeb}) {
		t.Fatalf("eval overrides %+v %v", cfg, err)
	}
	cfg, _ = Config{Capabilities: &CapabilitiesConfig{EnabledTools: []BuiltinTool{BuiltinViewFile}}}.Eval("")
	if cfg.Capabilities.DisabledTools != nil {
		t.Fatal("disabled tools added despite enabled tools")
	}

	if _, err := (Config{Models: []ModelTarget{{Name: "x", Endpoint: &GeminiAPIEndpoint{Options: &GeminiModelOptions{ThinkingLevel: ThinkingLow}}}}}).Eval(ThinkingHigh); err == nil || !strings.Contains(err.Error(), "already sets thinking_level") {
		t.Fatalf("existing thinking level: %v", err)
	}
	if _, err := (Config{Models: []ModelTarget{{Name: "x"}}}).Eval(ThinkingHigh); err == nil || !strings.Contains(err.Error(), "endpoint must be a GeminiAPIEndpoint or VertexEndpoint") {
		t.Fatalf("nil endpoint: %v", err)
	}
	cfg, err = (Config{Models: []ModelTarget{{Name: "x", Endpoint: &VertexEndpoint{Project: "p", Location: "l", Options: &GeminiModelOptions{ServiceTier: ServiceTierFlex}}}}}).Eval(ThinkingExtraHigh)
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
	cfg := Config{Model: "llama3.1", OpenAI: &OpenAIEndpoint{BaseURL: "http://localhost:11434/v1"}}
	hc := harnessConfig(t, cfg)
	if len(hc.Models) != 1 {
		t.Fatalf("models %+v", hc.Models)
	}
	m := hc.Models[0]
	if m.GetName() != "llama3.1" || !reflect.DeepEqual(m.GetTypes(), []wire.ModelType{wire.ModelTypeText}) ||
		m.GetGemmaEndpoint().GetBaseURL() != "http://localhost:11434/v1" || m.GeminiAPIEndpoint != nil {
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
	empty := Config{Model: "test", OpenAI: &OpenAIEndpoint{}}
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
	lw = Config{Model: "m", OpenAI: cfg.OpenAI, Compaction: &CompactionConfig{TokenThreshold: 20000}}.Lightweight()
	if lw.Compaction.TokenThreshold != 20000 {
		t.Fatal("explicit compaction not kept")
	}

	// Eval cannot set a thinking level on an OpenAI-compatible model.
	if _, err := cfg.Eval(ThinkingHigh); err == nil || !strings.Contains(err.Error(), "only supported on Gemini or Vertex") {
		t.Fatalf("eval: %v", err)
	}
	ev, err := cfg.Eval("")
	if err != nil || len(harnessConfig(t, ev).Models) != 1 {
		t.Fatalf("eval without thinking level: %v", err)
	}

	// MCP servers and subagents are passed through.
	cfg.MCPServers = []MCPServer{&MCPStdioServer{Name: "test_mcp", Command: "echo", Args: []string{"hello"}}}
	cfg.Subagents = []SubagentConfig{{Name: "test_subagent", Description: "A test subagent", SystemInstructions: TextSystemInstructions("You are a subagent")}}
	hc = harnessConfig(t, cfg)
	if len(hc.MCPServers) != 1 || hc.MCPServers[0].GetName() != "test_mcp" || len(hc.CustomSubagents) != 1 || hc.CustomSubagents[0].GetName() != "test_subagent" {
		t.Fatalf("mcp/subagents %+v %+v", hc.MCPServers, hc.CustomSubagents)
	}

	// An OpenAIEndpoint can also be one target among others.
	mixed := Config{Models: []ModelTarget{{Name: "local", Endpoint: &OpenAIEndpoint{BaseURL: "http://x/v1"}}}, APIKey: "k"}
	hc = harnessConfig(t, mixed)
	if len(hc.Models) != 2 || hc.Models[0].GetGemmaEndpoint().GetBaseURL() != "http://x/v1" || hc.Models[1].GetName() != DefaultImageGenerationModel {
		t.Fatalf("mixed models %+v", hc.Models)
	}
}
