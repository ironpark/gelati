package gelati

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"iter"
	"slices"
	"sync/atomic"
	"testing"
)

// fakeProvider opens fakeConns whose turns replay events.
type fakeProvider struct {
	events    []Event
	result    *Result
	resultErr error
	// block makes Result wait for its ctx.
	block bool
	cfg   Config
	conn  *fakeConn
}

func (p *fakeProvider) Name() string { return "fake" }

func (p *fakeProvider) Open(_ context.Context, cfg Config) (Conn, error) {
	p.cfg = cfg
	p.conn = &fakeConn{p: p, done: make(chan struct{})}
	return p.conn, nil
}

type fakeConn struct {
	p       *fakeProvider
	done    chan struct{}
	closed  atomic.Bool
	sent    [][]Input
	cancels atomic.Int32
}

func (c *fakeConn) Send(_ context.Context, input []Input) (TurnConn, error) {
	c.sent = append(c.sent, input)
	return &fakeTurn{c: c}, nil
}

func (c *fakeConn) ID() string            { return "conv-1" }
func (c *fakeConn) Native() any           { return c }
func (c *fakeConn) Done() <-chan struct{} { return c.done }
func (c *fakeConn) Err() error            { return nil }
func (c *fakeConn) Close() error {
	if c.closed.CompareAndSwap(false, true) {
		close(c.done)
	}
	return nil
}

type fakeTurn struct {
	c      *fakeConn
	closed atomic.Bool
}

func (t *fakeTurn) Events(context.Context) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		for _, ev := range t.c.p.events {
			if !yield(ev, nil) {
				return
			}
		}
	}
}

func (t *fakeTurn) Result(ctx context.Context) (*Result, error) {
	if t.c.p.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return t.c.p.result, t.c.p.resultErr
}

func (t *fakeTurn) Cancel(context.Context) error {
	t.c.cancels.Add(1)
	return nil
}

func (t *fakeTurn) Close() error {
	t.closed.Store(true)
	return nil
}

