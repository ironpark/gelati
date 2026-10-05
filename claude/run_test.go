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
	res, err := Run(t.Context(), "What is 2+2?", &Options{Transport: ft})
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
		ft.finish(NewProcessError("Command failed", &code, ""))
	}
	res, err := Run(t.Context(), "hi", &Options{Transport: ft})
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
	res, err := Run(t.Context(), "hi", &Options{Transport: ft})
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
	res, err := Run(t.Context(), "hi", &Options{Transport: ft})
	if _, ok := errors.AsType[*ConnectionError](err); !ok || res != nil {
		t.Fatalf("Run = %+v, %v; want nil and a *ConnectionError", res, err)
	}
}

func TestClientRun(t *testing.T) {
	t.Parallel()
	client, ft := connectedClient(t, nil)
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
	res, err := client.Run(t.Context(), "ping", "")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Result != "pong" {
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

func TestClientDoneOnDisconnect(t *testing.T) {
	t.Parallel()
	ft := newFakeTransport()
	initResponder(ft, nil)
	client := NewClient(&Options{Transport: ft})
	waitDone(t, client.Done()) // no session yet
	if _, ok := errors.AsType[*ConnectionError](client.Err()); !ok {
		t.Fatalf("Err before Connect = %v", client.Err())
	}
	if err := client.Connect(t.Context()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	done := client.Done()
	select {
	case <-done:
		t.Fatal("Done closed while connected")
	default:
	}
	if err := client.Err(); err != nil {
		t.Fatalf("Err while running = %v", err)
	}
	if err := client.Disconnect(); err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	// Disconnect settles Done and Err before it returns.
	select {
	case <-done:
	default:
		t.Fatal("Done still open after Disconnect")
	}
	if err := client.Err(); err != nil {
		t.Fatalf("Err after Disconnect = %v", err)
	}
	// The ended session stays reported until the next Connect.
	waitDone(t, client.Done())

	ft2 := newFakeTransport()
	initResponder(ft2, nil)
	client.opts.Transport = ft2
	if err := client.Connect(t.Context()); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	defer client.Disconnect()
	select {
	case <-client.Done():
		t.Fatal("a new session should report a fresh Done")
	default:
	}
}

func TestClientDoneOnProcessExit(t *testing.T) {
	t.Parallel()
	client, ft := connectedClient(t, nil)
	code := 1
	ft.finish(NewProcessError("Command failed", &code, "boom"))
	waitDone(t, client.Done())
	if _, ok := errors.AsType[*ProcessError](client.Err()); !ok {
		t.Fatalf("Err = %T (%v), want *ProcessError", client.Err(), client.Err())
	}
}

func TestClientDoneOnOutputEnd(t *testing.T) {
	t.Parallel()
	client, ft := connectedClient(t, nil)
	ft.finish(nil)
	waitDone(t, client.Done())
	if _, ok := errors.AsType[*ConnectionError](client.Err()); !ok {
		t.Fatalf("Err = %T (%v), want *ConnectionError", client.Err(), client.Err())
	}
	// Disconnect keeps the reason the session ended.
	_ = client.Disconnect()
	if client.Err() == nil {
		t.Fatal("Err should survive Disconnect")
	}
}

func TestClientDoneOnContextCancel(t *testing.T) {
	t.Parallel()
	ft := newFakeTransport()
	initResponder(ft, nil)
	client := NewClient(&Options{Transport: ft})
	ctx, cancel := context.WithCancel(t.Context())
	if err := client.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Disconnect()
	cancel()
	// A real transport ends its output when ctx is cancelled.
	ft.finish(nil)
	waitDone(t, client.Done())
	if !errors.Is(client.Err(), context.Canceled) {
		t.Fatalf("Err = %v, want context.Canceled", client.Err())
	}
}
