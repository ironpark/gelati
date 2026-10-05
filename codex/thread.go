package codex

import (
	"context"
	"encoding/json/jsontext"
	"fmt"
	"iter"
	"sync"

	"github.com/ironpark/gelati/internal/jsonx"
	"github.com/ironpark/gelati/internal/lifecycle"
)

// ThreadEvent is a thread lifecycle notification delivered to a subscriber.
type ThreadEvent struct {
	// Method is the notification method, one of the MethodThread* constants.
	Method string
	// ThreadID is the thread the event belongs to.
	ThreadID string
	// Thread is set for thread/started.
	Thread *ThreadInfo
	// Status is set for thread/status/changed.
	Status *ThreadStatus
	// Name is set for thread/name/updated.
	Name string
	// Params is the raw notification payload.
	Params jsontext.Value
}

// Thread is a handle on one thread of a Client. StartThread, ResumeThread,
// and ForkThread return one; Client.Thread wraps a known thread id. A Thread
// is safe for concurrent use.
type Thread struct {
	client *Client
	info   ThreadInfo
}

// Thread returns a handle for the thread id without contacting the server.
// Its Info holds only the id. Turns need the thread loaded on this client's
// app-server, by StartThread, ResumeThread, or ForkThread.
func (c *Client) Thread(id string) *Thread {
	return &Thread{client: c, info: ThreadInfo{ID: id}}
}

// ID returns the thread id.
func (t *Thread) ID() string { return t.info.ID }

// Info returns the thread as the server reported it when the handle was
// created. It is not refreshed; call Client.ReadThread for the current state.
// Its Turns are set when the server returned the history, which the handle
// then keeps; set ExcludeTurns on resume or fork to leave it out.
func (t *Thread) Info() ThreadInfo { return t.info }

// threadSubscription tracks one subscribed thread and its active turns.
type threadSubscription struct {
	id     string
	events *broadcast[ThreadEvent]
	// wake signals the pump that queue has notifications.
	wake chan struct{}
	// quit ends when the subscription closes; it stops enqueue and the pump.
	quit lifecycle.Done

	mu      sync.Mutex
	streams []*TurnStream

	// queue holds turn notifications waiting for the pump (see enqueue).
	queueMu sync.Mutex
	queue   []queuedNotification
}

// emit delivers an event to every Events loop without ever blocking the
// transport reader. A slow consumer loses events rather than stalling the
// connection.
func (s *threadSubscription) emit(c *Client, event ThreadEvent) {
	if s.events.publish(event) {
		c.logger.Debug("codex: dropped thread event", "threadId", s.id, "method", event.Method)
	}
}

// close stops delivery and ends every Events loop.
func (s *threadSubscription) close() {
	if s.quit.Finish(nil) {
		s.events.close()
	}
}

// eventBuffer returns the configured per-listener buffer capacity.
func (c *Client) eventBuffer() int {
	if c.opts.EventBuffer > 0 {
		return c.opts.EventBuffer
	}
	return defaultEventBuffer
}

