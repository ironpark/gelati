package codex

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"reflect"
	"testing"
)

func TestDynamicToolSpecJSON(t *testing.T) {
	specs := []DynamicToolSpec{
		{Name: "add", Description: "Add", InputSchema: map[string]any{"type": "object"}, DeferLoading: true},
		{Name: "bare", Description: "No schema"},
		{Type: DynamicToolNamespace, Name: "math", Description: "Math", Tools: []DynamicToolSpec{{Name: "mul", Description: "Mul"}}},
	}
	got, err := json.Marshal(specs)
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"type":"function","name":"add","description":"Add","inputSchema":{"type":"object"},"deferLoading":true},` +
		`{"type":"function","name":"bare","description":"No schema","inputSchema":{"properties":{},"type":"object"}},` +
		`{"type":"namespace","name":"math","description":"Math","tools":[{"type":"function","name":"mul","description":"Mul","inputSchema":{"properties":{},"type":"object"}}]}]`
	if string(got) != want {
		t.Fatalf("marshal =\n%s\nwant\n%s", got, want)
	}
	var back []DynamicToolSpec
	if err := json.Unmarshal(got, &back); err != nil {
		t.Fatal(err)
	}
	if back[0].Type != DynamicToolFunction || !back[0].DeferLoading || back[2].Tools[0].Name != "mul" {
		t.Fatalf("unmarshal = %+v", back)
	}
}

// dynamicTool is an ApprovalHandler that is a DynamicToolHandler.
type dynamicTool struct {
	ApprovalFuncs
	got *DynamicToolCallRequest
}

func (d *dynamicTool) CallDynamicTool(_ context.Context, req *DynamicToolCallRequest) (*DynamicToolCallResponse, error) {
	d.got = req
	return DynamicToolText("ok", true), nil
}

func TestDynamicToolCall(t *testing.T) {
	params := map[string]any{"threadId": "thr_1", "turnId": "turn_1", "callId": "c1",
		"tool": "mul", "namespace": "math", "arguments": map[string]any{"a": 2}}

	t.Run("handler", func(t *testing.T) {
		h := &dynamicTool{}
		_, server := connect(t, Options{Approvals: h})
		reply := server.awaitReply(server.request("sr-t", MethodDynamicToolCall, params))
		if reply.Error != nil || string(reply.Result) != `{"contentItems":[{"type":"inputText","text":"ok"}],"success":true}` {
			t.Fatalf("reply = %s, %+v", reply.Result, reply.Error)
		}
		want := &DynamicToolCallRequest{CallID: "c1", ThreadID: "thr_1", TurnID: "turn_1", Tool: "mul", Namespace: "math",
			Arguments: jsontext.Value(`{"a":2}`), Params: h.got.Params}
		if !reflect.DeepEqual(h.got, want) {
			t.Fatalf("request = %+v", h.got)
		}
	})
	t.Run("funcs", func(t *testing.T) {
		_, server := connect(t, Options{Approvals: ApprovalFuncs{
			DynamicTool: func(_ context.Context, req *DynamicToolCallRequest) (*DynamicToolCallResponse, error) {
				return DynamicToolText(req.Tool, false), nil
			},
		}})
		reply := server.awaitReply(server.request("sr-t", MethodDynamicToolCall, params))
		if reply.Error != nil || string(reply.Result) != `{"contentItems":[{"type":"inputText","text":"mul"}],"success":false}` {
			t.Fatalf("reply = %s, %+v", reply.Result, reply.Error)
		}
	})
	t.Run("nil response", func(t *testing.T) {
		_, server := connect(t, Options{Approvals: ApprovalFuncs{
			DynamicTool: func(context.Context, *DynamicToolCallRequest) (*DynamicToolCallResponse, error) { return nil, nil },
		}})
		if reply := server.awaitReply(server.request("sr-t", MethodDynamicToolCall, params)); reply.Error == nil {
			t.Fatalf("reply = %s", reply.Result)
		}
	})
}
