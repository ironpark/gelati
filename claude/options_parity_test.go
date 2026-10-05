package claude

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ironpark/gelati/internal/jsonx"
)

func TestInitializeExtras(t *testing.T) {
	t.Parallel()
	schema := map[string]any{"type": "object"}
	cases := []struct {
		name string
		opts *Options
		want string
	}{
		{"nilOptions", nil, `{"systemPrompt":[""]}`},
		{"defaultPromptIsEmpty", &Options{}, `{"systemPrompt":[""]}`},
		{"text", &Options{SystemPrompt: SystemPromptText("be nice")}, `{"systemPrompt":["be nice"]}`},
		{"blocks", &Options{SystemPrompt: SystemPromptBlocks{"static", SystemPromptDynamicBoundary, "dynamic"}},
			`{"systemPrompt":["static","__SYSTEM_PROMPT_DYNAMIC_BOUNDARY__","dynamic"]}`},
		{"emptyBlocks", &Options{SystemPrompt: SystemPromptBlocks(nil)}, `{"systemPrompt":[]}`},
		{"custom", &Options{SystemPrompt: &SystemPromptCustom{Prompt: []string{"bot"}, Snapshot: new(false)}},
			`{"systemPrompt":["bot"],"systemPromptSnapshot":false}`},
		{"customNoSnapshot", &Options{SystemPrompt: &SystemPromptCustom{Prompt: []string{"bot"}}},
			`{"systemPrompt":["bot"]}`},
		{"preset", &Options{SystemPrompt: &SystemPromptPreset{}}, `{}`},
		{"presetFull", &Options{SystemPrompt: &SystemPromptPreset{Append: "more", ExcludeDynamicSections: true, Snapshot: new(true)}},
			`{"appendSystemPrompt":"more","excludeDynamicSections":true,"systemPromptSnapshot":true}`},
		{"fileTravelsAsFlag", &Options{SystemPrompt: &SystemPromptFile{Path: "/p.md"}}, `{}`},
		{"jsonSchema", &Options{SystemPrompt: &SystemPromptPreset{},
			OutputFormat: map[string]any{"type": "json_schema", "schema": schema}},
			`{"jsonSchema":{"type":"object"}}`},
		{"otherOutputFormat", &Options{SystemPrompt: &SystemPromptPreset{},
			OutputFormat: map[string]any{"type": "text", "schema": schema}}, `{}`},
		{"uxFields", &Options{
			SystemPrompt:           &SystemPromptPreset{},
			PlanModeInstructions:   "plan it",
			ToolAliases:            map[string]string{"Bash": "mcp__ws__bash"},
			Title:                  "Release",
			PromptSuggestions:      true,
			AgentProgressSummaries: true,
			PerTaskStopAffordance:  true,
		}, `{"agentProgressSummaries":true,"perTaskStopAffordance":true,"planModeInstructions":"plan it",` +
			`"promptSuggestions":true,"title":"Release","toolAliases":{"Bash":"mcp__ws__bash"}}`},
		{"pluginsViaInitialize", &Options{
			SystemPrompt:   &SystemPromptPreset{},
			PluginDelivery: PluginDeliveryInitialize,
			Plugins:        []PluginConfig{{Path: "/a"}, {Type: "local", Path: "/b", SkipMCPDiscovery: true}},
		}, `{"plugins":[{"path":"/a","type":"local"},{"path":"/b","skipMcpDiscovery":true,"type":"local"}]}`},
		{"pluginsViaArgv", &Options{SystemPrompt: &SystemPromptPreset{}, Plugins: []PluginConfig{{Path: "/a"}}}, `{}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := jsonx.Marshal(initializeExtras(t, tc.opts))
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("initializeExtras = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestOptionsValidation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		opts    Options
		wantErr string
	}{
		{"fallbackEqualsModel", Options{Model: "opus", FallbackModel: "opus"}, "FallbackModel"},
		{"fallbackDiffers", Options{Model: "opus", FallbackModel: "sonnet"}, ""},
		{"fallbackOnly", Options{FallbackModel: "sonnet"}, ""},
		{"noPersistenceWithStore", Options{NoSessionPersistence: true, SessionStore: &mirrorStoreFake{}}, "NoSessionPersistence"},
		{"badPluginDelivery", Options{PluginDelivery: "stdin"}, "PluginDelivery"},
		{"badPluginType", Options{Plugins: []PluginConfig{{Type: "remote", Path: "/p"}}}, "unsupported plugin type"},
		{"badPluginTypeViaInitialize", Options{PluginDelivery: PluginDeliveryInitialize,
			Plugins: []PluginConfig{{Type: "remote", Path: "/p"}}}, "unsupported plugin type"},
		{"settingsPathWithSandbox", Options{Settings: "/s.json", Sandbox: map[string]any{"enabled": true}}, "settings file path"},
		{"sandboxNotObject", Options{Sandbox: jsontext.Value(`[1]`)}, "JSON object"},
		{"settingsNotObject", Options{Settings: []string{"x"}}, "JSON object"},
		{"badSkillName", Options{Skills: SkillList{"bad,name"}}, "invalid skill name"},
		{"canUseToolWithPromptTool", Options{PermissionPromptToolName: "mcp__x",
			CanUseTool: func(context.Context, string, map[string]any, ToolPermissionContext) (PermissionResult, error) {
				return nil, nil
			}}, "PermissionPromptToolName"},
		{"dialogKindsWithoutHandler", Options{SupportedDialogKinds: []string{"k"}}, "OnUserDialog"},
		{"customTransportValidated", Options{Transport: newFakeTransport(), Skills: SkillList{"*"}}, "SkillsAll"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := prepareOptions(&tc.opts, entrypoint)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want one mentioning %q", err, tc.wantErr)
			}
		})
	}
}

func TestBuildSettingsValue(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		opts Options
		want string
	}{
		{"none", Options{}, ""},
		{"pathOnly", Options{Settings: "/s.json"}, "/s.json"},
		{"inlineOnly", Options{Settings: `{"a":1}`}, `{"a":1}`},
		{"mapOnly", Options{Settings: map[string]any{"a": 1}}, `{"a":1}`},
		{"typedNilSandbox", Options{Settings: "/s.json", Sandbox: (*SandboxSettings)(nil)}, "/s.json"},
		{"enabledDefaultsFailClosed", Options{Sandbox: &SandboxSettings{Enabled: new(true)}},
			`{"sandbox":{"enabled":true,"failIfUnavailable":true}}`},
		{"explicitFailOpenKept", Options{Sandbox: &SandboxSettings{Enabled: new(true), FailIfUnavailable: new(false)}},
			`{"sandbox":{"enabled":true,"failIfUnavailable":false}}`},
		{"disabledNotDefaulted", Options{Sandbox: SandboxSettings{Enabled: new(false)}},
			`{"sandbox":{"enabled":false}}`},
		{"rawSandboxMergedIntoInline", Options{Settings: `{"model":"opus"}`, Sandbox: jsontext.Value(`{"enabled":true}`)},
			`{"model":"opus","sandbox":{"enabled":true,"failIfUnavailable":true}}`},
		{"sandboxReplacesSettingsSandbox", Options{Settings: Settings{"sandbox": map[string]any{"x": 1}},
			Sandbox: map[string]any{"autoAllowBashIfSandboxed": true}},
			`{"sandbox":{"autoAllowBashIfSandboxed":true}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := buildSettingsValue(&tc.opts)
			if err != nil {
				t.Fatalf("buildSettingsValue: %v", err)
			}
			if got != tc.want {
				t.Fatalf("settings = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestSandboxSettingsMarshalJSON(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   SandboxSettings
		want string
	}{
		{"empty", SandboxSettings{}, `{}`},
		{"typed", SandboxSettings{
			Enabled:    new(true),
			Network:    &SandboxNetworkSettings{AllowedDomains: []string{"example.com"}, HTTPProxyPort: new(8080)},
			Filesystem: &SandboxFilesystemSettings{DenyRead: []string{"~/.ssh"}},
			Ripgrep:    &SandboxRipgrepConfig{Command: "rg"},
		}, `{"enabled":true,"network":{"allowedDomains":["example.com"],"httpProxyPort":8080},` +
			`"filesystem":{"denyRead":["~/.ssh"]},"ripgrep":{"command":"rg"}}`},
		{"extraMergedTypedWins", SandboxSettings{Enabled: new(true), Extra: map[string]any{"enabled": false, "future": "x"}},
			`{"enabled":true,"future":"x"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := jsonx.Marshal(tc.in)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("json = %s, want %s", got, tc.want)
			}
			// The pointer form encodes identically.
			ptr, _ := jsonx.Marshal(&tc.in)
			if string(ptr) != tc.want {
				t.Fatalf("pointer json = %s, want %s", ptr, tc.want)
			}
		})
	}
}

func TestResolveCommand(t *testing.T) {
	t.Parallel()
	flags := []string{"--verbose"}
	cases := []struct {
		name     string
		cliPath  string
		opts     Options
		wantCmd  string
		wantArgs []string
	}{
		{"native", "/bin/claude", Options{}, "/bin/claude", []string{"--verbose"}},
		{"nativeWithArgs", "/bin/claude", Options{ExecutableArgs: []string{"--x"}}, "/bin/claude", []string{"--x", "--verbose"}},
		{"jsDefaultsToNode", "/lib/cli.js", Options{}, "node", []string{"/lib/cli.js", "--verbose"}},
		{"mjsUnderBun", "/lib/cli.mjs", Options{Executable: "bun", ExecutableArgs: []string{"--smol"}},
			"bun", []string{"--smol", "/lib/cli.mjs", "--verbose"}},
		{"tsx", "cli.tsx", Options{Executable: "deno"}, "deno", []string{"cli.tsx", "--verbose"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cmd, args := resolveCommand(tc.cliPath, &tc.opts, flags)
			if cmd != tc.wantCmd || !slices.Equal(args, tc.wantArgs) {
				t.Fatalf("got %q %q, want %q %q", cmd, args, tc.wantCmd, tc.wantArgs)
			}
		})
	}
}

func TestSubprocessTransportSpawnHook(t *testing.T) {
	t.Parallel()
	stub := writeStub(t, `echo '{"type":"system","subtype":"init"}'`)
	var got SpawnOptions
	tr := newTestTransport(t, &Options{
		CLIPath: stub,
		// A custom spawner's cwd lives in its own environment, so it is
		// not checked locally.
		Cwd: filepath.Join(t.TempDir(), "remote-only"),
		Spawn: func(ctx context.Context, opts SpawnOptions) (SpawnedProcess, error) {
			got = opts
			local := opts
			local.Cwd = ""
			return SpawnLocalProcess(ctx, local)
		},
	})
	if err := tr.Connect(t.Context()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer tr.Close()
	lines, err := collect(t, tr)
	if err != nil || len(lines) != 1 {
		t.Fatalf("lines = %q, err = %v", lines, err)
	}
	if got.Command != stub || !slices.Contains(got.Args, "--input-format") || !strings.HasSuffix(got.Cwd, "remote-only") {
		t.Fatalf("spawn options = %+v", got)
	}
	if !slices.Contains(got.Env, "CLAUDE_CODE_SDK_READS_SESSION_STATE=1") {
		t.Fatalf("spawn env lacks the SDK variables: %q", got.Env)
	}
}

func TestSubprocessTransportSpawnErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		cliPath  string
		spawnErr error
		check    func(t *testing.T, err error, cmd string)
	}{
		{"defaultCommandWithoutDiscovery", "", errors.New("boom"), func(t *testing.T, err error, cmd string) {
			var connErr *ConnectionError
			if cmd != "claude" || !errors.As(err, &connErr) || !strings.Contains(err.Error(), "boom") {
				t.Fatalf("cmd = %q, err = %v", cmd, err)
			}
		}},
		{"notFound", "/x/claude", exec.ErrNotFound, func(t *testing.T, err error, cmd string) {
			var notFound *CLINotFoundError
			if !errors.As(err, &notFound) {
				t.Fatalf("err = %T %v", err, err)
			}
		}},
		{"nilProcess", "/x/claude", nil, func(t *testing.T, err error, cmd string) {
			if err == nil || !strings.Contains(err.Error(), "no process") {
				t.Fatalf("err = %v", err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var cmd string
			tr := newTestTransport(t, &Options{
				CLIPath: tc.cliPath,
				Spawn: func(_ context.Context, opts SpawnOptions) (SpawnedProcess, error) {
					cmd = opts.Command
					return nil, tc.spawnErr
				},
			})
			err := tr.Connect(t.Context())
			tc.check(t, err, cmd)
		})
	}
}

// startForClose connects a stub and waits until it printed its first line, so
// signal traps are installed before the test closes it.
func startForClose(t *testing.T, body string, grace, kill time.Duration, stderr func(string)) *subprocessTransport {
	t.Helper()
	tr := newTestTransport(t, &Options{CLIPath: writeStub(t, body), Stderr: stderr})
	tr.gracefulTimeout = grace
	tr.killTimeout = kill
	if err := tr.Connect(t.Context()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	first := make(chan struct{})
	go func() {
		once := sync.OnceFunc(func() { close(first) })
		for range tr.ReadMessages() {
			once()
		}
		once()
	}()
	<-first
	return tr
}

func TestSubprocessTransportGracefulClose(t *testing.T) {
	t.Parallel()
	ready := `echo '{"ready":true}'` + "\n"
	cases := []struct {
		name       string
		body       string
		grace      time.Duration
		kill       time.Duration
		maxElapsed time.Duration
		minElapsed time.Duration
		stderrLine string
	}{
		// The CLI exits on stdin EOF well inside the grace period.
		{"exitsOnEOF", ready + "cat >/dev/null\necho bye >&2\n", 10 * time.Second, 10 * time.Second,
			5 * time.Second, 0, "bye"},
		// Still running after the grace period: SIGTERM.
		{"sigterm", ready + "trap 'echo got-term >&2; exit 7' TERM\nwhile :; do sleep 0.05; done\n",
			100 * time.Millisecond, 10 * time.Second, 5 * time.Second, 100 * time.Millisecond, "got-term"},
		// SIGTERM ignored: killed after the force-kill timeout.
		{"sigkill", ready + "trap '' TERM\nexec sleep 30\n",
			50 * time.Millisecond, 200 * time.Millisecond, 5 * time.Second, 250 * time.Millisecond, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var mu sync.Mutex
			var lines []string
			tr := startForClose(t, tc.body, tc.grace, tc.kill, func(line string) {
				mu.Lock()
				defer mu.Unlock()
				lines = append(lines, line)
			})
			start := time.Now()
			if err := tr.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}
			elapsed := time.Since(start)
			if elapsed > tc.maxElapsed || elapsed < tc.minElapsed {
				t.Fatalf("close took %v, want between %v and %v", elapsed, tc.minElapsed, tc.maxElapsed)
			}
			mu.Lock()
			defer mu.Unlock()
			if tc.stderrLine != "" && !slices.Contains(lines, tc.stderrLine) {
				t.Fatalf("stderr = %q, want %q", lines, tc.stderrLine)
			}
		})
	}
}

func TestSystemPromptFileStaysAFlag(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "prompt.md")
	if err := os.WriteFile(path, []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := &Options{SystemPrompt: &SystemPromptFile{Path: path}}
	args, err := buildCommandArgs(opts)
	if err != nil {
		t.Fatal(err)
	}
	if i := slices.Index(args, "--system-prompt-file"); i < 0 || args[i+1] != path {
		t.Fatalf("args = %q", args)
	}
	if _, ok := initializeExtras(t, opts)["systemPrompt"]; ok {
		t.Fatal("a file prompt must not also be sent in initialize")
	}
}
