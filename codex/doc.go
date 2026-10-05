// Package codex drives the Codex agent through the `codex app-server`
// protocol.
//
// The app-server speaks JSON-RPC 2.0 (with the "jsonrpc" header omitted on the
// wire) over newline-delimited JSON on the subprocess stdio. Traffic is
// bidirectional: client requests and responses, server-initiated requests such
// as approval prompts, and a stream of thread, turn, and item notifications.
//
// # Lifecycle
//
// New spawns the subprocess and performs the initialize/initialized handshake.
// StartThread (or ResumeThread, ForkThread) opens a conversation, subscribes
// to its events, and returns a *Thread handle; Client.Thread wraps a known
// thread id. Thread.Run sends user input and waits for the turn's TurnResult,
// the equivalent of upstream's thread.run; Thread.Send returns a TurnStream
// instead: range over Events for typed events or Text for the answer's text
// deltas, call Result for the collected TurnResult, Cancel to interrupt the
// turn on the server, or Close to stop reading it. SendTurn and RunTurn take
// a TurnRequest carrying per-turn options or an ExternalMessage; for
// structured output set TurnOptions.OutputSchema to SchemaFor[T]() and read
// the answer with TurnResult.DecodeStructuredOutput. Client.Close stops the
// subprocess and releases every waiting caller.
//
//	client, err := codex.New(ctx, codex.Options{})
//	if err != nil {
//		return err
//	}
//	defer client.Close()
//
//	thread, err := client.StartThread(ctx, codex.StartThreadParams{
//		ThreadSettings: codex.ThreadSettings{Cwd: "/repo", Sandbox: codex.SandboxModeWorkspaceWrite},
//	})
//	if err != nil {
//		return err
//	}
//	result, err := thread.Run(ctx, codex.Text("Run the tests"))
//	if err != nil {
//		return err
//	}
//	fmt.Println(result.Text())
//
// To stream, iterate a TurnStream (Text, or Events for every event) and then
// call Result:
//
//	turn, err := thread.Send(ctx, codex.Text("Run the tests"))
//	if err != nil {
//		return err
//	}
//	for delta, err := range turn.Text(ctx) {
//		if err != nil {
//			return err
//		}
//		fmt.Print(delta)
//	}
//	result, err := turn.Result(ctx)
//
// # Upstream mapping
//
// The package follows the official Python SDK (openai_codex), which drives the
// same app-server protocol, and borrows the TypeScript SDK's Run naming:
//
//	Codex()                         New
//	codex.thread_start(...)         Client.StartThread -> *Thread
//	thread.run(input) -> TurnResult Thread.Run, RunTurn, TurnStream.Result
//	thread.turn(input) -> handle    Thread.Send, SendTurn -> *TurnStream
//	handle.steer                    TurnStream.Steer, Thread.Steer
//	handle.interrupt                TurnStream.Cancel, Thread.Interrupt
//	ExternalMessage                 TurnRequest.External
//	ApprovalMode, Sandbox           ApprovalMode.Settings, SandboxMode
//	codex.models()                  Client.ListModels
//	retry_on_overload               RetryOnOverload, IsOverloaded
//	login_chatgpt().wait()          Client.LoginChatGPT, AwaitLogin
//
// Methods this package does not wrap are reachable through Client.Call.
//
// Unlike upstream, approval requests are declined when Options.Approvals is
// nil: upstream's default handler accepts every command and file change.
//
// # Delivery guarantees
//
// The transport reader never blocks. Turn events are handed to a per-thread
// pump that blocks only on its own thread's consumer, so a slow reader delays
// that thread alone. While it lags by a few buffers (4 × Options.EventBuffer
// notifications), streaming deltas and plan or diff updates are dropped, so
// concatenated deltas may have gaps; turn and item start and completion,
// token usage, and errors are never dropped, so the completed items and
// TurnResult stay whole. Closing a TurnStream releases its pump immediately.
//
// Thread.Events and Client.AccountUpdates are iterators: each loop
// subscribes when it starts and unsubscribes when it exits, so events that
// arrive before the loop starts are not seen. Every loop has its own bounded
// buffer (Options.EventBuffer); while it is full, newer events are dropped
// for that loop rather than stalling the connection.
//
// # Failure modes
//
// Every error type the package originates implements Error. Server error
// responses surface as *RPCError; IsOverloaded reports the
// retryable overload errors and RetryOnOverload retries them. A failed turn
// carries a *TurnError whose Kind reports the codexErrorInfo discriminator;
// Thread.Run and TurnStream.Result return it as their error. Errors the
// server is still retrying arrive as EventError events. If the subprocess
// exits, the channel from Done closes, Err reports the cause (wrapping a
// *ProcessError with the exit status and stderr tail), active turn streams
// fail with ErrClosed, Thread.Events and AccountUpdates loops end with Err,
// and later calls return ErrClosed.
//
// Unknown item types decode into UnknownItem and unmodeled turn-scoped
// notifications arrive as EventNotification events, so a newer app-server
// does not break this client.
package codex
