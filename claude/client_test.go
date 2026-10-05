package claude

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// initResponder answers the initialize handshake with the given server info.
func initResponder(ft *fakeTransport, info map[string]any) {
	ft.mu.Lock()
	ft.onWrite = func(frame map[string]any) {
		if frame["type"] != "control_request" {
			return
		}
		req, _ := frame["request"].(map[string]any)
		payload := map[string]any{}
		if req["subtype"] == "initialize" {
			payload = info
		}
		ft.push(map[string]any{"type": "control_response", "response": map[string]any{
			"subtype": "success", "request_id": frame["request_id"], "response": payload}})
	}
	ft.mu.Unlock()
}

func connectedClient(t *testing.T, opts *Options) (*Client, *fakeTransport) {
	t.Helper()
	ft := newFakeTransport()
	initResponder(ft, map[string]any{"commands": []any{"/help"}, "output_style": "default"})
	if opts == nil {
		opts = &Options{}
	}
	opts.Transport = ft
	client := NewClient(*opts)
	if err := client.Connect(t.Context()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Disconnect() })
	return client, ft
}

func TestClientConnectAndServerInfo(t *testing.T) {
	t.Parallel()
	client, ft := connectedClient(t, nil)
	if info := client.ServerInfo(); info == nil || info["output_style"] != "default" {
		t.Fatalf("server info = %#v", client.ServerInfo())
	}
	frames := ft.frames(t)
	if len(frames) != 1 || frames[0]["request"].(map[string]any)["subtype"] != "initialize" {
		t.Fatalf("frames = %#v", frames)
	}
	// The client entrypoint is reported to the CLI.
	if client.opts.Env["CLAUDE_CODE_ENTRYPOINT"] != "" {
		t.Fatal("the caller's options must not be mutated")
	}

	// Connecting twice is refused.
	if err := client.Connect(t.Context()); err == nil || !strings.Contains(err.Error(), "already connected") {
		t.Fatalf("error = %v", err)
	}
}

