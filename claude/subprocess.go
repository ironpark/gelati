package claude

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"
)

// Version is the SDK version reported to the CLI in CLAUDE_AGENT_SDK_VERSION.
const Version = "0.1.0"

// entrypoint and entrypointClient are reported to the CLI in
// CLAUDE_CODE_ENTRYPOINT, distinguishing one-shot queries from interactive
// client sessions.
const (
	entrypoint       = "sdk-go"
	entrypointClient = "sdk-go-client"
)

// stderrTailLimit caps how much stderr is retained for error reports.
const stderrTailLimit = 8 * 1024

// Graceful shutdown timings, matching the TypeScript SDK: after stdin is
// closed the CLI gets defaultGracefulExitTimeout to exit on its own, then
// SIGTERM, then defaultForceKillTimeout before it is killed.
const (
	defaultGracefulExitTimeout = 2 * time.Second
	defaultForceKillTimeout    = 5 * time.Second
)

// subprocessTransport runs the Claude Code CLI as a child process and speaks
// stream-json over its stdin and stdout.
type subprocessTransport struct {
	opts *Options

	// writeMu serializes frames on stdin. It is separate from mu so that a
	// Write blocked on a full pipe never holds mu: Close and EndInput can
	// still close stdin, which unblocks it.
	writeMu sync.Mutex

	mu      sync.Mutex
	proc    SpawnedProcess
	stdin   io.WriteCloser
	stdout  io.Reader
	ready   bool
	exitErr error
	closed  bool

	cliPath string

	// closeStdin closes the process's stdin exactly once; it is safe to
	// call without holding mu.
	closeStdin func() error

	waitOnce sync.Once
	waitErr  error
	exited   chan struct{}

	stderrMu   sync.Mutex
	stderrTail []byte
	stderrDone chan struct{}

	termOnce        sync.Once
	gracefulTimeout time.Duration
	killTimeout     time.Duration

	cancel context.CancelFunc
}

// newSubprocessTransport builds a transport for the given options. The CLI is
// located and the command line built at Connect time.
func newSubprocessTransport(opts *Options) *subprocessTransport {
	if opts == nil {
		opts = &Options{}
	}
	return &subprocessTransport{
		opts:            opts,
		gracefulTimeout: defaultGracefulExitTimeout,
		killTimeout:     defaultForceKillTimeout,
	}
}

// Connect locates the CLI, builds its command line and starts it.
func (t *subprocessTransport) Connect(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.proc != nil {
		return nil
	}
	if t.opts.User != "" {
		return NewConnectionError(
			"Options.User is not supported by the subprocess transport; " +
				"run the process as the desired user instead")
	}

	spawn := t.opts.Spawn
	cliPath := t.opts.CLIPath
	if cliPath == "" {
		if spawn != nil {
			// A custom spawner runs the CLI somewhere else; discovery on
			// this machine says nothing about that environment.
			cliPath = "claude"
		} else {
			found, err := findCLI()
			if err != nil {
				return err
			}
			cliPath = found
		}
	}
	t.cliPath = cliPath

	args, err := buildCommandArgs(t.opts)
	if err != nil {
		return err
	}

	if t.opts.Cwd != "" && spawn == nil {
		if info, err := os.Stat(t.opts.Cwd); err != nil || !info.IsDir() {
			return NewConnectionError("Working directory does not exist: " + t.opts.Cwd)
		}
	}
	if spawn == nil {
		spawn = SpawnLocalProcess
	}

	// The process is bound to a cancellable child of ctx. Both an explicit
	// Close and a cancelled ctx run the graceful shutdown, which cancels
	// runCtx last, so a spawner may tie forced teardown to it.
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	t.cancel = cancel

	command, argv := resolveCommand(cliPath, t.opts, args)
	proc, err := spawn(runCtx, SpawnOptions{
		Command: command,
		Args:    argv,
		Cwd:     t.opts.Cwd,
		Env:     buildEnv(t.opts),
	})
	if err == nil && proc == nil {
		err = errors.New("Options.Spawn returned no process")
	}
	if err != nil {
		cancel()
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			return NewCLINotFoundError("Claude Code not found at", cliPath)
		}
		return NewConnectionError("Failed to start Claude Code: " + err.Error())
	}

	stdin := proc.Stdin()
	t.proc = proc
	t.stdin = stdin
	t.stdout = proc.Stdout()
	t.closeStdin = sync.OnceValue(stdin.Close)
	t.exited = make(chan struct{})
	t.ready = true
	t.stderrDone = make(chan struct{})
	if stderr := proc.Stderr(); stderr != nil {
		go t.pumpStderr(stderr)
	} else {
		close(t.stderrDone)
	}
	go func() {
		select {
		case <-ctx.Done():
			t.terminate()
		case <-runCtx.Done():
		}
	}()
	return nil
}

