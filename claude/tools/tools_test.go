package tools

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

// allNames lists every tool-name constant.
var allNames = []string{
	Agent, Bash, ExitPlanMode, Edit, Read, Write, Glob, Grep, TaskStop,
	ListMcpResources, RefreshMcpTools, NotebookEdit, ReadMcpResourceDir,
	ReadMcpResource, ReportFindings, TodoWrite, WebFetch, WebSearch,
	AskUserQuestion, SendFeedback, ClaudeDesign, Projects, EnterPlanMode,
	TaskCreate, TaskGet, TaskUpdate, TaskList, Workflow, CronCreate,
	CronDelete, CronList, ScheduleWakeup, RemoteTrigger,
	ShowOnboardingRolePicker, ReadNotifications, Monitor, ProposeSkills,
	ProposeGoal, Artifact, PushNotification, EnterWorktree, ExitWorktree,
	"mcp__github__create_issue",
}

func TestRegistryCoversSchema(t *testing.T) {
	if len(schemaInputTypes) != 43 || len(schemaOutputTypes) != 43 {
		t.Fatalf("schema has %d inputs and %d outputs, want 43 each", len(schemaInputTypes), len(schemaOutputTypes))
	}
	if len(allNames) != len(registry) {
		t.Fatalf("%d names for %d registry entries", len(allNames), len(registry))
	}
	inputs := map[reflect.Type]string{}
	outputs := map[reflect.Type]string{}
	for _, name := range allNames {
		tool, ok := Lookup(name)
		if !ok {
			t.Fatalf("Lookup(%q) failed", name)
		}
		if prev, dup := inputs[tool.Input]; dup {
			t.Errorf("%s and %s share input type %s", prev, name, tool.Input)
		}
		inputs[tool.Input] = name
		outputs[tool.Output] = name
	}
	for _, typ := range schemaInputTypes {
		if _, ok := inputs[typ]; !ok {
			t.Errorf("input type %s is not reachable by tool name", typ)
		}
	}
	for _, typ := range schemaOutputTypes {
		if _, ok := outputs[typ]; !ok {
			t.Errorf("output type %s is not reachable by tool name", typ)
		}
	}
	if got := len(Tools()); got != len(registry) {
		t.Errorf("Tools() returned %d entries", got)
	}
	// Every input type decodes from an empty object.
	for _, name := range allNames {
		if _, err := DecodeInput(name, map[string]any{}); err != nil {
			t.Errorf("DecodeInput(%q, {}): %v", name, err)
		}
	}
}

func TestCanonical(t *testing.T) {
	for in, want := range map[string]string{
		"Task":              Agent,
		"KillShell":         TaskStop,
		"KillBash":          TaskStop,
		"ListMcpResources":  ListMcpResources,
		"ReadMcpResource":   ReadMcpResource,
		"mcp__srv__do_this": McpPrefix,
		"Bash":              Bash,
		"SomethingElse":     "SomethingElse",
	} {
		if got := Canonical(in); got != want {
			t.Errorf("Canonical(%q) = %q, want %q", in, got, want)
		}
	}
}

type sample struct {
	tool string
	json string
	want any // zero value of the expected concrete type
}

