package policy_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ironpark/gelati/antigravity"
	"github.com/ironpark/gelati/antigravity/policy"
)

func call(name string, args map[string]any) *antigravity.ToolCall {
	return &antigravity.ToolCall{Name: name, Args: args}
}

func mcpCall(server, name string) *antigravity.ToolCall {
	return &antigravity.ToolCall{Name: name, ServerName: server}
}

// enforce builds the in-process hook or fails the test.
func enforce(t *testing.T, policies []antigravity.Policy, servers ...antigravity.MCPServer) func(*antigravity.ToolCall) antigravity.HookResult {
	t.Helper()
	var srv []antigravity.MCPServer
	if servers != nil {
		srv = servers
	}
	hook, err := policy.Enforce(policies, srv)
	if err != nil {
		t.Fatal(err)
	}
	return func(c *antigravity.ToolCall) antigravity.HookResult {
		res, err := hook(context.Background(), nil, c)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
}

func list(p ...antigravity.Policy) []antigravity.Policy { return p }

func TestBuilders(t *testing.T) {
	p := policy.Allow("read_file", policy.Name("allow-read"))
	if p.Tool != "read_file" || p.Decision != antigravity.DecisionApprove || p.When != nil || p.AskUser != nil || p.Name != "allow-read" {
		t.Fatalf("Allow = %+v", p)
	}
	if p := policy.Deny("run_command", policy.Name("block-cmd")); p.Decision != antigravity.DecisionDeny || p.Name != "block-cmd" {
		t.Fatalf("Deny = %+v", p)
	}
	handler := func(context.Context, antigravity.ToolCall, string) (bool, error) { return true, nil }
	if p := policy.AskUser("run_command", policy.Handler(handler), policy.Name("confirm-cmd")); p.Decision != antigravity.DecisionAskUser || p.AskUser == nil {
		t.Fatalf("AskUser = %+v", p)
	}
	if p := policy.AskUser("run_command"); p.AskUser != nil {
		t.Fatalf("AskUser without handler = %+v", p)
	}
	if p := policy.Deny("run_command", policy.WhenArgs(func(map[string]any) bool { return true })); p.When == nil {
		t.Fatal("When not set")
	}
	if p := policy.AllowAll(); p.Tool != "*" || p.Decision != antigravity.DecisionApprove || p.Name != "allow_all" {
		t.Fatalf("AllowAll = %+v", p)
	}
	if p := policy.DenyAll(); p.Tool != "*" || p.Decision != antigravity.DecisionDeny || p.Name != "deny_all" {
		t.Fatalf("DenyAll = %+v", p)
	}
}

func TestEnforceValidation(t *testing.T) {
	_, err := policy.Enforce(list(antigravity.Policy{Tool: "run_command", Decision: antigravity.DecisionAskUser, Name: "oops"}), nil)
	if err == nil || !strings.Contains(err.Error(), "oops") || !strings.Contains(err.Error(), "missing an ask_user handler") {
		t.Fatalf("ask without handler: %v", err)
	}
	if _, err := policy.Enforce(list(antigravity.Policy{Tool: "my_tool", Decision: antigravity.DecisionAskUser}), nil); err == nil || !strings.Contains(err.Error(), "my_tool") {
		t.Fatalf("unnamed ask without handler: %v", err)
	}
	if _, err := policy.Enforce(list(policy.Allow("github/create_issue")), nil); err == nil || !strings.Contains(err.Error(), "MCP policies") {
		t.Fatalf("MCP policy without servers: %v", err)
	}
	if _, err := policy.Enforce(list(policy.Allow("github/create_issue")), []antigravity.MCPServer{}); err != nil {
		t.Fatalf("MCP policy with empty servers: %v", err)
	}
}

func TestPriority(t *testing.T) {
	run := enforce(t, list(policy.Allow("*"), policy.Deny("dangerous_tool")))
	if !run(call("dangerous_tool", nil)).Deny {
		t.Error("specific deny lost to wildcard allow")
	}
	run = enforce(t, list(policy.Allow("run_command"), policy.Deny("run_command")))
	if !run(call("run_command", nil)).Deny {
		t.Error("specific deny lost to specific allow")
	}
	yes := func(context.Context, antigravity.ToolCall, string) (bool, error) { return true, nil }
	run = enforce(t, list(policy.Deny("*"), policy.AskUser("run_command", policy.Handler(yes))))
	if run(call("run_command", nil)).Deny {
		t.Error("specific ask lost to wildcard deny")
	}
	run = enforce(t, list(policy.Deny("*"), policy.Allow("read_file")))
	if run(call("read_file", nil)).Deny || !run(call("run_command", nil)).Deny {
		t.Error("specific allow over wildcard deny")
	}

	math := &antigravity.MCPStdioServer{Name: "math", Command: "npx"}
	run = enforce(t, append(policy.AllowMCP(math, []string{"calc"}), policy.DenyMCP(math, nil)...), math)
	if run(mcpCall("math", "calc")).Deny || !run(mcpCall("math", "multiply")).Deny {
		t.Error("specific MCP allow over prefix deny")
	}
	run = enforce(t, append(list(policy.Allow("calc")), policy.DenyMCP(math, nil)...), math)
	if !run(mcpCall("math", "calc")).Deny {
		t.Error("bare tool policy matched an MCP tool call")
	}
	run = enforce(t, policy.WorkspaceOnly("/tmp/workspace"))
	if run(call("view_file", map[string]any{"path": "/any/path"})).Deny {
		t.Error("workspace-only policy enforced in process")
	}
}

func TestShortCircuitAndPredicates(t *testing.T) {
	count := 0
	counting := policy.WhenArgs(func(map[string]any) bool { count++; return true })
	run := enforce(t, list(policy.Deny("run_command", counting, policy.Name("first")), policy.Deny("run_command", counting, policy.Name("second"))))
	if res := run(call("run_command", nil)); !res.Deny || count != 1 || !strings.Contains(res.Message, "first") {
		t.Fatalf("first match: %+v count=%d", res, count)
	}
	run = enforce(t, list(
		policy.Deny("run_command", policy.WhenArgs(func(map[string]any) bool { return false }), policy.Name("skip-me")),
		policy.Deny("run_command", policy.WhenArgs(func(map[string]any) bool { return true }), policy.Name("catch-me")),
	))
	if res := run(call("run_command", nil)); !strings.Contains(res.Message, "catch-me") {
		t.Fatalf("non-matching predicate not skipped: %+v", res)
	}

	startsWithRm := policy.WhenArgs(func(args map[string]any) bool {
		s, _ := args["CommandLine"].(string)
		return strings.HasPrefix(s, "rm")
	})
	run = enforce(t, list(policy.Deny("run_command", startsWithRm)))
	if !run(call("run_command", map[string]any{"CommandLine": "rm -rf /"})).Deny || run(call("run_command", map[string]any{"CommandLine": "echo hi"})).Deny {
		t.Fatal("args predicate")
	}

	type runCommandArgs struct {
		CommandLine string `json:"command_line"`
	}
	run = enforce(t, list(policy.Deny("run_command", policy.WhenTyped(func(a runCommandArgs) bool { return strings.Contains(a.CommandLine, "rm") }))))
	if !run(call("run_command", map[string]any{"command_line": "rm -rf"})).Deny || run(call("run_command", map[string]any{"command_line": "echo hi"})).Deny {
		t.Fatal("typed predicate")
	}
	if !run(call("run_command", map[string]any{"command_line": 42})).Deny {
		t.Fatal("undecodable typed args did not fail closed")
	}

	failing := policy.When(func(context.Context, antigravity.ToolCall) (bool, error) { return false, errors.New("boom") })
	run = enforce(t, list(policy.Deny("run_command", failing, policy.Name("broken"))))
	if res := run(call("run_command", nil)); !res.Deny || !strings.Contains(res.Message, "broken") || !strings.Contains(res.Message, "boom") {
		t.Fatalf("failing deny predicate: %+v", res)
	}
	run = enforce(t, list(policy.Allow("run_command", failing, policy.Name("broken-allow")), policy.Allow("*")))
	if res := run(call("run_command", nil)); !res.Deny || !strings.Contains(res.Message, "broken-allow") {
		t.Fatalf("failing allow predicate: %+v", res)
	}
	panicky := policy.When(func(context.Context, antigravity.ToolCall) (bool, error) { panic("kaboom") })
	run = enforce(t, list(policy.Allow("run_command", panicky)))
	if res := run(call("run_command", nil)); !res.Deny || !strings.Contains(res.Message, "kaboom") {
		t.Fatalf("panicking predicate: %+v", res)
	}

	allowed := false
	run = enforce(t, list(
		policy.Allow("run_command", policy.When(func(context.Context, antigravity.ToolCall) (bool, error) { return allowed, nil })),
		policy.Deny("*"),
	))
	if !run(call("run_command", nil)).Deny {
		t.Fatal("predicate false should fall through to deny")
	}
	allowed = true
	if run(call("run_command", nil)).Deny {
		t.Fatal("predicate true should allow")
	}
}

func TestAskUser(t *testing.T) {
	answer := func(ok bool) antigravity.AskUserHandler {
		return func(context.Context, antigravity.ToolCall, string) (bool, error) { return ok, nil }
	}
	if enforce(t, list(policy.AskUser("run_command", policy.Handler(answer(true)))))(call("run_command", nil)).Deny {
		t.Fatal("approved call denied")
	}
	res := enforce(t, list(policy.AskUser("run_command", policy.Handler(answer(false)))))(call("run_command", nil))
	if !res.Deny || !strings.Contains(res.Message, "User denied") {
		t.Fatalf("rejected call: %+v", res)
	}
	var got antigravity.ToolCall
	var gotReason string
	capture := func(_ context.Context, c antigravity.ToolCall, reason string) (bool, error) {
		got, gotReason = c, reason
		return false, nil
	}
	res = enforce(t, list(policy.AskUser("run_command", policy.Handler(capture), policy.Reason("Requires approval before modifying production state"))))(
		call("run_command", map[string]any{"CommandLine": "echo hi"}))
	if got.Args["CommandLine"] != "echo hi" || gotReason != "Requires approval before modifying production state" || res.Message != gotReason {
		t.Fatalf("handler saw %+v %q, result %+v", got, gotReason, res)
	}
	broken := func(context.Context, antigravity.ToolCall, string) (bool, error) {
		return false, errors.New("handler broke")
	}
	res = enforce(t, list(policy.AskUser("run_command", policy.Handler(broken), policy.Name("broken-ask"))))(call("run_command", nil))
	if !res.Deny || !strings.Contains(res.Message, "broken-ask") || !strings.Contains(res.Message, "handler broke") {
		t.Fatalf("failing handler: %+v", res)
	}
}

func TestDefaultsAndPresets(t *testing.T) {
	if enforce(t, list(policy.Deny("other_tool")))(call("unrelated_tool", nil)).Deny {
		t.Error("unmatched call denied")
	}
	if enforce(t, nil)(call("any_tool", nil)).Deny {
		t.Error("empty policies denied")
	}
	for _, tool := range []string{"run_command", "view_file", "create_file", "unknown_tool"} {
		if enforce(t, list(policy.AllowAll()))(call(tool, nil)).Deny {
			t.Errorf("AllowAll denied %s", tool)
		}
		if tool != "unknown_tool" && !enforce(t, list(policy.DenyAll()))(call(tool, nil)).Deny {
			t.Errorf("DenyAll allowed %s", tool)
		}
	}
	run := enforce(t, list(policy.DenyAll(), policy.Allow("view_file")))
	if run(call("view_file", nil)).Deny || !run(call("run_command", nil)).Deny {
		t.Error("deny-all with allow override")
	}

	if res := enforce(t, list(policy.Deny("run_command", policy.Name("no-commands"))))(call("run_command", nil)); !strings.Contains(res.Message, "no-commands") {
		t.Errorf("named deny reason %q", res.Message)
	}
	if res := enforce(t, list(policy.Deny("run_command")))(call("run_command", nil)); !strings.Contains(res.Message, "run_command") {
		t.Errorf("unnamed deny reason %q", res.Message)
	}
	if res := enforce(t, list(policy.Deny("run_command", policy.Reason("Custom security policy: shell execution forbidden."))))(call("run_command", nil)); res.Message != "Custom security policy: shell execution forbidden." {
		t.Errorf("custom deny reason %q", res.Message)
	}

	asked := false
	handler := func(context.Context, antigravity.ToolCall, string) (bool, error) { asked = true; return true, nil }
	run = enforce(t, policy.SafeDefaults(handler))
	for _, tool := range []string{"list_directory", "search_directory", "find_file", "view_file", "finish"} {
		if run(call(tool, nil)).Deny || asked {
			t.Errorf("SafeDefaults asked about or denied %s", tool)
		}
	}
	if run(call("run_command", nil)).Deny || !asked {
		t.Error("SafeDefaults did not ask about run_command")
	}

	run = enforce(t, policy.ConfirmRunCommand(nil))
	if res := run(call("run_command", nil)); !res.Deny || !strings.Contains(res.Message, "confirm_run_command") {
		t.Errorf("ConfirmRunCommand: %+v", res)
	}
	for _, tool := range antigravity.AllTools() {
		if tool != antigravity.BuiltinRunCommand && run(call(string(tool), nil)).Deny {
			t.Errorf("ConfirmRunCommand denied %s", tool)
		}
	}
	no := func(context.Context, antigravity.ToolCall, string) (bool, error) { return false, nil }
	run = enforce(t, policy.ConfirmRunCommand(no))
	if res := run(call("run_command", nil)); !res.Deny || !strings.Contains(res.Message, "User denied") || run(call("view_file", nil)).Deny {
		t.Errorf("ConfirmRunCommand with handler: %+v", res)
	}
	if len(policy.ConfirmRunCommand(nil)) != 2 || len(policy.ConfirmRunCommand(no)) != 2 {
		t.Error("ConfirmRunCommand length")
	}

	ws := policy.WorkspaceOnly("/tmp/workspace")
	if len(ws) != len(antigravity.FileTools()) {
		t.Fatalf("WorkspaceOnly = %+v", ws)
	}
	for _, p := range ws {
		if p.Decision != antigravity.DecisionDeny || p.Name != "workspace_only" {
			t.Errorf("WorkspaceOnly policy %+v", p)
		}
	}
}

func TestMCPBuilders(t *testing.T) {
	math := &antigravity.MCPStdioServer{Name: "math", Command: "npx"}
	if ps := policy.AllowMCP(math, nil); len(ps) != 1 || ps[0].Tool != "math/*" || ps[0].Decision != antigravity.DecisionApprove || ps[0].Name != "approve_math_all" {
		t.Fatalf("AllowMCP wildcard %+v", ps)
	}
	if ps := policy.AllowMCP(math, []string{"calc", "multiply"}); len(ps) != 2 || ps[0].Tool != "math/calc" || ps[1].Tool != "math/multiply" || ps[0].Name != "approve_math_calc" {
		t.Fatalf("AllowMCP tools %+v", ps)
	}
	if ps := policy.DenyMCP(math, []string{"calc"}); len(ps) != 1 || ps[0].Tool != "math/calc" || ps[0].Decision != antigravity.DecisionDeny {
		t.Fatalf("DenyMCP %+v", ps)
	}
	h := func(context.Context, antigravity.ToolCall, string) (bool, error) { return true, nil }
	if ps := policy.AskUserMCP(math, []string{"calc"}, policy.Handler(h)); ps[0].Decision != antigravity.DecisionAskUser || ps[0].AskUser == nil {
		t.Fatalf("AskUserMCP %+v", ps)
	}
	if ps := policy.AskUserMCP(math, nil); ps[0].AskUser != nil {
		t.Fatalf("AskUserMCP without handler %+v", ps)
	}
	if ps := policy.AllowMCP(math, []string{"calc"}, policy.Name("custom")); ps[0].Name != "custom_calc" {
		t.Fatalf("custom name %q", ps[0].Name)
	}

	adv := &antigravity.MCPStdioServer{Name: "math_advanced", Command: "npx"}
	run := enforce(t, append(policy.AllowMCP(math, nil), policy.DenyMCP(adv, nil)...), math, adv)
	if res := run(mcpCall("math_advanced", "calc")); !res.Deny || !strings.Contains(res.Message, "math_advanced_all") {
		t.Fatalf("math_advanced call: %+v", res)
	}
	if run(mcpCall("math", "calc")).Deny {
		t.Fatal("math call denied")
	}
	run = enforce(t, list(policy.Allow("math/*")), math)
	if run(mcpCall("unknown", "calc")).Deny {
		t.Fatal("unknown server call should match nothing and be allowed")
	}
}

func TestAuto(t *testing.T) {
	p := policy.Auto()
	if !p.Auto || p.AskUser != nil {
		t.Fatalf("Auto() = %+v", p)
	}
	h := func(context.Context, antigravity.ToolCall, string) (bool, error) { return true, nil }
	p = policy.Auto(policy.Handler(h), policy.Name("my-auto"), policy.Model("flash"))
	if !p.Auto || p.AskUser == nil || p.Name != "my-auto" || p.AutoModel != "flash" {
		t.Fatalf("Auto(...) = %+v", p)
	}
	// Auto policies are enforced by the runtime, not in process.
	if enforce(t, list(policy.Auto()))(call("run_command", nil)).Deny {
		t.Fatal("auto policy enforced in process")
	}
}