// pumpStderr forwards the CLI's stderr to the callback line by line and keeps a
// tail for inclusion in process errors.
func (t *subprocessTransport) pumpStderr(r io.Reader) {
	defer close(t.stderrDone)
	scanner := bufio.NewScanner(r)
	// The initial capacity must not exceed the limit: the scanner accepts
	// lines up to the larger of the two.
	maxSize := t.opts.bufferSize()
	scanner.Buffer(make([]byte, 0, min(64*1024, maxSize)), maxSize)
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r\n \t")
		if line == "" {
			continue
		}
		t.appendStderr(line)
		if t.opts.Stderr != nil {
			// Isolated per line: a panicking callback must not stop the
			// pump and silently drop every later line.
			func() {
				defer func() { _ = recover() }()
				t.opts.Stderr(line)
			}()
		}
	}
	// Drain the rest after an oversized line, so the child never blocks on
	// a full stderr pipe.
	_, _ = io.Copy(io.Discard, r)
}

// wait reaps the child process exactly once and reports its status. It first
// drains the stderr pump, since exec closes the pipes when Wait returns.
func (t *subprocessTransport) wait() error {
	t.waitOnce.Do(func() {
		if t.stderrDone != nil {
			<-t.stderrDone
		}
		t.waitErr = t.proc.Wait()
		close(t.exited)
	})
	return t.waitErr
}

// terminate shuts the process down once, in the background, the way the
// TypeScript SDK does: close stdin, give the CLI a grace period to exit,
// send SIGTERM, and kill it if it is still running after killTimeout. On
// Windows, where SIGTERM cannot be delivered, the process is killed after
// both periods.
func (t *subprocessTransport) terminate() {
	t.termOnce.Do(func() {
		go func() {
			defer t.cancel()
			// Closing stdin first also unblocks a Write stuck on a full
			// pipe.
			_ = t.closeStdin()
			t.mu.Lock()
			t.ready = false
			t.mu.Unlock()
			go func() { _ = t.wait() }()
			if waitClosed(t.exited, t.gracefulTimeout) {
				return
			}
			_ = t.proc.Signal(syscall.SIGTERM)
			if waitClosed(t.exited, t.killTimeout) {
				return
			}
			_ = t.proc.Kill()
		}()
	})
}

// waitClosed reports whether ch closes within d.
func waitClosed(ch <-chan struct{}, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ch:
		return true
	case <-timer.C:
		return false
	}
}

func (t *subprocessTransport) appendStderr(line string) {
	t.stderrMu.Lock()
	defer t.stderrMu.Unlock()
	t.stderrTail = append(t.stderrTail, line...)
	t.stderrTail = append(t.stderrTail, '\n')
	if len(t.stderrTail) > stderrTailLimit {
		t.stderrTail = t.stderrTail[len(t.stderrTail)-stderrTailLimit:]
	}
}

func (t *subprocessTransport) stderrSnapshot() string {
	t.stderrMu.Lock()
	defer t.stderrMu.Unlock()
	return strings.TrimSpace(string(t.stderrTail))
}

