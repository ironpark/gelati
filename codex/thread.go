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
	Thread *Thread
	// Status is set for thread/status/changed.
	Status *ThreadStatus
	// Name is set for thread/name/updated.
	Name string
	// Params is the raw notification payload.
	Params jsontext.Value
}

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

// emit delivers an event to every ThreadEvents loop without ever blocking the
// transport reader. A slow consumer loses events rather than stalling the
// connection.
func (s *threadSubscription) emit(c *Client, event ThreadEvent) {
	if s.events.publish(event) {
		c.logger.Debug("codex: dropped thread event", "threadId", s.id, "method", event.Method)
	}
}

// close stops delivery and ends every ThreadEvents loop.
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

// unsubscribeLocal removes a thread subscription and ends its ThreadEvents
// loops.
func (c *Client) unsubscribeLocal(threadID string) {
	c.mu.Lock()
	sub := c.threads[threadID]
	delete(c.threads, threadID)
	c.mu.Unlock()
	if sub != nil {
		sub.close()
	}
}

// ThreadEvents iterates the lifecycle events of a subscribed thread (one
// opened by StartThread, ResumeThread, or ForkThread). Events are received
// from the moment the loop starts until it exits; any number of loops may
// range over the same thread, each seeing every event.
//
// The sequence ends cleanly when UnsubscribeThread drops the thread. It ends
// with an error when ctx ends (ctx's error), when the client shuts down
// (Client.Err, typically ErrClosed), or immediately when the thread is not
// subscribed.
//
// Each loop buffers up to Options.EventBuffer events; while that buffer is
// full, newer events are dropped rather than stalling the connection, so
// keep the loop body short.
func (c *Client) ThreadEvents(ctx context.Context, threadID string) iter.Seq2[ThreadEvent, error] {
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
// delivered on ThreadEvents.
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
	var result ThreadResult
	if err := c.tr.Call(ctx, "thread/start", params, &result); err != nil {
		return nil, err
	}
	c.subscribe(result.Thread.ID)
	return &result.Thread, nil
}

// ResumeThread reopens a stored thread so later turns append to it.
func (c *Client) ResumeThread(ctx context.Context, params ResumeThreadParams) (*Thread, error) {
	var result ThreadResult
	if err := c.tr.Call(ctx, "thread/resume", params, &result); err != nil {
		return nil, err
	}
	c.subscribe(result.Thread.ID)
	return &result.Thread, nil
}

// ForkThread branches a stored thread into a new thread id.
func (c *Client) ForkThread(ctx context.Context, params ForkThreadParams) (*Thread, error) {
	var result ThreadResult
	if err := c.tr.Call(ctx, "thread/fork", params, &result); err != nil {
		return nil, err
	}
	c.subscribe(result.Thread.ID)
	return &result.Thread, nil
}

// ReadThread reads a stored thread without resuming or subscribing to it.
func (c *Client) ReadThread(ctx context.Context, params ReadThreadParams) (*Thread, error) {
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
func (c *Client) AllThreads(ctx context.Context, params ListThreadsParams) iter.Seq2[Thread, error] {
	return func(yield func(Thread, error) bool) {
		page := params
		for {
			result, err := c.ListThreads(ctx, page)
			if err != nil {
				yield(Thread{}, err)
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
func (c *Client) UnarchiveThread(ctx context.Context, threadID string) (*Thread, error) {
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

// UnsubscribeThread drops this connection's subscription to a thread and
// ends its ThreadEvents loops. The returned status is "unsubscribed",
// "notSubscribed", or "notLoaded".
func (c *Client) UnsubscribeThread(ctx context.Context, threadID string) (string, error) {
	var result UnsubscribeResult
	err := c.tr.Call(ctx, "thread/unsubscribe", ThreadIDParams{ThreadID: threadID}, &result)
	c.unsubscribeLocal(threadID)
	if err != nil {
		return "", err
	}
	return result.Status, nil
}

// SetThreadName sets a thread's user-facing name.
func (c *Client) SetThreadName(ctx context.Context, threadID, name string) error {
	params := struct {
		ThreadID string `json:"threadId"`
		Name     string `json:"name"`
	}{ThreadID: threadID, Name: name}
	return c.tr.Call(ctx, "thread/name/set", params, nil)
}

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
