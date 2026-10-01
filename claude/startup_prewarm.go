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
}

// Unwrap returns the underlying error.
func (e *ClaimError) Unwrap() error { return e.Err }

// SpareProcess is a pre-started CLI process parked until a session claims it,
// so the first response of a session arrives without the startup latency.
// Build one with Prewarm, then call Claim or ClaimStream once.
//
// Alpha: this API may change.
type SpareProcess struct {
	sess       *session
	parkDir    string
	sdkServers []string

	mu      sync.Mutex
	claimed bool
	closed  bool

	settleOnce sync.Once
	settled    chan struct{}
	result     *ClaimResult
	err        error

	parkOnce sync.Once
}

// Prewarm starts a CLI process that waits for a session to claim it (the
// CLI's --await-claim mode) and completes the initialize handshake, bounded by
// DefaultInitializeTimeout unless ctx has an earlier deadline. A spare has no
// session yet, so Resume, ContinueConversation and ForkSession are rejected.
//
// Without Options.Cwd the process is parked in a fresh private directory under
// the Claude config directory (spares/spare-*), removed once the spare is
// claimed or gone. ctx governs the process's whole life, as for Startup.
//
// Alpha: this API may change, and it requires a CLI that supports
// --await-claim.
func Prewarm(ctx context.Context, opts *Options) (*SpareProcess, error) {
	return prewarm(ctx, opts, nil)
}

func prewarm(ctx context.Context, opts *Options, deps *sessionDeps) (*SpareProcess, error) {
	var copied Options
	if opts != nil {
		copied = *opts
	}
	if copied.Resume != "" || copied.ContinueConversation || copied.ForkSession {
		return nil, errors.New("claude: Prewarm: Resume, ContinueConversation and ForkSession describe a session; " +
			"a spare has none, so pass them to Query instead")
	}
	parkDir := ""
	if copied.Cwd == "" {
		root, err := spareParkRoot(copied.Env)
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(root, 0o700); err != nil {
			return nil, fmt.Errorf("claude: Prewarm: creating %s: %w", root, err)
		}
		dir, err := os.MkdirTemp(root, "spare-")
		if err != nil {
			return nil, fmt.Errorf("claude: Prewarm: creating park directory: %w", err)
		}
		parkDir = dir
		copied.Cwd = dir
	}
	extra := make(map[string]*string, len(copied.ExtraArgs)+1)
	maps.Copy(extra, copied.ExtraArgs)
	extra["await-claim"] = nil
	copied.ExtraArgs = extra

	removePark := func() {
		if parkDir != "" {
			_ = os.RemoveAll(parkDir)
		}
	}
	sess, err := openSession(ctx, &copied, entrypoint, deps)
	if err != nil {
		removePark()
		return nil, err
	}
	if err := initializeWarm(ctx, sess); err != nil {
		removePark()
		return nil, err
	}

	sdkServers := slices.Sorted(maps.Keys(sdkMCPServers(&copied)))
	s := &SpareProcess{
		sess:       sess,
		parkDir:    parkDir,
		sdkServers: sdkServers,
		settled:    make(chan struct{}),
	}
	go func() {
		<-sess.eng.readerDone
		s.mu.Lock()
		claimed := s.claimed
		s.mu.Unlock()
		if !claimed {
			s.settle(nil, &ClaimError{baseError: baseError{Msg: "spare_exited: the spare process exited before it was claimed"}})
		}
		s.removePark()
	}()
	return s, nil
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
			home, _ = os.UserHomeDir()
		}
		dir = filepath.Join(home, ".claude")
	}
	if !filepath.IsAbs(dir) {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".claude")
	}
	if !filepath.IsAbs(dir) {
		return "", errors.New("claude: Prewarm: no absolute config directory to park the spare under; " +
			"set CLAUDE_CONFIG_DIR or HOME to an absolute path, or set Options.Cwd")
	}
	return filepath.Join(dir, "spares"), nil
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
	return s.sess.eng.readerDone
}

