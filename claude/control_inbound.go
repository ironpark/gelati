package claude

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/ironpark/gelati/internal/jsonx"
	"github.com/ironpark/gelati/internal/safecall"
)

// errSuppressReply makes handleControlRequest write no reply. It covers the
// requests the TypeScript SDK deliberately leaves unanswered.
var errSuppressReply = errors.New("claude: control request left unanswered")

// ---------------------------------------------------------------------------
// Dispatch
// ---------------------------------------------------------------------------

// spawnControlHandler answers one control request from the CLI in its own
// goroutine, so a slow callback cannot stall the read loop. A request whose ID
// is already being answered (a duplicate delivery) is skipped.
func (e *engine) spawnControlHandler(ctx context.Context, frame map[string]any) {
	requestID := jsonx.Str(frame["request_id"])
	handlerCtx, cancel := context.WithCancel(ctx)
	h := &inflightHandler{cancel: cancel}
	e.mu.Lock()
	if _, dup := e.inflight[requestID]; dup || e.isClosed() {
		// A closed engine has already cancelled its handlers (see close).
		e.mu.Unlock()
		cancel()
		return
	}
	e.inflight[requestID] = h
	e.mu.Unlock()

	e.handlers.Add(1)
	go func() {
		defer e.handlers.Done()
		defer cancel()
		defer func() {
			e.mu.Lock()
			if e.inflight[requestID] == h {
				delete(e.inflight, requestID)
			}
			e.mu.Unlock()
		}()
		e.handleControlRequest(handlerCtx, requestID, frame)
	}()
}

// cancelInbound withdraws the handler answering request id, after the CLI
// sent a control_cancel_request for it.
func (e *engine) cancelInbound(id string) {
	e.mu.Lock()
	h := e.inflight[id]
	delete(e.inflight, id)
	e.mu.Unlock()
	if h != nil {
		h.cancel()
	}
}

// handleControlRequest answers one control request and writes the reply.
func (e *engine) handleControlRequest(ctx context.Context, requestID string, frame map[string]any) {
	request, _ := frame["request"].(map[string]any)
	data, err := e.dispatchControlRequest(ctx, requestID, request)
	if ctx.Err() != nil {
		// The CLI cancelled the request and is no longer listening for a
		// reply.
		return
	}
	if errors.Is(err, errSuppressReply) || errors.Is(err, ErrRespondedOutOfBand) {
		return
	}
	reply := controlSuccessFrame(requestID, data)
	if err != nil {
		e.cfg.logger.Warn("claude: control request failed",
			"subtype", jsonx.Str(request["subtype"]), "request_id", requestID, "error", err)
		reply = controlErrorFrame(requestID, err.Error())
	}
	if err := e.writeFrame(ctx, reply); err != nil {
		e.cfg.logger.Debug("claude: control reply not written", "request_id", requestID, "error", err)
	}
}

// dispatchControlRequest runs the handler for one control request subtype. A
// panicking callback becomes an error reply naming the subtype rather than
// taking the process down.
func (e *engine) dispatchControlRequest(ctx context.Context, requestID string, request map[string]any) (data map[string]any, err error) {
	subtype := jsonx.Str(request["subtype"])
	defer safecall.Recover(&err, subtype+" callback")
	switch subtype {
	case "remote_tool_call", "remote_plumbing_call", "remote_tools_probe", "remote_tools_reannounce":
		// Meant for the machine that serves this session's tools, not for
		// this host; the TypeScript SDK leaves them unanswered so its reply
		// cannot pre-empt the real answerer.
		return nil, errSuppressReply
	case "can_use_tool":
		return e.handleCanUseTool(ctx, requestID, request)
	case "hook_callback":
		return e.handleHookCallback(ctx, request)
	case "mcp_message":
		return e.mcpServers.handleControl(ctx, request)
	case "elicitation":
		return e.handleElicitation(ctx, requestID, request)
	case "request_user_dialog":
		return e.handleUserDialog(ctx, requestID, request)
	default:
		//lint:ignore ST1005 sent to the CLI verbatim, as the Python SDK does
		return nil, fmt.Errorf("Unsupported control request subtype: %s", subtype)
	}
}

