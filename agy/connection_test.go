package agy

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ironpark/gelati/agy/internal/harness"
	"github.com/ironpark/gelati/agy/internal/wire"
)

func TestConnectionStartsIdleWithHistory(t *testing.T) {
	hist := []*Step{{StepIndex: 1, Content: "Historical text", Status: StepStatusDone, Source: StepSourceModel}}
	c, _ := newTestConnection(t, connectionOptions{initialHistory: hist})
	if !c.IsIdle() {
		t.Fatal("new connection is not idle")
	}
	if h := c.InitialHistory(); len(h) != 1 || h[0].Content != "Historical text" {
		t.Fatalf("InitialHistory = %+v", h)
	}
	// An idle connection with nothing queued yields nothing.
	steps, err := collectSteps(t, c.ReceiveSteps(t.Context()))
	if err != nil || len(steps) != 0 {
		t.Fatalf("ReceiveSteps on idle = %v, %v", steps, err)
	}
}

func TestReceiveStepsBasic(t *testing.T) {
	c, tr := newTestConnection(t, connectionOptions{})
	startTurn(c)
	tr.emit(stepEvent(wire.StepUpdate_builder{
		StepIndex: new(uint32(1)),
		Text:      new("Hello world"),
		State:     new(wire.StepUpdate_STATE_ACTIVE),
		Source:    new(wire.StepUpdate_SOURCE_MODEL),
	}.Build()), idleEvent("my_cascade", ""))
	steps, err := collectSteps(t, c.ReceiveSteps(t.Context()))
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || steps[0].Content != "Hello world" || steps[0].Status != StepStatusActive || steps[0].Source != StepSourceModel {
		t.Fatalf("steps = %+v", steps)
	}
}

func systemErrorStep(code uint32, msg string) *wire.OutputEvent {
	return stepEvent(wire.StepUpdate_builder{
		TrajectoryId: new("my_cascade"),
		StepIndex:    new(uint32(1)),
		Error:        wire.ActionError_builder{ErrorMessage: new(msg), HttpCode: new(code)}.Build(),
		State:        new(wire.StepUpdate_STATE_ERROR),
		Source:       new(wire.StepUpdate_SOURCE_SYSTEM),
	}.Build())
}

func TestReceiveStepsFatalSystemErrors(t *testing.T) {
	for _, code := range []uint32{400, 401, 403} {
		c, tr := newTestConnection(t, connectionOptions{})
		startTurn(c)
		tr.emit(systemErrorStep(code, "Fatal system failure"))
		tr.hangUp()
		steps, err := collectSteps(t, c.ReceiveSteps(t.Context()))
		ce, ok := errors.AsType[*ConnectionError](err)
		if !ok || ce.Message != "Fatal system failure" {
			t.Fatalf("code %d: err = %v, want ConnectionError", code, err)
		}
		// The failing step itself is yielded first.
		if len(steps) != 1 || steps[0].HTTPCode != int(code) {
			t.Fatalf("code %d: steps = %+v", code, steps)
		}
	}
}

func TestReceiveStepsNonFatalSystemErrorThenIdleError(t *testing.T) {
	c, tr := newTestConnection(t, connectionOptions{})
	if err := c.Send(t.Context(), Text("Hello")); err != nil {
		t.Fatal(err)
	}
	if got := tr.next(t).GetUserInput().GetParts()[0].GetText(); got != "Hello" {
		t.Fatalf("sent text %q", got)
	}
	tr.emit(systemErrorStep(429, "Resource exhausted"), idleEvent("my_cascade", "executor run failed: Resource exhausted"))
	tr.hangUp()
	steps, err := collectSteps(t, c.ReceiveSteps(t.Context()))
	if ee, ok := errors.AsType[*ExecutionError](err); !ok || ee.Message != "executor run failed: Resource exhausted" {
		t.Fatalf("err = %v, want ExecutionError", err)
	}
	if len(steps) != 1 || steps[0].Error != "Resource exhausted" || steps[0].Status != StepStatusError {
		t.Fatalf("steps = %+v", steps)
	}
}

func TestReceiveStepsTrajectoryError(t *testing.T) {
	c, tr := newTestConnection(t, connectionOptions{})
	if err := c.Send(t.Context(), Text("Hello")); err != nil {
		t.Fatal(err)
	}
	tr.next(t)
	tr.emit(modelText("my_cascade", 1, wire.StepUpdate_STATE_ACTIVE, wire.StepUpdate_TARGET_USER, "I'm working"),
		idleEvent("my_cascade", "Trajectory execution failed"))
	steps, err := collectSteps(t, c.ReceiveSteps(t.Context()))
	if _, ok := errors.AsType[*ExecutionError](err); !ok || !strings.Contains(err.Error(), "Trajectory execution failed") {
		t.Fatalf("err = %v", err)
	}
	if len(steps) != 1 || steps[0].Content != "I'm working" {
		t.Fatalf("steps = %+v", steps)
	}
}

func TestReceiveStepsMultipleIdleUpdates(t *testing.T) {
	c, tr := newTestConnection(t, connectionOptions{})
	if err := c.Send(t.Context(), Text("Hello")); err != nil {
		t.Fatal(err)
	}
	tr.next(t)
	tr.emit(idleEvent("my_cascade", ""),
		modelText("my_cascade", 1, wire.StepUpdate_STATE_DONE, wire.StepUpdate_TARGET_USER, "Step content after idle"),
		idleEvent("my_cascade", ""))
	// The reader may run before all events are processed; wait for them.
	waitFor(t, "events processed", func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return len(c.cur.queue) == 1
	})
	steps, err := collectSteps(t, c.ReceiveSteps(t.Context()))
	if err != nil || len(steps) != 1 || steps[0].Content != "Step content after idle" {
		t.Fatalf("steps = %+v, err %v", steps, err)
	}
}

