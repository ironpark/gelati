package wire

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The goldens in testdata/golden are produced by Python's json_format from the
// upstream descriptors; see testdata/gen_golden.py.

func readGolden(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "golden", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// semantic decodes JSON into a generic tree for comparison, so formatting,
// member order and 1.0 versus 1 do not matter.
func semantic(t *testing.T, b []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("invalid JSON %s: %v", b, err)
	}
	return v
}

func newRoot(prefix string) any {
	switch prefix {
	case "init":
		return &InitializeConversationEvent{}
	case "input":
		return &InputEvent{}
	case "output":
		return &OutputEvent{}
	}
	panic(prefix)
}

func TestGoldenRoundTrip(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "golden", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no golden files")
	}
	for _, path := range files {
		name := filepath.Base(path)
		if strings.HasSuffix(name, ".snake.json") {
			continue
		}
		t.Run(name, func(t *testing.T) {
			golden := readGolden(t, name)
			msg := newRoot(strings.SplitN(name, "_", 2)[0])
			if err := Unmarshal(golden, msg); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			got, err := Marshal(msg)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if !reflect.DeepEqual(semantic(t, got), semantic(t, golden)) {
				t.Fatalf("round trip mismatch\n got: %s\nwant: %s", got, golden)
			}

			snakeName := strings.TrimSuffix(name, ".json") + ".snake.json"
			if _, err := os.Stat(filepath.Join("testdata", "golden", snakeName)); err != nil {
				return
			}
			snake := newRoot(strings.SplitN(name, "_", 2)[0])
			if err := Unmarshal(readGolden(t, snakeName), snake); err != nil {
				t.Fatalf("Unmarshal snake_case: %v", err)
			}
			if !reflect.DeepEqual(snake, msg) {
				gotSnake, _ := Marshal(snake)
				t.Fatalf("snake_case decode differs\n got: %s\nwant: %s", gotSnake, got)
			}
		})
	}
}

