package claude

import (
	"context"
	"encoding/json"
	"errors"
)

// Public types of the callbacks that answer the CLI's inbound control
// requests. The permission callback (CanUseTool) and hook callbacks have
// their own files.

// ErrRespondedOutOfBand is returned (or wrapped) by a CanUseTool,
// OnElicitation or OnUserDialog callback that has already sent the
// control_response some other way, for example a signed HTTP POST echoing
// the request ID. The SDK then writes no reply of its own.
//
// Use it only after the reply was really sent: the CLI keeps waiting for a
// reply that never comes otherwise. A permission prompt has no deadline, so
// its tool stays blocked indefinitely.
var ErrRespondedOutOfBand = errors.New("claude: control request answered out of band")

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
