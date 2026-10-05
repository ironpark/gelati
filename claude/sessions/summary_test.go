package sessions

import (
	jsonv1 "encoding/json" // Number: callers may hand in values decoded with UseNumber
	"encoding/json/v2"
	"maps"
	"reflect"
	"strings"
	"testing"

	"github.com/ironpark/gelati/internal/jsonx"
)

var summaryTestKey = Key{ProjectKey: storeTestKey, SessionID: "11111111-1111-4111-8111-111111111111"}

func TestSummaryFoldSessionSummary(t *testing.T) {
	t.Parallel()

	t.Run("init from nil", func(t *testing.T) {
		t.Parallel()
		s := FoldSummary(nil, summaryTestKey, nil, nil)
		want := SummaryEntry{SessionID: summaryTestKey.SessionID, MTime: 0, Data: map[string]any{}}
		if !reflect.DeepEqual(s, want) {
			t.Errorf("got %#v", s)
		}
	})

	t.Run("set-once fields freeze", func(t *testing.T) {
		t.Parallel()
		s := FoldSummary(nil, summaryTestKey, []Entry{
			{"type": "x", "timestamp": "2024-01-01T00:00:00.000Z", "cwd": "/a", "isSidechain": false},
			{"type": "x", "timestamp": "2024-01-01T00:00:05.000Z", "cwd": "/b"},
		}, nil)
		if s.Data["createdAt"] != int64(1704067200000) || s.Data["cwd"] != "/a" || s.Data["isSidechain"] != false {
			t.Fatalf("data = %v", s.Data)
		}
		s2 := FoldSummary(&s, summaryTestKey, []Entry{
			{"type": "x", "timestamp": "2024-01-02T00:00:00.000Z", "cwd": "/c", "isSidechain": true},
		}, nil)
		if s2.Data["createdAt"] != int64(1704067200000) || s2.Data["cwd"] != "/a" || s2.Data["isSidechain"] != false {
			t.Errorf("data = %v", s2.Data)
		}
	})

	t.Run("last wins", func(t *testing.T) {
		t.Parallel()
		s := FoldSummary(nil, summaryTestKey, []Entry{
			{"type": "x", "timestamp": "2024-01-01T00:00:00Z", "customTitle": "t1", "gitBranch": "main"},
			{"type": "x", "timestamp": "2024-01-01T00:00:01Z", "customTitle": "t2"},
		}, nil)
		if s.Data["customTitle"] != "t2" || s.Data["gitBranch"] != "main" {
			t.Fatalf("data = %v", s.Data)
		}
		s2 := FoldSummary(&s, summaryTestKey, []Entry{
			{"type": "x", "aiTitle": "ai", "lastPrompt": "lp", "summary": "sm", "gitBranch": "dev"},
		}, nil)
		want := map[string]any{"customTitle": "t2", "aiTitle": "ai", "lastPrompt": "lp", "summaryHint": "sm", "gitBranch": "dev"}
		for k, v := range want {
			if s2.Data[k] != v {
				t.Errorf("%s = %v, want %v", k, s2.Data[k], v)
			}
		}
	})

	t.Run("mtime not derived from entries", func(t *testing.T) {
		t.Parallel()
		s := FoldSummary(nil, summaryTestKey, []Entry{
			{"type": "x", "timestamp": "2024-01-01T00:00:05.000Z"},
			{"type": "x", "timestamp": "2024-01-01T00:00:01.000Z"},
		}, nil)
		if s.MTime != 0 {
			t.Errorf("new mtime = %d", s.MTime)
		}
		prev := SummaryEntry{SessionID: summaryTestKey.SessionID, MTime: 42, Data: map[string]any{}}
		if s2 := FoldSummary(&prev, summaryTestKey, []Entry{{"type": "x", "timestamp": "2024-01-01T00:00:10.000Z"}}, nil); s2.MTime != 42 {
			t.Errorf("carried mtime = %d", s2.MTime)
		}
	})

	t.Run("tag set and clear", func(t *testing.T) {
		t.Parallel()
		s := FoldSummary(nil, summaryTestKey, []Entry{{"type": "tag", "tag": "wip"}}, nil)
		if s.Data["tag"] != "wip" {
			t.Fatalf("data = %v", s.Data)
		}
		if s2 := FoldSummary(&s, summaryTestKey, []Entry{{"type": "tag", "tag": ""}}, nil); s2.Data["tag"] != nil {
			t.Errorf("cleared: %v", s2.Data)
		}
		if s3 := FoldSummary(&s, summaryTestKey, []Entry{{"type": "tag"}}, nil); s3.Data["tag"] != nil {
			t.Errorf("absent clears: %v", s3.Data)
		}
		if s4 := FoldSummary(&s, summaryTestKey, []Entry{{"type": "user", "tag": "ignored"}}, nil); s4.Data["tag"] != "wip" {
			t.Errorf("non-tag entry: %v", s4.Data)
		}
	})

	t.Run("sidechain latches on first entry", func(t *testing.T) {
		t.Parallel()
		s := FoldSummary(nil, summaryTestKey, []Entry{{"type": "x", "timestamp": "2024-01-01T00:00:00Z", "isSidechain": true}}, nil)
		if s.Data["isSidechain"] != true {
			t.Errorf("data = %v", s.Data)
		}
		s = FoldSummary(nil, summaryTestKey, []Entry{
			{"type": "user", "isSidechain": true},
			{"type": "x", "timestamp": "2024-01-01T00:00:00Z"},
		}, nil)
		if s.Data["isSidechain"] != true || s.Data["createdAt"] != int64(1704067200000) {
			t.Errorf("data = %v", s.Data)
		}
		// Only a literal true counts.
		s = FoldSummary(nil, summaryTestKey, []Entry{{"type": "user", "isSidechain": "yes"}}, nil)
		if s.Data["isSidechain"] != false {
			t.Errorf("truthy non-bool: %v", s.Data)
		}
	})

	t.Run("first prompt", func(t *testing.T) {
		t.Parallel()
		u := func(text any, extra ...any) Entry {
			return summaryUser(text, "2024-01-01T00:00:00.000Z", extra...)
		}
		s := FoldSummary(nil, summaryTestKey, []Entry{
			u("ignored meta", "isMeta", true),
			u("ignored compact", "isCompactSummary", true),
			u([]any{map[string]any{"type": "tool_result", "tool_use_id": "x", "content": "res"}}),
			u("real first"),
			u("not me"),
		}, nil)
		if s.Data["firstPrompt"] != "real first" || s.Data["firstPromptLocked"] != true {
			t.Errorf("data = %v", s.Data)
		}

		s = FoldSummary(nil, summaryTestKey, []Entry{
			u("<command-name>/init</command-name> stuff"),
			u("<command-name>/second</command-name>"),
		}, nil)
		if s.Data["firstPromptLocked"] == true || s.Data["commandFallback"] != "/init" {
			t.Errorf("command fallback: %v", s.Data)
		}
		s2 := FoldSummary(&s, summaryTestKey, []Entry{u("now real")}, nil)
		if s2.Data["firstPrompt"] != "now real" || s2.Data["firstPromptLocked"] != true || s2.Data["commandFallback"] != "/init" {
			t.Errorf("locked later: %v", s2.Data)
		}

		s = FoldSummary(nil, summaryTestKey, []Entry{u("<local-command-stdout> some output"), u("hello")}, nil)
		if s.Data["firstPrompt"] != "hello" {
			t.Errorf("skip pattern: %v", s.Data)
		}
		s = FoldSummary(nil, summaryTestKey, []Entry{u(strings.Repeat("x", 300))}, nil)
		if fp, _ := s.Data["firstPrompt"].(string); len([]rune(fp)) > 201 || !strings.HasSuffix(fp, "\u2026") {
			t.Errorf("truncated: %q", fp)
		}
	})

	t.Run("prev is not mutated", func(t *testing.T) {
		t.Parallel()
		prev := SummaryEntry{SessionID: "a", MTime: 5, Data: map[string]any{"cwd": "/x"}}
		before := maps.Clone(prev.Data)
		s := FoldSummary(&prev, summaryTestKey, []Entry{{"type": "x", "customTitle": "t"}}, nil)
		if !reflect.DeepEqual(prev.Data, before) || prev.SessionID != "a" || prev.MTime != 5 {
			t.Errorf("prev mutated: %#v", prev)
		}
		if s.SessionID != "a" || s.Data["customTitle"] != "t" || s.Data["cwd"] != "/x" {
			t.Errorf("result = %#v", s)
		}
		nilData := SummaryEntry{SessionID: "b"}
		if s := FoldSummary(&nilData, summaryTestKey, []Entry{{"type": "x", "customTitle": "t"}}, nil); s.Data["customTitle"] != "t" {
			t.Errorf("nil prev data: %#v", s)
		}
	})
}

