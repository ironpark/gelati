package claude

import (
	"encoding/json/v2"
	"reflect"
	"testing"
	"time"
)

// TestUserInputFrame checks the SDKUserMessage wire shape of UserInput
// (sdk.d.ts SDKUserMessage).
func TestUserInputFrame(t *testing.T) {
	t.Parallel()
	no := false
	ts := time.Date(2026, 1, 2, 3, 4, 5, 678_000_000, time.FixedZone("KST", 9*3600))
	cases := []struct {
		name  string
		input UserInput
		want  string
	}{
		{"plain", UserInput{Content: "hi"},
			`{"message":{"content":"hi","role":"user"},"parent_tool_use_id":null,"session_id":"","type":"user"}`},
		{"allFields", UserInput{
			Content: "hi", UUID: "8c1f", SessionID: "s1", ParentToolUseID: "tu1",
			Origin: &MessageOrigin{Kind: OriginHuman}, Priority: MessagePriorityLater, ShouldQuery: &no,
			IsSynthetic: true, Timestamp: ts, ClientComposed: true,
			PastedContent: []string{"pasted"}, InlinePastes: []string{"hi"},
		}, `{"client_composed":true,"inline_pastes":["hi"],"isSynthetic":true,
		    "message":{"content":"hi","role":"user"},"origin":{"kind":"human"},"parent_tool_use_id":"tu1",
		    "pasted_content":["pasted"],"priority":"later","session_id":"s1","shouldQuery":false,
		    "timestamp":"2026-01-01T18:04:05.678Z","type":"user","uuid":"8c1f"}`},
		{"blocks", UserInput{
			Content: "What is in this image?",
			Blocks: []ContentBlock{
				&ImageBlock{Source: BlockSource{Type: SourceBase64, MediaType: "image/png", Data: "iVBO"}},
				&DocumentBlock{Source: BlockSource{Type: SourceFile, FileID: "file_1"}, Title: "Spec"},
				nil,
				&UnknownBlock{Type: "search_result", Raw: map[string]any{"source": "s", "title": "t", "content": []any{}}},
			},
		}, `{"message":{"role":"user","content":[
		      {"type":"text","text":"What is in this image?"},
		      {"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBO"}},
		      {"type":"document","source":{"type":"file","file_id":"file_1"},"title":"Spec"},
		      {"type":"search_result","source":"s","title":"t","content":[]}]},
		    "parent_tool_use_id":null,"session_id":"","type":"user"}`},
		{"blocksOnly", UserInput{Blocks: []ContentBlock{&TextBlock{Text: "a"}}},
			`{"message":{"role":"user","content":[{"type":"text","text":"a"}]},"parent_tool_use_id":null,"session_id":"","type":"user"}`},
		{"raw", UserInput{Content: "ignored", UUID: "x", Raw: map[string]any{"type": "user", "custom": true}},
			`{"custom":true,"type":"user"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			payload, err := encodeFrame(tc.input.frame())
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var got, want any
			if err := json.Unmarshal(payload, &got); err != nil {
				t.Fatalf("decode %s: %v", payload, err)
			}
			if err := json.Unmarshal([]byte(tc.want), &want); err != nil {
				t.Fatalf("bad fixture: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("frame = %s\nwant    %s", payload, tc.want)
			}
		})
	}
}

// TestUserInputFrameParsesBack checks that a frame UserInput writes is read
// back by the parser as the same user message (the CLI echoes it on replay).
func TestUserInputFrameParsesBack(t *testing.T) {
	t.Parallel()
	in := UserInput{
		Content: "look", UUID: "u1", SessionID: "s1", Priority: MessagePriorityNow, IsSynthetic: true,
		Blocks: []ContentBlock{&ImageBlock{Source: BlockSource{Type: SourceURL, URL: "https://x/a.png"}}},
	}
	payload, err := encodeFrame(in.frame())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	um := mustParse(t, string(payload)).(*UserMessage)
	want := []ContentBlock{&TextBlock{Text: "look"}, &ImageBlock{Source: BlockSource{Type: SourceURL, URL: "https://x/a.png"}}}
	if !reflect.DeepEqual(um.Content, want) || um.UUID != "u1" || um.SessionID != "s1" ||
		um.Priority != MessagePriorityNow || !um.IsSynthetic {
		t.Fatalf("got %+v", um)
	}
}
