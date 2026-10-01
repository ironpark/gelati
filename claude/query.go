package claude

import (
	"context"
	"errors"
	"iter"
	"maps"
	"slices"
	"time"
)

// UserInput is one user turn written to the CLI's input stream: the
// TypeScript SDKUserMessage.
type UserInput struct {
	// Content is the prompt text.
	Content string
	// Blocks sends the message as content blocks, e.g. an *ImageBlock or
	// *DocumentBlock next to a *TextBlock; an *UnknownBlock passes any other
	// block kind through verbatim. When Blocks is set, a non-empty Content
	// becomes a leading text block.
	Blocks []ContentBlock
	// UUID identifies the message. Set it to correlate replies
	// (AssistantMessage.UserMessageUUID, ResultMessage.UserMessageUUID) and to
	// target the message later, e.g. with file rewinds.
	UUID string
	// SessionID targets a specific session; empty lets the CLI choose.
	SessionID string
	// ParentToolUseID attributes the turn to a tool call, when relevant.
	ParentToolUseID string
	// Origin stamps the message's provenance. Only the "human" kind is
	// honored from an SDK host.
	Origin *MessageOrigin
	// Priority queues the message: MessagePriorityNow, MessagePriorityNext
	// or MessagePriorityLater. Empty leaves the CLI default.
	Priority MessagePriority
	// ShouldQuery, when false, appends the message to the transcript without
	// starting a turn; it is merged into the next message that does. nil
	// leaves the CLI default (start a turn).
	ShouldQuery *bool
	// IsSynthetic marks content the user did not type.
	IsSynthetic bool
	// Timestamp is the message's creation time, for display; zero omits it.
	Timestamp time.Time
	// ClientComposed sends the text as written: no @-mention expansion, no
	// slash-command dispatch and no turn-start attachments. It is what
	// Options.VerbatimPrompts sets on every message.
	ClientComposed bool
	// PastedContent is text the user pasted rather than typed; the CLI
	// appends each entry after the typed text.
	PastedContent []string
	// InlinePastes marks pasted text that is still inside Content where the
	// user put it, one entry per paste.
	InlinePastes []string
	// Raw replaces the whole frame when set, for fields this struct does not
	// model. Content and the other fields are ignored.
	Raw map[string]any
}

// frame renders the input as a stream-json user message.
func (u UserInput) frame() map[string]any {
	if u.Raw != nil {
		return u.Raw
	}
	var content any = u.Content
	if len(u.Blocks) > 0 {
		blocks := make([]any, 0, len(u.Blocks)+1)
		if u.Content != "" {
			blocks = append(blocks, wireBlock{&TextBlock{Text: u.Content}})
		}
		for _, b := range u.Blocks {
			if b != nil {
				blocks = append(blocks, wireBlock{b})
			}
		}
		content = blocks
	}
	out := map[string]any{
		"type":               "user",
		"session_id":         u.SessionID,
		"message":            map[string]any{"role": "user", "content": content},
		"parent_tool_use_id": nil,
	}
	if u.ParentToolUseID != "" {
		out["parent_tool_use_id"] = u.ParentToolUseID
	}
	if u.Origin != nil {
		out["origin"] = u.Origin
	}
	if u.UUID != "" {
		out["uuid"] = u.UUID
	}
	if u.Priority != "" {
		out["priority"] = u.Priority
	}
	if u.ShouldQuery != nil {
		out["shouldQuery"] = *u.ShouldQuery
	}
	if u.IsSynthetic {
		out["isSynthetic"] = true
	}
	if !u.Timestamp.IsZero() {
		// The ISO 8601 form JavaScript's Date.toISOString produces.
		out["timestamp"] = u.Timestamp.UTC().Format("2006-01-02T15:04:05.000Z07:00")
	}
	if u.ClientComposed {
		out["client_composed"] = true
	}
	if len(u.PastedContent) > 0 {
		out["pasted_content"] = u.PastedContent
	}
	if len(u.InlinePastes) > 0 {
		out["inline_pastes"] = u.InlinePastes
	}
	return out
}

// stampUserMessage applies Options.VerbatimPrompts to an outgoing frame. The
// frame is returned unchanged when verbatim is off; otherwise a copy carries
// client_composed=true, so a caller's Raw map is never mutated.
func stampUserMessage(frame map[string]any, verbatim bool) map[string]any {
	if !verbatim {
		return frame
	}
	out := maps.Clone(frame)
	out["client_composed"] = true
	return out
}

// Query runs a one-shot prompt and yields the messages it produces, ending with
// a ResultMessage. It is the simple, stateless entry point; use Client for
// interactive sessions that need follow-ups or interrupts.
//
// The CLI subprocess is torn down when the sequence ends, when the caller
// breaks out of the range loop, or when ctx is cancelled.
//
//	for msg, err := range claude.Query(ctx, "What is 2+2?", nil) {
//		if err != nil {
//			return err
//		}
//		fmt.Println(msg)
//	}
func Query(ctx context.Context, prompt string, opts *Options) iter.Seq2[Message, error] {
	return QueryStream(ctx, slices.Values([]UserInput{{Content: prompt}}), opts)
}

// QueryStream is Query with several user turns known up front. Every input is
// written before the responses are consumed, so it stays unidirectional: use
// Client when a later turn depends on an earlier response.
func QueryStream(ctx context.Context, inputs iter.Seq[UserInput], opts *Options) iter.Seq2[Message, error] {
	return queryStream(ctx, inputs, opts, nil)
}

