package codex

import (
	"context"
	"encoding/json/v2"
	"errors"
	"iter"
	"testing"
	"time"
)

// serve runs fn on a goroutine so a blocking client call can be answered.
func serve(t *testing.T, fn func()) chan struct{} {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	return done
}

func TestStartThread(t *testing.T) {
	client, server := connect(t, Options{})

	done := serve(t, func() {
		req := server.expect("thread/start")
		var params StartThreadParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			t.Errorf("params: %v", err)
			return
		}
		if params.Model != "gpt-5.6-terra" || params.Cwd != "/Users/me/project" {
			t.Errorf("params = %+v", params)
		}
		if params.ApprovalPolicy != ApprovalNever || params.Sandbox != SandboxModeWorkspaceWrite {
			t.Errorf("params = %+v", params)
		}
		if params.ServiceName != "mohae" {
			t.Errorf("serviceName = %q", params.ServiceName)
		}
		server.respond(req, map[string]any{"thread": map[string]any{
			"id": "thr_123", "sessionId": "thr_123", "modelProvider": "openai", "createdAt": 1730910000,
		}})
	})

	thread, err := client.StartThread(context.Background(), StartThreadParams{
		ThreadSettings: ThreadSettings{
			Model:          "gpt-5.6-terra",
			Cwd:            "/Users/me/project",
			ApprovalPolicy: ApprovalNever,
			Sandbox:        SandboxModeWorkspaceWrite,
		},
		Personality: "friendly",
		ServiceName: "mohae",
	})
	<-done
	if err != nil {
		t.Fatalf("StartThread: %v", err)
	}
	if info := thread.Info(); thread.ID() != "thr_123" || info.SessionID != "thr_123" {
		t.Fatalf("thread = %+v", info)
	}
	if client.lookup("thr_123") == nil {
		t.Fatal("thread/start did not subscribe")
	}
}

func TestResumeAndForkThread(t *testing.T) {
	client, server := connect(t, Options{})

	done := serve(t, func() {
		req := server.expect("thread/resume")
		var resume ResumeThreadParams
		if err := json.Unmarshal(req.Params, &resume); err != nil {
			t.Errorf("params: %v", err)
		}
		if resume.ThreadID != "thr_123" || resume.Personality != "friendly" {
			t.Errorf("resume params = %+v", resume)
		}
		server.respond(req, map[string]any{"thread": map[string]any{"id": "thr_123", "name": "Bug bash notes"}})

		req = server.expect("thread/fork")
		var fork ForkThreadParams
		if err := json.Unmarshal(req.Params, &fork); err != nil {
			t.Errorf("params: %v", err)
		}
		if fork.ThreadID != "thr_123" || fork.LastTurnID != "turn_456" {
			t.Errorf("fork params = %+v", fork)
		}
		server.respond(req, map[string]any{"thread": map[string]any{
			"id": "thr_456", "sessionId": "thr_123", "forkedFromId": "thr_123",
		}})
	})

	resumed, err := client.ResumeThread(context.Background(), ResumeThreadParams{
		ThreadID: "thr_123", Personality: "friendly",
	})
	if err != nil {
		t.Fatalf("ResumeThread: %v", err)
	}
	if name := resumed.Info().Name; name == nil || *name != "Bug bash notes" {
		t.Fatalf("name = %v", name)
	}

	forked, err := client.ForkThread(context.Background(), ForkThreadParams{
		ThreadID: "thr_123", LastTurnID: "turn_456",
	})
	<-done
	if err != nil {
		t.Fatalf("ForkThread: %v", err)
	}
	if forked.ID() != "thr_456" || forked.Info().ForkedFromID != "thr_123" {
		t.Fatalf("forked = %+v", forked.Info())
	}
	if client.lookup("thr_456") == nil {
		t.Fatal("fork did not subscribe")
	}
}

func TestReadThread(t *testing.T) {
	client, server := connect(t, Options{})

	done := serve(t, func() {
		req := server.expect("thread/read")
		var params ReadThreadParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			t.Errorf("params: %v", err)
		}
		if params.ThreadID != "thr_123" || !params.IncludeTurns {
			t.Errorf("params = %+v", params)
		}
		server.respond(req, map[string]any{"thread": map[string]any{
			"id": "thr_123", "name": "Bug bash notes", "ephemeral": false,
			"status": map[string]any{"type": "notLoaded"}, "turns": []any{},
		}})
	})

	thread, err := client.ReadThread(context.Background(), ReadThreadParams{ThreadID: "thr_123", IncludeTurns: true})
	<-done
	if err != nil {
		t.Fatalf("ReadThread: %v", err)
	}
	if thread.Status == nil || thread.Status.Type != ThreadStatusNotLoaded {
		t.Fatalf("status = %+v", thread.Status)
	}
	// thread/read must not subscribe.
	if client.lookup("thr_123") != nil {
		t.Fatal("thread/read subscribed to the thread")
	}
}

