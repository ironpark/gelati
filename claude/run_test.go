package claude

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRunReturnsResult(t *testing.T) {
	t.Parallel()
	result := resultFrame()
	result["result"] = "4"
	ft := scriptedCLI(t, assistantFrame("4"), result)
	res, err := Run(t.Context(), "What is 2+2?", Options{Transport: ft})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || res.Result != "4" || res.SessionID != "s1" {
		t.Fatalf("result = %+v", res)
	}
}

func TestRunReportsStreamError(t *testing.T) {
	t.Parallel()
	// The CLI reports a result and then exits non-zero.
	ft := newFakeTransport()
	code := 2
	ft.onWrite = func(frame map[string]any) {
		req, _ := frame["request"].(map[string]any)
		if frame["type"] != "control_request" || req["subtype"] != "initialize" {
			return
		}
		ft.push(map[string]any{"type": "control_response", "response": map[string]any{
			"subtype": "success", "request_id": frame["request_id"], "response": map[string]any{}}})
		ft.push(resultFrame())
		ft.finish(newProcessError("Command failed", &code, "", nil))
	}
	res, err := Run(t.Context(), "hi", Options{Transport: ft})
	if _, ok := errors.AsType[*ProcessError](err); !ok {
		t.Fatalf("error = %T (%v), want *ProcessError", err, err)
	}
	if res == nil || res.SessionID != "s1" {
		t.Fatalf("result seen before the error should be returned, got %+v", res)
	}
}

func TestRunErrorResult(t *testing.T) {
	t.Parallel()
	result := resultFrame()
	result["is_error"] = true
	result["subtype"] = "error_max_turns"
	ft := scriptedCLI(t, result)
	res, err := Run(t.Context(), "hi", Options{Transport: ft})
	rerr, ok := errors.AsType[*ResultError](err)
	if !ok || rerr.Subtype != "error_max_turns" {
		t.Fatalf("error = %T (%v), want *ResultError", err, err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("result = %+v", res)
	}
}

func TestRunWithoutResult(t *testing.T) {
	t.Parallel()
	ft := scriptedCLI(t, assistantFrame("partial"))
	res, err := Run(t.Context(), "hi", Options{Transport: ft})
	if _, ok := errors.AsType[*ConnectionError](err); !ok || res != nil {
		t.Fatalf("Run = %+v, %v; want nil and a *ConnectionError", res, err)
	}
}

func TestClientRun(t *testing.T) {
	t.Parallel()
	client, ft := testClient(t, nil, nil)
	go func() {
		for {
			frame := ft.nextWrite(t)
			if frame["type"] != "user" {
				continue
			}
			result := resultFrame()
			result["result"] = "pong"
			ft.push(assistantFrame("pong"))
			ft.push(result)
			return
		}
	}()
	res, err := client.Run(t.Context(), Text("ping"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Text() != "pong" {
		t.Fatalf("result = %+v", res)
	}
}

// waitDone fails the test unless ch closes promptly.
func waitDone(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("Done was not closed")
	}
}

func TestClientDoneOnClose(t *testing.T) {
	t.Parallel()
	client, _ := testClient(t, nil, nil)
	done := client.Done()
	select {
	case <-done:
		t.Fatal("Done closed while running")
	default:
	}
	if err := client.Err(); err != nil {
		t.Fatalf("Err while running = %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Close settles Done and Err before it returns.
	select {
	case <-done:
	default:
		t.Fatal("Done still open after Close")
	}
	if err := client.Err(); err != nil {
		t.Fatalf("Err after Close = %v", err)
	}
}

func TestClientDoneOnProcessExit(t *testing.T) {
	t.Parallel()
	client, ft := testClient(t, nil, nil)
	code := 1
	ft.finish(newProcessError("Command failed", &code, "boom", nil))
	waitDone(t, client.Done())
	if _, ok := errors.AsType[*ProcessError](client.Err()); !ok {
		t.Fatalf("Err = %T (%v), want *ProcessError", client.Err(), client.Err())
	}
}

func TestClientDoneOnOutputEnd(t *testing.T) {
	t.Parallel()
	client, ft := testClient(t, nil, nil)
	ft.finish(nil)
	waitDone(t, client.Done())
	if _, ok := errors.AsType[*ConnectionError](client.Err()); !ok {
		t.Fatalf("Err = %T (%v), want *ConnectionError", client.Err(), client.Err())
	}
	// Close keeps the reason the session ended.
	_ = client.Close()
	if client.Err() == nil {
		t.Fatal("Err should survive Close")
	}
}

// The session outlives the ctx given to New: only Close ends it.
func TestClientOutlivesStartupContext(t *testing.T) {
	t.Parallel()
	ft := newFakeTransport()
	initResponder(ft, nil)
	ctx, cancel := context.WithCancel(t.Context())
	client, err := New(ctx, Options{Transport: ft})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer client.Close()
	cancel()

	if err := client.Interrupt(t.Context()); err != nil {
		t.Fatalf("interrupt after cancelling New's ctx: %v", err)
	}
	if isDone(client.Done()) || isDone(ft.closedCh) {
		t.Fatalf("session ended after New's ctx was cancelled: %v", client.Err())
	}

	if err := client.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if !isDone(ft.closedCh) {
		t.Fatal("Close should close the transport")
	}
	if err := client.Err(); err != nil {
		t.Fatalf("Err after Close = %v", err)
	}
}

// Abandoning New during the handshake tears the session down.
func TestNewCancelledDuringHandshake(t *testing.T) {
	t.Parallel()
	ft := newFakeTransport()
	ctx, cancel := context.WithCancel(t.Context())
	ft.mu.Lock()
	ft.onWrite = func(map[string]any) { cancel() }
	ft.mu.Unlock()
	client, err := New(ctx, Options{Transport: ft})
	if !errors.Is(err, context.Canceled) || client != nil {
		t.Fatalf("New = %v, %v; want context.Canceled", client, err)
	}
	if !isDone(ft.closedCh) {
		t.Fatal("an abandoned New should close the transport")
	}
}