var inputSamples = []sample{
	{Bash, `{"command":"go test ./...","timeout":120000,"description":"Run the test suite","run_in_background":false}`, BashInput{}},
	{Read, `{"file_path":"/repo/main.go","offset":10,"limit":200}`, FileReadInput{}},
	{Read, `{"file_path":"/repo/spec.pdf","pages":"1-5"}`, FileReadInput{}},
	{Edit, `{"file_path":"/repo/main.go","old_string":"fmt.Println(x)","new_string":"log.Println(x)","replace_all":true}`, FileEditInput{}},
	{Write, `{"file_path":"/repo/README.md","content":"# Title\n"}`, FileWriteInput{}},
	{Glob, `{"pattern":"**/*.go","path":"/repo"}`, GlobInput{}},
	{Grep, `{"pattern":"func \\w+","path":"/repo","glob":"*.go","output_mode":"content","-B":2,"-A":3,"-n":true,"-i":false,"head_limit":50,"multiline":false}`, GrepInput{}},
	{Agent, `{"description":"Find callers","prompt":"Find all callers of Foo","subagent_type":"Explore","model":"haiku","run_in_background":true,"isolation":"worktree"}`, AgentInput{}},
	{Task, `{"description":"Legacy","prompt":"Do it"}`, AgentInput{}},
	{NotebookEdit, `{"notebook_path":"/repo/a.ipynb","cell_id":"c1","new_source":"print(1)","cell_type":"code","edit_mode":"insert"}`, NotebookEditInput{}},
	{TodoWrite, `{"todos":[{"content":"Write tests","status":"in_progress","activeForm":"Writing tests"},{"content":"Ship","status":"pending","activeForm":"Shipping"}]}`, TodoWriteInput{}},
	{WebFetch, `{"url":"https://go.dev/doc/","prompt":"Summarise the release notes"}`, WebFetchInput{}},
	{WebSearch, `{"query":"golang 1.27 release","allowed_domains":["go.dev"],"blocked_domains":["example.com"]}`, WebSearchInput{}},
	{AskUserQuestion, `{"questions":[{"question":"Which library?","header":"Library","options":[{"label":"A","description":"Use A"},{"label":"B","description":"Use B","preview":"b()"}],"multiSelect":false}],"answers":{"Which library?":"A"},"annotations":{"Which library?":{"notes":"fine"}},"metadata":{"source":"remember"}}`, AskUserQuestionInput{}},
	{ExitPlanMode, `{"plan":"1. Do X\n2. Do Y","allowedPrompts":[{"tool":"Bash","prompt":"run tests"}]}`, ExitPlanModeInput{}},
	{TaskStop, `{"task_id":"bash_1"}`, TaskStopInput{}},
	{KillShell, `{"shell_id":"bash_2"}`, TaskStopInput{}},
	{Monitor, `{"description":"Watch deploy","timeout_ms":300000,"command":"tail -f deploy.log"}`, MonitorInput{}},
	{Monitor, `{"description":"Watch socket","timeout_ms":60000,"ws":{"url":"wss://example.com/events","protocols":["v1"]}}`, MonitorInput{}},
	{EnterWorktree, `{"name":"feature/x"}`, EnterWorktreeInput{}},
	{ExitWorktree, `{"action":"remove","discard_changes":true}`, ExitWorktreeInput{}},
	{EnterPlanMode, `{}`, EnterPlanModeInput{}},
	{ListMcpResources, `{"server":"github"}`, ListMcpResourcesInput{}},
	{ReadMcpResource, `{"server":"github","uri":"repo://x"}`, ReadMcpResourceInput{}},
	{TaskUpdate, `{"taskId":"7","status":"deleted","metadata":{"k":null}}`, TaskUpdateInput{}},
	{CronCreate, `{"cron":"*/5 * * * *","prompt":"check CI","recurring":true,"durable":false}`, CronCreateInput{}},
	{ScheduleWakeup, `{"delaySeconds":120,"reason":"wait for CI","prompt":"/loop check","noop":true}`, ScheduleWakeupInput{}},
	{Workflow, `{"name":"review","args":{"pr":12}}`, WorkflowInput{}},
	{ProposeSkills, `{"proposals":[{"name":"deploy","kind":"new","description":"Deploy","skillMd":"---\n---\nbody"}]}`, ProposeSkillsInput{}},
	{Artifact, `{"action":"upload_asset","url":"https://claude.ai/artifact/1","file_paths":["a.png","b.png"]}`, ArtifactInput{}},
	{"mcp__github__create_issue", `{"title":"Bug","labels":["bug"],"body":{"nested":1}}`, McpInput{}},
}