func TestListThreadsPagination(t *testing.T) {
	client, server := connect(t, Options{})

	done := serve(t, func() {
		first := server.expect("thread/list")
		var params ListThreadsParams
		if err := json.Unmarshal(first.Params, &params); err != nil {
			t.Errorf("params: %v", err)
		}
		if params.Cursor != "" || params.Limit != 2 || params.SortKey != SortKeyCreatedAt {
			t.Errorf("first page params = %+v", params)
		}
		server.respond(first, map[string]any{
			"data":       []any{map[string]any{"id": "thr_a"}, map[string]any{"id": "thr_b"}},
			"nextCursor": "page-2",
		})

		second := server.expect("thread/list")
		if err := json.Unmarshal(second.Params, &params); err != nil {
			t.Errorf("params: %v", err)
		}
		if params.Cursor != "page-2" {
			t.Errorf("second page cursor = %q", params.Cursor)
		}
		server.respond(second, map[string]any{
			"data":       []any{map[string]any{"id": "thr_c"}},
			"nextCursor": nil,
		})
	})

	var ids []string
	for thread, err := range client.AllThreads(context.Background(), ListThreadsParams{
		Limit: 2, SortKey: SortKeyCreatedAt,
	}) {
		if err != nil {
			t.Fatalf("AllThreads: %v", err)
		}
		ids = append(ids, thread.ID)
	}
	<-done

	if len(ids) != 3 || ids[0] != "thr_a" || ids[2] != "thr_c" {
		t.Fatalf("ids = %v", ids)
	}
}

func TestListThreadsIterationStop(t *testing.T) {
	client, server := connect(t, Options{})

	done := serve(t, func() {
		req := server.expect("thread/list")
		server.respond(req, map[string]any{
			"data":       []any{map[string]any{"id": "thr_a"}, map[string]any{"id": "thr_b"}},
			"nextCursor": "page-2",
		})
	})

	count := 0
	for range client.AllThreads(context.Background(), ListThreadsParams{}) {
		count++
		break
	}
	<-done
	if count != 1 {
		t.Fatalf("count = %d", count)
	}
}

func TestThreadMutations(t *testing.T) {
	client, server := connect(t, Options{})

	tests := []struct {
		name       string
		method     string
		result     any
		invoke     func() error
		wantParams string
	}{
		{
			name:       "archive",
			method:     "thread/archive",
			result:     map[string]any{},
			invoke:     func() error { return client.ArchiveThread(context.Background(), "thr_b") },
			wantParams: `{"threadId":"thr_b"}`,
		},
		{
			name:       "delete",
			method:     "thread/delete",
			result:     map[string]any{},
			invoke:     func() error { return client.DeleteThread(context.Background(), "thr_b") },
			wantParams: `{"threadId":"thr_b"}`,
		},
		{
			name:   "unarchive",
			method: "thread/unarchive",
			result: map[string]any{"thread": map[string]any{"id": "thr_b", "name": "Bug bash notes"}},
			invoke: func() error {
				thread, err := client.UnarchiveThread(context.Background(), "thr_b")
				if err == nil && thread.ID != "thr_b" {
					t.Errorf("thread = %+v", thread)
				}
				return err
			},
			wantParams: `{"threadId":"thr_b"}`,
		},
		{
			name:   "name/set",
			method: "thread/name/set",
			result: map[string]any{},
			invoke: func() error {
				return client.Thread("thr_b").SetName(context.Background(), "Renamed")
			},
			wantParams: `{"threadId":"thr_b","name":"Renamed"}`,
		},
		{
			name:   "loaded/list",
			method: "thread/loaded/list",
			result: map[string]any{"data": []string{"thr_123", "thr_456"}},
			invoke: func() error {
				ids, err := client.ListLoadedThreads(context.Background())
				if err == nil && len(ids) != 2 {
					t.Errorf("ids = %v", ids)
				}
				return err
			},
			wantParams: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			done := serve(t, func() {
				req := server.expect(tc.method)
				if string(req.Params) != tc.wantParams {
					t.Errorf("params = %s, want %s", req.Params, tc.wantParams)
				}
				server.respond(req, tc.result)
			})
			if err := tc.invoke(); err != nil {
				t.Fatalf("%s: %v", tc.method, err)
			}
			<-done
		})
	}
}

