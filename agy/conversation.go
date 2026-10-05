package agy

import (
	"context"
	"iter"
	"slices"
	"sync"
	"time"
)

// DefaultMaxHistorySize is the number of steps a Conversation keeps before
// discarding the oldest.
const DefaultMaxHistorySize = 10_000

// Conversation is a stateful session over a Connection: it records the step
// history (noting where the context was compacted), counts turns, tracks
// per-turn usage, and offers Chat, which sends a prompt and returns a
// streaming TurnStream. Agent.Conversation returns the agent's
// conversation; its methods are safe for concurrent use.
type Conversation struct {
	conn *Connection

	mu    sync.Mutex
	steps []*Step
	// dropped counts the steps trimmed from the front of the history;
	// turnStarts and compactions hold indices counted from before them.
	dropped        int
	turnStarts     []int
	compactions    []int
	maxHistory     int
	turnStartUsage *UsageMetadata
	last           *TurnStream
}

func newConversation(conn *Connection, history []*Step) *Conversation {
	c := &Conversation{conn: conn, maxHistory: DefaultMaxHistorySize}
	for _, s := range history {
		c.appendLocked(s)
	}
	return c
}

// SetMaxHistorySize changes how many steps the history keeps; zero
// disables the limit. Excess steps are discarded oldest first.
func (c *Conversation) SetMaxHistorySize(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.maxHistory = n
	c.enforceMaxLocked()
}

func (c *Conversation) appendLocked(step *Step) {
	if step.Type == StepTypeCompaction {
		c.compactions = append(c.compactions, c.dropped+len(c.steps))
	}
	c.steps = append(c.steps, step)
	c.enforceMaxLocked()
}

func (c *Conversation) record(step *Step) {
	c.mu.Lock()
	c.appendLocked(step)
	c.mu.Unlock()
}

// enforceMaxLocked trims the history to maxHistory steps. It reslices
// rather than copies, so trimming one step per append is amortized O(1):
// append moves the live steps to a new array when the old one fills up.
func (c *Conversation) enforceMaxLocked() {
	if c.maxHistory <= 0 || len(c.steps) <= c.maxHistory {
		return
	}
	overflow := len(c.steps) - c.maxHistory
	clear(c.steps[:overflow])
	c.steps = c.steps[overflow:]
	c.dropped += overflow
	drop := func(idx []int) []int {
		n, _ := slices.BinarySearch(idx, c.dropped)
		return idx[n:]
	}
	c.turnStarts = drop(c.turnStarts)
	c.compactions = drop(c.compactions)
}

// Send starts a turn. If the previous turn is still running, its remaining
// steps are drained into the history first (through the previous
// TurnStream when there is one), and Send waits for the agent to go idle;
// when another goroutine is reading the steps, Send only waits. Errors that
// end the previous turn belong to that turn and are not returned here
// (upstream raises them from send).
func (c *Conversation) Send(ctx context.Context, content ...Content) error {
	_, err := c.send(ctx, content)
	return err
}

func (c *Conversation) send(ctx context.Context, content []Content) (*turn, error) {
	if !c.conn.IsIdle() {
		c.mu.Lock()
		prev := c.last
		c.mu.Unlock()
		if prev != nil {
			_, _ = prev.drain(ctx)
		}
		for _, err := range c.ReceiveSteps(ctx) {
			if err != nil {
				break
			}
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := c.conn.WaitForIdle(ctx); err != nil {
			return nil, err
		}
	}
	c.mu.Lock()
	c.turnStarts = append(c.turnStarts, c.dropped+len(c.steps))
	usage := c.conn.CumulativeUsage()
	c.turnStartUsage = &usage
	c.mu.Unlock()
	return c.conn.sendTurn(ctx, content)
}

// ReceiveSteps yields the current turn's steps as they arrive, recording
// each in the history, until the agent goes idle. See
// Connection.ReceiveSteps for the errors that end the sequence.
func (c *Conversation) ReceiveSteps(ctx context.Context) iter.Seq2[*Step, error] {
	return c.conn.receiveSteps(ctx, c.record)
}

// Chat sends a prompt and returns the turn's stream at once. It is Send
// followed by reading the turn as chunks; the steps behind them are
// recorded in the history as they are read.
func (c *Conversation) Chat(ctx context.Context, content ...Content) (*TurnStream, error) {
	t, err := c.send(ctx, content)
	if err != nil {
		return nil, err
	}
	src := c.newChunkSource(t)
	resp := newTurnStream(src.next)
	resp.conv, resp.turn, resp.onDone = c, t, src.release
	c.mu.Lock()
	c.last = resp
	c.mu.Unlock()
	return resp, nil
}

// chunkSource turns the step stream of one turn into chunks.
type chunkSource struct {
	conv    *Conversation
	t       *turn // nil for the current turn when reading starts
	r       *stepReceiver
	pending []Chunk
	seen    map[string]bool
	ended   bool
}

func (c *Conversation) newChunkSource(t *turn) *chunkSource {
	return &chunkSource{conv: c, t: t, seen: map[string]bool{}}
}

func (s *chunkSource) next(ctx context.Context) (Chunk, bool, error) {
	for len(s.pending) == 0 {
		if s.ended {
			return nil, false, nil
		}
		if s.r == nil {
			r, err := s.conv.conn.newReceiver(s.t)
			if err != nil {
				return nil, false, err
			}
			s.r = r
		}
		step, err := s.r.next(ctx)
		if err != nil {
			return nil, false, err
		}
		if step == nil {
			s.ended = true
			return nil, false, nil
		}
		s.conv.record(step)
		s.pending = chunksFromStep(step, s.seen)
	}
	ch := s.pending[0]
	s.pending = s.pending[1:]
	return ch, true, nil
}

func (s *chunkSource) release() {
	if s.r != nil {
		s.r.release()
	}
}

// chunksFromStep derives the chunks of one step. Tool calls repeated across
// steps (dispatch, execution, result) are reported once per ID; calls
// without an ID are always reported.
func chunksFromStep(step *Step, seen map[string]bool) []Chunk {
	var out []Chunk
	if step.Source == StepSourceModel && step.Target == StepTargetUser && step.Status != StepStatusError {
		if step.ThinkingDelta != "" {
			out = append(out, &ThoughtChunk{StepIndex: step.StepIndex, Text: step.ThinkingDelta})
		}
		if step.ContentDelta != "" {
			out = append(out, &TextChunk{StepIndex: step.StepIndex, Text: step.ContentDelta})
		}
	}
	for _, call := range step.ToolCalls {
		if call.ID == "" || !seen[call.ID] {
			if call.ID != "" {
				seen[call.ID] = true
			}
			out = append(out, call)
		}
	}
	return out
}

// LastStructuredOutput returns the structured output of the most recent
// finish step in the history, or nil.
func (c *Conversation) LastStructuredOutput() any {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range slices.Backward(c.steps) {
		if s.Type == StepTypeFinish {
			return s.StructuredOutput
		}
	}
	return nil
}

// History returns every step received so far, oldest first: the full
// transcript, including steps compacted out of the model's context (see
// CompactionIndices).
func (c *Conversation) History() []*Step {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.steps)
}

