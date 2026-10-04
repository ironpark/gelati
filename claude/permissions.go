package claude

import (
	"context"
	"encoding/json"
)

// PermissionMode selects how the session handles permission prompts.
type PermissionMode = string

// Supported permission modes.
const (
	PermissionModeDefault           PermissionMode = "default"
	PermissionModeAcceptEdits       PermissionMode = "acceptEdits"
	PermissionModePlan              PermissionMode = "plan"
	PermissionModeBypassPermissions PermissionMode = "bypassPermissions"
	PermissionModeDontAsk           PermissionMode = "dontAsk"
	PermissionModeAuto              PermissionMode = "auto"
)

// PermissionUpdateDestination selects where a permission update is persisted.
type PermissionUpdateDestination = string

// Supported permission update destinations.
const (
	DestinationUserSettings    PermissionUpdateDestination = "userSettings"
	DestinationProjectSettings PermissionUpdateDestination = "projectSettings"
	DestinationLocalSettings   PermissionUpdateDestination = "localSettings"
	DestinationSession         PermissionUpdateDestination = "session"
	// DestinationCLIArg is the in-memory layer of rules passed on the
	// command line (--allowedTools and friends).
	DestinationCLIArg PermissionUpdateDestination = "cliArg"
)

// PermissionBehavior is the effect of a permission rule.
type PermissionBehavior = string

// Supported permission behaviors.
const (
	BehaviorAllow PermissionBehavior = "allow"
	BehaviorDeny  PermissionBehavior = "deny"
	BehaviorAsk   PermissionBehavior = "ask"
)

// PermissionRuleValue names a tool and, optionally, the rule content that
// narrows the match (e.g. a Bash command prefix).
type PermissionRuleValue struct {
	ToolName string `json:"toolName"`
	// RuleContent is nil for a rule without content, which omits the key
	// rather than sending null, like the TypeScript SDK.
	RuleContent *string `json:"ruleContent,omitempty"`
}

// Permission update kinds.
const (
	PermissionUpdateAddRules          = "addRules"
	PermissionUpdateReplaceRules      = "replaceRules"
	PermissionUpdateRemoveRules       = "removeRules"
	PermissionUpdateSetMode           = "setMode"
	PermissionUpdateAddDirectories    = "addDirectories"
	PermissionUpdateRemoveDirectories = "removeDirectories"
)

// PermissionUpdate is one change to the session's permission configuration.
// Only the fields relevant to Type are put on the wire.
type PermissionUpdate struct {
	Type        string                      `json:"type"`
	Rules       []PermissionRuleValue       `json:"rules,omitzero"`
	Behavior    PermissionBehavior          `json:"behavior,omitempty"`
	Mode        PermissionMode              `json:"mode,omitempty"`
	Directories []string                    `json:"directories,omitzero"`
	Destination PermissionUpdateDestination `json:"destination,omitempty"`
}

// MarshalJSON emits the control-protocol shape, matching the TypeScript SDK:
// fields that do not belong to Type are left out.
func (u PermissionUpdate) MarshalJSON() ([]byte, error) {
	type alias PermissionUpdate
	out := alias{Type: u.Type, Destination: u.Destination}
	switch u.Type {
	case PermissionUpdateAddRules, PermissionUpdateReplaceRules, PermissionUpdateRemoveRules:
		out.Rules, out.Behavior = u.Rules, u.Behavior
	case PermissionUpdateSetMode:
		out.Mode = u.Mode
	case PermissionUpdateAddDirectories, PermissionUpdateRemoveDirectories:
		out.Directories = u.Directories
	}
	return json.Marshal(out)
}

// UnmarshalJSON reads the control-protocol shape produced by MarshalJSON. A
// member of an unexpected JSON type is left at its zero value.
func (u *PermissionUpdate) UnmarshalJSON(data []byte) error {
	type alias PermissionUpdate
	var a alias
	if err := json.Unmarshal(data, &a); fatalDecodeErr(err) {
		return err
	}
	*u = PermissionUpdate(a)
	return nil
}

