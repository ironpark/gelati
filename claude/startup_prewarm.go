package claude

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// claimHookSettingKeys are flag settings that govern hooks. A claim cannot
// carry them: the claimed folder's SessionStart hooks start with the claim,
// before an overlay lands.
var claimHookSettingKeys = []string{
	"hooks", "disableAllHooks", "allowManagedHooksOnly", "allowedHttpHookUrls", "httpHookAllowedEnvVars",
}

// ClaimOptions are the per-session options a SpareProcess is claimed with:
// what a host only knows once the user picked a folder. Everything else is
// given to Prewarm and fixed for the life of the spare.
//
// Alpha: this API may change.
type ClaimOptions struct {
	// Cwd is the session's working directory (required). A relative path
	// is resolved against this process's working directory; ~/ is expanded
	// by the CLI.
	Cwd string
	// Env holds per-session environment additions the CLI accepts at claim
	// time (fresh tokens, the host's session ID). Variables read during
	// startup belong in the Prewarm options; the claim is refused with them.
	Env map[string]string
	// AdditionalDirectories are added like --add-dir; relative entries
	// resolve against Cwd.
	AdditionalDirectories []string
	// Model selects the model, as Client.SetModel does.
	Model string
	// PermissionMode is part of the claim itself: a mode the process cannot
	// take refuses the claim rather than running under another mode.
	PermissionMode PermissionMode
	// Settings is a flag-tier settings overlay applied as
	// Client.ApplyFlagSettings does. With an overlay the prompt is held until
	// the process accepts it, and not sent if it is refused. Keys that
	// govern hooks are rejected; give them to Prewarm instead.
	Settings map[string]any
	// AppendSystemPrompt is appended to the system prompt.
	AppendSystemPrompt string
	// Title is the session title.
	Title string
	// Agents defines subagents for the session.
	Agents map[string]AgentDefinition
}

// ClaimResult describes a claimed spare.
//
// Alpha: this API may change.
type ClaimResult struct {
	// Cwd is the canonical working directory of the session.
	Cwd string
	// SessionID is the claimed session's ID.
	SessionID string
	// ParkedMS is how long the spare waited for its claim, when reported.
	ParkedMS *int64
	// SDKMCPSettled reports whether the in-process MCP servers were settled
	// at claim time.
	SDKMCPSettled bool
}

// ClaimError reports a claim that failed or only partly applied. Its message
// starts with the reason: spare_exited, spare_closed, settings_not_applied,
// option_not_applied, or the CLI's own refusal (not_a_spare, cwd_not_found,
// permission_mode_not_claimable, claim_failed, ...). Only after
// option_not_applied does the prompt run; start the session with Query
// otherwise.
//
// Alpha: this API may change.
type ClaimError struct {
	baseError
	// Claim describes the claimed session for settings_not_applied and
	// option_not_applied, whose claim itself succeeded; nil otherwise.
	Claim *ClaimResult
	// Err is the underlying error, if any.
	Err error

	// reason is the reason the SDK itself reported, one of the claim*
	// constants; empty for the CLI's refusals.
	reason string
}

// Unwrap returns the underlying error.
func (e *ClaimError) Unwrap() error { return e.Err }

// Reasons the SDK reports in a ClaimError.
const (
	claimSpareExited        = "spare_exited"
	claimSpareClosed        = "spare_closed"
	claimFailed             = "claim_failed"
	claimSettingsNotApplied = "settings_not_applied"
	claimOptionNotApplied   = "option_not_applied"
)

// newClaimError builds a ClaimError whose message is reason: detail.
func newClaimError(reason, detail string, claim *ClaimResult, err error) *ClaimError {
	return &ClaimError{baseError: baseError{Msg: reason + ": " + detail}, Claim: claim, Err: err, reason: reason}
}

// spareState is where a SpareProcess is in its life.
type spareState int

const (
	// spareParked waits for a claim.
	spareParked spareState = iota
	// spareClaimed has been claimed; it stays so when the session ends.
	spareClaimed
	// spareClosed was closed before a claim.
	spareClosed
)

// SpareProcess is a pre-started CLI process parked until a session claims it,
// so the first response of a session arrives without the startup latency.
// Build one with Prewarm, then call Claim or ClaimStream once.
//
// Alpha: this API may change.
type SpareProcess struct {
	sess       *session
	parkDir    string
	sdkServers []string

	mu    sync.Mutex
	state spareState

	settleOnce sync.Once
	settled    chan struct{}
	result     *ClaimResult
	err        error

	parkOnce sync.Once
}

