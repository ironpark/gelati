package agy

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ironpark/gelati"
	"github.com/ironpark/gelati/agy/internal/wire"
)

func TestProviderOptions(t *testing.T) {
	base := Options{
		Model:              "base-model",
		Workspaces:         []string{"/a", "/b"},
		SystemInstructions: TextSystemInstructions("Base."),
		Env:                map[string]string{"KEEP": "1", "OVER": "base"},
		CLIPath:            "base-cli",
	}
	logger := quietLogger()
	schema := map[string]any{"type": "object"}
	p := Provider(base).(provider)
	if p.Name() != "agy" {
		t.Fatalf("name %q", p.Name())
	}
	got := mustOptions(t, p, gelati.Config{
		Model:        "m",
		Dir:          "/b",
		Instructions: "Extra.",
		OutputSchema: schema,
		CLIPath:      "cli",
		Env:          map[string]string{"OVER": "cfg", "NEW": "2"},
		Logger:       logger,
	})
	if got.Model != "m" || got.CLIPath != "cli" || got.Logger != logger {
		t.Fatalf("model %q cli %q logger %v", got.Model, got.CLIPath, got.Logger)
	}
	if !reflect.DeepEqual(got.Workspaces, []string{"/b", "/a"}) {
		t.Fatalf("workspaces %q", got.Workspaces)
	}
	wantSI := TemplatedSystemInstructions{Sections: []SystemInstructionSection{{Content: "Base."}, {Content: "Extra."}}}
	if !reflect.DeepEqual(got.SystemInstructions, wantSI) {
		t.Fatalf("system instructions %#v", got.SystemInstructions)
	}
	if !reflect.DeepEqual(got.ResponseSchema, schema) {
		t.Fatalf("response schema %#v", got.ResponseSchema)
	}
	if want := map[string]string{"KEEP": "1", "OVER": "cfg", "NEW": "2"}; !reflect.DeepEqual(got.Env, want) {
		t.Fatalf("env %v", got.Env)
	}
	// base is untouched.
	if !reflect.DeepEqual(base.Workspaces, []string{"/a", "/b"}) || !reflect.DeepEqual(base.Env, map[string]string{"KEEP": "1", "OVER": "base"}) ||
		base.SystemInstructions != TextSystemInstructions("Base.") || base.Model != "base-model" || base.ResponseSchema != nil {
		t.Fatalf("base modified: %+v", base)
	}

	// The zero Config keeps base.
	if got := mustOptions(t, p, gelati.Config{}); !reflect.DeepEqual(got, base) {
		t.Fatalf("zero config changed options: %+v", got)
	}
	// Dir with no base workspaces, Env with no base Env.
	got = mustOptions(t, Provider(Options{}).(provider), gelati.Config{Dir: "/w", Env: map[string]string{"K": "v"}})
	if !reflect.DeepEqual(got.Workspaces, []string{"/w"}) || got.Env["K"] != "v" {
		t.Fatalf("workspaces %q env %v", got.Workspaces, got.Env)
	}
}

func mustOptions(t *testing.T, p provider, cfg gelati.Config) Options {
	t.Helper()
	opts, err := p.options(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return opts
}

func TestAppendInstructions(t *testing.T) {
	sections := []SystemInstructionSection{{Title: "t", Content: "one"}}
	templated := TemplatedSystemInstructions{Identity: "id", Sections: sections}
	for _, tc := range []struct {
		name string
		in   SystemInstructions
		want SystemInstructions
	}{
		{"nil", nil, TextSystemInstructions("x")},
		{"empty text", TextSystemInstructions(""), TextSystemInstructions("x")},
		{"text", TextSystemInstructions("a"), TemplatedSystemInstructions{Sections: []SystemInstructionSection{{Content: "a"}, {Content: "x"}}}},
		{"templated", templated, TemplatedSystemInstructions{Identity: "id", Sections: []SystemInstructionSection{{Title: "t", Content: "one"}, {Content: "x"}}}},
		{"templated pointer", &templated, TemplatedSystemInstructions{Identity: "id", Sections: []SystemInstructionSection{{Title: "t", Content: "one"}, {Content: "x"}}}},
		{"nil templated pointer", (*TemplatedSystemInstructions)(nil), TextSystemInstructions("x")},
		{"custom", CustomSystemInstructions{Text: "all"}, CustomSystemInstructions{Text: "all\n\nx"}},
		{"custom pointer", &CustomSystemInstructions{Text: "all"}, CustomSystemInstructions{Text: "all\n\nx"}},
		{"empty custom", CustomSystemInstructions{}, CustomSystemInstructions{Text: "x"}},
	} {
		if got := appendInstructions(tc.in, "x"); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %#v, want %#v", tc.name, got, tc.want)
		}
	}
	if !reflect.DeepEqual(templated.Sections, sections) || len(templated.Sections) != 1 {
		t.Fatalf("templated sections modified: %+v", templated.Sections)
	}
}