var outputSamples = []sample{
	{Bash, `{"stdout":"ok\n","stderr":"","interrupted":false,"isImage":false,"noOutputExpected":false,"gitOperation":{"commit":{"sha":"abc123","kind":"committed","branch":"main"},"pr":{"number":42,"action":"created","url":"https://github.com/o/r/pull/42"}}}`, BashOutput{}},
	{Read, `{"type":"text","file":{"filePath":"/repo/main.go","content":"package main\n","numLines":1,"startLine":1,"totalLines":1}}`, FileReadOutputText{}},
	{Read, `{"type":"image","file":{"base64":"iVBORw0KGgo=","type":"image/png","originalSize":1024,"dimensions":{"originalWidth":64,"originalHeight":32}}}`, FileReadOutputImage{}},
	{Read, `{"type":"file_unchanged","file":{"filePath":"/repo/CLAUDE.md"},"source":"seeded"}`, FileReadOutputFileUnchanged{}},
	{Edit, `{"filePath":"/repo/main.go","oldString":"a","newString":"b","originalFile":"a\n","structuredPatch":[{"oldStart":1,"oldLines":1,"newStart":1,"newLines":1,"lines":["-a","+b"]}],"userModified":false,"replaceAll":false,"gitDiff":{"filename":"main.go","status":"modified","additions":1,"deletions":1,"changes":2,"patch":"@@","repository":"o/r"}}`, FileEditOutput{}},
	{Write, `{"type":"create","filePath":"/repo/new.go","content":"package x\n","structuredPatch":[],"originalFile":null}`, FileWriteOutput{}},
	{Glob, `{"durationMs":12,"numFiles":2,"filenames":["a.go","b.go"],"truncated":false,"totalMatches":2,"countIsComplete":true}`, GlobOutput{}},
	{Grep, `{"mode":"content","numFiles":1,"filenames":[],"content":"main.go:3:func main()","numLines":1,"appliedLimit":250}`, GrepOutput{}},
	{Agent, `{"status":"completed","agentId":"a1","content":[{"type":"text","text":"done"}],"totalToolUseCount":3,"totalDurationMs":4521.7,"totalTokens":900,"usage":{"input_tokens":500,"output_tokens":400,"cache_creation_input_tokens":null,"cache_read_input_tokens":128,"server_tool_use":null,"service_tier":"standard","cache_creation":{"ephemeral_1h_input_tokens":0,"ephemeral_5m_input_tokens":0}},"prompt":"Find callers"}`, AgentOutputCompleted{}},
	{Agent, `{"status":"async_launched","agentId":"a2","description":"bg","prompt":"p","outputFile":"/tmp/a2.out","canReadOutputFile":true}`, AgentOutputAsyncLaunched{}},
	{NotebookEdit, `{"new_source":"print(1)","cell_id":"c1","cell_type":"code","language":"python","edit_mode":"replace","notebook_path":"/repo/a.ipynb","original_file":"{}","updated_file":"{}"}`, NotebookEditOutput{}},
	{TodoWrite, `{"oldTodos":[],"newTodos":[{"content":"Ship","status":"completed","activeForm":"Shipping"}]}`, TodoWriteOutput{}},
	{WebFetch, `{"bytes":5120,"code":200,"codeText":"OK","result":"Summary","durationMs":830,"url":"https://go.dev/doc/"}`, WebFetchOutput{}},
	{WebSearch, `{"query":"go","results":[{"tool_use_id":"srvtoolu_1","content":[{"title":"Go","url":"https://go.dev"}]},"Go is a programming language."],"durationSeconds":1.25,"searchCount":1}`, WebSearchOutput{}},
	{AskUserQuestion, `{"questions":[{"question":"Which?","header":"Pick","options":[{"label":"A","description":"a"},{"label":"B","description":"b"}],"multiSelect":true}],"answers":{"Which?":"A, B"}}`, AskUserQuestionOutput{}},
	{ExitPlanMode, `{"plan":null,"isAgent":false,"filePath":"/repo/.claude/plan.md"}`, ExitPlanModeOutput{}},
	{TaskStop, `{"message":"stopped","task_id":"bash_1","task_type":"local_bash","command":"sleep 100"}`, TaskStopOutput{}},
	{Monitor, `{"taskId":"m1","timeoutMs":300000,"persistent":false}`, MonitorOutput{}},
	{ListMcpResources, `[{"uri":"repo://x","name":"x","server":"github"}]`, ListMcpResourcesOutput{}},
	{TaskGet, `{"task":null}`, TaskGetOutput{}},
	{Projects, `{"method":"project_read","path":"docs/a.md","created_at":null}`, ProjectsOutputProjectRead{}},
	{Artifact, `{"watches":[{"url":"u1","task_id":"t","since":1700000000000,"explicit":true,"connected":true,"token_expires_at":1700000600000},{"url":"u2","rail":"durable_wake","trigger_id":"tr","since":"2026-01-01T00:00:00Z"}]}`, ArtifactOutput{}},
	{"mcp__srv__tool", `"plain text result"`, McpOutput{}},
	{"mcp__srv__tool", `[{"type":"text","text":"hi"},{"type":"image","data":"AA==","mimeType":"image/png"}]`, McpOutput{}},
	{"mcp__srv__tool", `{"structured":{"ok":true}}`, McpOutput{}},
}