// Write sends one frame to the CLI's stdin. mu is not held while writing,
// so a write stuck on a full pipe cannot block Close or EndInput.
func (t *subprocessTransport) Write(ctx context.Context, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	t.mu.Lock()
	stdin, ready, exitErr := t.stdin, t.ready, t.exitErr
	t.mu.Unlock()
	if !ready || stdin == nil {
		return NewConnectionError("transport is not ready for writing")
	}
	if exitErr != nil {
		return NewConnectionError("Cannot write to process that exited with error: " + exitErr.Error())
	}
	if len(data) == 0 || data[len(data)-1] != '\n' {
		// One write per frame, newline included.
		data = append(slices.Clip(data), '\n')
	}
	if _, err := stdin.Write(data); err != nil {
		t.mu.Lock()
		t.ready = false
		if t.exitErr == nil {
			t.exitErr = err
		}
		t.mu.Unlock()
		return NewConnectionError("Failed to write to process stdin: " + err.Error())
	}
	return nil
}

// EndInput closes the CLI's stdin.
func (t *subprocessTransport) EndInput() error {
	t.mu.Lock()
	if t.stdin == nil {
		t.mu.Unlock()
		return nil
	}
	t.stdin = nil
	t.ready = false
	t.mu.Unlock()
	if err := t.closeStdin(); err != nil && !errors.Is(err, os.ErrClosed) {
		return err
	}
	return nil
}

// Ready reports whether the CLI is running and accepting writes.
func (t *subprocessTransport) Ready() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.ready
}

// Close terminates the CLI, gracefully first (see terminate), and releases
// its resources. It returns once the process has exited.
func (t *subprocessTransport) Close() error {
	t.mu.Lock()
	if t.closed || t.proc == nil {
		t.closed = true
		t.ready = false
		cancel := t.cancel
		t.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		return nil
	}
	t.closed = true
	t.ready = false
	t.stdin = nil
	t.mu.Unlock()

	t.terminate()
	err := t.wait()
	if _, ok := errors.AsType[exitCoder](err); ok {
		// A non-zero status after an explicit Close is expected.
		return nil
	}
	return err
}

// ReadMessages yields the CLI's newline-delimited JSON output.
func (t *subprocessTransport) ReadMessages() iter.Seq2[json.RawMessage, error] {
	return func(yield func(json.RawMessage, error) bool) {
		t.mu.Lock()
		stdout, proc := t.stdout, t.proc
		t.mu.Unlock()
		if stdout == nil || proc == nil {
			yield(nil, NewConnectionError("not connected"))
			return
		}

		maxSize := t.opts.bufferSize()
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, min(64*1024, maxSize)), maxSize)
		for scanner.Scan() {
			// The scanner reuses its buffer, so the line is copied once,
			// after it is known to be a frame.
			line := bytes.TrimSpace(scanner.Bytes())
			if len(line) == 0 || line[0] != '{' {
				// Some CLI builds write diagnostics such as
				// "[SandboxDebug] ..." to stdout; they carry no message.
				continue
			}
			if !json.Valid(line) {
				// Like the TypeScript SDK, a line that is not valid JSON is
				// skipped rather than ending the session.
				continue
			}
			if !yield(json.RawMessage(bytes.Clone(line)), nil) {
				return
			}
		}
		if err := scanner.Err(); err != nil {
			if errors.Is(err, bufio.ErrTooLong) {
				msg := fmt.Sprintf(
					"JSON message exceeded maximum buffer size of %d bytes", maxSize)
				yield(nil, &JSONDecodeError{baseError{Msg: msg}, "", err})
				return
			}
			if !errors.Is(err, os.ErrClosed) {
				yield(nil, NewConnectionError("Failed to read from process stdout: "+err.Error()))
				return
			}
		}

		// Output is exhausted: reap the process and report a failure exit.
		waitErr := t.wait()
		t.mu.Lock()
		t.ready = false
		t.mu.Unlock()
		if exitErr, ok := errors.AsType[exitCoder](waitErr); ok {
			code := exitErr.ExitCode()
			perr := NewProcessError(
				fmt.Sprintf("Command failed with exit code %d", code), &code, t.stderrSnapshot())
			t.mu.Lock()
			t.exitErr = perr
			t.mu.Unlock()
			yield(nil, perr)
		}
	}
}

