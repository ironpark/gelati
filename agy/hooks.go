package agy

import "context"

// Hooks intercept, observe and modify the agent at points of its lifecycle.
// A hook is one of the function types below converted to Hook, listed in
// Config.Hooks:
//
//	Hooks: []agy.Hook{
//		agy.PreToolCallHook(func(ctx context.Context, hc *agy.HookContext, call *agy.ToolCall) (agy.HookResult, error) {
//			if call.Name == string(agy.BuiltinRunCommand) {
//				return agy.HookResult{Deny: true, Message: "no shell"}, nil
//			}
//			return agy.HookResult{}, nil
//		}),
//	}
//
// Hooks come in three kinds. Inspect hooks (session start and end, post
// turn, post tool call, compaction) observe and cannot block. Decide hooks
// (pre turn, pre tool call) return a HookResult that allows or denies.
// Transform hooks (tool error, interaction, stop) return replacement data.
// Hooks of one kind run in registration order; a decide hook that denies
// stops the chain.
//
// The harness calls the hooks it was told about at session start (it only
// dispatches the kinds that have at least one hook), and every hook runs in
// the Go process. A hook that returns an error fails the request: the
// harness receives the error message.
type Hook interface{ hookKind() hookKind }

type hookKind int

const (
	hookSessionStart hookKind = iota
	hookSessionEnd
	hookPreTurn
	hookPostTurn
	hookPreToolCall
	hookPostToolCall
	hookToolError
	hookInteraction
	hookCompaction
	hookStop
	numHookKinds
)

// OnSessionStartHook runs when the session starts.
type OnSessionStartHook func(ctx context.Context, hc *HookContext) error

// OnSessionEndHook runs when the session ends, during Agent.Close.
type OnSessionEndHook func(ctx context.Context, hc *HookContext) error

// PreTurnHook runs before a turn starts, with the user's prompt (a single
// empty Text when the prompt was empty). Denying cancels the turn: the
// response ends with an *ExecutionError carrying the message.
type PreTurnHook func(ctx context.Context, hc *HookContext, prompt []Content) (HookResult, error)

// PostTurnHook runs after a turn ends, with the model's final response text.
type PostTurnHook func(ctx context.Context, hc *HookContext, response string) error

// PreToolCallHook decides whether a tool call (builtin, custom or MCP, in
// the root agent or a subagent) may run. It can allow the call, allow it
// with ModifiedArgs merged over the arguments, or deny it with a message
// the model sees. Hooks run in order, each seeing the arguments as modified
// by the previous ones. A PreToolCallHook also counts as a safety policy for
// the Agent's write-tool check.
type PreToolCallHook func(ctx context.Context, hc *HookContext, call *ToolCall) (HookResult, error)

// PostToolCallHook runs after a tool call completes, successfully or not.
// For builtin tools ToolResult.Result holds the structured result types of
// this package when the output parses.
type PostToolCallHook func(ctx context.Context, hc *HookContext, result *ToolResult) error

// OnToolErrorHook runs when a tool call fails. Returning a non-empty message
// replaces the error text the model sees; returning "" defers to the next
// hook and finally to the default message. Upstream also stops at a hook
// that returns an empty (non-None) value, with the same visible result.
type OnToolErrorHook func(ctx context.Context, hc *HookContext, err *ToolExecutionError) (string, error)

// OnInteractionHook answers the questions the agent asks the user (the
// ask_question tool). Returning nil defers to the next hook; when no hook
// answers, every question is reported unanswered.
type OnInteractionHook func(ctx context.Context, hc *HookContext, spec AskQuestionInteractionSpec) (*QuestionHookResult, error)

// OnCompactionHook observes context compaction. The step has type
// StepTypeCompaction and the compaction summary as Content.
type OnCompactionHook func(ctx context.Context, hc *HookContext, step *Step) error

// StopHook runs when the root trajectory is about to finish a turn and
// decides whether to stop or to continue with an injected system prompt.
// The first hook that returns StopDecisionContinue wins.
type StopHook func(ctx context.Context, hc *HookContext, args StopArgs) (StopHookResult, error)

