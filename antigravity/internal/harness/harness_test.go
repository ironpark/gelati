package harness

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/ironpark/gelati/antigravity/internal/wire"
)

// The test binary doubles as a fake localharness: when fakeModeEnv is set,
// TestMain runs fakeHarness instead of the tests.
const fakeModeEnv = "GELATI_FAKE_HARNESS"

const fakeAPIKey = "fake-key"

func TestMain(m *testing.M) {
	if mode := os.Getenv(fakeModeEnv); mode != "" {
		fakeHarness(mode)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeHarness implements the harness side of the protocol. Modes:
//
//	normal        echo user input; exit on stdin EOF; crash on halt_request
//	die-early     write to stderr and exit before the handshake completes
//	hang          never answer the handshake
//	slow-listen   start listening some time after reporting the port
//	ignore-stdin  keep running after stdin closes
//	wrong-first   answer the initialize event with a step update
func fakeHarness(mode string) {
	fail := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "fake: "+format+"\n", args...)
		os.Exit(2)
	}
	var lenBuf [4]byte
	if _, err := io.ReadFull(os.Stdin, lenBuf[:]); err != nil {
		fail("read length: %v", err)
	}
	buf := make([]byte, binary.LittleEndian.Uint32(lenBuf[:]))
	if _, err := io.ReadFull(os.Stdin, buf); err != nil {
		fail("read input config: %v", err)
	}
	var in wire.InputConfig
	if err := in.UnmarshalBinary(buf); err != nil {
		fail("decode input config: %v", err)
	}
	fmt.Fprintf(os.Stderr, "fake harness starting in %s mode\n", mode)
	switch mode {
	case "die-early":
		fmt.Fprintln(os.Stderr, "fatal: boom")
		os.Exit(2)
	case "hang":
		time.Sleep(time.Hour)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fail("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if mode == "slow-listen" {
		ln.Close()
	}
	out, _ := (&wire.OutputConfig{Port: new(int32(port)), APIKey: new(fakeAPIKey)}).MarshalBinary()
	os.Stdout.Write(append(binary.LittleEndian.AppendUint32(nil, uint32(len(out))), out...))
	if mode == "slow-listen" {
		time.Sleep(400 * time.Millisecond)
		if ln, err = net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port)); err != nil {
			fail("relisten: %v", err)
		}
	}

	if mode != "ignore-stdin" {
		go func() {
			_, _ = io.Copy(io.Discard, os.Stdin)
			os.Exit(0)
		}()
	}
	err = http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(APIKeyHeader) != fakeAPIKey {
			http.Error(w, "bad key", http.StatusUnauthorized)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		serveFake(c, mode, &in)
	}))
	fail("serve: %v", err)
}

func serveFake(c *websocket.Conn, mode string, in *wire.InputConfig) {
	ctx := context.Background()
	write := func(ev *wire.OutputEvent) {
		b, _ := wire.Marshal(ev)
		_ = c.Write(ctx, websocket.MessageText, b)
	}
	_, data, err := c.Read(ctx)
	if err != nil {
		return
	}
	var init wire.InitializeConversationEvent
	if err := wire.Unmarshal(data, &init); err != nil {
		fmt.Fprintf(os.Stderr, "fake: bad init: %v\n", err)
		return
	}
	if mode == "wrong-first" {
		write(&wire.OutputEvent{StepUpdate: &wire.StepUpdate{Text: new("early")}})
	}
	// Report what the client sent, for the test to check.
	summary := fmt.Sprintf("cascade=%s lang=%s storage=%v env=%s",
		init.GetConfig().GetCascadeID(), in.GetClientInfo().GetLanguage(),
		in.StorageDirectory != nil, in.GetEnv()["FAKE_VAR"]+"/"+os.Getenv("FAKE_VAR"))
	write(&wire.OutputEvent{
		SeqNum:                         new(wire.Int64(1)),
		InitializeConversationResponse: &wire.InitializeConversationResponse{CascadeID: new(summary)},
	})
	for seq := wire.Int64(2); ; seq++ {
		_, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		var ev wire.InputEvent
		if err := wire.Unmarshal(data, &ev); err != nil {
			fmt.Fprintf(os.Stderr, "fake: bad input event: %v\n", err)
			return
		}
		switch {
		case ev.UserInput != nil:
			write(&wire.OutputEvent{SeqNum: new(seq), StepUpdate: &wire.StepUpdate{
				Text: new("echo: " + ev.UserInput.Parts[0].GetText()),
			}})
		case ev.GetHaltRequest():
			fmt.Fprintln(os.Stderr, "panic: halted")
			os.Exit(3)
		}
	}
}

func startFake(t *testing.T, mode string, opts Options) (*Harness, error) {
	t.Helper()
	opts.BinaryPath = os.Args[0]
	if opts.Env == nil {
		opts.Env = map[string]string{}
	}
	opts.Env[fakeModeEnv] = mode
	opts.Env["GORACE"] = "atexit_sleep_ms=0" // the race runtime sleeps a second on exit by default
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	h, err := Start(ctx, opts)
	if err == nil {
		t.Cleanup(func() { h.Close() })
	}
	return h, err
}