// ---------------------------------------------------------------------------
// CLI discovery
// ---------------------------------------------------------------------------

// findCLI locates the claude executable on PATH or in the usual install
// locations.
func findCLI() (string, error) {
	name := "claude"
	if runtime.GOOS == "windows" {
		name = "claude.exe"
	}
	if path, err := exec.LookPath(name); err == nil {
		return path, nil
	}
	for _, c := range cliCandidatesFn() {
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			return c, nil
		}
	}
	return "", NewCLINotFoundError(
		"Claude Code not found. Install with:\n"+
			"  npm install -g @anthropic-ai/claude-code\n"+
			"\nIf already installed locally, try:\n"+
			"  export PATH=\"$HOME/node_modules/.bin:$PATH\"\n"+
			"\nOr provide the path via Options.CLIPath", "")
}

// cliCandidatesFn is the candidate list used by findCLI; tests replace it.
var cliCandidatesFn = cliCandidates

// cliCandidates lists the usual install locations for the CLI, in the order
// they are probed.
func cliCandidates() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	if runtime.GOOS == "windows" {
		if home == "" {
			return nil
		}
		return []string{filepath.Join(home, ".local", "bin", "claude.exe")}
	}
	candidates := []string{"/usr/local/bin/claude"}
	if home == "" {
		return candidates
	}
	return append(candidates,
		filepath.Join(home, ".npm-global", "bin", "claude"),
		filepath.Join(home, ".local", "bin", "claude"),
		filepath.Join(home, "node_modules", ".bin", "claude"),
		filepath.Join(home, ".yarn", "bin", "claude"),
		filepath.Join(home, ".claude", "local", "claude"),
	)
}

// ---------------------------------------------------------------------------
// Command line construction
// ---------------------------------------------------------------------------

