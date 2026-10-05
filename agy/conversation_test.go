package agy

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ironpark/gelati/agy/internal/wire"
)

func newTestConversation(t *testing.T, o connectionOptions) (*Conversation, *fakeTransport) {
	t.Helper()
	c, tr := newTestConnection(t, o)
	return newConversation(c, c.InitialHistory()), tr
}

// emitTurn emits the events of one turn followed by idle.
func emitTurn(tr *fakeTransport, traj string, evs ...*wire.OutputEvent) {
	tr.emit(evs...)
	tr.emit(idleEvent(traj, ""))
}

func chunkSummary(chunks []Chunk) []string {
	var out []string
	for _, c := range chunks {
		switch c := c.(type) {
		case *TextChunk:
			out = append(out, "text:"+c.Text)
		case *ThoughtChunk:
			out = append(out, "thought:"+c.Text)
		case *ToolCall:
			out = append(out, "call:"+c.Name+":"+c.ID)
		}
	}
	return out
}

func TestChunksFromSteps(t *testing.T) {
	seen := map[string]bool{}
	user := func(s *Step) *Step { s.Source, s.Target = StepSourceModel, StepTargetUser; return s }
	steps := []*Step{
		user(&Step{ThinkingDelta: "Thinking...", Status: StepStatusActive}),
		user(&Step{ContentDelta: "Hello", Status: StepStatusActive}),
		{Source: StepSourceUser, Target: StepTargetUser, ContentDelta: "Prompt context"},
		{Source: StepSourceModel, Target: StepTargetEnvironment, ContentDelta: "Confirming tool call..."},
		user(&Step{ContentDelta: "error text", ThinkingDelta: "error thinking", Status: StepStatusError}),
		{ToolCalls: []*ToolCall{{ID: "call_456", Name: "generate_image"}}},
		{ToolCalls: []*ToolCall{{ID: "call_456", Name: "generate_image"}}},
		{ToolCalls: []*ToolCall{{ID: "call_a", Name: "tool_1"}, {ID: "call_b", Name: "tool_2"}}},
		{ToolCalls: []*ToolCall{{Name: "tool_x"}}},
		{ToolCalls: []*ToolCall{{Name: "tool_x"}}},
	}
	var all []Chunk
	for _, s := range steps {
		all = append(all, chunksFromStep(s, seen)...)
	}
	want := []string{"thought:Thinking...", "text:Hello", "call:generate_image:call_456", "call:tool_1:call_a", "call:tool_2:call_b", "call:tool_x:", "call:tool_x:"}
	if got := chunkSummary(all); !reflect.DeepEqual(got, want) {
		t.Fatalf("chunks %q, want %q", got, want)
	}
}