// Prewarm starts a CLI process that waits for a session to claim it (the
// CLI's --await-claim mode) and completes the initialize handshake, bounded by
// DefaultInitializeTimeout when ctx has no deadline. A spare has no session
// yet, so Resume, ContinueConversation and ForkSession are rejected.
//
// Without Options.Cwd the process is parked in a fresh private directory under
// the Claude config directory (spares/spare-*), removed once the spare is
// claimed or gone. ctx governs the process's whole life, as for Startup.
//
// Alpha: this API may change, and it requires a CLI that supports
// --await-claim.
func Prewarm(ctx context.Context, opts Options) (*SpareProcess, error) {
	return prewarm(ctx, &opts, nil)
}

func prewarm(ctx context.Context, opts *Options, deps *sessionDeps) (*SpareProcess, error) {
	spareOpts, parkDir, err := spareOptions(opts)
	if err != nil {
		return nil, err
	}
	sess, err := startSession(ctx, spareOpts, entrypoint, deps)
	if err != nil {
		if parkDir != "" {
			_ = os.RemoveAll(parkDir)
		}
		return nil, err
	}
	s := &SpareProcess{
		sess:       sess,
		parkDir:    parkDir,
		sdkServers: slices.Sorted(maps.Keys(sdkMCPServers(spareOpts))),
		settled:    make(chan struct{}),
	}
	go s.watchExit()
	return s, nil
}

// spareOptions copies opts for a spare process: started with --await-claim
// and, without a Cwd, parked in a fresh private directory, which is returned
// (empty when opts name a Cwd).
func spareOptions(opts *Options) (*Options, string, error) {
	var copied Options
	if opts != nil {
		copied = *opts
	}
	if copied.Resume != "" || copied.ContinueConversation || copied.ForkSession {
		return nil, "", errors.New("claude: Prewarm: Resume, ContinueConversation and ForkSession describe a session; " +
			"a spare has none, so pass them to Query instead")
	}
	parkDir := ""
	if copied.Cwd == "" {
		root, err := spareParkRoot(copied.Env)
		if err != nil {
			return nil, "", err
		}
		if err := os.MkdirAll(root, 0o700); err != nil {
			return nil, "", fmt.Errorf("claude: Prewarm: creating %s: %w", root, err)
		}
		dir, err := os.MkdirTemp(root, "spare-")
		if err != nil {
			return nil, "", fmt.Errorf("claude: Prewarm: creating park directory: %w", err)
		}
		parkDir = dir
		copied.Cwd = dir
	}
	extra := make(map[string]*string, len(copied.ExtraArgs)+1)
	maps.Copy(extra, copied.ExtraArgs)
	extra["await-claim"] = nil
	copied.ExtraArgs = extra
	return &copied, parkDir, nil
}

// spareParkRoot picks the directory spares are parked under:
// <config dir>/spares, where the config dir comes from Options.Env
// (CLAUDE_CONFIG_DIR, else HOME/.claude) or else the process environment.
func spareParkRoot(env map[string]string) (string, error) {
	var dir string
	if len(env) == 0 {
		dir = claudeConfigHomeDir()
	} else if c := strings.TrimSpace(env["CLAUDE_CONFIG_DIR"]); c != "" {
		dir = c
	} else {
		home := strings.TrimSpace(env["HOME"])
		if home == "" {
			home, _ = userHomeDir()
		}
		dir = filepath.Join(home, ".claude")
	}
	if !filepath.IsAbs(dir) {
		home, _ := userHomeDir()
		dir = filepath.Join(home, ".claude")
	}
	if !filepath.IsAbs(dir) {
		return "", errors.New("claude: Prewarm: no absolute config directory to park the spare under; " +
			"set CLAUDE_CONFIG_DIR or HOME to an absolute path, or set Options.Cwd")
	}
	return filepath.Join(dir, "spares"), nil
}

// watchExit reports a spare whose process ends before a claim, and removes the
// park directory once the process is gone.
func (s *SpareProcess) watchExit() {
	<-s.sess.eng.outputDone()
	s.mu.Lock()
	claimed := s.state == spareClaimed
	s.mu.Unlock()
	if !claimed {
		s.settle(nil, newClaimError(claimSpareExited, "the spare process exited before it was claimed", nil, nil))
	}
	s.removePark()
}

func (s *SpareProcess) removePark() {
	s.parkOnce.Do(func() {
		if s.parkDir != "" {
			_ = os.RemoveAll(s.parkDir)
		}
	})
}

// settle records the claim outcome once.
func (s *SpareProcess) settle(result *ClaimResult, err error) {
	s.settleOnce.Do(func() {
		s.result, s.err = result, err
		close(s.settled)
	})
}

