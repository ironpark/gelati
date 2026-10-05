package agy

import (
	"context"
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

	"github.com/ironpark/gelati/agy/internal/harness"
	"github.com/ironpark/gelati/agy/internal/wire"
)

// The test binary doubles as a fake localharness: when fakeHarnessEnv is set
// in its environment, TestMain runs runFakeHarness instead of the tests.
// Agent tests point Config.CLIPath at os.Args[0].
const (
	fakeHarnessEnv = "GELATI_AGY_FAKE_HARNESS"
	// fakeRecordEnv names a file the fake writes the HarnessConfig it
	// received to, as protobuf JSON.
	fakeRecordEnv = "GELATI_AGY_FAKE_RECORD"
	// fakeLogEnv names a file the fake appends one line to per notable
	// input event (triggers, session end).
	fakeLogEnv  = "GELATI_AGY_FAKE_LOG"
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
	if err := harness.ReadFrame(os.Stdin, &wire.InputConfig{}); err != nil {
		fail("read input config: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fail("listen: %v", err)
	}
	_ = harness.WriteFrame(os.Stdout, wire.OutputConfig_builder{Port: new(int32(ln.Addr().(*net.TCPAddr).Port)), ApiKey: new(fakeKey)}.Build())
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
	seq   int64
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
	ev.SetSeqNum(s.seq)
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
	req := wire.CallHookRequest_builder{RequestId: new(id), Type: new(typ), Name: new(string(typ))}.Build()
	if fill != nil {
		fill(req)
	}
	s.write(wire.OutputEvent_builder{CallHookRequest: req}.Build())
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
	s.write(wire.OutputEvent_builder{InitializeConversationResponse: wire.InitializeConversationResponse_builder{
		CascadeId:       new(fakeCascade),
		History:         []*wire.StepUpdate{wire.StepUpdate_builder{TrajectoryId: new(fakeCascade), StepIndex: new(uint32(0)), State: new(wire.StepUpdate_STATE_DONE), Source: new(wire.StepUpdate_SOURCE_USER), Text: new("earlier prompt")}.Build()},
		CumulativeUsage: wire.UsageMetadata_builder{TotalTokenCount: new(uint64(10))}.Build(),
		SandboxStatus:   wire.SandboxStatus_builder{Available: new(true)}.Build(),
	}.Build()}.Build())
	if s.hookEnabled(wire.LifecycleHook_LIFECYCLE_HOOK_ON_SESSION_START) {
		go s.callHook("session-start", wire.LifecycleHook_LIFECYCLE_HOOK_ON_SESSION_START, nil)
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
		case ev.GetUserInput() != nil:
			s.turns <- &ev
		case ev.HasAutomatedTrigger():
			appendLog("trigger:" + ev.GetAutomatedTrigger())
		case ev.GetHaltRequest():
			s.halt <- struct{}{}
		case ev.GetCallHookResponse() != nil:
			s.deliver(ev.GetCallHookResponse().GetRequestId(), &ev)
		case ev.GetToolResponse() != nil:
			s.deliver(ev.GetToolResponse().GetId(), &ev)
		case ev.GetPolicyDecisionResponse() != nil:
			s.deliver(ev.GetPolicyDecisionResponse().GetRequestId(), &ev)
		case ev.GetSessionEndRequest():
			appendLog("session_end")
			if s.hookEnabled(wire.LifecycleHook_LIFECYCLE_HOOK_ON_SESSION_END) {
				go func() {
					s.callHook("session-end", wire.LifecycleHook_LIFECYCLE_HOOK_ON_SESSION_END, nil)
					s.write(wire.OutputEvent_builder{SessionEndResponse: new(true)}.Build())
				}()
			} else {
				s.write(wire.OutputEvent_builder{SessionEndResponse: new(true)}.Build())
			}
		}
	}
}

func (s *fakeSession) state(st wire.TrajectoryStateUpdate_State, errMsg string) {
	tsu := wire.TrajectoryStateUpdate_builder{TrajectoryId: new(fakeCascade), State: new(st)}.Build()
	if errMsg != "" {
		tsu.SetError(errMsg)
	}
	s.write(wire.OutputEvent_builder{TrajectoryStateUpdate: tsu}.Build())
}