func TestCancelEndsTurnWithCancelledError(t *testing.T) {
	c, tr := newTestConnection(t, connectionOptions{})
	if err := c.Send(t.Context(), Text("Hello")); err != nil {
		t.Fatal(err)
	}
	tr.next(t)
	tr.emit(modelText("my_cascade", 1, wire.StepUpdate_STATE_ACTIVE, wire.StepUpdate_TARGET_USER, "I'm working"))

	type result struct {
		steps []*Step
		err   error
	}
	done := make(chan result, 1)
	go func() {
		var r result
		for s, err := range c.ReceiveSteps(context.Background()) {
			if err != nil {
				r.err = err
				break
			}
			r.steps = append(r.steps, s)
		}
		done <- r
	}()
	time.Sleep(50 * time.Millisecond)
	if err := c.Cancel(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !tr.next(t).GetHaltRequest() {
		t.Fatal("no halt request sent")
	}
	tr.emit(idleEvent("my_cascade", ""))
	r := <-done
	if len(r.steps) != 1 || r.steps[0].Content != "I'm working" {
		t.Fatalf("steps = %+v", r.steps)
	}
	if _, ok := errors.AsType[*CancelledError](r.err); !ok || !errors.Is(r.err, context.Canceled) {
		t.Fatalf("err = %v, want CancelledError matching context.Canceled", r.err)
	}
}

func TestConcurrentReceiveSteps(t *testing.T) {
	c, tr := newTestConnection(t, connectionOptions{})
	startTurn(c)
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range c.ReceiveSteps(context.Background()) {
			close(started)
		}
	}()
	tr.emit(modelText("t", 1, wire.StepUpdate_STATE_ACTIVE, wire.StepUpdate_TARGET_USER, "x"))
	<-started
	_, err := collectSteps(t, c.ReceiveSteps(t.Context()))
	if !errors.Is(err, ErrConcurrentReceive) {
		t.Fatalf("second reader err = %v", err)
	}
	tr.emit(idleEvent("t", ""))
	<-done
	// Once the first reader is done, a new one may start.
	if _, err := collectSteps(t, c.ReceiveSteps(t.Context())); err != nil {
		t.Fatal(err)
	}
}

func TestWaitForIdleMultipleWaiters(t *testing.T) {
	c, tr := newTestConnection(t, connectionOptions{})
	startTurn(c)
	tr.emit(modelText("parent_traj", 1, wire.StepUpdate_STATE_ACTIVE, wire.StepUpdate_TARGET_USER, "Hello"))
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			if err := c.WaitForIdle(context.Background()); err != nil {
				t.Error(err)
			}
		})
	}
	time.Sleep(50 * time.Millisecond)
	tr.emit(idleEvent("parent_traj", ""))
	waited := make(chan struct{})
	go func() { wg.Wait(); close(waited) }()
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("WaitForIdle deadlocked")
	}
}

func TestWaitForIdleFailsWhenHarnessGone(t *testing.T) {
	c, tr := newTestConnection(t, connectionOptions{})
	startTurn(c)
	tr.hangUp()
	err := c.WaitForIdle(t.Context())
	if _, ok := errors.AsType[*ConnectionError](err); !ok {
		t.Fatalf("WaitForIdle = %v", err)
	}
}

func TestStopReasonTrackedPerTurn(t *testing.T) {
	c, tr := newTestConnection(t, connectionOptions{})
	if got := c.LastTurnStopReason(); got != StopReasonUnspecified {
		t.Fatalf("initial stop reason %s", got)
	}
	startTurn(c)
	tr.emit(wire.OutputEvent_builder{TrajectoryStateUpdate: wire.TrajectoryStateUpdate_builder{
		TrajectoryId: new("main"),
		State:        new(wire.TrajectoryStateUpdate_STATE_FULLY_IDLE),
		StopReason:   new(wire.TrajectoryStateUpdate_STOP_REASON_QUOTA_EXHAUSTED),
	}.Build()}.Build())
	waitFor(t, "stop reason", func() bool { return c.LastTurnStopReason() == StopReasonQuotaExhausted })
	if err := c.Send(t.Context(), Text("again")); err != nil {
		t.Fatal(err)
	}
	if got := c.LastTurnStopReason(); got != StopReasonUnspecified {
		t.Fatalf("stop reason after send = %s", got)
	}
}

func TestTrajectoryStateIdleTransitions(t *testing.T) {
	c, tr := newTestConnection(t, connectionOptions{})
	// RUNNING clears idle.
	tr.emit(wire.OutputEvent_builder{TrajectoryStateUpdate: wire.TrajectoryStateUpdate_builder{TrajectoryId: new("main"), State: new(wire.TrajectoryStateUpdate_STATE_RUNNING)}.Build()}.Build())
	waitFor(t, "not idle", func() bool { return !c.IsIdle() })

	// Once the main trajectory is known, subagent states are ignored.
	tr.emit(modelText("main", 1, wire.StepUpdate_STATE_ACTIVE, wire.StepUpdate_TARGET_USER, "x"))
	waitFor(t, "main trajectory", func() bool { return c.ConversationID() == "main" })
	tr.emit(idleEvent("subagent", "Subagent failure"))
	tr.emit(wire.OutputEvent_builder{TrajectoryStateUpdate: wire.TrajectoryStateUpdate_builder{TrajectoryId: new("main"), State: new(wire.TrajectoryStateUpdate_STATE_CANCELLED), Error: new("Cancelled by user")}.Build()}.Build())
	steps, err := collectSteps(t, c.ReceiveSteps(t.Context()))
	if len(steps) != 1 {
		t.Fatalf("steps = %+v", steps)
	}
	if ee, ok := errors.AsType[*ExecutionError](err); !ok || ee.Message != "Cancelled by user" {
		t.Fatalf("err = %v, want the cancellation, not the subagent failure", err)
	}
	if !c.IsIdle() {
		t.Fatal("not idle after CANCELLED")
	}
}

func TestUsageUpdates(t *testing.T) {
	c, tr := newTestConnection(t, connectionOptions{initialUsage: &UsageMetadata{PromptTokenCount: new(int64(1))}})
	if got := c.CumulativeUsage(); val(got.PromptTokenCount) != 1 {
		t.Fatalf("initial usage %+v", got)
	}
	u := func(p, total uint64) *wire.UsageMetadata {
		return wire.UsageMetadata_builder{PromptTokenCount: new(uint64(p)), TotalTokenCount: new(uint64(total))}.Build()
	}
	tr.emit(wire.OutputEvent_builder{UsageUpdate: wire.UsageUpdate_builder{
		Agents: []*wire.TrajectoryUsageEntry{
			wire.TrajectoryUsageEntry_builder{TrajectoryId: new("main_traj"), Usage: u(120, 150)}.Build(),
			wire.TrajectoryUsageEntry_builder{TrajectoryId: new("subagent_1"), Usage: u(180, 250)}.Build(),
		},
		Total: u(300, 400),
	}.Build()}.Build())
	waitFor(t, "usage", func() bool { return val(c.CumulativeUsage().TotalTokenCount) == 400 })
	tu := c.TrajectoryUsages()
	if val(tu["main_traj"].PromptTokenCount) != 120 || val(tu["subagent_1"].TotalTokenCount) != 250 {
		t.Fatalf("trajectory usages %+v", tu)
	}
}

