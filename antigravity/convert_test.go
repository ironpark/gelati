package antigravity

import (
	"reflect"
	"testing"

	"github.com/ironpark/gelati/antigravity/internal/wire"
)

func TestStepFromUpdateBasics(t *testing.T) {
	s := stepFromUpdate(&wire.StepUpdate{
		StepIndex: new(uint32(1)),
		Text:      new("Hello world"),
		State:     new(wire.StepUpdateStateActive),
		Source:    new(wire.StepUpdateSourceModel),
		Target:    new(wire.StepUpdateTargetUser),
	})
	if s.ID != "1" || s.Content != "Hello world" || s.Status != StepStatusActive || s.Source != StepSourceModel ||
		s.Target != StepTargetUser || s.Type != StepTypeTextResponse {
		t.Fatalf("step %+v", s)
	}
	if s.Thinking != "" || s.ContentDelta != "" || s.ThinkingDelta != "" {
		t.Fatalf("unexpected deltas %+v", s)
	}

	s = stepFromUpdate(&wire.StepUpdate{
		Thinking: new("pondering"), ThinkingDelta: new("ing"), TextDelta: new("Hi"),
		Target: new(wire.StepUpdateTargetModel),
	})
	if s.Type != StepTypeThinking || s.ThinkingDelta != "ing" || s.ContentDelta != "Hi" || s.Target != StepTargetUnknown || s.Source != StepSourceUnknown {
		t.Fatalf("thinking step %+v", s)
	}

	s = stepFromUpdate(&wire.StepUpdate{
		StepIndex: new(uint32(5)), TrajectoryID: new("child_traj_123"), ParentTrajectoryID: new("parent_traj_456"),
		State: new(wire.StepUpdateStateDone), Source: new(wire.StepUpdateSourceModel), Text: new("Task finished"),
	})
	if s.ID != "child_traj_123:5" || s.ParentTrajectoryID != "parent_traj_456" || s.StepIndex != 5 {
		t.Fatalf("subagent step %+v", s)
	}
}

func TestStepIsCompleteResponse(t *testing.T) {
	base := func() *wire.StepUpdate {
		return &wire.StepUpdate{
			Source: new(wire.StepUpdateSourceModel), State: new(wire.StepUpdateStateDone),
			Text: new("Here is my answer."), Target: new(wire.StepUpdateTargetUser),
		}
	}
	if !stepFromUpdate(base()).IsCompleteResponse {
		t.Fatal("complete response not detected")
	}
	for name, mod := range map[string]func(*wire.StepUpdate){
		"user source":   func(s *wire.StepUpdate) { s.Source = new(wire.StepUpdateSourceUser) },
		"active":        func(s *wire.StepUpdate) { s.State = new(wire.StepUpdateStateActive) },
		"no text":       func(s *wire.StepUpdate) { s.Text = nil },
		"error state":   func(s *wire.StepUpdate) { s.State = new(wire.StepUpdateStateError) },
		"environment":   func(s *wire.StepUpdate) { s.Target = new(wire.StepUpdateTargetEnvironment) },
		"target unset":  func(s *wire.StepUpdate) { s.Target = nil },
		"empty message": func(s *wire.StepUpdate) { s.Text = new("") },
	} {
		su := base()
		mod(su)
		if stepFromUpdate(su).IsCompleteResponse {
			t.Errorf("%s: reported as complete response", name)
		}
	}
}