func TestThreadUnsubscribe(t *testing.T) {
	client, server := connect(t, Options{})

	done := serve(t, func() {
		req := server.expect("thread/start")
		server.respond(req, map[string]any{"thread": map[string]any{"id": "thr_1"}})
		req = server.expect("thread/unsubscribe")
		server.respond(req, map[string]any{"status": "unsubscribed"})
	})

	if _, err := client.StartThread(context.Background(), StartThreadParams{}); err != nil {
		t.Fatalf("StartThread: %v", err)
	}
	events, errs := watchThread(t, client, "thr_1")

	status, err := client.Thread("thr_1").Unsubscribe(context.Background())
	<-done
	if err != nil {
		t.Fatalf("Unsubscribe: %v", err)
	}
	if status != "unsubscribed" {
		t.Fatalf("status = %q", status)
	}
	select {
	case _, ok := <-events:
		if ok {
			t.Fatal("Thread.Events still running")
		}
		if err := <-errs; err != nil {
			t.Fatalf("Thread.Events ended with %v, want a clean end", err)
		}
	case <-time.After(fakeTimeout):
		t.Fatal("Thread.Events not ended by unsubscribe")
	}
	if client.lookup("thr_1") != nil {
		t.Fatal("subscription not removed")
	}
}

func TestThreadNotificationRouting(t *testing.T) {
	client, server := connect(t, Options{})

	done := serve(t, func() {
		for _, id := range []string{"thr_1", "thr_2"} {
			req := server.expect("thread/start")
			server.respond(req, map[string]any{"thread": map[string]any{"id": id}})
		}
	})
	for range 2 {
		if _, err := client.StartThread(context.Background(), StartThreadParams{}); err != nil {
			t.Fatalf("StartThread: %v", err)
		}
	}
	<-done

	first, _ := watchThread(t, client, "thr_1")
	second, _ := watchThread(t, client, "thr_2")

	server.notify(MethodThreadStatusChanged, map[string]any{
		"threadId": "thr_1",
		"status":   map[string]any{"type": "active", "activeFlags": []string{"waitingOnApproval"}},
	})
	server.notify(MethodThreadArchived, map[string]any{"threadId": "thr_1"})
	server.notify(MethodThreadNameUpdated, map[string]any{"threadId": "thr_1", "threadName": "Renamed"})
	// An event for a thread nobody subscribed to must be dropped silently.
	server.notify(MethodThreadClosed, map[string]any{"threadId": "thr_unknown"})

	statusEvent := recvThreadEvent(t, first)
	if statusEvent.Method != MethodThreadStatusChanged || statusEvent.Status == nil {
		t.Fatalf("event = %+v", statusEvent)
	}
	if statusEvent.Status.Type != ThreadStatusActive || len(statusEvent.Status.ActiveFlags) != 1 {
		t.Fatalf("status = %+v", statusEvent.Status)
	}
	if got := recvThreadEvent(t, first); got.Method != MethodThreadArchived {
		t.Fatalf("event = %+v", got)
	}
	if got := recvThreadEvent(t, first); got.Name != "Renamed" {
		t.Fatalf("event = %+v", got)
	}

	select {
	case event := <-second:
		t.Fatalf("thr_2 received %+v", event)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestThreadStartedNotificationCarriesThread(t *testing.T) {
	client, server := connect(t, Options{})

	done := serve(t, func() {
		req := server.expect("thread/start")
		server.respond(req, map[string]any{"thread": map[string]any{"id": "thr_1"}})
	})
	if _, err := client.StartThread(context.Background(), StartThreadParams{}); err != nil {
		t.Fatalf("StartThread: %v", err)
	}
	<-done

	events, _ := watchThread(t, client, "thr_1")
	server.notify(MethodThreadStarted, map[string]any{"thread": map[string]any{"id": "thr_1", "preview": "hello"}})

	event := recvThreadEvent(t, events)
	if event.Thread == nil || event.Thread.Preview != "hello" {
		t.Fatalf("event = %+v", event)
	}
	if event.ThreadID != "thr_1" {
		t.Fatalf("threadId = %q", event.ThreadID)
	}
}

func TestThreadEventsClosedOnShutdown(t *testing.T) {
	client, server := connect(t, Options{})

	done := serve(t, func() {
		req := server.expect("thread/start")
		server.respond(req, map[string]any{"thread": map[string]any{"id": "thr_1"}})
	})
	if _, err := client.StartThread(context.Background(), StartThreadParams{}); err != nil {
		t.Fatalf("StartThread: %v", err)
	}
	<-done

	events, errs := watchThread(t, client, "thr_1")
	_ = client.Close()

	select {
	case _, ok := <-events:
		if ok {
			t.Fatal("Thread.Events still running after Close")
		}
		if err := <-errs; !errors.Is(err, ErrClosed) {
			t.Fatalf("Thread.Events ended with %v, want ErrClosed", err)
		}
	case <-time.After(fakeTimeout):
		t.Fatal("Thread.Events not ended after Close")
	}

	// A loop started after the shutdown ends at once with the client's error.
	for _, err := range client.Thread("thr_1").Events(context.Background()) {
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("late Thread.Events = %v, want ErrClosed", err)
		}
	}
}

func TestThreadEventsNotSubscribed(t *testing.T) {
	client, _ := connect(t, Options{})
	var got error
	for _, err := range client.Thread("thr_missing").Events(context.Background()) {
		got = err
	}
	if got == nil {
		t.Fatal("Thread.Events on an unsubscribed thread yielded no error")
	}
}

func TestThreadEventsContextAndFanOut(t *testing.T) {
	client, server := connect(t, Options{})
	done := serve(t, func() {
		req := server.expect("thread/start")
		server.respond(req, map[string]any{"thread": map[string]any{"id": "thr_1"}})
	})
	if _, err := client.StartThread(context.Background(), StartThreadParams{}); err != nil {
		t.Fatalf("StartThread: %v", err)
	}
	<-done

	first, _ := watchThread(t, client, "thr_1")
	ctx, cancel := context.WithCancel(context.Background())
	second, secondErrs := watchSeq(t, client.lookup("thr_1").events, client.Thread("thr_1").Events(ctx))

	server.notify(MethodThreadArchived, map[string]any{"threadId": "thr_1"})
	if got := recvThreadEvent(t, first); got.Method != MethodThreadArchived {
		t.Fatalf("first = %+v", got)
	}
	if got := recvThreadEvent(t, second); got.Method != MethodThreadArchived {
		t.Fatalf("second = %+v", got)
	}

	cancel()
	for range second {
	}
	if err := <-secondErrs; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled loop ended with %v", err)
	}
	waitFor(t, func() bool { return client.lookup("thr_1").events.listenerCount() == 1 })
}