// buildCommandArgs renders the CLI arguments for opts. The SDK always drives
// the CLI in streaming-json mode on both directions, matching the reference
// SDKs, so large configuration (agents, hooks) can ride on the initialize
// control request instead of the command line.
//
// opts must have passed prepareOptions, which validates option combinations.
func buildCommandArgs(opts *Options) ([]string, error) {
	args := []string{"--output-format", "stream-json", "--verbose"}

	// Every other system prompt form travels in the initialize request
	// (see initializeExtras), as in the TypeScript SDK.
	if sp, ok := opts.SystemPrompt.(*SystemPromptFile); ok {
		args = append(args, "--system-prompt-file", sp.Path)
	}

	switch tools := opts.Tools.(type) {
	case nil:
	case ToolList:
		args = append(args, "--tools", strings.Join(tools, ","))
	case ToolsPreset:
		// The claude_code preset maps to the CLI's "default" tool set.
		args = append(args, "--tools", "default")
	}

	allowedTools, err := applySkillsDefaults(opts)
	if err != nil {
		return nil, err
	}
	if len(allowedTools) > 0 {
		args = append(args, "--allowedTools", strings.Join(allowedTools, ","))
	}
	// Zero turns is dropped, as in the TypeScript SDK.
	if opts.MaxTurns != nil && *opts.MaxTurns != 0 {
		args = append(args, "--max-turns", strconv.Itoa(*opts.MaxTurns))
	}
	if opts.MaxBudgetUSD != nil {
		args = append(args, "--max-budget-usd", strconv.FormatFloat(*opts.MaxBudgetUSD, 'g', -1, 64))
	}
	if len(opts.DisallowedTools) > 0 {
		args = append(args, "--disallowedTools", strings.Join(opts.DisallowedTools, ","))
	}
	if opts.TaskBudget != nil {
		args = append(args, "--task-budget", strconv.Itoa(opts.TaskBudget.Total))
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if opts.Agent != "" {
		args = append(args, "--agent", opts.Agent)
	}
	if opts.FallbackModel != "" {
		args = append(args, "--fallback-model", opts.FallbackModel)
	}
	if len(opts.Betas) > 0 {
		args = append(args, "--betas", strings.Join(opts.Betas, ","))
	}
	if opts.DebugFile != "" {
		args = append(args, "--debug-file", opts.DebugFile)
	} else if opts.Debug {
		args = append(args, "--debug")
	}
	if opts.PermissionPromptToolName != "" {
		args = append(args, "--permission-prompt-tool", opts.PermissionPromptToolName)
	}
	if opts.PermissionPrompts != "" {
		args = append(args, "--permission-prompts", opts.PermissionPrompts)
	}
	if opts.PermissionMode != "" {
		args = append(args, "--permission-mode", opts.PermissionMode)
	}
	if opts.AllowDangerouslySkipPermissions {
		args = append(args, "--allow-dangerously-skip-permissions")
	}
	if opts.ContinueConversation {
		args = append(args, "--continue")
	}
	// The equals form binds a dash-leading value to its flag; in the
	// two-token form the CLI would parse it as a separate flag, which lets an
	// untrusted session name inject arbitrary options.
	if opts.Resume != "" {
		args = append(args, "--resume="+opts.Resume)
	}
	if opts.SessionID != "" {
		args = append(args, "--session-id="+opts.SessionID)
	}
	settings, err := buildSettingsValue(opts)
	if err != nil {
		return nil, err
	}
	if settings != "" {
		args = append(args, "--settings", settings)
	}
	if opts.ManagedSettings != nil {
		payload, err := json.Marshal(opts.ManagedSettings)
		if err != nil {
			return nil, fmt.Errorf("claude: encoding managed settings: %w", err)
		}
		args = append(args, "--managed-settings", string(payload))
	}
	if opts.ProjectConfigRoot != "" {
		args = append(args, "--project-config-root="+opts.ProjectConfigRoot)
	}
	for _, dir := range opts.AddDirs {
		args = append(args, "--add-dir", dir)
	}
	if opts.MCPConfigPath != "" {
		args = append(args, "--mcp-config", opts.MCPConfigPath)
	} else if servers := processMCPServers(opts.MCPServers); len(servers) > 0 {
		// In-process SDK servers are declared in the initialize request
		// (sdkMcpServers), as the TypeScript SDK does, not here.
		payload, err := json.Marshal(map[string]any{"mcpServers": servers})
		if err != nil {
			return nil, fmt.Errorf("claude: encoding mcp servers: %w", err)
		}
		args = append(args, "--mcp-config", string(payload))
	}
	if opts.IncludePartialMessages {
		args = append(args, "--include-partial-messages")
	}
	if opts.IncludeHookEvents {
		args = append(args, "--include-hook-events")
	}
	if opts.StrictMCPConfig {
		args = append(args, "--strict-mcp-config")
	}
	if opts.ForkSession {
		args = append(args, "--fork-session")
	}
	if opts.ResumeSessionAt != "" {
		args = append(args, "--resume-session-at="+opts.ResumeSessionAt)
	}
	if opts.ResumeDropsTurn != "" {
		args = append(args, "--resume-drops-turn="+opts.ResumeDropsTurn)
	}
	if opts.NoSessionPersistence {
		args = append(args, "--no-session-persistence")
	}
	if opts.SessionStore != nil {
		// The CLI then emits transcript_mirror frames, which the engine
		// forwards to the store.
		args = append(args, "--session-mirror")
	}
	if opts.SettingSources != nil {
		args = append(args, "--setting-sources="+strings.Join(*opts.SettingSources, ","))
	}
	if pluginsViaInitialize(opts) {
		args = append(args, "--await-initialize")
	} else {
		for _, p := range opts.Plugins {
			flag := "--plugin-dir"
			if p.SkipMCPDiscovery {
				flag = "--plugin-dir-no-mcp"
			}
			args = append(args, flag, p.Path)
		}
	}
	for _, flag := range sortedKeys(opts.ExtraArgs) {
		value := opts.ExtraArgs[flag]
		switch {
		case value == nil:
			args = append(args, "--"+flag)
		case len(*value) > 1 && strings.HasPrefix(*value, "-"):
			args = append(args, "--"+flag+"="+*value)
		default:
			args = append(args, "--"+flag, *value)
		}
	}
	args = appendThinkingArgs(args, opts)
	if opts.Effort != "" {
		args = append(args, "--effort", opts.Effort)
	}
	if schema, ok := outputSchema(opts); ok {
		payload, err := json.Marshal(schema)
		if err != nil {
			return nil, fmt.Errorf("claude: encoding output schema: %w", err)
		}
		args = append(args, "--json-schema", string(payload))
	}

	args = append(args, "--input-format", "stream-json")
	return args, nil
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

// outputSchema returns the JSON schema of a json_schema OutputFormat.
func outputSchema(opts *Options) (any, bool) {
	if opts.OutputFormat == nil || opts.OutputFormat["type"] != "json_schema" {
		return nil, false
	}
	schema, ok := opts.OutputFormat["schema"]
	return schema, ok && schema != nil
}

// applySkillsDefaults computes the effective allowed tools for
// Options.Skills: enabling skills implies the Skill tool, so callers do not
// have to allow it by hand. Like the TypeScript SDK, it leaves the setting
// sources alone.
func applySkillsDefaults(opts *Options) ([]string, error) {
	allowed := append([]string(nil), opts.AllowedTools...)
	switch skills := opts.Skills.(type) {
	case SkillsAll:
		if !slices.Contains(allowed, "Skill") {
			allowed = append(allowed, "Skill")
		}
	case SkillList:
		for _, name := range skills {
			if err := validateSkillName(name); err != nil {
				return nil, err
			}
			rule := "Skill(" + name + ")"
			if !slices.Contains(allowed, rule) {
				allowed = append(allowed, rule)
			}
		}
	}
	return allowed, nil
}

// validateSkillName rejects names that cannot ride safely in a Skill(name)
// permission rule, or that could never match a discovered skill.
func validateSkillName(name string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("claude: skill names must be non-empty")
	}
	if !utf8.ValidString(name) {
		return fmt.Errorf("claude: invalid skill name %q: not valid UTF-8, so no discovered skill can match", name)
	}
	if name != strings.TrimSpace(name) {
		return fmt.Errorf("claude: invalid skill name %q: leading or trailing whitespace can never match", name)
	}
	if name == "*" {
		return errors.New(`claude: invalid skill name "*": use SkillsAll{} to enable every skill`)
	}
	if strings.HasSuffix(name, ":*") || strings.HasSuffix(name, " *") {
		return fmt.Errorf("claude: invalid skill name %q: wildcard suffixes are not allowed", name)
	}
	if strings.HasPrefix(name, "/") {
		return fmt.Errorf("claude: invalid skill name %q: use the canonical name, not the slash-command form", name)
	}
	if strings.Contains(name, `\\`) || strings.HasSuffix(name, `\`) {
		return fmt.Errorf("claude: invalid skill name %q: backslash escapes are not allowed", name)
	}
	for _, r := range name {
		if r == '(' || r == ')' || r == ',' || r == '\ufeff' || unicode.IsControl(r) {
			return fmt.Errorf("claude: invalid skill name %q: parentheses, commas and control characters are not allowed", name)
		}
	}
	return nil
}

// buildEnv renders the child process environment.
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
	env["CLAUDE_CODE_ENTRYPOINT"] = entrypoint
	for k, v := range opts.Env {
		env[k] = v
	}
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
