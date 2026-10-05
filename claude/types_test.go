package claude

import (
	jsonv1 "encoding/json" // Number: callers may hand in values decoded with UseNumber
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"math"
	"reflect"
	"testing"
)

func TestContentBlockJSONRoundTrip(t *testing.T) {
	t.Parallel()
	isErr := true
	cases := []struct {
		name  string
		block ContentBlock
		json  string
	}{
		{"text", &TextBlock{Text: "hello"}, `{"text":"hello"}`},
		{"thinking", &ThinkingBlock{Thinking: "hmm", Signature: "sig"}, `{"thinking":"hmm","signature":"sig"}`},
		{"tool_use", &ToolUseBlock{ID: "tu1", Name: "Read", Input: map[string]any{"file_path": "/tmp/x"}},
			`{"id":"tu1","name":"Read","input":{"file_path":"/tmp/x"}}`},
		{"tool_result", &ToolResultBlock{ToolUseID: "tu1", IsError: &isErr}, `{"tool_use_id":"tu1","is_error":true}`},
		{"server_tool_use", &ServerToolUseBlock{ID: "st1", Name: ServerToolWebSearch, Input: map[string]any{"query": "go"}},
			`{"id":"st1","name":"web_search","input":{"query":"go"}}`},
		{"advisor_tool_result", &ServerToolResultBlock{ToolUseID: "st1", Content: map[string]any{"type": "advisor_result"}},
			`{"tool_use_id":"st1","content":{"type":"advisor_result"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(tc.block, json.Deterministic(true))
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(got) != tc.json {
				t.Fatalf("marshal = %s, want %s", got, tc.json)
			}
			if tc.block.BlockType() != tc.name {
				t.Fatalf("BlockType = %q, want %q", tc.block.BlockType(), tc.name)
			}
		})
	}
}

func TestModelUsageUnmarshal(t *testing.T) {
	t.Parallel()
	const raw = `{"inputTokens":10,"outputTokens":20,"cacheReadInputTokens":1,
	  "cacheCreationInputTokens":2,"webSearchRequests":0,"costUSD":0.5,
	  "contextWindow":200000,"maxOutputTokens":64000,"canonicalModel":"claude-opus-4-7"}`
	var mu ModelUsage
	if err := json.Unmarshal([]byte(raw), &mu); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if mu.InputTokens != 10 || mu.CostUSD != 0.5 || mu.CanonicalModel != "claude-opus-4-7" {
		t.Fatalf("unexpected usage: %+v", mu)
	}
}

func TestMessageOriginRoundTrip(t *testing.T) {
	t.Parallel()
	const raw = `{"kind":"peer","from":"agent://x","name":"X","fromSession":"s1","verifiedPeerPid":42}`
	var o MessageOrigin
	if err := json.Unmarshal([]byte(raw), &o); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if o.Kind != OriginPeer || o.From != "agent://x" || o.VerifiedPeerPID != 42 {
		t.Fatalf("unexpected origin: %+v", o)
	}
	if len(o.Extra) != 0 {
		t.Fatalf("extra = %v, want none", o.Extra)
	}
	out, err := json.Marshal(o, json.Deterministic(true))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(out) != raw {
		t.Fatalf("marshal = %s, want %s", out, raw)
	}
}

func TestMessageOriginExtraRoundTrip(t *testing.T) {
	t.Parallel()
	const raw = `{"kind":"peer","from":"agent://x","hop":2,"trace":{"id":"t1"}}`
	var o MessageOrigin
	if err := json.Unmarshal([]byte(raw), &o); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if o.Extra["hop"] != float64(2) {
		t.Fatalf("extra = %v, want hop 2", o.Extra)
	}
	out, err := json.Marshal(o, json.Deterministic(true))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if got["kind"] != OriginPeer || got["hop"] != float64(2) {
		t.Fatalf("marshal = %s", out)
	}
	if trace, ok := got["trace"].(map[string]any); !ok || trace["id"] != "t1" {
		t.Fatalf("trace lost: %s", out)
	}
}

func TestPermissionUpdateWireFormat(t *testing.T) {
	t.Parallel()
	content := "npm test"
	cases := []struct {
		name string
		u    PermissionUpdate
		want string
	}{
		{
			"addRules",
			PermissionUpdate{
				Type:        PermissionUpdateAddRules,
				Rules:       []PermissionRuleValue{{ToolName: "Bash", RuleContent: &content}},
				Behavior:    BehaviorAllow,
				Destination: DestinationSession,
			},
			`{"type":"addRules","rules":[{"toolName":"Bash","ruleContent":"npm test"}],"behavior":"allow","destination":"session"}`,
		},
		{
			"setMode",
			PermissionUpdate{Type: PermissionUpdateSetMode, Mode: PermissionModeAcceptEdits},
			`{"type":"setMode","mode":"acceptEdits"}`,
		},
		{
			"addDirectories",
			PermissionUpdate{Type: PermissionUpdateAddDirectories, Directories: []string{"/tmp"}},
			`{"type":"addDirectories","directories":["/tmp"]}`,
		},
		{
			// Fields that do not belong to the variant are dropped.
			"setModeIgnoresRules",
			PermissionUpdate{Type: PermissionUpdateSetMode, Mode: PermissionModePlan, Directories: []string{"/tmp"}},
			`{"type":"setMode","mode":"plan"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(tc.u, json.Deterministic(true))
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("marshal = %s, want %s", got, tc.want)
			}
			var back PermissionUpdate
			if err := json.Unmarshal(got, &back); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if back.Type != tc.u.Type {
				t.Fatalf("round trip type = %q, want %q", back.Type, tc.u.Type)
			}
		})
	}
}