// watchThread ranges over Thread.Events for threadID on a goroutine until the
// test ends, forwarding events. It returns once the loop is registered.
func watchThread(t *testing.T, client *Client, threadID string) (<-chan ThreadEvent, <-chan error) {
	t.Helper()
	sub := client.lookup(threadID)
	if sub == nil {
		t.Fatalf("thread %s not subscribed", threadID)
	}
	return watchSeq(t, sub.events, client.Thread(threadID).Events(t.Context()))
}

// watchSeq ranges over seq on a goroutine, forwarding values until it ends;
// the error it ended with, or nil, follows on the second channel. It returns
// once seq has registered a listener on b.
func watchSeq[T any](t *testing.T, b *broadcast[T], seq iter.Seq2[T, error]) (<-chan T, <-chan error) {
	t.Helper()
	before := b.listenerCount()
	values := make(chan T, 64)
	errs := make(chan error, 1)
	go func() {
		defer close(values)
		for v, err := range seq {
			if err != nil {
				errs <- err
				return
			}
			values <- v
		}
		errs <- nil
	}()
	waitFor(t, func() bool { return b.listenerCount() > before })
	return values, errs
}

// waitFor polls cond until it holds or the fake timeout passes.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(fakeTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(time.Millisecond)
	}
}

func recvThreadEvent(t *testing.T, ch <-chan ThreadEvent) ThreadEvent {
	t.Helper()
	select {
	case event, ok := <-ch:
		if !ok {
			t.Fatal("thread event channel closed")
		}
		return event
	case <-time.After(fakeTimeout):
		t.Fatal("timed out waiting for a thread event")
		return ThreadEvent{}
	}
}

