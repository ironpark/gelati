package antigravity

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/coder/websocket"
	"github.com/ironpark/gelati/antigravity/internal/harness"
	"github.com/ironpark/gelati/antigravity/internal/wire"
)

// The test binary doubles as a fake localharness: when fakeHarnessEnv is set
// in its environment, TestMain runs runFakeHarness instead of the tests.
// Agent tests point Config.BinaryPath at os.Args[0].
const (
	fakeHarnessEnv = "GELATI_ANTIGRAVITY_FAKE_HARNESS"
	// fakeRecordEnv names a file the fake writes the HarnessConfig it
	// received to, as protobuf JSON.
	fakeRecordEnv = "GELATI_ANTIGRAVITY_FAKE_RECORD"
	// fakeLogEnv names a file the fake appends one line to per notable
	// input event (triggers, session end).
	fakeLogEnv  = "GELATI_ANTIGRAVITY_FAKE_LOG"
	fakeKey     = "fake-api-key"
	fakeCascade = "0123456789abcdef0123456789abcdef"
)

func TestMain(m *testing.M) {
	if os.Getenv(fakeHarnessEnv) != "" {
		runFakeHarness()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func runFakeHarness() {
	fail := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "fake harness: "+format+"\n", args...)
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
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fail("listen: %v", err)
	}
	out, _ := (&wire.OutputConfig{Port: new(int32(ln.Addr().(*net.TCPAddr).Port)), APIKey: new(fakeKey)}).MarshalBinary()
	os.Stdout.Write(append(binary.LittleEndian.AppendUint32(nil, uint32(len(out))), out...))
	go func() {
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	}()
	err = http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(harness.APIKeyHeader) != fakeKey {
			http.Error(w, "bad key", http.StatusUnauthorized)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		(&fakeSession{ws: c, responses: map[string]chan *wire.InputEvent{}}).serve()
	}))
	fail("serve: %v", err)
}

// fakeSession plays the harness side of one WebSocket session.
type fakeSession struct {
	ws    *websocket.Conn
	cfg   *wire.HarnessConfig
	seq   wire.Int64
	mu    sync.Mutex
	turns chan *wire.InputEvent
	halt  chan struct{}

	// responses routes replies (hook, tool and policy responses) to the
	// request waiting for them, by request or call ID.
	responses map[string]chan *wire.InputEvent
}

func (s *fakeSession) write(ev *wire.OutputEvent) {
	s.mu.Lock()
	s.seq++
	ev.SeqNum = new(s.seq)
	b, _ := wire.Marshal(ev)
	s.mu.Unlock()
	_ = s.ws.Write(context.Background(), websocket.MessageText, b)
}

func (s *fakeSession) expect(id string) chan *wire.InputEvent {
	ch := make(chan *wire.InputEvent, 1)
	s.mu.Lock()
	s.responses[id] = ch
	s.mu.Unlock()
	return ch
}

func (s *fakeSession) deliver(id string, ev *wire.InputEvent) {
	s.mu.Lock()
	ch := s.responses[id]
	delete(s.responses, id)
	s.mu.Unlock()
	if ch != nil {
		ch <- ev
	}
}

func (s *fakeSession) hookEnabled(h wire.LifecycleHook) bool {
	return slices.Contains(s.cfg.GetEnabledHooks(), h)
}

// callHook sends a CallHookRequest and waits for the response.
func (s *fakeSession) callHook(id string, typ wire.LifecycleHook, fill func(*wire.CallHookRequest)) *wire.CallHookResponse {
	ch := s.expect(id)
	req := &wire.CallHookRequest{RequestID: new(id), Type: new(typ), Name: new(string(typ))}
	if fill != nil {
		fill(req)
	}
	s.write(&wire.OutputEvent{CallHookRequest: req})
	return (<-ch).GetCallHookResponse()
}

func appendLog(line string) {
	if p := os.Getenv(fakeLogEnv); p != "" {
		f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprintln(f, line)
			f.Close()
		}
	}
}