func (OnSessionStartHook) hookKind() hookKind { return hookSessionStart }
func (OnSessionEndHook) hookKind() hookKind   { return hookSessionEnd }
func (PreTurnHook) hookKind() hookKind        { return hookPreTurn }
func (PostTurnHook) hookKind() hookKind       { return hookPostTurn }
func (PreToolCallHook) hookKind() hookKind    { return hookPreToolCall }
func (PostToolCallHook) hookKind() hookKind   { return hookPostToolCall }
func (OnToolErrorHook) hookKind() hookKind    { return hookToolError }
func (OnInteractionHook) hookKind() hookKind  { return hookInteraction }
func (OnCompactionHook) hookKind() hookKind   { return hookCompaction }
func (StopHook) hookKind() hookKind           { return hookStop }

// HookResult is the outcome of a decide hook. The zero value allows.
// Upstream's allow=False maps to Deny: true.
type HookResult struct {
	// Deny blocks the operation.
	Deny bool
	// Message explains a denial; the model sees it.
	Message string
	// ModifiedArgs, on an allowing PreToolCallHook result, are merged over
	// the tool call's arguments (overwriting the given keys) before it runs.
	ModifiedArgs map[string]any
}

// StopDecision is the decision of a StopHook.
type StopDecision string

// StopDecision values. The zero value means StopDecisionAllowStop.
const (
	// StopDecisionAllowStop lets the turn finish.
	StopDecisionAllowStop StopDecision = "ALLOW_STOP"
	// StopDecisionContinue blocks termination, injects StopHookResult.Reason
	// as a system message and resumes the agent loop.
	StopDecisionContinue StopDecision = "CONTINUE"
)

// StopHookResult is the outcome of a StopHook. A StopDecisionContinue
// result must carry a non-empty Reason.
type StopHookResult struct {
	Decision StopDecision
	Reason   string
}

// StopArgs describes the turn a StopHook is asked about.
type StopArgs struct {
	// ResponseText is the most recent assistant response of the turn.
	ResponseText string
	TrajectoryID string
	// ContinuationCount is the 0-based number of StopHook continuations in
	// the current turn.
	ContinuationCount int
	StopReason        StopReason
	// ErrorMessage is set when the turn stopped on a fatal error.
	ErrorMessage string
}

// AskQuestionOption is one option of a question.
type AskQuestionOption struct {
	ID   string
	Text string
}

// AskQuestionEntry is one question with its options.
type AskQuestionEntry struct {
	Question      string
	Options       []AskQuestionOption
	IsMultiSelect bool
}

// AskQuestionInteractionSpec is what OnInteractionHook is asked to answer.
type AskQuestionInteractionSpec struct {
	Questions []AskQuestionEntry
}

// QuestionResponse answers one question.
type QuestionResponse struct {
	// SelectedOptionIDs are the AskQuestionOption IDs chosen.
	SelectedOptionIDs []string
	// FreeformResponse is a written answer.
	FreeformResponse string
	// Skipped leaves the question unanswered.
	Skipped bool
}

// QuestionHookResult answers an AskQuestionInteractionSpec, one response
// per question in order.
type QuestionHookResult struct {
	Responses []QuestionResponse
	Cancelled bool
}

// HookScope is the lifetime of a HookContext.
type HookScope int

// HookScope values.
const (
	// ScopeSession lives for the whole session.
	ScopeSession HookScope = iota
	// ScopeTurn lives for one turn (prompt to response).
	ScopeTurn
	// ScopeOperation lives for one operation, such as a tool call.
	ScopeOperation
)

// HookContext is the state shared between hooks. Contexts nest: an
// operation context's parent is its turn context, whose parent is the
// session context. State set in a broader scope is visible in narrower
// ones, not the other way round. Hook state is separate from the
// ToolContext state of tools.
type HookContext struct {
	StateStore
	scope  HookScope
	parent *HookContext
}

func newHookContext(scope HookScope, parent *HookContext) *HookContext {
	hc := &HookContext{scope: scope, parent: parent}
	if parent != nil {
		hc.StateStore.parent = &parent.StateStore
	}
	return hc
}

// Scope returns the context's scope.
func (hc *HookContext) Scope() HookScope { return hc.scope }

// Parent returns the enclosing context, or nil for the session context.
func (hc *HookContext) Parent() *HookContext { return hc.parent }
