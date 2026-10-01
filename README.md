# gelati

Go SDKs for driving coding agents from Go programs.

| Package | What it does |
|---|---|
| [`claude`](./claude) | Claude Agent SDK for Go: drives the Claude Code CLI over its stream-json control protocol |
| [`claude/tools`](./claude/tools) | Typed inputs and outputs for Claude Code's built-in tools (Bash, Read, Edit, Agent, …) |
| [`claude/sessionstoretest`](./claude/sessionstoretest) | Conformance suite for custom `claude.SessionStore` adapters |
| [`codex`](./codex) | Client for the Codex agent over the `codex app-server` JSON-RPC protocol |

The module uses only the Go standard library.

## Requirements

- Go 1.27 or newer
- For `claude`: the [Claude Code CLI](https://docs.claude.com/en/docs/claude-code) (`claude`) on `PATH`, or `Options.CLIPath`
- For `codex`: the `codex` CLI

```sh
go get github.com/ironpark/gelati
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
for msg, err := range claude.Query(ctx, "What is 2+2?", nil) {
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

### Interactive client

```go
client := claude.NewClient(&claude.Options{
	PermissionMode: claude.PermissionModeAcceptEdits,
	Cwd:            "/path/to/repo",
})
if err := client.Connect(ctx); err != nil {
	log.Fatal(err)
}
defer client.Disconnect()

if err := client.Query(ctx, "Summarize this repository", ""); err != nil {
	log.Fatal(err)
}
for msg, err := range client.ReceiveResponse(ctx) {
	// ... same message handling as above
}
```

A connected `Client` can also interrupt a turn, change the model or permission
mode, rewind files, manage MCP servers, reload plugins and skills, apply
settings mid-session, and query commands, models, account info and context
usage. `SendControlRequest` reaches any control request without a wrapper.

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
	return claude.TextResult(strconv.Itoa(args.A + args.B)), nil
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

```go
client, err := codex.New(ctx, codex.Options{
	ClientInfo: codex.ClientInfo{Name: "my-app", Version: "0.1.0"},
	Approvals: codex.ApprovalFuncs{
		Command: func(ctx context.Context, req *codex.CommandApprovalRequest) (codex.Decision, error) {
			return codex.DecisionAccept, nil
		},
	},
})
if err != nil {
	log.Fatal(err)
}
defer client.Close()

thread, err := client.StartThread(ctx, codex.StartThreadParams{Cwd: "/repo"})
if err != nil {
	log.Fatal(err)
}
stream, err := client.StartTurn(ctx, thread.ID, codex.Text("Run the tests"), nil)
if err != nil {
	log.Fatal(err)
}
for event := range stream.Events() {
	if event.Kind == codex.EventAgentMessageDelta {
		fmt.Print(event.Delta)
	}
}
```

## Development

```sh
go vet ./...
go test -race ./...
```

Optional suites:

| Variable | Runs |
|---|---|
| `GELATI_CLAUDE_E2E=1` | End-to-end tests against the installed `claude` CLI (uses your credentials and costs a few cents) |
| `GELATI_TS_SDK=/path/to/sdk.mjs` | Session parity tests against the real TypeScript SDK runtime (needs `node`) |
| `GELATI_SDK_TOOLS_DTS=/path/to/sdk-tools.d.ts` | Checks that `claude/tools` is up to date with the given schema |