// TestEncodeMatchesPython builds the InitializeConversationEvent the Python SDK
// sends for a typical agent and compares it with json_format's output.
func TestEncodeMatchesPython(t *testing.T) {
	ev := &InitializeConversationEvent{Config: &HarnessConfig{
		CascadeID:               new(""),
		SessionContinuationMode: new(HarnessConfigSessionContinuationModeUnspecified),
		HarnessSideTools: &HarnessSideTools{
			Find:       &FindToolConfig{Enabled: new(true)},
			RunCommand: &RunCommandToolConfig{Enabled: new(false), MaxTimeoutMs: new(uint32(0))},
			Subagents:  &SubagentsConfig{Enabled: new(false)},
		},
		CompactionThreshold: new(uint32(0)),
		Workspaces: []*Workspace{{
			FilesystemWorkspace: &FilesystemWorkspace{Directory: new("/repo")},
		}},
		Models: []*ModelConfig{{
			Name:              new("gemini-3-flash"),
			Types:             []ModelType{ModelTypeText},
			GeminiAPIEndpoint: &GeminiAPIEndpoint{APIKey: new("k"), HTTPHeaders: map[string]string{"X-A": "b"}},
		}},
		EnabledHooks:  []LifecycleHook{LifecycleHookPreTool, LifecycleHookStop},
		BudgetConfig:  &BudgetConfig{MaxTotalTokens: new(Int64(1 << 40))},
		AgentBehavior: new(AgentBehaviorAutonomous),
		SkillsConfig:  &SkillsConfig{Enabled: new(false)},
	}}
	want := `{"config":{"cascadeId":"","harnessSideTools":{"find":{"enabled":true},
		"runCommand":{"enabled":false,"maxTimeoutMs":0},"subagents":{"enabled":false}},
		"compactionThreshold":0,"workspaces":[{"filesystemWorkspace":{"directory":"/repo"}}],
		"models":[{"name":"gemini-3-flash","types":["MODEL_TYPE_TEXT"],
		"geminiApiEndpoint":{"httpHeaders":{"X-A":"b"},"apiKey":"k"}}],
		"enabledHooks":["LIFECYCLE_HOOK_PRE_TOOL","LIFECYCLE_HOOK_STOP"],
		"sessionContinuationMode":"SESSION_CONTINUATION_MODE_UNSPECIFIED",
		"agentBehavior":"AGENT_BEHAVIOR_AUTONOMOUS","budgetConfig":{"maxTotalTokens":"1099511627776"},
		"skillsConfig":{"enabled":false}}}`
	got, err := Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(semantic(t, got), semantic(t, []byte(want))) {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestDecodeLenient(t *testing.T) {
	var ev OutputEvent
	in := `{"seq_num": 12, "timestampMicros": "34", "unknownField": {"x": 1},
		"step_update": {"state": 2, "step_index": 3, "text": "a_b\":"},
		"usageUpdate": {"total": {"prompt_token_count": 5, "candidatesTokenCount": "6"}}}`
	if err := Unmarshal([]byte(in), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.GetSeqNum() != 12 || ev.GetTimestampMicros() != 34 {
		t.Errorf("seq/timestamp = %d/%d", ev.GetSeqNum(), ev.GetTimestampMicros())
	}
	su := ev.GetStepUpdate()
	if su.GetState() != StepUpdateStateDone || su.GetStepIndex() != 3 || su.GetText() != `a_b":` {
		t.Errorf("step update = %+v", su)
	}
	total := ev.GetUsageUpdate().GetTotal()
	if total.GetPromptTokenCount() != 5 || total.GetCandidatesTokenCount() != 6 || total.TotalTokenCount != nil {
		t.Errorf("usage = %+v", total)
	}
}

func TestEnumUnknownNumber(t *testing.T) {
	var su StepUpdate
	if err := Unmarshal([]byte(`{"state": 42}`), &su); err != nil {
		t.Fatal(err)
	}
	if su.GetState() != "42" {
		t.Fatalf("state = %q", su.GetState())
	}
	b, err := Marshal(&su)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"state":42}` {
		t.Fatalf("re-encoded as %s", b)
	}
}

func TestMapKeysNotRewritten(t *testing.T) {
	var ev InitializeConversationEvent
	in := `{"config": {"mcp_servers": [{"stdio": {"env": {"MY_VAR": "1", "camelCase": "2"}}}]}}`
	if err := Unmarshal([]byte(in), &ev); err != nil {
		t.Fatal(err)
	}
	env := ev.GetConfig().GetMCPServers()[0].GetStdio().GetEnv()
	if !reflect.DeepEqual(env, map[string]string{"MY_VAR": "1", "camelCase": "2"}) {
		t.Fatalf("env = %v", env)
	}
}

func TestValueNull(t *testing.T) {
	s, err := StructOf(map[string]any{"n": nil, "x": 1, "l": []any{"a", true}, "m": map[string]any{"k": 2.5}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"fields":[{"name":"l","value":{"listValue":{"values":[{"stringValue":"a"},{"boolValue":true}]}}},` +
		`{"name":"m","value":{"structValue":{"fields":[{"name":"k","value":{"numberValue":2.5}}]}}},` +
		`{"name":"n","value":{"nullValue":null}},{"name":"x","value":{"numberValue":1}}]}`
	if string(b) != want {
		t.Fatalf("got  %s\nwant %s", b, want)
	}
	var back Struct
	if err := Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	got := back.AsMap()
	if !reflect.DeepEqual(got, map[string]any{"n": nil, "x": 1.0, "l": []any{"a", true}, "m": map[string]any{"k": 2.5}}) {
		t.Fatalf("AsMap = %#v", got)
	}
	if back.Fields[2].Value.NullValue == nil {
		t.Fatal("null_value lost on decode")
	}
}

func TestValueOfStructFallback(t *testing.T) {
	type point struct {
		X    int      `json:"x"`
		Tags []string `json:"tags"`
	}
	v, err := ValueOf(point{X: 3, Tags: []string{"a"}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(v.AsAny(), map[string]any{"x": 3.0, "tags": []any{"a"}}) {
		t.Fatalf("AsAny = %#v", v.AsAny())
	}
}

func TestDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		json string
	}{
		{0, `"0s"`},
		{1500 * time.Millisecond, `"1.500s"`},
		{-1500 * time.Microsecond, `"-0.001500s"`},
		{3*time.Second + 1, `"3.000000001s"`},
	}
	for _, c := range cases {
		b, err := json.Marshal(NewDuration(c.d))
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != c.json {
			t.Errorf("%v encodes as %s, want %s", c.d, b, c.json)
		}
		var back Duration
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatal(err)
		}
		if back.AsDuration() != c.d {
			t.Errorf("%s decodes as %v, want %v", b, back.AsDuration(), c.d)
		}
	}
	var d Duration
	for _, bad := range []string{`"1"`, `"s"`, `1`, `"1.0000000001s"`} {
		if err := json.Unmarshal([]byte(bad), &d); err == nil {
			t.Errorf("%s: expected error", bad)
		}
	}
}

func TestDefaults(t *testing.T) {
	var ic *InputConfig
	if ic.GetBindAddress() != "localhost" {
		t.Errorf("bind address default = %q", ic.GetBindAddress())
	}
	if (&SkillsConfig{}).GetEnabled() != true {
		t.Error("skills enabled default should be true")
	}
	if (&SkillsConfig{Enabled: new(false)}).GetEnabled() {
		t.Error("explicit false lost")
	}
	if (&SubagentsConfig{}).GetMaxNestingDepth() != 1 {
		t.Error("max nesting depth default should be 1")
	}
}

func readHex(t *testing.T, name string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.TrimSpace(string(readGolden(t, name))))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestInputConfigBinary(t *testing.T) {
	python := &InputConfig{
		StorageDirectory: new(""),
		ClientInfo: &ClientInfo{
			Language: new("python"), Version: new("0.1.20"), LanguageVersion: new("3.12.1"),
			OS: new("darwin"), OSVersion: new("24.6.0"),
		},
		Env: map[string]string{"GEMINI_API_KEY": "k"},
	}
	full := &InputConfig{
		StorageDirectory: new("/tmp/s"), Port: new(uint32(4242)), BindAddress: new("127.0.0.1"),
		ClientInfo: &ClientInfo{Language: new("go")}, Env: map[string]string{"A": "1"},
		UseInteractionsAPI: new(false),
	}
	for name, want := range map[string]*InputConfig{"inputconfig_python.hex": python, "inputconfig_full.hex": full} {
		golden := readHex(t, name)
		got, err := want.MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, golden) {
			t.Errorf("%s: encoded %x, want %x", name, got, golden)
		}
		var back InputConfig
		if err := back.UnmarshalBinary(golden); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(&back, want) {
			t.Errorf("%s: decoded %+v, want %+v", name, back, want)
		}
	}
}