func TestClientSendAttributesSessions(t *testing.T) {
	t.Parallel()
	client, ft := connectedClient(t, nil)
	if _, err := client.Send(t.Context(), Text("a"), UserInput{Content: "b", SessionID: "own"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	frames := ft.frames(t)
	if len(frames) != 3 || frames[1]["type"] != "user" {
		t.Fatalf("frames = %#v", frames)
	}
	if frames[1]["session_id"] != DefaultSessionID || frames[2]["session_id"] != "own" {
		t.Fatalf("frames = %#v", frames[1:])
	}
	if _, err := client.Send(t.Context()); err == nil {
		t.Fatal("an empty Send should fail")
	}
}

// turnTexts collects the assistant texts of a turn's events.
func turnTexts(t *testing.T, turn *TurnStream) []string {
	t.Helper()
	var texts []string
	for msg, err := range turn.Events(t.Context()) {
		if err != nil {
			t.Fatalf("events: %v", err)
		}
		if am, ok := msg.(*AssistantMessage); ok {
			texts = append(texts, am.Content[0].(*TextBlock).Text)
		}
	}
	return texts
}

func TestClientTurnStreams(t *testing.T) {
	t.Parallel()
	client, ft := connectedClient(t, nil)

	first, err := client.Send(t.Context(), Text("first"))
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	ft.push(assistantFrame("one"))
	ft.push(resultFrame())
	// The second turn's messages are queued behind the first result and must
	// not be consumed by the first turn.
	ft.push(assistantFrame("two"))
	ft.push(resultFrame())

	if texts := turnTexts(t, first); len(texts) != 1 || texts[0] != "one" {
		t.Fatalf("first turn = %q", texts)
	}
	// The ended turn yields nothing more, and its result stays available.
	if texts := turnTexts(t, first); texts != nil {
		t.Fatalf("first turn again = %q", texts)
	}
	if res, err := first.Result(t.Context()); err != nil || res.SessionID != "s1" {
		t.Fatalf("result = %+v, %v", res, err)
	}

	second, err := client.Send(t.Context(), Text("second"))
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if texts := turnTexts(t, second); len(texts) != 1 || texts[0] != "two" {
		t.Fatalf("second turn = %q", texts)
	}
}

func TestClientTurnSkipsUnreadEarlierTurn(t *testing.T) {
	t.Parallel()
	client, ft := connectedClient(t, nil)
	first, err := client.Send(t.Context(), Text("first"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := client.Send(t.Context(), Text("second"))
	if err != nil {
		t.Fatal(err)
	}
	ft.push(assistantFrame("one"))
	ft.push(map[string]any{"type": "result", "subtype": "success", "is_error": false, "session_id": "s1", "result": "r1"})
	ft.push(assistantFrame("two"))
	ft.push(map[string]any{"type": "result", "subtype": "success", "is_error": false, "session_id": "s1", "result": "r2"})

	// Reading the second turn first skips the rest of the first.
	if texts := turnTexts(t, second); len(texts) != 1 || texts[0] != "two" {
		t.Fatalf("second turn = %q", texts)
	}
	if res, err := second.Result(t.Context()); err != nil || res.Text() != "r2" {
		t.Fatalf("second result = %+v, %v", res, err)
	}
	if res, err := first.Result(t.Context()); err != nil || res.Text() != "r1" {
		t.Fatalf("first result = %+v, %v", res, err)
	}

	// A closed turn fails with ErrClosed, and its unread messages are
	// skipped by the next turn.
	third, err := client.Send(t.Context(), Text("third"))
	if err != nil {
		t.Fatal(err)
	}
	ft.push(assistantFrame("three"))
	ft.push(resultFrame())
	for msg, err := range third.Events(t.Context()) {
		if err != nil || msg == nil {
			t.Fatalf("events: %v", err)
		}
		break
	}
	if err := third.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := third.Result(t.Context()); !errors.Is(err, ErrClosed) {
		t.Fatalf("result after close = %v", err)
	}
	fourth, err := client.Send(t.Context(), Text("fourth"))
	if err != nil {
		t.Fatal(err)
	}
	ft.push(assistantFrame("four"))
	ft.push(resultFrame())
	if texts := turnTexts(t, fourth); len(texts) != 1 || texts[0] != "four" {
		t.Fatalf("fourth turn = %q", texts)
	}
}

// interrupts counts the interrupt requests written to ft.
func interrupts(t *testing.T, ft *fakeTransport) int {
	t.Helper()
	n := 0
	for _, frame := range ft.frames(t) {
		if req, ok := frame["request"].(map[string]any); ok && req["subtype"] == "interrupt" {
			n++
		}
	}
	return n
}

func TestClientTurnCloseAndCancelRules(t *testing.T) {
	t.Parallel()
	client, ft := connectedClient(t, nil)

	// Close on a turn that has ended is a no-op: its result stays.
	first, err := client.Send(t.Context(), Text("first"))
	if err != nil {
		t.Fatal(err)
	}
	ft.push(map[string]any{"type": "result", "subtype": "success", "is_error": false, "session_id": "s1", "result": "r1"})
	if _, err := first.Result(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if res, err := first.Result(t.Context()); err != nil || res.Text() != "r1" {
		t.Fatalf("result after close = %+v, %v", res, err)
	}
	for _, err := range first.Events(t.Context()) {
		t.Fatalf("events after close of an ended turn: %v", err)
	}

	// A turn ended by reading a later one keeps its result after Close too.
	second, err := client.Send(t.Context(), Text("second"))
	if err != nil {
		t.Fatal(err)
	}
	third, err := client.Send(t.Context(), Text("third"))
	if err != nil {
		t.Fatal(err)
	}
	ft.push(map[string]any{"type": "result", "subtype": "success", "is_error": false, "session_id": "s1", "result": "r2"})
	ft.push(map[string]any{"type": "result", "subtype": "success", "is_error": false, "session_id": "s1", "result": "r3"})
	if res, err := third.Result(t.Context()); err != nil || res.Text() != "r3" {
		t.Fatalf("third result = %+v, %v", res, err)
	}
	_ = second.Close()
	if res, err := second.Result(t.Context()); err != nil || res.Text() != "r2" {
		t.Fatalf("second result after close = %+v, %v", res, err)
	}

	// Cancel interrupts a running turn after Close; Close never does.
	fourth, err := client.Send(t.Context(), Text("fourth"))
	if err != nil {
		t.Fatal(err)
	}
	before := interrupts(t, ft)
	_ = fourth.Close()
	if n := interrupts(t, ft); n != before {
		t.Fatalf("close sent %d interrupts", n-before)
	}
	if _, err := fourth.Result(t.Context()); !errors.Is(err, ErrClosed) {
		t.Fatalf("result of a closed, running turn = %v", err)
	}
	if err := fourth.Cancel(t.Context()); err != nil {
		t.Fatalf("cancel after close: %v", err)
	}
	if n := interrupts(t, ft); n != before+1 {
		t.Fatalf("cancel after close sent %d interrupts", n-before)
	}

	// The closed turn's messages are skipped by the next turn, which ends
	// the closed one; Cancel is then a no-op.
	fifth, err := client.Send(t.Context(), Text("fifth"))
	if err != nil {
		t.Fatal(err)
	}
	ft.push(assistantFrame("four"))
	ft.push(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true, "session_id": "s1"})
	ft.push(assistantFrame("five"))
	ft.push(resultFrame())
	if texts := turnTexts(t, fifth); len(texts) != 1 || texts[0] != "five" {
		t.Fatalf("fifth turn = %q", texts)
	}
	if err := fourth.Cancel(t.Context()); err != nil {
		t.Fatalf("cancel after end: %v", err)
	}
	if n := interrupts(t, ft); n != before+1 {
		t.Fatal("cancel after end should not interrupt")
	}
	// The error result is returned with its error, even after Close.
	var resErr *ResultError
	if res, err := fourth.Result(t.Context()); res == nil || !errors.As(err, &resErr) {
		t.Fatalf("fourth result = %+v, %v", res, err)
	}
}

func TestClientTurnErrors(t *testing.T) {
	t.Parallel()
	client, ft := connectedClient(t, nil)
	turn, err := client.Send(t.Context(), Text("hi"))
	if err != nil {
		t.Fatal(err)
	}
	if err := turn.Cancel(t.Context()); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	var interrupted bool
	for _, frame := range ft.frames(t) {
		if req, ok := frame["request"].(map[string]any); ok && req["subtype"] == "interrupt" {
			interrupted = true
		}
	}
	if !interrupted {
		t.Fatal("cancel should send an interrupt")
	}
	ft.push(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true, "session_id": "s1"})
	res, err := turn.Result(t.Context())
	var resErr *ResultError
	if res == nil || !errors.As(err, &resErr) || resErr.Subtype != "error_during_execution" {
		t.Fatalf("result = %+v, %v", res, err)
	}
	// Cancel is a no-op once the turn has ended.
	if err := turn.Cancel(t.Context()); err != nil {
		t.Fatalf("cancel after end: %v", err)
	}

	// A cancelled ctx does not end the turn: a later read resumes it.
	next, err := client.Send(t.Context(), Text("again"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := next.Result(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("result with cancelled ctx = %v", err)
	}
	ft.push(resultFrame())
	if _, err := next.Result(t.Context()); err != nil {
		t.Fatalf("result after resume = %v", err)
	}

	// A stream that ends without a result is a ConnectionError.
	last, err := client.Send(t.Context(), Text("last"))
	if err != nil {
		t.Fatal(err)
	}
	ft.finish(nil)
	var connErr *ConnectionError
	if _, err := last.Result(t.Context()); !errors.As(err, &connErr) {
		t.Fatalf("result = %v", err)
	}
}

func TestClientControlMethods(t *testing.T) {
	t.Parallel()
	client, ft := connectedClient(t, nil)
	ctx := t.Context()
	if err := client.Interrupt(ctx); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	if err := client.SetPermissionMode(ctx, PermissionModeAcceptEdits); err != nil {
		t.Fatalf("set permission mode: %v", err)
	}
	if err := client.SetModel(ctx, "sonnet"); err != nil {
		t.Fatalf("set model: %v", err)
	}
	if _, err := client.RewindFiles(ctx, "u1", nil); err != nil {
		t.Fatalf("rewind: %v", err)
	}
	if err := client.ReconnectMCPServer(ctx, "fs"); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	if err := client.ToggleMCPServer(ctx, "fs", true); err != nil {
		t.Fatalf("toggle: %v", err)
	}
	if err := client.StopTask(ctx, "t1"); err != nil {
		t.Fatalf("stop task: %v", err)
	}
	if _, err := client.MCPServerStatus(ctx); err != nil {
		t.Fatalf("mcp status: %v", err)
	}
	if _, err := client.ContextUsage(ctx, nil); err != nil {
		t.Fatalf("context usage: %v", err)
	}

	var subtypes []string
	for _, frame := range ft.frames(t) {
		if frame["type"] == "control_request" {
			subtypes = append(subtypes, frame["request"].(map[string]any)["subtype"].(string))
		}
	}
	want := []string{"initialize", "interrupt", "set_permission_mode", "set_model",
		"rewind_files", "mcp_reconnect", "mcp_toggle", "stop_task", "mcp_status", "get_context_usage"}
	if len(subtypes) != len(want) {
		t.Fatalf("subtypes = %q, want %q", subtypes, want)
	}
	for i := range want {
		if subtypes[i] != want[i] {
			t.Fatalf("subtypes = %q, want %q", subtypes, want)
		}
	}
}

func TestClientUseBeforeConnect(t *testing.T) {
	t.Parallel()
	client := NewClient(Options{})
	ctx := t.Context()
	checks := map[string]error{
		"interrupt": client.Interrupt(ctx),
		"setMode":   client.SetPermissionMode(ctx, PermissionModePlan),
		"setModel":  client.SetModel(ctx, "opus"),
		"reconnect": client.ReconnectMCPServer(ctx, "fs"),
		"toggle":    client.ToggleMCPServer(ctx, "fs", true),
		"stopTask":  client.StopTask(ctx, "t1"),
	}
	_, checks["send"] = client.Send(ctx, Text("hi"))
	_, checks["run"] = client.Run(ctx, Text("hi"))
	for name, err := range checks {
		var connErr *ConnectionError
		if !errors.As(err, &connErr) || !errors.Is(err, ErrNotConnected) {
			t.Errorf("%s: error = %T (%v), want *ConnectionError matching ErrNotConnected", name, err, err)
		}
	}
	if _, err := client.MCPServerStatus(ctx); err == nil {
		t.Error("mcp status should fail before connect")
	}
	if _, err := client.ContextUsage(ctx, nil); err == nil {
		t.Error("context usage should fail before connect")
	}
	if _, err := client.RewindFiles(ctx, "u1", nil); err == nil {
		t.Error("rewind should fail before connect")
	}
	if client.ServerInfo() != nil {
		t.Error("server info should be nil before connect")
	}
	var got error
	for _, err := range client.ReceiveMessages(ctx) {
		got = err
	}
	var connErr *ConnectionError
	if !errors.As(got, &connErr) {
		t.Errorf("receive: error = %T (%v)", got, got)
	}
}

func TestClientConcurrentControlDuringReceive(t *testing.T) {
	t.Parallel()
	client, ft := connectedClient(t, nil)

	received := make(chan struct{})
	go func() {
		for msg := range client.ReceiveMessages(t.Context()) {
			if _, ok := msg.(*ResultMessage); ok {
				close(received)
				return
			}
		}
	}()

	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = client.Interrupt(t.Context())
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("interrupt %d: %v", i, err)
		}
	}
	ft.push(resultFrame())
	select {
	case <-received:
	case <-time.After(5 * time.Second):
		t.Fatal("receiver never saw the result")
	}
}

func TestClientDisconnectIsIdempotent(t *testing.T) {
	t.Parallel()
	client, ft := connectedClient(t, nil)
	if err := client.Disconnect(); err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	ft.mu.Lock()
	closed := ft.closed
	ft.mu.Unlock()
	if !closed {
		t.Fatal("disconnect should close the transport")
	}
	if err := client.Disconnect(); err != nil {
		t.Fatalf("second disconnect: %v", err)
	}
	if _, err := client.Send(t.Context(), Text("hi")); !errors.Is(err, ErrClosed) {
		t.Fatalf("send after disconnect = %v, want ErrClosed", err)
	}
	if err := client.Interrupt(t.Context()); !errors.Is(err, ErrClosed) {
		t.Fatalf("interrupt after disconnect = %v, want ErrClosed", err)
	}
}

func TestClientReceiveHonorsContext(t *testing.T) {
	t.Parallel()
	client, _ := connectedClient(t, nil)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		var last error
		for _, err := range client.ReceiveMessages(ctx) {
			last = err
		}
		done <- last
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelling the context did not end the stream")
	}
}

func TestClientConnectFailurePropagates(t *testing.T) {
	t.Parallel()
	ft := newFakeTransport()
	ft.mu.Lock()
	ft.onWrite = func(frame map[string]any) {
		ft.push(map[string]any{"type": "control_response", "response": map[string]any{
			"subtype": "error", "request_id": frame["request_id"], "error": "handshake refused"}})
	}
	ft.mu.Unlock()
	client := NewClient(Options{Transport: ft})
	err := client.Connect(t.Context())
	var ctrlErr *ControlError
	if !errors.As(err, &ctrlErr) {
		t.Fatalf("error = %T (%v)", err, err)
	}
	// A failed connect leaves the client usable for a retry, and closes the
	// transport it opened.
	ft.mu.Lock()
	closed := ft.closed
	ft.mu.Unlock()
	if !closed {
		t.Fatal("a failed connect should close the transport")
	}
	if client.ServerInfo() != nil {
		t.Fatal("server info should be nil after a failed connect")
	}
}

func TestClientHooksAndPermissionsRoundTrip(t *testing.T) {
	t.Parallel()
	hookRan := make(chan struct{}, 1)
	opts := &Options{
		CanUseTool: func(_ context.Context, tool string, _ map[string]any, _ ToolPermissionContext) (PermissionResult, error) {
			if tool == "Bash" {
				return &PermissionResultDeny{Message: "no"}, nil
			}
			return &PermissionResultAllow{}, nil
		},
		Hooks: map[HookEvent][]HookMatcher{
			HookPreToolUse: {{Hooks: []HookCallback{
				func(context.Context, map[string]any, string, HookContext) (HookOutput, error) {
					hookRan <- struct{}{}
					return HookOutput{}, nil
				},
			}}},
		},
	}
	client, ft := connectedClient(t, opts)
	// The permission callback implies the stdio permission prompt tool.
	c := client
	if c.opts.PermissionPromptToolName != "" {
		t.Fatal("the caller's options must not be mutated")
	}

	ft.mu.Lock()
	ft.onWrite = nil
	ft.mu.Unlock()
	ft.push(map[string]any{"type": "control_request", "request_id": "p1", "request": map[string]any{
		"subtype": "can_use_tool", "tool_name": "Bash", "input": map[string]any{}, "tool_use_id": "tu1"}})
	out := ft.nextResponse(t)["response"].(map[string]any)
	if out["behavior"] != "deny" {
		t.Fatalf("permission response = %#v", out)
	}

	ft.push(map[string]any{"type": "control_request", "request_id": "h1", "request": map[string]any{
		"subtype": "hook_callback", "callback_id": "hook_0", "input": map[string]any{}}})
	if resp := ft.nextResponse(t); resp["subtype"] != "success" {
		t.Fatalf("hook response = %#v", resp)
	}
	select {
	case <-hookRan:
	case <-time.After(5 * time.Second):
		t.Fatal("hook was not invoked")
	}
}
