package claude

import (
	"context"
	"maps"

	"github.com/ironpark/gelati/claude/internal/transcript"
)

// sessionDeps are the process-level dependencies of opening a session.
// Tests replace them; nil means the real ones.
type sessionDeps struct {
	// newTransport builds the transport when Options.Transport is nil.
	newTransport func(*Options) Transport
	// resume drives SessionStore resume materialization.
	resume resumeEnv
}

// session is a live CLI session: its engine and what must be cleaned up
// after it.
type session struct {
	eng *engine
	// materialized is the temp config dir of a SessionStore resume, or nil.
	// Remove it with cleanup once eng is closed.
	materialized *materializedResume
}

// close stops the engine and then removes the temp config dir of a
// SessionStore resume, which the CLI must no longer be using. It is
// idempotent.
func (s *session) close() error {
	err := s.eng.close()
	s.materialized.cleanup()
	return err
}

// startSession is the startup path shared by New, Query, Startup and
// Prewarm: it opens a session, starts its engine and completes the
// initialize handshake, which DefaultInitializeTimeout bounds when ctx has no
// deadline. On failure the session is closed and nothing is left behind.
//
// ctx bounds the startup only: the session outlives it and lasts until it is
// closed or the CLI exits. ctx's values are kept for the handlers the engine
// runs.
func startSession(ctx context.Context, opts *Options, entry string, deps *sessionDeps) (*session, error) {
	sess, err := openSession(ctx, opts, entry, deps)
	if err != nil {
		return nil, err
	}
	sess.eng.start(ctx)
	if _, err := sess.eng.initialize(ctx); err != nil {
		_ = sess.close()
		return nil, err
	}
	return sess, nil
}

// openSession runs the setup that precedes the handshake, in the order of the
// Python SDK: validate the options and resolve the launch configuration;
// materialize a SessionStore-backed resume into a temp CLAUDE_CONFIG_DIR
// (skipped with a custom Transport, which never sees the rewritten options);
// connect the transport; build the engine with SDK MCP servers and, with a
// SessionStore, transcript mirroring. Nothing is left behind on failure.
func openSession(ctx context.Context, raw *Options, entry string, deps *sessionDeps) (*session, error) {
	if deps == nil {
		deps = &sessionDeps{}
	}
	opts, launch, err := prepareOptions(raw, entry)
	if err != nil {
		return nil, err
	}
	var materialized *materializedResume
	if opts.Transport == nil {
		materialized, err = materializeResumeSession(ctx, opts, deps.resume)
		if err != nil {
			return nil, err
		}
	}
	mirrorDir := transcript.ProjectsDir(opts.Env)
	if materialized != nil {
		// The rewrite touches only single-channel flags and the
		// environment, which the subprocess transport renders from opts,
		// so launch stays valid.
		opts = applyMaterializedOptions(opts, materialized)
		mirrorDir = materialized.projectsDir()
	}

	transport := opts.Transport
	if transport == nil {
		if deps.newTransport != nil {
			transport = deps.newTransport(opts)
		} else {
			transport = newSubprocessTransport(opts, launch)
		}
	}
	if err := transport.Connect(ctx); err != nil {
		materialized.cleanup()
		return nil, err
	}

	eng := newEngine(transport, opts, launch)
	eng.enableTranscriptMirror(opts, mirrorDir)
	return &session{eng: eng, materialized: materialized}, nil
}

// prepareOptions validates the options and returns a copy carrying the
// derived settings (the SDK permission handler and the entrypoint marker),
// resolved as a launchConfig. The launch is resolved here, before anything is
// materialized or spawned, so its encoding errors surface early whatever the
// transport, and its settings are encoded once per session.
func prepareOptions(opts *Options, entry string) (*Options, *launchConfig, error) {
	if err := validateOptions(opts); err != nil {
		return nil, nil, err
	}
	var copied Options
	if opts != nil {
		copied = *opts
	}
	if copied.CanUseTool != nil {
		// Routes permission prompts over the control protocol.
		copied.PermissionPromptToolName = "stdio"
	}
	env := make(map[string]string, len(copied.Env)+1)
	maps.Copy(env, copied.Env)
	if _, ok := env["CLAUDE_CODE_ENTRYPOINT"]; !ok {
		env["CLAUDE_CODE_ENTRYPOINT"] = entry
	}
	copied.Env = env
	launch, err := resolveLaunch(&copied)
	if err != nil {
		return nil, nil, err
	}
	return &copied, launch, nil
}
