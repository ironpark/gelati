// Package policy builds tool call policies for antigravity agents, mirroring
// the upstream google.antigravity.hooks.policy module:
//
//	cfg := agy.Config{
//		Policies: []agy.Policy{
//			policy.DenyAll(),
//			policy.Allow("view_file"),
//			policy.Deny("run_command", policy.WhenArgs(func(args map[string]any) bool {
//				cmd, _ := args["CommandLine"].(string)
//				return strings.Contains(cmd, "rm ")
//			})),
//			policy.AskUser("run_command", policy.Handler(confirm)),
//		},
//	}
//
// Policies are evaluated by precedence (see agy.Policy): specific
// rules before server prefixes before the wildcard, and within each, deny
// before ask before allow; the first match of a level wins, and a call no
// rule matches is allowed.
//
// Policies leave a tool visible to the model and reject calls at run time.
// To hide a tool from the model altogether, use
// agy.CapabilitiesConfig.DisabledTools or EnabledTools instead.
package policy

import (
	"context"

	"github.com/ironpark/gelati/agy"
)

// Option customizes a policy built by this package.
type Option func(*agy.Policy)

// When restricts the policy to the calls pred matches. A predicate that
// fails counts as a match, so a broken deny rule still denies.
func When(pred agy.Predicate) Option {
	return func(p *agy.Policy) { p.When = pred }
}

// WhenArgs restricts the policy to calls whose arguments pred matches.
func WhenArgs(pred func(args map[string]any) bool) Option {
	return When(func(_ context.Context, call agy.ToolCall) (bool, error) {
		return pred(call.Args), nil
	})
}

// WhenTyped restricts the policy to calls whose arguments, decoded into T
// through encoding/json, pred matches. Arguments that do not decode count
// as a match (fail closed). It is the Go form of an upstream predicate
// annotated with a pydantic model.
func WhenTyped[T any](pred func(args T) bool) Option {
	return When(func(_ context.Context, call agy.ToolCall) (bool, error) {
		var v T
		if err := call.DecodeArgs(&v); err != nil {
			return false, err
		}
		return pred(v), nil
	})
}

// Name labels the policy in logs and denial messages. For MCP policies
// covering several tools, each policy's name gets the tool name appended.
func Name(name string) Option {
	return func(p *agy.Policy) { p.Name = name }
}

// Reason explains why the policy matched: the message of a denial, or the
// reason passed to an ask-user handler.
func Reason(reason string) Option {
	return func(p *agy.Policy) { p.Reason = reason }
}

// Handler sets the handler of an AskUser or Auto policy.
func Handler(h agy.AskUserHandler) Option {
	return func(p *agy.Policy) { p.AskUser = h }
}

// Model sets the model used by an Auto policy's safety assessments.
func Model(model string) Option {
	return func(p *agy.Policy) { p.AutoModel = model }
}

func build(tool string, decision agy.Decision, opts []Option) agy.Policy {
	p := agy.Policy{Tool: tool, Decision: decision}
	for _, o := range opts {
		o(&p)
	}
	return p
}

// Allow approves calls to tool ("*" for every tool, "server/tool" or
// "server/*" for MCP tools).
func Allow(tool string, opts ...Option) agy.Policy {
	return build(tool, agy.DecisionApprove, opts)
}

// Deny rejects calls to tool.
func Deny(tool string, opts ...Option) agy.Policy {
	return build(tool, agy.DecisionDeny, opts)
}

// AskUser asks a handler (see Handler) to approve calls to tool. Without a
// handler the confirmation is left to the host platform; Enforce rejects
// such policies.
func AskUser(tool string, opts ...Option) agy.Policy {
	return build(tool, agy.DecisionAskUser, opts)
}

// mcp builds MCP policies: one server-wide "server/*" policy when tools is
// nil, else one "server/tool" policy per tool.
func mcp(decision agy.Decision, server agy.MCPServer, tools []string, opts []Option) []agy.Policy {
	name := server.ServerName()
	prefix := map[agy.Decision]string{
		agy.DecisionApprove: "approve",
		agy.DecisionDeny:    "deny",
		agy.DecisionAskUser: "ask_user",
	}[decision]
	if tools == nil {
		p := build(name+"/*", decision, opts)
		if p.Name == "" {
			p.Name = prefix + "_" + name + "_all"
		}
		return []agy.Policy{p}
	}
	out := make([]agy.Policy, 0, len(tools))
	for _, t := range tools {
		p := build(name+"/"+t, decision, opts)
		if p.Name == "" {
			p.Name = prefix + "_" + name + "_" + t
		} else {
			p.Name += "_" + t
		}
		out = append(out, p)
	}
	return out
}

// AllowMCP approves the listed tools of an MCP server, or all of its tools
// when tools is nil.
func AllowMCP(server agy.MCPServer, tools []string, opts ...Option) []agy.Policy {
	return mcp(agy.DecisionApprove, server, tools, opts)
}

// DenyMCP rejects the listed tools of an MCP server, or all of its tools
// when tools is nil.
func DenyMCP(server agy.MCPServer, tools []string, opts ...Option) []agy.Policy {
	return mcp(agy.DecisionDeny, server, tools, opts)
}

// AskUserMCP asks for approval of the listed tools of an MCP server, or of
// all of its tools when tools is nil.
func AskUserMCP(server agy.MCPServer, tools []string, opts ...Option) []agy.Policy {
	return mcp(agy.DecisionAskUser, server, tools, opts)
}

// AllowAll approves every tool call. Meant for autonomous agents and local
// development.
func AllowAll() agy.Policy {
	return agy.AllowAllPolicy()
}

// DenyAll denies every tool call. Combine it with specific Allow rules for
// a deny-by-default posture; specific rules take precedence.
func DenyAll() agy.Policy {
	return agy.Policy{Tool: agy.WildcardTool, Decision: agy.DecisionDeny, Name: "deny_all"}
}

// SafeDefaults allows the read-only (and deprecated read-only) tools and
// asks handler about every other call.
func SafeDefaults(handler agy.AskUserHandler) []agy.Policy {
	var out []agy.Policy
	for _, t := range agy.PolicyFreeTools() {
		out = append(out, Allow(string(t)))
	}
	return append(out, AskUser(agy.WildcardTool, Handler(handler)))
}

// ConfirmRunCommand allows every tool except run_command, which is denied,
// or, with a non-nil handler, confirmed through it. This is the default
// policy set of agy.Config.
func ConfirmRunCommand(handler agy.AskUserHandler) []agy.Policy {
	return agy.ConfirmRunCommandPolicies(handler)
}

// WorkspaceOnly confines the file tools to the session's workspace
// directories. The harness enforces it natively against Config.Workspaces,
// so workspaces is informational, as upstream.
func WorkspaceOnly(workspaces ...string) []agy.Policy {
	_ = workspaces
	return agy.WorkspaceOnlyPolicies()
}

// Auto enables auto policy mode: the runtime assesses calls with execution
// or network risk (shell commands, URL fetches) with a model before they
// run, and denies flagged calls or, with a Handler, asks it. Specific rules
// run before it and wildcard rules after it. Options: Name (default
// "auto"), Handler and Model.
func Auto(opts ...Option) agy.Policy {
	p := agy.Policy{Auto: true}
	for _, o := range opts {
		o(&p)
	}
	return p
}

// Enforce returns a hook that evaluates policies in this process; see
// agy.EnforcePolicies.
func Enforce(policies []agy.Policy, mcpServers []agy.MCPServer) (agy.PreToolCallHook, error) {
	return agy.EnforcePolicies(policies, mcpServers)
}
