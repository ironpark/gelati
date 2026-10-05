package codex

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"testing"
	"time"
)

// notifyTurn emits a notification scoped to thr_1/turn_1.
func notifyTurn(server *fakeServer, method string, params map[string]any) {
	params["threadId"], params["turnId"] = "thr_1", "turn_1"
	server.notify(method, params)
}

func completedMessage(id, text, phase string) map[string]any {
	item := map[string]any{"type": "agentMessage", "id": id, "text": text}
	if phase != "" {
		item["phase"] = phase
	}
	return map[string]any{"item": item}
}

func TestTurnResult(t *testing.T) {
	client, server := connect(t, Options{})
	threadID := startThread(t, client, server, "thr_1")
	stream := startTurn(t, client, server, threadID, "turn_1", Text("hi"))

	notifyTurn(server, MethodItemCompleted, completedMessage("m1", "let me look", PhaseCommentary))
	notifyTurn(server, MethodItemCompleted, completedMessage("m2", "the answer", PhaseFinalAnswer))
	notifyTurn(server, MethodItemCompleted, completedMessage("m3", "a late note", PhaseCommentary))
	notifyTurn(server, MethodTokenUsageUpdated, map[string]any{"tokenUsage": map[string]any{
		"total": map[string]any{"totalTokens": 30}, "last": map[string]any{"totalTokens": 30}}})
	server.notify(MethodTurnCompleted, map[string]any{"threadId": "thr_1",
		"turn": map[string]any{"id": "turn_1", "status": "completed", "items": []any{}, "durationMs": 1200}})

	// Reading some events first must not hide them from the result.
	if event, ok := recvEvent(t, stream); !ok || event.Kind != EventItemCompleted {
		t.Fatalf("event = %+v", event)
	}
	result, err := stream.Result(context.Background())
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if result.Text() != "the answer" || len(result.Items) != 3 {
		t.Fatalf("result = %+v", result)
	}
	if result.Usage == nil || result.Usage.Total.TotalTokens != 30 {
		t.Fatalf("usage = %+v", result.Usage)
	}
	if result.Turn.Status != TurnCompleted || result.Turn.DurationMs == nil || *result.Turn.DurationMs != 1200 {
		t.Fatalf("turn = %+v", result.Turn)
	}
}

func TestTurnResultText(t *testing.T) {
	item := func(text, phase string) ThreadItem {
		return ThreadItem{Item: &AgentMessageItem{Text: text, Phase: phase}}
	}
	cases := []struct {
		items []ThreadItem
		want  string
	}{
		{nil, ""},
		{[]ThreadItem{item("a", ""), item("b", "")}, "b"},
		{[]ThreadItem{item("a", ""), item("b", PhaseCommentary)}, "a"},
		{[]ThreadItem{item("only commentary", PhaseCommentary)}, ""},
		{[]ThreadItem{item("final", PhaseFinalAnswer), item("x", "")}, "final"},
	}
	for _, tc := range cases {
		if got := (&TurnResult{Items: tc.items}).Text(); got != tc.want {
			t.Errorf("Text = %q, want %q", got, tc.want)
		}
	}
	if got := (*TurnResult)(nil).Text(); got != "" {
		t.Errorf("nil Text = %q", got)
	}
}

func TestRunFailedTurn(t *testing.T) {
	client, server := connect(t, Options{})
	threadID := startThread(t, client, server, "thr_1")

	done := serve(t, func() {
		req := server.expect("turn/start")
		server.respond(req, map[string]any{"turn": map[string]any{"id": "turn_1", "status": "inProgress", "items": []any{}}})
		notifyTurn(server, MethodError, map[string]any{"willRetry": true,
			"error": map[string]any{"message": "reconnecting", "codexErrorInfo": map[string]any{
				"responseStreamDisconnected": map[string]any{"httpStatusCode": 502}}}})
		server.notify(MethodTurnCompleted, map[string]any{"threadId": "thr_1", "turn": map[string]any{
			"id": "turn_1", "status": "failed", "items": []any{},
			"error": map[string]any{"message": "usage limit", "codexErrorInfo": ErrorInfoUsageLimitExceeded}}})
	})
	result, err := client.Run(context.Background(), threadID, Text("hi"), nil)
	<-done
	turnErr, ok := errors.AsType[*TurnError](err)
	if !ok || turnErr.Kind() != ErrorInfoUsageLimitExceeded {
		t.Fatalf("err = %v", err)
	}
	if result == nil || result.Turn.Status != TurnFailed {
		t.Fatalf("result = %+v", result)
	}
}

