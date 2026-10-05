package claude

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// warmCLI answers control requests per subtype (an "error" key in the
// payload makes it an error response) and, once a user message arrives,
// replies with an assistant message and a result and ends its output.
func warmCLI(answers map[string]map[string]any) *fakeTransport {
	ft := newFakeTransport()
	ft.mu.Lock()
	ft.onWrite = func(frame map[string]any) {
		switch frame["type"] {
		case "control_request":
			subtype := frame["request"].(map[string]any)["subtype"].(string)
			payload := answers[subtype]
			if payload == nil {
				payload = map[string]any{}
			}
			response := map[string]any{"subtype": "success", "request_id": frame["request_id"], "response": payload}
			if msg, ok := payload["error"].(string); ok {
				response = map[string]any{"subtype": "error", "request_id": frame["request_id"], "error": msg}
			}
			ft.push(map[string]any{"type": "control_response", "response": response})
		case "user":
			ft.push(assistantFrame("hi"))
			ft.push(resultFrame())
			ft.finish(nil)
		}
	}
	ft.mu.Unlock()
	return ft
}

func frameKinds(t *testing.T, ft *fakeTransport) []string {
	t.Helper()
	var kinds []string
	for _, f := range ft.frames(t) {
		if f["type"] == "control_request" {
			kinds = append(kinds, f["request"].(map[string]any)["subtype"].(string))
		} else {
			kinds = append(kinds, f["type"].(string))
		}
	}
	return kinds
}

func collectMessages(t *testing.T, seq func(func(Message, error) bool)) (int, error) {
	t.Helper()
	n := 0
	for msg, err := range seq {
		if err != nil {
			return n, err
		}
		if msg != nil {
			n++
		}
	}
	return n, nil
}

func TestStartupWarmQuery(t *testing.T) {
	t.Parallel()
	ft := warmCLI(map[string]map[string]any{"initialize": {"output_style": "warm"}})
	warm, err := Startup(t.Context(), Options{Transport: ft})
	if err != nil {
		t.Fatal(err)
	}
	defer warm.Close()
	if warm.InitializationResult().OutputStyle != "warm" {
		t.Fatalf("init = %#v", warm.InitializationResult())
	}
	// Nothing but the handshake happens before the prompt.
	if kinds := frameKinds(t, ft); strings.Join(kinds, ",") != "initialize" {
		t.Fatalf("frames = %q", kinds)
	}
	n, err := collectMessages(t, warm.Query(t.Context(), "hello"))
	if err != nil || n != 2 {
		t.Fatalf("messages = %d, err = %v", n, err)
	}
	if !ft.endedInput() {
		t.Fatal("input should be closed after the run")
	}
	if _, err := collectMessages(t, warm.Query(t.Context(), "again")); err == nil {
		t.Fatal("a second query should fail")
	}
	if err := warm.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStartupCloseWithoutQuery(t *testing.T) {
	t.Parallel()
	ft := warmCLI(nil)
	warm, err := Startup(t.Context(), Options{Transport: ft})
	if err != nil {
		t.Fatal(err)
	}
	_ = warm.Close()
	_ = warm.Close()
	if _, err := collectMessages(t, warm.Query(t.Context(), "late")); err == nil {
		t.Fatal("query after Close should fail")
	}
}

// Like New, Startup's ctx bounds the startup only.
func TestStartupSessionOutlivesContext(t *testing.T) {
	t.Parallel()
	ft := warmCLI(nil)
	ctx, cancel := context.WithCancel(t.Context())
	warm, err := Startup(ctx, Options{Transport: ft})
	if err != nil {
		t.Fatal(err)
	}
	defer warm.Close()
	cancel()
	n, err := collectMessages(t, warm.Query(t.Context(), "hello"))
	if err != nil || n != 2 {
		t.Fatalf("messages = %d, err = %v", n, err)
	}
}

// The query's own ctx ends it, and the session with it.
func TestWarmQueryContextCancellation(t *testing.T) {
	t.Parallel()
	ft := newFakeTransport()
	initResponder(ft, nil)
	warm, err := Startup(t.Context(), Options{Transport: ft})
	if err != nil {
		t.Fatal(err)
	}
	defer warm.Close()

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := collectMessages(t, warm.Query(ctx, "hello"))
		done <- err
	}()
	for ft.nextWrite(t)["type"] != "user" {
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("query error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelling the query's ctx did not end it")
	}
	if !isDone(ft.closedCh) {
		t.Fatal("cancelling the query's ctx should close the transport")
	}
}

func TestStartupInitializeFailure(t *testing.T) {
	t.Parallel()
	ft := warmCLI(map[string]map[string]any{"initialize": {"error": "not logged in"}})
	if _, err := Startup(t.Context(), Options{Transport: ft}); err == nil || !strings.Contains(err.Error(), "not logged in") {
		t.Fatalf("error = %v", err)
	}
	if !ft.closed {
		t.Fatal("transport should be closed after a failed startup")
	}
}

// prewarmWith starts a spare whose transport is ft, capturing the options the
// transport was built from.
func prewarmWith(t *testing.T, ft *fakeTransport, opts *Options) (*SpareProcess, *Options) {
	t.Helper()
	var built *Options
	deps := &sessionDeps{newTransport: func(o *Options) Transport {
		built = o
		return ft
	}}
	spare, err := prewarm(t.Context(), opts, deps)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = spare.Close() })
	return spare, built
}