// jsonEqual reports whether a and b are the same JSON value.
func jsonEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatalf("unmarshal %s: %v", a, err)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatalf("unmarshal %s: %v", b, err)
	}
	return reflect.DeepEqual(x, y)
}

func TestInputRoundTrip(t *testing.T) {
	for _, s := range inputSamples {
		t.Run(s.tool, func(t *testing.T) {
			var raw map[string]any
			if err := json.Unmarshal([]byte(s.json), &raw); err != nil {
				t.Fatal(err)
			}
			v, err := DecodeInput(s.tool, raw)
			if err != nil {
				t.Fatal(err)
			}
			if reflect.TypeOf(v) != reflect.TypeOf(s.want) {
				t.Fatalf("DecodeInput returned %T, want %T", v, s.want)
			}
			back, err := ToMap(v)
			if err != nil {
				t.Fatal(err)
			}
			got, _ := json.Marshal(back)
			if !jsonEqual(t, got, []byte(s.json)) {
				t.Errorf("round trip mismatch:\n got %s\nwant %s", got, s.json)
			}
		})
	}
}

func TestOutputRoundTrip(t *testing.T) {
	for _, s := range outputSamples {
		t.Run(s.tool, func(t *testing.T) {
			// Exercise both raw JSON and the decoded-any form the claude
			// package stores in UserMessage.ToolUseResult.
			var generic any
			if err := json.Unmarshal([]byte(s.json), &generic); err != nil {
				t.Fatal(err)
			}
			for _, in := range []any{json.RawMessage(s.json), generic} {
				v, err := DecodeOutput(s.tool, in)
				if err != nil {
					t.Fatal(err)
				}
				if reflect.TypeOf(v) != reflect.TypeOf(s.want) {
					t.Fatalf("DecodeOutput returned %T, want %T", v, s.want)
				}
				got, err := json.Marshal(v)
				if err != nil {
					t.Fatal(err)
				}
				if !jsonEqual(t, got, []byte(s.json)) {
					t.Errorf("round trip mismatch:\n got %s\nwant %s", got, s.json)
				}
			}
		})
	}
}

func TestTypedFields(t *testing.T) {
	v, err := DecodeInput(Grep, map[string]any{"pattern": "x", "-C": 2.0, "-i": true, "output_mode": "count"})
	if err != nil {
		t.Fatal(err)
	}
	g := v.(GrepInput)
	if g.ContextAlias == nil || *g.ContextAlias != 2 || g.CaseInsensitive == nil || !*g.CaseInsensitive || g.OutputMode != GrepOutputModeCount {
		t.Errorf("unexpected GrepInput %+v", g)
	}

	out, err := DecodeOutput(Agent, json.RawMessage(outputSamples[8].json))
	if err != nil {
		t.Fatal(err)
	}
	c := out.(AgentOutputCompleted)
	if c.Usage.CacheCreationInputTokens != nil || *c.Usage.CacheReadInputTokens != 128 || c.TotalDurationMs != 4521.7 {
		t.Errorf("unexpected AgentOutputCompleted %+v", c)
	}

	ws, err := DecodeOutput(WebSearch, json.RawMessage(outputSamples[13].json))
	if err != nil {
		t.Fatal(err)
	}
	res := ws.(WebSearchOutput).Results
	if len(res) != 2 || res[0].Object == nil || res[0].Object.Content[0].URL != "https://go.dev" || res[1].String == nil {
		t.Errorf("unexpected WebSearch results %+v", res)
	}

	art, err := DecodeOutput(Artifact, json.RawMessage(outputSamples[21].json))
	if err != nil {
		t.Fatal(err)
	}
	w := art.(ArtifactOutput).Watches
	if *w[0].Since.Number != 1700000000000 || *w[1].Since.String != "2026-01-01T00:00:00Z" || w[1].Rail != ArtifactWatchEntryRailDurableWake {
		t.Errorf("unexpected watches %+v", w)
	}

	mcp, err := DecodeOutput("mcp__srv__tool", json.RawMessage(outputSamples[23].json))
	if err != nil {
		t.Fatal(err)
	}
	blocks := mcp.(McpOutput).Array
	if len(blocks) != 2 || blocks[1].Type != "image" || blocks[1].Extra["mimeType"] != "image/png" {
		t.Errorf("unexpected MCP blocks %+v", blocks)
	}
}

