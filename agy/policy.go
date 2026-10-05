package agy

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"

	"github.com/ironpark/gelati/agy/internal/wire"
	"github.com/ironpark/gelati/internal/safecall"
)

// Decision is the outcome a Policy produces when it matches.
type Decision string

// Decision values.
const (
	DecisionApprove Decision = "APPROVE"
	DecisionDeny    Decision = "DENY"
	DecisionAskUser Decision = "ASK_USER"
)

// Predicate narrows a Policy to the calls it returns true for. A predicate
// that fails (returns an error or panics) counts as a match, so a broken
// deny rule still denies. See the policy package for helpers that inspect
// the arguments map or decode it into a struct.
type Predicate func(ctx context.Context, call ToolCall) (bool, error)

// AskUserHandler asks the user to approve a tool call and reports whether
// they did. reason explains why confirmation is requested; it comes from the
// policy or, for auto policies, from the runtime's safety assessment.
type AskUserHandler func(ctx context.Context, call ToolCall, reason string) (bool, error)

// WildcardTool is the Policy.Tool that matches every tool.
const WildcardTool = "*"

// WorkspaceOnlyPolicyName names the policies WorkspaceOnlyPolicies builds.
const WorkspaceOnlyPolicyName = "workspace_only"

// policyKind marks the policies built by AllowAllPolicy and
// WorkspaceOnlyPolicies, whose meaning the harness recognizes. Only those
// constructors set it, so a policy merely named like them is an ordinary
// rule.
type policyKind uint8

const (
	kindRule policyKind = iota
	kindAllowAll
	kindWorkspaceOnly
)

// Policy is one tool call rule. Policies are sent to the harness, which
// evaluates them by priority; rules with a predicate or an ask-user
// decision call back into this process to decide. Most users build
// policies with the policy package:
//
//	Policies: []agy.Policy{
//		policy.DenyAll(),
//		policy.Allow("view_file"),
//		policy.AskUser("run_command", policy.Handler(confirm)),
//	}
//
// Precedence, highest first: specific deny, ask, allow; then server-prefix
// ("server/*") deny, ask, allow; then wildcard ("*") deny, ask, allow.
// Within a level the first matching rule wins, and a call no rule matches
// is allowed.
type Policy struct {
	// Tool is the tool name, "*" for every tool, "server/tool" for one MCP
	// tool or "server/*" for every tool of an MCP server.
	Tool string
	// Decision is applied when the policy matches. Ignored for Auto
	// policies, whose decision is DecisionAskUser with a handler and
	// DecisionDeny without.
	Decision Decision
	// When restricts the policy to matching calls; nil matches all.
	When Predicate
	// AskUser is the handler of a DecisionAskUser policy.
	AskUser AskUserHandler
	// Name labels the policy in logs and denial messages.
	Name string
	// Reason explains a denial, or is passed to AskUser.
	Reason string
	// Auto enables auto policy mode: the runtime assesses risky tool calls
	// (shell commands, URL fetches) with a model before they run, denying
	// flagged ones or asking AskUser. At most one auto policy is allowed.
	Auto bool
	// AutoModel is the Gemini model used for auto policy assessments; empty
	// uses the runtime default.
	AutoModel string

	kind policyKind
}

// normalized returns p with the derived fields of an auto policy filled in.
func (p Policy) normalized() Policy {
	if p.Auto {
		p.Tool = WildcardTool
		p.When = nil
		p.Reason = ""
		if p.Name == "" {
			p.Name = "auto"
		}
		p.Decision = DecisionDeny
		if p.AskUser != nil {
			p.Decision = DecisionAskUser
		}
	}
	return p
}

func (p *Policy) label() string {
	if p.Name != "" {
		return p.Name
	}
	return p.Tool
}

// AllowAllPolicy approves every tool call (upstream policy.allow_all; the
// policy package wraps it as policy.AllowAll). Unless WorkspaceOnlyPolicies
// are also present, it lifts the harness's workspace containment.
func AllowAllPolicy() Policy {
	return Policy{Tool: WildcardTool, Decision: DecisionApprove, Name: "allow_all", kind: kindAllowAll}
}