// ToolPermissionContext is the context handed to a CanUseTool callback.
type ToolPermissionContext struct {
	// Suggestions holds permission updates the CLI proposes.
	Suggestions []PermissionUpdate `json:"permission_suggestions,omitempty"`
	// ToolUseID identifies this specific tool call. Always non-empty.
	ToolUseID string `json:"tool_use_id"`
	// AgentID is set when the call originates inside a sub-agent.
	AgentID string `json:"agent_id,omitempty"`
	// BlockedPath is the file path that triggered the request, if any.
	BlockedPath string `json:"blocked_path,omitempty"`
	// DecisionReason explains why the request was triggered.
	DecisionReason string `json:"decision_reason,omitempty"`
	// Title is the full permission prompt sentence, when provided.
	Title string `json:"title,omitempty"`
	// DisplayName is a short noun phrase for the action.
	DisplayName string `json:"display_name,omitempty"`
	// Description is a human-readable subtitle for the permission UI.
	Description string `json:"description,omitempty"`
	// MCPServer is set for mcp__* tools: the server serving the tool and
	// where its definition came from. Source "sdk" means one of the
	// in-process servers this host registered; any other value is a server
	// from configuration whose name is untrusted text. Key trust decisions
	// on Source, never on the name or the tool-name prefix. nil for
	// non-MCP tools and on CLIs that predate the field.
	MCPServer *MCPServerProvenance `json:"mcp_server,omitempty"`
	// DefaultToNo asks the host to open the prompt on its decline option and
	// offer no one-key approve shortcut.
	DefaultToNo bool `json:"default_to_no,omitempty"`
	// SuppressAlwaysAllowRule asks the host not to offer a persistent
	// "don't ask again" choice: the rule it would write grants more than
	// this request's own action.
	SuppressAlwaysAllowRule bool `json:"suppress_always_allow_rule,omitempty"`
	// MatchedAskRule is set when a user-configured ask rule forced this
	// prompt. Host-side auto-approval should treat such requests as
	// rule-forced: the user asked for a human prompt.
	MatchedAskRule *MatchedAskRule `json:"matched_ask_rule,omitempty"`
	// RequiresUserInteraction reports whether the CLI considers a human
	// answer necessary; nil when the CLI did not say.
	RequiresUserInteraction *bool `json:"requires_user_interaction,omitempty"`
	// RequestID is the control request's request_id. A reply sent out of
	// band (see ErrRespondedOutOfBand) must echo it.
	RequestID string `json:"-"`
	// Raw is the complete request payload, including fields not modeled
	// above (decision_reason_type, classifier_approvable, ...).
	Raw map[string]any `json:"-"`
}

// MCPServerProvenance identifies the MCP server behind a tool and where its
// definition came from.
type MCPServerProvenance struct {
	// Name is the server name as registered or configured.
	Name string `json:"name"`
	// Source is "sdk" for an in-process server this host registered, or a
	// configuration source (plugin, user, project, local, dynamic, managed,
	// enterprise, claudeai, agent, ...). The set is open: treat unknown
	// values as an unrecognized configured source, never as "sdk".
	Source string `json:"source"`
}

// MatchedAskRule is the user-configured ask rule that forced a permission
// prompt.
type MatchedAskRule struct {
	Source   string `json:"source"`
	ToolName string `json:"tool_name"`
	// RuleContent narrows the rule, when it has content.
	RuleContent *string `json:"rule_content,omitempty"`
}

// PermissionDecisionClassification classifies a permission decision for
// telemetry. Hosts that prompt users should report what actually happened;
// when unset the CLI infers it conservatively (temporary for allow, reject
// for deny).
type PermissionDecisionClassification = string

// Permission decision classifications.
const (
	// DecisionUserTemporary is an allow-once decision.
	DecisionUserTemporary PermissionDecisionClassification = "user_temporary"
	// DecisionUserPermanent is an always-allow decision, both the click and
	// later cache hits.
	DecisionUserPermanent PermissionDecisionClassification = "user_permanent"
	// DecisionUserReject is a denial.
	DecisionUserReject PermissionDecisionClassification = "user_reject"
)

// PermissionResult is the answer to a permission request: either
// *PermissionResultAllow or *PermissionResultDeny.
type PermissionResult interface {
	isPermissionResult()
}

// PermissionResultAllow allows the tool call, optionally rewriting its input or
// adding permission rules.
type PermissionResultAllow struct {
	UpdatedInput       map[string]any
	UpdatedPermissions []PermissionUpdate
	// DecisionClassification reports how the user decided; optional.
	DecisionClassification PermissionDecisionClassification
}

func (*PermissionResultAllow) isPermissionResult() {}

// PermissionResultDeny denies the tool call. Interrupt additionally aborts the
// turn.
type PermissionResultDeny struct {
	Message   string
	Interrupt bool
	// DecisionClassification reports how the user decided; optional.
	DecisionClassification PermissionDecisionClassification
}

func (*PermissionResultDeny) isPermissionResult() {}