func TestStepBuiltinToolCalls(t *testing.T) {
	s := stepFromUpdate(&wire.StepUpdate{
		Source: new(wire.StepUpdateSourceModel), State: new(wire.StepUpdateStateActive),
		ViewFile: &wire.ActionViewFile{FilePath: new("/foo")},
	})
	if s.Type != StepTypeToolCall || len(s.ToolCalls) != 1 {
		t.Fatalf("step %+v", s)
	}
	tc := s.ToolCalls[0]
	if tc.Name != "view_file" || !reflect.DeepEqual(tc.Args, map[string]any{"file_path": "/foo"}) || tc.StepID != "0" || tc.ID != "0" || tc.CanonicalPath != "/foo" {
		t.Fatalf("tool call %+v", tc)
	}

	s = stepFromUpdate(&wire.StepUpdate{
		Source: new(wire.StepUpdateSourceModel), State: new(wire.StepUpdateStateDone),
		GenerateImage: &wire.ActionGenerateImage{
			Prompt: new("A sunset"), ImageName: new("sunset"), AspectRatio: new("16:9"),
			OutputPath: new("file:///tmp/sunset_123.png"),
		},
	})
	want := map[string]any{"prompt": "A sunset", "image_name": "sunset", "aspect_ratio": "16:9", "output_path": "/tmp/sunset_123.png"}
	if tc := s.ToolCalls[0]; tc.Name != "generate_image" || !reflect.DeepEqual(tc.Args, want) || tc.CanonicalPath != "/tmp/sunset_123.png" {
		t.Fatalf("generate_image call %+v", tc)
	}

	s = stepFromUpdate(&wire.StepUpdate{
		StepIndex: new(uint32(1)), TrajectoryID: new("traj_1"), State: new(wire.StepUpdateStateWaitingForUser),
		CreateFile: &wire.ActionCreateFile{FilePath: new("cns://el-d/home/user/workspace/kittens.md")},
	})
	if tc := s.ToolCalls[0]; tc.Args["file_path"] != "/cns/el-d/home/user/workspace/kittens.md" || tc.CanonicalPath != "/cns/el-d/home/user/workspace/kittens.md" || tc.ID != "traj_1:1" {
		t.Fatalf("cns path call %+v", tc)
	}

	// A failed builtin step keeps its tool call and the top-level error.
	s = stepFromUpdate(&wire.StepUpdate{
		TrajectoryID: new("traj_123"), StepIndex: new(uint32(2)),
		Source: new(wire.StepUpdateSourceModel), Target: new(wire.StepUpdateTargetEnvironment),
		State: new(wire.StepUpdateStateError), Text: new("View missing.txt"), TextDelta: new(""),
		ErrorMessage: new("Cannot view file file:///tmp/missing.txt which does not exist."),
		ViewFile:     &wire.ActionViewFile{FilePath: new("file:///tmp/missing.txt"), StartLine: new(uint32(0)), EndLine: new(uint32(799))},
	})
	if s.Type != StepTypeToolCall || s.Status != StepStatusError || s.Target != StepTargetEnvironment ||
		s.ToolCalls[0].CanonicalPath != "/tmp/missing.txt" || s.Error != "Cannot view file file:///tmp/missing.txt which does not exist." {
		t.Fatalf("failed step %+v", s)
	}
	if got := s.ToolCalls[0].Args["end_line"]; got != 799.0 {
		t.Fatalf("end_line = %#v", got)
	}

	// invoke_subagent is the start_subagent tool.
	s = stepFromUpdate(&wire.StepUpdate{Text: new("Invoking subagent"), InvokeSubagent: &wire.ActionInvokeSubagent{}})
	if s.Type != StepTypeToolCall || s.ToolCalls[0].Name != "start_subagent" || len(s.ToolCalls[0].Args) != 0 {
		t.Fatalf("subagent step %+v", s)
	}
}

func TestStepCustomAndMCPToolCalls(t *testing.T) {
	s := stepFromUpdate(&wire.StepUpdate{
		Source: new(wire.StepUpdateSourceModel), State: new(wire.StepUpdateStateDone),
		CustomTool: &wire.ActionCustomTool{
			ToolCall:     &wire.ToolCall{ID: new("call_1"), Name: new("my_custom_tool"), ArgumentsJSON: new(`{"arg1": "val1", "file_path": "file:///foo"}`)},
			ToolResponse: &wire.ToolResponse{ID: new("my_custom_tool"), ResponseJSON: new(`{"result": "ok"}`)},
		},
	})
	if tc := s.ToolCalls[0]; s.Type != StepTypeToolCall || tc.Name != "my_custom_tool" || tc.ID != "call_1" ||
		!reflect.DeepEqual(tc.Args, map[string]any{"arg1": "val1", "file_path": "/foo"}) || tc.CanonicalPath != "/foo" {
		t.Fatalf("custom tool step %+v", s.ToolCalls[0])
	}

	s = stepFromUpdate(&wire.StepUpdate{
		TrajectoryID: new("traj_123"), StepIndex: new(uint32(5)),
		CustomTool: &wire.ActionCustomTool{ToolCall: &wire.ToolCall{Name: new("my_custom_tool"), ArgumentsJSON: new("{}")}},
	})
	if s.ToolCalls[0].ID != "traj_123:5" {
		t.Fatalf("fallback id %q", s.ToolCalls[0].ID)
	}

	for _, args := range []string{"{not json", "42", "null", `"s"`, "[1, 2]"} {
		s = stepFromUpdate(&wire.StepUpdate{CustomTool: &wire.ActionCustomTool{ToolCall: &wire.ToolCall{Name: new("t"), ArgumentsJSON: new(args)}}})
		if len(s.ToolCalls) != 1 || len(s.ToolCalls[0].Args) != 0 {
			t.Errorf("arguments %q: %+v", args, s.ToolCalls)
		}
	}

	s = stepFromUpdate(&wire.StepUpdate{MCPTool: &wire.ActionMCPTool{
		ServerName: new("pirate_math"), ToolName: new("multiply"), ArgumentsJSON: new(`{"a": 5}`),
	}})
	if tc := s.ToolCalls[0]; s.Type != StepTypeToolCall || tc.Name != "multiply" || tc.ServerName != "pirate_math" || tc.Args["a"] != 5.0 {
		t.Fatalf("mcp step %+v", tc)
	}
}