// Claimed waits until the process has answered the claim and the options
// sent with it, and returns the claimed session. An error (a *ClaimError
// unless ctx ended) means the prompt has not run, except for
// option_not_applied: then the session runs without that option.
func (s *SpareProcess) Claimed(ctx context.Context) (*ClaimResult, error) {
	select {
	case <-s.settled:
		return s.result, s.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Exited is closed when the spare's process ends, claimed or not. A parked
// spare that exits should be replaced.
func (s *SpareProcess) Exited() <-chan struct{} {
	return s.sess.eng.outputDone()
}

// InitializationResult reports the initialize response: of the parked process
// before a claim, and of the claimed session once the claim succeeded.
func (s *SpareProcess) InitializationResult() *InitializeResult {
	return s.sess.eng.initializeResult()
}

// Claim binds the spare to a session and sends prompt as its first message,
// returning the session's messages as the package-level Query does. It can be
// called once; the claim itself is written before Claim returns, and the
// prompt when the sequence is ranged over (after the claim is accepted when
// opts.Settings is set). Claimed reports the claim's outcome.
func (s *SpareProcess) Claim(ctx context.Context, prompt string, opts ClaimOptions) (iter.Seq2[Message, error], error) {
	return s.ClaimStream(ctx, slices.Values([]UserInput{{Content: prompt}}), opts)
}

// ClaimStream is Claim with several user turns known up front.
func (s *SpareProcess) ClaimStream(ctx context.Context, inputs iter.Seq[UserInput], opts ClaimOptions) (iter.Seq2[Message, error], error) {
	if err := validateClaimSettings(opts.Settings); err != nil {
		return nil, err
	}
	if err := s.reserveClaim(); err != nil {
		return nil, err
	}

	// The claim, then the per-session options, are written in order before
	// the prompt can be.
	cwd := resolveClaimCwd(opts.Cwd)
	claimWait, err := s.sess.eng.beginControlRequest(ctx, claimRequest(cwd, s.sdkServers, opts))
	if err != nil {
		s.settle(nil, newClaimError(claimFailed, err.Error(), nil, err))
		return nil, err
	}
	var modelWait, settingsWait replyWait
	if opts.Model != "" {
		modelWait = s.beginOption(ctx, map[string]any{"subtype": "set_model", "model": opts.Model})
	}
	if opts.Settings != nil {
		settingsWait = s.beginOption(ctx, map[string]any{"subtype": "apply_flag_settings", "settings": opts.Settings})
	}
	go s.awaitClaim(ctx, claimWait, modelWait, settingsWait, cwd)

	holdForSettings := opts.Settings != nil
	return singleUse(func(yield func(Message, error) bool) {
		s.runClaimed(ctx, inputs, holdForSettings, yield)
	}, "claude: a claimed session's sequence can be ranged over only once"), nil
}

// validateClaimSettings refuses a claim settings overlay that governs hooks.
func validateClaimSettings(settings map[string]any) error {
	var hookKeys []string
	for _, key := range claimHookSettingKeys {
		if v, ok := settings[key]; ok && v != nil {
			hookKeys = append(hookKeys, key)
		}
	}
	if len(hookKeys) == 0 {
		return nil
	}
	return fmt.Errorf("claude: SpareProcess.Claim: hook settings (%s) cannot be applied at claim, "+
		"because the claimed folder's SessionStart hooks start with the claim; pass them to Prewarm in "+
		"Options.Settings. Nothing was sent: the spare can still be claimed without them",
		strings.Join(hookKeys, ", "))
}

// reserveClaim moves a parked spare to claimed, refusing a second claim and
// a spare that was closed or has exited.
func (s *SpareProcess) reserveClaim() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.state == spareClaimed:
		return errors.New("claude: SpareProcess.Claim can be called only once")
	case s.state == spareClosed || isDone(s.sess.eng.outputDone()):
		return errors.New("claude: SpareProcess.Claim: the spare was closed or has exited; start the session with Query")
	}
	s.state = spareClaimed
	return nil
}

// resolveClaimCwd makes a relative claim directory absolute against this
// process's working directory; ~ forms are left for the CLI to expand.
func resolveClaimCwd(cwd string) string {
	if strings.TrimSpace(cwd) != "" && !filepath.IsAbs(cwd) && cwd != "~" && !strings.HasPrefix(cwd, "~/") {
		if abs, err := filepath.Abs(cwd); err == nil {
			return abs
		}
	}
	return cwd
}

// claimRequest builds the claim_session control request.
func claimRequest(cwd string, sdkServers []string, opts ClaimOptions) map[string]any {
	req := map[string]any{
		"subtype":            "claim_session",
		"cwd":                cwd,
		"sdk_mcp_servers":    nonNilStrings(sdkServers),
		"include_initialize": true,
	}
	if opts.PermissionMode != "" {
		req["permission_mode"] = opts.PermissionMode
	}
	if opts.Env != nil {
		req["env"] = opts.Env
	}
	if opts.AdditionalDirectories != nil {
		req["additional_directories"] = opts.AdditionalDirectories
	}
	if opts.AppendSystemPrompt != "" {
		req["append_system_prompt"] = opts.AppendSystemPrompt
	}
	if opts.Title != "" {
		req["title"] = opts.Title
	}
	if opts.Agents != nil {
		req["agents"] = opts.Agents
	}
	return req
}

