# gelati

Go SDKs for driving coding agents from Go programs.

| Package | What it does |
|---|---|
| [`claude`](./claude) | Claude Agent SDK for Go: drives the Claude Code CLI over its stream-json control protocol |
| [`claude/tools`](./claude/tools) | Typed inputs and outputs for Claude Code's built-in tools (Bash, Read, Edit, Agent, …) |
| [`claude/sessionstoretest`](./claude/sessionstoretest) | Conformance suite for custom `claude.SessionStore` adapters |
| [`codex`](./codex) | Client for the Codex agent over the `codex app-server` JSON-RPC protocol |
| [`agy`](./agy) | Port of Google's Antigravity Python SDK: drives the `localharness` agent runtime over WebSocket |
| [`agy/policy`](./agy/policy) | Tool-call policy builders (allow/deny/ask-user, workspace-only, safe defaults) |

Besides the Go standard library, the module depends only on
[`coder/websocket`](https://github.com/coder/websocket) and
[`google.golang.org/protobuf`](https://pkg.go.dev/google.golang.org/protobuf), both used by `agy`.

## Requirements

- Go 1.27 or newer
- For `claude`: the [Claude Code CLI](https://docs.claude.com/en/docs/claude-code) (`claude`) on `PATH`, or `Options.CLIPath`
- For `codex`: the `codex` CLI
- For `agy`: the `localharness` binary from the
  [`google-antigravity`](https://pypi.org/project/google-antigravity/) wheel
  (via `ANTIGRAVITY_HARNESS_PATH`, `PATH` or `Options.CLIPath`), and a
  `GEMINI_API_KEY` (or Vertex AI credentials, or a local OpenAI-compatible server)

```sh
go get github.com/ironpark/gelati
```

## Common shape

The three packages follow their upstream SDKs, so their lifecycles differ,
but the pieces around them line up:

| | `claude` | `codex` | `agy` |
|---|---|---|---|
| Create | `NewClient(Options)` | `New(ctx, Options)` (starts the process) | `NewAgent(Options)` |
| Start | `Connect(ctx)` | — | `Start(ctx)` |
| Stop | `Disconnect()` | `Close()` | `Close()` |
| Start a turn | `Client.Send(ctx, input...)` | `Client.StartTurn(ctx, threadID, input, opts)` | `Agent.Chat(ctx, content...)` |
| One-shot | `Run` / `Client.Run` → `*ResultMessage` | `Client.Run` → `*TurnResult` | `TurnStream.Result` → `*TurnResult` |
| Final text | `ResultMessage.Text()` | `TurnResult.Text()` | `TurnResult.Text()` |
| Conversation id | `SessionID` | thread ID | `ConversationID()` |
| Session end | `Done()` / `Err()` | `Done()` / `Err()` | `Done()` / `Err()` |
| Executable | `Options.CLIPath` | `Options.CLIPath` | `Options.CLIPath` |
| Extra environment | `Options.Env` | `Options.Env` | `Options.Env` |
| Diagnostics | `Options.Logger` | `Options.Logger` | `Options.Logger` |
| Errors | `Error`, `ErrCLINotFound`, `ErrNotConnected`, `ErrClosed`, `*ProcessError` | `Error`, `ErrCLINotFound`, `ErrClosed`, `*ProcessError` | `Error`, `ErrCLINotFound`, `ErrNotStarted`, `ErrClosed`, `*ProcessError` |
| Unmodeled data | `*UnknownMessage`, `*UnknownBlock` | `*UnknownItem`, `EventNotification` | — |

Options are passed by value and their zero value means defaults. A turn is a
`*TurnStream` in every package, with the same four methods:

| Method | Does |
|---|---|
| `Events(ctx)` | `iter.Seq2` over the turn's events: claude `Message`s, codex `Event`s, agy `Chunk`s |
| `Result(ctx)` | waits for the end of the turn, reading what `Events` did not, and returns its result |
| `Cancel(ctx)` | interrupts the turn |
| `Close()` | stops reading; the turn is not interrupted |

The rules are the same everywhere: `Result` returns the turn's result whenever
there is one, together with the error when the turn failed; `Close` on a turn
that already ended changes nothing; `Cancel` still interrupts a running turn
after `Close`, and does nothing once the turn has ended. A CLI killed by a
signal has a nil `ProcessError.ExitCode`.

A CLI that exits unexpectedly surfaces as a `*ProcessError` (exit code and
stderr tail) that `errors.As` finds in the returned error.

`Done` is closed once the session ends for any reason, and `Err` then reports
why (nil after an explicit stop). A nil `Logger` passes warnings and errors to
`slog.Default()` and drops debug and info records.

The child process is handled the same way everywhere:

- Without a `CLIPath`, the executable is looked up on `PATH`, then in the usual
  install locations (`agy` checks `ANTIGRAVITY_HARNESS_PATH` first). When it
  cannot be found or started, `errors.Is(err, pkg.ErrCLINotFound)` holds.
- `Env` is a map merged over this process's environment.
- The CLI runs in a process group of its own. Stopping it closes its stdin,
  waits for it to exit, then sends SIGTERM and finally SIGKILL to the whole
  group, so shells and MCP servers it started go with it. A terminal's Ctrl-C
  reaches only your program, which then stops the CLI this way.
- A panic in a callback (hook, tool, approval handler) fails that call instead
  of the process.

JSON goes through `encoding/json/v2`: raw JSON in the APIs (codex `Event.Params`,
approval `Params`, `UnknownItem.Raw`, agy `Options.ResponseSchema`, …) is a
`jsontext.Value`, and JSON decoded into your types (tool arguments, structured
output) matches field names case-sensitively. What the CLIs write is decoded
tolerantly: invalid UTF-8 and repeated keys do not fail a message.

What happens to a tool call nobody approved differs, because each package keeps
its upstream's model:

| Package | Without an approval callback |
|---|---|
| `claude` | The CLI decides from `PermissionMode`, `AllowedTools` / `DisallowedTools` and its settings; set `CanUseTool` to answer the prompts it would show a user |
| `codex` | Commands and file changes are declined, permission requests grant nothing and MCP elicitations are declined (upstream's Python SDK accepts); set `Options.Approvals` |
| `agy` | Builtin tools run except `run_command`, which is denied; set `Options.Policies` |

Each package has runnable programs under `examples/`:

```sh
go run ./claude/examples/hello_world
go run ./codex/examples/streaming
go run ./agy/examples/hooks
```

## claude

`claude` is a Go port of the official Claude Agent SDK. It started from
[claude-agent-sdk-python](https://github.com/anthropics/claude-agent-sdk-python)
and follows the [TypeScript SDK](https://github.com/anthropics/claude-agent-sdk-typescript)
(v0.3.x) where the two differ: same wire protocol, option surface and
semantics, expressed with contexts, iterators and sealed interfaces.

### One-shot query

```go
ctx := context.Background()
for msg, err := range claude.Query(ctx, "What is 2+2?", claude.Options{}) {
	if err != nil {
		log.Fatal(err)
	}
	switch m := msg.(type) {
	case *claude.AssistantMessage:
		for _, block := range m.Content {
			if text, ok := block.(*claude.TextBlock); ok {
				fmt.Print(text.Text)
			}
		}
	case *claude.ResultMessage:
		if m.TotalCostUSD != nil {
			fmt.Printf("\ncost: $%.4f\n", *m.TotalCostUSD)
		}
	}
}
```

When only the final answer matters, `Run` drains the stream and returns the
`*ResultMessage` (`Text()` is the final text); an error result, a failed exit
or a stream that ends without a result is returned as an error:

```go
res, err := claude.Run(ctx, "What is 2+2?", claude.Options{})
if err != nil {
	log.Fatal(err)
}
fmt.Println(res.Text())
```

### Interactive client

```go
client := claude.NewClient(claude.Options{
	PermissionMode: claude.PermissionModeAcceptEdits,
	Cwd:            "/path/to/repo",
})
if err := client.Connect(ctx); err != nil {
	log.Fatal(err)
}
defer client.Disconnect()

turn, err := client.Send(ctx, claude.Text("Summarize this repository"))
if err != nil {
	log.Fatal(err)
}
for msg, err := range turn.Events(ctx) {
	// ... same message handling as above
}
res, err := turn.Result(ctx)
```

`Send` may be called while an earlier turn is still running: the CLI queues
the input and answers it after the current turn. The client has one message
stream, so turns are read in the order they were sent and one `TurnStream` is
read at a time; reading a later turn first skips the earlier turn's unread
messages, while its `Result` stays available.

A connected `Client` can also interrupt a turn, change the model or permission
mode, rewind files, manage MCP servers, reload plugins and skills, apply
settings mid-session, and query commands, models, account info and context
usage. `SendControlRequest` reaches any control request without a wrapper.
`client.Run(ctx, claude.Text(prompt))` sends a turn and waits for its
`*ResultMessage`.
`Done()` is closed when the session ends for any reason (`Disconnect`, ctx
cancellation, CLI exit, transport failure) and `Err()` then reports why.

### Permissions, hooks and in-process tools

```go
type addArgs struct {
	A int `json:"a"`
	B int `json:"b"`
}

add := claude.NewTool("add", "Add two integers.", map[string]any{
	"type": "object",
	"properties": map[string]any{
		"a": map[string]any{"type": "integer"},
		"b": map[string]any{"type": "integer"},
	},
	"required": []any{"a", "b"},
}, func(ctx context.Context, args addArgs) (claude.ToolResult, error) {
	return claude.TextResult("%d", args.A+args.B), nil
})

opts := &claude.Options{
	MCPServers: map[string]claude.MCPServerConfig{
		"calc": claude.NewSDKMCPServer("calc", "1.0.0", add),
	},
	CanUseTool: func(ctx context.Context, tool string, input map[string]any,
		pc claude.ToolPermissionContext) (claude.PermissionResult, error) {
		if tool == "Bash" {
			return &claude.PermissionResultDeny{Message: "no shell access"}, nil
		}
		return &claude.PermissionResultAllow{}, nil
	},
	Hooks: map[claude.HookEvent][]claude.HookMatcher{
		claude.HookPreToolUse: {{Hooks: []claude.HookCallback{
			claude.TypedHook(func(ctx context.Context, in *claude.PreToolUseHookInput,
				toolUseID string, hc claude.HookContext) (claude.HookOutput, error) {
				log.Printf("tool call: %s", in.ToolName)
				return claude.HookOutput{}, nil
			}),
		}}},
	},
}
```

All 33 hook events are supported (`claude.HookEvents`), with typed inputs via
`DecodeHookInput` / `TypedHook`. `Options.OnElicitation` and
`Options.OnUserDialog` answer MCP elicitations and CLI dialogs.

### Sessions

Read and edit the CLI's local transcripts without starting a session:

```go
sessions, err := claude.ListSessions(&claude.ListSessionsOptions{Directory: "/path/to/repo", Limit: 10})
msgs, err := claude.GetSessionMessages(sessions[0].SessionID, nil)
err = claude.RenameSession(sessions[0].SessionID, "Refactor auth", "")
fork, err := claude.ForkSession(sessions[0].SessionID, nil)
```

`Options.SessionStore` mirrors every transcript line to external storage
(implement `claude.SessionStore`; `claude.InMemorySessionStore` is a reference
implementation) and lets `Resume` / `ContinueConversation` restore sessions
from it. `*FromStore` / `*ViaStore` functions read and edit sessions in a
store directly, and `ImportSessionToStore` uploads a local session.

### Warm start

`claude.Startup` spawns the CLI and completes the initialize handshake ahead of
time; `claude.Prewarm` (alpha) parks a spare process for a later session to
claim.

### Typed tools

```go
in, err := tools.DecodeInput(block.Name, block.Input)
switch in := in.(type) {
case tools.BashInput:
	fmt.Println("$", in.Command)
case tools.FileEditInput:
	fmt.Println("edit", in.FilePath)
}
```

The structs are generated from the TypeScript SDK's `sdk-tools.d.ts`; see
[`claude/tools/doc.go`](./claude/tools/doc.go) for regeneration.

### Examples

[`claude/examples`](./claude/examples): `hello_world` (`Run`), `streaming`
(partial messages and tool calls), `client` (a multi-turn session) and
`permissions` (`CanUseTool`, a hook and an in-process MCP tool).

### Differences from the reference SDKs

- `Options.Env` is merged into the parent environment (Python behaviour)
  rather than replacing it (TypeScript behaviour).
- `Options.User` is rejected; run the process as the desired user instead.
- The TypeScript SDK's `resolveSettings` (alpha) and its bridge, browser and
  bundling entry points are not ported.
- Deprecated TypeScript options (`maxThinkingTokens`, `setMaxThinkingTokens`)
  are kept only where the Python port had them.

The package documentation (`go doc github.com/ironpark/gelati/claude`) has the
full Python/TypeScript-to-Go name mapping.

## codex

`codex` drives the Codex agent through the `codex app-server` JSON-RPC
protocol, following the official
[Python SDK](https://github.com/openai/codex/tree/main/sdk/python) (with the
[TypeScript SDK](https://github.com/openai/codex/tree/main/sdk/typescript)'s
`run` naming). It needs the `codex` CLI on `PATH` (or `Options.CLIPath`) and a
signed-in account.

```go
client, err := codex.New(ctx, codex.Options{})
if err != nil {
	log.Fatal(err)
}
defer client.Close()

policy, reviewer := codex.ApprovalModeAutoReview.Settings()
thread, err := client.StartThread(ctx, codex.StartThreadParams{
	ThreadSettings: codex.ThreadSettings{
		Cwd:               "/repo",
		Sandbox:           codex.SandboxModeWorkspaceWrite,
		ApprovalPolicy:    policy,
		ApprovalsReviewer: reviewer,
	},
})
if err != nil {
	log.Fatal(err)
}
result, err := client.Run(ctx, thread.ID, codex.Text("Run the tests"), nil)
if err != nil {
	log.Fatal(err)
}
fmt.Println(result.Text())
```

To stream instead, `StartTurn` returns a `TurnStream`:

```go
stream, err := client.StartTurn(ctx, thread.ID, codex.Text("Run the tests"), nil)
if err != nil {
	log.Fatal(err)
}
for event, err := range stream.Events(ctx) {
	if err != nil {
		log.Fatal(err)
	}
	if event.Kind == codex.EventAgentMessageDelta {
		fmt.Print(event.Delta)
	}
}
result, err := stream.Result(ctx)
```

Approval prompts go to `Options.Approvals`; with none, commands and file
changes are declined (upstream's Python SDK accepts them). Methods the package
does not wrap are available through `Client.Call`.

[`codex/examples`](./codex/examples): `hello_world` (`Run`), `streaming`
(`StartTurn` events) and `approvals` (`ApprovalFuncs`).

## agy

`agy` is a Go port of Google's
[Antigravity Python SDK](https://pypi.org/project/google-antigravity/) (v0.1.20).
The agent loop runs in the SDK's `localharness` binary; the package launches it,
configures it, streams its steps over WebSocket, and runs custom tools, hooks,
policies and triggers in your Go process when the harness calls back.

### Requirements

The harness ships inside the platform wheel of the Python package. Extract it
once and point the SDK at it:

```sh
pip download google-antigravity==0.1.20 --no-deps --only-binary=:all: -d /tmp/ag
unzip -o /tmp/ag/google_antigravity-*.whl 'google/antigravity/bin/*' -d /tmp/ag
export ANTIGRAVITY_HARNESS_PATH=/tmp/ag/google/antigravity/bin/localharness
export GEMINI_API_KEY=...
```

Without `ANTIGRAVITY_HARNESS_PATH` (or `Options.CLIPath`), `localharness` is
looked up on `PATH`. Set `Options.Vertex` (or `GOOGLE_GENAI_USE_VERTEXAI=true`)
with a project and location or an API key to use Vertex AI instead.

### Quickstart

```go
ctx := context.Background()
agent, err := agy.NewAgent(agy.Options{
	SystemInstructions: agy.TextSystemInstructions("Answer briefly."),
})
if err != nil {
	log.Fatal(err)
}
if err := agent.Start(ctx); err != nil {
	log.Fatal(err)
}
defer agent.Close()

turn, err := agent.Chat(ctx, agy.Text("What is the capital of France?"))
if err != nil {
	log.Fatal(err)
}
res, err := turn.Result(ctx)
if err != nil {
	log.Fatal(err)
}
fmt.Println(res.Text())
```

The zero `Options` runs the default Gemini model with the default builtin tools,
`run_command` denied, and the current directory as the workspace.

### Streaming

A `TurnStream` streams one turn. Besides `Events` (every chunk), `Text`,
`Thoughts` and `ToolCalls` are focused `iter.Seq2` sequences; each is an
independent cursor, so a turn can be read several times and from several
goroutines.

```go
for thought, err := range turn.Thoughts(ctx) {
	if err != nil {
		log.Fatal(err)
	}
	fmt.Print(thought)
}
for delta, err := range turn.Text(ctx) {
	if err != nil {
		log.Fatal(err)
	}
	fmt.Print(delta)
}
res, err := turn.Result(ctx)
if err != nil {
	log.Fatal(err)
}
fmt.Println(res.StopReason, res.Usage)
```

`turn.Cancel` aborts the turn (the stream ends with `*agy.CancelledError`),
and `Options.ResponseSchema` plus `TurnResult.DecodeStructuredOutput` return
typed structured output.

### Tools

`NewTool` turns a typed Go function into a custom tool; the parameter schema
is derived from the input type (`json`, `description` and `enum` tags). Every
call gets a `ToolContext` with session-scoped state.

```go
type weatherArgs struct {
	City string `json:"city" description:"The city to look up."`
}

weather := agy.NewTool("get_weather", "Returns the weather for a city.",
	func(ctx context.Context, tc *agy.ToolContext, in weatherArgs) (string, error) {
		return "sunny in " + in.City, nil
	})

agent, err := agy.NewAgent(agy.Options{
	Tools: []*agy.Tool{weather},
	MCPServers: []agy.MCPServer{
		&agy.MCPStdioServer{Name: "fs", Command: "npx", Args: []string{"-y", "@modelcontextprotocol/server-filesystem", "."}},
	},
})
```

Tools may return `Image`, `Document`, `Audio` or `Video` values (alone or
inside slices and maps) to show media to the model. Builtin tools are chosen
with `CapabilitiesConfig.EnabledTools` / `DisabledTools` and presets such as
`ReadOnlyTools()`; static subagents are declared with `Options.Subagents`.

### Hooks and policies

Hooks are function types listed in `Options.Hooks`; policies (built with the
`agy/policy` package) decide which tool calls run, are evaluated by
the harness, and call back into Go for predicates and ask-user handlers.

```go
opts := agy.Options{
	Hooks: []agy.Hook{
		agy.PreToolCallHook(func(ctx context.Context, hc *agy.HookContext,
			call *agy.ToolCall) (agy.HookResult, error) {
			log.Printf("tool call: %s %v", call.Name, call.Args)
			return agy.HookResult{}, nil
		}),
		agy.StopHook(func(ctx context.Context, hc *agy.HookContext,
			args agy.StopArgs) (agy.StopHookResult, error) {
			if args.ContinuationCount == 0 && !strings.Contains(args.ResponseText, "DONE") {
				return agy.StopHookResult{Decision: agy.StopDecisionContinue,
					Reason: "Finish the task and end with DONE."}, nil
			}
			return agy.StopHookResult{}, nil
		}),
	},
	Policies: []agy.Policy{
		policy.DenyAll(),
		policy.Allow("view_file"),
		policy.AskUser("run_command", policy.Handler(confirm)),
	},
	Triggers: []agy.Trigger{
		agy.Every(time.Hour, func(ctx context.Context, tc *agy.TriggerContext) error {
			return tc.Send(ctx, "Hourly check: summarize anything new in the workspace.")
		}),
	},
}
```

### Local models

`Options.OpenAI` runs the session on an OpenAI-compatible server such as Ollama
or LM Studio, with no Gemini credentials (upstream `LocalOpenAIAgentConfig`):

```go
opts := agy.Options{
	Model:  "gemma3",
	OpenAI: &agy.OpenAIEndpoint{BaseURL: "http://localhost:11434/v1"},
}.Lightweight()
```

### Examples

[`agy/examples`](./agy/examples) ports upstream's
getting-started examples as runnable programs: `hello_world`, `streaming`,
`custom_tools`, `hooks`, `policies`, `structured_output`, `mcp_tools`,
`subagents`, `persistence`, `multimodal`, `triggers`, `cancellation`, `vertex`
and `local_models`.

```sh
go run ./agy/examples/hello_world
```

### Differences from upstream

- Configuration is one `Options` struct (upstream `LocalAgentConfig`,
  `LocalOpenAIAgentConfig`); `Lightweight` and `Eval` return modified copies.
  Where Python distinguishes unset from empty, nil and empty slices differ, and
  options whose upstream default is true are inverted (`DisableSubagents`).
- Hooks are typed function values rather than decorated functions; every hook
  receives a `HookContext`. `HookResult{Deny: true}` replaces `allow=False`.
- Errors are `*ValidationError`, `*ExecutionError`, `*ConnectionError` and
  `*CancelledError` (which also matches `context.Canceled`).
- `OnFileChange` polls instead of using `watchfiles`; `Connection.Close` waits
  at most a minute for session end hooks.
- Not ported: the LiteRT backend (it needs upstream's Python LiteRT-LM
  server), OpenTelemetry instrumentation, and the interactive terminal helpers.
  `DebugConfig` is replaced by `Options.Logger`.

The package documentation (`go doc github.com/ironpark/gelati/agy`)
has the full Python-to-Go name mapping.

## Development

```sh
go vet ./...
go test -race ./...
```

`agy`'s wire messages are generated from the upstream `.proto` definitions
(`agy/internal/wire/proto`) with [buf](https://buf.build) and `protoc-gen-go`,
which is pinned as a tool in `go.mod`:

```sh
go generate ./agy/internal/wire   # runs buf generate
```

Optional suites:

| Variable | Runs |
|---|---|
| `GELATI_CLAUDE_E2E=1` | End-to-end tests against the installed `claude` CLI (uses your credentials and costs a few cents) |
| `GELATI_TS_SDK=/path/to/sdk.mjs` | Session parity tests against the real TypeScript SDK runtime (needs `node`) |
| `GELATI_SDK_TOOLS_DTS=/path/to/sdk-tools.d.ts` | Checks that `claude/tools` is up to date with the given schema |
| `GELATI_CODEX_E2E=1` | End-to-end test against the installed `codex` CLI (needs a signed-in account; spends a few tokens) |
| `GELATI_AGY_HARNESS=/path/to/localharness` | Integration tests against the real Antigravity harness, with no credentials (an invalid API key and a fake OpenAI-compatible server) |
| `GELATI_AGY_E2E=1` | End-to-end test against Gemini through the real harness (needs `GEMINI_API_KEY` and the harness on `ANTIGRAVITY_HARNESS_PATH` or `PATH`; costs a little) |