func TestSendContent(t *testing.T) {
	c, tr := newTestConnection(t, connectionOptions{})
	ctx := t.Context()
	sentJSON := func() map[string]any {
		b, err := wire.Marshal(tr.next(t))
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		return m["userInput"].(map[string]any)
	}
	parts := func(m map[string]any) []any { return m["parts"].([]any) }

	if err := c.Send(ctx, Text("Standard text prompt")); err != nil {
		t.Fatal(err)
	}
	if p := parts(sentJSON()); len(p) != 1 || p[0].(map[string]any)["text"] != "Standard text prompt" {
		t.Fatalf("text parts %v", p)
	}

	if err := c.Send(ctx); err != nil {
		t.Fatal(err)
	}
	if p := parts(sentJSON()); len(p) != 1 || p[0].(map[string]any)["text"] != "" {
		t.Fatalf("empty prompt parts %v", p)
	}

	if err := c.Send(ctx, &Image{MIMEType: "image/png", Data: []byte("fake_png"), Description: "logo image"}); err != nil {
		t.Fatal(err)
	}
	media := parts(sentJSON())[0].(map[string]any)["media"].(map[string]any)
	if media["mimeType"] != "image/png" || media["description"] != "logo image" || media["data"] != "ZmFrZV9wbmc=" {
		t.Fatalf("media %v", media)
	}

	if err := c.Send(ctx, Text("Context text instruction."), Document{MIMEType: "application/pdf", Data: []byte("fake_pdf")}); err != nil {
		t.Fatal(err)
	}
	p := parts(sentJSON())
	if len(p) != 2 || p[1].(map[string]any)["media"].(map[string]any)["data"] != "ZmFrZV9wZGY=" {
		t.Fatalf("mixed parts %v", p)
	}

	if err := c.Send(ctx, SlashCommandPlan); err != nil {
		t.Fatal(err)
	}
	if p := parts(sentJSON()); p[0].(map[string]any)["slashCommand"].(map[string]any)["name"] != "plan" {
		t.Fatalf("slash parts %v", p)
	}

	if err := c.Send(ctx, Text("Bad\x00Input\x7f")); err != nil {
		t.Fatal(err)
	}
	if got := parts(sentJSON())[0].(map[string]any)["text"]; got != "Bad Input " {
		t.Fatalf("sanitized text %q", got)
	}

	if err := c.Send(ctx, Image{MIMEType: "image/gif"}); err == nil {
		t.Fatal("unsupported media accepted")
	}
}

func TestTriggerNotificationWhileBusy(t *testing.T) {
	c, tr := newTestConnection(t, connectionOptions{})
	ctx := t.Context()
	if err := c.Send(ctx, Text("initial prompt")); err != nil {
		t.Fatal(err)
	}
	tr.next(t)
	if err := c.SendTriggerNotification(ctx, "trigger content"); err != nil {
		t.Fatal(err)
	}
	if got := tr.next(t).GetAutomatedTrigger(); got != "trigger content" {
		t.Fatalf("trigger %q", got)
	}
	if err := c.Send(ctx, Text("follow-up prompt")); err != nil {
		t.Fatal(err)
	}
	if got := tr.next(t).GetUserInput().GetParts()[0].GetText(); got != "follow-up prompt" {
		t.Fatalf("follow-up %q", got)
	}
}

func TestUnexpectedCloseSurfacesStderr(t *testing.T) {
	c, tr := newTestConnection(t, connectionOptions{})
	startTurn(c)
	tr.errs <- &harness.ConnectionError{Code: 1006, Stderr: "panic: boom"}
	_, err := collectSteps(t, c.ReceiveSteps(t.Context()))
	ce, ok := errors.AsType[*ConnectionError](err)
	if !ok || ce.Code != 1006 || !strings.Contains(ce.Stderr, "panic: boom") || !strings.Contains(err.Error(), "panic: boom") {
		t.Fatalf("err = %v", err)
	}
}

func TestExpectedCloseIsNotAnError(t *testing.T) {
	c, _ := newTestConnection(t, connectionOptions{})
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	err := c.cur.err
	c.mu.Unlock()
	if err != nil {
		t.Fatalf("turn ended with %v on close", err)
	}
	if err := c.Send(t.Context(), Text("x")); !errors.Is(err, ErrClosed) {
		t.Fatalf("Send after Close = %v", err)
	}
}

func TestToolCallRunsCustomTool(t *testing.T) {
	type args struct {
		Name string `json:"name"`
	}
	greet := NewTool("greet", "Greets.", func(_ context.Context, _ *ToolContext, in args) (string, error) {
		return "Hello " + in.Name, nil
	})
	c, tr := newTestConnection(t, connectionOptions{tools: mustTools(t, greet)})
	startTurn(c)
	tr.emit(wire.OutputEvent_builder{ToolCall: wire.ToolCall_builder{
		Id: new("call_1"), Name: new("greet"), ArgumentsJson: new(`{"name": "Ada"}`), TrajectoryId: new("traj_sub"),
	}.Build()}.Build())
	resp := tr.next(t).GetToolResponse()
	if resp.GetId() != "call_1" || resp.GetResponseJson() != `{"result":"Hello Ada"}` {
		t.Fatalf("tool response %+v %s", resp, resp.GetResponseJson())
	}
	// The call is also reported as a step.
	tr.emit(idleEvent("", ""))
	steps, err := collectSteps(t, c.ReceiveSteps(t.Context()))
	if err != nil || len(steps) != 1 {
		t.Fatalf("steps %+v, %v", steps, err)
	}
	s := steps[0]
	if s.ID != "call_1" || s.TrajectoryID != "traj_sub" || s.StepIndex != 1 || s.Type != StepTypeToolCall ||
		s.Source != StepSourceModel || s.Target != StepTargetEnvironment || s.Status != StepStatusActive ||
		len(s.ToolCalls) != 1 || s.ToolCalls[0].ID != "call_1" || !reflect.DeepEqual(s.ToolCalls[0].Args, map[string]any{"name": "Ada"}) {
		t.Fatalf("tool call step %+v", s)
	}
}

func TestToolCallArguments(t *testing.T) {
	st, _ := wire.StructOf(map[string]any{"query": "SELECT 1", "limit": 10})
	mode, _ := wire.StructOf(map[string]any{"mode": "structured"})
	for _, tc := range []struct {
		name string
		call *wire.ToolCall
		want map[string]any
	}{
		{"struct", wire.ToolCall_builder{Arguments: st}.Build(), map[string]any{"query": "SELECT 1", "limit": 10.0}},
		{"struct wins", wire.ToolCall_builder{Arguments: mode, ArgumentsJson: new(`{"mode": "stringified"}`)}.Build(), map[string]any{"mode": "structured"}},
		{"json", wire.ToolCall_builder{ArgumentsJson: new(`{"path": "README.md"}`)}.Build(), map[string]any{"path": "README.md"}},
		{"invalid json", wire.ToolCall_builder{ArgumentsJson: new("{invalid_json}")}.Build(), map[string]any{}},
		{"number", wire.ToolCall_builder{ArgumentsJson: new("123")}.Build(), map[string]any{}},
		{"null", wire.ToolCall_builder{ArgumentsJson: new("null")}.Build(), map[string]any{}},
		{"string", wire.ToolCall_builder{ArgumentsJson: new(`"string"`)}.Build(), map[string]any{}},
		{"list", wire.ToolCall_builder{ArgumentsJson: new("[1, 2]")}.Build(), map[string]any{}},
		{"none", &wire.ToolCall{}, map[string]any{}},
	} {
		if got := toolCallArgs(tc.call); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: args = %#v, want %#v", tc.name, got, tc.want)
		}
	}
}