func TestSummaryEntryToSessionInfo(t *testing.T) {
	t.Parallel()
	if info := summaryEntryToSessionInfo(SummaryEntry{SessionID: "s", MTime: 1, Data: map[string]any{"isSidechain": true, "customTitle": "t"}}, ""); info != nil {
		t.Errorf("sidechain: %+v", info)
	}
	if info := summaryEntryToSessionInfo(SummaryEntry{SessionID: "s", MTime: 1, Data: map[string]any{}}, ""); info != nil {
		t.Errorf("empty: %+v", info)
	}
	if info := summaryEntryToSessionInfo(SummaryEntry{SessionID: "s", MTime: 1}, ""); info != nil {
		t.Errorf("nil data: %+v", info)
	}

	data := map[string]any{
		"firstPrompt": "fp", "firstPromptLocked": true, "commandFallback": "/cmd",
		"summaryHint": "sh", "lastPrompt": "lp", "aiTitle": "ai", "customTitle": "ct",
	}
	base := SummaryEntry{SessionID: "s", MTime: 1, Data: data}
	steps := []struct {
		remove                       string
		summary, customTitle, prompt string
	}{
		{"", "ct", "ct", "fp"},
		{"customTitle", "ai", "ai", "fp"},
		{"aiTitle", "lp", "", "fp"},
		{"lastPrompt", "sh", "", "fp"},
		{"summaryHint", "fp", "", "fp"},
	}
	for _, st := range steps {
		delete(data, st.remove)
		info := summaryEntryToSessionInfo(base, "")
		if info == nil || info.Summary != st.summary || info.CustomTitle != st.customTitle || info.FirstPrompt != st.prompt {
			t.Errorf("after removing %q: %+v", st.remove, info)
		}
	}
	data["firstPromptLocked"] = false
	if info := summaryEntryToSessionInfo(base, ""); info == nil || info.Summary != "/cmd" || info.FirstPrompt != "/cmd" {
		t.Errorf("command fallback: %+v", info)
	}

	if info := summaryEntryToSessionInfo(SummaryEntry{SessionID: "s", MTime: 1, Data: map[string]any{"customTitle": "t"}}, "/proj"); info == nil || info.Cwd != "/proj" {
		t.Errorf("cwd fallback: %+v", info)
	}
	if info := summaryEntryToSessionInfo(SummaryEntry{SessionID: "s", MTime: 1, Data: map[string]any{"customTitle": "t", "cwd": "/own"}}, "/proj"); info == nil || info.Cwd != "/own" {
		t.Errorf("own cwd: %+v", info)
	}

	want := &Info{SessionID: "s", Summary: "t", LastModified: 99, CustomTitle: "t", GitBranch: "main", Tag: "wip", CreatedAt: 50}
	for _, createdAt := range []any{int64(50), 50.0, jsonv1.Number("50"), 50} {
		info := summaryEntryToSessionInfo(SummaryEntry{SessionID: "s", MTime: 99, Data: map[string]any{
			"customTitle": "t", "gitBranch": "main", "tag": "wip", "createdAt": createdAt,
		}}, "")
		if !reflect.DeepEqual(info, want) {
			t.Errorf("createdAt %T: %+v", createdAt, info)
		}
	}
}

