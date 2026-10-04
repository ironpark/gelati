package claude

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// buildCommandArgs returns the CLI flags the subprocess transport renders
// for opts.
func buildCommandArgs(opts *Options) ([]string, error) {
	launch, err := resolveLaunch(opts)
	if err != nil {
		return nil, err
	}
	return launch.commandArgs(opts), nil
}

// newTestTransport builds a subprocess transport for opts, with its launch
// resolved.
func newTestTransport(t *testing.T, opts *Options) *subprocessTransport {
	t.Helper()
	launch, err := resolveLaunch(opts)
	if err != nil {
		t.Fatal(err)
	}
	return newSubprocessTransport(opts, launch)
}

// initializeExtras returns the initialize fields resolveLaunch renders for
// opts, nil meaning none.
func initializeExtras(t *testing.T, opts *Options) map[string]any {
	t.Helper()
	if opts == nil {
		opts = &Options{}
	}
	launch, err := resolveLaunch(opts)
	if err != nil {
		t.Fatal(err)
	}
	return launch.initFields
}

func TestBuildCommandArgsDefaults(t *testing.T) {
	t.Parallel()
	args, err := buildCommandArgs(&Options{})
	if err != nil {
		t.Fatalf("buildCommandArgs: %v", err)
	}
	// The system prompt travels in the initialize request, not on argv.
	want := []string{
		"--output-format", "stream-json", "--verbose",
		"--input-format", "stream-json",
	}
	if !slices.Equal(args, want) {
		t.Fatalf("args = %q, want %q", args, want)
	}
}