func TestRoundTrip(t *testing.T) {
	h, err := startFake(t, "normal", Options{Env: map[string]string{"FAKE_VAR": "v1"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	resp, err := h.Initialize(ctx, &wire.HarnessConfig{CascadeID: new("abc")})
	if err != nil {
		t.Fatal(err)
	}
	if want := "cascade=abc lang=go storage=true env=v1/v1"; resp.GetCascadeID() != want {
		t.Fatalf("fake saw %q, want %q", resp.GetCascadeID(), want)
	}
	if err := h.Send(ctx, &wire.InputEvent{UserInput: &wire.UserInput{
		Parts: []*wire.UserInputPart{{Text: new("hi")}},
	}}); err != nil {
		t.Fatal(err)
	}
	ev, err := h.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ev.GetSeqNum() != 2 || ev.GetStepUpdate().GetText() != "echo: hi" {
		t.Fatalf("event = %+v", ev)
	}
	if !strings.Contains(h.StderrTail(), "normal mode") {
		t.Errorf("stderr tail = %q", h.StderrTail())
	}
	start := time.Now()
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	t.Logf("Close took %v", time.Since(start))
	select {
	case <-h.Exited():
	default:
		t.Fatal("process still running after Close")
	}
	if err := h.ExitErr(); err != nil {
		t.Errorf("exit error %v", err)
	}
	if err := h.Send(ctx, &wire.InputEvent{HaltRequest: new(true)}); !errors.Is(err, ErrClosed) {
		t.Errorf("Send after Close = %v", err)
	}
	if _, err := h.Receive(ctx); !errors.Is(err, ErrClosed) {
		t.Errorf("Receive after Close = %v", err)
	}
	if err := h.Close(); err != nil {
		t.Errorf("second Close = %v", err)
	}
}

func TestCrashReportsStderr(t *testing.T) {
	h, err := startFake(t, "normal", Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if _, err := h.Initialize(ctx, &wire.HarnessConfig{}); err != nil {
		t.Fatal(err)
	}
	if err := h.Send(ctx, &wire.InputEvent{HaltRequest: new(true)}); err != nil {
		t.Fatal(err)
	}
	_, err = h.Receive(ctx)
	ce, ok := errors.AsType[*ConnectionError](err)
	if !ok {
		t.Fatalf("Receive = %v, want *ConnectionError", err)
	}
	if ce.Code != websocket.StatusAbnormalClosure || !strings.Contains(ce.Stderr, "panic: halted") {
		t.Fatalf("ConnectionError = %+v", ce)
	}
	if !strings.Contains(err.Error(), "panic: halted") {
		t.Fatalf("message lacks stderr: %v", err)
	}
}

func TestDiesEarly(t *testing.T) {
	_, err := startFake(t, "die-early", Options{})
	se, ok := errors.AsType[*StartError](err)
	if !ok {
		t.Fatalf("Start = %v, want *StartError", err)
	}
	if !strings.Contains(se.Stderr, "fatal: boom") || !strings.Contains(err.Error(), "fatal: boom") {
		t.Fatalf("StartError = %v", err)
	}
}

func TestConnectRetry(t *testing.T) {
	h, err := startFake(t, "slow-listen", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Initialize(t.Context(), &wire.HarnessConfig{}); err != nil {
		t.Fatal(err)
	}
}

func TestStartContextCanceled(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	_, err := Start(ctx, Options{BinaryPath: os.Args[0], Env: map[string]string{fakeModeEnv: "hang"}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Start = %v", err)
	}
}

func TestWrongFirstEvent(t *testing.T) {
	h, err := startFake(t, "wrong-first", Options{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.Initialize(t.Context(), &wire.HarnessConfig{})
	if _, ok := errors.AsType[*StartError](err); !ok || !strings.Contains(err.Error(), "not an initialize response") {
		t.Fatalf("Initialize = %v", err)
	}
	select {
	case <-h.Exited():
	case <-time.After(5 * time.Second):
		t.Fatal("process not killed after a failed Initialize")
	}
}

func TestCloseTerminatesStuckProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("relies on SIGTERM")
	}
	h, err := startFake(t, "ignore-stdin", Options{ShutdownTimeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("Close took %v", d)
	}
	select {
	case <-h.Exited():
	default:
		t.Fatal("process still running")
	}
}

func TestFindBinary(t *testing.T) {
	dir := t.TempDir()
	onPath := filepath.Join(dir, BinaryName)
	if runtime.GOOS == "windows" {
		onPath += ".exe"
	}
	if err := os.WriteFile(onPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv(EnvBinaryPath, "")

	if got, err := FindBinary(nil); err != nil || got != onPath {
		t.Errorf("PATH lookup = %q, %v", got, err)
	}
	t.Setenv(EnvBinaryPath, "/from/os/env")
	if got, _ := FindBinary(nil); got != "/from/os/env" {
		t.Errorf("os env lookup = %q", got)
	}
	if got, _ := FindBinary(map[string]string{EnvBinaryPath: "/from/options"}); got != "/from/options" {
		t.Errorf("options env lookup = %q", got)
	}
	t.Setenv(EnvBinaryPath, "")
	t.Setenv("PATH", t.TempDir())
	if _, err := FindBinary(nil); !errors.Is(err, ErrBinaryNotFound) {
		t.Errorf("missing binary = %v", err)
	}
}

func TestDefaultClientInfo(t *testing.T) {
	ci := DefaultClientInfo()
	if ci.GetLanguage() != "go" || ci.GetOS() != runtime.GOOS || ci.GetVersion() == "" ||
		strings.HasPrefix(ci.GetLanguageVersion(), "go") {
		t.Fatalf("client info = %+v", ci)
	}
	if runtime.GOOS == "darwin" || runtime.GOOS == "linux" {
		if ci.GetOSVersion() == "" {
			t.Error("empty OS version")
		}
	}
}