func TestOpenAndRun(t *testing.T) {
	p := &fakeProvider{result: &Result{Text: "4"}}
	a, err := Open(t.Context(), p, Config{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if p.cfg.Model != "m" || a.Provider() != "fake" || a.ID() != "conv-1" || a.Native() != p.conn {
		t.Fatalf("agent = %+v, cfg = %+v", a, p.cfg)
	}
	res, err := a.Run(t.Context(), Text("2+2?"))
	if err != nil || res.Text != "4" {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	if got := p.conn.sent[0]; len(got) != 1 || got[0] != (TextInput{Text: "2+2?"}) {
		t.Fatalf("sent %v", got)
	}
	if _, err := a.Send(t.Context()); err == nil {
		t.Fatal("Send without input succeeded")
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-a.Done():
	default:
		t.Fatal("Done not closed after Close")
	}
	if _, err := Open(t.Context(), nil, Config{}); err == nil {
		t.Fatal("Open without a provider succeeded")
	}
}

func TestPackageRunClosesSession(t *testing.T) {
	p := &fakeProvider{result: &Result{Text: "ok"}}
	res, err := Run(t.Context(), p, "hi", Config{})
	if err != nil || res.Text != "ok" || !p.conn.closed.Load() {
		t.Fatalf("Run = %+v, %v (closed %v)", res, err, p.conn.closed.Load())
	}
}

func TestTurnText(t *testing.T) {
	p := &fakeProvider{
		events: []Event{
			{Kind: EventThoughtDelta, Text: "hmm"},
			{Kind: EventTextDelta, Text: "Hel"},
			{Kind: EventToolCall, Tool: &ToolCall{Name: "Bash"}},
			{Kind: EventTextDelta, Text: "lo"},
		},
		result: &Result{Text: "Hello"},
	}
	a, _ := Open(t.Context(), p, Config{})
	turn, err := a.Send(t.Context(), Text("hi"))
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for text, err := range turn.Text(t.Context()) {
		if err != nil {
			t.Fatal(err)
		}
		texts = append(texts, text)
	}
	if !slices.Equal(texts, []string{"Hel", "lo"}) {
		t.Fatalf("texts = %q", texts)
	}

	// A failed turn ends Text with the error Result reports.
	failed := errors.New("turn failed")
	p.resultErr = failed
	turn, _ = a.Send(t.Context(), Text("again"))
	var last error
	for _, err := range turn.Text(t.Context()) {
		last = err
	}
	if !errors.Is(last, failed) {
		t.Fatalf("final error = %v", last)
	}
}

func TestAgentRunCancelsOnContextEnd(t *testing.T) {
	p := &fakeProvider{block: true}
	a, _ := Open(t.Context(), p, Config{})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := a.Run(ctx, Text("slow")); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v", err)
	}
	if p.conn.cancels.Load() != 1 {
		t.Fatalf("cancels = %d", p.conn.cancels.Load())
	}
}

func TestResultDecodeStructuredOutput(t *testing.T) {
	var v struct {
		Answer int `json:"answer"`
	}
	if err := (&Result{StructuredOutput: []byte(`{"answer":4}`)}).DecodeStructuredOutput(&v); err != nil || v.Answer != 4 {
		t.Fatalf("decode = %+v, %v", v, err)
	}
	if err := (&Result{}).DecodeStructuredOutput(&v); err == nil {
		t.Fatal("decoded a result without structured output")
	}
}

func TestNewTool(t *testing.T) {
	type args struct {
		A int `json:"a"`
		B int `json:"b,omitempty"`
	}
	sum := NewTool("sum", "Adds", func(_ context.Context, in args) (map[string]int, error) {
		return map[string]int{"sum": in.A + in.B}, nil
	})
	if sum.Name != "sum" || sum.Description != "Adds" || !slices.Equal(sum.InputSchema["required"].([]any), []any{"a"}) {
		t.Fatalf("tool = %+v", sum)
	}
	if got, err := sum.Run(t.Context(), jsontext.Value(`{"a":2,"b":3}`)); err != nil || got != `{"sum":5}` {
		t.Fatalf("Run = %q, %v", got, err)
	}
	if _, err := sum.Run(t.Context(), jsontext.Value(`{"a":"x"}`)); err == nil {
		t.Fatal("Run with bad arguments succeeded")
	}

	echo := NewTool("echo", "", func(_ context.Context, in struct{ S string }) (string, error) { return in.S, nil })
	if got, err := echo.Run(t.Context(), nil); err != nil || got != "" {
		t.Fatalf("Run without arguments = %q, %v", got, err)
	}
	if got, _ := echo.Run(t.Context(), jsontext.Value(`{"S":"\"quoted\""}`)); got != `"quoted"` {
		t.Fatalf("a string result is not sent as is: %q", got)
	}
	failed := errors.New("failed")
	bad := NewTool("bad", "", func(context.Context, struct{}) (int, error) { return 0, failed })
	if _, err := bad.Run(t.Context(), nil); !errors.Is(err, failed) {
		t.Fatalf("Run error = %v", err)
	}
}

func TestOpenChecksTools(t *testing.T) {
	run := func(context.Context, jsontext.Value) (string, error) { return "", nil }
	for name, tools := range map[string][]Tool{
		"unnamed":   {{Run: run}},
		"no run":    {{Name: "a"}},
		"duplicate": {{Name: "a", Run: run}, {Name: "a", Run: run}},
	} {
		if _, err := Open(t.Context(), &fakeProvider{}, Config{Tools: tools}); err == nil {
			t.Errorf("%s: Open succeeded", name)
		}
	}
}

func TestImage(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\n")
	if got := Image(png, "").(ImageInput); got.MIMEType != "image/png" || string(got.Data) != string(png) {
		t.Fatalf("Image = %+v", got)
	}
	if got := Image(png, "image/webp").(ImageInput); got.MIMEType != "image/webp" {
		t.Fatalf("Image = %+v", got)
	}
}