func TestHookOutputWireFormat(t *testing.T) {
	t.Parallel()
	yes := true
	timeout := 5000
	cases := []struct {
		name string
		out  HookOutput
		want string
	}{
		{"empty", HookOutput{}, `{}`},
		{"continue", HookOutput{Continue: &yes}, `{"continue":true}`},
		{"block", HookOutput{Decision: "block", Reason: "nope"}, `{"decision":"block","reason":"nope"}`},
		{
			"hookSpecific",
			HookOutput{HookSpecificOutput: map[string]any{"hookEventName": "PreToolUse", "permissionDecision": "allow"}},
			`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow"}}`,
		},
		{"async", HookOutput{Async: true, AsyncTimeout: &timeout}, `{"async":true,"asyncTimeout":5000}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(tc.out, json.Deterministic(true))
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("marshal = %s, want %s", got, tc.want)
			}
			var back HookOutput
			if err := json.Unmarshal(got, &back); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if back.Decision != tc.out.Decision || back.Async != tc.out.Async {
				t.Fatalf("round trip = %+v, want %+v", back, tc.out)
			}
		})
	}
}

func TestMCPServerConfigJSON(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  MCPServerConfig
		want string
	}{
		{"stdio", &MCPStdioServerConfig{Command: "node", Args: []string{"srv.js"}}, `{"type":"stdio","command":"node","args":["srv.js"]}`},
		{"sse", &MCPSSEServerConfig{URL: "https://x/sse"}, `{"type":"sse","url":"https://x/sse"}`},
		{"http", &MCPHTTPServerConfig{URL: "https://x/mcp", Headers: map[string]string{"A": "b"}}, `{"type":"http","url":"https://x/mcp","headers":{"A":"b"}}`},
		{"sdk", &MCPSDKServerConfig{Name: "calc", Instance: &MCPServer{}}, `{"name":"calc","type":"sdk"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(tc.cfg, json.Deterministic(true))
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("marshal = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestOptionsBufferSize(t *testing.T) {
	t.Parallel()
	var nilOpts *Options
	if got := nilOpts.bufferSize(); got != DefaultMaxBufferSize {
		t.Fatalf("nil options buffer = %d, want %d", got, DefaultMaxBufferSize)
	}
	if got := (&Options{}).bufferSize(); got != DefaultMaxBufferSize {
		t.Fatalf("zero options buffer = %d, want %d", got, DefaultMaxBufferSize)
	}
	if got := (&Options{MaxBufferSize: 32}).bufferSize(); got != 32 {
		t.Fatalf("buffer = %d, want 32", got)
	}
}

func TestAgentDefinitionJSON(t *testing.T) {
	t.Parallel()
	got, err := json.Marshal(AgentDefinition{
		Description: "reviewer",
		Prompt:      "review code",
		Tools:       []string{"Read"},
		Model:       "sonnet",
	}, json.Deterministic(true))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const want = `{"description":"reviewer","prompt":"review code","tools":["Read"],"model":"sonnet"}`
	if string(got) != want {
		t.Fatalf("marshal = %s, want %s", got, want)
	}
}

func TestErrorHierarchy(t *testing.T) {
	t.Parallel()

	notFound := NewCLINotFoundError("", "/usr/bin/claude")
	if notFound.Error() != "Claude Code not found: /usr/bin/claude" {
		t.Fatalf("message = %q", notFound.Error())
	}
	var connErr *ConnectionError
	if !errors.As(error(notFound), &connErr) {
		t.Fatal("CLINotFoundError should unwrap to *ConnectionError")
	}
	var sdkErr Error
	if !errors.As(error(notFound), &sdkErr) {
		t.Fatal("CLINotFoundError should satisfy claude.Error")
	}
	if !errors.Is(notFound, ErrCLINotFound) {
		t.Fatal("CLINotFoundError should match ErrCLINotFound")
	}

	code := 2
	proc := NewProcessError("Command failed", &code, "boom")
	if proc.Error() != "Command failed (exit code: 2)\nError output: boom" {
		t.Fatalf("message = %q", proc.Error())
	}
	if proc.ExitCode == nil || *proc.ExitCode != 2 || proc.Stderr != "boom" {
		t.Fatalf("unexpected fields: %+v", proc)
	}
}

func TestResultError(t *testing.T) {
	t.Parallel()
	code := 1
	data := map[string]any{
		"subtype":          "error_max_turns",
		"errors":           []any{" limit reached ", "", 7},
		"result":           "API Error: overloaded",
		"api_error_status": float64(529),
		"terminal_reason":  "api_error",
		"session_id":       "sess-1",
	}
	err := NewResultError("Query failed", data, &code)
	if err.Subtype != "error_max_turns" {
		t.Fatalf("subtype = %q", err.Subtype)
	}
	if len(err.Errors) != 1 || err.Errors[0] != "limit reached" {
		t.Fatalf("errors = %#v", err.Errors)
	}
	if err.APIErrorStatus == nil || *err.APIErrorStatus != 529 {
		t.Fatalf("api status = %v", err.APIErrorStatus)
	}
	if err.TerminalReason != "api_error" || err.SessionID != "sess-1" {
		t.Fatalf("unexpected fields: %+v", err)
	}
	var proc *ProcessError
	if !errors.As(error(err), &proc) {
		t.Fatal("ResultError should unwrap to *ProcessError")
	}

	// A bare string errors field is tolerated.
	err = NewResultError("x", map[string]any{"errors": "oops"}, nil)
	if len(err.Errors) != 1 || err.Errors[0] != "oops" {
		t.Fatalf("errors = %#v", err.Errors)
	}
	// A missing payload is tolerated.
	if e := NewResultError("x", nil, nil); e.Subtype != "" || e.Errors != nil {
		t.Fatalf("unexpected: %+v", e)
	}
}

func TestJSONDecodeAndParseErrors(t *testing.T) {
	t.Parallel()
	inner := errors.New("unexpected token")
	line := "{" + string(make([]byte, 0)) + "not json"
	de := NewJSONDecodeError(line, inner)
	if !errors.Is(de, inner) {
		t.Fatal("JSONDecodeError should wrap its cause")
	}
	if de.Line != line {
		t.Fatalf("line = %q", de.Line)
	}

	pe := NewMessageParseError("missing type", jsontext.Value(`{"a":1}`))
	if pe.Error() != "missing type" || string(pe.Data) != `{"a":1}` {
		t.Fatalf("unexpected: %+v", pe)
	}
	var sdkErr Error
	if !errors.As(error(pe), &sdkErr) {
		t.Fatal("MessageParseError should satisfy claude.Error")
	}
}

// TestNestedUnmarshalersAreLenient checks that a type mismatch inside a value
// with a custom UnmarshalJSON neither fails nor cuts short the enclosing
// decode.
func TestNestedUnmarshalersAreLenient(t *testing.T) {
	t.Parallel()
	var got struct {
		Tool   ToolResultBlock       `json:"tool"`
		MCP    MCPToolResultBlock    `json:"mcp"`
		Server ServerToolResultBlock `json:"server"`
		Origin MessageOrigin         `json:"origin"`
		Update PermissionUpdate      `json:"update"`
		Wrong  PermissionUpdate      `json:"wrong"`
		After  string                `json:"after"`
	}
	const raw = `{
	  "tool":{"tool_use_id":"t","is_error":"yes","content":"c"},
	  "mcp":{"tool_use_id":"m","is_error":1,"content":[{"type":"text"}]},
	  "server":{"tool_use_id":7,"content":{"k":1}},
	  "origin":{"kind":3,"verifiedPeerPid":"x","from":"agent://a"},
	  "update":{"type":"setMode","mode":5,"destination":"session"},
	  "wrong":"not an object",
	  "after":"kept"}`
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.After != "kept" {
		t.Fatalf("decode stopped early: %+v", got)
	}
	if got.Tool.ToolUseID != "t" || got.Tool.ContentText == nil || *got.Tool.ContentText != "c" {
		t.Fatalf("tool = %+v", got.Tool)
	}
	if got.MCP.ToolUseID != "m" || len(got.MCP.ContentList) != 1 {
		t.Fatalf("mcp = %+v", got.MCP)
	}
	if got.Server.ToolUseID != "" || got.Server.Content["k"] != 1.0 {
		t.Fatalf("server = %+v", got.Server)
	}
	if got.Origin.Kind != OriginUnclassified || got.Origin.From != "agent://a" || got.Origin.Extra["kind"] != 3.0 {
		t.Fatalf("origin = %+v", got.Origin)
	}
	if got.Update.Type != PermissionUpdateSetMode || got.Update.Mode != "" || got.Update.Destination != DestinationSession {
		t.Fatalf("update = %+v", got.Update)
	}
	if got.Wrong.Type != "" {
		t.Fatalf("wrong = %+v", got.Wrong)
	}
	// Syntax errors still fail.
	var b ToolResultBlock
	if err := b.UnmarshalJSON([]byte(`{"tool_use_id":`)); err == nil {
		t.Fatal("want a syntax error")
	}
}

func TestMessageOriginUnmarshalUnclassified(t *testing.T) {
	t.Parallel()
	var o MessageOrigin
	if err := json.Unmarshal([]byte(`{"from":"agent://a"}`), &o); err != nil {
		t.Fatal(err)
	}
	if o.Kind != OriginUnclassified || o.From != "agent://a" || o.Extra != nil {
		t.Fatalf("origin = %+v", o)
	}
}

func TestPermissionUpdateKeepsEmptyLists(t *testing.T) {
	t.Parallel()
	got, err := json.Marshal(PermissionUpdate{Type: PermissionUpdateReplaceRules, Rules: []PermissionRuleValue{}}, json.Deterministic(true))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"type":"replaceRules","rules":[]}` {
		t.Fatalf("marshal = %s", got)
	}
}

func TestHookOutputEmptySpecificMap(t *testing.T) {
	t.Parallel()
	got, err := json.Marshal(HookOutput{HookSpecificOutput: map[string]any{}}, json.Deterministic(true))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"hookSpecificOutput":{}}` {
		t.Fatalf("marshal = %s", got)
	}
}

func TestPermissionDecisionWire(t *testing.T) {
	t.Parallel()
	input := map[string]any{"command": "ls"}
	request := map[string]any{"tool_use_id": "tu1", "input": input}
	cases := []struct {
		name    string
		result  PermissionResult
		request map[string]any
		want    string
	}{
		{"hookAllow", &PermissionResultAllow{}, nil, `{"behavior":"allow"}`},
		{"hookDeny", &PermissionResultDeny{}, nil, `{"behavior":"deny"}`},
		{"replyAllowDefaultsInput", &PermissionResultAllow{DecisionClassification: DecisionUserPermanent}, request,
			`{"behavior":"allow","decisionClassification":"user_permanent","toolUseID":"tu1","updatedInput":{"command":"ls"}}`},
		{"replyAllowNoInput", &PermissionResultAllow{}, map[string]any{}, `{"behavior":"allow","updatedInput":null}`},
		{"replyDenyAlwaysMessage", &PermissionResultDeny{Interrupt: true}, request,
			`{"behavior":"deny","interrupt":true,"message":"","toolUseID":"tu1"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, ok := permissionDecisionWire(tc.result)
			if tc.request != nil {
				out, ok = permissionReply(tc.result, tc.request)
			}
			if !ok {
				t.Fatal("not ok")
			}
			got, err := json.Marshal(out, json.Deterministic(true))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
	for _, bad := range []PermissionResult{nil, (*PermissionResultAllow)(nil), (*PermissionResultDeny)(nil)} {
		if _, ok := permissionReply(bad, request); ok {
			t.Fatalf("%#v: want not ok", bad)
		}
	}
}

func TestToolPermissionContextLenient(t *testing.T) {
	t.Parallel()
	request := map[string]any{
		"tool_use_id":            "tu1",
		"title":                  7,
		"default_to_no":          "yes",
		"permission_suggestions": []any{map[string]any{"type": "setMode", "mode": "plan"}},
		"matched_ask_rule":       map[string]any{"source": "userSettings", "tool_name": "Bash"},
	}
	pc := toolPermissionContext("req1", request)
	if pc.ToolUseID != "tu1" || pc.Title != "" || pc.DefaultToNo || pc.RequestID != "req1" || pc.Raw["title"] != 7 {
		t.Fatalf("context = %+v", pc)
	}
	if len(pc.Suggestions) != 1 || pc.Suggestions[0].Mode != PermissionModePlan {
		t.Fatalf("suggestions = %+v", pc.Suggestions)
	}
	if r := pc.MatchedAskRule; r == nil || r.ToolName != "Bash" || r.RuleContent != nil {
		t.Fatalf("matched ask rule = %+v", pc.MatchedAskRule)
	}
}

func TestToInt64(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   any
		want int64
		ok   bool
	}{
		{float64(12.7), 12, true},
		{jsonv1.Number("1700000000123"), 1700000000123, true},
		{jsonv1.Number("12.9"), 12, true},
		{int64(1) << 40, 1 << 40, true},
		{"12", 0, false},
		{math.NaN(), 0, false},
		{nil, 0, false},
	}
	for _, tc := range cases {
		if got, ok := toInt64(tc.in); got != tc.want || ok != tc.ok {
			t.Errorf("toInt64(%#v) = %d, %v; want %d, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestDecodeControlLenient(t *testing.T) {
	t.Parallel()
	data := map[string]any{"canRewind": "yes", "insertions": 3.0, "future": true}
	got, err := decodeControl[RewindFilesResult](data, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.CanRewind || got.Insertions != 3 || got.Raw["future"] != true {
		t.Fatalf("got %+v", got)
	}
}

func TestJSONMemberNames(t *testing.T) {
	t.Parallel()
	if !hookOutputFields["asyncTimeout"] || !hookOutputFields["continue"] || hookOutputFields["Specific"] || hookOutputFields["Extra"] {
		t.Fatalf("hook output fields = %v", hookOutputFields)
	}
	if len(originFields) != 9 || !originFields["verifiedPeerPid"] {
		t.Fatalf("origin fields = %v", originFields)
	}
	// Embedded structs contribute their members.
	names := jsonMemberNames(reflect.TypeFor[PreModelSwitchHookInput]())
	if !names["hook_event_name"] || !names["from_model"] {
		t.Fatalf("names = %v", names)
	}
}