func TestPrewarmClaim(t *testing.T) {
	t.Parallel()
	configDir := t.TempDir()
	ft := warmCLI(map[string]map[string]any{
		"claim_session": {"cwd": "/canon", "session_id": "s1", "parked_ms": 42, "sdk_mcp_settled": true,
			"initialize": map[string]any{"output_style": "claimed"}},
	})
	srv := NewSDKMCPServer("tools", "1.0")
	spare, built := prewarmWith(t, ft, &Options{
		Env:        map[string]string{"CLAUDE_CONFIG_DIR": configDir},
		MCPServers: map[string]MCPServerConfig{"tools": srv, "remote": &MCPHTTPServerConfig{URL: "https://x"}},
	})
	if v, ok := built.ExtraArgs["await-claim"]; !ok || v != nil {
		t.Fatalf("extra args = %#v", built.ExtraArgs)
	}
	parkDir := built.Cwd
	if filepath.Dir(parkDir) != filepath.Join(configDir, "spares") || !strings.HasPrefix(filepath.Base(parkDir), "spare-") {
		t.Fatalf("park dir = %q", parkDir)
	}
	if info, err := os.Stat(parkDir); err != nil || !info.IsDir() {
		t.Fatalf("park dir missing: %v", err)
	}

	seq, err := spare.Claim(t.Context(), "go", ClaimOptions{
		Cwd: "/work", Model: "opus", PermissionMode: PermissionModePlan, Title: "T",
		Settings: map[string]any{"fastMode": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := spare.Claim(t.Context(), "twice", ClaimOptions{Cwd: "/work"}); err == nil {
		t.Fatal("a second claim should fail")
	}
	n, err := collectMessages(t, seq)
	if err != nil || n != 2 {
		t.Fatalf("messages = %d, err = %v", n, err)
	}
	res, err := spare.Claimed(t.Context())
	if err != nil || res.Cwd != "/canon" || res.SessionID != "s1" || *res.ParkedMS != 42 || !res.SDKMCPSettled {
		t.Fatalf("claimed = %#v, %v", res, err)
	}
	if got := strings.Join(frameKinds(t, ft), ","); got != "initialize,claim_session,set_model,apply_flag_settings,user" {
		t.Fatalf("frames = %s", got)
	}
	claim := ft.frames(t)[1]["request"].(map[string]any)
	assertJSONEqual(t, claim, map[string]any{"subtype": "claim_session", "cwd": "/work", "permission_mode": "plan",
		"title": "T", "sdk_mcp_servers": []any{"tools"}, "include_initialize": true})
	if spare.InitializationResult().OutputStyle != "claimed" {
		t.Fatalf("init = %#v", spare.InitializationResult())
	}
	if _, err := os.Stat(parkDir); !os.IsNotExist(err) {
		t.Fatalf("park dir should be removed after the claim: %v", err)
	}
}

func TestPrewarmSettingsRefused(t *testing.T) {
	t.Parallel()
	ft := warmCLI(map[string]map[string]any{"apply_flag_settings": {"error": "bad key"}})
	spare, _ := prewarmWith(t, ft, &Options{Cwd: t.TempDir()})
	seq, err := spare.Claim(t.Context(), "go", ClaimOptions{Cwd: "/w", Settings: map[string]any{"x": 1}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = collectMessages(t, seq)
	var claimErr *ClaimError
	if !errors.As(err, &claimErr) || !strings.HasPrefix(claimErr.Msg, "settings_not_applied") || claimErr.Claim == nil {
		t.Fatalf("error = %v", err)
	}
	for _, kind := range frameKinds(t, ft) {
		if kind == "user" {
			t.Fatal("the prompt must not be sent when the settings were refused")
		}
	}
}

func TestPrewarmRejections(t *testing.T) {
	t.Parallel()
	if _, err := Prewarm(t.Context(), Options{Resume: "abc"}); err == nil {
		t.Fatal("Prewarm with Resume should fail")
	}

	ft := warmCLI(nil)
	spare, _ := prewarmWith(t, ft, &Options{Cwd: t.TempDir()})
	if _, err := spare.Claim(t.Context(), "go", ClaimOptions{Cwd: "/w", Settings: map[string]any{"hooks": map[string]any{}}}); err == nil {
		t.Fatal("hook settings should be refused")
	}
	// Nothing was sent, so the spare can still be claimed... or closed.
	if got := strings.Join(frameKinds(t, ft), ","); got != "initialize" {
		t.Fatalf("frames = %s", got)
	}
	_ = spare.Close()
	_, err := spare.Claimed(context.Background())
	var claimErr *ClaimError
	if !errors.As(err, &claimErr) || !strings.HasPrefix(claimErr.Msg, "spare_closed") {
		t.Fatalf("claimed error = %v", err)
	}
	if _, err := spare.Claim(t.Context(), "go", ClaimOptions{Cwd: "/w"}); err == nil {
		t.Fatal("claim after Close should fail")
	}
}

func TestPrewarmOptionNotAppliedStillRuns(t *testing.T) {
	t.Parallel()
	ft := warmCLI(map[string]map[string]any{"set_model": {"error": "unknown model"}})
	spare, _ := prewarmWith(t, ft, &Options{Cwd: t.TempDir()})
	// The settings overlay holds the prompt until the claim settles; a
	// refused model alone must not keep it from running.
	seq, err := spare.Claim(t.Context(), "go", ClaimOptions{Cwd: "/w", Model: "nope", Settings: map[string]any{"x": 1}})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := collectMessages(t, seq); err != nil || n != 2 {
		t.Fatalf("messages = %d, err = %v", n, err)
	}
	_, err = spare.Claimed(t.Context())
	var claimErr *ClaimError
	if !errors.As(err, &claimErr) || !strings.HasPrefix(claimErr.Msg, "option_not_applied: model: unknown model") || claimErr.Claim == nil {
		t.Fatalf("claimed error = %v", err)
	}
}