func TestProviderTurnEvents(t *testing.T) {
	call := &ToolCall{ID: "c1", Name: "fn", Args: map[string]any{"a": 1.0}}
	chunks := []Chunk{&ThoughtChunk{Text: "hm"}, &TextChunk{Text: "Hi"}, call}
	boom := errors.New("boom")
	i := 0
	turn := &providerTurn{stream: newTurnStream(func(context.Context) (Chunk, bool, error) {
		if i == len(chunks) {
			return nil, false, boom
		}
		i++
		return chunks[i-1], true, nil
	})}
	var got []gelati.Event
	var gotErr error
	for ev, err := range turn.Events(t.Context()) {
		if err != nil {
			gotErr = err
			break
		}
		got = append(got, ev)
	}
	want := []gelati.Event{
		{Kind: gelati.EventThoughtDelta, Text: "hm", Raw: chunks[0]},
		{Kind: gelati.EventTextDelta, Text: "Hi", Raw: chunks[1]},
		{Kind: gelati.EventToolCall, Tool: &gelati.ToolCall{ID: "c1", Name: "fn", Input: map[string]any{"a": 1.0}}, Raw: call},
	}
	if !reflect.DeepEqual(got, want) || gotErr != boom {
		t.Fatalf("events %+v err %v", got, gotErr)
	}
	// A failed turn returns its result together with the error.
	res, err := turn.Result(t.Context())
	if err != boom || res == nil || res.Text != "Hi" {
		t.Fatalf("Result %+v %v", res, err)
	}
	if r, ok := res.Raw.(*TurnResult); !ok || len(r.Chunks) != 3 {
		t.Fatalf("raw %#v", res.Raw)
	}
	if ev := chunkEvent(nil); ev.Kind != gelati.EventOther {
		t.Fatalf("unknown chunk %+v", ev)
	}
}

func TestProviderTurnResult(t *testing.T) {
	n := func(v int64) *int64 { return &v }
	r := &TurnResult{
		Chunks:           []Chunk{&TextChunk{Text: "a"}, &ThoughtChunk{Text: "t"}, &TextChunk{Text: "b"}},
		StructuredOutput: map[string]any{"b": 1.0, "a": "x"},
		Usage: &UsageMetadata{
			PromptTokenCount: n(100), CachedContentTokenCount: n(40),
			CandidatesTokenCount: n(7), ThoughtsTokenCount: n(3), TotalTokenCount: n(110),
		},
	}
	res, err := turnResult(r)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "ab" || res.Raw != r || res.CostUSD != nil {
		t.Fatalf("result %+v", res)
	}
	if want := (gelati.Usage{InputTokens: 100, CachedInputTokens: 40, OutputTokens: 10}); res.Usage != want {
		t.Fatalf("usage %+v", res.Usage)
	}
	if string(res.StructuredOutput) != `{"a":"x","b":1}` {
		t.Fatalf("structured output %s", res.StructuredOutput)
	}

	res, err = turnResult(&TurnResult{})
	if err != nil || res.StructuredOutput != nil || res.Usage != (gelati.Usage{}) {
		t.Fatalf("empty result %+v %v", res, err)
	}
	res, err = turnResult(&TurnResult{StructuredOutput: make(chan int)})
	if err == nil || res == nil {
		t.Fatalf("unencodable structured output: %+v %v", res, err)
	}
}