// queryStream is QueryStream with replaceable session dependencies.
func queryStream(ctx context.Context, inputs iter.Seq[UserInput], opts *Options, deps *sessionDeps) iter.Seq2[Message, error] {
	return func(yield func(Message, error) bool) {
		sess, err := openSession(ctx, opts, entrypoint, deps)
		if err != nil {
			yield(nil, err)
			return
		}
		sess.eng.Start(ctx)
		if _, err := sess.eng.Initialize(ctx); err != nil {
			_ = sess.close()
			yield(nil, err)
			return
		}
		runQuery(ctx, sess, inputs, yield)
	}
}

// runQuery writes inputs to an initialized session, yields its messages and
// closes the session when the sequence ends or the caller stops.
func runQuery(ctx context.Context, sess *session, inputs iter.Seq[UserInput], yield func(Message, error) bool) {
	opts, eng := sess.opts, sess.eng
	defer sess.close()

	// Inputs are written in the background: with hooks, permission
	// callbacks or in-process MCP servers configured the writer holds the
	// input stream open until the run ends, which cannot happen before the
	// caller has consumed the messages.
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		_ = eng.StreamInput(ctx, func(yieldInput func(map[string]any) bool) {
			for input := range inputs {
				if !yieldInput(stampUserMessage(input.frame(), opts.VerbatimPrompts)) {
					return
				}
			}
		})
	}()
	stopped := true
	defer func() {
		// A caller that stopped early does not wait for the writer: closing
		// the engine releases one still holding the input open. Either way
		// the writer gets a moment to finish so the goroutine never outlives
		// the query.
		if stopped {
			_ = eng.Close()
		}
		select {
		case <-writerDone:
		case <-time.After(5 * time.Second):
		}
	}()

	for msg, err := range eng.messagesWithContext(ctx) {
		if !yield(msg, err) {
			return
		}
		if err != nil {
			return
		}
	}
	stopped = false
}

// sessionDeps are the process-level dependencies of opening a session.
// Tests replace them; nil means the real ones.
type sessionDeps struct {
	// newTransport builds the transport when Options.Transport is nil.
	newTransport func(*Options) Transport
	// resume drives SessionStore resume materialization.
	resume resumeEnv
}

// session is an opened, not yet started, CLI session.
type session struct {
	// opts are the effective options: prepared, and pointed at the
	// materialized config directory when there is one.
	opts *Options
	eng  *engine
	// materialized is the temp config dir of a SessionStore resume, or nil.
	// Remove it with cleanup once eng is closed.
	materialized *materializedResume
}

// close stops the engine and then removes the temp config dir of a
// SessionStore resume, which the CLI must no longer be using. It is
// idempotent.
func (s *session) close() error {
	err := s.eng.Close()
	s.materialized.cleanup()
	return err
}

// openSession runs the setup shared by Query and Client.Connect, in the order
// of the Python SDK: validate the options; materialize a SessionStore-backed
// resume into a temp CLAUDE_CONFIG_DIR (skipped with a custom Transport,
// which never sees the rewritten options); connect the transport; build the
// engine with SDK MCP servers and, with a SessionStore, transcript
// mirroring. Nothing is left behind on failure.
func openSession(ctx context.Context, raw *Options, entry string, deps *sessionDeps) (*session, error) {
	if deps == nil {
		deps = &sessionDeps{}
	}
	opts, err := prepareOptions(raw, entry)
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
	mirrorDir := projectsDir(opts.Env)
	if materialized != nil {
		opts = applyMaterializedOptions(opts, materialized)
		mirrorDir = materialized.projectsDir()
	}

	transport := opts.Transport
	if transport == nil {
		if deps.newTransport != nil {
			transport = deps.newTransport(opts)
		} else {
			transport = newSubprocessTransport(opts)
		}
	}
	if err := transport.Connect(ctx); err != nil {
		materialized.cleanup()
		return nil, err
	}

	eng := newEngine(transport, opts)
	attachSDKMCPServers(eng, opts)
	eng.enableTranscriptMirror(mirrorDir)
	return &session{opts: opts, eng: eng, materialized: materialized}, nil
}

// prepareOptions validates the options and returns a copy carrying the
// derived settings: the SDK permission handler and the entrypoint marker.
func prepareOptions(opts *Options, entry string) (*Options, error) {
	// Invalid SessionStore combinations fail before anything is spawned.
	if err := validateSessionStoreOptions(opts); err != nil {
		return nil, err
	}
	if err := validateCallbackOptions(opts); err != nil {
		return nil, err
	}
	var copied Options
	if opts != nil {
		copied = *opts
	}
	if err := validateProcessOptions(&copied); err != nil {
		return nil, err
	}
	if copied.CanUseTool != nil {
		if copied.PermissionPromptToolName != "" {
			return nil, errors.New(
				"claude: Options.CanUseTool cannot be used with Options.PermissionPromptToolName; use one or the other")
		}
		// Routes permission prompts over the control protocol.
		copied.PermissionPromptToolName = "stdio"
	}
	env := make(map[string]string, len(copied.Env)+1)
	maps.Copy(env, copied.Env)
	if _, ok := env["CLAUDE_CODE_ENTRYPOINT"]; !ok {
		env["CLAUDE_CODE_ENTRYPOINT"] = entry
	}
	copied.Env = env
	return &copied, nil
}