// WorkspaceOnlyPolicies confine the file tools to the session's workspace
// directories (upstream policy.workspace_only; the policy package wraps it
// as policy.WorkspaceOnly). The harness enforces them natively against
// Options.Workspaces; EnforcePolicies skips them.
func WorkspaceOnlyPolicies() []Policy {
	var out []Policy
	for _, t := range FileTools() {
		out = append(out, Policy{Tool: string(t), Decision: DecisionDeny, Name: WorkspaceOnlyPolicyName, kind: kindWorkspaceOnly})
	}
	return out
}

// ConfirmRunCommandPolicies is the default policy set of Options (upstream
// confirm_run_command; the policy package wraps it as
// policy.ConfirmRunCommand): run_command is denied, or confirmed through
// handler when it is non-nil, and every other tool is allowed.
func ConfirmRunCommandPolicies(handler AskUserHandler) []Policy {
	first := Policy{Tool: string(BuiltinRunCommand), Decision: DecisionDeny, Name: "confirm_run_command"}
	if handler != nil {
		first.Decision, first.AskUser = DecisionAskUser, handler
	}
	return []Policy{first, {Tool: WildcardTool, Decision: DecisionApprove, Name: "confirm_run_command"}}
}

// evaluatePredicate reports whether p's predicate matches call. A nil
// predicate matches; a panic is returned as an error.
func evaluatePredicate(ctx context.Context, p *Policy, call ToolCall) (matched bool, err error) {
	if p.When == nil {
		return true, nil
	}
	defer safecall.Recover(&err, "agy: hook")
	return p.When(ctx, call)
}

// executeAskUser runs p's ask-user handler.
func executeAskUser(ctx context.Context, p *Policy, call ToolCall, reason string) (approved bool, err error) {
	if p.AskUser == nil {
		return false, fmt.Errorf("policy %q has no ask_user handler", p.label())
	}
	defer safecall.Recover(&err, "agy: hook")
	return p.AskUser(ctx, call, reason)
}

// matchesTarget reports whether a policy tool target applies to call.
func matchesTarget(policyTool string, call *ToolCall) bool {
	if policyTool == WildcardTool {
		return true
	}
	if call.ServerName != "" {
		if server, ok := strings.CutSuffix(policyTool, "/*"); ok {
			return server == call.ServerName
		}
		return policyTool == call.ServerName+"/"+call.Name
	}
	return policyTool == call.Name
}

// policyScope orders specific targets before server prefixes before the
// wildcard.
func policyScope(p *Policy) int {
	switch {
	case p.Tool == WildcardTool:
		return 2
	case strings.HasSuffix(p.Tool, "/*"):
		return 1
	}
	return 0
}

func decisionRank(d Decision) int {
	switch d {
	case DecisionDeny:
		return 0
	case DecisionAskUser:
		return 1
	case DecisionApprove:
		return 2
	}
	return 3
}

// sortPolicies orders policies by precedence, keeping declaration order
// within a level.
func sortPolicies(policies []Policy) []Policy {
	out := make([]Policy, len(policies))
	for i, p := range policies {
		out[i] = p.normalized()
	}
	slices.SortStableFunc(out, func(a, b Policy) int {
		return cmp.Or(cmp.Compare(policyScope(&a), policyScope(&b)), cmp.Compare(decisionRank(a.Decision), decisionRank(b.Decision)))
	})
	return out
}

// EnforcePolicies returns a PreToolCallHook that evaluates policies in this
// process, by precedence (see Policy). Auto and workspace-only policies are
// skipped: the runtime enforces those. The hook fails closed: a predicate or
// handler failure denies the call.
//
// Policies that target MCP tools ("server/...") require mcpServers to be
// non-nil, so that matching is checked against the registered servers; pass
// an empty slice when the servers are managed elsewhere. Every
// DecisionAskUser policy needs a handler.
//
// Agents send their policies to the harness instead; this hook is for
// enforcing policies in custom flows. The policy package wraps it as
// policy.Enforce.
func EnforcePolicies(policies []Policy, mcpServers []MCPServer) (PreToolCallHook, error) {
	sorted := sortPolicies(policies)
	if mcpServers == nil {
		for i := range sorted {
			if strings.Contains(sorted[i].Tool, "/") && sorted[i].Tool != WildcardTool {
				return nil, validationErrorf("MCP policies (containing '/') were detected, but 'mcpServers' was not provided to EnforcePolicies. " +
					"You must pass the registered MCP servers to enable secure policy matching and prevent silent bypasses.")
			}
		}
	}
	for i := range sorted {
		if sorted[i].Decision == DecisionAskUser && sorted[i].AskUser == nil {
			return nil, validationErrorf("ASK_USER policy '%s' is missing an ask_user handler. Provide one via policy.AskUser(tool, policy.Handler(...)).", sorted[i].label())
		}
	}
	return func(ctx context.Context, _ *HookContext, call *ToolCall) (HookResult, error) {
		return evaluatePolicies(ctx, sorted, call), nil
	}, nil
}