func TestThreadHandleRun(t *testing.T) {
	client, server := connect(t, Options{})

	done := serve(t, func() {
		server.respond(server.expect("thread/start"), map[string]any{"thread": map[string]any{
			"id": "thr_1", "preview": "hello",
			"turns": []any{map[string]any{"id": "turn_0", "status": "completed", "items": []any{}}},
		}})
		req := server.expect("turn/start")
		if string(req.Params) != `{"threadId":"thr_1","input":[{"text":"a","type":"text"},{"text":"b","type":"text"}]}` {
			t.Errorf("params = %s", req.Params)
		}
		server.respond(req, map[string]any{"turn": map[string]any{"id": "turn_1", "status": "inProgress"}})
		notifyTurn(server, MethodItemCompleted, completedMessage("m1", "done", ""))
		server.notify(MethodTurnCompleted, map[string]any{"threadId": "thr_1",
			"turn": map[string]any{"id": "turn_1", "status": "completed"}})
	})
	thread, err := client.StartThread(context.Background(), StartThreadParams{})
	if err != nil {
		t.Fatalf("StartThread: %v", err)
	}
	if info := thread.Info(); info.ID != "thr_1" || info.Preview != "hello" || len(info.Turns) != 1 {
		t.Fatalf("info = %+v", info)
	}
	result, err := thread.Run(context.Background(), Text("a"), Text("b"))
	<-done
	if err != nil || result.Text() != "done" {
		t.Fatalf("Run = %+v, %v", result, err)
	}
}

func TestClientThreadHandle(t *testing.T) {
	client, server := connect(t, Options{})
	thread := client.Thread("thr_9")
	if thread.ID() != "thr_9" || thread.Info().ID != "thr_9" || thread.Info().Preview != "" {
		t.Fatalf("handle = %+v", thread.Info())
	}

	// A turn on a known id needs no thread/start from this client.
	done := serve(t, func() {
		req := server.expect("turn/start")
		if string(req.Params) != `{"threadId":"thr_9","input":[{"text":"hi","type":"text"}]}` {
			t.Errorf("params = %s", req.Params)
		}
		server.respond(req, map[string]any{"turn": map[string]any{"id": "turn_1", "status": "inProgress"}})
	})
	stream, err := thread.Send(context.Background(), Text("hi"))
	<-done
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if stream.ThreadID() != "thr_9" || stream.TurnID() != "turn_1" {
		t.Fatalf("stream ids = %q %q", stream.ThreadID(), stream.TurnID())
	}
	_ = stream.Close()

	if _, err := client.Thread("").Send(context.Background(), Text("hi")); err == nil {
		t.Fatal("Send on an empty thread id succeeded")
	}
}

func TestThreadGoal(t *testing.T) {
	client, server := connect(t, Options{})
	thread := client.Thread("thr_1")

	done := serve(t, func() {
		req := server.expect("thread/goal/set")
		if string(req.Params) != `{"threadId":"thr_1","objective":"ship it","status":"active"}` {
			t.Errorf("set params = %s", req.Params)
		}
		server.respond(req, map[string]any{"goal": map[string]any{
			"threadId": "thr_1", "objective": "ship it", "status": GoalActive}})

		req = server.expect("thread/goal/get")
		if string(req.Params) != `{"threadId":"thr_1"}` {
			t.Errorf("get params = %s", req.Params)
		}
		server.respond(req, map[string]any{"goal": nil})

		req = server.expect("thread/goal/clear")
		if string(req.Params) != `{"threadId":"thr_1"}` {
			t.Errorf("clear params = %s", req.Params)
		}
		server.respond(req, map[string]any{"cleared": true})
	})
	goal, err := thread.SetGoal(context.Background(), SetGoalParams{Objective: "ship it", Status: GoalActive})
	if err != nil || goal.Objective != "ship it" || goal.Status != GoalActive {
		t.Fatalf("SetGoal = %+v, %v", goal, err)
	}
	if goal, err := thread.Goal(context.Background()); err != nil || goal != nil {
		t.Fatalf("Goal = %+v, %v", goal, err)
	}
	if cleared, err := thread.ClearGoal(context.Background()); err != nil || !cleared {
		t.Fatalf("ClearGoal = %v, %v", cleared, err)
	}
	<-done
}
