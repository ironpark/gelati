// Package policy builds tool call policies for antigravity agents, mirroring
// the upstream google.antigravity.hooks.policy module:
//
//	cfg := antigravity.Config{
//		Policies: []antigravity.Policy{
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
// Policies are evaluated by precedence (see antigravity.Policy): specific
// rules before server prefixes before the wildcard, and within each, deny
// before ask before allow; the first match of a level wins, and a call no
// rule matches is allowed.
//
// Policies leave a tool visible to the model and reject calls at run time.
// To hide a tool from the model altogether, use
// antigravity.CapabilitiesConfig.DisabledTools or EnabledTools instead.
package policy

import (
	"context"

	"github.com/ironpark/gelati/antigravity"
)

// Option customizes a policy built by this package.
type Option func(*antigravity.Policy)

// When restricts the policy to the calls pred matches. A predicate that
// fails counts as a match, so a broken deny rule still denies.
func When(pred antigravity.Predicate) Option {
	return func(p *antigravity.Policy) { p.When = pred }
}

// WhenArgs restricts the policy to calls whose arguments pred matches.
func WhenArgs(pred func(args map[string]any) bool) Option {
	return When(func(_ context.Context, call antigravity.ToolCall) (bool, error) {
		return pred(call.Args), nil
	})
}

// WhenTyped restricts the policy to calls whose arguments, decoded into T
// through encoding/json, pred matches. Arguments that do not decode count
// as a match (fail closed). It is the Go form of an upstream predicate
// annotated with a pydantic model.
func WhenTyped[T any](pred func(args T) bool) Option {
	return When(func(_ context.Context, call antigravity.ToolCall) (bool, error) {
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
	return func(p *antigravity.Policy) { p.Name = name }
}

// Reason explains why the policy matched: the message of a denial, or the
// reason passed to an ask-user handler.
func Reason(reason string) Option {
	return func(p *antigravity.Policy) { p.Reason = reason }
}

// Handler sets the handler of an AskUser or Auto policy.
func Handler(h antigravity.AskUserHandler) Option {
	return func(p *antigravity.Policy) { p.AskUser = h }
}

// Model sets the model used by an Auto policy's safety assessments.
func Model(model string) Option {
	return func(p *antigravity.Policy) { p.AutoModel = model }
}

func build(tool string, decision antigravity.Decision, opts []Option) antigravity.Policy {
	p := antigravity.Policy{Tool: tool, Decision: decision}
	for _, o := range opts {
		o(&p)
	}
	return p
}

// Allow approves calls to tool ("*" for every tool, "server/tool" or
// "server/*" for MCP tools).
func Allow(tool string, opts ...Option) antigravity.Policy {
	return build(tool, antigravity.DecisionApprove, opts)
}

// Deny rejects calls to tool.
func Deny(tool string, opts ...Option) antigravity.Policy {
	return build(tool, antigravity.DecisionDeny, opts)
}

// AskUser asks a handler (see Handler) to approve calls to tool. Without a
// handler the confirmation is left to the host platform; Enforce rejects
// such policies.
func AskUser(tool string, opts ...Option) antigravity.Policy {
	return build(tool, antigravity.DecisionAskUser, opts)
}

// mcp builds MCP policies: one server-wide "server/*" policy when tools is
// nil, else one "server/tool" policy per tool.
func mcp(decision antigravity.Decision, server antigravity.MCPServer, tools []string, opts []Option) []antigravity.Policy {
	name := server.ServerName()
	prefix := map[antigravity.Decision]string{
		antigravity.DecisionApprove: "approve",
		antigravity.DecisionDeny:    "deny",
		antigravity.DecisionAskUser: "ask_user",
	}[decision]
	if tools == nil {
		p := build(name+"/*", decision, opts)
		if p.Name == "" {
			p.Name = prefix + "_" + name + "_all"
		}
		return []antigravity.Policy{p}
	}
	out := make([]antigravity.Policy, 0, len(tools))
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
func AllowMCP(server antigravity.MCPServer, tools []string, opts ...Option) []antigravity.Policy {
	return mcp(antigravity.DecisionApprove, server, tools, opts)
}

// DenyMCP rejects the listed tools of an MCP server, or all of its tools
// when tools is nil.
func DenyMCP(server antigravity.MCPServer, tools []string, opts ...Option) []antigravity.Policy {
	return mcp(antigravity.DecisionDeny, server, tools, opts)
}

// AskUserMCP asks for approval of the listed tools of an MCP server, or of
// all of its tools when tools is nil.
func AskUserMCP(server antigravity.MCPServer, tools []string, opts ...Option) []antigravity.Policy {
	return mcp(antigravity.DecisionAskUser, server, tools, opts)
}

// AllowAll approves every tool call. Meant for autonomous agents and local
// development.
func AllowAll() antigravity.Policy {
	return antigravity.AllowAllPolicy()
}

// DenyAll denies every tool call. Combine it with specific Allow rules for
// a deny-by-default posture; specific rules take precedence.
func DenyAll() antigravity.Policy {
	return antigravity.Policy{Tool: antigravity.WildcardTool, Decision: antigravity.DecisionDeny, Name: "deny_all"}
}

// SafeDefaults allows the read-only (and deprecated read-only) tools and
// asks handler about every other call.
func SafeDefaults(handler antigravity.AskUserHandler) []antigravity.Policy {
	var out []antigravity.Policy
	for _, t := range antigravity.PolicyFreeTools() {
		out = append(out, Allow(string(t)))
	}
	return append(out, AskUser(antigravity.WildcardTool, Handler(handler)))
}

// ConfirmRunCommand allows every tool except run_command, which is denied,
// or, with a non-nil handler, confirmed through it. This is the default
// policy set of antigravity.Config.
func ConfirmRunCommand(handler antigravity.AskUserHandler) []antigravity.Policy {
	return antigravity.ConfirmRunCommandPolicies(handler)
}

// WorkspaceOnly confines the file tools to the session's workspace
// directories. The harness enforces it natively against Config.Workspaces,
// so workspaces is informational, as upstream.
func WorkspaceOnly(workspaces ...string) []antigravity.Policy {
	_ = workspaces
	return antigravity.WorkspaceOnlyPolicies()
}

// Auto enables auto policy mode: the runtime assesses calls with execution
// or network risk (shell commands, URL fetches) with a model before they
// run, and denies flagged calls or, with a Handler, asks it. Specific rules
// run before it and wildcard rules after it. Options: Name (default
// "auto"), Handler and Model.
func Auto(opts ...Option) antigravity.Policy {
	p := antigravity.Policy{Auto: true}
	for _, o := range opts {
		o(&p)
	}
	return p
}

// Enforce returns a hook that evaluates policies in this process; see
// antigravity.EnforcePolicies.
func Enforce(policies []antigravity.Policy, mcpServers []antigravity.MCPServer) (antigravity.PreToolCallHook, error) {
	return antigravity.EnforcePolicies(policies, mcpServers)
}