func TestStepStructuredOutput(t *testing.T) {
	s := stepFromUpdate(&wire.StepUpdate{
		Source: new(wire.StepUpdateSourceModel), State: new(wire.StepUpdateStateDone),
		Finish: &wire.ActionFinish{OutputString: new(`{"total_revenue": 386.0, "top_selling_product": "Widget A"}`)},
	})
	if s.Type != StepTypeFinish || !reflect.DeepEqual(s.StructuredOutput, map[string]any{"total_revenue": 386.0, "top_selling_product": "Widget A"}) {
		t.Fatalf("finish step %+v", s)
	}
	if len(s.ToolCalls) != 1 || s.ToolCalls[0].Name != "finish" {
		t.Fatalf("finish tool call %+v", s.ToolCalls)
	}
	s = stepFromUpdate(&wire.StepUpdate{Finish: &wire.ActionFinish{OutputString: new(`{"total_revenue": 386.0, "top_selling_product": }`)}})
	if s.StructuredOutput != nil {
		t.Fatalf("invalid JSON parsed: %v", s.StructuredOutput)
	}
	if s := stepFromUpdate(&wire.StepUpdate{Compaction: &wire.ActionCompaction{}, Text: new("x")}); s.Type != StepTypeCompaction {
		t.Fatalf("compaction type %s", s.Type)
	}
}