// InitializationResult reports the initialize response: of the parked process
// before a claim, and of the claimed session once the claim succeeded.
func (s *SpareProcess) InitializationResult() *InitializeResult {
	return s.sess.eng.InitializeResult()
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
	var hookKeys []string
	for _, key := range claimHookSettingKeys {
		if v, ok := opts.Settings[key]; ok && v != nil {
			hookKeys = append(hookKeys, key)
		}
	}
	if len(hookKeys) > 0 {
		return nil, fmt.Errorf("claude: SpareProcess.Claim: hook settings (%s) cannot be applied at claim, "+
			"because the claimed folder's SessionStart hooks start with the claim; pass them to Prewarm in "+
			"Options.Settings. Nothing was sent: the spare can still be claimed without them",
			strings.Join(hookKeys, ", "))
	}

	s.mu.Lock()
	switch {
	case s.claimed:
		s.mu.Unlock()
		return nil, errors.New("claude: SpareProcess.Claim can be called only once")
	case s.closed || s.exited():
		s.mu.Unlock()
		return nil, errors.New("claude: SpareProcess.Claim: the spare was closed or has exited; start the session with Query")
	}
	s.claimed = true
	s.mu.Unlock()

	cwd := opts.Cwd
	if strings.TrimSpace(cwd) != "" && !filepath.IsAbs(cwd) && cwd != "~" && !strings.HasPrefix(cwd, "~/") {
		if abs, err := filepath.Abs(cwd); err == nil {
			cwd = abs
		}
	}
	eng := s.sess.eng

	// The claim, then the per-session options, are written in order before
	// the prompt can be.
	claimReq := map[string]any{
		"subtype":            "claim_session",
		"cwd":                cwd,
		"sdk_mcp_servers":    nonNilStrings(s.sdkServers),
		"include_initialize": true,
	}
	if opts.PermissionMode != "" {
		claimReq["permission_mode"] = opts.PermissionMode
	}
	if opts.Env != nil {
		claimReq["env"] = opts.Env
	}
	if opts.AdditionalDirectories != nil {
		claimReq["additional_directories"] = opts.AdditionalDirectories
	}
	if opts.AppendSystemPrompt != "" {
		claimReq["append_system_prompt"] = opts.AppendSystemPrompt
	}
	if opts.Title != "" {
		claimReq["title"] = opts.Title
	}
	if opts.Agents != nil {
		claimReq["agents"] = opts.Agents
	}
	claimWait, err := eng.beginControlRequest(ctx, claimReq)
	if err != nil {
		s.settle(nil, &ClaimError{baseError: baseError{Msg: "claim_failed: " + err.Error()}, Err: err})
		return nil, err
	}

	type option struct {
		name string
		wait func(context.Context) (map[string]any, error)
		err  error
	}
	var options []*option
	var settingsOpt *option
	if opts.Model != "" {
		o := &option{name: "model"}
		o.wait, o.err = eng.beginControlRequest(ctx, map[string]any{"subtype": "set_model", "model": opts.Model})
		options = append(options, o)
	}
	if opts.Settings != nil {
		settingsOpt = &option{name: "settings"}
		settingsOpt.wait, settingsOpt.err = eng.beginControlRequest(ctx,
			map[string]any{"subtype": "apply_flag_settings", "settings": opts.Settings})
		options = append(options, settingsOpt)
	}

	go func() {
		resp, err := claimWait(ctx)
		if err != nil {
			// The CLI answers a prompt sent to a refused claim with a
			// not_claimed error result; close the input so it exits.
			_ = s.sess.eng.transport.EndInput()
			s.settle(nil, &ClaimError{baseError: baseError{Msg: err.Error()}, Err: err})
			return
		}
		s.removePark()
		result := s.applyClaimResponse(resp, cwd)
		var failed []string
		var settingsErr error
		for _, o := range options {
			oerr := o.err
			if oerr == nil {
				_, oerr = o.wait(ctx)
			}
			if oerr == nil {
				continue
			}
			if o == settingsOpt {
				settingsErr = oerr
			} else {
				failed = append(failed, o.name+": "+oerr.Error())
			}
		}
		switch {
		case settingsErr != nil:
			s.mu.Lock()
			s.closed = true
			s.mu.Unlock()
			_ = s.sess.close()
			s.settle(nil, &ClaimError{
				baseError: baseError{Msg: "settings_not_applied: " + settingsErr.Error() +
					"; the prompt was not sent and the process was closed; start the session with Query"},
				Claim: result, Err: settingsErr,
			})
		case len(failed) > 0:
			s.settle(nil, &ClaimError{
				baseError: baseError{Msg: "option_not_applied: " + strings.Join(failed, "; ") + "; the session runs without these options"},
				Claim:     result,
			})
		default:
			s.settle(result, nil)
		}
	}()

	holdForSettings := opts.Settings != nil
	var once sync.Once
	return func(yield func(Message, error) bool) {
		ran := false
		once.Do(func() {
			ran = true
			if holdForSettings {
				select {
				case <-s.settled:
				case <-ctx.Done():
					_ = s.sess.close()
					yield(nil, ctx.Err())
					return
				}
				var claimErr *ClaimError
				if s.err != nil && (!errors.As(s.err, &claimErr) || !strings.HasPrefix(claimErr.Msg, "option_not_applied")) {
					_ = s.sess.close()
					yield(nil, s.err)
					return
				}
			}
			runQuery(ctx, s.sess, inputs, yield)
		})
		if !ran {
			yield(nil, errors.New("claude: a claimed session's sequence can be ranged over only once"))
		}
	}, nil
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
	if prev := eng.InitializeResult(); prev != nil {
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

func (s *SpareProcess) exited() bool {
	select {
	case <-s.sess.eng.readerDone:
		return true
	default:
		return false
	}
}

// Close terminates the process. Before a claim this discards the spare and
// Claimed reports spare_closed; after one it ends the session. It is
// idempotent.
func (s *SpareProcess) Close() error {
	s.mu.Lock()
	s.closed = true
	claimed := s.claimed
	s.mu.Unlock()
	if !claimed {
		s.settle(nil, &ClaimError{baseError: baseError{Msg: "spare_closed: the spare was closed before it was claimed"}})
	}
	err := s.sess.close()
	s.removePark()
	return err
}