func TestBuildCommandArgsOptions(t *testing.T) {
	t.Parallel()
	maxTurns := 3
	budget := 1.5
	value := "v"
	dash := "-x"
	sources := []string{SettingSourceProject}
	cases := []struct {
		name string
		opts Options
		want []string // fragments that must appear in order
		not  []string
	}{
		{"systemPromptText", Options{SystemPrompt: SystemPromptText("be nice")},
			nil, []string{"--system-prompt", "be nice"}},
		{"systemPromptPresetAppend", Options{SystemPrompt: &SystemPromptPreset{Append: "extra"}},
			nil, []string{"--system-prompt", "--append-system-prompt", "extra"}},
		{"systemPromptFile", Options{SystemPrompt: &SystemPromptFile{Path: "/p.md"}},
			[]string{"--system-prompt-file", "/p.md"}, nil},
		{"toolsList", Options{Tools: ToolList{"Bash", "Read"}}, []string{"--tools", "Bash,Read"}, nil},
		{"toolsEmpty", Options{Tools: ToolList{}}, []string{"--tools", ""}, nil},
		{"toolsPreset", Options{Tools: ToolsPreset{}}, []string{"--tools", "default"}, nil},
		{"allowedTools", Options{AllowedTools: []string{"Bash", "Read"}}, []string{"--allowedTools", "Bash,Read"}, nil},
		{"disallowedTools", Options{DisallowedTools: []string{"Bash"}}, []string{"--disallowedTools", "Bash"}, nil},
		{"maxTurns", Options{MaxTurns: &maxTurns}, []string{"--max-turns", "3"}, nil},
		{"maxTurnsZero", Options{MaxTurns: new(0)}, nil, []string{"--max-turns"}},
		{"maxBudget", Options{MaxBudgetUSD: &budget}, []string{"--max-budget-usd", "1.5"}, nil},
		{"taskBudget", Options{TaskBudget: &TaskBudget{Total: 100}}, []string{"--task-budget", "100"}, nil},
		{"model", Options{Model: "opus", FallbackModel: "sonnet"},
			[]string{"--model", "opus", "--fallback-model", "sonnet"}, nil},
		{"betas", Options{Betas: []SDKBeta{SDKBetaContext1M}}, []string{"--betas", "context-1m-2025-08-07"}, nil},
		{"permission", Options{PermissionMode: PermissionModePlan, PermissionPromptToolName: "stdio"},
			[]string{"--permission-prompt-tool", "stdio", "--permission-mode", "plan"}, nil},
		{"continue", Options{ContinueConversation: true}, []string{"--continue"}, nil},
		// The equals form keeps a dash-leading value bound to its flag.
		{"resume", Options{Resume: "--evil"}, []string{"--resume=--evil"}, nil},
		{"sessionID", Options{SessionID: "sid"}, []string{"--session-id=sid"}, nil},
		{"resumeAt", Options{ResumeSessionAt: "u1", ResumeDropsTurn: "u2"},
			[]string{"--resume-session-at=u1", "--resume-drops-turn=u2"}, nil},
		{"sessionMirror", Options{SessionStore: &mirrorStoreFake{}}, []string{"--session-mirror"}, nil},
		{"noSessionMirror", Options{}, nil, []string{"--session-mirror"}},
		{"settings", Options{Settings: "/s.json"}, []string{"--settings", "/s.json"}, nil},
		{"addDirs", Options{AddDirs: []string{"/a", "/b"}}, []string{"--add-dir", "/a", "--add-dir", "/b"}, nil},
		{"flags", Options{IncludePartialMessages: true, IncludeHookEvents: true, StrictMCPConfig: true, ForkSession: true},
			[]string{"--include-partial-messages", "--include-hook-events", "--strict-mcp-config", "--fork-session"}, nil},
		{"settingSources", Options{SettingSources: &sources}, []string{"--setting-sources=project"}, nil},
		{"plugins", Options{Plugins: []PluginConfig{{Type: "local", Path: "/p"}}}, []string{"--plugin-dir", "/p"}, nil},
		{"extraArgsValue", Options{ExtraArgs: map[string]*string{"foo": &value}}, []string{"--foo", "v"}, nil},
		{"extraArgsBool", Options{ExtraArgs: map[string]*string{"bar": nil}}, []string{"--bar"}, nil},
		{"extraArgsDash", Options{ExtraArgs: map[string]*string{"baz": &dash}}, []string{"--baz=-x"}, nil},
		{"extraArgsLoneDash", Options{ExtraArgs: map[string]*string{"baz": new("-")}}, []string{"--baz", "-"}, nil},
		{"thinkingAdaptive", Options{Thinking: &ThinkingConfig{Type: "adaptive"}}, []string{"--thinking", "adaptive"}, nil},
		{"thinkingEnabled", Options{Thinking: &ThinkingConfig{Type: "enabled", BudgetTokens: &maxTurns}},
			[]string{"--max-thinking-tokens", "3"}, nil},
		{"thinkingDisabled", Options{Thinking: &ThinkingConfig{Type: "disabled"}}, []string{"--thinking", "disabled"}, nil},
		{"maxThinkingTokens", Options{MaxThinkingTokens: &maxTurns}, []string{"--max-thinking-tokens", "3"}, nil},
		{"maxThinkingTokensZero", Options{MaxThinkingTokens: new(0)}, []string{"--thinking", "disabled"}, nil},
		// enabled without a budget means adaptive in the TypeScript SDK.
		{"thinkingEnabledNoBudget", Options{Thinking: &ThinkingConfig{Type: ThinkingEnabled}},
			[]string{"--thinking", "adaptive"}, []string{"--max-thinking-tokens"}},
		{"thinkingDisplay", Options{Thinking: &ThinkingConfig{Type: ThinkingAdaptive, Display: ThinkingDisplayOmitted}},
			[]string{"--thinking", "adaptive", "--thinking-display", "omitted"}, nil},
		{"thinkingDisplayDisabled", Options{Thinking: &ThinkingConfig{Type: ThinkingDisabled, Display: ThinkingDisplaySummarized}},
			[]string{"--thinking", "disabled"}, []string{"--thinking-display"}},
		{"thinkingWinsOverMaxTokens", Options{Thinking: &ThinkingConfig{Type: ThinkingAdaptive}, MaxThinkingTokens: &maxTurns},
			[]string{"--thinking", "adaptive"}, []string{"--max-thinking-tokens"}},
		{"allowDangerouslySkip", Options{PermissionMode: PermissionModeBypassPermissions, AllowDangerouslySkipPermissions: true},
			[]string{"--permission-mode", "bypassPermissions", "--allow-dangerously-skip-permissions"}, nil},
		{"permissionPrompts", Options{PermissionPrompts: PermissionPromptsNone}, []string{"--permission-prompts", "none"}, nil},
		{"agent", Options{Agent: "reviewer"}, []string{"--agent", "reviewer"}, nil},
		{"debug", Options{Debug: true}, []string{"--debug"}, nil},
		{"debugFileWins", Options{Debug: true, DebugFile: "/d.log"}, []string{"--debug-file", "/d.log"}, []string{"--debug"}},
		{"noSessionPersistence", Options{NoSessionPersistence: true}, []string{"--no-session-persistence"}, nil},
		{"projectConfigRoot", Options{ProjectConfigRoot: "/repo"}, []string{"--project-config-root=/repo"}, nil},
		{"managedSettings", Options{ManagedSettings: Settings{"model": "opus"}},
			[]string{"--managed-settings", `{"model":"opus"}`}, nil},
		{"settingsObject", Options{Settings: Settings{"model": "opus"}}, []string{"--settings", `{"model":"opus"}`}, nil},
		{"settingsRaw", Options{Settings: json.RawMessage(`{"a":1}`)}, []string{"--settings", `{"a":1}`}, nil},
		{"settingsTypedNil", Options{Settings: json.RawMessage(nil)}, nil, []string{"--settings"}},
		{"pluginNoMCP", Options{Plugins: []PluginConfig{{Path: "/p", SkipMCPDiscovery: true}}},
			[]string{"--plugin-dir-no-mcp", "/p"}, []string{"--plugin-dir"}},
		{"pluginsViaInitialize", Options{Plugins: []PluginConfig{{Path: "/p"}}, PluginDelivery: PluginDeliveryInitialize},
			[]string{"--await-initialize"}, []string{"--plugin-dir", "/p"}},
		{"pluginDeliveryWithoutPlugins", Options{PluginDelivery: PluginDeliveryInitialize}, nil, []string{"--await-initialize"}},
		{"outputFormat", Options{OutputFormat: map[string]any{"type": "json_schema", "schema": map[string]any{"type": "object"}}},
			[]string{"--json-schema", `{"type":"object"}`}, nil},
		// Skills no longer force --setting-sources, as in the TypeScript SDK.
		{"skillsKeepSources", Options{Skills: SkillsAll{}}, []string{"--allowedTools", "Skill"}, []string{"--setting-sources=user,project"}},
		{"settingSourcesEmpty", Options{SettingSources: new([]string{})}, []string{"--setting-sources="}, nil},
		{"effort", Options{Effort: EffortHigh}, []string{"--effort", "high"}, nil},
		{"mcpConfigPath", Options{MCPConfigPath: "/mcp.json"}, []string{"--mcp-config", "/mcp.json"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args, err := buildCommandArgs(&tc.opts)
			if err != nil {
				t.Fatalf("buildCommandArgs: %v", err)
			}
			joined := strings.Join(args, "\x00")
			if len(tc.want) > 0 && !strings.Contains(joined, strings.Join(tc.want, "\x00")) {
				t.Fatalf("args %q missing %q", args, tc.want)
			}
			for _, absent := range tc.not {
				if slices.Contains(args, absent) {
					t.Fatalf("args %q should not contain %q", args, absent)
				}
			}
			// Streaming mode is always negotiated on both directions.
			if !strings.HasSuffix(joined, "--input-format\x00stream-json") {
				t.Fatalf("args %q should end with the input format", args)
			}
		})
	}
}