func TestToolCallWithoutRunnerQueuesStepOnly(t *testing.T) {
	c, tr := newTestConnection(t, connectionOptions{})
	startTurn(c)
	tr.emit(wire.OutputEvent_builder{ToolCall: wire.ToolCall_builder{Id: new("call_99"), Name: new("custom_tool"), ArgumentsJson: new(`{"key": "value"}`)}.Build()}.Build())
	waitFor(t, "queued step", func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return len(c.cur.queue) == 1
	})
	c.mu.Lock()
	s := c.cur.queue[0]
	c.mu.Unlock()
	if s.Type != StepTypeToolCall || s.ToolCalls[0].Name != "custom_tool" || s.ToolCalls[0].ID != "call_99" {
		t.Fatalf("step %+v", s)
	}
	tr.expectNothingSent(t, 100*time.Millisecond)
}

func TestToolErrorsAndMediaResults(t *testing.T) {
	jpeg := []byte{0xff, 0xd8, 0xff, 0xd9}
	tools := mustTools(t,
		NewToolWithSchema("failing_tool", "", nil, func(context.Context, *ToolContext, map[string]any) (any, error) {
			return nil, errors.New("Intentional failure")
		}),
		NewToolWithSchema("image_tool", "", nil, func(context.Context, *ToolContext, map[string]any) (any, error) {
			return []any{"here is the snapshot", &Image{Data: jpeg, MIMEType: "image/jpeg"}}, nil
		}),
		NewToolWithSchema("photo_tool", "", nil, func(context.Context, *ToolContext, map[string]any) (any, error) {
			return Image{Data: jpeg, MIMEType: "image/jpeg", Description: "a deck photo"}, nil
		}),
		NewToolWithSchema("large_tool", "", nil, func(context.Context, *ToolContext, map[string]any) (any, error) {
			return strings.Repeat("X", 5<<20), nil
		}),
		NewToolWithSchema("panicky", "", nil, func(context.Context, *ToolContext, map[string]any) (any, error) {
			panic("kaboom")
		}),
	)
	_, tr := newTestConnection(t, connectionOptions{tools: tools})
	call := func(name string) *wire.ToolResponse {
		tr.emit(wire.OutputEvent_builder{ToolCall: wire.ToolCall_builder{Id: new("call_" + name), Name: new(name), ArgumentsJson: new("{}")}.Build()}.Build())
		r := tr.next(t).GetToolResponse()
		if r.GetId() != "call_"+name {
			t.Fatalf("response id %q", r.GetId())
		}
		return r
	}
	if r := call("failing_tool"); !strings.Contains(r.GetErrorMessage(), "Intentional failure") {
		t.Fatalf("failing tool response %+v", r)
	}
	if r := call("panicky"); !strings.Contains(r.GetErrorMessage(), "kaboom") {
		t.Fatalf("panicking tool response %+v", r)
	}
	r := call("image_tool")
	if !strings.Contains(r.GetResponseJson(), "here is the snapshot") || len(r.GetSupplementalMedia()) != 1 ||
		r.GetSupplementalMedia()[0].GetMimeType() != "image/jpeg" || string(r.GetSupplementalMedia()[0].GetData()) != string(jpeg) {
		t.Fatalf("image tool response %+v", r)
	}
	r = call("photo_tool")
	if !strings.Contains(r.GetResponseJson(), "Returned 1 media attachment(s)") || r.GetSupplementalMedia()[0].GetDescription() != "a deck photo" {
		t.Fatalf("photo tool response %+v", r)
	}
	if r := call("large_tool"); len(r.GetResponseJson()) < 5<<20 {
		t.Fatal("large result truncated")
	}
	if r := call("unknown_tool"); r.GetErrorMessage() != "Unknown tool: 'unknown_tool'" {
		t.Fatalf("unknown tool response %+v", r)
	}
}

func TestExtractMedia(t *testing.T) {
	img := &Image{Data: []byte{1}, MIMEType: "image/jpeg"}
	if c, m := extractMedia(img); c != nil || len(m) != 1 {
		t.Fatalf("bare media: %v %v", c, m)
	}
	if c, m := extractMedia([]any{"a", img, "b"}); !reflect.DeepEqual(c, []any{"a", "b"}) || len(m) != 1 {
		t.Fatalf("list: %v %v", c, m)
	}
	if c, m := extractMedia(map[string]any{"k": "v"}); !reflect.DeepEqual(c, map[string]any{"k": "v"}) || len(m) != 0 {
		t.Fatalf("map: %v %v", c, m)
	}
	if c, m := extractMedia(map[string]any{"caption": "hi", "img": img}); !reflect.DeepEqual(c, map[string]any{"caption": "hi"}) || len(m) != 1 {
		t.Fatalf("map with media: %v %v", c, m)
	}
	if c, m := extractMedia([]Media{img, Audio{MIMEType: "audio/wav"}}); c != nil || len(m) != 2 {
		t.Fatalf("typed slice: %v %v", c, m)
	}
}

func TestToolResultPayload(t *testing.T) {
	type out struct {
		A int `json:"a"`
	}
	for _, tc := range []struct {
		res  *ToolResult
		want string
	}{
		{&ToolResult{Result: "x"}, `{"result":"x"}`},
		{&ToolResult{Result: map[string]any{"k": 1}}, `{"k":1}`},
		{&ToolResult{Result: out{A: 2}}, `{"a":2}`},
		{&ToolResult{Result: []int{1, 2}}, `{"result":[1,2]}`},
		{&ToolResult{Result: nil}, `{"result":null}`},
		{&ToolResult{Result: make(chan int)}, `{"result":"`},
		{&ToolResult{Error: "bad"}, `{"error":"bad"}`},
	} {
		b, _ := json.Marshal(toolResultPayload(tc.res))
		if !strings.HasPrefix(string(b), tc.want) {
			t.Errorf("payload of %+v = %s, want %s", tc.res, b, tc.want)
		}
	}
}

