package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

// ErrRespondedOutOfBand is returned (or wrapped) by a CanUseTool,
// OnElicitation or OnUserDialog callback that has already sent the
// control_response some other way, for example a signed HTTP POST echoing
// the request ID. The SDK then writes no reply of its own.
//
// Use it only after the reply was really sent: the CLI keeps waiting for a
// reply that never comes otherwise. A permission prompt has no deadline, so
// its tool stays blocked indefinitely.
var ErrRespondedOutOfBand = errors.New("claude: control request answered out of band")

// errSuppressReply makes handleControlRequest write no reply. It covers the
// requests the TypeScript SDK deliberately leaves unanswered.
var errSuppressReply = errors.New("claude: control request left unanswered")

// unansweredSubtypes are inbound requests meant for the machine that serves
// this session's tools, not for this host; the TypeScript SDK leaves them
// unanswered so its reply cannot pre-empt the real answerer.
var unansweredSubtypes = map[string]bool{
	"remote_tool_call":        true,
	"remote_plumbing_call":    true,
	"remote_tools_probe":      true,
	"remote_tools_reannounce": true,
}

// ---------------------------------------------------------------------------
// MCP elicitation
// ---------------------------------------------------------------------------

// ElicitationMode is how an MCP server collects the input it asks for.
type ElicitationMode = string

// Elicitation modes.
const (
	// ElicitationModeForm asks for structured input described by
	// ElicitationRequest.RequestedSchema.
	ElicitationModeForm ElicitationMode = "form"
	// ElicitationModeURL asks the user to complete a flow in a browser at
	// ElicitationRequest.URL.
	ElicitationModeURL ElicitationMode = "url"
)

// ElicitationRequest is an MCP server's request for user input.
type ElicitationRequest struct {
	// ServerName names the MCP server asking.
	ServerName string
	// Message is the text to show the user.
	Message string
	// Mode is "form", "url" or empty.
	Mode ElicitationMode
	// URL is the page to open in "url" mode.
	URL string
	// ElicitationID correlates a "url" elicitation with its completion
	// notification.
	ElicitationID string
	// RequestedSchema is the JSON Schema of the input asked for in "form"
	// mode.
	RequestedSchema map[string]any
	// Title, DisplayName and Description come from the server's
	// permission-display metadata, for prompts driven by an elicitation.
	Title       string
	DisplayName string
	Description string
	// RequestID is the control request's request_id. A reply sent out of
	// band (see ErrRespondedOutOfBand) must echo it.
	RequestID string
	// Raw is the complete request payload.
	Raw map[string]any
}

// ElicitationAction is the user's answer to an elicitation.
type ElicitationAction = string

// Elicitation actions.
const (
	ElicitationAccept  ElicitationAction = "accept"
	ElicitationDecline ElicitationAction = "decline"
	ElicitationCancel  ElicitationAction = "cancel"
)

// ElicitationResult answers an elicitation (MCP ElicitResult).
type ElicitationResult struct {
	Action ElicitationAction `json:"action"`
	// Content holds the submitted values when Action is "accept" in form
	// mode.
	Content map[string]any `json:"content,omitempty"`
	// Meta is the result's _meta object.
	Meta map[string]any `json:"_meta,omitempty"`
}

// OnElicitation answers an MCP elicitation. ctx is cancelled when the CLI
// withdraws the request. Returning an error wrapping ErrRespondedOutOfBand
// sends no reply; the elicitation then stays pending until the server times
// it out unless the reply really went out some other way.
type OnElicitation func(ctx context.Context, req ElicitationRequest) (*ElicitationResult, error)

// ---------------------------------------------------------------------------
// User dialogs
// ---------------------------------------------------------------------------

// UserDialogRequest asks the host to render a blocking dialog. Each kind
// defines its own payload and result shape; the protocol carries both
// opaquely.
type UserDialogRequest struct {
	// DialogKind identifies the dialog. The set is open: answer a kind you
	// cannot render with UserDialogCancelled.
	DialogKind string
	// Payload is the dialog-specific data for the renderer.
	Payload map[string]any
	// ToolUseID is set when the dialog belongs to a tool call; it matches
	// ToolPermissionContext.ToolUseID.
	ToolUseID string
	// RequestID is the control request's request_id. A reply sent out of
	// band (see ErrRespondedOutOfBand) must echo it.
	RequestID string
	// Raw is the complete request payload.
	Raw map[string]any
}