func TestConversationChat(t *testing.T) {
	conv, tr := newTestConversation(t, connectionOptions{})
	ctx := t.Context()
	resp, err := conv.Chat(ctx, Text("question"))
	if err != nil {
		t.Fatal(err)
	}
	if got := tr.next(t).GetUserInput().GetParts()[0].GetText(); got != "question" {
		t.Fatalf("sent %q", got)
	}
	emitTurn(tr, "traj",
		stepEvent(wire.StepUpdate_builder{TrajectoryId: new("traj"), StepIndex: new(uint32(0)), Source: new(wire.StepUpdate_SOURCE_USER), State: new(wire.StepUpdate_STATE_DONE), Text: new("question")}.Build()),
		stepEvent(wire.StepUpdate_builder{TrajectoryId: new("traj"), StepIndex: new(uint32(1)), Source: new(wire.StepUpdate_SOURCE_MODEL), Target: new(wire.StepUpdate_TARGET_USER), State: new(wire.StepUpdate_STATE_ACTIVE), Thinking: new("hmm"), ThinkingDelta: new("hmm")}.Build()),
		stepEvent(wire.StepUpdate_builder{TrajectoryId: new("traj"), StepIndex: new(uint32(1)), Source: new(wire.StepUpdate_SOURCE_MODEL), Target: new(wire.StepUpdate_TARGET_USER), State: new(wire.StepUpdate_STATE_ACTIVE), Text: new("Hello"), TextDelta: new("Hello")}.Build()),
		stepEvent(wire.StepUpdate_builder{TrajectoryId: new("traj"), StepIndex: new(uint32(1)), Source: new(wire.StepUpdate_SOURCE_MODEL), Target: new(wire.StepUpdate_TARGET_USER), State: new(wire.StepUpdate_STATE_DONE), Text: new("Hello world!"), TextDelta: new(" world!")}.Build()),
	)
	var deltas []string
	for d, err := range resp.Text(ctx) {
		if err != nil {
			t.Fatal(err)
		}
		deltas = append(deltas, d)
	}
	if !reflect.DeepEqual(deltas, []string{"Hello", " world!"}) {
		t.Fatalf("deltas %q", deltas)
	}
	var thoughts []string
	for d, err := range resp.Thoughts(ctx) {
		if err != nil {
			t.Fatal(err)
		}
		thoughts = append(thoughts, d)
	}
	if !reflect.DeepEqual(thoughts, []string{"hmm"}) {
		t.Fatalf("thoughts %q", thoughts)
	}
	res, err := resp.Result(ctx)
	if err != nil || res.Text() != "Hello world!" || res.Thoughts() != "hmm" {
		t.Fatalf("Result = %+v, %v", res, err)
	}
	if conv.LastResponse() != "Hello world!" || len(conv.History()) != 4 || conv.TurnCount() != 1 || conv.ConversationID() != "traj" {
		t.Fatalf("conversation state: last %q, history %d, turns %d, id %q", conv.LastResponse(), len(conv.History()), conv.TurnCount(), conv.ConversationID())
	}
	if res.StructuredOutput != nil {
		t.Fatalf("structured output %v", res.StructuredOutput)
	}
	if res.Usage != nil || res.StopReason != StopReasonUnspecified {
		t.Fatal("usage or stop reason")
	}
	if err := resp.Cancel(ctx); err != nil {
		t.Fatal(err)
	}
	tr.expectNothingSent(t, 50*time.Millisecond) // cancel after completion is a no-op
}

