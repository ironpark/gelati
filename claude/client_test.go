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

// testClient starts a client on a fake transport that respond sets up to
// answer the CLI side; nil answers control requests with a default
// initialize response.
func testClient(t *testing.T, opts *Options, respond func(*fakeTransport)) (*Client, *fakeTransport) {
	t.Helper()
	ft := newFakeTransport()
	if respond == nil {
		initResponder(ft, map[string]any{"commands": []any{"/help"}, "output_style": "default"})
	} else {
		respond(ft)
	}
	if opts == nil {
		opts = &Options{}
	}
	opts.Transport = ft
	client, err := New(t.Context(), *opts)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client, ft
}

func TestNewAndServerInfo(t *testing.T) {
	t.Parallel()
	opts := &Options{Env: map[string]string{"A": "b"}}
	client, ft := testClient(t, opts, nil)
	if info := client.ServerInfo(); info == nil || info["output_style"] != "default" {
		t.Fatalf("server info = %#v", client.ServerInfo())
	}
	frames := ft.frames(t)
	if len(frames) != 1 || frames[0]["request"].(map[string]any)["subtype"] != "initialize" {
		t.Fatalf("frames = %#v", frames)
	}
	// The client entrypoint is reported to the CLI.
	if _, ok := opts.Env["CLAUDE_CODE_ENTRYPOINT"]; ok {
		t.Fatal("the caller's options must not be mutated")
	}
}