// subscribe registers interest in a thread's notifications. The app-server
// subscribes the connection automatically on thread/start, thread/resume, and
// thread/fork; this mirrors that state on the client side.
func (c *Client) subscribe(threadID string) *threadSubscription {
	if threadID == "" {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if sub, ok := c.threads[threadID]; ok {
		return sub
	}
	sub := &threadSubscription{
		id:     threadID,
		events: newBroadcast[ThreadEvent](c.eventBuffer()),
		wake:   make(chan struct{}, 1),
	}
	c.threads[threadID] = sub
	go sub.pump(c)
	return sub
}

// lookup returns the subscription for a thread, if any.
func (c *Client) lookup(threadID string) *threadSubscription {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.threads[threadID]
}

// unsubscribeLocal removes a thread subscription and ends its Events loops.
func (c *Client) unsubscribeLocal(threadID string) {
	c.mu.Lock()
	sub := c.threads[threadID]
	delete(c.threads, threadID)
	c.mu.Unlock()
	if sub != nil {
		sub.close()
	}
}

// Events iterates the lifecycle events of the thread while this client is
// subscribed to it (after StartThread, ResumeThread, ForkThread, or a turn
// started on it). Events are received from the moment the loop starts until
// it exits; any number of loops may range over the same thread, each seeing
// every event.
//
// The sequence ends cleanly when Unsubscribe drops the thread. It ends with
// an error when ctx ends (ctx's error), when the client shuts down
// (Client.Err, typically ErrClosed), or immediately when the thread is not
// subscribed.
//
// Each loop buffers up to Options.EventBuffer events; while that buffer is
// full, newer events are dropped rather than stalling the connection, so
// keep the loop body short.
func (t *Thread) Events(ctx context.Context) iter.Seq2[ThreadEvent, error] {
	c, threadID := t.client, t.ID()
	return func(yield func(ThreadEvent, error) bool) {
		sub := c.lookup(threadID)
		if sub == nil {
			if err := c.Err(); err != nil {
				yield(ThreadEvent{}, err)
			} else {
				yield(ThreadEvent{}, fmt.Errorf("codex: thread %q is not subscribed", threadID))
			}
			return
		}
		sub.events.seq(ctx, c.endErr)(yield)
	}
}

// endErr is the error a subscription iterator ends with once its source
// closes: nil after an unsubscribe, the client's error after a shutdown.
func (c *Client) endErr() error {
	select {
	case <-c.tr.Done():
		return c.Err()
	default:
		return nil
	}
}

// isThreadMethod reports whether method is a thread lifecycle notification
// delivered on Thread.Events.
func isThreadMethod(method string) bool {
	switch method {
	case MethodThreadStarted, MethodThreadStatusChanged, MethodThreadArchived,
		MethodThreadUnarchived, MethodThreadDeleted, MethodThreadClosed,
		MethodThreadNameUpdated, MethodServerRequestResolved:
		return true
	default:
		return false
	}
}

// routeThreadNotification delivers a thread lifecycle notification to its
// subscriber.
func (c *Client) routeThreadNotification(method string, params jsontext.Value, threadID string) {
	if !isThreadMethod(method) {
		return
	}
	sub := c.lookup(threadID)
	if sub == nil {
		c.logger.Debug("codex: event for unknown thread", "threadId", threadID, "method", method)
		return
	}

	event := ThreadEvent{Method: method, ThreadID: threadID, Params: params}
	switch method {
	case MethodThreadStarted:
		var payload ThreadStartedParams
		if err := jsonx.Unmarshal(params, &payload); err == nil {
			event.Thread = &payload.Thread
		}
	case MethodThreadStatusChanged:
		var payload ThreadStatusChangedParams
		if err := jsonx.Unmarshal(params, &payload); err == nil {
			event.Status = &payload.Status
		}
	case MethodThreadNameUpdated:
		var payload ThreadNameUpdatedParams
		if err := jsonx.Unmarshal(params, &payload); err == nil {
			event.Name = payload.Name
		}
	}
	sub.emit(c, event)
}

// StartThread creates a new thread and subscribes to its turn and item events.
func (c *Client) StartThread(ctx context.Context, params StartThreadParams) (*Thread, error) {
	return c.openThread(ctx, "thread/start", params)
}

// ResumeThread reopens a stored thread so later turns append to it.
func (c *Client) ResumeThread(ctx context.Context, params ResumeThreadParams) (*Thread, error) {
	return c.openThread(ctx, "thread/resume", params)
}

// ForkThread branches a stored thread into a new thread id.
func (c *Client) ForkThread(ctx context.Context, params ForkThreadParams) (*Thread, error) {
	return c.openThread(ctx, "thread/fork", params)
}

// openThread sends a thread/start, thread/resume, or thread/fork request and
// subscribes to the thread it returns.
func (c *Client) openThread(ctx context.Context, method string, params any) (*Thread, error) {
	var result ThreadResult
	if err := c.tr.Call(ctx, method, params, &result); err != nil {
		return nil, err
	}
	c.subscribe(result.Thread.ID)
	return &Thread{client: c, info: result.Thread}, nil
}

// ReadThread reads a stored thread without resuming or subscribing to it.
func (c *Client) ReadThread(ctx context.Context, params ReadThreadParams) (*ThreadInfo, error) {
	var result ThreadResult
	if err := c.tr.Call(ctx, "thread/read", params, &result); err != nil {
		return nil, err
	}
	return &result.Thread, nil
}

// ListThreads returns one page of stored threads. An empty NextCursor means
// the final page.
func (c *Client) ListThreads(ctx context.Context, params ListThreadsParams) (*ListThreadsResult, error) {
	var result ListThreadsResult
	if err := c.tr.Call(ctx, "thread/list", params, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// AllThreads iterates every stored thread matching params, following
// nextCursor to exhaustion. Iteration stops after yielding a non-nil error.
func (c *Client) AllThreads(ctx context.Context, params ListThreadsParams) iter.Seq2[ThreadInfo, error] {
	return func(yield func(ThreadInfo, error) bool) {
		page := params
		for {
			result, err := c.ListThreads(ctx, page)
			if err != nil {
				yield(ThreadInfo{}, err)
				return
			}
			for _, thread := range result.Data {
				if !yield(thread, nil) {
					return
				}
			}
			if result.NextCursor == "" {
				return
			}
			page.Cursor = result.NextCursor
		}
	}
}

// ArchiveThread moves a thread's log into the archived directory, along with
// spawned descendant threads.
func (c *Client) ArchiveThread(ctx context.Context, threadID string) error {
	return c.tr.Call(ctx, "thread/archive", ThreadIDParams{ThreadID: threadID}, nil)
}

// UnarchiveThread restores an archived thread and returns it.
func (c *Client) UnarchiveThread(ctx context.Context, threadID string) (*ThreadInfo, error) {
	var result ThreadResult
	if err := c.tr.Call(ctx, "thread/unarchive", ThreadIDParams{ThreadID: threadID}, &result); err != nil {
		return nil, err
	}
	return &result.Thread, nil
}

// DeleteThread permanently deletes a stored thread and its spawned
// descendants.
func (c *Client) DeleteThread(ctx context.Context, threadID string) error {
	return c.tr.Call(ctx, "thread/delete", ThreadIDParams{ThreadID: threadID}, nil)
}

// Unsubscribe drops this connection's subscription to the thread, ends its
// Events loops, and fails its open turn streams with ErrClosed. The returned
// status is "unsubscribed", "notSubscribed", or "notLoaded".
func (t *Thread) Unsubscribe(ctx context.Context) (string, error) {
	var result UnsubscribeResult
	err := t.client.tr.Call(ctx, "thread/unsubscribe", t.idParams(), &result)
	t.client.unsubscribeLocal(t.ID())
	if err != nil {
		return "", err
	}
	return result.Status, nil
}

// SetName sets the thread's user-facing name.
func (t *Thread) SetName(ctx context.Context, name string) error {
	params := struct {
		ThreadID string `json:"threadId"`
		Name     string `json:"name"`
	}{ThreadID: t.ID(), Name: name}
	return t.client.tr.Call(ctx, "thread/name/set", params, nil)
}

// Compact triggers manual history compaction. Progress streams as ordinary
// turn and item notifications.
func (t *Thread) Compact(ctx context.Context) error {
	return t.client.tr.Call(ctx, "thread/compact/start", t.idParams(), nil)
}

// RunShellCommand runs a user-initiated shell command against the thread. It
// runs outside the sandbox with full access.
func (t *Thread) RunShellCommand(ctx context.Context, command string) error {
	params := struct {
		ThreadID string `json:"threadId"`
		Command  string `json:"command"`
	}{ThreadID: t.ID(), Command: command}
	return t.client.tr.Call(ctx, "thread/shellCommand", params, nil)
}

// idParams returns the `{ "threadId": ... }` params of the thread.
func (t *Thread) idParams() ThreadIDParams { return ThreadIDParams{ThreadID: t.ID()} }

// ListLoadedThreads returns the thread ids currently loaded in memory.
func (c *Client) ListLoadedThreads(ctx context.Context) ([]string, error) {
	var result struct {
		Data []string `json:"data"`
	}
	if err := c.tr.Call(ctx, "thread/loaded/list", nil, &result); err != nil {
		return nil, err
	}
	return result.Data, nil
}

// shutdown closes every subscription once the transport stops.
func (c *Client) shutdown() {
	c.accounts.close()
	c.mu.Lock()
	subs := c.threads
	c.threads = make(map[string]*threadSubscription)
	c.mu.Unlock()

	for _, sub := range subs {
		for _, stream := range sub.activeStreams() {
			stream.finish(nil, ErrClosed)
		}
		sub.close()
	}
}