// replyWait waits for the response to a control request already written.
type replyWait func(context.Context) (map[string]any, error)

// beginOption writes a per-session option request of a claim, right after the
// claim itself. A request that could not be written reports its error when
// waited for.
func (s *SpareProcess) beginOption(ctx context.Context, request map[string]any) replyWait {
	wait, err := s.sess.eng.beginControlRequest(ctx, request)
	if err != nil {
		return func(context.Context) (map[string]any, error) { return nil, err }
	}
	return wait
}

// awaitClaim waits for the claim and its option requests (nil when not sent)
// to be answered and settles the outcome. A refused model leaves the session
// running without it; a refused settings overlay fails the claim.
func (s *SpareProcess) awaitClaim(ctx context.Context, claimWait, modelWait, settingsWait replyWait, cwd string) {
	resp, err := claimWait(ctx)
	if err != nil {
		// The CLI answers a prompt sent to a refused claim with a
		// not_claimed error result; close the input so it exits.
		_ = s.sess.eng.endInput()
		s.settle(nil, &ClaimError{baseError: baseError{Msg: err.Error()}, Err: err})
		return
	}
	s.removePark()
	result := s.applyClaimResponse(resp, cwd)
	var modelErr, settingsErr error
	if modelWait != nil {
		_, modelErr = modelWait(ctx)
	}
	if settingsWait != nil {
		_, settingsErr = settingsWait(ctx)
	}
	switch {
	case settingsErr != nil:
		_ = s.sess.close()
		s.settle(nil, newClaimError(claimSettingsNotApplied, settingsErr.Error()+
			"; the prompt was not sent and the process was closed; start the session with Query", result, settingsErr))
	case modelErr != nil:
		s.settle(nil, newClaimError(claimOptionNotApplied,
			"model: "+modelErr.Error()+"; the session runs without these options", result, nil))
	default:
		s.settle(result, nil)
	}
}

// runClaimed runs the claimed session's query. With a settings overlay the
// prompt first waits for the claim's outcome, and is not sent unless the
// session runs.
func (s *SpareProcess) runClaimed(ctx context.Context, inputs iter.Seq[UserInput], holdForSettings bool, yield func(Message, error) bool) {
	if holdForSettings {
		select {
		case <-s.settled:
		case <-ctx.Done():
			_ = s.sess.close()
			yield(nil, ctx.Err())
			return
		}
		if s.err != nil {
			if claimErr, ok := errors.AsType[*ClaimError](s.err); !ok || claimErr.reason != claimOptionNotApplied {
				_ = s.sess.close()
				yield(nil, s.err)
				return
			}
		}
	}
	runQuery(ctx, s.sess, inputs, yield)
}

// applyClaimResponse turns a claim_session response into a ClaimResult and
// adopts the claimed session's initialize response, keeping hooks_applied and
// plugins_applied from the parked process when the claim does not report
// them.
func (s *SpareProcess) applyClaimResponse(resp map[string]any, cwd string) *ClaimResult {
	result := &ClaimResult{Cwd: cwd, SessionID: str(resp["session_id"])}
	if c := str(resp["cwd"]); c != "" {
		result.Cwd = c
	}
	if ms, ok := toInt(resp["parked_ms"]); ok {
		v := int64(ms)
		result.ParkedMS = &v
	}
	result.SDKMCPSettled, _ = resp["sdk_mcp_settled"].(bool)

	initialize, ok := resp["initialize"].(map[string]any)
	if !ok {
		return result
	}
	eng := s.sess.eng
	merged := maps.Clone(initialize)
	if prev := eng.initializeResult(); prev != nil {
		for _, key := range []string{"hooks_applied", "plugins_applied"} {
			if _, has := merged[key]; !has {
				if v, ok := prev.Raw[key]; ok {
					merged[key] = v
				}
			}
		}
	}
	eng.setInitResponse(merged)
	return result
}

// Close terminates the process. Before a claim this discards the spare and
// Claimed reports spare_closed; after one it ends the session. It is
// idempotent.
func (s *SpareProcess) Close() error {
	s.mu.Lock()
	parked := s.state == spareParked
	if parked {
		s.state = spareClosed
	}
	s.mu.Unlock()
	if parked {
		s.settle(nil, newClaimError(claimSpareClosed, "the spare was closed before it was claimed", nil, nil))
	}
	err := s.sess.close()
	s.removePark()
	return err
}
