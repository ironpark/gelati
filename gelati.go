// Package gelati drives coding agents (Claude Code, Codex and Antigravity)
// through one provider-neutral API. Each agent also has a package of its own
// (claude, codex and agy) with its full native API; this package covers what
// they share — running turns, streaming their text and tool calls, tool
// approval, custom tools, image input and structured output — and hands
// over to the native API for the rest.
//
// A Provider, built from a native package's options, opens an Agent:
//
//	a, err := gelati.Open(ctx, claude.Provider(claude.Options{}), gelati.Config{
//		Dir: "/repo",
//	})
//	if err != nil {
//		return err
//	}
//	defer a.Close()
//	res, err := a.Run(ctx, gelati.Text("Run the tests"))
//
// The native options a Provider is built with configure everything the
// provider offers; Config sets the common settings over them. Native returns
// the provider's own handle, and Event.Raw and Result.Raw the provider's own
// events and results.
//
// The package imports no provider, so a program links only the providers it
// uses.
package gelati

import (
	"context"
	"errors"
	"fmt"
	"iter"

	"github.com/ironpark/gelati/internal/lifecycle"
)

// Open starts a session with p, configured by cfg over the options p was
// built with. ctx bounds the startup only: the session lasts until Close or
// until the provider's process exits.
func Open(ctx context.Context, p Provider, cfg Config) (*Agent, error) {
	if p == nil {
		return nil, errors.New("gelati: Open needs a provider")
	}
	if err := checkTools(cfg.Tools); err != nil {
		return nil, err
	}
	conn, err := p.Open(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &Agent{provider: p.Name(), conn: conn}, nil
}

// checkTools reports a tool without a name or Run, or two tools of one name.
func checkTools(tools []Tool) error {
	seen := make(map[string]bool, len(tools))
	for _, t := range tools {
		switch {
		case t.Name == "":
			return errors.New("gelati: a tool has no name")
		case t.Run == nil:
			return fmt.Errorf("gelati: tool %q has no Run", t.Name)
		case seen[t.Name]:
			return fmt.Errorf("gelati: two tools are named %q", t.Name)
		}
		seen[t.Name] = true
	}
	return nil
}

// Run opens a session with p, runs prompt as one turn and closes the
// session. ctx bounds the whole call.
func Run(ctx context.Context, p Provider, prompt string, cfg Config) (*Result, error) {
	a, err := Open(ctx, p, cfg)
	if err != nil {
		return nil, err
	}
	res, err := a.Run(ctx, Text(prompt))
	if closeErr := a.Close(); err == nil {
		err = closeErr
	}
	return res, err
}

// Agent is a session with one provider. It is safe for concurrent use as far
// as the provider's session is; turns are read one at a time.
type Agent struct {
	provider string
	conn     Conn
}

// Send starts a turn and returns its stream.
func (a *Agent) Send(ctx context.Context, input ...Input) (*Turn, error) {
	if len(input) == 0 {
		return nil, errors.New("gelati: Send needs input")
	}
	tc, err := a.conn.Send(ctx, input)
	if err != nil {
		return nil, err
	}
	return &Turn{tc: tc}, nil
}

// Run starts a turn and waits for its result: Send followed by Turn.Result.
// A failed turn returns its result, when there is one, together with the
// error. If ctx ends before the turn does, Run cancels the turn and returns
// ctx's error.
func (a *Agent) Run(ctx context.Context, input ...Input) (*Result, error) {
	t, err := a.Send(ctx, input...)
	if err != nil {
		return nil, err
	}
	res, err := t.Result(ctx)
	if lifecycle.CancelIfDone(ctx, err, t.Cancel) {
		_ = t.Close()
	}
	return res, err
}

// ID returns the provider's conversation id: the claude session, the codex
// thread or the agy conversation. It may be empty until the provider reports
// one.
func (a *Agent) ID() string { return a.conn.ID() }

// Provider returns the name of the agent's provider.
func (a *Agent) Provider() string { return a.provider }

// Native returns the provider's own session handle, for what this package
// does not cover: a *claude.Client, a *codex.Thread or an *agy.Agent.
func (a *Agent) Native() any { return a.conn.Native() }

// Done is closed once the session has ended.
func (a *Agent) Done() <-chan struct{} { return a.conn.Done() }

// Err reports why the session ended: nil while it runs or after Close.
func (a *Agent) Err() error { return a.conn.Err() }

// Close ends the session. It is idempotent.
func (a *Agent) Close() error { return a.conn.Close() }

// Turn is one turn of an Agent. Read it with Events or Text, or wait for it
// with Result. The rules are those of every gelati turn stream:
//
//   - Close stops reading and never interrupts the turn. Close on a turn
//     that already ended changes nothing, and Result still returns its
//     result.
//   - Cancel interrupts the turn while it runs, also after Close. Once the
//     turn has ended it does nothing.
//   - Result returns the turn's result whenever there is one, together with
//     the error when the turn failed.
type Turn struct {
	tc TurnConn
}

// Events yields the turn's events as they arrive. A failure is the final
// item.
func (t *Turn) Events(ctx context.Context) iter.Seq2[Event, error] {
	return t.tc.Events(ctx)
}

// Text yields the assistant's answer as it arrives: the Text of each
// EventTextDelta. It reads the same stream as Events, so use one of the two.
// When the turn fails, the final item is the error Result returns.
func (t *Turn) Text(ctx context.Context) iter.Seq2[string, error] {
	return func(yield func(string, error) bool) {
		for ev, err := range t.tc.Events(ctx) {
			if err != nil {
				yield("", err)
				return
			}
			if ev.Kind == EventTextDelta && ev.Text != "" && !yield(ev.Text, nil) {
				return
			}
		}
		if _, err := t.tc.Result(ctx); err != nil {
			yield("", err)
		}
	}
}

// Result waits for the turn to end, reading what Events did not, and returns
// its result. When ctx ends first the turn keeps running; Agent.Run cancels
// it instead.
func (t *Turn) Result(ctx context.Context) (*Result, error) { return t.tc.Result(ctx) }

// Cancel interrupts the turn.
func (t *Turn) Cancel(ctx context.Context) error { return t.tc.Cancel(ctx) }

// Close stops reading the turn without interrupting it.
func (t *Turn) Close() error { return t.tc.Close() }