// CanUseTool decides whether a tool call that would otherwise prompt the user
// may proceed. Returning an error surfaces as a control-protocol error to the
// CLI. Returning an error wrapping ErrRespondedOutOfBand sends no reply at
// all, for hosts that already answered the request some other way; the tool
// stays blocked if nobody did, since permission prompts have no deadline.
type CanUseTool func(ctx context.Context, toolName string, input map[string]any, permCtx ToolPermissionContext) (PermissionResult, error)

// ---------------------------------------------------------------------------
// Wire helpers
// ---------------------------------------------------------------------------

// toolPermissionContext builds the CanUseTool context from a can_use_tool
// request, as the TypeScript SDK maps it. Members of an unexpected JSON type
// are left unset (nil for the optional ones), and suggestions that are not
// well-formed objects are dropped; Raw still has everything.
func toolPermissionContext(requestID string, request map[string]any) ToolPermissionContext {
	permCtx := ToolPermissionContext{
		ToolUseID:               str(request["tool_use_id"]),
		AgentID:                 str(request["agent_id"]),
		BlockedPath:             str(request["blocked_path"]),
		DecisionReason:          str(request["decision_reason"]),
		Title:                   str(request["title"]),
		DisplayName:             str(request["display_name"]),
		Description:             str(request["description"]),
		DefaultToNo:             request["default_to_no"] == true,
		SuppressAlwaysAllowRule: request["suppress_always_allow_rule"] == true,
		RequestID:               requestID,
		Raw:                     request,
	}
	if suggestions, ok := request["permission_suggestions"].([]any); ok {
		for _, s := range suggestions {
			// Decoded strictly, through the method-less alias, so a
			// malformed suggestion is dropped rather than echoed back.
			type strictUpdate PermissionUpdate
			var update strictUpdate
			if m, ok := s.(map[string]any); ok && decodeResponse(m, &update) == nil {
				permCtx.Suggestions = append(permCtx.Suggestions, PermissionUpdate(update))
			}
		}
	}
	if server, ok := request["mcp_server"].(map[string]any); ok {
		permCtx.MCPServer = &MCPServerProvenance{Name: str(server["name"]), Source: str(server["source"])}
	}
	if b, ok := request["requires_user_interaction"].(bool); ok {
		permCtx.RequiresUserInteraction = &b
	}
	if rule, ok := request["matched_ask_rule"].(map[string]any); ok {
		matched := &MatchedAskRule{Source: str(rule["source"]), ToolName: str(rule["tool_name"])}
		if content, ok := rule["rule_content"].(string); ok {
			matched.RuleContent = &content
		}
		permCtx.MatchedAskRule = matched
	}
	return permCtx
}

// permissionDecisionWire renders result as a permission decision object:
// {"behavior": "allow", ...} or {"behavior": "deny", ...}. ok is false when
// result is not a *PermissionResultAllow or *PermissionResultDeny (nil
// included); callers report that in their own words.
//
// With a nil request it renders the decision of a PermissionRequest hook
// output, where optional members are left out when empty. With the
// can_use_tool request it answers, it renders the control reply: an allow
// always carries updatedInput (the request's input when UpdatedInput is nil),
// a deny always carries message, and stampPermissionReply's fields are added.
func permissionDecisionWire(result PermissionResult, request map[string]any) (out map[string]any, ok bool) {
	reply := request != nil
	switch r := result.(type) {
	case *PermissionResultAllow:
		if r == nil {
			return nil, false
		}
		out = map[string]any{"behavior": BehaviorAllow}
		switch {
		case reply && r.UpdatedInput == nil:
			out["updatedInput"], _ = request["input"].(map[string]any)
		case reply || r.UpdatedInput != nil:
			out["updatedInput"] = r.UpdatedInput
		}
		if r.UpdatedPermissions != nil {
			out["updatedPermissions"] = r.UpdatedPermissions
		}
		if reply {
			stampPermissionReply(out, request, r.DecisionClassification)
		}
	case *PermissionResultDeny:
		if r == nil {
			return nil, false
		}
		out = map[string]any{"behavior": BehaviorDeny}
		if r.Message != "" || reply {
			out["message"] = r.Message
		}
		if r.Interrupt {
			out["interrupt"] = true
		}
		if reply {
			stampPermissionReply(out, request, r.DecisionClassification)
		}
	default:
		return nil, false
	}
	return out, true
}

// stampPermissionReply adds the fields the TypeScript SDK puts on every
// permission reply: the request's tool_use_id echoed as toolUseID, and the
// optional decision classification.
func stampPermissionReply(out, request map[string]any, classification PermissionDecisionClassification) {
	if id, ok := request["tool_use_id"].(string); ok {
		out["toolUseID"] = id
	}
	if classification != "" {
		out["decisionClassification"] = classification
	}
}