func TestLocalCustomToolStepSuppressed(t *testing.T) {
	local := NewToolWithSchema("my_local_tool", "", nil, func(context.Context, *ToolContext, map[string]any) (any, error) { return nil, nil })
	c, tr := newTestConnection(t, connectionOptions{tools: mustTools(t, local)})
	startTurn(c)
	custom := func(name string, idx uint32) *wire.OutputEvent {
		return stepEvent(wire.StepUpdate_builder{
			TrajectoryId: new("traj_123"), StepIndex: new(idx),
			Source: new(wire.StepUpdate_SOURCE_MODEL), State: new(wire.StepUpdate_STATE_DONE),
			CustomTool: wire.ActionCustomTool_builder{ToolCall: wire.ToolCall_builder{Name: new(name), ArgumentsJson: new("{}")}.Build()}.Build(),
		}.Build())
	}
	tr.emit(custom("my_local_tool", 5), custom("my_remote_tool", 6), idleEvent("traj_123", ""))
	steps, err := collectSteps(t, c.ReceiveSteps(t.Context()))
	if err != nil || len(steps) != 2 {
		t.Fatalf("steps %+v, %v", steps, err)
	}
	if steps[0].Type != StepTypeToolCall || len(steps[0].ToolCalls) != 0 {
		t.Fatalf("local tool step %+v", steps[0])
	}
	if len(steps[1].ToolCalls) != 1 || steps[1].ToolCalls[0].Name != "my_remote_tool" {
		t.Fatalf("remote tool step %+v", steps[1])
	}
}

func TestToolConfirmationAlwaysAccepted(t *testing.T) {
	deny := PreToolCallHook(func(context.Context, *HookContext, *ToolCall) (HookResult, error) {
		return HookResult{Deny: true, Message: "Denied"}, nil
	})
	c, tr := newTestConnection(t, connectionOptions{hooks: mustHooks(t, deny)})
	startTurn(c)
	ev := stepEvent(wire.StepUpdate_builder{
		TrajectoryId: new("traj"), StepIndex: new(uint32(0)),
		Text:                    new(`Requesting permission to call tool "run_command"`),
		State:                   new(wire.StepUpdate_STATE_WAITING_FOR_USER),
		Source:                  new(wire.StepUpdate_SOURCE_MODEL),
		Target:                  new(wire.StepUpdate_TARGET_ENVIRONMENT),
		ToolConfirmationRequest: &wire.ToolConfirmationRequest{},
		RunCommand:              wire.ActionRunCommand_builder{CommandLine: new("rm -rf /")}.Build(),
	}.Build())
	tr.emit(ev, ev) // a repeated wait update is answered once
	conf := tr.next(t).GetToolConfirmation()
	if !conf.GetAccepted() || conf.GetTrajectoryId() != "traj" {
		t.Fatalf("confirmation %+v", conf)
	}
	tr.expectNothingSent(t, 100*time.Millisecond)
}

func TestQuestionRequests(t *testing.T) {
	answer := OnInteractionHook(func(_ context.Context, _ *HookContext, spec AskQuestionInteractionSpec) (*QuestionHookResult, error) {
		if len(spec.Questions) != 1 || spec.Questions[0].Question != "Do you agree?" || spec.Questions[0].Options[1].ID != "2" {
			return nil, errors.New("unexpected spec")
		}
		return &QuestionHookResult{Responses: []QuestionResponse{{SelectedOptionIDs: []string{"1", "x"}, FreeformResponse: "sure"}}}, nil
	})
	_, tr := newTestConnection(t, connectionOptions{hooks: mustHooks(t, answer)})
	ask := func(questions ...*wire.UserQuestion) *wire.UserQuestionsResponse {
		tr.emit(stepEvent(wire.StepUpdate_builder{
			TrajectoryId: new("test_traj"), StepIndex: new(uint32(1)),
			State:            new(wire.StepUpdate_STATE_WAITING_FOR_USER),
			QuestionsRequest: wire.UserQuestionsRequest_builder{Questions: questions}.Build(),
		}.Build()))
		return tr.next(t).GetQuestionResponse()
	}
	mc := wire.UserQuestion_builder{MultipleChoice: wire.MultipleChoice_builder{Question: new("Do you agree?"), Choices: []string{"Yes", "No"}}.Build()}.Build()
	resp := ask(mc, &wire.UserQuestion{})
	answers := resp.GetResponse().GetAnswers()
	if resp.GetTrajectoryId() != "test_traj" || len(answers) != 2 {
		t.Fatalf("response %+v", resp)
	}
	got := answers[0].GetMultipleChoiceAnswer()
	if !reflect.DeepEqual(got.GetSelectedChoiceIndices(), []int32{0}) || got.GetFreeformResponse() != "sure" {
		t.Fatalf("first answer %+v", got)
	}
	if !answers[1].GetUnanswered() {
		t.Fatalf("second answer %+v", answers[1])
	}
}

func TestQuestionRequestEmptyAndFailing(t *testing.T) {
	failing := OnInteractionHook(func(context.Context, *HookContext, AskQuestionInteractionSpec) (*QuestionHookResult, error) {
		return nil, errors.New("hook crashed")
	})
	c, tr := newTestConnection(t, connectionOptions{hooks: mustHooks(t, failing)})
	_ = c
	tr.emit(stepEvent(wire.StepUpdate_builder{
		TrajectoryId: new("a"), StepIndex: new(uint32(1)), State: new(wire.StepUpdate_STATE_WAITING_FOR_USER),
		QuestionsRequest: &wire.UserQuestionsRequest{},
	}.Build()))
	if b, _ := wire.Marshal(tr.next(t).GetQuestionResponse().GetResponse()); string(b) != "{}" {
		t.Fatalf("empty question response %s", b)
	}
	tr.emit(stepEvent(wire.StepUpdate_builder{
		TrajectoryId: new("b"), StepIndex: new(uint32(1)), State: new(wire.StepUpdate_STATE_WAITING_FOR_USER),
		QuestionsRequest: wire.UserQuestionsRequest_builder{Questions: []*wire.UserQuestion{
			wire.UserQuestion_builder{MultipleChoice: wire.MultipleChoice_builder{Question: new("q"), Choices: []string{"a"}}.Build()}.Build(),
		}}.Build(),
	}.Build()))
	ans := tr.next(t).GetQuestionResponse().GetResponse().GetAnswers()
	if len(ans) != 1 || !strings.Contains(ans[0].GetMultipleChoiceAnswer().GetFreeformResponse(), "SDK error processing question") {
		t.Fatalf("failing hook answers %+v", ans)
	}
}