// TestSummaryLegacyKeys checks that summaries written with the snake_case
// data keys of the Python SDK (and earlier versions of this package) are
// read, and migrated to camelCase by the next fold.
func TestSummaryLegacyKeys(t *testing.T) {
	t.Parallel()
	legacy := SummaryEntry{SessionID: "s", MTime: 9, Data: map[string]any{
		"is_sidechain": false, "created_at": 50.0, "cwd": "/w", "first_prompt": "fp", "first_prompt_locked": true,
		"custom_title": "ct", "git_branch": "main", "tag": "wip",
	}}
	want := &Info{SessionID: "s", Summary: "ct", LastModified: 9, CustomTitle: "ct", FirstPrompt: "fp",
		GitBranch: "main", Cwd: "/w", Tag: "wip", CreatedAt: 50}
	if info := summaryEntryToSessionInfo(legacy, ""); !reflect.DeepEqual(info, want) {
		t.Errorf("legacy read: %+v", info)
	}
	if info := summaryEntryToSessionInfo(SummaryEntry{SessionID: "s", Data: map[string]any{"is_sidechain": true, "custom_title": "t"}}, ""); info != nil {
		t.Errorf("legacy sidechain: %+v", info)
	}
	// camelCase wins over a stale snake_case duplicate.
	mixed := SummaryEntry{SessionID: "s", Data: map[string]any{"customTitle": "new", "custom_title": "old"}}
	if info := summaryEntryToSessionInfo(mixed, ""); info == nil || info.CustomTitle != "new" {
		t.Errorf("mixed: %+v", info)
	}

	folded := FoldSummary(&legacy, summaryTestKey, []Entry{
		summaryUser("later prompt", "2024-01-01T00:00:00.000Z", "cwd", "/other", "isSidechain", true),
		{"type": "x", "aiTitle": "ai"},
	}, nil)
	wantData := map[string]any{
		"isSidechain": false, "createdAt": 50.0, "cwd": "/w", "firstPrompt": "fp", "firstPromptLocked": true,
		"customTitle": "ct", "aiTitle": "ai", "gitBranch": "main", "tag": "wip",
	}
	if !reflect.DeepEqual(folded.Data, wantData) {
		t.Errorf("migrated data = %v", folded.Data)
	}
	if _, ok := legacy.Data["is_sidechain"]; !ok {
		t.Error("prev mutated by migration")
	}
}