func (s *fakeSession) serve() {
	ctx := context.Background()
	_, data, err := s.ws.Read(ctx)
	if err != nil {
		return
	}
	var init wire.InitializeConversationEvent
	if err := wire.Unmarshal(data, &init); err != nil {
		fmt.Fprintf(os.Stderr, "fake harness: bad init: %v\n", err)
		return
	}
	s.cfg = init.GetConfig()
	if p := os.Getenv(fakeRecordEnv); p != "" {
		b, _ := wire.Marshal(s.cfg)
		_ = os.WriteFile(p, b, 0o644)
	}
	s.write(&wire.OutputEvent{InitializeConversationResponse: &wire.InitializeConversationResponse{
		CascadeID:       new(fakeCascade),
		History:         []*wire.StepUpdate{{TrajectoryID: new(fakeCascade), StepIndex: new(uint32(0)), State: new(wire.StepUpdateStateDone), Source: new(wire.StepUpdateSourceUser), Text: new("earlier prompt")}},
		CumulativeUsage: &wire.UsageMetadata{TotalTokenCount: new(wire.Uint64(10))},
		SandboxStatus:   &wire.SandboxStatus{Available: new(true)},
	}})
	if s.hookEnabled(wire.LifecycleHookOnSessionStart) {
		go s.callHook("session-start", wire.LifecycleHookOnSessionStart, nil)
	}
	s.turns = make(chan *wire.InputEvent, 16)
	s.halt = make(chan struct{}, 1)
	go func() {
		for ev := range s.turns {
			s.runTurn(ev)
		}
	}()
	for {
		_, data, err := s.ws.Read(ctx)
		if err != nil {
			return
		}
		var ev wire.InputEvent
		if err := wire.Unmarshal(data, &ev); err != nil {
			fmt.Fprintf(os.Stderr, "fake harness: bad input: %v\n", err)
			return
		}
		switch {
		case ev.UserInput != nil:
			s.turns <- &ev
		case ev.AutomatedTrigger != nil:
			appendLog("trigger:" + ev.GetAutomatedTrigger())
		case ev.GetHaltRequest():
			s.halt <- struct{}{}
		case ev.CallHookResponse != nil:
			s.deliver(ev.CallHookResponse.GetRequestID(), &ev)
		case ev.ToolResponse != nil:
			s.deliver(ev.ToolResponse.GetID(), &ev)
		case ev.PolicyDecisionResponse != nil:
			s.deliver(ev.PolicyDecisionResponse.GetRequestID(), &ev)
		case ev.GetSessionEndRequest():
			appendLog("session_end")
			if s.hookEnabled(wire.LifecycleHookOnSessionEnd) {
				go func() {
					s.callHook("session-end", wire.LifecycleHookOnSessionEnd, nil)
					s.write(&wire.OutputEvent{SessionEndResponse: new(true)})
				}()
			} else {
				s.write(&wire.OutputEvent{SessionEndResponse: new(true)})
			}
		}
	}
}

func (s *fakeSession) state(st wire.TrajectoryStateUpdateState, errMsg string) {
	tsu := &wire.TrajectoryStateUpdate{TrajectoryID: new(fakeCascade), State: new(st)}
	if errMsg != "" {
		tsu.Error = new(errMsg)
	}
	s.write(&wire.OutputEvent{TrajectoryStateUpdate: tsu})
}

func (s *fakeSession) step(idx uint32, su *wire.StepUpdate) {
	su.CascadeID = new(fakeCascade)
	su.TrajectoryID = new(fakeCascade)
	su.StepIndex = new(idx)
	s.write(&wire.OutputEvent{StepUpdate: su})
}

func (s *fakeSession) say(idx uint32, text string) {
	s.step(idx, &wire.StepUpdate{
		State: new(wire.StepUpdateStateDone), Source: new(wire.StepUpdateSourceModel),
		Target: new(wire.StepUpdateTargetUser), Text: new(text), TextDelta: new(text),
	})
}