func TestWaitingStepIsQueued(t *testing.T) {
	c, tr := newTestConnection(t, connectionOptions{})
	startTurn(c)
	tr.emit(stepEvent(wire.StepUpdate_builder{
		StepIndex: new(uint32(5)), TrajectoryId: new("ui_traj"),
		State: new(wire.StepUpdate_STATE_WAITING_FOR_USER), Text: new("Waiting for confirmation"),
	}.Build()))
	r, err := c.newReceiver(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.release()
	s, err := r.next(t.Context())
	if err != nil || s.ID != "ui_traj:5" || s.Status != StepStatusWaitingForUser || s.Content != "Waiting for confirmation" {
		t.Fatalf("step %+v, %v", s, err)
	}
}

func TestSessionEndHandshakeOnClose(t *testing.T) {
	var ended atomic.Bool
	hooks := mustHooks(t, OnSessionEndHook(func(context.Context, *HookContext) error {
		ended.Store(true)
		return nil
	}))
	tr := newFakeTransport()
	c := newConnection(tr, connectionOptions{hooks: hooks, logger: quietLogger()})
	closed := make(chan error, 1)
	go func() { closed <- c.Close() }()

	if !tr.next(t).GetSessionEndRequest() {
		t.Fatal("no session end request")
	}
	tr.emit(hookRequest("req_end", wire.LifecycleHook_LIFECYCLE_HOOK_ON_SESSION_END, nil))
	resp := tr.next(t).GetCallHookResponse()
	if resp.GetRequestId() != "req_end" || resp.GetEmptyResult() == nil {
		t.Fatalf("hook response %+v", resp)
	}
	tr.emit(wire.OutputEvent_builder{SessionEndResponse: new(true)}.Build())
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return")
	}
	if !ended.Load() {
		t.Fatal("session end hook did not run")
	}
}

func TestCloseSkipsSessionEndWhenHarnessGone(t *testing.T) {
	hooks := mustHooks(t, OnSessionEndHook(func(context.Context, *HookContext) error { return nil }))
	tr := newFakeTransport()
	c := newConnection(tr, connectionOptions{hooks: hooks, logger: quietLogger()})
	tr.hangUp()
	<-c.readerDone
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	tr.expectNothingSent(t, 50*time.Millisecond)
}

func TestHooksOverConnection(t *testing.T) {
	var mu sync.Mutex
	var log []string
	add := func(s string) {
		mu.Lock()
		log = append(log, s)
		mu.Unlock()
	}
	hooks := mustHooks(t,
		OnSessionStartHook(func(context.Context, *HookContext) error { add("start"); return nil }),
		PreTurnHook(func(_ context.Context, hc *HookContext, prompt []Content) (HookResult, error) {
			hc.SetState("turn", "t1")
			add("pre_turn:" + string(prompt[0].(Text)))
			return HookResult{}, nil
		}),
		PreToolCallHook(func(_ context.Context, hc *HookContext, call *ToolCall) (HookResult, error) {
			v, _ := hc.GetState("turn")
			add("pre_tool:" + call.ID + ":" + v.(string))
			return HookResult{}, nil
		}),
		PostToolCallHook(func(_ context.Context, _ *HookContext, r *ToolResult) error { add("post_tool:" + r.ID); return nil }),
		OnToolErrorHook(func(_ context.Context, _ *HookContext, e *ToolExecutionError) (string, error) {
			add("error:" + e.CallID)
			return "", nil
		}),
		PostTurnHook(func(_ context.Context, _ *HookContext, resp string) error { add("post_turn:" + resp); return nil }),
	)
	c, tr := newTestConnection(t, connectionOptions{hooks: hooks})
	_ = c
	tr.emit(hookRequest("s", wire.LifecycleHook_LIFECYCLE_HOOK_ON_SESSION_START, nil))
	tr.next(t)
	tr.emit(hookRequest("p", wire.LifecycleHook_LIFECYCLE_HOOK_PRE_TURN, func(r *wire.CallHookRequest) {
		r.SetPreTurnArgs(wire.PreTurnArgs_builder{UserInput: wire.UserInput_builder{Parts: []*wire.UserInput_Part{wire.UserInput_Part_builder{Text: new("")}.Build()}}.Build()}.Build())
	}))
	if d := tr.next(t).GetCallHookResponse().GetPreTurnResult().GetDecision(); d != wire.PreTurnResult_ALLOW {
		t.Fatalf("pre turn decision %s", d)
	}
	tr.emit(hookRequest("pt", wire.LifecycleHook_LIFECYCLE_HOOK_PRE_TOOL, func(r *wire.CallHookRequest) {
		r.SetPreToolArgs(wire.PreToolArgs_builder{ToolName: new("greet"), ArgumentsJson: new(`{"name": "Alice"}`), CallId: new("call_pre_123")}.Build())
	}))
	tr.next(t)
	tr.emit(hookRequest("po", wire.LifecycleHook_LIFECYCLE_HOOK_POST_TOOL, func(r *wire.CallHookRequest) {
		r.SetPostToolArgs(wire.PostToolArgs_builder{ToolName: new("greet"), Result: new("Hello Alice"), CallId: new("call_post_456")}.Build())
	}))
	tr.next(t)
	tr.emit(hookRequest("e", wire.LifecycleHook_LIFECYCLE_HOOK_ON_TOOL_ERROR, func(r *wire.CallHookRequest) {
		r.SetOnToolErrorArgs(wire.OnToolErrorArgs_builder{ToolName: new("broken"), ErrorMessage: new("failed"), CallId: new("call_err_789")}.Build())
	}))
	tr.next(t)
	tr.emit(hookRequest("pp", wire.LifecycleHook_LIFECYCLE_HOOK_POST_TURN, func(r *wire.CallHookRequest) {
		r.SetPostTurnArgs(wire.PostTurnArgs_builder{ResponseText: new("Final answer")}.Build())
	}))
	tr.next(t)
	mu.Lock()
	defer mu.Unlock()
	want := []string{"start", "pre_turn:", "pre_tool:call_pre_123:t1", "post_tool:call_post_456", "error:call_err_789", "post_turn:Final answer"}
	if !reflect.DeepEqual(log, want) {
		t.Fatalf("hook log %q, want %q", log, want)
	}
}

func TestPreTurnDenyCancelsTurn(t *testing.T) {
	deny := PreTurnHook(func(context.Context, *HookContext, []Content) (HookResult, error) {
		return HookResult{Deny: true, Message: "Denied by hook"}, nil
	})
	c, tr := newTestConnection(t, connectionOptions{hooks: mustHooks(t, deny)})
	if err := c.Send(t.Context(), Text("Hello")); err != nil {
		t.Fatal(err)
	}
	tr.next(t)
	tr.emit(hookRequest("req_deny", wire.LifecycleHook_LIFECYCLE_HOOK_PRE_TURN, func(r *wire.CallHookRequest) {
		r.SetPreTurnArgs(wire.PreTurnArgs_builder{UserInput: wire.UserInput_builder{Parts: []*wire.UserInput_Part{wire.UserInput_Part_builder{Text: new("Hello")}.Build()}}.Build()}.Build())
	}))
	res := tr.next(t).GetCallHookResponse().GetPreTurnResult()
	if res.GetDecision() != wire.PreTurnResult_DENY || res.GetReason() != "Denied by hook" {
		t.Fatalf("pre turn result %+v", res)
	}
	// The harness answers a denied turn with STATE_CANCELLED.
	tr.emit(wire.OutputEvent_builder{TrajectoryStateUpdate: wire.TrajectoryStateUpdate_builder{
		TrajectoryId: new("test"), State: new(wire.TrajectoryStateUpdate_STATE_CANCELLED), Error: new("Denied by hook"),
	}.Build()}.Build())
	_, err := collectSteps(t, c.ReceiveSteps(t.Context()))
	if _, ok := errors.AsType[*ExecutionError](err); !ok || !strings.Contains(err.Error(), "Denied by hook") {
		t.Fatalf("err = %v", err)
	}
}