func TestMakeStepIDAndPaths(t *testing.T) {
	if makeStepID("traj_1", 5) != "traj_1:5" || makeStepID("", 5) != "5" {
		t.Fatal("makeStepID")
	}
	for in, want := range map[string]string{
		"file:///dev/shm/workspace/foo.py":          "/dev/shm/workspace/foo.py",
		"file:///home/user/my%20file.py":            "/home/user/my file.py",
		"cns://el-d/home/user/workspace/kittens.md": "/cns/el-d/home/user/workspace/kittens.md",
		"/plain/path":   "/plain/path",
		"relative/path": "relative/path",
	} {
		if got := normalizeWirePath(in); got != want {
			t.Errorf("normalizeWirePath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseUsageAndStopReason(t *testing.T) {
	u := parseUsage(&wire.UsageMetadata{
		PromptTokenCount: new(wire.Uint64(100)), CachedContentTokenCount: new(wire.Uint64(50)),
		CandidatesTokenCount: new(wire.Uint64(75)), ThoughtsTokenCount: new(wire.Uint64(25)),
		TotalTokenCount: new(wire.Uint64(250)), ServiceTier: new("priority"),
	})
	if val(u.PromptTokenCount) != 100 || val(u.CachedContentTokenCount) != 50 || val(u.CandidatesTokenCount) != 75 ||
		val(u.ThoughtsTokenCount) != 25 || val(u.TotalTokenCount) != 250 || u.ServiceTier != ServiceTierPriority {
		t.Fatalf("usage %+v", u)
	}
	if u := parseUsage(&wire.UsageMetadata{}); u.PromptTokenCount != nil || u.TotalTokenCount != nil || u.ServiceTier != "" {
		t.Fatalf("empty usage %+v", u)
	}
	if u := parseUsage(&wire.UsageMetadata{TotalTokenCount: new(wire.Uint64(250)), ServiceTier: new("PROVISIONED_THROUGHPUT")}); u.ServiceTier != "" || val(u.TotalTokenCount) != 250 {
		t.Fatalf("unknown tier usage %+v", u)
	}
	for in, want := range map[wire.TrajectoryStateUpdateStopReason]StopReason{
		wire.TrajectoryStateUpdateStopReasonMaxModelCallsExceeded:   StopReasonMaxModelCallsExceeded,
		wire.TrajectoryStateUpdateStopReasonMaxToolCallsExceeded:    StopReasonMaxToolCallsExceeded,
		wire.TrajectoryStateUpdateStopReasonMaxInputTokensExceeded:  StopReasonMaxInputTokensExceeded,
		wire.TrajectoryStateUpdateStopReasonMaxOutputTokensExceeded: StopReasonMaxOutputTokensExceeded,
		wire.TrajectoryStateUpdateStopReasonMaxTotalTokensExceeded:  StopReasonMaxTotalTokensExceeded,
		wire.TrajectoryStateUpdateStopReasonQuotaExhausted:          StopReasonQuotaExhausted,
		wire.TrajectoryStateUpdateStopReasonUnspecified:             StopReasonUnspecified,
		"99": StopReasonUnspecified,
	} {
		if got := parseStopReason(in); got != want {
			t.Errorf("parseStopReason(%s) = %s, want %s", in, got, want)
		}
	}
}

func TestParseInitializeResponse(t *testing.T) {
	r := parseInitializeResponse(&wire.InitializeConversationResponse{
		History:         []*wire.StepUpdate{{StepIndex: new(uint32(1)), Text: new("old")}},
		CumulativeUsage: &wire.UsageMetadata{TotalTokenCount: new(wire.Uint64(7))},
		TrajectoryUsage: []*wire.TrajectoryUsageEntry{{TrajectoryID: new("t"), Usage: &wire.UsageMetadata{}}, {Usage: &wire.UsageMetadata{}}},
		SandboxStatus:   &wire.SandboxStatus{Available: new(false), UnavailableReason: new("no user namespaces")},
	})
	if len(r.history) != 1 || r.history[0].Content != "old" || val(r.cumulativeUsage.TotalTokenCount) != 7 || len(r.trajectoryUsages) != 1 {
		t.Fatalf("result %+v", r)
	}
	if r.sandbox == nil || r.sandbox.Available || r.sandbox.UnavailableReason != "no user namespaces" {
		t.Fatalf("sandbox %+v", r.sandbox)
	}
	if r := parseInitializeResponse(&wire.InitializeConversationResponse{SandboxStatus: &wire.SandboxStatus{Available: new(true)}}); !r.sandbox.Available || r.cumulativeUsage != nil {
		t.Fatalf("available sandbox %+v", r)
	}
	if r := parseInitializeResponse(&wire.InitializeConversationResponse{}); r.sandbox != nil {
		t.Fatalf("unset sandbox %+v", r.sandbox)
	}
}

func TestSanitizePrompt(t *testing.T) {
	for in, want := range map[string]string{
		"Hello\x00World\x07!\x7f\u0080": "Hello World !  ",
		"Line1\nLine2\r\tTab":           "Line1\nLine2\r\tTab",
		"":                              "",
		"\x00\x00":                      " ",
	} {
		if got := sanitizePrompt(in); got != want {
			t.Errorf("sanitizePrompt(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestContentFromUserInput(t *testing.T) {
	got := contentFromUserInput(&wire.UserInput{Parts: []*wire.UserInputPart{
		{Text: new("hello")},
		{SlashCommand: &wire.UserInputSlashCommand{Name: new("plan")}},
		{SlashCommand: &wire.UserInputSlashCommand{Name: new("unknown")}},
		{Media: &wire.UserInputMedia{MimeType: new("image/png"), Data: []byte("x"), Description: new("d")}},
		{Media: &wire.UserInputMedia{MimeType: new("image/gif"), Data: []byte("x")}},
	}})
	want := []Content{Text("hello"), SlashCommandPlan, &Image{Data: []byte("x"), MIMEType: "image/png", Description: "d"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("content %#v", got)
	}
}

func TestStructOfArgsFallsBackToString(t *testing.T) {
	type custom struct{ C chan int }
	s := structOfArgs(map[string]any{"custom": custom{}, "str_val": "hello"})
	m := s.AsMap()
	if m["str_val"] != "hello" {
		t.Fatalf("str_val %v", m["str_val"])
	}
	if _, ok := m["custom"].(string); !ok {
		t.Fatalf("custom %#v, want its string form", m["custom"])
	}
}