func TestConversationStructuredOutputAndUsage(t *testing.T) {
	conv, tr := newTestConversation(t, connectionOptions{})
	ctx := t.Context()
	resp, err := conv.Chat(ctx, Text("report"))
	if err != nil {
		t.Fatal(err)
	}
	tr.next(t)
	tr.emit(wire.OutputEvent_builder{UsageUpdate: wire.UsageUpdate_builder{Total: wire.UsageMetadata_builder{
		PromptTokenCount: new(uint64(300)), CandidatesTokenCount: new(uint64(80)), TotalTokenCount: new(uint64(380)),
	}.Build()}.Build()}.Build())
	emitTurn(tr, "traj", stepEvent(wire.StepUpdate_builder{
		TrajectoryId: new("traj"), StepIndex: new(uint32(2)), Source: new(wire.StepUpdate_SOURCE_MODEL), State: new(wire.StepUpdate_STATE_DONE),
		Finish: wire.ActionFinish_builder{OutputString: new(`{"result": "data", "n": 2}`)}.Build(),
	}.Build()))
	var out struct {
		Result string `json:"result"`
		N      int    `json:"n"`
	}
	res, err := resp.Result(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := res.DecodeStructuredOutput(&out); err != nil || out.Result != "data" || out.N != 2 {
		t.Fatalf("structured %+v %v", out, err)
	}
	u := res.Usage
	if u == nil || val(u.PromptTokenCount) != 300 || val(u.CandidatesTokenCount) != 80 || val(u.TotalTokenCount) != 380 {
		t.Fatalf("usage %+v", u)
	}
	if tu := conv.TotalUsage(); val(tu.TotalTokenCount) != 380 {
		t.Fatalf("total usage %+v", tu)
	}

	// The next turn's usage is the difference.
	resp, _ = conv.Chat(ctx, Text("again"))
	tr.next(t)
	tr.emit(wire.OutputEvent_builder{UsageUpdate: wire.UsageUpdate_builder{Total: wire.UsageMetadata_builder{PromptTokenCount: new(uint64(450)), TotalTokenCount: new(uint64(560))}.Build()}.Build()}.Build())
	emitTurn(tr, "traj", modelText("traj", 3, wire.StepUpdate_STATE_DONE, wire.StepUpdate_TARGET_USER, "ok"))
	res, err = resp.Result(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if u := res.Usage; val(u.PromptTokenCount) != 150 || val(u.TotalTokenCount) != 180 {
		t.Fatalf("second turn usage %+v", u)
	}
	if lu := conv.LastTurnUsage(); val(lu.TotalTokenCount) != 180 {
		t.Fatalf("LastTurnUsage %+v", lu)
	}
	if err := res.DecodeStructuredOutput(&out); err != nil {
		// The finish step of the first turn is still the latest one.
		t.Fatal(err)
	}
	conv.ClearHistory()
	if conv.LastTurnUsage() != nil || len(conv.History()) != 0 || conv.TurnCount() != 0 {
		t.Fatal("ClearHistory")
	}
}

func TestConversationNoUsageMeansNil(t *testing.T) {
	conv, tr := newTestConversation(t, connectionOptions{initialUsage: &UsageMetadata{PromptTokenCount: i64(300), TotalTokenCount: i64(360)}})
	if val(conv.TotalUsage().TotalTokenCount) != 360 || conv.LastTurnUsage() != nil {
		t.Fatal("initial usage")
	}
	resp, _ := conv.Chat(t.Context(), Text("q"))
	tr.next(t)
	emitTurn(tr, "t", modelText("t", 0, wire.StepUpdate_STATE_DONE, wire.StepUpdate_TARGET_USER, "answer"))
	res, err := resp.Result(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if res.Usage != nil {
		t.Fatalf("usage %+v", res.Usage)
	}
}

func TestConversationHistoryAndCompaction(t *testing.T) {
	hist := []*Step{{Content: "h0"}, {Type: StepTypeCompaction, Content: "summary"}}
	conv, tr := newTestConversation(t, connectionOptions{initialHistory: hist})
	if got := conv.CompactionIndices(); !reflect.DeepEqual(got, []int{1}) {
		t.Fatalf("initial compaction indices %v", got)
	}
	ctx := t.Context()
	if err := conv.Send(ctx, Text("q1")); err != nil {
		t.Fatal(err)
	}
	tr.next(t)
	emitTurn(tr, "t",
		modelText("t", 1, wire.StepUpdate_STATE_DONE, wire.StepUpdate_TARGET_USER, "first answer"),
		stepEvent(wire.StepUpdate_builder{TrajectoryId: new("t"), StepIndex: new(uint32(2)), Compaction: &wire.ActionCompaction{}}.Build()),
	)
	for _, err := range conv.ReceiveSteps(ctx) {
		if err != nil {
			t.Fatal(err)
		}
	}
	if conv.LastResponse() != "first answer" || !reflect.DeepEqual(conv.CompactionIndices(), []int{1, 3}) {
		t.Fatalf("last %q compactions %v", conv.LastResponse(), conv.CompactionIndices())
	}
	h := conv.History()
	h[0] = nil
	if conv.History()[0] == nil {
		t.Fatal("History returned the internal slice")
	}
	ci := conv.CompactionIndices()
	ci[0] = 99
	if conv.CompactionIndices()[0] == 99 {
		t.Fatal("CompactionIndices returned the internal slice")
	}

	// Trimming shifts the indices.
	conv.SetMaxHistorySize(2)
	if len(conv.History()) != 2 || !reflect.DeepEqual(conv.CompactionIndices(), []int{1}) {
		t.Fatalf("after trim: history %d, compactions %v", len(conv.History()), conv.CompactionIndices())
	}
	conv.SetMaxHistorySize(0)
	for range 5 {
		conv.record(&Step{})
	}
	if len(conv.History()) != 7 {
		t.Fatalf("unlimited history %d", len(conv.History()))
	}
}

func TestConversationSendDrainsPreviousTurn(t *testing.T) {
	conv, tr := newTestConversation(t, connectionOptions{})
	ctx := t.Context()
	if err := conv.Send(ctx, Text("first question")); err != nil {
		t.Fatal(err)
	}
	tr.next(t)
	emitTurn(tr, "t", modelText("t", 1, wire.StepUpdate_STATE_DONE, wire.StepUpdate_TARGET_USER, "first reply"))
	waitFor(t, "first turn idle", func() bool { return conv.IsIdle() })
	// Force the not-idle path: the turn's steps are still queued.
	conv.conn.mu.Lock()
	conv.conn.idle = false
	conv.conn.idleCh = make(chan struct{})
	conv.conn.mu.Unlock()
	go func() {
		time.Sleep(20 * time.Millisecond)
		tr.emit(idleEvent("t", ""))
	}()
	if err := conv.Send(ctx, Text("second question")); err != nil {
		t.Fatal(err)
	}
	if h := conv.History(); len(h) != 1 || h[0].Content != "first reply" || conv.TurnCount() != 2 {
		t.Fatalf("history after drain %+v, turns %d", h, conv.TurnCount())
	}
	tr.next(t)
	emitTurn(tr, "t", modelText("t", 2, wire.StepUpdate_STATE_DONE, wire.StepUpdate_TARGET_USER, "second reply"))
	for _, err := range conv.ReceiveSteps(ctx) {
		if err != nil {
			t.Fatal(err)
		}
	}
	if h := conv.History(); len(h) != 2 || h[1].Content != "second reply" {
		t.Fatalf("history %+v", h)
	}
}

func TestConversationSendDrainsAbandonedResponse(t *testing.T) {
	conv, tr := newTestConversation(t, connectionOptions{})
	ctx := t.Context()
	resp, err := conv.Chat(ctx, Text("one"))
	if err != nil {
		t.Fatal(err)
	}
	tr.next(t)
	tr.emit(modelText("t", 1, wire.StepUpdate_STATE_ACTIVE, wire.StepUpdate_TARGET_USER, "partial"))
	// Read one chunk, then abandon the response mid-turn.
	for range resp.Events(ctx) {
		break
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		tr.emit(modelText("t", 1, wire.StepUpdate_STATE_DONE, wire.StepUpdate_TARGET_USER, "partial done"), idleEvent("t", "turn failed"))
	}()
	resp2, err := conv.Chat(ctx, Text("two"))
	if err != nil {
		t.Fatalf("second Chat: %v", err)
	}
	// The first turn's error stays with its response.
	if res, err := resp.Result(ctx); err == nil || res == nil || !strings.HasPrefix(res.Text(), "partial") {
		t.Fatalf("first response: %+v, %v", res, err)
	}
	if len(conv.History()) != 2 {
		t.Fatalf("history %+v", conv.History())
	}
	tr.next(t)
	emitTurn(tr, "t", modelText("t", 2, wire.StepUpdate_STATE_DONE, wire.StepUpdate_TARGET_USER, "fine"))
	if res, err := resp2.Result(ctx); err != nil || res.Text() != "fine" {
		t.Fatalf("second response %+v %v", res, err)
	}
}

func TestConversationSendWaitsForOtherReader(t *testing.T) {
	conv, tr := newTestConversation(t, connectionOptions{})
	ctx := t.Context()
	if err := conv.Send(ctx, Text("first")); err != nil {
		t.Fatal(err)
	}
	tr.next(t)
	tr.emit(modelText("t", 1, wire.StepUpdate_STATE_ACTIVE, wire.StepUpdate_TARGET_USER, "working"))
	reading := make(chan struct{})
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for range conv.ReceiveSteps(context.Background()) {
			select {
			case <-reading:
			default:
				close(reading)
			}
		}
	}()
	<-reading
	sent := make(chan error, 1)
	go func() { sent <- conv.Send(ctx, Text("second")) }()
	time.Sleep(50 * time.Millisecond)
	select {
	case err := <-sent:
		t.Fatalf("Send returned before the agent was idle: %v", err)
	default:
	}
	tr.emit(idleEvent("t", ""))
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
	<-readerDone
	if got := tr.next(t).GetUserInput().GetParts()[0].GetText(); got != "second" {
		t.Fatalf("second prompt %q", got)
	}
}

func TestConversationChatChunks(t *testing.T) {
	conv, tr := newTestConversation(t, connectionOptions{})
	resp, err := conv.Chat(t.Context(), Text("q"))
	if err != nil {
		t.Fatal(err)
	}
	tr.next(t)
	emitTurn(tr, "t",
		modelText("t", 1, wire.StepUpdate_STATE_ACTIVE, wire.StepUpdate_TARGET_USER, "Hi"),
		stepEvent(wire.StepUpdate_builder{TrajectoryId: new("t"), StepIndex: new(uint32(2)), ViewFile: wire.ActionViewFile_builder{FilePath: new("README.md")}.Build()}.Build()),
		stepEvent(wire.StepUpdate_builder{TrajectoryId: new("t"), StepIndex: new(uint32(2)), State: new(wire.StepUpdate_STATE_DONE), ViewFile: wire.ActionViewFile_builder{FilePath: new("README.md")}.Build()}.Build()),
	)
	var chunks []Chunk
	for c, err := range resp.Events(t.Context()) {
		if err != nil {
			t.Fatal(err)
		}
		chunks = append(chunks, c)
	}
	if got := chunkSummary(chunks); !reflect.DeepEqual(got, []string{"text:Hi", "call:view_file:t:2"}) {
		t.Fatalf("chunks %q", got)
	}
	if len(conv.History()) != 3 {
		t.Fatalf("history %d", len(conv.History()))
	}
}

func TestTurnStreamCursors(t *testing.T) {
	chunks := []Chunk{
		&ThoughtChunk{StepIndex: 1, Text: "A"},
		&TextChunk{StepIndex: 2, Text: "B"},
		&ThoughtChunk{StepIndex: 3, Text: "C"},
		&TextChunk{StepIndex: 4, Text: "D"},
		&ToolCall{ID: "c1", Name: "fn"},
	}
	pulls := 0
	var mu sync.Mutex
	src := func(context.Context) (Chunk, bool, error) {
		mu.Lock()
		defer mu.Unlock()
		if pulls >= len(chunks) {
			return nil, false, nil
		}
		pulls++
		return chunks[pulls-1], true, nil
	}
	resp := newTurnStream(src)
	ctx := t.Context()
	res, err := resp.Result(ctx)
	if err != nil || len(res.Chunks) != 5 || len(res.ToolCalls()) != 1 || res.Thoughts() != "AC" {
		t.Fatalf("Result %+v %v", res, err)
	}
	collect := func(seq func(func(string, error) bool)) []string {
		var out []string
		for s, err := range seq {
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, s)
		}
		return out
	}
	if got := collect(resp.Thoughts(ctx)); !reflect.DeepEqual(got, []string{"A", "C"}) {
		t.Fatalf("thoughts %q", got)
	}
	if got := collect(resp.Text(ctx)); !reflect.DeepEqual(got, []string{"B", "D"}) {
		t.Fatalf("text %q", got)
	}
	var calls []*ToolCall
	for c, err := range resp.ToolCalls(ctx) {
		if err != nil {
			t.Fatal(err)
		}
		calls = append(calls, c)
	}
	if len(calls) != 1 || calls[0].Name != "fn" {
		t.Fatalf("calls %v", calls)
	}
	if res.Text() != "BD" {
		t.Fatalf("Text %q", res.Text())
	}
	if pulls != 5 {
		t.Fatalf("source pulled %d times, want each chunk once", pulls)
	}
}

func TestTurnStreamConcurrentCursors(t *testing.T) {
	ch := make(chan Chunk)
	src := func(ctx context.Context) (Chunk, bool, error) {
		select {
		case c, ok := <-ch:
			return c, ok, nil
		case <-ctx.Done():
			return nil, false, ctx.Err()
		}
	}
	resp := newTurnStream(src)
	var wg sync.WaitGroup
	results := make([][]string, 4)
	for i := range results {
		wg.Go(func() {
			for c, err := range resp.Events(context.Background()) {
				if err != nil {
					t.Error(err)
					return
				}
				results[i] = append(results[i], c.(*TextChunk).Text)
			}
		})
	}
	for _, s := range []string{"a", "b", "c"} {
		ch <- &TextChunk{Text: s}
	}
	close(ch)
	wg.Wait()
	for i, r := range results {
		if !reflect.DeepEqual(r, []string{"a", "b", "c"}) {
			t.Fatalf("cursor %d saw %q", i, r)
		}
	}
}

func TestTurnStreamErrorsAndCancellation(t *testing.T) {
	boom := errors.New("network failure")
	n := 0
	src := func(context.Context) (Chunk, bool, error) {
		n++
		if n == 1 {
			return &TextChunk{Text: "ok"}, true, nil
		}
		return nil, false, boom
	}
	resp := newTurnStream(src)
	for range 2 {
		var got []string
		var gotErr error
		for d, err := range resp.Text(t.Context()) {
			if err != nil {
				gotErr = err
				break
			}
			got = append(got, d)
		}
		if !reflect.DeepEqual(got, []string{"ok"}) || !errors.Is(gotErr, boom) {
			t.Fatalf("cursor saw %q, %v", got, gotErr)
		}
	}
	if !resp.end.Ended() {
		t.Fatal("stream not marked done")
	}

	// Cancelling one cursor's context does not end the stream.
	release := make(chan struct{})
	src = func(ctx context.Context) (Chunk, bool, error) {
		select {
		case <-release:
			return nil, false, nil
		case <-ctx.Done():
			return nil, false, ctx.Err()
		}
	}
	resp = newTurnStream(src)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if res, err := resp.Result(ctx); res != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled cursor %v %v", res, err)
	}
	if resp.end.Ended() {
		t.Fatal("cancellation poisoned the stream")
	}
	close(release)
	if _, err := resp.Result(t.Context()); err != nil {
		t.Fatal(err)
	}

	// A failed stream's Result carries the partial result.
	n = 0
	src = func(context.Context) (Chunk, bool, error) {
		n++
		if n == 1 {
			return &TextChunk{Text: "partial"}, true, nil
		}
		return nil, false, boom
	}
	if res, err := newTurnStream(src).Result(t.Context()); !errors.Is(err, boom) || res == nil || !strings.HasPrefix(res.Text(), "partial") {
		t.Fatalf("failed stream %+v %v", res, err)
	}

	// An empty stream.
	resp = newTurnStream(func(context.Context) (Chunk, bool, error) { return nil, false, nil })
	res, err := resp.Result(t.Context())
	if err != nil || res.Text() != "" {
		t.Fatalf("empty stream %+v %v", res, err)
	}
	if err := res.DecodeStructuredOutput(&struct{}{}); err == nil {
		t.Fatal("decoded structured output that does not exist")
	}
}