func TestBuildCommandArgsMCPServers(t *testing.T) {
	t.Parallel()
	args, err := buildCommandArgs(&Options{MCPServers: map[string]MCPServerConfig{
		"calc": &MCPSDKServerConfig{Name: "calc", Instance: &MCPServer{}},
		"fs":   &MCPStdioServerConfig{Command: "node", Args: []string{"fs.js"}},
	}})
	if err != nil {
		t.Fatalf("buildCommandArgs: %v", err)
	}
	i := slices.Index(args, "--mcp-config")
	if i < 0 || i+1 >= len(args) {
		t.Fatalf("missing --mcp-config in %q", args)
	}
	var payload struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(args[i+1]), &payload); err != nil {
		t.Fatalf("mcp config is not JSON: %v", err)
	}
	// In-process servers are declared in the initialize request, as the
	// TypeScript SDK does, never in --mcp-config.
	if sdk, ok := payload.MCPServers["calc"]; ok {
		t.Fatalf("sdk server reached --mcp-config: %#v", sdk)
	}
	if payload.MCPServers["fs"]["command"] != "node" {
		t.Fatalf("stdio server = %#v", payload.MCPServers["fs"])
	}

	// With only in-process servers there is no --mcp-config at all.
	args, err = buildCommandArgs(&Options{MCPServers: map[string]MCPServerConfig{
		"calc": &MCPSDKServerConfig{Name: "calc", Instance: &MCPServer{}},
	}})
	if err != nil {
		t.Fatalf("buildCommandArgs: %v", err)
	}
	if slices.Contains(args, "--mcp-config") {
		t.Fatalf("args %q should not carry --mcp-config", args)
	}
}

