package claude

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// Regression tests for shutdown and concurrency hangs. Each one fails by
// timing out against the bug it covers.

// finishesWithin runs fn and fails the test if it has not returned by d.
func finishesWithin(t *testing.T, d time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not return within %v", what, d)
	}
}

// stuckProcess is a SpawnedProcess that never reads its stdin, so writes
// block once the pipe is full, and exits only when signalled or killed.
type stuckProcess struct {
	stdinR  *io.PipeReader
	stdinW  *io.PipeWriter
	stdoutR *io.PipeReader
	stdoutW *io.PipeWriter
	done    chan struct{}
	once    sync.Once
}

func newStuckProcess() *stuckProcess {
	p := &stuckProcess{done: make(chan struct{})}
	p.stdinR, p.stdinW = io.Pipe()
	p.stdoutR, p.stdoutW = io.Pipe()
	return p
}

func (p *stuckProcess) Stdin() io.WriteCloser { return p.stdinW }
func (p *stuckProcess) Stdout() io.Reader     { return p.stdoutR }
func (p *stuckProcess) Stderr() io.Reader     { return nil }
func (p *stuckProcess) Wait() error           { <-p.done; return nil }
func (p *stuckProcess) Signal(os.Signal) error {
	p.stop()
	return nil
}
func (p *stuckProcess) Kill() error {
	p.stop()
	return nil
}

func (p *stuckProcess) stop() {
	p.once.Do(func() {
		close(p.done)
		_ = p.stdoutW.Close()
	})
}