func TestErrorAndOtherTurnNotifications(t *testing.T) {
	client, server := connect(t, Options{})
	threadID := startThread(t, client, server, "thr_1")
	stream := startTurn(t, client, server, threadID, "turn_1", Text("hi"))

	notifyTurn(server, MethodError, map[string]any{"willRetry": true,
		"error": map[string]any{"message": "reconnecting", "codexErrorInfo": map[string]any{
			"responseStreamDisconnected": map[string]any{"httpStatusCode": 502}}}})
	notifyTurn(server, "model/rerouted", map[string]any{"fromModel": "a", "toModel": "b"})

	event, _ := recvEvent(t, stream)
	if event.Kind != EventError || !event.WillRetry || event.Error.Kind() != ErrorInfoResponseStreamDisconnected {
		t.Fatalf("error event = %+v", event)
	}
	if code, ok := event.Error.HTTPStatusCode(); !ok || code != 502 {
		t.Fatalf("http status = %d, %v", code, ok)
	}
	event, _ = recvEvent(t, stream)
	if event.Kind != EventNotification || event.Method != "model/rerouted" || event.TurnID != "turn_1" {
		t.Fatalf("other event = %+v", event)
	}
	stream.Close()
}

func TestRunInterruptsOnCancel(t *testing.T) {
	client, server := connect(t, Options{})
	threadID := startThread(t, client, server, "thr_1")
	stream := startTurn(t, client, server, threadID, "turn_1", Text("hi"))

	ctx, cancel := context.WithCancel(context.Background())
	interrupted := make(chan jsontext.Value, 1)
	done := serve(t, func() {
		req := server.expect("turn/interrupt")
		interrupted <- req.Params
		server.respond(req, nil)
	})
	cancel()
	// Run is StartTurn followed by collect; drive collect on the started turn.
	_, err := collect(ctx, stream)
	<-done
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	var params InterruptTurnParams
	if raw := <-interrupted; json.Unmarshal(raw, &params) != nil || params.TurnID != "turn_1" || params.ThreadID != "thr_1" {
		t.Fatalf("interrupt params = %+v", params)
	}
	select {
	case <-stream.Done():
	default:
		t.Fatal("stream not closed after the interrupt")
	}
}