func TestTurnStreamCancelSendsHalt(t *testing.T) {
	conv, tr := newTestConversation(t, connectionOptions{})
	ctx := t.Context()
	resp, err := conv.Chat(ctx, Text("long task"))
	if err != nil {
		t.Fatal(err)
	}
	tr.next(t)
	tr.emit(modelText("t", 1, wire.StepUpdate_STATE_ACTIVE, wire.StepUpdate_TARGET_USER, "working"))
	for range resp.Events(ctx) {
		break
	}
	if err := resp.Cancel(ctx); err != nil {
		t.Fatal(err)
	}
	if !tr.next(t).GetHaltRequest() {
		t.Fatal("no halt request")
	}
	tr.emit(idleEvent("t", ""))
	res, err := resp.Result(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled turn ended with %v", err)
	}
	var ce *CancelledError
	if !errors.As(err, &ce) || res == nil || res.Text() != "working" {
		t.Fatalf("cancelled result %+v %v", res, err)
	}
}

func TestTurnStreamClose(t *testing.T) {
	conv, tr := newTestConversation(t, connectionOptions{})
	ctx := t.Context()
	resp, err := conv.Chat(ctx, Text("long task"))
	if err != nil {
		t.Fatal(err)
	}
	tr.next(t)
	tr.emit(modelText("t", 1, wire.StepUpdate_STATE_ACTIVE, wire.StepUpdate_TARGET_USER, "working"))
	for range resp.Events(ctx) {
		break
	}
	// A cursor blocked in a pull is released by Close.
	blocked := make(chan error, 1)
	go func() {
		var last error
		for _, err := range resp.Events(ctx) {
			last = err
		}
		blocked <- last
	}()
	time.Sleep(20 * time.Millisecond)
	if err := resp.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-blocked; !errors.Is(err, ErrClosed) {
		t.Fatalf("blocked cursor ended with %v", err)
	}
	var got []string
	var gotErr error
	for d, err := range resp.Text(ctx) {
		if err != nil {
			gotErr = err
			break
		}
		got = append(got, d)
	}
	if !reflect.DeepEqual(got, []string{"working"}) || !errors.Is(gotErr, ErrClosed) {
		t.Fatalf("after Close: %q, %v", got, gotErr)
	}
	if res, err := resp.Result(ctx); res != nil || !errors.Is(err, ErrClosed) {
		t.Fatalf("Result after Close %+v %v", res, err)
	}
	// Close gave up the step reader without cancelling the turn.
	tr.expectNothingSent(t, 50*time.Millisecond)
	tr.emit(modelText("t", 1, wire.StepUpdate_STATE_DONE, wire.StepUpdate_TARGET_USER, "working done"), idleEvent("t", ""))
	for _, err := range conv.ReceiveSteps(ctx) {
		if err != nil {
			t.Fatal(err)
		}
	}
	if conv.LastResponse() != "working done" {
		t.Fatalf("last response %q", conv.LastResponse())
	}
	// Cancel after Close is a no-op once the turn has ended.
	if err := resp.Cancel(ctx); err != nil {
		t.Fatal(err)
	}
	tr.expectNothingSent(t, 50*time.Millisecond)
	if err := resp.Close(); err != nil {
		t.Fatal(err)
	}
}
