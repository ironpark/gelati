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
//	client, err := claude.New(ctx, claude.Options{PermissionMode: claude.PermissionModeAcceptEdits})
//	if err != nil {
//		return err
//	}
//	defer client.Close()
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
// Every blocking call takes a [context.Context]. The ctx of a [Query] or
// [Run] governs the whole call: cancelling it terminates the CLI subprocess
// and unblocks readers. [New] starts the session and returns a live
// [Client]; its ctx bounds the startup only, and the session lasts until
// [Client.Close], which is idempotent and safe to defer. The ctx of each
// later Client call bounds that call. A message sequence ends when the CLI's
// output ends, and a fatal error arrives as the final item of the sequence
// rather than as a panic. Breaking out of a [Query] range loop tears the
// session down. [Client.Done] and [Client.Err] report a session that ended
// on its own.
//
// Client calls made after Close fail with an error matching [ErrClosed]. A
// CLI that exited with a failure status is reported as a *[ProcessError],
// which errors.As finds also through a *[ResultError].
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
// Besides sending turns, a [Client] can steer the session:
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
// Package [github.com/ironpark/gelati/claude/sessions] reads and edits
// session transcripts without starting a session: the CLI's local ones
// ([sessions.List], [sessions.GetMessages], [sessions.Fork] and friends)
// and copies held in a [sessions.Store] (the *InStore variants).
//
// [Options.SessionStore] mirrors every transcript line the CLI writes to
// external storage through a [sessions.Store] (Append and Load), flushed per
// turn or eagerly as [Options.SessionStoreFlush] says; a batch the store
// keeps rejecting is reported as a [MirrorErrorMessage] while the session
// carries on. With a store, [Options.Resume] and
// [Options.ContinueConversation] resume from the store: the session is
// materialized into a temporary CLAUDE_CONFIG_DIR (together with the
// caller's credentials, refresh token removed, and user settings) that is
// removed when the session ends. A custom [Options.Transport] skips that
// step but still mirrors. ContinueConversation needs a [sessions.Lister];
// [sessions.SubkeyLister] is used, when present, to materialize subagent
// transcripts too.
//
// # Name mapping
//
// The Go names differ from the Python and TypeScript ones where Go
// conventions differ:
//
//	query()                        -> Query, QueryStream, Run (final result only)
//	ClaudeSDKClient                -> Client
//	client.connect(), async with   -> New (returns a live Client)
//	client.disconnect()            -> Client.Close
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
//	list_sessions()                -> sessions.List
//	get_session_info()             -> sessions.GetInfo
//	get_session_messages()         -> sessions.GetMessages
//	list_subagents()               -> sessions.ListSubagents
//	get_subagent_messages()        -> sessions.GetSubagentMessages
//	rename_session(), tag_session() -> sessions.Rename, sessions.Tag
//	delete_session(), fork_session() -> sessions.Delete, sessions.Fork
//	*_from_store(), *_via_store()  -> sessions.*InStore (e.g. ListInStore,
//	                                  RenameInStore)
//	SDKSessionInfo                 -> sessions.Info
//	SessionMessage                 -> sessions.Message
//	SessionKey                     -> sessions.Key
//	SessionStore                   -> sessions.Store
//	SessionStore optional methods  -> sessions.Lister, SummaryLister,
//	                                  Deleter, SubkeyLister
//	fold_session_summary()         -> sessions.FoldSummary
//	project_key_for_directory()    -> sessions.ProjectKey
//	import_session_to_store()      -> sessions.ImportToStore
//	InMemorySessionStore()         -> sessions.NewInMemoryStore
//	run_session_store_conformance  -> package sessions/sessionstoretest
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
//	includeProgrammatic (TS)       -> sessions.ListOptions.ExcludeProgrammatic
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
