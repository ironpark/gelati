package wire

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
)

// The goldens in testdata/golden are produced by Python's json_format and
// SerializeToString from the upstream descriptors; see testdata/gen_golden.py.

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

func newRoot(prefix string) proto.Message {
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

// TestGoldenRoundTrip checks that what Python writes decodes, re-encodes to
// the same JSON, and decodes the same from its snake_case form.
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
			if !proto.Equal(snake, msg) {
				t.Fatalf("snake_case decode differs\n got: %v\nwant: %v", snake, msg)
			}
		})
	}
}

// TestEncodeMatchesPython builds the InitializeConversationEvent the Python SDK
// sends for a typical agent and compares it with json_format's output.
func TestEncodeMatchesPython(t *testing.T) {
	ev := InitializeConversationEvent_builder{Config: HarnessConfig_builder{
		CascadeId:               new(""),
		SessionContinuationMode: new(HarnessConfig_SESSION_CONTINUATION_MODE_UNSPECIFIED),
		HarnessSideTools: HarnessSideTools_builder{
			Find:       FindToolConfig_builder{Enabled: new(true)}.Build(),
			RunCommand: RunCommandToolConfig_builder{Enabled: new(false), MaxTimeoutMs: new(uint32(0))}.Build(),
			Subagents:  SubagentsConfig_builder{Enabled: new(false)}.Build(),
		}.Build(),
		CompactionThreshold: new(uint32(0)),
		Workspaces: []*Workspace{Workspace_builder{
			FilesystemWorkspace: FilesystemWorkspace_builder{Directory: new("/repo")}.Build(),
		}.Build()},
		Models: []*ModelConfig{ModelConfig_builder{
			Name:              new("gemini-3-flash"),
			Types:             []ModelType{ModelType_MODEL_TYPE_TEXT},
			GeminiApiEndpoint: GeminiAPIEndpoint_builder{ApiKey: new("k"), HttpHeaders: map[string]string{"X-A": "b"}}.Build(),
		}.Build()},
		EnabledHooks:  []LifecycleHook{LifecycleHook_LIFECYCLE_HOOK_PRE_TOOL, LifecycleHook_LIFECYCLE_HOOK_STOP},
		BudgetConfig:  BudgetConfig_builder{MaxTotalTokens: new(int64(1 << 40))}.Build(),
		AgentBehavior: new(AgentBehavior_AGENT_BEHAVIOR_AUTONOMOUS),
		SkillsConfig:  SkillsConfig_builder{Enabled: new(false)}.Build(),
	}.Build()}.Build()
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

// TestDecodeLenient checks that proto field names, quoted integers and
// unknown members are accepted.
func TestDecodeLenient(t *testing.T) {
	ev := &OutputEvent{}
	in := `{"seq_num": 12, "timestampMicros": "34", "unknownField": {"x": 1},
		"step_update": {"state": 2, "step_index": 3, "text": "a_b\":"}}`
	if err := Unmarshal([]byte(in), ev); err != nil {
		t.Fatal(err)
	}
	if ev.GetSeqNum() != 12 || ev.GetTimestampMicros() != 34 {
		t.Errorf("seq/timestamp = %d/%d", ev.GetSeqNum(), ev.GetTimestampMicros())
	}
	su := ev.GetStepUpdate()
	if su.GetState() != StepUpdate_STATE_DONE || su.GetStepIndex() != 3 || su.GetText() != `a_b":` {
		t.Errorf("step update = %v", su)
	}
	in = `{"usageUpdate": {"total": {"prompt_token_count": 5, "candidatesTokenCount": "6"}}}`
	if err := Unmarshal([]byte(in), ev); err != nil {
		t.Fatal(err)
	}
	total := ev.GetUsageUpdate().GetTotal()
	if total.GetPromptTokenCount() != 5 || total.GetCandidatesTokenCount() != 6 || total.HasTotalTokenCount() {
		t.Errorf("usage = %v", total)
	}
}

func TestStructRoundTrip(t *testing.T) {
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
	if !reflect.DeepEqual(semantic(t, b), semantic(t, []byte(want))) {
		t.Fatalf("got  %s\nwant %s", b, want)
	}
	back := &Struct{}
	if err := Unmarshal(b, back); err != nil {
		t.Fatal(err)
	}
	got := back.AsMap()
	if !reflect.DeepEqual(got, map[string]any{"n": nil, "x": 1.0, "l": []any{"a", true}, "m": map[string]any{"k": 2.5}}) {
		t.Fatalf("AsMap = %#v", got)
	}
	if !back.GetFields()[2].GetValue().HasNullValue() {
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

func TestProtoMap(t *testing.T) {
	got, err := ProtoMap(ActionListDirectory_builder{
		DirectoryPath: new("file:///tmp"),
		Results: []*ActionListDirectory_Result{
			ActionListDirectory_Result_builder{Name: new("a.txt"), FileSize: new(uint64(100))}.Build(),
			ActionListDirectory_Result_builder{Name: new("sub"), IsDirectory: new(true)}.Build(),
		},
	}.Build())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"directory_path": "file:///tmp",
		"results": []any{
			map[string]any{"name": "a.txt", "file_size": "100"},
			map[string]any{"name": "sub", "is_directory": true},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ProtoMap = %#v\nwant %#v", got, want)
	}

	// Set zero values are kept; unset fields are omitted.
	got, err = ProtoMap(ActionViewFile_builder{FilePath: new(""), StartLine: new(uint32(0))}.Build())
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]any{"file_path": "", "start_line": float64(0)}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ProtoMap = %#v, want %#v", got, want)
	}

	// Empty messages are empty maps; nil messages are nil.
	if got, _ := ProtoMap(&ActionInvokeSubagent{}); got == nil || len(got) != 0 {
		t.Fatalf("empty message = %#v", got)
	}
	if got, _ := ProtoMap((*ActionInvokeSubagent)(nil)); got != nil {
		t.Fatalf("nil message = %#v", got)
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

// TestHandshakeBinary checks the stdin/stdout handshake messages against the
// bytes Python's SerializeToString produces.
func TestHandshakeBinary(t *testing.T) {
	for name, want := range map[string]proto.Message{
		"inputconfig_python.hex": InputConfig_builder{
			StorageDirectory: new(""),
			ClientInfo: ClientInfo_builder{
				Language: new("python"), Version: new("0.1.20"), LanguageVersion: new("3.12.1"),
				Os: new("darwin"), OsVersion: new("24.6.0"),
			}.Build(),
			Env: map[string]string{"GEMINI_API_KEY": "k"},
		}.Build(),
		"inputconfig_full.hex": InputConfig_builder{
			StorageDirectory: new("/tmp/s"), Port: new(uint32(4242)), BindAddress: new("127.0.0.1"),
			ClientInfo: ClientInfo_builder{Language: new("go")}.Build(), Env: map[string]string{"A": "1"},
			UseInteractionsApi: new(false),
		}.Build(),
		"outputconfig.hex":          OutputConfig_builder{Port: new(int32(54321)), ApiKey: new("secret-key")}.Build(),
		"outputconfig_negative.hex": OutputConfig_builder{Port: new(int32(-1))}.Build(),
	} {
		golden := readHex(t, name)
		got, err := proto.MarshalOptions{Deterministic: true}.Marshal(want)
		if err != nil {
			t.Fatal(err)
		}
		if hex.EncodeToString(got) != hex.EncodeToString(golden) {
			t.Errorf("%s: encoded %x, want %x", name, got, golden)
		}
		back := want.ProtoReflect().New().Interface()
		if err := proto.Unmarshal(golden, back); err != nil {
			t.Fatal(err)
		}
		if !proto.Equal(back, want) {
			t.Errorf("%s: decoded %v, want %v", name, back, want)
		}
	}
}
