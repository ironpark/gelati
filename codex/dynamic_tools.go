package codex

import (
	"context"
	"encoding/json/jsontext"
	"errors"

	"github.com/ironpark/gelati/internal/jsonx"
)

// Dynamic tool spec types.
const (
	// DynamicToolFunction is a tool the model calls.
	DynamicToolFunction = "function"
	// DynamicToolNamespace groups function tools under a name.
	DynamicToolNamespace = "namespace"
)

// DynamicToolSpec declares a client-provided tool in
// StartThreadParams.DynamicTools. The model's calls to it arrive as
// MethodDynamicToolCall requests, which a DynamicToolHandler answers.
// Dynamic tools are part of the experimental API, which Options.Capabilities
// enables by default.
type DynamicToolSpec struct {
	// Type is DynamicToolFunction, the default, or DynamicToolNamespace.
	Type        string `json:"type"`
	Name        string `json:"name"`
	Description string `json:"description"`
	// InputSchema is a function's JSON schema of its arguments object. Nil
	// sends an object without declared properties.
	InputSchema map[string]any `json:"inputSchema,omitzero"`
	// DeferLoading hides a function behind tool search until it is needed.
	DeferLoading bool `json:"deferLoading,omitzero"`
	// Tools are the functions of a namespace.
	Tools []DynamicToolSpec `json:"tools,omitzero"`
}

// MarshalJSON emits the spec with the defaults of its type filled in.
func (s DynamicToolSpec) MarshalJSON() ([]byte, error) {
	type plain DynamicToolSpec
	if s.Type == DynamicToolNamespace {
		s.InputSchema, s.DeferLoading = nil, false
		if s.Tools == nil {
			s.Tools = []DynamicToolSpec{}
		}
	} else {
		s.Type, s.Tools = DynamicToolFunction, nil
		if s.InputSchema == nil {
			s.InputSchema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
	}
	return jsonx.Marshal(plain(s))
}

// DynamicToolCallRequest is a MethodDynamicToolCall request: the model
// calling a tool of StartThreadParams.DynamicTools.
type DynamicToolCallRequest struct {
	CallID   string `json:"callId"`
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	// Tool is the function's name, and Namespace its namespace's, if any.
	Tool      string `json:"tool"`
	Namespace string `json:"namespace,omitempty"`
	// Arguments is the call's arguments, normally a JSON object.
	Arguments jsontext.Value `json:"arguments,omitempty"`
	// Params is the raw request payload.
	Params jsontext.Value `json:"-"`
}

// DynamicToolCallResponse is the outcome of a dynamic tool call.
type DynamicToolCallResponse struct {
	ContentItems []DynamicToolOutput `json:"contentItems"`
	// Success is false when the call failed; the model still reads the
	// content.
	Success bool `json:"success"`
}

// Dynamic tool output types.
const (
	DynamicToolOutputText  = "inputText"
	DynamicToolOutputImage = "inputImage"
	DynamicToolOutputAudio = "inputAudio"
)

// DynamicToolOutput is one content item of a DynamicToolCallResponse.
type DynamicToolOutput struct {
	// Type is one of the DynamicToolOutput* constants, which set Text,
	// ImageURL or AudioURL respectively.
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"imageUrl,omitempty"`
	AudioURL string `json:"audioUrl,omitempty"`
}

// DynamicToolText returns a response with one text item.
func DynamicToolText(text string, success bool) *DynamicToolCallResponse {
	return &DynamicToolCallResponse{
		ContentItems: []DynamicToolOutput{{Type: DynamicToolOutputText, Text: text}},
		Success:      success,
	}
}

// DynamicToolHandler is an optional ApprovalHandler extension that answers
// MethodDynamicToolCall requests. Without it, they go to a
// ServerRequestHandler, and fail with method-not-found when there is none.
type DynamicToolHandler interface {
	CallDynamicTool(ctx context.Context, req *DynamicToolCallRequest) (*DynamicToolCallResponse, error)
}

// dynamicToolCall decodes a MethodDynamicToolCall request.
func dynamicToolCall(params jsontext.Value) (*DynamicToolCallRequest, error) {
	req := &DynamicToolCallRequest{}
	if err := jsonx.Unmarshal(params, req); err != nil {
		return nil, &RPCError{Code: CodeInvalidParams, Message: err.Error()}
	}
	req.Params = params
	return req, nil
}

// callDynamicTool runs h on a MethodDynamicToolCall request.
func callDynamicTool(ctx context.Context, h func(context.Context, *DynamicToolCallRequest) (*DynamicToolCallResponse, error), params jsontext.Value) (any, error) {
	req, err := dynamicToolCall(params)
	if err != nil {
		return nil, err
	}
	res, err := h(ctx, req)
	if err == nil && res == nil {
		err = errors.New("codex: the dynamic tool handler returned no response")
	}
	if err != nil {
		return nil, err
	}
	return res, nil
}

// CallDynamicTool implements DynamicToolHandler: DynamicTool answers, or
// else Other, whose result is read as a DynamicToolCallResponse.
func (f ApprovalFuncs) CallDynamicTool(ctx context.Context, req *DynamicToolCallRequest) (*DynamicToolCallResponse, error) {
	if f.DynamicTool != nil {
		return f.DynamicTool(ctx, req)
	}
	res, err := f.HandleServerRequest(ctx, MethodDynamicToolCall, req.Params)
	if err != nil {
		return nil, err
	}
	b, err := jsonx.Marshal(res)
	if err != nil {
		return nil, err
	}
	out := &DynamicToolCallResponse{}
	if err := jsonx.Unmarshal(b, out); err != nil {
		return nil, &RPCError{Code: CodeInternalError, Message: "codex: invalid dynamic tool response: " + err.Error()}
	}
	return out, nil
}