// runTurn plays one scripted turn, chosen by the prompt's first word:
//
//	hello            text in two deltas, a thought and a usage update
//	tool NAME JSON   call custom tool NAME (after the PreTool hook) and report its result
//	builtin JSON     ask the PreTool hook about run_command, then report
//	policy RULE      ask the SDK to evaluate dynamic policy rule RULE
//	fail             a fatal HTTP 400 system error
//	slow             work until halted
//	structured JSON  finish with JSON as structured output
func (s *fakeSession) runTurn(ev *wire.InputEvent) {
	prompt := ev.GetUserInput().GetParts()[0].GetText()
	cmd, rest, _ := strings.Cut(prompt, " ")
	s.state(wire.TrajectoryStateUpdateStateRunning, "")
	if s.hookEnabled(wire.LifecycleHookPreTurn) {
		resp := s.callHook("pre-turn", wire.LifecycleHookPreTurn, func(r *wire.CallHookRequest) {
			r.PreTurnArgs = &wire.PreTurnArgs{UserInput: ev.GetUserInput()}
		})
		if resp.GetPreTurnResult().GetDecision() == wire.PreTurnResultDecisionDeny {
			s.state(wire.TrajectoryStateUpdateStateCancelled, resp.GetPreTurnResult().GetReason())
			return
		}
	}
	s.step(0, &wire.StepUpdate{State: new(wire.StepUpdateStateDone), Source: new(wire.StepUpdateSourceUser), Target: new(wire.StepUpdateTargetModel), Text: new(prompt)})
	switch cmd {
	case "hello":
		model := func(state wire.StepUpdateState, text, delta, thinkingDelta string) *wire.StepUpdate {
			return &wire.StepUpdate{State: new(state), Source: new(wire.StepUpdateSourceModel), Target: new(wire.StepUpdateTargetUser),
				Text: new(text), TextDelta: new(delta), ThinkingDelta: new(thinkingDelta)}
		}
		s.step(1, model(wire.StepUpdateStateActive, "", "", "thinking"))
		s.step(1, model(wire.StepUpdateStateActive, "Hello", "Hello", ""))
		s.step(1, model(wire.StepUpdateStateDone, "Hello there!", " there!", ""))
		s.write(&wire.OutputEvent{UsageUpdate: &wire.UsageUpdate{Total: &wire.UsageMetadata{
			PromptTokenCount: new(wire.Uint64(30)), CandidatesTokenCount: new(wire.Uint64(5)), TotalTokenCount: new(wire.Uint64(45)),
		}}})
	case "tool":
		name, args, _ := strings.Cut(rest, " ")
		if s.hookEnabled(wire.LifecycleHookPreTool) {
			resp := s.callHook("pre-tool", wire.LifecycleHookPreTool, func(r *wire.CallHookRequest) {
				r.PreToolArgs = &wire.PreToolArgs{ToolName: new(name), ArgumentsJSON: new(args), CallID: new("call-1")}
			})
			if res := resp.GetPreToolResult(); res.GetDecision() == wire.PreToolResultDecisionDeny {
				s.say(2, "denied: "+res.GetReason())
				break
			} else if res.ModifiedArgs != nil {
				b, _ := json.Marshal(res.ModifiedArgs.AsMap())
				args = string(b)
			}
		}
		ch := s.expect("call-1")
		s.write(&wire.OutputEvent{ToolCall: &wire.ToolCall{ID: new("call-1"), Name: new(name), ArgumentsJSON: new(args), TrajectoryID: new(fakeCascade)}})
		resp := (<-ch).GetToolResponse()
		result := resp.GetResponseJSON()
		if resp.ErrorMessage != nil {
			result = "error " + resp.GetErrorMessage()
			if s.hookEnabled(wire.LifecycleHookOnToolError) {
				hr := s.callHook("tool-error", wire.LifecycleHookOnToolError, func(r *wire.CallHookRequest) {
					r.OnToolErrorArgs = &wire.OnToolErrorArgs{ToolName: new(name), ErrorMessage: new(resp.GetErrorMessage()), CallID: new("call-1")}
				})
				if m := hr.GetOnToolErrorResult().GetCustomErrorMessage(); m != "" {
					result = "error " + m
				}
			}
		}
		if s.hookEnabled(wire.LifecycleHookPostTool) {
			s.callHook("post-tool", wire.LifecycleHookPostTool, func(r *wire.CallHookRequest) {
				r.PostToolArgs = &wire.PostToolArgs{ToolName: new(name), Result: new(resp.GetResponseJSON()), Error: resp.ErrorMessage, CallID: new("call-1")}
			})
		}
		s.say(2, "tool said: "+result)
	case "policy":
		ch := s.expect("policy-1")
		s.write(&wire.OutputEvent{PolicyDecisionRequest: &wire.PolicyDecisionRequest{
			RequestID: new("policy-1"), RuleID: new(rest),
			ToolArgs: &wire.PreToolArgs{ToolName: new("run_command"), ArgumentsJSON: new(`{"CommandLine": "rm -rf /"}`)},
		}})
		resp := (<-ch).GetPolicyDecisionResponse()
		s.say(2, fmt.Sprintf("policy %s %s", resp.GetOutcome(), resp.GetDenyReason()))
	case "fail":
		s.step(1, &wire.StepUpdate{
			State: new(wire.StepUpdateStateError), Source: new(wire.StepUpdateSourceSystem),
			ErrorMessage: new("Agent execution terminated due to error."),
			Error:        &wire.ActionError{ErrorMessage: new("API key not valid."), HTTPCode: new(uint32(400))},
		})
		s.state(wire.TrajectoryStateUpdateStateFullyIdle, "Error 400, Message: API key not valid.")
		return
	case "slow":
		s.step(1, &wire.StepUpdate{State: new(wire.StepUpdateStateActive), Source: new(wire.StepUpdateSourceModel), Target: new(wire.StepUpdateTargetUser), Text: new("working"), TextDelta: new("working")})
		<-s.halt
	case "structured":
		s.step(1, &wire.StepUpdate{State: new(wire.StepUpdateStateDone), Source: new(wire.StepUpdateSourceModel), Finish: &wire.ActionFinish{OutputString: new(rest)}})
	default:
		s.say(1, "unknown command: "+prompt)
	}
	if s.hookEnabled(wire.LifecycleHookPostTurn) {
		s.callHook("post-turn", wire.LifecycleHookPostTurn, func(r *wire.CallHookRequest) {
			r.PostTurnArgs = &wire.PostTurnArgs{ResponseText: new("done")}
		})
	}
	s.state(wire.TrajectoryStateUpdateStateFullyIdle, "")
}