// LastResponse returns the content of the most recent complete model
// response, or "".
func (c *Conversation) LastResponse() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range slices.Backward(c.steps) {
		if s.IsCompleteResponse {
			return s.Content
		}
	}
	return ""
}

// TurnCount returns the number of Send calls.
func (c *Conversation) TurnCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.turnStarts)
}

// CompactionIndices returns the History indices of the compaction steps.
// Steps before them may no longer be in the model's context.
func (c *Conversation) CompactionIndices() []int {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []int
	for _, idx := range c.compactions {
		out = append(out, idx-c.dropped)
	}
	return out
}

// ClearHistory discards the recorded history, turn and compaction indices
// and the per-turn usage baseline. The session itself continues.
func (c *Conversation) ClearHistory() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.steps = nil
	c.dropped = 0
	c.turnStarts = nil
	c.compactions = nil
	c.turnStartUsage = nil
}

// Connection returns the underlying connection. Reading steps from it
// directly bypasses the history.
func (c *Conversation) Connection() *Connection { return c.conn }

// IsIdle reports whether the agent is idle and ready for input.
func (c *Conversation) IsIdle() bool { return c.conn.IsIdle() }

// ConversationID returns the conversation identifier assigned by the
// runtime; pass it as Options.ConversationID to resume the session later.
func (c *Conversation) ConversationID() string { return c.conn.ConversationID() }

// SandboxStatus returns the OS command sandbox status reported at the
// handshake, or nil.
func (c *Conversation) SandboxStatus() *SandboxStatus { return c.conn.SandboxStatus() }

// TotalUsage returns the cumulative token usage of the session.
func (c *Conversation) TotalUsage() UsageMetadata { return c.conn.CumulativeUsage() }

// TrajectoryUsages returns the cumulative token usage per trajectory: the
// main agent (keyed by the conversation ID) and each subagent run.
func (c *Conversation) TrajectoryUsages() map[string]UsageMetadata { return c.conn.TrajectoryUsages() }

// LastTurnUsage returns the token usage of the most recent turn, or nil
// when it used no tokens or no turn was sent.
func (c *Conversation) LastTurnUsage() *UsageMetadata {
	c.mu.Lock()
	start := c.turnStartUsage
	c.mu.Unlock()
	if start == nil {
		return nil
	}
	diff := c.conn.CumulativeUsage().Sub(*start)
	if val(diff.TotalTokenCount) == 0 {
		return nil
	}
	return &diff
}

// Cancel halts the current turn.
func (c *Conversation) Cancel(ctx context.Context) error { return c.conn.Cancel(ctx) }

// WaitForIdle blocks until the agent is idle.
func (c *Conversation) WaitForIdle(ctx context.Context) error { return c.conn.WaitForIdle(ctx) }

// WaitForWakeup reports whether the conversation woke up within timeout;
// see Connection.WaitForWakeup.
func (c *Conversation) WaitForWakeup(ctx context.Context, timeout time.Duration) (bool, error) {
	return c.conn.WaitForWakeup(ctx, timeout)
}

// Done returns a channel closed when the underlying connection's session
// ends; see Connection.Done.
func (c *Conversation) Done() <-chan struct{} { return c.conn.Done() }

// Err returns the error that ended the underlying connection's session;
// see Connection.Err.
func (c *Conversation) Err() error { return c.conn.Err() }

// Close closes the underlying connection.
func (c *Conversation) Close() error { return c.conn.Close() }