func TestSummaryFoldOptionsAndRelocation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		prevMTime int64
		opts      *FoldOptions
		wantMTime int64
	}{
		{"nil options keep prev", 5, nil, 5},
		{"zero mtime keeps prev", 5, &FoldOptions{}, 5},
		{"mtime stamps", 5, &FoldOptions{MTime: 77}, 77},
	}
	for _, tt := range tests {
		prev := SummaryEntry{SessionID: summaryTestKey.SessionID, MTime: tt.prevMTime, Data: map[string]any{}}
		if got := FoldSummary(&prev, summaryTestKey, nil, tt.opts); got.MTime != tt.wantMTime {
			t.Errorf("%s: mtime = %d, want %d", tt.name, got.MTime, tt.wantMTime)
		}
	}
	if got := FoldSummary(nil, summaryTestKey, nil, &FoldOptions{MTime: 3}); got.MTime != 3 {
		t.Errorf("new summary mtime = %d", got.MTime)
	}

	s := FoldSummary(nil, summaryTestKey, []Entry{
		{"type": "user", "cwd": "/first"},
		{"type": "relocated", "relocatedCwd": "/moved"},
		{"type": "user", "cwd": "/ignored"},
	}, nil)
	if s.Data["cwd"] != "/moved" {
		t.Errorf("relocated cwd = %v", s.Data["cwd"])
	}
	s = FoldSummary(&s, summaryTestKey, []Entry{{"type": "relocated", "relocatedCwd": ""}}, nil)
	if s.Data["cwd"] != "/moved" {
		t.Errorf("empty relocation changed cwd: %v", s.Data["cwd"])
	}
}