func TestExtraPreserved(t *testing.T) {
	in, err := As[ExitPlanModeInput](map[string]any{"plan": "do it", "planFilePath": "/p.md"})
	if err != nil {
		t.Fatal(err)
	}
	if in.Extra["plan"] != "do it" || in.Extra["planFilePath"] != "/p.md" || in.AllowedPrompts != nil {
		t.Fatalf("Extra = %v", in.Extra)
	}
	b, err := json.Marshal(ExitPlanModeInput{})
	if err != nil || string(b) != "{}" {
		t.Fatalf("empty marshal = %s, %v", b, err)
	}
	b, err = json.Marshal(ExitPlanModeInput{Extra: map[string]any{"b": 1, "a": "x", "allowedPrompts": "ignored"}})
	if err != nil || string(b) != `{"a":"x","b":1}` {
		t.Fatalf("extra-only marshal = %s, %v", b, err)
	}
}

func TestUpdatedInputRoundTrip(t *testing.T) {
	// The CanUseTool pattern from the package documentation.
	input := map[string]any{"command": "sleep 1", "description": "Wait"}
	in, err := As[BashInput](input)
	if err != nil {
		t.Fatal(err)
	}
	in.Timeout = new(60_000)
	updated, err := ToMap(in)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"command": "sleep 1", "description": "Wait", "timeout": 60000.0}
	if !reflect.DeepEqual(updated, want) {
		t.Errorf("ToMap = %v, want %v", updated, want)
	}
}

func TestErrors(t *testing.T) {
	if _, err := DecodeInput("NoSuchTool", nil); !errors.Is(err, ErrUnknownTool) {
		t.Errorf("DecodeInput unknown: %v", err)
	}
	if _, err := DecodeOutput("NoSuchTool", nil); !errors.Is(err, ErrUnknownTool) {
		t.Errorf("DecodeOutput unknown: %v", err)
	}
	if _, err := DecodeInput(Bash, map[string]any{"command": 12}); err == nil {
		t.Error("DecodeInput accepted a number for a string field")
	}
	if _, err := DecodeOutput(Agent, map[string]any{"status": "teleported"}); err == nil {
		t.Error("DecodeOutput accepted an unknown AgentOutput variant")
	}
	if _, err := DecodeOutput("mcp__a__b", 12); err == nil {
		t.Error("McpOutput accepted a number")
	}
	if _, err := ToMap("not an object"); err == nil {
		t.Error("ToMap accepted a string")
	}
	// Unknown properties are ignored rather than rejected.
	v, err := DecodeInput(Read, map[string]any{"file_path": "/x", "future_option": true})
	if err != nil || v.(FileReadInput).FilePath != "/x" {
		t.Errorf("DecodeInput with unknown property = %v, %v", v, err)
	}
}

func TestMcpInputIsTheMap(t *testing.T) {
	raw := map[string]any{"a": 1.0}
	v, err := DecodeInput("mcp__x__y", raw)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(v, McpInput(raw)) {
		t.Errorf("got %v", v)
	}
}

func TestJSONValueKind(t *testing.T) {
	for in, want := range map[string]valueKind{
		` "x"`: kindString, `-1`: kindNumber, `true`: kindBool, `[]`: kindArray,
		"\n{}": kindObject, `null`: kindNull, ``: kindInvalid, `?`: kindInvalid,
	} {
		if got := jsonValueKind([]byte(in)); got != want {
			t.Errorf("jsonValueKind(%q) = %v, want %v", in, got, want)
		}
	}
}