func TestBuildCommandArgsOutputFormatAndSandbox(t *testing.T) {
	t.Parallel()
	args, err := buildCommandArgs(&Options{
		OutputFormat: map[string]any{"type": "json_schema", "schema": map[string]any{"type": "object"}},
		Sandbox:      json.RawMessage(`{"enabled":true}`),
		Settings:     `{"model":"opus"}`,
	})
	if err != nil {
		t.Fatalf("buildCommandArgs: %v", err)
	}
	i := slices.Index(args, "--json-schema")
	if i < 0 || args[i+1] != `{"type":"object"}` {
		t.Fatalf("json schema flag missing in %q", args)
	}
	j := slices.Index(args, "--settings")
	if j < 0 {
		t.Fatalf("settings flag missing in %q", args)
	}
	var merged map[string]any
	if err := json.Unmarshal([]byte(args[j+1]), &merged); err != nil {
		t.Fatalf("settings is not JSON: %v", err)
	}
	if merged["model"] != "opus" {
		t.Fatalf("settings lost its original keys: %#v", merged)
	}
	sandbox, ok := merged["sandbox"].(map[string]any)
	if !ok || sandbox["enabled"] != true {
		t.Fatalf("sandbox not merged: %#v", merged)
	}
	// An enabled sandbox fails closed unless the caller says otherwise.
	if sandbox["failIfUnavailable"] != true {
		t.Fatalf("failIfUnavailable not defaulted: %#v", sandbox)
	}
}

func TestApplySkillsDefaults(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		opts Options
		want []string
	}{
		{"all", Options{Skills: SkillsAll{}}, []string{"Skill"}},
		{"allKeepsExisting", Options{Skills: SkillsAll{}, AllowedTools: []string{"Skill"}}, []string{"Skill"}},
		{"list", Options{Skills: SkillList{"pdf", "plugin:docx"}, AllowedTools: []string{"Read"}},
			[]string{"Read", "Skill(pdf)", "Skill(plugin:docx)"}},
		{"unset", Options{AllowedTools: []string{"Read"}}, []string{"Read"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			args, err := buildCommandArgs(&tc.opts)
			if err != nil {
				t.Fatalf("buildCommandArgs: %v", err)
			}
			i := slices.Index(args, "--allowedTools")
			if i < 0 {
				t.Fatalf("args = %q, want --allowedTools", args)
			}
			if allowed := strings.Split(args[i+1], ","); !slices.Equal(allowed, tc.want) {
				t.Fatalf("allowed = %q, want %q", allowed, tc.want)
			}
		})
	}
}

