package agy

import (
	"context"
	"encoding/json/v2"
	"log/slog"
	"strings"

	"github.com/ironpark/gelati/internal/jsonx"

	"github.com/ironpark/gelati/agy/internal/wire"
)

// hookRouter answers the harness's CallHookRequests by dispatching them to
// the hook runner (upstream HookRouter). The connection passes the turn
// context of the turn each request belongs to.
type hookRouter struct {
	hooks *hookRunner
}

// stepIDOf returns the step ID of hook arguments that carry a trajectory and
// step index, or "" when neither is set.
func stepIDOf(trajectoryID string, stepIndex uint32, hasIndex bool) string {
	if trajectoryID == "" && !hasIndex {
		return ""
	}
	return makeStepID(trajectoryID, int(stepIndex))
}

// handle dispatches req and builds the response. A failing hook is reported
// to the harness as the response's error message.
func (r *hookRouter) handle(ctx context.Context, turn *HookContext, req *wire.CallHookRequest, logger *slog.Logger) *wire.CallHookResponse {
	resp := wire.CallHookResponse_builder{RequestId: new(req.GetRequestId())}.Build()
	var err error
	switch req.GetType() {
	case wire.LifecycleHook_LIFECYCLE_HOOK_ON_SESSION_START:
		err = r.hooks.dispatchSessionStart(ctx)
		resp.SetEmptyResult(&wire.EmptyResult{})
	case wire.LifecycleHook_LIFECYCLE_HOOK_ON_SESSION_END:
		err = r.hooks.dispatchSessionEnd(ctx)
		resp.SetEmptyResult(&wire.EmptyResult{})
	case wire.LifecycleHook_LIFECYCLE_HOOK_PRE_TURN:
		err = r.preTurn(ctx, turn, req, resp)
	case wire.LifecycleHook_LIFECYCLE_HOOK_POST_TURN:
		err = r.hooks.dispatchPostTurn(ctx, turn, req.GetPostTurnArgs().GetResponseText())
		resp.SetEmptyResult(&wire.EmptyResult{})
	case wire.LifecycleHook_LIFECYCLE_HOOK_PRE_TOOL:
		err = r.preTool(ctx, turn, req, resp)
	case wire.LifecycleHook_LIFECYCLE_HOOK_POST_TOOL:
		err = r.postTool(ctx, turn, req)
		resp.SetEmptyResult(&wire.EmptyResult{})
	case wire.LifecycleHook_LIFECYCLE_HOOK_ON_TOOL_ERROR:
		r.toolError(ctx, turn, req, resp)
	case wire.LifecycleHook_LIFECYCLE_HOOK_ON_COMPACTION:
		err = r.compaction(ctx, turn, req)
		resp.SetEmptyResult(&wire.EmptyResult{})
	case wire.LifecycleHook_LIFECYCLE_HOOK_STOP:
		err = r.stop(ctx, turn, req, resp)
	default:
		logger.Warn("unknown or unhandled hook", "type", req.GetType(), "name", req.GetName())
		resp.SetEmptyResult(&wire.EmptyResult{})
	}
	if err != nil {
		logger.Error("hook failed", "name", req.GetName(), "error", err)
		return wire.CallHookResponse_builder{RequestId: new(req.GetRequestId()), ErrorMessage: new("Hook failed: " + err.Error())}.Build()
	}
	return resp
}

func (r *hookRouter) preTurn(ctx context.Context, turn *HookContext, req *wire.CallHookRequest, resp *wire.CallHookResponse) error {
	prompt := contentFromUserInput(req.GetPreTurnArgs().GetUserInput())
	res, err := r.hooks.dispatchPreTurn(ctx, turn, prompt)
	if err != nil {
		return err
	}
	if res.Deny {
		resp.SetPreTurnResult(wire.PreTurnResult_builder{Decision: new(wire.PreTurnResult_DENY), Reason: new(res.Message)}.Build())
	} else {
		resp.SetPreTurnResult(wire.PreTurnResult_builder{Decision: new(wire.PreTurnResult_ALLOW)}.Build())
	}
	return nil
}