// evaluatePolicies applies the first matching policy of sorted to call.
func evaluatePolicies(ctx context.Context, sorted []Policy, call *ToolCall) HookResult {
	for i := range sorted {
		p := &sorted[i]
		if p.kind == kindWorkspaceOnly || p.Auto {
			// Enforced by the runtime.
			continue
		}
		if !matchesTarget(p.Tool, call) {
			continue
		}
		matched, err := evaluatePredicate(ctx, p, *call)
		if err != nil {
			return HookResult{Deny: true, Message: fmt.Sprintf("Policy evaluation failed for policy '%s': %v", p.label(), err)}
		}
		if !matched {
			continue
		}
		switch p.Decision {
		case DecisionDeny:
			return HookResult{Deny: true, Message: cmp.Or(p.Reason, fmt.Sprintf("Denied by policy '%s'.", p.label()))}
		case DecisionApprove:
			return HookResult{}
		case DecisionAskUser:
			if p.AskUser == nil {
				continue
			}
			approved, err := executeAskUser(ctx, p, *call, p.Reason)
			if err != nil {
				return HookResult{Deny: true, Message: fmt.Sprintf("Policy evaluation failed for policy '%s': %v", p.label(), err)}
			}
			if approved {
				return HookResult{}
			}
			return HookResult{Deny: true, Message: cmp.Or(p.Reason, fmt.Sprintf("User denied tool '%s' (policy '%s').", call.Name, p.label()))}
		}
	}
	return HookResult{}
}

// policyTarget splits a policy tool target into the tool and server names
// of a wire PolicyRule.
func policyTarget(tool string) (toolName, serverName string) {
	if tool == WildcardTool {
		return WildcardTool, ""
	}
	if server, name, ok := strings.Cut(tool, "/"); ok {
		return name, server
	}
	return tool, ""
}

var wireDecisions = map[Decision]wire.PolicyDecision{
	DecisionApprove: wire.PolicyDecision_POLICY_DECISION_ALLOW,
	DecisionDeny:    wire.PolicyDecision_POLICY_DECISION_DENY,
	DecisionAskUser: wire.PolicyDecision_POLICY_DECISION_ASK_USER,
}

// policyConfig serializes policies for the harness. Static rules are
// enforced entirely by the harness; dynamic ones (with a predicate, or
// asking the user) carry a rule ID and are evaluated by this process when
// the harness sends a PolicyDecisionRequest. It returns the dynamic rules by
// ID; the auto policy, if any, is registered as "auto".
func policyConfig(policies []Policy) (*wire.PolicyConfig, map[string]*Policy, error) {
	dynamic := make(map[string]*Policy)
	var rules []*wire.PolicyRule
	var auto *Policy
	hasWorkspaceOnly, hasAllowAll := false, false
	for i, p := range policies {
		p := p.normalized()
		if p.Auto {
			if auto != nil {
				return nil, nil, validationErrorf("Multiple AutoPolicy rules found; at most one policy.Auto() rule may be specified.")
			}
			auto = &p
			continue
		}
		decision, ok := wireDecisions[p.Decision]
		if !ok {
			return nil, nil, validationErrorf("policy %q has unknown decision %q", p.label(), p.Decision)
		}
		toolName, serverName := policyTarget(p.Tool)
		workspaceOnly := p.kind == kindWorkspaceOnly
		hasWorkspaceOnly = hasWorkspaceOnly || workspaceOnly
		if p.kind == kindAllowAll && p.Tool == WildcardTool && p.Decision == DecisionApprove && p.When == nil {
			hasAllowAll = true
		}
		isDynamic := (p.When != nil || p.Decision == DecisionAskUser) && !workspaceOnly
		ruleID := ""
		if isDynamic {
			ruleID = "rule_" + strconv.Itoa(i)
			dynamic[ruleID] = &p
		}
		rules = append(rules, wire.PolicyRule_builder{
			Tool:       new(toolName),
			ServerName: new(serverName),
			Decision:   new(decision),
			Name:       new(p.label()),
			IsDynamic:  new(isDynamic),
			RuleId:     new(ruleID),
			DenyReason: new(p.Reason),
		}.Build())
	}
	cfg := wire.PolicyConfig_builder{Rules: rules}.Build()
	containment := wire.PolicyConfig_WORKSPACE_CONTAINMENT_UNSPECIFIED
	if hasAllowAll && !hasWorkspaceOnly {
		containment = wire.PolicyConfig_WORKSPACE_CONTAINMENT_DISABLED
	}
	cfg.SetWorkspaceContainment(containment)
	if auto != nil {
		cfg.SetAutoConfig(wire.AutoPolicyConfig_builder{Enabled: new(true), Model: new(auto.AutoModel)}.Build())
		dynamic["auto"] = auto
	}
	return cfg, dynamic, nil
}