func TestTurnStreamCancel(t *testing.T) {
	client, server := connect(t, Options{})
	threadID := startThread(t, client, server, "thr_1")
	stream := startTurn(t, client, server, threadID, "turn_1", Text("hi"))

	done := serve(t, func() {
		req := server.expect("turn/interrupt")
		var params InterruptTurnParams
		if json.Unmarshal(req.Params, &params) != nil || params.TurnID != "turn_1" || params.ThreadID != "thr_1" {
			t.Errorf("interrupt params = %s", req.Params)
		}
		server.respond(req, map[string]any{})
		server.notify(MethodTurnCompleted, map[string]any{"threadId": "thr_1",
			"turn": map[string]any{"id": "turn_1", "status": "interrupted", "items": []any{}}})
	})
	if err := stream.Cancel(context.Background()); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	<-done
	result, err := stream.Result(context.Background())
	if err != nil || result.Turn.Status != TurnInterrupted {
		t.Fatalf("Result = %+v, %v", result, err)
	}
	// The turn is over: Cancel is a no-op and Close leaves the result intact.
	if err := stream.Cancel(context.Background()); err != nil {
		t.Fatalf("Cancel after end: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if result, err := stream.Result(context.Background()); err != nil || result.Turn.Status != TurnInterrupted {
		t.Fatalf("Result after Close = %+v, %v", result, err)
	}
}

func TestStartExternalTurn(t *testing.T) {
	client, server := connect(t, Options{})
	threadID := startThread(t, client, server, "thr_1")

	if _, err := client.StartExternalTurn(context.Background(), threadID, ExternalMessage{}, nil); err == nil {
		t.Fatal("empty ToolName accepted")
	}
	done := serve(t, func() {
		req := server.expect("turn/start")
		const want = `{"threadId":"thr_1","input":[],"toolOutput":{"name":"inbox","output":"mail body"},"turnTrigger":"cron"}`
		if string(req.Params) != want {
			t.Errorf("params = %s\nwant     %s", req.Params, want)
		}
		server.respond(req, map[string]any{"turn": map[string]any{"id": "turn_1", "status": "inProgress", "items": []any{}}})
	})
	stream, err := client.StartExternalTurn(context.Background(), threadID,
		ExternalMessage{ToolName: "inbox", Output: "mail body"}, &TurnOptions{TurnTrigger: "cron"})
	<-done
	if err != nil {
		t.Fatalf("StartExternalTurn: %v", err)
	}
	stream.Close()
}

func TestUnsubscribeFinishesStream(t *testing.T) {
	client, server := connect(t, Options{})
	threadID := startThread(t, client, server, "thr_1")
	stream := startTurn(t, client, server, threadID, "turn_1", Text("hi"))

	done := serve(t, func() {
		server.respond(server.expect("thread/unsubscribe"), map[string]any{"status": "unsubscribed"})
	})
	if _, err := client.UnsubscribeThread(context.Background(), threadID); err != nil {
		t.Fatalf("UnsubscribeThread: %v", err)
	}
	<-done
	ctx, cancel := context.WithTimeout(context.Background(), fakeTimeout)
	defer cancel()
	if _, err := stream.Result(ctx); !errors.Is(err, ErrClosed) {
		t.Fatalf("Result after unsubscribe = %v, want ErrClosed", err)
	}
}

func TestAwaitLoginAfterCompletion(t *testing.T) {
	client, server := connect(t, Options{})

	server.notify(MethodLoginCompleted, map[string]any{"loginId": "login-1", "success": true})
	// A round trip guarantees the client has read the notification.
	done := serve(t, func() { server.respond(server.expect("account/logout"), nil) })
	if err := client.Logout(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-done

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	completed, err := client.AwaitLogin(ctx, "login-1")
	if err != nil || completed.LoginID != "login-1" {
		t.Fatalf("AwaitLogin = %+v, %v", completed, err)
	}
}

func TestIsOverloaded(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{&RPCError{Code: CodeServerOverloaded}, true},
		{&RPCError{Code: -32000, Data: jsontext.Value(`{"codexErrorInfo":"server_overloaded"}`)}, true},
		{&RPCError{Code: -32000, Data: jsontext.Value(`{"codexErrorInfo":{"serverOverloaded":{}}}`)}, true},
		{&RPCError{Code: -32000, Data: jsontext.Value(`{"codexErrorInfo":"other"}`)}, false},
		{&RPCError{Code: CodeInvalidParams, Data: jsontext.Value(`"server_overloaded"`)}, false},
		{errors.New("plain"), false},
	}
	for _, tc := range cases {
		if got := IsOverloaded(tc.err); got != tc.want {
			t.Errorf("IsOverloaded(%v) = %v", tc.err, got)
		}
	}
	if !IsRetryLimitExceeded(&RPCError{Code: -32000, Message: "Too many failed attempts"}) {
		t.Error("retry limit not detected")
	}
}

func TestRetryOnOverload(t *testing.T) {
	opts := RetryOptions{MaxAttempts: 3, InitialDelay: time.Millisecond}
	calls := 0
	got, err := RetryOnOverload(context.Background(), opts, func(context.Context) (string, error) {
		calls++
		if calls < 3 {
			return "", &RPCError{Code: CodeServerOverloaded}
		}
		return "ok", nil
	})
	if got != "ok" || err != nil || calls != 3 {
		t.Fatalf("got %q, %v after %d calls", got, err, calls)
	}

	calls = 0
	_, err = RetryOnOverload(context.Background(), opts, func(context.Context) (int, error) {
		calls++
		return 0, &RPCError{Code: CodeInvalidParams}
	})
	if calls != 1 || err == nil {
		t.Fatalf("non-retryable error retried %d times", calls)
	}
}

func TestEventsIterator(t *testing.T) {
	t.Run("completes", func(t *testing.T) {
		client, server := connect(t, Options{})
		threadID := startThread(t, client, server, "thr_1")
		stream := startTurn(t, client, server, threadID, "turn_1", Text("hi"))
		notifyTurn(server, MethodAgentMessageDelta, map[string]any{"itemId": "m", "delta": "hi"})
		server.notify(MethodTurnCompleted, map[string]any{"threadId": "thr_1",
			"turn": map[string]any{"id": "turn_1", "status": "completed", "items": []any{}}})

		var kinds []EventKind
		for event, err := range stream.Events(context.Background()) {
			if err != nil {
				t.Fatalf("Events error: %v", err)
			}
			kinds = append(kinds, event.Kind)
		}
		if len(kinds) != 2 || kinds[0] != EventAgentMessageDelta || kinds[1] != EventTurnCompleted {
			t.Fatalf("kinds = %v", kinds)
		}
	})
	t.Run("abandoned", func(t *testing.T) {
		client, server := connect(t, Options{})
		threadID := startThread(t, client, server, "thr_1")
		stream := startTurn(t, client, server, threadID, "turn_1", Text("hi"))
		stream.Close()
		// The pump closes the channel when it next sees the turn.
		notifyTurn(server, MethodAgentMessageDelta, map[string]any{"itemId": "m", "delta": "hi"})

		var last error
		for _, err := range stream.Events(context.Background()) {
			last = err
		}
		if !errors.Is(last, ErrClosed) {
			t.Fatalf("last error = %v, want ErrClosed", last)
		}
	})
	t.Run("context", func(t *testing.T) {
		client, server := connect(t, Options{})
		threadID := startThread(t, client, server, "thr_1")
		stream := startTurn(t, client, server, threadID, "turn_1", Text("hi"))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		for _, err := range stream.Events(ctx) {
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v", err)
			}
		}
		stream.Close()
	})
}