// A write blocked on a CLI that stopped reading stdin must not keep Close
// from shutting the process down.
func TestSubprocessCloseUnblocksStuckWrite(t *testing.T) {
	t.Parallel()
	proc := newStuckProcess()
	tr := newSubprocessTransport(&Options{
		Spawn: func(context.Context, SpawnOptions) (SpawnedProcess, error) { return proc, nil },
	})
	tr.gracefulTimeout = 50 * time.Millisecond
	tr.killTimeout = 50 * time.Millisecond
	if err := tr.Connect(t.Context()); err != nil {
		t.Fatalf("connect: %v", err)
	}

	writeErr := make(chan error, 1)
	go func() { writeErr <- tr.Write(context.Background(), []byte(`{"type":"user"}`)) }()
	// Give the write time to block on the unread pipe.
	time.Sleep(50 * time.Millisecond)

	finishesWithin(t, 3*time.Second, "Close", func() { _ = tr.Close() })
	select {
	case err := <-writeErr:
		if err == nil {
			t.Fatal("the stuck write reported success")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the stuck write never returned")
	}
}

// A control request still waiting when the CLI's output ends cleanly fails
// instead of waiting forever.
func TestEngineControlRequestEndsWithOutput(t *testing.T) {
	t.Parallel()
	eng, ft := startEngine(t, nil)
	errc := make(chan error, 1)
	go func() {
		_, err := eng.sendControlRequest(context.Background(), map[string]any{"subtype": "interrupt"})
		errc <- err
	}()
	ft.nextWrite(t) // the request is on the wire
	ft.finish(nil)  // the CLI exits cleanly without answering

	select {
	case err := <-errc:
		var connErr *ConnectionError
		if !errors.As(err, &connErr) {
			t.Fatalf("error = %T (%v), want *ConnectionError", err, err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the control request hung after the output ended")
	}
}

// Closing the engine cancels callbacks still serving CLI requests, so a
// permission prompt waiting on its context does not block teardown.
func TestEngineCloseCancelsInflightHandlers(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	cancelled := make(chan struct{})
	opts := &Options{
		CanUseTool: func(ctx context.Context, _ string, _ map[string]any, _ ToolPermissionContext) (PermissionResult, error) {
			close(started)
			<-ctx.Done()
			close(cancelled)
			return nil, ctx.Err()
		},
	}
	eng, ft := startEngine(t, opts)
	ft.push(map[string]any{"type": "control_request", "request_id": "p1", "request": map[string]any{
		"subtype": "can_use_tool", "tool_name": "Bash", "input": map[string]any{}, "tool_use_id": "tu1"}})
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("the permission callback was never called")
	}

	finishesWithin(t, 3*time.Second, "engine Close", func() { _ = eng.close() })
	select {
	case <-cancelled:
	default:
		t.Fatal("the callback's context was not cancelled")
	}
}

// Two concurrent Connect calls on one Client start one session; the second
// is refused instead of leaking a CLI process.
func TestClientConcurrentConnect(t *testing.T) {
	t.Parallel()
	ft := newFakeTransport()
	client := NewClient(&Options{Transport: ft})
	t.Cleanup(func() { _ = client.Disconnect() })

	first := make(chan error, 1)
	go func() { first <- client.Connect(t.Context()) }()
	// The first Connect is mid-handshake: its initialize request is out
	// and unanswered.
	init := ft.nextWrite(t)

	var err error
	finishesWithin(t, 3*time.Second, "second Connect", func() { err = client.Connect(t.Context()) })
	if err == nil || !strings.Contains(err.Error(), "already connected") {
		t.Fatalf("second Connect error = %v, want already connected", err)
	}

	ft.push(map[string]any{"type": "control_response", "response": map[string]any{
		"subtype": "success", "request_id": init["request_id"], "response": map[string]any{}}})
	select {
	case err := <-first:
		if err != nil {
			t.Fatalf("first Connect: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the first Connect never finished")
	}
}

// Cancelling ctx ends a Query even when the transport keeps its output open.
func TestQueryEndsOnContextCancel(t *testing.T) {
	t.Parallel()
	ft := newFakeTransport()
	initResponder(ft, map[string]any{})
	ack := ft.onWrite
	ft.onWrite = func(frame map[string]any) {
		ack(frame)
		if frame["type"] == "user" {
			ft.push(map[string]any{"type": "system", "subtype": "status", "status": "working"})
		}
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var lastErr error
	finishesWithin(t, 3*time.Second, "Query after cancel", func() {
		for msg, err := range Query(ctx, "hi", &Options{Transport: ft}) {
			if err != nil {
				lastErr = err
				break
			}
			if msg != nil {
				// The CLI is still running; the caller gives up.
				cancel()
			}
		}
	})
	if !errors.Is(lastErr, context.Canceled) {
		t.Fatalf("Query ended with %v, want context.Canceled", lastErr)
	}
}

// blockingConnector is an MCP server whose disconnect func blocks until
// released.
type blockingConnector struct {
	onDisconnect func()
}

func (blockingConnector) HandleMCPMessage(context.Context, json.RawMessage) (json.RawMessage, error) {
	return nil, nil
}

func (c blockingConnector) ConnectMCP(MCPSendFunc) func() { return c.onDisconnect }

// A connector's disconnect func runs outside the session's Close, so a slow
// one does not hold up Disconnect.
func TestClientDisconnectWithBlockingConnector(t *testing.T) {
	t.Parallel()
	ft := newFakeTransport()
	initResponder(ft, map[string]any{})
	release := make(chan struct{})
	disconnected := make(chan struct{})
	conn := blockingConnector{onDisconnect: func() {
		<-release
		close(disconnected)
	}}
	client := NewClient(&Options{
		Transport:  ft,
		MCPServers: map[string]MCPServerConfig{"slow": &MCPSDKServerConfig{Name: "slow", Instance: conn}},
	})
	if err := client.Connect(t.Context()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	finishesWithin(t, 3*time.Second, "Disconnect", func() { _ = client.Disconnect() })
	close(release)
	select {
	case <-disconnected:
	case <-time.After(3 * time.Second):
		t.Fatal("the connector's disconnect func never ran")
	}
}

// Wrong-typed optional members of a can_use_tool request stay unset rather
// than turning into zero-value pointers, and malformed suggestions are
// dropped instead of being offered back to the caller.
func TestToolPermissionContextWrongTypes(t *testing.T) {
	t.Parallel()
	ctx := toolPermissionContext("r1", map[string]any{
		"tool_use_id":               "tu1",
		"mcp_server":                "not an object",
		"matched_ask_rule":          []any{},
		"requires_user_interaction": "yes",
		"permission_suggestions": []any{
			"x", nil, 5,
			map[string]any{"type": "addRules", "rules": "bad"},
			map[string]any{"type": "setMode", "mode": "acceptEdits", "destination": "session"},
		},
	})
	if ctx.MCPServer != nil || ctx.MatchedAskRule != nil || ctx.RequiresUserInteraction != nil {
		t.Errorf("wrong-typed members were set: %+v", ctx)
	}
	if len(ctx.Suggestions) != 1 || ctx.Suggestions[0].Mode != PermissionModeAcceptEdits {
		t.Errorf("suggestions = %+v, want only the well-formed setMode", ctx.Suggestions)
	}
	rule := toolPermissionContext("r2", map[string]any{
		"matched_ask_rule": map[string]any{"source": "userSettings", "tool_name": "Bash", "rule_content": 7},
	}).MatchedAskRule
	if rule == nil || rule.ToolName != "Bash" || rule.RuleContent != nil {
		t.Errorf("matched rule = %+v, want Bash with no rule content", rule)
	}
}

// A rate-limit reset time that is not a number is left nil, not 0.
func TestRateLimitResetsAtWrongType(t *testing.T) {
	t.Parallel()
	msg, err := ParseMessage([]byte(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":"soon","overageResetsAt":true}}`))
	if err != nil {
		t.Fatal(err)
	}
	info := msg.(*RateLimitEvent).RateLimitInfo
	if info.ResetsAt != nil || info.OverageResetsAt != nil {
		t.Errorf("ResetsAt = %v, OverageResetsAt = %v, want nil", info.ResetsAt, info.OverageResetsAt)
	}
}