func (s *fakeSession) step(idx uint32, su *wire.StepUpdate) {
	su.SetCascadeId(fakeCascade)
	su.SetTrajectoryId(fakeCascade)
	su.SetStepIndex(idx)
	s.write(wire.OutputEvent_builder{StepUpdate: su}.Build())
}

func (s *fakeSession) say(idx uint32, text string) {
	s.step(idx, wire.StepUpdate_builder{
		State: new(wire.StepUpdate_STATE_DONE), Source: new(wire.StepUpdate_SOURCE_MODEL),
		Target: new(wire.StepUpdate_TARGET_USER), Text: new(text), TextDelta: new(text),
	}.Build())
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
//	crash            write to stderr and exit the process
func (s *fakeSession) runTurn(ev *wire.InputEvent) {
	prompt := ev.GetUserInput().GetParts()[0].GetText()
	cmd, rest, _ := strings.Cut(prompt, " ")
	s.state(wire.TrajectoryStateUpdate_STATE_RUNNING, "")
	if s.hookEnabled(wire.LifecycleHook_LIFECYCLE_HOOK_PRE_TURN) {
		resp := s.callHook("pre-turn", wire.LifecycleHook_LIFECYCLE_HOOK_PRE_TURN, func(r *wire.CallHookRequest) {
			r.SetPreTurnArgs(wire.PreTurnArgs_builder{UserInput: ev.GetUserInput()}.Build())
		})
		if resp.GetPreTurnResult().GetDecision() == wire.PreTurnResult_DENY {
			s.state(wire.TrajectoryStateUpdate_STATE_CANCELLED, resp.GetPreTurnResult().GetReason())
			return
		}
	}
	s.step(0, wire.StepUpdate_builder{State: new(wire.StepUpdate_STATE_DONE), Source: new(wire.StepUpdate_SOURCE_USER), Target: new(wire.StepUpdate_TARGET_MODEL), Text: new(prompt)}.Build())
	switch cmd {
	case "hello":
		model := func(state wire.StepUpdate_State, text, delta, thinkingDelta string) *wire.StepUpdate {
			return wire.StepUpdate_builder{State: new(state), Source: new(wire.StepUpdate_SOURCE_MODEL), Target: new(wire.StepUpdate_TARGET_USER),
				Text: new(text), TextDelta: new(delta), ThinkingDelta: new(thinkingDelta)}.Build()
		}
		s.step(1, model(wire.StepUpdate_STATE_ACTIVE, "", "", "thinking"))
		s.step(1, model(wire.StepUpdate_STATE_ACTIVE, "Hello", "Hello", ""))
		s.step(1, model(wire.StepUpdate_STATE_DONE, "Hello there!", " there!", ""))
		s.write(wire.OutputEvent_builder{UsageUpdate: wire.UsageUpdate_builder{Total: wire.UsageMetadata_builder{
			PromptTokenCount: new(uint64(30)), CandidatesTokenCount: new(uint64(5)), TotalTokenCount: new(uint64(45)),
		}.Build()}.Build()}.Build())
	case "tool":
		name, args, _ := strings.Cut(rest, " ")
		if s.hookEnabled(wire.LifecycleHook_LIFECYCLE_HOOK_PRE_TOOL) {
			resp := s.callHook("pre-tool", wire.LifecycleHook_LIFECYCLE_HOOK_PRE_TOOL, func(r *wire.CallHookRequest) {
				r.SetPreToolArgs(wire.PreToolArgs_builder{ToolName: new(name), ArgumentsJson: new(args), CallId: new("call-1")}.Build())
			})
			if res := resp.GetPreToolResult(); res.GetDecision() == wire.PreToolResult_DENY {
				s.say(2, "denied: "+res.GetReason())
				break
			} else if res.GetModifiedArgs() != nil {
				b, _ := json.Marshal(res.GetModifiedArgs().AsMap())
				args = string(b)
			}
		}
		ch := s.expect("call-1")
		s.write(wire.OutputEvent_builder{ToolCall: wire.ToolCall_builder{Id: new("call-1"), Name: new(name), ArgumentsJson: new(args), TrajectoryId: new(fakeCascade)}.Build()}.Build())
		resp := (<-ch).GetToolResponse()
		result := resp.GetResponseJson()
		if resp.HasErrorMessage() {
			result = "error " + resp.GetErrorMessage()
			if s.hookEnabled(wire.LifecycleHook_LIFECYCLE_HOOK_ON_TOOL_ERROR) {
				hr := s.callHook("tool-error", wire.LifecycleHook_LIFECYCLE_HOOK_ON_TOOL_ERROR, func(r *wire.CallHookRequest) {
					r.SetOnToolErrorArgs(wire.OnToolErrorArgs_builder{ToolName: new(name), ErrorMessage: new(resp.GetErrorMessage()), CallId: new("call-1")}.Build())
				})
				if m := hr.GetOnToolErrorResult().GetCustomErrorMessage(); m != "" {
					result = "error " + m
				}
			}
		}
		if s.hookEnabled(wire.LifecycleHook_LIFECYCLE_HOOK_POST_TOOL) {
			s.callHook("post-tool", wire.LifecycleHook_LIFECYCLE_HOOK_POST_TOOL, func(r *wire.CallHookRequest) {
				args := wire.PostToolArgs_builder{ToolName: new(name), Result: new(resp.GetResponseJson()), CallId: new("call-1")}.Build()
				if resp.HasErrorMessage() {
					args.SetError(resp.GetErrorMessage())
				}
				r.SetPostToolArgs(args)
			})
		}
		s.say(2, "tool said: "+result)
	case "policy":
		ch := s.expect("policy-1")
		s.write(wire.OutputEvent_builder{PolicyDecisionRequest: wire.PolicyDecisionRequest_builder{
			RequestId: new("policy-1"), RuleId: new(rest),
			ToolArgs: wire.PreToolArgs_builder{ToolName: new("run_command"), ArgumentsJson: new(`{"CommandLine": "rm -rf /"}`)}.Build(),
		}.Build()}.Build())
		resp := (<-ch).GetPolicyDecisionResponse()
		s.say(2, fmt.Sprintf("policy %s %s", resp.GetOutcome(), resp.GetDenyReason()))
	case "fail":
		s.step(1, wire.StepUpdate_builder{
			State: new(wire.StepUpdate_STATE_ERROR), Source: new(wire.StepUpdate_SOURCE_SYSTEM),
			ErrorMessage: new("Agent execution terminated due to error."),
			Error:        wire.ActionError_builder{ErrorMessage: new("API key not valid."), HttpCode: new(uint32(400))}.Build(),
		}.Build())
		s.state(wire.TrajectoryStateUpdate_STATE_FULLY_IDLE, "Error 400, Message: API key not valid.")
		return
	case "slow":
		s.step(1, wire.StepUpdate_builder{State: new(wire.StepUpdate_STATE_ACTIVE), Source: new(wire.StepUpdate_SOURCE_MODEL), Target: new(wire.StepUpdate_TARGET_USER), Text: new("working"), TextDelta: new("working")}.Build())
		<-s.halt
	case "crash":
		fmt.Fprintln(os.Stderr, "fake harness: crashing on purpose")
		os.Exit(3)
	case "structured":
		s.step(1, wire.StepUpdate_builder{State: new(wire.StepUpdate_STATE_DONE), Source: new(wire.StepUpdate_SOURCE_MODEL), Finish: wire.ActionFinish_builder{OutputString: new(rest)}.Build()}.Build())
	default:
		s.say(1, "unknown command: "+prompt)
	}
	if s.hookEnabled(wire.LifecycleHook_LIFECYCLE_HOOK_POST_TURN) {
		s.callHook("post-turn", wire.LifecycleHook_LIFECYCLE_HOOK_POST_TURN, func(r *wire.CallHookRequest) {
			r.SetPostTurnArgs(wire.PostTurnArgs_builder{ResponseText: new("done")}.Build())
		})
	}
	s.state(wire.TrajectoryStateUpdate_STATE_FULLY_IDLE, "")
}