func TestHookWithoutRunnerGetsEmptyResult(t *testing.T) {
	_, tr := newTestConnection(t, connectionOptions{})
	tr.emit(hookRequest("r", wire.LifecycleHook_LIFECYCLE_HOOK_PRE_TOOL, nil))
	resp := tr.next(t).GetCallHookResponse()
	if resp.GetRequestId() != "r" || resp.GetEmptyResult() == nil {
		t.Fatalf("response %+v", resp)
	}
}

func TestCompactionAndStopHooksOverConnection(t *testing.T) {
	var compacted *Step
	var stopArgs StopArgs
	var mu sync.Mutex
	hooks := mustHooks(t,
		OnCompactionHook(func(_ context.Context, _ *HookContext, s *Step) error {
			mu.Lock()
			compacted = s
			mu.Unlock()
			return nil
		}),
		StopHook(func(_ context.Context, _ *HookContext, a StopArgs) (StopHookResult, error) {
			mu.Lock()
			stopArgs = a
			mu.Unlock()
			return StopHookResult{Decision: StopDecisionContinue, Reason: "Continue working"}, nil
		}),
	)
	_, tr := newTestConnection(t, connectionOptions{hooks: hooks})
	tr.emit(hookRequest("req_comp_1", wire.LifecycleHook_LIFECYCLE_HOOK_ON_COMPACTION, func(r *wire.CallHookRequest) {
		r.SetOnCompactionArgs(wire.OnCompactionArgs_builder{TrajectoryId: new("main"), StepIndex: new(uint32(1)), Summary: new("Context compaction")}.Build())
	}))
	resp := tr.next(t).GetCallHookResponse()
	if resp.GetRequestId() != "req_comp_1" || resp.GetEmptyResult() == nil {
		t.Fatalf("compaction response %+v", resp)
	}
	tr.emit(hookRequest("req_stop_1", wire.LifecycleHook_LIFECYCLE_HOOK_STOP, func(r *wire.CallHookRequest) {
		r.SetStopArgs(wire.StopArgs_builder{ResponseText: new("Done with task"), TrajectoryId: new("main_traj"), ContinuationCount: new(int32(0))}.Build())
	}))
	sr := tr.next(t).GetCallHookResponse().GetStopResult()
	if sr.GetDecision() != wire.StopResult_CONTINUE || sr.GetReason() != "Continue working" {
		t.Fatalf("stop result %+v", sr)
	}
	mu.Lock()
	defer mu.Unlock()
	if compacted.Type != StepTypeCompaction || compacted.Content != "Context compaction" || compacted.Status != StepStatusDone ||
		compacted.Source != StepSourceSystem || compacted.Target != StepTargetUser || compacted.TrajectoryID != "main" || compacted.StepIndex != 1 {
		t.Fatalf("compaction step %+v", compacted)
	}
	if stopArgs.ResponseText != "Done with task" || stopArgs.TrajectoryID != "main_traj" || stopArgs.StopReason != StopReasonUnspecified {
		t.Fatalf("stop args %+v", stopArgs)
	}
}