func TestClientSendAttributesSessions(t *testing.T) {
	t.Parallel()
	client, ft := testClient(t, nil, nil)
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
	client, ft := testClient(t, nil, nil)

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
	client, ft := testClient(t, nil, nil)
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
	client, ft := testClient(t, nil, nil)

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
	client, ft := testClient(t, nil, nil)
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
	client, ft := testClient(t, nil, nil)
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

func TestClientUseAfterClose(t *testing.T) {
	t.Parallel()
	client, _ := testClient(t, nil, nil)
	if err := client.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
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
	_, checks["mcpStatus"] = client.MCPServerStatus(ctx)
	_, checks["contextUsage"] = client.ContextUsage(ctx, nil)
	_, checks["rewind"] = client.RewindFiles(ctx, "u1", nil)
	_, checks["reinitialize"] = client.Reinitialize(ctx)
	_, checks["setMCPServers"] = client.SetMCPServers(ctx, nil)
	for _, err := range client.ReceiveMessages(ctx) {
		checks["receive"] = err
	}
	for name, err := range checks {
		var connErr *ConnectionError
		if !errors.As(err, &connErr) || !errors.Is(err, ErrClosed) {
			t.Errorf("%s: error = %T (%v), want *ConnectionError matching ErrClosed", name, err, err)
		}
	}
	// What the handshake reported stays readable.
	if client.ServerInfo() == nil {
		t.Error("server info should survive Close")
	}
}

func TestClientConcurrentControlDuringReceive(t *testing.T) {
	t.Parallel()
	client, ft := testClient(t, nil, nil)

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

func TestClientCloseIsIdempotent(t *testing.T) {
	t.Parallel()
	client, ft := testClient(t, nil, nil)
	if err := client.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if !isDone(ft.closedCh) {
		t.Fatal("Close should close the transport")
	}
	if err := client.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if _, err := client.Send(t.Context(), Text("hi")); !errors.Is(err, ErrClosed) {
		t.Fatalf("send after close = %v, want ErrClosed", err)
	}
	if err := client.Interrupt(t.Context()); !errors.Is(err, ErrClosed) {
		t.Fatalf("interrupt after close = %v, want ErrClosed", err)
	}
}

func TestClientReceiveHonorsContext(t *testing.T) {
	t.Parallel()
	client, _ := testClient(t, nil, nil)
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

func TestNewFailurePropagates(t *testing.T) {
	t.Parallel()
	ft := newFakeTransport()
	ft.mu.Lock()
	ft.onWrite = func(frame map[string]any) {
		ft.push(map[string]any{"type": "control_response", "response": map[string]any{
			"subtype": "error", "request_id": frame["request_id"], "error": "handshake refused"}})
	}
	ft.mu.Unlock()
	client, err := New(t.Context(), Options{Transport: ft})
	var ctrlErr *ControlError
	if !errors.As(err, &ctrlErr) {
		t.Fatalf("error = %T (%v)", err, err)
	}
	if client != nil {
		t.Fatal("a failed New should return no client")
	}
	// A failed New closes the transport it opened.
	if !isDone(ft.closedCh) {
		t.Fatal("a failed New should close the transport")
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
	_, ft := testClient(t, opts, nil)
	// The permission callback implies the stdio permission prompt tool.
	if opts.PermissionPromptToolName != "" {
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

// collectText ranges over turn.Text and returns the texts and the error it
// ended with.
func collectText(ctx context.Context, turn *TurnStream) ([]string, error) {
	var texts []string
	for text, err := range turn.Text(ctx) {
		if err != nil {
			return texts, err
		}
		texts = append(texts, text)
	}
	return texts, nil
}

// streamFrame is a stream_event frame carrying event, from the subagent
// parent names when it is not empty.
func streamFrame(event map[string]any, parent string) map[string]any {
	frame := map[string]any{"type": "stream_event", "uuid": "u", "session_id": "s1", "event": event}
	if parent != "" {
		frame["parent_tool_use_id"] = parent
	}
	return frame
}

// deltaFrame is a stream_event frame carrying a text delta.
func deltaFrame(text, parent string) map[string]any {
	return streamFrame(map[string]any{"type": "content_block_delta", "index": 0,
		"delta": map[string]any{"type": "text_delta", "text": text}}, parent)
}

// messageStartFrame is a stream_event frame starting the API message id.
func messageStartFrame(id, parent string) map[string]any {
	return streamFrame(map[string]any{"type": "message_start",
		"message": map[string]any{"id": id, "type": "message", "role": "assistant"}}, parent)
}

// assistantBlocksFrame is an assistant frame of the API message id with the
// given content blocks, from the subagent parent names when it is not empty.
func assistantBlocksFrame(id, parent string, blocks ...any) map[string]any {
	frame := map[string]any{"type": "assistant", "session_id": "s1", "message": map[string]any{
		"id": id, "model": "claude-opus-4-5", "content": blocks}}
	if parent != "" {
		frame["parent_tool_use_id"] = parent
	}
	return frame
}

func textBlock(text string) map[string]any {
	return map[string]any{"type": "text", "text": text}
}

func TestTurnTextFromAssistantMessages(t *testing.T) {
	t.Parallel()
	client, ft := testClient(t, nil, nil)
	turn, err := client.Send(t.Context(), Text("hi"))
	if err != nil {
		t.Fatal(err)
	}
	// Without partial messages the text comes from the TextBlocks, tool
	// calls and subagent output left out.
	ft.push(assistantBlocksFrame("m1", "", textBlock("Hello"),
		map[string]any{"type": "tool_use", "id": "tu1", "name": "Agent", "input": map[string]any{}}))
	ft.push(assistantBlocksFrame("m2", "tu1", textBlock("from the subagent")))
	ft.push(assistantBlocksFrame("m3", "", textBlock(" world"), textBlock("!")))
	ft.push(resultFrame())

	texts, err := collectText(t.Context(), turn)
	if err != nil || strings.Join(texts, "|") != "Hello| world|!" {
		t.Fatalf("text = %q, %v", texts, err)
	}
	// The result stays available, and an ended turn yields no more text.
	if res, err := turn.Result(t.Context()); err != nil || res.SessionID != "s1" {
		t.Fatalf("result = %+v, %v", res, err)
	}
	if texts, err := collectText(t.Context(), turn); texts != nil || err != nil {
		t.Fatalf("text again = %q, %v", texts, err)
	}
}

func TestTurnTextFromPartialMessages(t *testing.T) {
	t.Parallel()
	client, ft := testClient(t, &Options{IncludePartialMessages: true}, nil)
	turn, err := client.Send(t.Context(), Text("hi"))
	if err != nil {
		t.Fatal(err)
	}
	ft.push(messageStartFrame("m1", ""))
	ft.push(deltaFrame("Hel", ""))
	ft.push(streamFrame(map[string]any{"type": "content_block_delta", "index": 1,
		"delta": map[string]any{"type": "input_json_delta", "partial_json": "{}"}}, ""))
	ft.push(deltaFrame("lo", ""))
	// The complete message repeats the deltas and is skipped.
	ft.push(assistantBlocksFrame("m1", "", textBlock("Hello")))
	// Subagent deltas and messages are skipped.
	ft.push(messageStartFrame("m2", "tu1"))
	ft.push(deltaFrame("sub", "tu1"))
	ft.push(assistantBlocksFrame("m2", "tu1", textBlock("sub")))
	ft.push(messageStartFrame("m3", ""))
	ft.push(deltaFrame(" world", ""))
	ft.push(assistantBlocksFrame("m3", "", textBlock(" world")))
	// A message that was never streamed, such as a synthetic one, keeps
	// its text.
	ft.push(assistantBlocksFrame("m4", "", textBlock("!")))
	ft.push(resultFrame())

	// Breaking out and ranging again resumes where the loop stopped.
	var texts []string
	for text, err := range turn.Text(t.Context()) {
		if err != nil {
			t.Fatal(err)
		}
		texts = append(texts, text)
		break
	}
	rest, err := collectText(t.Context(), turn)
	texts = append(texts, rest...)
	if err != nil || strings.Join(texts, "|") != "Hel|lo| world|!" {
		t.Fatalf("text = %q, %v", texts, err)
	}
}

func TestTurnTextErrors(t *testing.T) {
	t.Parallel()
	client, ft := testClient(t, nil, nil)

	// An error result ends Text with the *ResultError Result returns, and
	// Result still returns the result.
	failed, err := client.Send(t.Context(), Text("fail"))
	if err != nil {
		t.Fatal(err)
	}
	ft.push(assistantFrame("partial"))
	ft.push(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true, "session_id": "s1"})
	texts, err := collectText(t.Context(), failed)
	var resErr *ResultError
	if len(texts) != 1 || texts[0] != "partial" || !errors.As(err, &resErr) {
		t.Fatalf("text = %q, %v", texts, err)
	}
	if res, err := failed.Result(t.Context()); res == nil || !errors.As(err, &resErr) {
		t.Fatalf("result = %+v, %v", res, err)
	}

	// A cancelled ctx ends Text with its error without ending the turn.
	turn, err := client.Send(t.Context(), Text("again"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := collectText(ctx, turn); !errors.Is(err, context.Canceled) {
		t.Fatalf("text with cancelled ctx = %v", err)
	}
	// After Close on a running turn Text fails with ErrClosed.
	_ = turn.Close()
	if _, err := collectText(t.Context(), turn); !errors.Is(err, ErrClosed) {
		t.Fatalf("text after close = %v", err)
	}

	// A fatal stream error is the final item.
	last, err := client.Send(t.Context(), Text("last"))
	if err != nil {
		t.Fatal(err)
	}
	ft.push(resultFrame()) // ends the closed turn
	ft.push(assistantFrame("before"))
	ft.finish(errors.New("boom"))
	texts, err = collectText(t.Context(), last)
	if len(texts) != 1 || texts[0] != "before" || err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("text = %q, %v", texts, err)
	}
}

func TestClientRunInterruptsOnContextEnd(t *testing.T) {
	t.Parallel()
	client, ft := testClient(t, nil, nil)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := client.Run(ctx, Text("long"))
		done <- err
	}()
	// Cancel once the turn has been written.
	for {
		if frame := ft.nextWrite(t); frame["type"] == "user" {
			break
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("run = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its ctx ended")
	}
	if n := interrupts(t, ft); n != 1 {
		t.Fatalf("run sent %d interrupts, want 1", n)
	}

	// The session stays usable: the interrupted turn's result is skipped by
	// the next one.
	ft.push(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true, "session_id": "s1"})
	ft.push(map[string]any{"type": "result", "subtype": "success", "is_error": false, "session_id": "s1", "result": "next"})
	if res, err := client.Run(t.Context(), Text("next")); err != nil || res.Text() != "next" {
		t.Fatalf("next run = %+v, %v", res, err)
	}
	if n := interrupts(t, ft); n != 1 {
		t.Fatalf("a completed run sent an interrupt (%d total)", n)
	}
}