// UserDialogBehavior is how a dialog was settled.
type UserDialogBehavior = string

// Dialog outcomes.
const (
	// UserDialogCompleted reports the user's choice in UserDialogResult.Result.
	UserDialogCompleted UserDialogBehavior = "completed"
	// UserDialogCancelled dismisses the dialog; the CLI applies its default
	// behavior.
	UserDialogCancelled UserDialogBehavior = "cancelled"
)

// UserDialogResult is the host's answer to a UserDialogRequest.
type UserDialogResult struct {
	// Behavior is UserDialogCompleted or UserDialogCancelled. Empty means
	// completed.
	Behavior UserDialogBehavior
	// Result is the dialog-specific answer when completed.
	Result any
}

// MarshalJSON emits {"behavior":"completed","result":...} or
// {"behavior":"cancelled"}.
func (r UserDialogResult) MarshalJSON() ([]byte, error) {
	if r.Behavior == UserDialogCancelled {
		return json.Marshal(map[string]any{"behavior": UserDialogCancelled})
	}
	return json.Marshal(map[string]any{"behavior": UserDialogCompleted, "result": r.Result})
}

// OnUserDialog renders a dialog the CLI requested and returns the user's
// answer. ctx is cancelled when the CLI withdraws the request. Returning an
// error wrapping ErrRespondedOutOfBand sends no reply; the dialog then stays
// parked until the CLI's deadline unless the reply went out some other way.
type OnUserDialog func(ctx context.Context, req UserDialogRequest) (*UserDialogResult, error)

// validateCallbackOptions rejects callback option combinations the CLI could
// not serve.
func validateCallbackOptions(opts *Options) error {
	if opts != nil && len(opts.SupportedDialogKinds) > 0 && opts.OnUserDialog == nil {
		return errors.New("claude: Options.SupportedDialogKinds requires Options.OnUserDialog; " +
			"declaring dialog kinds without a handler would park dialogs nothing can answer")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func (e *engine) handleElicitation(ctx context.Context, requestID string, request map[string]any) (data map[string]any, err error) {
	if e.opts.OnElicitation == nil {
		return map[string]any{"action": ElicitationDecline}, nil
	}
	req := ElicitationRequest{
		ServerName:    str(request["mcp_server_name"]),
		Message:       str(request["message"]),
		Mode:          str(request["mode"]),
		URL:           str(request["url"]),
		ElicitationID: str(request["elicitation_id"]),
		Title:         str(request["title"]),
		DisplayName:   str(request["display_name"]),
		Description:   str(request["description"]),
		RequestID:     requestID,
		Raw:           request,
	}
	req.RequestedSchema, _ = request["requested_schema"].(map[string]any)

	defer func() {
		if r := recover(); r != nil {
			data, err = nil, fmt.Errorf("elicitation callback panicked: %v", r)
		}
	}()
	result, err := e.opts.OnElicitation(ctx, req)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, errors.New("elicitation callback returned a nil result; return ErrRespondedOutOfBand to send no reply")
	}
	return toWireMap(result, "control response")
}

func (e *engine) handleUserDialog(ctx context.Context, requestID string, request map[string]any) (data map[string]any, err error) {
	kind := str(request["dialog_kind"])
	// Without a handler, or for a kind this host did not declare, stay
	// silent: an error reply would be discarded and a "cancelled" one is a
	// real settlement, so silence lets a capable client or the CLI's
	// deadline settle the dialog.
	if e.opts.OnUserDialog == nil {
		return nil, errSuppressReply
	}
	if len(e.opts.SupportedDialogKinds) > 0 && !slices.Contains(e.opts.SupportedDialogKinds, kind) {
		return nil, errSuppressReply
	}
	req := UserDialogRequest{
		DialogKind: kind,
		ToolUseID:  str(request["tool_use_id"]),
		RequestID:  requestID,
		Raw:        request,
	}
	req.Payload, _ = request["payload"].(map[string]any)

	defer func() {
		if r := recover(); r != nil {
			data, err = nil, fmt.Errorf("user dialog callback panicked: %v", r)
		}
	}()
	result, err := e.opts.OnUserDialog(ctx, req)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, errors.New("user dialog callback returned a nil result; return ErrRespondedOutOfBand to send no reply")
	}
	return toWireMap(result, "control response")
}
