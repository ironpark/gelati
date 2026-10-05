package antigravity

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/ironpark/gelati/antigravity/internal/wire"
)

// hookRouter answers the harness's CallHookRequests by dispatching them to
// the hook runner (upstream HookRouter). The connection passes the turn
// context of the turn each request belongs to.
type hookRouter struct {
	hooks *hookRunner
}

// stepIDOf returns the step ID of hook arguments that carry a trajectory and
// step index, or "" when neither is set.
func stepIDOf(trajectoryID string, stepIndex *uint32) string {
	if trajectoryID == "" && stepIndex == nil {
		return ""
	}
	var idx int
	if stepIndex != nil {
		idx = int(*stepIndex)
	}
	return makeStepID(trajectoryID, idx)
}

// handle dispatches req and builds the response. A failing hook is reported
// to the harness as the response's error message.
func (r *hookRouter) handle(ctx context.Context, turn *HookContext, req *wire.CallHookRequest, logger *slog.Logger) *wire.CallHookResponse {
	resp := &wire.CallHookResponse{RequestID: new(req.GetRequestID())}
	var err error
	switch req.GetType() {
	case wire.LifecycleHookOnSessionStart:
		err = r.hooks.dispatchSessionStart(ctx)
		resp.EmptyResult = &wire.EmptyResult{}
	case wire.LifecycleHookOnSessionEnd:
		err = r.hooks.dispatchSessionEnd(ctx)
		resp.EmptyResult = &wire.EmptyResult{}
	case wire.LifecycleHookPreTurn:
		err = r.preTurn(ctx, turn, req, resp)
	case wire.LifecycleHookPostTurn:
		err = r.hooks.dispatchPostTurn(ctx, turn, req.GetPostTurnArgs().GetResponseText())
		resp.EmptyResult = &wire.EmptyResult{}
	case wire.LifecycleHookPreTool:
		err = r.preTool(ctx, turn, req, resp)
	case wire.LifecycleHookPostTool:
		err = r.postTool(ctx, turn, req)
		resp.EmptyResult = &wire.EmptyResult{}
	case wire.LifecycleHookOnToolError:
		r.toolError(ctx, turn, req, resp)
	case wire.LifecycleHookOnCompaction:
		err = r.compaction(ctx, turn, req)
		resp.EmptyResult = &wire.EmptyResult{}
	case wire.LifecycleHookStop:
		err = r.stop(ctx, turn, req, resp)
	default:
		logger.Warn("unknown or unhandled hook", "type", req.GetType(), "name", req.GetName())
		resp.EmptyResult = &wire.EmptyResult{}
	}
	if err != nil {
		logger.Error("hook failed", "name", req.GetName(), "error", err)
		return &wire.CallHookResponse{RequestID: new(req.GetRequestID()), ErrorMessage: new("Hook failed: " + err.Error())}
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
		resp.PreTurnResult = &wire.PreTurnResult{Decision: new(wire.PreTurnResultDecisionDeny), Reason: new(res.Message)}
	} else {
		resp.PreTurnResult = &wire.PreTurnResult{Decision: new(wire.PreTurnResultDecisionAllow)}
	}
	return nil
}

func (r *hookRouter) preTool(ctx context.Context, turn *HookContext, req *wire.CallHookRequest, resp *wire.CallHookResponse) error {
	call := &ToolCall{Args: map[string]any{}}
	if pta := req.GetPreToolArgs(); pta != nil {
		// Unlike elsewhere, arguments that are not a JSON object fail the
		// request, as upstream.
		var args map[string]any
		if s := pta.GetArgumentsJSON(); s != "" {
			if err := json.Unmarshal([]byte(s), &args); err != nil {
				return err
			}
		}
		call = toolCallFromWire(pta.GetToolName(), args, pta.GetCallID(), stepIDOf(pta.GetTrajectoryID(), pta.StepIndex), pta.GetServerName())
	}
	res, err := r.hooks.dispatchPreToolCall(ctx, turn, call)
	if err != nil {
		return err
	}
	if res.Deny {
		resp.PreToolResult = &wire.PreToolResult{Decision: new(wire.PreToolResultDecisionDeny), Reason: new(res.Message)}
		return nil
	}
	ptr := &wire.PreToolResult{Decision: new(wire.PreToolResultDecisionAllow)}
	if res.ModifiedArgs != nil {
		ptr.ModifiedArgs = structOfArgs(res.ModifiedArgs)
	}
	resp.PreToolResult = ptr
	return nil
}

func (r *hookRouter) postTool(ctx context.Context, turn *HookContext, req *wire.CallHookRequest) error {
	result := &ToolResult{}
	if pta := req.GetPostToolArgs(); pta != nil {
		result.Name = sdkToolName(pta.GetToolName())
		result.ServerName = pta.GetServerName()
		result.ID = pta.GetCallID()
		result.StepID = stepIDOf(pta.GetTrajectoryID(), pta.StepIndex)
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
		toolErr.CallID = ote.GetCallID()
		toolErr.StepID = stepIDOf(ote.GetTrajectoryID(), ote.StepIndex)
	}
	recovery := strings.TrimSpace(r.hooks.dispatchToolError(ctx, newHookContext(ScopeOperation, turn), toolErr))
	if recovery != "" {
		resp.OnToolErrorResult = &wire.OnToolErrorResult{CustomErrorMessage: new(recovery)}
		return
	}
	resp.EmptyResult = &wire.EmptyResult{}
}

func (r *hookRouter) compaction(ctx context.Context, turn *HookContext, req *wire.CallHookRequest) error {
	args := req.GetOnCompactionArgs()
	summary := args.GetSummary()
	if summary == "" {
		summary = "Context compaction"
	}
	idx := int(args.GetStepIndex())
	step := &Step{
		ID:           makeStepID(args.GetTrajectoryID(), idx),
		Type:         StepTypeCompaction,
		Status:       StepStatusDone,
		Source:       StepSourceSystem,
		Target:       StepTargetUser,
		Content:      summary,
		TrajectoryID: args.GetTrajectoryID(),
		StepIndex:    idx,
	}
	return r.hooks.dispatchCompaction(ctx, turn, step)
}

func (r *hookRouter) stop(ctx context.Context, turn *HookContext, req *wire.CallHookRequest, resp *wire.CallHookResponse) error {
	sa := req.GetStopArgs()
	args := StopArgs{
		ResponseText:      sa.GetResponseText(),
		TrajectoryID:      sa.GetTrajectoryID(),
		ContinuationCount: int(sa.GetContinuationCount()),
		StopReason:        parseStopReason(sa.GetStopReason()),
		ErrorMessage:      sa.GetErrorMessage(),
	}
	res, err := r.hooks.dispatchStop(ctx, turn, args)
	if err != nil {
		return err
	}
	if res.Decision == StopDecisionContinue {
		resp.StopResult = &wire.StopResult{Decision: new(wire.StopResultDecisionContinue), Reason: new(strings.TrimSpace(res.Reason))}
		return nil
	}
	resp.StopResult = &wire.StopResult{Decision: new(wire.StopResultDecisionAllowStop)}
	return nil
}