func TestValidateSkillName(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{"", "  ", " pdf", "pdf ", "*", "plugin:*", "/pdf", `a\\b`, `a\`, "a,b", "a(b)", "a\x00b", "a\xffb"} {
		if err := validateSkillName(bad); err == nil {
			t.Errorf("validateSkillName(%q) = nil, want error", bad)
		}
	}
	for _, good := range []string{"pdf", "plugin:docx", "my-skill_1"} {
		if err := validateSkillName(good); err != nil {
			t.Errorf("validateSkillName(%q) = %v", good, err)
		}
	}
}

func TestBuildEnv(t *testing.T) {
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("MOHAE_TEST_KEEP", "yes")
	env := buildEnv(&Options{
		Env:                     map[string]string{"CLAUDE_CODE_ENTRYPOINT": "custom", "EXTRA": "1"},
		EnableFileCheckpointing: true,
		Cwd:                     "/tmp",
	})
	got := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = v
	}
	if _, ok := got["CLAUDECODE"]; ok {
		t.Fatal("CLAUDECODE should be filtered out")
	}
	if got["MOHAE_TEST_KEEP"] != "yes" {
		t.Fatal("inherited env should be preserved")
	}
	// options.Env may override the entrypoint but never the SDK version.
	if got["CLAUDE_CODE_ENTRYPOINT"] != "custom" || got["EXTRA"] != "1" {
		t.Fatalf("env = %#v", got)
	}
	if got["CLAUDE_AGENT_SDK_VERSION"] != Version {
		t.Fatalf("version = %q", got["CLAUDE_AGENT_SDK_VERSION"])
	}
	if got["CLAUDE_CODE_ENABLE_SDK_FILE_CHECKPOINTING"] != "true" || got["PWD"] != "/tmp" {
		t.Fatalf("env = %#v", got)
	}
	// The entrypoint comes from Options.Env, where prepareOptions puts it;
	// buildEnv adds none of its own.
	t.Setenv("CLAUDE_CODE_ENTRYPOINT", "cli")
	if got := envMap(buildEnv(&Options{}))["CLAUDE_CODE_ENTRYPOINT"]; got != "cli" {
		t.Fatalf("entrypoint = %q, want the inherited value", got)
	}
	prepared, _, err := prepareOptions(&Options{}, entrypoint)
	if err != nil {
		t.Fatal(err)
	}
	if got := envMap(buildEnv(prepared))["CLAUDE_CODE_ENTRYPOINT"]; got != entrypoint {
		t.Fatalf("entrypoint = %q, want %q", got, entrypoint)
	}
}

// envMap renders buildEnv's output as a map.
func envMap(env []string) map[string]string {
	got := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = v
	}
	return got
}

func TestBuildEnvTypeScriptParity(t *testing.T) {
	t.Setenv("NODE_OPTIONS", "--inspect")
	t.Setenv("DEBUG", "express:*")
	t.Setenv("DEBUG_CLAUDE_AGENT_SDK", "")
	cases := []struct {
		name   string
		opts   Options
		want   map[string]string
		absent []string
	}{
		{"defaults", Options{},
			map[string]string{"CLAUDE_CODE_SDK_READS_SESSION_STATE": "1"},
			[]string{"NODE_OPTIONS", "DEBUG", "CLAUDE_CODE_QUESTION_PREVIEW_FORMAT"}},
		{"sessionStateCallerValueAnyCase", Options{Env: map[string]string{"claude_code_sdk_reads_session_state": "0"}},
			map[string]string{"claude_code_sdk_reads_session_state": "0"},
			[]string{"CLAUDE_CODE_SDK_READS_SESSION_STATE"}},
		{"nodeOptionsAlwaysRemoved", Options{Env: map[string]string{"NODE_OPTIONS": "--x"}}, nil, []string{"NODE_OPTIONS"}},
		{"sdkDebug", Options{Env: map[string]string{"DEBUG_CLAUDE_AGENT_SDK": "true"}},
			map[string]string{"DEBUG": "1"}, nil},
		{"sdkDebugOff", Options{Env: map[string]string{"DEBUG_CLAUDE_AGENT_SDK": "0", "DEBUG": "x"}}, nil, []string{"DEBUG"}},
		{"previewFormat", Options{ToolConfig: &ToolConfig{AskUserQuestion: &AskUserQuestionConfig{PreviewFormat: QuestionPreviewHTML}}},
			map[string]string{"CLAUDE_CODE_QUESTION_PREVIEW_FORMAT": "html"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := envMap(buildEnv(&tc.opts))
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("%s = %q, want %q", k, got[k], v)
				}
			}
			for _, k := range tc.absent {
				if _, ok := got[k]; ok {
					t.Errorf("%s should be absent, got %q", k, got[k])
				}
			}
		})
	}
}

func TestFindCLI(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "claude")
	if runtime.GOOS == "windows" {
		stub += ".exe"
	}
	if err := os.WriteFile(stub, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// PATH wins.
	t.Setenv("PATH", dir)
	got, err := findCLI()
	if err != nil {
		t.Fatalf("findCLI: %v", err)
	}
	if got != stub {
		t.Fatalf("findCLI = %q, want %q", got, stub)
	}

	// Fallback to a known install location.
	t.Setenv("PATH", filepath.Join(dir, "empty"))
	restore := cliCandidatesFn
	t.Cleanup(func() { cliCandidatesFn = restore })
	cliCandidatesFn = func() []string { return []string{filepath.Join(dir, "missing"), stub} }
	got, err = findCLI()
	if err != nil || got != stub {
		t.Fatalf("findCLI = %q, %v", got, err)
	}

	// Nothing anywhere.
	cliCandidatesFn = func() []string { return []string{filepath.Join(dir, "missing")} }
	if _, err := findCLI(); err == nil {
		t.Fatal("expected an error")
	} else {
		var notFound *CLINotFoundError
		if !errors.As(err, &notFound) {
			t.Fatalf("error = %T (%v)", err, err)
		}
		if !strings.Contains(notFound.Error(), "npm install -g @anthropic-ai/claude-code") {
			t.Fatalf("error should carry an install hint: %v", notFound)
		}
	}
}

// writeStub writes an executable /bin/sh script and returns its path.
func writeStub(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("stub CLI scripts need a POSIX shell")
	}
	path := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func collect(t *testing.T, tr Transport) ([]string, error) {
	t.Helper()
	var lines []string
	for raw, err := range tr.ReadMessages() {
		if err != nil {
			return lines, err
		}
		lines = append(lines, string(raw))
	}
	return lines, nil
}

func TestSubprocessTransportReadsNDJSON(t *testing.T) {
	stub := writeStub(t, `
echo '{"type":"system","subtype":"init"}'
echo ''
echo '[SandboxDebug] not json'
printf '{"type":"result","subtype":"success"}'
`)
	tr := newTestTransport(t, &Options{CLIPath: stub})
	if err := tr.Connect(t.Context()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer tr.Close()
	if !tr.Ready() {
		t.Fatal("transport should be ready after connect")
	}
	lines, err := collect(t, tr)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	want := []string{`{"type":"system","subtype":"init"}`, `{"type":"result","subtype":"success"}`}
	if !slices.Equal(lines, want) {
		t.Fatalf("lines = %q, want %q", lines, want)
	}
}

func TestSubprocessTransportWriteAndEndInput(t *testing.T) {
	// The stub echoes back whatever it is sent, so the round trip proves both
	// stdin framing and stdout reading.
	stub := writeStub(t, `while IFS= read -r line; do echo "$line"; done`)
	tr := newTestTransport(t, &Options{CLIPath: stub})
	if err := tr.Connect(t.Context()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer tr.Close()

	// A frame without a trailing newline gets one appended.
	if err := tr.Write(t.Context(), []byte(`{"a":1}`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := tr.Write(t.Context(), []byte("{\"b\":2}\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := tr.EndInput(); err != nil {
		t.Fatalf("end input: %v", err)
	}
	if tr.Ready() {
		t.Fatal("transport should not be ready after EndInput")
	}
	if err := tr.Write(t.Context(), []byte(`{"c":3}`)); err == nil {
		t.Fatal("write after EndInput should fail")
	}
	lines, err := collect(t, tr)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !slices.Equal(lines, []string{`{"a":1}`, `{"b":2}`}) {
		t.Fatalf("lines = %q", lines)
	}
}

func TestSubprocessTransportProcessError(t *testing.T) {
	stub := writeStub(t, `
echo '{"type":"system","subtype":"init"}'
echo 'boom happened' >&2
exit 3
`)
	var stderrLines []string
	tr := newTestTransport(t, &Options{
		CLIPath: stub,
		Stderr:  func(line string) { stderrLines = append(stderrLines, line) },
	})
	if err := tr.Connect(t.Context()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer tr.Close()
	lines, err := collect(t, tr)
	if len(lines) != 1 {
		t.Fatalf("lines = %q", lines)
	}
	var perr *ProcessError
	if !errors.As(err, &perr) {
		t.Fatalf("error = %T (%v), want *ProcessError", err, err)
	}
	if perr.ExitCode == nil || *perr.ExitCode != 3 {
		t.Fatalf("exit code = %v", perr.ExitCode)
	}
	if !strings.Contains(perr.Stderr, "boom happened") {
		t.Fatalf("stderr = %q", perr.Stderr)
	}
	if !slices.Contains(stderrLines, "boom happened") {
		t.Fatalf("stderr callback saw %q", stderrLines)
	}
}

func TestSubprocessTransportOversizedLine(t *testing.T) {
	stub := writeStub(t, `
awk 'BEGIN { printf "{\"a\":\""; for (i = 0; i < 200; i++) printf "0123456789"; print "\"}" }'
`)
	tr := newTestTransport(t, &Options{CLIPath: stub, MaxBufferSize: 64})
	if err := tr.Connect(t.Context()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer tr.Close()
	_, err := collect(t, tr)
	var de *JSONDecodeError
	if !errors.As(err, &de) {
		t.Fatalf("error = %T (%v), want *JSONDecodeError", err, err)
	}
	if !strings.Contains(err.Error(), "maximum buffer size") {
		t.Fatalf("error = %v", err)
	}
}

func TestSubprocessTransportSkipsInvalidJSON(t *testing.T) {
	stub := writeStub(t, `
echo '{"type": broken}'
echo '{"type":"result","subtype":"success"}'
`)
	tr := newTestTransport(t, &Options{CLIPath: stub})
	if err := tr.Connect(t.Context()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer tr.Close()
	lines, err := collect(t, tr)
	if err != nil {
		t.Fatalf("error = %v, want the invalid line skipped", err)
	}
	if len(lines) != 1 || lines[0] != `{"type":"result","subtype":"success"}` {
		t.Fatalf("lines = %q", lines)
	}
}

func TestSubprocessTransportCloseTerminates(t *testing.T) {
	stub := writeStub(t, `
echo '{"type":"system","subtype":"init"}'
exec sleep 30
`)
	tr := newTestTransport(t, &Options{CLIPath: stub})
	tr.gracefulTimeout = 50 * time.Millisecond
	if err := tr.Connect(t.Context()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range tr.ReadMessages() {
			// Drain until the process is killed.
		}
	}()
	if err := tr.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Close is idempotent.
	if err := tr.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("reader did not stop after Close")
	}
	if tr.Ready() {
		t.Fatal("transport should not be ready after Close")
	}
}

func TestSubprocessTransportContextCancelKills(t *testing.T) {
	stub := writeStub(t, `exec sleep 30`)
	ctx, cancel := context.WithCancel(t.Context())
	tr := newTestTransport(t, &Options{CLIPath: stub})
	tr.gracefulTimeout = 50 * time.Millisecond
	if err := tr.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer tr.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range tr.ReadMessages() {
		}
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("cancelling the context did not stop the process")
	}
}

func TestSubprocessTransportConnectErrors(t *testing.T) {
	t.Parallel()
	// A missing working directory is reported before the process starts.
	tr := newTestTransport(t, &Options{CLIPath: "/bin/echo", Cwd: filepath.Join(t.TempDir(), "nope")})
	err := tr.Connect(t.Context())
	var connErr *ConnectionError
	if !errors.As(err, &connErr) || !strings.Contains(err.Error(), "Working directory") {
		t.Fatalf("error = %v", err)
	}

	// Options.User is refused rather than silently ignored.
	tr = newTestTransport(t, &Options{CLIPath: "/bin/echo", User: "nobody"})
	if err := tr.Connect(t.Context()); err == nil || !strings.Contains(err.Error(), "Options.User") {
		t.Fatalf("error = %v", err)
	}

	// An option that cannot be rendered is reported before any transport
	// is built, not swallowed.
	if _, err := resolveLaunch(&Options{CLIPath: "/bin/echo", Settings: []string{"x"}}); err == nil ||
		!strings.Contains(err.Error(), "JSON object") {
		t.Fatalf("error = %v, want a settings encoding error", err)
	}
}

func TestSubprocessTransportReadBeforeConnect(t *testing.T) {
	t.Parallel()
	tr := newTestTransport(t, &Options{})
	_, err := collect(t, tr)
	var connErr *ConnectionError
	if !errors.As(err, &connErr) {
		t.Fatalf("error = %T (%v)", err, err)
	}
}