// decidePolicy answers a PolicyDecisionRequest for a dynamic rule.
func decidePolicy(ctx context.Context, logger *slog.Logger, dynamic map[string]*Policy, req *wire.PolicyDecisionRequest) *wire.PolicyDecisionResponse {
	resp := func(outcome wire.PolicyEvaluationOutcome, reason string) *wire.PolicyDecisionResponse {
		return wire.PolicyDecisionResponse_builder{RequestId: new(req.GetRequestId()), Outcome: &outcome, DenyReason: new(reason)}.Build()
	}
	ruleID := req.GetRuleId()
	p := dynamic[ruleID]
	if p == nil {
		logger.Error("unknown policy rule_id", "rule_id", ruleID)
		return resp(wire.PolicyEvaluationOutcome_POLICY_EVALUATION_OUTCOME_DENY, "Unknown rule_id: "+ruleID)
	}
	ta := req.GetToolArgs()
	call := *toolCallFromWire(ta.GetToolName(), argsFromJSON(ta.GetArgumentsJson()), "", "", ta.GetServerName())
	if p.When != nil {
		matched, err := evaluatePredicate(ctx, p, call)
		if err != nil {
			logger.Error("policy evaluation failed", "rule_id", ruleID, "error", err)
			return resp(wire.PolicyEvaluationOutcome_POLICY_EVALUATION_OUTCOME_DENY, "Policy evaluation error: "+err.Error())
		}
		if !matched {
			return resp(wire.PolicyEvaluationOutcome_POLICY_EVALUATION_OUTCOME_NO_MATCH, "")
		}
	}
	switch {
	case p.AskUser != nil:
		reason := cmp.Or(req.GetReason(), p.Reason)
		approved, err := executeAskUser(ctx, p, call, reason)
		if err != nil {
			logger.Error("policy evaluation failed", "rule_id", ruleID, "error", err)
			return resp(wire.PolicyEvaluationOutcome_POLICY_EVALUATION_OUTCOME_DENY, "Policy evaluation error: "+err.Error())
		}
		if approved {
			return resp(wire.PolicyEvaluationOutcome_POLICY_EVALUATION_OUTCOME_ALLOW, "")
		}
		return resp(wire.PolicyEvaluationOutcome_POLICY_EVALUATION_OUTCOME_DENY, cmp.Or(reason, fmt.Sprintf("Denied by user (%s).", p.label())))
	case p.Decision == DecisionAskUser:
		msg := fmt.Sprintf("Policy '%s' requires ask_user handler, but none was provided.", p.label())
		logger.Error(msg)
		return resp(wire.PolicyEvaluationOutcome_POLICY_EVALUATION_OUTCOME_DENY, msg)
	case p.Decision == DecisionDeny:
		return resp(wire.PolicyEvaluationOutcome_POLICY_EVALUATION_OUTCOME_DENY, cmp.Or(req.GetReason(), fmt.Sprintf("Denied by policy '%s'.", p.label())))
	case p.Decision == DecisionApprove:
		return resp(wire.PolicyEvaluationOutcome_POLICY_EVALUATION_OUTCOME_ALLOW, "")
	}
	msg := fmt.Sprintf("Unhandled policy decision '%s' for '%s'.", p.Decision, p.label())
	logger.Error(msg)
	return resp(wire.PolicyEvaluationOutcome_POLICY_EVALUATION_OUTCOME_DENY, msg)
}