func (r *hookRouter) preTool(ctx context.Context, turn *HookContext, req *wire.CallHookRequest, resp *wire.CallHookResponse) error {
	call := &ToolCall{Args: map[string]any{}}
	if pta := req.GetPreToolArgs(); pta != nil {
		// Unlike elsewhere, arguments that are not a JSON object fail the
		// request, as upstream.
		var args map[string]any
		if s := pta.GetArgumentsJson(); s != "" {
			if err := json.Unmarshal([]byte(s), &args, jsonx.Foreign); err != nil {
				return err
			}
		}
		call = toolCallFromWire(pta.GetToolName(), args, pta.GetCallId(), stepIDOf(pta.GetTrajectoryId(), pta.GetStepIndex(), pta.HasStepIndex()), pta.GetServerName())
	}
	res, err := r.hooks.dispatchPreToolCall(ctx, turn, call)
	if err != nil {
		return err
	}
	if res.Deny {
		resp.SetPreToolResult(wire.PreToolResult_builder{Decision: new(wire.PreToolResult_DENY), Reason: new(res.Message)}.Build())
		return nil
	}
	ptr := wire.PreToolResult_builder{Decision: new(wire.PreToolResult_ALLOW)}.Build()
	if res.ModifiedArgs != nil {
		ptr.SetModifiedArgs(structOfArgs(res.ModifiedArgs))
	}
	resp.SetPreToolResult(ptr)
	return nil
}

func (r *hookRouter) postTool(ctx context.Context, turn *HookContext, req *wire.CallHookRequest) error {
	result := &ToolResult{}
	if pta := req.GetPostToolArgs(); pta != nil {
		result.Name = sdkToolName(pta.GetToolName())
		result.ServerName = pta.GetServerName()
		result.ID = pta.GetCallId()
		result.StepID = stepIDOf(pta.GetTrajectoryId(), pta.GetStepIndex(), pta.HasStepIndex())
		result.Error = pta.GetError()
		if result.Error == "" {
			result.Result = pta.GetResult()
			if pta.GetResult() != "" {
				if extracted := extractToolResult(result.Name, pta.GetResult()); extracted != nil {
					result.Result = extracted
				}
			}
		}
	}
	return r.hooks.dispatchPostToolCall(ctx, newHookContext(ScopeOperation, turn), result)
}

func (r *hookRouter) toolError(ctx context.Context, turn *HookContext, req *wire.CallHookRequest, resp *wire.CallHookResponse) {
	toolErr := &ToolExecutionError{Message: "Tool failed"}
	if ote := req.GetOnToolErrorArgs(); ote != nil {
		if m := ote.GetErrorMessage(); m != "" {
			toolErr.Message = m
		}
		toolErr.ToolName = sdkToolName(ote.GetToolName())
		toolErr.ServerName = ote.GetServerName()
		toolErr.CallID = ote.GetCallId()
		toolErr.StepID = stepIDOf(ote.GetTrajectoryId(), ote.GetStepIndex(), ote.HasStepIndex())
	}
	recovery := strings.TrimSpace(r.hooks.dispatchToolError(ctx, newHookContext(ScopeOperation, turn), toolErr))
	if recovery != "" {
		resp.SetOnToolErrorResult(wire.OnToolErrorResult_builder{CustomErrorMessage: new(recovery)}.Build())
		return
	}
	resp.SetEmptyResult(&wire.EmptyResult{})
}

func (r *hookRouter) compaction(ctx context.Context, turn *HookContext, req *wire.CallHookRequest) error {
	args := req.GetOnCompactionArgs()
	summary := args.GetSummary()
	if summary == "" {
		summary = "Context compaction"
	}
	idx := int(args.GetStepIndex())
	step := &Step{
		ID:           makeStepID(args.GetTrajectoryId(), idx),
		Type:         StepTypeCompaction,
		Status:       StepStatusDone,
		Source:       StepSourceSystem,
		Target:       StepTargetUser,
		Content:      summary,
		TrajectoryID: args.GetTrajectoryId(),
		StepIndex:    idx,
	}
	return r.hooks.dispatchCompaction(ctx, turn, step)
}

func (r *hookRouter) stop(ctx context.Context, turn *HookContext, req *wire.CallHookRequest, resp *wire.CallHookResponse) error {
	sa := req.GetStopArgs()
	args := StopArgs{
		ResponseText:      sa.GetResponseText(),
		TrajectoryID:      sa.GetTrajectoryId(),
		ContinuationCount: int(sa.GetContinuationCount()),
		StopReason:        parseStopReason(sa.GetStopReason()),
		ErrorMessage:      sa.GetErrorMessage(),
	}
	res, err := r.hooks.dispatchStop(ctx, turn, args)
	if err != nil {
		return err
	}
	if res.Decision == StopDecisionContinue {
		resp.SetStopResult(wire.StopResult_builder{Decision: new(wire.StopResult_CONTINUE), Reason: new(strings.TrimSpace(res.Reason))}.Build())
		return nil
	}
	resp.SetStopResult(wire.StopResult_builder{Decision: new(wire.StopResult_ALLOW_STOP)}.Build())
	return nil
}