// otherInput is an Input kind agy does not support.
type otherInput struct{ gelati.TextInput }

func TestProviderUnsupportedInput(t *testing.T) {
	c := &providerConn{}
	_, err := c.Send(t.Context(), []gelati.Input{gelati.Text("hi"), otherInput{}})
	if !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("Send = %v", err)
	}
}

func TestProviderEndToEnd(t *testing.T) {
	type answer struct {
		Total float64 `json:"total"`
	}
	echo := NewTool("echo", "Echoes a message.", func(_ context.Context, _ *ToolContext, in struct {
		Message string `json:"message"`
	}) (string, error) {
		return in.Message, nil
	})
	base, baseRecord, _ := fakeAgentConfig(t)
	base.Tools = []*Tool{echo}
	base.SystemInstructions = TextSystemInstructions("Base.")
	dir := t.TempDir()
	record := filepath.Join(t.TempDir(), "record.json")

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	shout := gelati.NewTool("shout", "Shouts a message.", func(_ context.Context, in struct {
		Message string `json:"message"`
	}) (string, error) {
		return strings.ToUpper(in.Message), nil
	})
	var mu sync.Mutex
	var approved []gelati.ToolRequest
	a, err := gelati.Open(ctx, Provider(base), gelati.Config{
		Model:        "gemini-test",
		Dir:          dir,
		Instructions: "Be brief.",
		OutputSchema: gelati.SchemaFor[answer](),
		Env:          map[string]string{fakeRecordEnv: record},
		Tools:        []gelati.Tool{shout},
		Approve: func(_ context.Context, req gelati.ToolRequest) (gelati.Decision, error) {
			mu.Lock()
			defer mu.Unlock()
			approved = append(approved, req)
			return gelati.Allow(), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if a.Provider() != "agy" {
		t.Fatalf("provider %q", a.Provider())
	}
	native, ok := a.Native().(*Agent)
	if !ok {
		t.Fatalf("native %T", a.Native())
	}

	res, err := a.Run(ctx, gelati.Text("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "Hello there!" || res.Usage != (gelati.Usage{InputTokens: 30, OutputTokens: 5}) {
		t.Fatalf("result text %q usage %+v", res.Text, res.Usage)
	}
	if _, ok := res.Raw.(*TurnResult); !ok {
		t.Fatalf("raw %T", res.Raw)
	}
	if a.ID() != fakeCascade || native.ConversationID() != fakeCascade {
		t.Fatalf("id %q", a.ID())
	}

	// cfg.Env won over base's record path.
	hc := readRecord(t, record)
	if _, err := os.Stat(baseRecord); err == nil {
		t.Fatal("base record path used")
	}
	if m := hc.GetModels(); len(m) == 0 || m[0].GetName() != "gemini-test" {
		t.Fatalf("models %+v", m)
	}
	wantDirs, err := Options{Workspaces: []string{dir}}.ResolvedWorkspaces()
	if err != nil {
		t.Fatal(err)
	}
	if ws := hc.GetWorkspaces(); len(ws) != 2 || ws[0].GetFilesystemWorkspace().GetDirectory() != filepath.ToSlash(wantDirs[0]) {
		t.Fatalf("workspaces %+v", ws)
	}
	secs := hc.GetSystemInstructions().GetAppended().GetAppendedSections()
	if len(secs) != 2 || secs[0].GetContent() != "Base." || secs[1].GetContent() != "Be brief." {
		t.Fatalf("system instructions %+v", hc.GetSystemInstructions())
	}
	if s := hc.GetFinishToolSchemaJson(); !strings.Contains(s, `"total"`) {
		t.Fatalf("finish schema %q", s)
	}

	// Tool calls stream as events.
	turn, err := a.Send(ctx, gelati.Text(`tool echo {"message": "hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	var kinds []gelati.EventKind
	var call *gelati.ToolCall
	for ev, err := range turn.Events(ctx) {
		if err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, ev.Kind)
		if ev.Kind == gelati.EventToolCall {
			call = ev.Tool
			if _, ok := ev.Raw.(*ToolCall); !ok {
				t.Fatalf("raw %T", ev.Raw)
			}
		}
	}
	if call == nil || call.ID != "call-1" || call.Name != "echo" || call.Input["message"] != "hi" {
		t.Fatalf("events %v call %+v", kinds, call)
	}
	if res, err := turn.Result(ctx); err != nil || res.Text != `tool said: {"result":"hi"}` || res.StructuredOutput != nil {
		t.Fatalf("tool turn %+v %v", res, err)
	}

	// A gelati tool runs, allowed by its own policy rule.
	if res, err := a.Run(ctx, gelati.Text(`tool shout {"message": "hi"}`)); err != nil || res.Text != `tool said: {"result":"HI"}` {
		t.Fatalf("gelati tool turn %+v %v", res, err)
	}
	rules := readRecord(t, record).GetPolicyConfig().GetRules()
	if len(rules) != 3 || !rules[0].GetIsDynamic() || rules[2].GetTool() != "shout" || rules[2].GetDecision() != wire.PolicyDecision_POLICY_DECISION_ALLOW {
		t.Fatalf("policy rules %+v", rules)
	}
	// run_command asks Approve.
	if res, err := a.Run(ctx, gelati.Text("policy rule_0")); err != nil || res.Text != "policy POLICY_EVALUATION_OUTCOME_ALLOW " {
		t.Fatalf("policy turn %+v %v", res, err)
	}
	mu.Lock()
	if len(approved) != 1 || approved[0].Name != "run_command" || approved[0].Input["CommandLine"] != "rm -rf /" {
		t.Fatalf("approve requests %+v", approved)
	}
	if _, ok := approved[0].Raw.(*ToolCall); !ok {
		t.Fatalf("approve raw %T", approved[0].Raw)
	}
	mu.Unlock()

	// Structured output.
	res, err = a.Run(ctx, gelati.Text(`structured {"total": 3}`))
	if err != nil {
		t.Fatal(err)
	}
	var got answer
	if err := res.DecodeStructuredOutput(&got); err != nil || got.Total != 3 {
		t.Fatalf("structured output %s: %+v %v", res.StructuredOutput, got, err)
	}

	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	waitDone(t, a.Done())
	if a.Err() != nil {
		t.Fatalf("Err after Close = %v", a.Err())
	}
}

// recordApprove returns an Approve function that records its requests and
// answers with d and err.
func recordApprove(reqs *[]gelati.ToolRequest, d gelati.Decision, err error) func(context.Context, gelati.ToolRequest) (gelati.Decision, error) {
	return func(_ context.Context, req gelati.ToolRequest) (gelati.Decision, error) {
		*reqs = append(*reqs, req)
		return d, err
	}
}

func TestProviderApprove(t *testing.T) {
	ctx := t.Context()
	call := ToolCall{ID: "c1", Name: "run_command", Args: map[string]any{"CommandLine": "ls"}}
	var reqs []gelati.ToolRequest

	// A nil base Policies becomes ConfirmRunCommand asking Approve.
	got := mustOptions(t, Provider(Options{}).(provider), gelati.Config{Approve: recordApprove(&reqs, gelati.Allow(), nil)})
	if len(got.Policies) != 2 || got.Tools != nil {
		t.Fatalf("policies %+v tools %v", got.Policies, got.Tools)
	}
	first, rest := got.Policies[0], got.Policies[1]
	if first.Tool != "run_command" || first.Decision != DecisionAskUser || first.Name != "confirm_run_command" || first.AskUser == nil {
		t.Fatalf("first policy %+v", first)
	}
	if rest.Tool != WildcardTool || rest.Decision != DecisionApprove || rest.AskUser != nil {
		t.Fatalf("second policy %+v", rest)
	}
	if ok, err := first.AskUser(ctx, call, "why"); !ok || err != nil {
		t.Fatalf("allow: %v %v", ok, err)
	}
	if len(reqs) != 1 {
		t.Fatalf("requests %+v", reqs)
	}
	req := reqs[0]
	if raw, ok := req.Raw.(*ToolCall); !ok || !reflect.DeepEqual(*raw, call) {
		t.Fatalf("raw %#v", req.Raw)
	}
	req.Raw = nil
	if want := (gelati.ToolRequest{ID: "c1", Name: "run_command", Input: call.Args, Reason: "why"}); !reflect.DeepEqual(req, want) {
		t.Fatalf("request %+v", req)
	}

	// Deny and errors deny; the error reaches agy, which denies with it.
	got = mustOptions(t, Provider(Options{}).(provider), gelati.Config{Approve: recordApprove(&reqs, gelati.Deny("no"), nil)})
	if ok, err := got.Policies[0].AskUser(ctx, call, ""); ok || err != nil {
		t.Fatalf("deny: %v %v", ok, err)
	}
	boom := errors.New("boom")
	got = mustOptions(t, Provider(Options{}).(provider), gelati.Config{Approve: recordApprove(&reqs, gelati.Allow(), boom)})
	if ok, err := got.Policies[0].AskUser(ctx, call, ""); ok || err != boom {
		t.Fatalf("error: %v %v", ok, err)
	}

	// Explicit base policies: Approve answers the ask-user and auto ones.
	baseAsked := 0
	baseHandler := func(context.Context, ToolCall, string) (bool, error) { baseAsked++; return false, nil }
	base := Options{Policies: []Policy{
		{Tool: "run_command", Decision: DecisionAskUser, AskUser: baseHandler},
		{Tool: "x", Decision: DecisionDeny},
		{Auto: true},
		AllowAllPolicy(),
	}}
	reqs = nil
	got = mustOptions(t, Provider(base).(provider), gelati.Config{Approve: recordApprove(&reqs, gelati.Allow(), nil)})
	if len(got.Policies) != 4 || got.Policies[1].AskUser != nil || got.Policies[3].AskUser != nil {
		t.Fatalf("policies %+v", got.Policies)
	}
	for _, i := range []int{0, 2} {
		if ok, _ := got.Policies[i].AskUser(ctx, call, ""); !ok {
			t.Fatalf("policy %d not answered by Approve", i)
		}
	}
	if len(reqs) != 2 || baseAsked != 0 {
		t.Fatalf("requests %d base asked %d", len(reqs), baseAsked)
	}
	// base is untouched.
	if base.Policies[2].AskUser != nil {
		t.Fatal("base auto policy modified")
	}
	if ok, _ := base.Policies[0].AskUser(ctx, call, ""); ok || baseAsked != 1 {
		t.Fatal("base ask-user policy modified")
	}
	// The policies compile, with the auto policy now asking.
	if _, dynamic, err := policyConfig(got.Policies); err != nil || dynamic["auto"].Decision != DecisionAskUser {
		t.Fatalf("policyConfig: %v", err)
	}
}

func TestProviderTools(t *testing.T) {
	ctx := t.Context()
	echo := NewTool("echo", "Echoes.", func(_ context.Context, _ *ToolContext, in struct{}) (string, error) { return "", nil })
	var gotArgs []string
	boom := errors.New("boom")
	schema := map[string]any{"type": "object", "properties": map[string]any{"n": map[string]any{"type": "integer"}}}
	tools := []gelati.Tool{
		{Name: "count", Description: "Counts.", InputSchema: schema, Run: func(_ context.Context, args jsontext.Value) (string, error) {
			gotArgs = append(gotArgs, string(args))
			return "counted", nil
		}},
		{Name: "fail", Run: func(context.Context, jsontext.Value) (string, error) { return "", boom }},
	}
	base := Options{Tools: []*Tool{echo}}
	p := Provider(base).(provider)
	got := mustOptions(t, p, gelati.Config{Tools: tools})
	if len(got.Tools) != 3 || got.Tools[0] != echo || len(base.Tools) != 1 {
		t.Fatalf("tools %v base %v", got.Tools, base.Tools)
	}
	count, fail := got.Tools[1], got.Tools[2]
	if count.Name() != "count" || count.Description() != "Counts." || !reflect.DeepEqual(count.Schema(), NormalizeSchema(schema)) {
		t.Fatalf("count tool %s %q %v", count.Name(), count.Description(), count.Schema())
	}
	if want := map[string]any{"type": "object", "properties": map[string]any{}}; fail.Name() != "fail" || !reflect.DeepEqual(fail.Schema(), want) {
		t.Fatalf("fail tool %s %v", fail.Name(), fail.Schema())
	}
	if out, err := count.Call(ctx, nil, map[string]any{"n": 2.0, "s": "x"}); out != "counted" || err != nil {
		t.Fatalf("count: %v %v", out, err)
	}
	if out, err := count.Call(ctx, nil, nil); out != "counted" || err != nil {
		t.Fatalf("count without args: %v %v", out, err)
	}
	if want := []string{`{"n":2,"s":"x"}`, `{}`}; !reflect.DeepEqual(gotArgs, want) {
		t.Fatalf("args %q", gotArgs)
	}
	if _, err := fail.Call(ctx, nil, nil); err != boom {
		t.Fatalf("fail: %v", err)
	}

	// A nil base Policies becomes ConfirmRunCommand plus an allow per tool.
	allow := func(name string) Policy { return Policy{Tool: name, Decision: DecisionApprove, Name: "gelati_tool"} }
	if want := append(ConfirmRunCommandPolicies(nil), allow("count"), allow("fail")); !reflect.DeepEqual(got.Policies, want) {
		t.Fatalf("policies %+v", got.Policies)
	}
	var reqs []gelati.ToolRequest
	got = mustOptions(t, p, gelati.Config{Tools: tools[:1], Approve: recordApprove(&reqs, gelati.Allow(), nil)})
	if len(got.Policies) != 3 || got.Policies[0].AskUser == nil || !reflect.DeepEqual(got.Policies[2], allow("count")) {
		t.Fatalf("policies with Approve %+v", got.Policies)
	}

	// The tools run even when wildcard rules deny or ask.
	base.Policies = []Policy{{Tool: WildcardTool, Decision: DecisionDeny}}
	got = mustOptions(t, Provider(base).(provider), gelati.Config{Tools: tools})
	hook, err := EnforcePolicies(got.Policies, nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, deny := range map[string]bool{"count": false, "fail": false, "echo": true, "view_file": true} {
		if res, _ := hook(ctx, nil, &ToolCall{Name: name}); res.Deny != deny {
			t.Errorf("%s: deny %v", name, res.Deny)
		}
	}
	if len(base.Policies) != 1 {
		t.Fatalf("base policies modified: %+v", base.Policies)
	}
	// Empty Policies apply no rule and stay empty.
	base.Policies = []Policy{}
	if got := mustOptions(t, Provider(base).(provider), gelati.Config{Tools: tools}); got.Policies == nil || len(got.Policies) != 0 {
		t.Fatalf("empty policies became %+v", got.Policies)
	}
	// Without Approve and Tools, a nil Policies stays nil.
	if got := mustOptions(t, p, gelati.Config{Model: "m"}); got.Policies != nil {
		t.Fatalf("policies %+v", got.Policies)
	}

	// Names a policy would confuse with other tools are rejected.
	run := tools[0].Run
	for _, name := range []string{"run_command", "*", "srv/tool"} {
		if _, err := p.options(gelati.Config{Tools: []gelati.Tool{{Name: name, Run: run}}}); err == nil {
			t.Errorf("tool %q accepted", name)
		}
	}
	// A tool named like one of base's fails Open.
	_, err = gelati.Open(ctx, p, gelati.Config{Tools: []gelati.Tool{{Name: "echo", Run: run}}})
	if err == nil || !strings.Contains(err.Error(), "Duplicate custom tool name 'echo'") {
		t.Fatalf("Open with a duplicate tool: %v", err)
	}
}

func TestProviderInputContent(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\nrest")
	got, err := inputContent([]gelati.Input{gelati.Text("look"), gelati.Image(png, ""), gelati.Image([]byte{1}, "image/webp")})
	if err != nil {
		t.Fatal(err)
	}
	want := []Content{Text("look"), Image{Data: png, MIMEType: "image/png"}, Image{Data: []byte{1}, MIMEType: "image/webp"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("content %#v", got)
	}
}