// TestSummaryRoundTripsThroughJSON checks that a persisted summary (numbers
// decoded as float64) converts like the in-memory one.
func TestSummaryRoundTripsThroughJSON(t *testing.T) {
	t.Parallel()
	folded := FoldSummary(nil, summaryTestKey, []Entry{
		summaryUser("hello", "2024-01-01T00:00:00.123Z", "cwd", "/w"),
		{"type": "tag", "tag": "wip"},
	}, nil)
	folded.MTime = 7
	b, err := jsonx.Marshal(folded)
	if err != nil {
		t.Fatal(err)
	}
	var decoded SummaryEntry
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatal(err)
	}
	a, c := summaryEntryToSessionInfo(folded, ""), summaryEntryToSessionInfo(decoded, "")
	if !reflect.DeepEqual(a, c) || a.CreatedAt != 1704067200123 {
		t.Errorf("in-memory %+v\ndecoded   %+v", a, c)
	}
}

// TestSummaryParityWithLiteParse checks that folding entries incrementally
// yields the same Info as lite-parsing their serialized transcript.
func TestSummaryParityWithLiteParse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		sid     string
		cwd     string
		entries []Entry
		split   int
	}{
		{
			name: "full", sid: "22222222-2222-4222-8222-222222222222", cwd: "/work", split: 3,
			entries: []Entry{
				summaryUser("<command-name>/clear</command-name>", "2024-01-01T00:00:00.000Z", "cwd", "/work", "gitBranch", "main"),
				summaryUser("ignored", "2024-01-01T00:00:01.000Z", "isMeta", true),
				summaryUser("real prompt here", "2024-01-01T00:00:02.000Z"),
				{"type": "assistant", "timestamp": "2024-01-01T00:00:03.000Z", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "ok"}}}},
				{"type": "x", "timestamp": "2024-01-01T00:00:04.000Z", "aiTitle": "AI Named"},
				{"type": "tag", "timestamp": "2024-01-01T00:00:05.000Z", "tag": "wip"},
				{"type": "x", "timestamp": "2024-01-01T00:00:06.000Z", "customTitle": "User Named", "gitBranch": "feature"},
			},
		},
		{
			name: "first prompt only", sid: "33333333-3333-4333-8333-333333333333", cwd: "/w", split: 1,
			entries: []Entry{summaryUser("just a prompt", "2024-02-01T00:00:00.000Z", "cwd", "/w")},
		},
		{
			name: "unicode", sid: "44444444-4444-4444-8444-444444444444", cwd: "/w", split: 1,
			entries: []Entry{
				summaryUser("  héllo 😀 wörld  ", "2024-02-01T00:00:00.000Z"),
				{"type": "x", "lastPrompt": "dernière"},
			},
		},
	}
	for _, tt := range tests {
		key := Key{ProjectKey: storeTestKey, SessionID: tt.sid}
		folded := FoldSummary(nil, key, tt.entries[:tt.split], nil)
		folded = FoldSummary(&folded, key, tt.entries[tt.split:], nil)
		incremental := summaryEntryToSessionInfo(folded, tt.cwd)

		batch := parseSessionInfoFromLite(tt.sid, jsonlToLite(entriesToJSONL(tt.entries), folded.MTime), tt.cwd, "")
		if incremental == nil || batch == nil {
			t.Fatalf("%s: incremental=%+v batch=%+v", tt.name, incremental, batch)
		}
		batch.FileSize = 0
		if !reflect.DeepEqual(incremental, batch) {
			t.Errorf("%s:\nincremental %+v\nbatch       %+v", tt.name, incremental, batch)
		}
	}
}