// callbackReply renders the result of a callback that must either answer or
// return an error: a nil result is refused, since the way to send no reply is
// ErrRespondedOutOfBand.
func callbackReply[T any](what string, result *T, err error) (map[string]any, error) {
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, fmt.Errorf("%s returned a nil result; return ErrRespondedOutOfBand to send no reply", what)
	}
	return toWireMap(result, "control response")
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func (e *engine) handleCanUseTool(ctx context.Context, requestID string, request map[string]any) (map[string]any, error) {
	if e.cfg.canUseTool == nil {
		return nil, errors.New("canUseTool callback is not provided")
	}
	input, _ := request["input"].(map[string]any)
	permCtx := toolPermissionContext(requestID, request)
	result, err := e.cfg.canUseTool(ctx, jsonx.Str(request["tool_name"]), input, permCtx)
	if err != nil {
		return nil, err
	}
	out, ok := permissionReply(result, request)
	if !ok {
		return nil, fmt.Errorf("permission callback returned %T, want *PermissionResultAllow or *PermissionResultDeny", result)
	}
	return out, nil
}

func (e *engine) handleHookCallback(ctx context.Context, request map[string]any) (map[string]any, error) {
	id := jsonx.Str(request["callback_id"])
	callback, ok := e.cfg.hookCallbacks[id]
	if !ok {
		//lint:ignore ST1005 sent to the CLI verbatim, as the Python SDK does
		return nil, fmt.Errorf("No hook callback found for ID: %s", id)
	}
	input, _ := request["input"].(map[string]any)
	out, err := callback(ctx, input, jsonx.Str(request["tool_use_id"]), HookContext{Raw: input})
	if err != nil {
		return nil, err
	}
	return toWireMap(out, "hook output")
}

func (e *engine) handleElicitation(ctx context.Context, requestID string, request map[string]any) (map[string]any, error) {
	if e.cfg.onElicitation == nil {
		return map[string]any{"action": ElicitationDecline}, nil
	}
	req := ElicitationRequest{
		ServerName:    jsonx.Str(request["mcp_server_name"]),
		Message:       jsonx.Str(request["message"]),
		Mode:          jsonx.Str(request["mode"]),
		URL:           jsonx.Str(request["url"]),
		ElicitationID: jsonx.Str(request["elicitation_id"]),
		Title:         jsonx.Str(request["title"]),
		DisplayName:   jsonx.Str(request["display_name"]),
		Description:   jsonx.Str(request["description"]),
		RequestID:     requestID,
		Raw:           request,
	}
	req.RequestedSchema, _ = request["requested_schema"].(map[string]any)
	result, err := e.cfg.onElicitation(ctx, req)
	return callbackReply("elicitation callback", result, err)
}

func (e *engine) handleUserDialog(ctx context.Context, requestID string, request map[string]any) (map[string]any, error) {
	kind := jsonx.Str(request["dialog_kind"])
	// Without a handler, or for a kind this host did not declare, stay
	// silent: an error reply would be discarded and a "cancelled" one is a
	// real settlement, so silence lets a capable client or the CLI's
	// deadline settle the dialog.
	if e.cfg.onUserDialog == nil {
		return nil, errSuppressReply
	}
	if len(e.cfg.dialogKinds) > 0 && !slices.Contains(e.cfg.dialogKinds, kind) {
		return nil, errSuppressReply
	}
	req := UserDialogRequest{
		DialogKind: kind,
		ToolUseID:  jsonx.Str(request["tool_use_id"]),
		RequestID:  requestID,
		Raw:        request,
	}
	req.Payload, _ = request["payload"].(map[string]any)
	result, err := e.cfg.onUserDialog(ctx, req)
	return callbackReply("user dialog callback", result, err)
}
