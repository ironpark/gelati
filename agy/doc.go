// Package agy builds AI agents on Google's Antigravity agent runtime
// from Go.
//
// It is a port of the google-antigravity Python SDK (v0.1.20). The agent
// loop runs in the localharness binary that ships with that SDK; this
// package launches it, configures it, streams its steps, and runs custom
// tools, hooks, policies and triggers in the Go process when the harness
// calls back. The harness is located through Options.CLIPath, the
// ANTIGRAVITY_HARNESS_PATH environment variable, or localharness on PATH.
//
// # Quick start
//
//	agent, err := agy.New(ctx, agy.Options{
//		SystemInstructions: agy.TextSystemInstructions("Answer briefly."),
//	})
//	if err != nil {
//		return err
//	}
//	defer agent.Close()
//
//	stream, err := agent.Chat(ctx, agy.Text("What is the capital of France?"))
//	if err != nil {
//		return err
//	}
//	for delta, err := range stream.Text(ctx) {
//		if err != nil {
//			return err
//		}
//		fmt.Print(delta)
//	}
//
// The zero Options runs DefaultModel on the Gemini API with the key from
// GEMINI_API_KEY (or Options.APIKey), with the default builtin tools enabled
// and run_command denied. Set Vertex (or GOOGLE_GENAI_USE_VERTEXAI) to use
// Vertex AI instead. Set OpenAI to run on a local OpenAI-compatible
// server (Ollama, LM Studio, vLLM) without Gemini credentials:
//
//	opts := agy.Options{
//		Model:  "gemma3",
//		OpenAI: &agy.OpenAIEndpoint{BaseURL: "http://localhost:11434/v1"},
//	}.Lightweight()
//
// Runnable versions of upstream's getting-started examples live under
// agy/examples.
//
// # Layers
//
// Agent is the high-level API: New starts a session and Close ends it
// (Python's "async with"), and Chat sends a prompt. Agent.Conversation
// exposes the Conversation underneath, which keeps the step history, turn
// count, compaction indices and per-turn usage; Conversation.Connection
// exposes the Connection to the harness for direct step access. Each layer has Done,
// closed when the session ends (Close, the harness exiting or the
// connection dropping), and Err, the error that ended it.
//
// A TurnStream streams one turn. Events (every chunk), Text, Thoughts and
// ToolCalls are iter.Seq2 sequences; each is an independent cursor over a
// shared buffer, so a turn can be read several times and from several
// goroutines. Result waits for the turn to end and returns a TurnResult:
// the chunks, the full text (TurnResult.Text), the structured output, the
// stop reason and the token usage. Cancel halts the turn; Close stops
// reading it and gives up the connection's step reader without cancelling
// it. Conversation.Send and ReceiveSteps are the lower-level, step-based
// alternative to Chat.
//
// # Tools, hooks, policies and triggers
//
// NewTool turns a typed Go function into a custom tool; its parameter
// schema is derived from the input type by reflection (json tags,
// `description` and `enum` tags). NewToolWithSchema takes an explicit
// schema. Every tool call receives a ToolContext with a session-scoped
// StateStore.
//
// Hooks are function types (PreToolCallHook, PostTurnHook, StopHook, ...)
// listed in Options.Hooks. Policies (see the policy subpackage) decide which
// tool calls run; they are evaluated by the harness, calling back into this
// process for predicates and ask-user handlers. Triggers (Every,
// OnFileChange) run alongside the session and push messages to the agent.
//
// # Errors
//
// Errors this package originates implement Error. Turn failures end a
// turn stream with *ExecutionError (the agent loop failed),
// *ConnectionError (a fatal HTTP 400/401/403 model error, or the harness
// went away) or *CancelledError (after Cancel; it matches
// context.Canceled). Invalid configuration and input yield
// *ValidationError. A session that ends on its own (the harness exited or
// the connection dropped) closes Done, and Err then returns a
// *ConnectionError carrying the harness's stderr tail; Err is nil after a
// Close. When the harness process exited, during New or mid-session, the
// *ConnectionError wraps a *ProcessError with its exit status, so
// errors.As(err, &processErr) tells a crash from other connection
// failures. ErrClosed reports a closed Agent, Connection or TurnStream.
//
// # Name mapping
//
// The Go names differ from the Python ones where Go conventions differ:
//
//	Agent(config), async with          -> New, Agent.Close
//	agent.chat(prompt)                 -> Agent.Chat(ctx, content...)
//	LocalAgentConfig, AgentConfig      -> Options
//	LocalOpenAIAgentConfig(model, base_url) -> Options{Model: model, OpenAI: &OpenAIEndpoint{BaseURL: base_url}}
//	config.lightweight(), .eval()      -> Options.Lightweight, Options.Eval
//	str prompt, Content sequence       -> Text, ...Content
//	SlashCommand(name=PLAN)            -> SlashCommandPlan
//	from_file, from_bytes              -> FromFile, FromBytes (and ImageFromFile, ...)
//	system_instructions="..."          -> TextSystemInstructions("...")
//	BuiltinTools.READ_ONLY ...         -> BuiltinViewFile ..., ReadOnlyTools() ...
//	CapabilitiesConfig.enable_subagents -> CapabilitiesConfig.DisableSubagents (inverted)
//	RunCommandConfig.timeout_seconds   -> RunCommandConfig.Timeout (time.Duration)
//	RetryConfig.benchmark()            -> BenchmarkRetryConfig
//	McpStdioServer                     -> MCPStdioServer
//	McpStreamableHttpServer            -> MCPStreamableHTTPServer
//	ChatResponse                       -> TurnStream
//	ChatResponse.__aiter__             -> TurnStream.Text
//	ChatResponse.chunks/.thoughts      -> TurnStream.Events, TurnStream.Thoughts
//	ChatResponse.text(), .resolve()    -> TurnStream.Result, then TurnResult.Text, TurnResult.Chunks
//	ChatResponse.structured_output()   -> TurnResult.StructuredOutput, TurnResult.DecodeStructuredOutput
//	ChatResponse.usage_metadata, .stop_reason -> TurnResult.Usage, TurnResult.StopReason
//	ChatResponse.cancel()              -> TurnStream.Cancel
//	Conversation.receive_chunks()      -> Conversation.Chat, then TurnStream.Events
//	Text, Thought (stream chunks)      -> TextChunk, ThoughtChunk
//	UsageMetadata +, -, *, sum()       -> UsageMetadata.Add, Sub, Scale, SumUsage
//	HookResult(allow=False)            -> HookResult{Deny: true}
//	@pre_tool_call_decide etc.         -> PreToolCallHook(...) etc. in Options.Hooks
//	PreToolCallDecideHook              -> PreToolCallHook
//	HookContext/SessionContext/...     -> HookContext with HookContext.Scope
//	ToolWithSchema(fn, schema)         -> NewToolWithSchema
//	ToolRunner, HookRunner             -> unexported; configure through Options
//	StateStore.get_state/set_state     -> StateStore.GetState, SetState, UpdateState
//	with ctx: (state lock)             -> StateStore.Atomically
//	policy.allow/deny/ask_user(...)    -> policy.Allow/Deny/AskUser (MCP: AllowMCP, ...)
//	policy.allow_all(), deny_all()     -> policy.AllowAll(), policy.DenyAll() (AllowAllPolicy)
//	confirm_run_command, workspace_only -> policy.ConfirmRunCommand, WorkspaceOnly (ConfirmRunCommandPolicies, WorkspaceOnlyPolicies)
//	policy.enforce(...)                -> policy.Enforce, EnforcePolicies
//	triggers.every, on_file_change     -> Every, OnFileChange
//	AntigravityConnectionError         -> ConnectionError
//	AntigravityExecutionError          -> ExecutionError
//	AntigravityCancelledError          -> CancelledError
//	AntigravityValidationError, ValueError -> ValidationError
//	RuntimeError("Concurrent receive_steps()") -> ErrConcurrentReceive
//
// Upstream raises ValueError from many validators; here they return
// *ValidationError: from Options.Validate, and so New, for configuration and
// the safety policy guard, and from New for endpoint credentials.
//
// # Differences from upstream
//
// Where Python distinguishes "unset" from "empty", nil and empty slices
// differ (Options.Policies, Options.Workspaces, EnabledTools, DisabledTools).
// Boolean options whose upstream default is true are inverted
// (DisableSubagents). Lightweight and Eval therefore cannot tell an
// explicit false from an unset option and always apply their preset there.
//
// Upstream recognizes the allow_all and workspace_only policies by name;
// here only AllowAllPolicy and WorkspaceOnlyPolicies (and the policy
// package wrappers) build them, so a policy merely named like them is an
// ordinary rule.
//
// Conversation.Send drains a still-running previous turn like upstream,
// but does not report that turn's errors (they stay with its
// TurnStream), and waits for the agent to be idle before sending, so the
// previous turn's final events cannot be mistaken for the new turn's.
//
// OpenAIEndpoint is also a ModelEndpoint, so an OpenAI-compatible model
// can be one ModelTarget among Gemini ones; upstream only offers it as the
// whole-session LocalOpenAIAgentConfig.
//
// Connection.Close waits at most a minute for the session end hooks and
// skips them when the harness is already gone; upstream waits
// indefinitely. OnFileChange polls instead of using watchfiles.
//
// # Not ported
//
// The LiteRT backend (LiteRTAgentConfig, LiteRTBackend, litert_server.py)
// is not supported: it runs the model in a Python LiteRT-LM server that
// the SDK spawns next to the harness, which has no Go equivalent. Point
// Options.OpenAI at any OpenAI-compatible local server instead. If it is
// ported, connectLocal (strategy.go) is the seam it plugs into.
//
// OpenTelemetry instrumentation (utils/otel) and the interactive terminal
// helpers (utils/interactive, such as AskQuestionHook and the ask-user
// handler) are not ported; OnInteractionHook and policy.Handler take their
// place. DebugConfig is replaced by Options.Logger. Tool results containing
// genai Content protos have no Go equivalent; media results use Image,
// Document, Audio and Video instead.
package agy