func TestOutputConfigBinary(t *testing.T) {
	for name, want := range map[string]*OutputConfig{
		"outputconfig.hex":          {Port: new(int32(54321)), APIKey: new("secret-key")},
		"outputconfig_negative.hex": {Port: new(int32(-1))},
	} {
		golden := readHex(t, name)
		var got OutputConfig
		if err := got.UnmarshalBinary(golden); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(&got, want) {
			t.Errorf("%s: decoded %+v, want %+v", name, got, want)
		}
		enc, err := want.MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(enc, golden) {
			t.Errorf("%s: encoded %x, want %x", name, enc, golden)
		}
	}
}

func TestBinarySkipsUnknownAndRejectsTruncated(t *testing.T) {
	// Unknown fields of every wire type before the real ones: varint (3),
	// fixed64 (4), fixed32 (5), a group (6) and a length-delimited field (7).
	var b []byte
	b = appendVarintField(b, 3, 1<<40)
	b = append(appendTag(b, 4, wireI64), 1, 2, 3, 4, 5, 6, 7, 8)
	b = append(appendTag(b, 5, wireI32), 1, 2, 3, 4)
	b = appendTag(b, 6, wireStartG)
	b = appendVarintField(b, 1, 9)
	b = appendTag(b, 6, wireEndG)
	b = appendLen(b, 7, "x")
	b = appendVarintField(b, 1, 8080)
	b = appendLen(b, 2, "key")
	var oc OutputConfig
	if err := oc.UnmarshalBinary(b); err != nil {
		t.Fatal(err)
	}
	if oc.GetPort() != 8080 || oc.GetAPIKey() != "key" {
		t.Fatalf("decoded %+v", oc)
	}
	for i := 1; i < len(b); i++ {
		var oc OutputConfig
		// Every proper prefix that cuts a field must fail rather than panic.
		_ = oc.UnmarshalBinary(b[:i])
	}
	if err := oc.UnmarshalBinary([]byte{0x12, 0x05, 'a'}); err == nil {
		t.Fatal("expected truncation error")
	}
}
