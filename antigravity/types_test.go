package antigravity

import (
	"context"
	"errors"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
)

func i64(v int64) *int64 { return &v }

func TestUsageAdd(t *testing.T) {
	u1 := UsageMetadata{i64(100), i64(50), i64(30), i64(20), i64(150), ""}
	u2 := UsageMetadata{i64(200), i64(10), i64(40), i64(5), i64(245), ""}
	if got := u1.Add(u2); !reflect.DeepEqual(got, UsageMetadata{i64(300), i64(60), i64(70), i64(25), i64(395), ""}) {
		t.Fatalf("Add = %+v", got)
	}
	got := UsageMetadata{PromptTokenCount: i64(100)}.Add(UsageMetadata{CandidatesTokenCount: i64(50)})
	if !reflect.DeepEqual(got, UsageMetadata{i64(100), i64(0), i64(50), i64(0), i64(0), ""}) {
		t.Fatalf("Add with nil = %+v", got)
	}
	tiers := []struct {
		a, b, want ServiceTier
	}{
		{ServiceTierPriority, ServiceTierPriority, ServiceTierPriority},
		{ServiceTierFlex, ServiceTierFlex, ServiceTierFlex},
		{ServiceTierPriority, "", ServiceTierPriority},
		{"", ServiceTierFlex, ServiceTierFlex},
		{ServiceTierPriority, ServiceTierFlex, ServiceTierStandard},
		{ServiceTierFlex, ServiceTierPriority, ServiceTierStandard},
		{ServiceTierPriority, ServiceTierStandard, ServiceTierStandard},
	}
	for _, tc := range tiers {
		if got := (UsageMetadata{ServiceTier: tc.a}).Add(UsageMetadata{ServiceTier: tc.b}).ServiceTier; got != tc.want {
			t.Errorf("%q + %q tier = %q, want %q", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestUsageSub(t *testing.T) {
	u1 := UsageMetadata{i64(300), i64(60), i64(70), i64(25), i64(395), ""}
	u2 := UsageMetadata{i64(100), i64(50), i64(30), i64(20), i64(150), ""}
	if got := u1.Sub(u2); !reflect.DeepEqual(got, UsageMetadata{i64(200), i64(10), i64(40), i64(5), i64(245), ""}) {
		t.Fatalf("Sub = %+v", got)
	}
	got := UsageMetadata{PromptTokenCount: i64(100)}.Sub(UsageMetadata{CandidatesTokenCount: i64(50)})
	if val(got.CandidatesTokenCount) != -50 || val(got.TotalTokenCount) != 0 || got.TotalTokenCount == nil {
		t.Fatalf("Sub with nil = %+v", got)
	}
	for _, tc := range []struct{ a, b, want ServiceTier }{
		{ServiceTierPriority, ServiceTierPriority, ServiceTierPriority},
		{ServiceTierPriority, "", ServiceTierPriority},
		{"", ServiceTierFlex, ServiceTierFlex},
		{ServiceTierPriority, ServiceTierFlex, ServiceTierPriority},
		{ServiceTierFlex, ServiceTierPriority, ServiceTierFlex},
		{"", "", ""},
	} {
		if got := (UsageMetadata{ServiceTier: tc.a}).Sub(UsageMetadata{ServiceTier: tc.b}).ServiceTier; got != tc.want {
			t.Errorf("%q - %q tier = %q, want %q", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestUsageSumAndScale(t *testing.T) {
	u1 := UsageMetadata{i64(100), i64(50), i64(30), i64(20), i64(150), ServiceTierPriority}
	u2 := UsageMetadata{i64(200), i64(10), i64(40), i64(5), i64(245), ServiceTierPriority}
	if got := SumUsage(u1, u2); !reflect.DeepEqual(got, u1.Add(u2)) {
		t.Fatalf("SumUsage = %+v", got)
	}
	single := SumUsage(u1)
	if !reflect.DeepEqual(single, u1) || single.PromptTokenCount == u1.PromptTokenCount {
		t.Fatal("SumUsage of one must be an equal copy")
	}
	if got := SumUsage(); !reflect.DeepEqual(got, UsageMetadata{}) {
		t.Fatalf("SumUsage() = %+v", got)
	}

	u := UsageMetadata{i64(100), i64(50), i64(31), i64(21), i64(152), ServiceTierPriority}
	scaled, err := u.Scale(2)
	if err != nil || !reflect.DeepEqual(scaled, UsageMetadata{i64(200), i64(100), i64(62), i64(42), i64(304), ServiceTierPriority}) {
		t.Fatalf("Scale(2) = %+v, %v", scaled, err)
	}
	// Round half to even.
	scaled, _ = u.Scale(1.5)
	if !reflect.DeepEqual(scaled, UsageMetadata{i64(150), i64(75), i64(46), i64(32), i64(228), ServiceTierPriority}) {
		t.Fatalf("Scale(1.5) = %+v", scaled)
	}
	scaled, _ = UsageMetadata{PromptTokenCount: i64(100)}.Scale(2)
	if val(scaled.PromptTokenCount) != 200 || scaled.CachedContentTokenCount != nil || scaled.TotalTokenCount != nil {
		t.Fatalf("Scale keeps nil fields: %+v", scaled)
	}
	for _, f := range []float64{-1, math.NaN(), math.Inf(1)} {
		if _, err := u.Scale(f); err == nil {
			t.Errorf("Scale(%v) accepted", f)
		}
	}
	c := u.Clone()
	*c.PromptTokenCount = 1
	if val(u.PromptTokenCount) != 100 {
		t.Fatal("Clone shares fields")
	}
}

func TestBuiltinToolPresets(t *testing.T) {
	if got := DeprecatedTools(); !reflect.DeepEqual(got, []BuiltinTool{BuiltinListDir, BuiltinSearchDir, BuiltinFindFile}) {
		t.Fatalf("deprecated %v", got)
	}
	if got := ReadOnlyTools(); !reflect.DeepEqual(got, []BuiltinTool{BuiltinViewFile, BuiltinReadURLContent, BuiltinSchedule, BuiltinFinish}) {
		t.Fatalf("read-only %v", got)
	}
	if len(AllTools()) != 14 || len(NoTools()) != 0 || NoTools() == nil {
		t.Fatal("all/none")
	}
	if got := MinimalTools(); !reflect.DeepEqual(got, []BuiltinTool{BuiltinRunCommand, BuiltinViewFile, BuiltinCreateFile, BuiltinEditFile}) {
		t.Fatalf("minimal %v", got)
	}
	def := DefaultTools()
	for _, excluded := range []BuiltinTool{BuiltinAskQuestion, BuiltinListDir, BuiltinSearchDir, BuiltinFindFile} {
		if slices.Contains(def, excluded) {
			t.Errorf("default includes %s", excluded)
		}
	}
	if len(def) != 10 {
		t.Errorf("default %v", def)
	}
	// Every tool is classified as read-only, deprecated, or not.
	for _, tool := range NondestructiveTools() {
		if !slices.Contains(AllTools(), tool) {
			t.Errorf("unknown nondestructive tool %s", tool)
		}
	}
}

func TestResolveActiveTools(t *testing.T) {
	set := func(tools ...BuiltinTool) map[BuiltinTool]bool {
		m := map[BuiltinTool]bool{}
		for _, t := range tools {
			m[t] = true
		}
		return m
	}
	if got := resolveActiveTools(nil, nil); !reflect.DeepEqual(got, set(DefaultTools()...)) {
		t.Fatalf("defaults %v", got)
	}
	if got := resolveActiveTools([]BuiltinTool{BuiltinViewFile}, nil); !reflect.DeepEqual(got, set(BuiltinViewFile)) {
		t.Fatalf("enabled %v", got)
	}
	got := resolveActiveTools(nil, []BuiltinTool{BuiltinRunCommand})
	if got[BuiltinRunCommand] || !got[BuiltinViewFile] || got[BuiltinAskQuestion] {
		t.Fatalf("disabled %v", got)
	}
	if got := resolveActiveTools([]BuiltinTool{}, nil); len(got) != 0 {
		t.Fatalf("empty enabled %v", got)
	}
}

func TestCapabilitiesValidation(t *testing.T) {
	logger := quietLogger()
	for name, tc := range map[string]struct {
		caps CapabilitiesConfig
		ok   bool
	}{
		"defaults":                     {CapabilitiesConfig{}, true},
		"enabled":                      {CapabilitiesConfig{EnabledTools: []BuiltinTool{BuiltinViewFile}}, true},
		"exclusive":                    {CapabilitiesConfig{EnabledTools: []BuiltinTool{}, DisabledTools: []BuiltinTool{}}, false},
		"depth and allowed":            {CapabilitiesConfig{MaxSubagentDepth: 3, AllowedSubagents: []string{"r"}}, true},
		"negative depth":               {CapabilitiesConfig{MaxSubagentDepth: -1}, false},
		"depth with subagents off":     {CapabilitiesConfig{DisableSubagents: true, MaxSubagentDepth: 1}, false},
		"allowed with subagents off":   {CapabilitiesConfig{DisableSubagents: true, AllowedSubagents: []string{}}, false},
		"depth with start disabled":    {CapabilitiesConfig{DisabledTools: []BuiltinTool{BuiltinStartSubagent}, MaxSubagentDepth: 2}, false},
		"allowed with start disabled":  {CapabilitiesConfig{DisabledTools: []BuiltinTool{BuiltinStartSubagent}, AllowedSubagents: []string{"x"}}, false},
		"depth without start enabled":  {CapabilitiesConfig{EnabledTools: []BuiltinTool{BuiltinViewFile}, MaxSubagentDepth: 2}, false},
		"allowed with start enabled":   {CapabilitiesConfig{EnabledTools: []BuiltinTool{BuiltinStartSubagent}, AllowedSubagents: []string{"x"}}, true},
		"negative timeout":             {CapabilitiesConfig{RunCommand: &RunCommandConfig{Timeout: -1}}, false},
		"truncation zero":              {CapabilitiesConfig{ToolOutputTruncation: &ToolOutputTruncationConfig{}}, true},
		"truncation negative":          {CapabilitiesConfig{ToolOutputTruncation: &ToolOutputTruncationConfig{MaxTokens: -1}}, false},
		"truncation overflow":          {CapabilitiesConfig{ToolOutputTruncation: &ToolOutputTruncationConfig{MaxTokens: math.MaxInt32 + 1}}, false},
		"negative compaction":          {CapabilitiesConfig{CompactionThreshold: -5}, false},
		"ask question not interactive": {CapabilitiesConfig{EnabledTools: []BuiltinTool{BuiltinAskQuestion}}, true},
	} {
		err := tc.caps.validate(logger)
		if (err == nil) != tc.ok {
			t.Errorf("%s: validate = %v", name, err)
		}
		if err != nil {
			if _, ok := errors.AsType[*ValidationError](err); !ok {
				t.Errorf("%s: error type %T", name, err)
			}
		}
	}
	for name, tc := range map[string]struct {
		caps SubagentCapabilities
		ok   bool
	}{
		"defaults":                    {SubagentCapabilities{}, true},
		"exclusive":                   {SubagentCapabilities{EnabledTools: []BuiltinTool{}, DisabledTools: []BuiltinTool{}}, false},
		"allowed with start enabled":  {SubagentCapabilities{EnabledTools: []BuiltinTool{BuiltinStartSubagent}, AllowedSubagents: []string{"x"}}, true},
		"allowed without start":       {SubagentCapabilities{EnabledTools: []BuiltinTool{BuiltinViewFile}, AllowedSubagents: []string{"x"}}, false},
		"allowed with start disabled": {SubagentCapabilities{DisabledTools: []BuiltinTool{BuiltinStartSubagent}, AllowedSubagents: []string{"x"}}, false},
	} {
		if err := tc.caps.validate(logger); (err == nil) != tc.ok {
			t.Errorf("subagent %s: validate = %v", name, err)
		}
	}
}

func TestAskQuestionWarning(t *testing.T) {
	var buf strings.Builder
	var mu sync.Mutex
	logger := slogTo(&buf, &mu)
	caps := CapabilitiesConfig{EnabledTools: []BuiltinTool{BuiltinAskQuestion}}
	_ = caps.validate(logger)
	if !strings.Contains(buf.String(), "not interactive") {
		t.Fatalf("no warning: %q", buf.String())
	}
	buf.Reset()
	caps.AgentBehavior = AgentBehaviorInteractive
	_ = caps.validate(logger)
	if buf.Len() != 0 {
		t.Fatalf("unexpected warning: %q", buf.String())
	}
}

func TestMCPServerValidation(t *testing.T) {
	for name, tc := range map[string]struct {
		s  MCPServer
		ok bool
	}{
		"stdio":        {&MCPStdioServer{Name: "my_server-1", Command: "node"}, true},
		"http":         {&MCPStreamableHTTPServer{Name: "web", URL: "http://localhost:8080/mcp"}, true},
		"empty name":   {&MCPStdioServer{Command: "node"}, false},
		"bad name":     {&MCPStdioServer{Name: "bad name!", Command: "node"}, false},
		"exclusive":    {&MCPStdioServer{Name: "s", EnabledTools: []string{"a"}, DisabledTools: []string{"b"}}, false},
		"filtering":    {&MCPStreamableHTTPServer{Name: "s", EnabledTools: []string{"a"}}, true},
		"nil instance": {nil, false},
	} {
		if err := validateMCPServer(tc.s); (err == nil) != tc.ok {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestRetryAndBudgetValidation(t *testing.T) {
	b := BenchmarkRetryConfig()
	if *b.APIRetry.MaxRetries != math.MaxUint32 || *b.APIRetry.InitialSleepDurationMs != 1000 || b.ModelOutputRetry != nil {
		t.Fatalf("benchmark %+v", b.APIRetry)
	}
	if err := (&RetryConfig{APIRetry: &ModelAPIRetryConfig{ExponentialMultiplier: new(-1.0)}}).validate(); err == nil {
		t.Error("negative multiplier accepted")
	}
	if err := (&RetryConfig{APIRetry: &ModelAPIRetryConfig{JitterRange: new(math.NaN())}}).validate(); err == nil {
		t.Error("NaN jitter accepted")
	}
	for _, bc := range []BudgetConfig{
		{MaxModelCalls: -1},
		{MaxToolCalls: math.MaxInt32 + 1},
		{MaxTotalTokens: -5},
		{Scope: "SOMETIMES"},
	} {
		if err := bc.validate(); err == nil {
			t.Errorf("budget %+v accepted", bc)
		}
	}
	if err := (&BudgetConfig{MaxModelCalls: 5, Scope: BudgetScopeForwardLooking}).validate(); err != nil {
		t.Error(err)
	}
}

func TestMediaTypes(t *testing.T) {
	if _, err := NewImage([]byte("x"), "image/png", "logo"); err != nil {
		t.Fatal(err)
	}
	if _, err := NewImage([]byte("x"), "application/pdf", ""); err == nil || !strings.Contains(err.Error(), "Unsupported Image MIME type") {
		t.Fatalf("image with pdf mime: %v", err)
	}
	if _, err := NewDocument(nil, "text/csv", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := NewAudio(nil, "audio/x-wav", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := NewVideo(nil, "video/quicktime", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := NewVideo(nil, "video/x-msvideo", ""); err == nil {
		t.Fatal("unsupported video accepted")
	}
	for mime, kind := range map[string]string{"image/webp": "Image", "application/json": "Document", "audio/flac": "Audio", "video/webm": "Video"} {
		m, err := FromBytes([]byte("x"), mime, "d")
		if err != nil || m.kind() != kind {
			t.Errorf("FromBytes(%s) = %v, %v", mime, m, err)
		}
	}
	if _, err := FromBytes(nil, "image/gif", ""); err == nil || !strings.Contains(err.Error(), "Unsupported MIME type: 'image/gif'") {
		t.Fatalf("gif: %v", err)
	}
}

func TestMediaFromFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("data"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	m, err := FromFile(write("pic.PNG"), "desc")
	if err != nil {
		t.Fatal(err)
	}
	img, ok := m.(*Image)
	if !ok || img.MIMEType != "image/png" || string(img.Data) != "data" || img.Description != "desc" {
		t.Fatalf("FromFile = %#v", m)
	}
	if d, err := DocumentFromFile(write("doc.pdf"), ""); err != nil || d.MIMEType != "application/pdf" {
		t.Fatalf("DocumentFromFile = %v, %v", d, err)
	}
	if a, err := AudioFromFile(write("a.mp3"), ""); err != nil || a.MIMEType != "audio/mpeg" {
		t.Fatalf("AudioFromFile = %v, %v", a, err)
	}
	if v, err := VideoFromFile(write("v.mp4"), ""); err != nil || v.MIMEType != "video/mp4" {
		t.Fatalf("VideoFromFile = %v, %v", v, err)
	}
	if _, err := ImageFromFile(write("doc2.pdf"), ""); err == nil || !strings.Contains(err.Error(), "Unsupported Image MIME type") {
		t.Fatalf("ImageFromFile(pdf) = %v", err)
	}
	if _, err := FromFile(write("noext"), ""); err == nil || !strings.Contains(err.Error(), "Could not infer a valid MIME type") {
		t.Fatalf("no extension: %v", err)
	}
	if _, err := FromFile(filepath.Join(dir, "missing.png"), ""); !errors.Is(err, fs.ErrNotExist) || !strings.Contains(err.Error(), "file not found") {
		t.Fatalf("missing: %v", err)
	}
	if _, err := FromFile(dir, ""); err == nil || !strings.Contains(err.Error(), "is a directory") {
		t.Fatalf("directory: %v", err)
	}
	if runtime.GOOS != "windows" && os.Getuid() != 0 {
		p := write("secret.png")
		if err := os.Chmod(p, 0); err != nil {
			t.Fatal(err)
		}
		if _, err := FromFile(p, ""); !errors.Is(err, fs.ErrPermission) {
			t.Fatalf("permission: %v", err)
		}
	}
}

func TestErrorTypes(t *testing.T) {
	var agErr Error
	for _, err := range []error{
		&ConnectionError{Message: "c"}, &ExecutionError{Message: "e"}, &CancelledError{},
		&ValidationError{Message: "v"}, &ToolExecutionError{Message: "t"},
	} {
		if !errors.As(err, &agErr) {
			t.Errorf("%T is not an antigravity.Error", err)
		}
	}
	if (&CancelledError{}).Error() != "The request was cancelled by the client." {
		t.Error("cancelled message")
	}
	if !errors.Is(&CancelledError{}, context.Canceled) || errors.Is(&ExecutionError{}, context.Canceled) {
		t.Error("CancelledError should match context.Canceled")
	}
	inner := errors.New("inner")
	if !errors.Is(&ConnectionError{Err: inner}, inner) || !errors.Is(&ValidationError{Err: inner}, inner) {
		t.Error("Unwrap")
	}
}
