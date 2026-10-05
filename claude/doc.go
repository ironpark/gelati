// Package claude drives the Claude Code CLI from Go.
//
// It started as a port of the official claude-agent-sdk-python and follows the
// TypeScript SDK (@anthropic-ai/claude-agent-sdk) where the two differ: the
// same wire protocol, option surface and semantics, expressed with contexts,
// iterators and interfaces instead of translated Python or TypeScript idioms.
//
// # Entry points
//
// [Query] runs a one-shot prompt and yields the messages it produces:
//
//	for msg, err := range claude.Query(ctx, "What is 2+2?", claude.Options{}) {
//		if err != nil {
//			return err
//		}
//		if am, ok := msg.(*claude.AssistantMessage); ok {
//			for _, block := range am.Content {
//				if text, ok := block.(*claude.TextBlock); ok {
//					fmt.Println(text.Text)
//				}
//			}
//		}
//	}
//
// [Run] runs the same one-shot prompt to completion and returns only the final
// [ResultMessage], whose [ResultMessage.Text] is the final text:
//
//	res, err := claude.Run(ctx, "What is 2+2?", claude.Options{})
//
// Options are passed by value; the zero Options means defaults.
//
// [Client] runs an interactive session where later turns depend on earlier
// responses, and supports interrupts and mid-conversation setters:
//
//	client := claude.NewClient(claude.Options{PermissionMode: claude.PermissionModeAcceptEdits})
//	if err := client.Connect(ctx); err != nil {
//		return err
//	}
//	defer client.Disconnect()
//	turn, err := client.Send(ctx, claude.Text("Summarize this repo"))
//	if err != nil {
//		return err
//	}
//	for msg, err := range turn.Events(ctx) {
//		...
//	}
//
// [Client.Send] returns a [TurnStream]: its Events yield the turn's messages
// up to its ResultMessage, Result waits for that, Cancel interrupts the turn
// and Close stops reading it. [Client.Run] is the shorthand of Send plus
// Result.
// [Client.Done] is closed when the session ends for any reason, and
// [Client.Err] then reports why.
//
// # Lifecycle
//
// Every blocking call takes a [context.Context]; cancelling it terminates the
// CLI subprocess and unblocks readers. A message sequence ends when the CLI's
// output ends, and a fatal error arrives as the final item of the sequence
// rather than as a panic. Breaking out of a [Query] range loop tears the
// session down; a [Client] is torn down by [Client.Disconnect], which is
// idempotent and safe to defer. [Client.Done] and [Client.Err] report a
// session that ended on its own.
//
// Client calls made before Connect fail with an error matching
// [ErrNotConnected], and after Disconnect with one matching [ErrClosed]. A CLI
// that exited with a failure status is reported as a *[ProcessError], which
// errors.As finds also through a *[ResultError].
//
// Message and content-block unions, and the option unions, are sealed
// interfaces ([Message], [ContentBlock], [PermissionResult], [MCPServerConfig],
// [ToolContent], [SystemPrompt], [ToolsConfig], [SkillsConfig]): switch on the
// concrete type rather than inspecting maps. Parsing is lenient about
// missing fields, so an older build of this package keeps working against a
// newer CLI: unknown top-level message types arrive as *[UnknownMessage], unknown system
// subtypes arrive as *[SystemMessage], and unknown content blocks arrive as
// *[UnknownBlock]. Typed messages keep the raw payload (SystemMessage.Data,
// Raw fields) for anything not modeled.
//
// # Callbacks
//
// [Options.CanUseTool] answers the permission prompts the CLI would otherwise
// show a user, [Options.Hooks] registers lifecycle hooks, [Options.OnElicitation]
// answers MCP elicitation requests, [Options.OnUserDialog] serves the dialog
// kinds listed in [Options.SupportedDialogKinds], and [NewSDKMCPServer] exposes
// in-process tools. They are served over the control protocol: the CLI writes
// a request and waits for this process to answer, so the input stream is held
// open until the run ends whenever any of them is configured. A callback that
// answers elsewhere returns [ErrRespondedOutOfBand].
//
// Hook callbacks receive the raw input map; [DecodeHookInput] turns it into
// the typed per-event struct, and [TypedHook] adapts a typed function into a
// [HookCallback]. [HookEvents] lists every event. In-process MCP servers are
// declared to the CLI in the initialize request, can change their tools at
// runtime ([MCPSDKServerConfig.Server] returns the built-in [MCPServer] behind
// a config), and can be swapped mid-session with [Client.SetMCPServers]; any
// [MCPHandler] can stand in for the built-in server.
//
// # Options
//
// [Options] reach the CLI on three channels: command-line flags, environment
// variables of the subprocess, and fields of the initialize control request
// the SDK sends once the CLI is up. Large or structured values (agents, most
// system prompts, skills lists, in-process MCP server declarations, hook
// registrations) travel in the initialize request, so they do not count
// against the OS command-line limit; a few options use two channels. A custom
// [Options.Transport] starts no subprocess, so it receives only the
// initialize-request fields.
//
// # Control
//
// Besides sending turns, a connected [Client] can steer the session:
// interrupts ([Client.InterruptWithReceipt]), permission mode and model
// setters, settings overlays ([Client.ApplyFlagSettings],
// [Client.UpdateSettings]), reloads of plugins, skills and output styles,
// file rewind, MCP server control, and queries such as
// [Client.SupportedCommands], [Client.SupportedModels], [Client.AccountInfo],
// [Client.ContextUsage] and [Client.PermissionRules]. [Client.SendControlRequest]
// reaches any control request subtype this package does not wrap.
//
// [Startup] spawns the CLI and completes the initialize handshake ahead of
// time, returning a [WarmQuery] that runs one prompt without startup latency.
// [Prewarm] (alpha) parks a spare CLI process that a later session claims.
//
// # Sessions
//
// The CLI's local session transcripts can be read and edited without
// starting a session: [ListSessions], [GetSessionInfo], [GetSessionMessages],
// [ListSubagents] and [GetSubagentMessages] read them, and [RenameSession],
// [TagSession], [DeleteSession] and [ForkSession] change them.
//
// [Options.SessionStore] mirrors every transcript line the CLI writes to
// external storage through a [SessionStore] (Append and Load), flushed per
// turn or eagerly as [Options.SessionStoreFlush] says; a batch the store
// keeps rejecting is reported as a [MirrorErrorMessage] while the session
// carries on. With a store, [Options.Resume] and
// [Options.ContinueConversation] resume from the store: the session is
// materialized into a temporary CLAUDE_CONFIG_DIR (together with the
// caller's credentials, refresh token removed, and user settings) that is
// removed when the session ends. A custom [Options.Transport] skips that
// step but still mirrors. ContinueConversation needs a [SessionLister];
// [SessionSubkeyLister], [SessionSummaryLister] and [SessionDeleter] are
// optional extensions probed with type assertions, and a call that needs a
// missing one fails with an error wrapping [errors.ErrUnsupported].
//
// The *FromStore readers ([ListSessionsFromStore] and friends) and *ViaStore
// mutations ([RenameSessionViaStore] and friends) work on a store directly,
// [ImportSessionToStore] copies a local session into one, and
// [InMemorySessionStore] is a reference implementation for tests. The
// sessionstoretest package holds a conformance suite for store adapters.
//
// # Name mapping
//
// The Go names differ from the Python and TypeScript ones where Go
// conventions differ:
//
//	query()                        -> Query, QueryStream, Run (final result only)
//	ClaudeSDKClient                -> Client
//	client.query() + receive_response() -> Client.Send, TurnStream
//	ClaudeAgentOptions             -> Options
//	create_sdk_mcp_server()        -> NewSDKMCPServer
//	createSdkMcpServer() (TS)      -> NewSDKMCPServerWithOptions, SDKMCPServerOptions
//	tool()                         -> NewTool, ToolDef
//	client.get_mcp_status()        -> Client.MCPServerStatus
//	client.get_context_usage()     -> Client.ContextUsage
//	client.get_server_info()       -> Client.ServerInfo
//	ClaudeSDKError                 -> Error
//	CLIJSONDecodeError             -> JSONDecodeError
//	CLIConnectionError             -> ConnectionError
//	SDKSessionInfo                 -> SessionInfo
//	get_session_messages()         -> GetSessionMessages
//	list_sessions_from_store()     -> ListSessionsFromStore (likewise *_from_store)
//	rename_session_via_store()     -> RenameSessionViaStore (likewise *_via_store)
//	fold_session_summary()         -> FoldSessionSummary
//	project_key_for_directory()    -> ProjectKeyForDirectory
//	import_session_to_store()      -> ImportSessionToStore
//	InMemorySessionStore()         -> NewInMemorySessionStore
//	SessionStore optional methods  -> SessionLister, SessionSummaryLister,
//	                                  SessionDeleter, SessionSubkeyLister
//	run_session_store_conformance  -> package sessionstoretest
//	load_timeout_ms                -> Options.LoadTimeout (a time.Duration)
//	startup() (TS)                 -> Startup, WarmQuery
//	prewarm() (TS)                 -> Prewarm, SpareProcess
//	query.interrupt() (TS)         -> Client.Interrupt, Client.InterruptWithReceipt
//	query.initializationResult()   -> Client.InitializationResult
//	query.usage_EXPERIMENTAL...()  -> Client.UsageExperimental
//	query.setMcpServers() (TS)     -> Client.SetMCPServers
//	spawnClaudeCodeProcess (TS)    -> Options.Spawn
//	HOOK_EVENTS (TS)               -> HookEvents
//	EXIT_REASONS (TS)              -> ExitReasons
//	includeProgrammatic (TS)       -> ListSessionsOptions.ExcludeProgrammatic
//	SYSTEM_PROMPT_DYNAMIC_BOUNDARY -> SystemPromptDynamicBoundary
//
// Python's hook output fields async_ and continue_ are [HookOutput.Async] and
// [HookOutput.Continue]; they reach the CLI under their reserved-word names.
//
// # Not ported
//
// Options.User is rejected by the subprocess transport instead of being
// silently ignored: run the process as the desired user instead.
//
// Options.Env is merged into the parent environment (as in the Python SDK)
// rather than replacing it (as in the TypeScript SDK).
//
// The TypeScript SDK's resolveSettings (alpha), which re-implements the CLI's
// settings cascade, is not ported, nor is its filterEscalatingDefaultMode
// helper. The TypeScript bridge, browser and bundling entry points are
// JavaScript-runtime specific and out of scope.
// Deprecated TypeScript options (maxThinkingTokens, setMaxThinkingTokens) are
// kept only where the Python port already had them.
package claude