func TestPolicyDecisionRequests(t *testing.T) {
	var captured []string
	var mu sync.Mutex
	policies := []Policy{
		{Tool: "run_command", Decision: DecisionDeny, Name: "block-all", When: func(_ context.Context, call ToolCall) (bool, error) {
			mu.Lock()
			captured = append(captured, call.Args["CommandLine"].(string))
			mu.Unlock()
			return true, nil
		}},
		{Tool: "run_command", Decision: DecisionDeny, When: func(context.Context, ToolCall) (bool, error) { return false, nil }},
		{Tool: "run_command", Decision: DecisionApprove, When: func(context.Context, ToolCall) (bool, error) { return true, nil }},
		{Tool: "run_command", Decision: DecisionAskUser, Name: "ask-test", AskUser: func(_ context.Context, _ ToolCall, reason string) (bool, error) {
			return reason == "ok", nil
		}},
		{Tool: "run_command", Decision: DecisionDeny, When: func(context.Context, ToolCall) (bool, error) { return false, errors.New("boom") }},
		{Tool: "run_command", Decision: DecisionAskUser, Name: "ask-no-handler", When: func(context.Context, ToolCall) (bool, error) { return true, nil }},
	}
	_, dynamic, err := policyConfig(policies)
	if err != nil {
		t.Fatal(err)
	}
	_, tr := newTestConnection(t, connectionOptions{dynamic: dynamic})
	decide := func(rule, reason, args string) *wire.PolicyDecisionResponse {
		tr.emit(wire.OutputEvent_builder{PolicyDecisionRequest: wire.PolicyDecisionRequest_builder{
			RequestId: new("req-" + rule), RuleId: new(rule), Reason: new(reason),
			ToolArgs: wire.PreToolArgs_builder{ToolName: new("run_command"), ArgumentsJson: new(args)}.Build(),
		}.Build()}.Build())
		resp := tr.next(t).GetPolicyDecisionResponse()
		if resp.GetRequestId() != "req-"+rule {
			t.Fatalf("request id %q", resp.GetRequestId())
		}
		return resp
	}
	for _, tc := range []struct {
		rule, reason, args string
		outcome            wire.PolicyEvaluationOutcome
		denyContains       string
	}{
		{"rule_0", "", `{"CommandLine": "echo hello"}`, wire.PolicyEvaluationOutcome_POLICY_EVALUATION_OUTCOME_DENY, "block-all"},
		{"rule_0", "Policy violation", `{"CommandLine": "x"}`, wire.PolicyEvaluationOutcome_POLICY_EVALUATION_OUTCOME_DENY, "Policy violation"},
		{"rule_1", "", "{}", wire.PolicyEvaluationOutcome_POLICY_EVALUATION_OUTCOME_NO_MATCH, ""},
		{"rule_2", "", "{}", wire.PolicyEvaluationOutcome_POLICY_EVALUATION_OUTCOME_ALLOW, ""},
		{"rule_3", "ok", "{}", wire.PolicyEvaluationOutcome_POLICY_EVALUATION_OUTCOME_ALLOW, ""},
		{"rule_3", "", "{}", wire.PolicyEvaluationOutcome_POLICY_EVALUATION_OUTCOME_DENY, "Denied by user (ask-test)."},
		{"rule_3", "Flagged by model", "{}", wire.PolicyEvaluationOutcome_POLICY_EVALUATION_OUTCOME_DENY, "Flagged by model"},
		{"rule_4", "", "{}", wire.PolicyEvaluationOutcome_POLICY_EVALUATION_OUTCOME_DENY, "Policy evaluation error"},
		{"rule_5", "", "{}", wire.PolicyEvaluationOutcome_POLICY_EVALUATION_OUTCOME_DENY, "requires ask_user handler"},
		{"nonexistent", "", "{}", wire.PolicyEvaluationOutcome_POLICY_EVALUATION_OUTCOME_DENY, "Unknown rule_id"},
	} {
		resp := decide(tc.rule, tc.reason, tc.args)
		if resp.GetOutcome() != tc.outcome || !strings.Contains(resp.GetDenyReason(), tc.denyContains) {
			t.Errorf("%s (reason %q): %s %q, want %s containing %q", tc.rule, tc.reason, resp.GetOutcome(), resp.GetDenyReason(), tc.outcome, tc.denyContains)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(captured) == 0 || captured[0] != "echo hello" {
		t.Fatalf("predicate saw %v", captured)
	}
}

func TestWarnIfSandboxUnavailable(t *testing.T) {
	var buf strings.Builder
	var mu sync.Mutex
	logger := slogTo(&buf, &mu)
	rc := func(enabled, sandbox bool) *wire.RunCommandToolConfig {
		return wire.RunCommandToolConfig_builder{Enabled: new(enabled), EnableSandbox: new(sandbox)}.Build()
	}
	for _, tc := range []struct {
		rc     *wire.RunCommandToolConfig
		status *SandboxStatus
		want   string
	}{
		{rc(true, true), &SandboxStatus{UnavailableReason: "no user namespaces"}, "no user namespaces"},
		{rc(true, true), &SandboxStatus{}, "reason unknown"},
		{rc(true, true), &SandboxStatus{Available: true}, ""},
		{rc(true, true), nil, ""},
		{rc(true, false), &SandboxStatus{}, ""},
		{rc(false, true), &SandboxStatus{}, ""},
	} {
		mu.Lock()
		buf.Reset()
		mu.Unlock()
		warnIfSandboxUnavailable(logger, tc.rc, tc.status)
		mu.Lock()
		got := buf.String()
		mu.Unlock()
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("status %+v: log %q, want %q", tc.status, got, tc.want)
		}
	}
}

func TestConversationIDFromHandshake(t *testing.T) {
	c, tr := newTestConnection(t, connectionOptions{conversationID: "main"})
	if got := c.ConversationID(); got != "main" {
		t.Fatalf("ConversationID = %q", got)
	}
	startTurn(c)
	// Subagent steps and states do not change it or end the turn.
	tr.emit(stepEvent(wire.StepUpdate_builder{TrajectoryId: new("sub"), ParentTrajectoryId: new("main"), StepIndex: new(uint32(1)), Text: new("x")}.Build()),
		idleEvent("sub", "subagent failed"), idleEvent("main", ""))
	steps, err := collectSteps(t, c.ReceiveSteps(t.Context()))
	if err != nil || len(steps) != 1 || c.ConversationID() != "main" {
		t.Fatalf("steps %+v, err %v, id %q", steps, err, c.ConversationID())
	}
}

func TestHarnessStartedTurn(t *testing.T) {
	c, tr := newTestConnection(t, connectionOptions{})
	if err := c.Send(t.Context(), Text("q")); err != nil {
		t.Fatal(err)
	}
	tr.next(t)
	tr.emit(wire.OutputEvent_builder{TrajectoryStateUpdate: wire.TrajectoryStateUpdate_builder{
		TrajectoryId: new("main"), State: new(wire.TrajectoryStateUpdate_STATE_FULLY_IDLE),
		StopReason: new(wire.TrajectoryStateUpdate_STOP_REASON_QUOTA_EXHAUSTED),
	}.Build()}.Build())
	waitFor(t, "idle", c.IsIdle)
	c.mu.Lock()
	first := c.cur
	c.mu.Unlock()
	// A trigger runs a turn without Send: RUNNING while idle starts it.
	tr.emit(wire.OutputEvent_builder{TrajectoryStateUpdate: wire.TrajectoryStateUpdate_builder{TrajectoryId: new("main"), State: new(wire.TrajectoryStateUpdate_STATE_RUNNING)}.Build()}.Build(),
		modelText("main", 3, wire.StepUpdate_STATE_DONE, wire.StepUpdate_TARGET_USER, "triggered"), idleEvent("main", ""))
	waitFor(t, "second turn", func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.cur != first && c.cur.ended
	})
	steps, err := collectSteps(t, c.ReceiveSteps(t.Context()))
	if err != nil || len(steps) != 1 || steps[0].Content != "triggered" {
		t.Fatalf("steps %+v, err %v", steps, err)
	}
	if c.turnStopReason(first) != StopReasonQuotaExhausted || c.LastTurnStopReason() != StopReasonUnspecified {
		t.Fatalf("stop reasons %s / %s", c.turnStopReason(first), c.LastTurnStopReason())
	}
}

// waitDone fails the test unless c's Done channel closes in time.
func waitDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Done")
	}
}

func TestDoneAndErrOnTransportError(t *testing.T) {
	c, tr := newTestConnection(t, connectionOptions{})
	select {
	case <-c.Done():
		t.Fatal("Done closed while running")
	default:
	}
	if err := c.Err(); err != nil {
		t.Fatalf("Err while running = %v", err)
	}
	tr.errs <- &harness.ConnectionError{Code: 1006, Stderr: "panic: boom"}
	waitDone(t, c.Done())
	ce, ok := errors.AsType[*ConnectionError](c.Err())
	if !ok || ce.Code != 1006 || !strings.Contains(ce.Stderr, "panic: boom") {
		t.Fatalf("Err = %v", c.Err())
	}
	// Close keeps the terminal error.
	_ = c.Close()
	if _, ok := errors.AsType[*ConnectionError](c.Err()); !ok {
		t.Fatalf("Err after Close = %v", c.Err())
	}
}

func TestDoneAndErrOnHangUp(t *testing.T) {
	c, tr := newTestConnection(t, connectionOptions{})
	conv := newConversation(c, nil)
	tr.hangUp()
	waitDone(t, conv.Done())
	if _, ok := errors.AsType[*ConnectionError](conv.Err()); !ok {
		t.Fatalf("Err = %v", conv.Err())
	}
}

func TestDoneOnClose(t *testing.T) {
	c, _ := newTestConnection(t, connectionOptions{})
	done := c.Done()
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	waitDone(t, done)
	if err := c.Err(); err != nil {
		t.Fatalf("Err after Close = %v", err)
	}
}
