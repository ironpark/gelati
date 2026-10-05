package antigravity

import (
	"context"
	"strings"
	"testing"

	"github.com/ironpark/gelati/antigravity/internal/wire"
)

func always(context.Context, ToolCall) (bool, error) { return true, nil }

func approve(context.Context, ToolCall, string) (bool, error) { return true, nil }

func TestPolicyConfigStaticRules(t *testing.T) {
	cfg, dynamic, err := policyConfig(nil)
	if err != nil || len(cfg.Rules) != 0 || len(dynamic) != 0 {
		t.Fatalf("empty: %+v %v %v", cfg, dynamic, err)
	}
	cfg, dynamic, err = policyConfig([]Policy{{Tool: "run_command", Decision: DecisionDeny, Name: "block_cmd", Reason: "Shell access disabled."}})
	if err != nil {
		t.Fatal(err)
	}
	r := cfg.Rules[0]
	if r.GetTool() != "run_command" || r.GetServerName() != "" || r.GetDecision() != wire.PolicyDecisionDeny || r.GetName() != "block_cmd" ||
		r.GetIsDynamic() || r.GetRuleID() != "" || r.GetDenyReason() != "Shell access disabled." || len(dynamic) != 0 {
		t.Fatalf("static deny rule %+v", r)
	}
	cfg, _, _ = policyConfig([]Policy{{Tool: "run_command", Decision: DecisionDeny}})
	if cfg.Rules[0].GetName() != "run_command" {
		t.Fatalf("unnamed rule name %q", cfg.Rules[0].GetName())
	}
	cfg, _, _ = policyConfig([]Policy{{Tool: "*", Decision: DecisionApprove}})
	if r := cfg.Rules[0]; r.GetTool() != "*" || r.GetServerName() != "" || r.GetDecision() != wire.PolicyDecisionAllow || r.GetIsDynamic() {
		t.Fatalf("wildcard rule %+v", r)
	}
	cfg, _, _ = policyConfig([]Policy{{Tool: "my_server/dangerous_tool", Decision: DecisionDeny}, {Tool: "my_server/*", Decision: DecisionDeny}})
	if cfg.Rules[0].GetTool() != "dangerous_tool" || cfg.Rules[0].GetServerName() != "my_server" ||
		cfg.Rules[1].GetTool() != "*" || cfg.Rules[1].GetServerName() != "my_server" {
		t.Fatalf("MCP rules %+v %+v", cfg.Rules[0], cfg.Rules[1])
	}
	cfg, _, _ = policyConfig([]Policy{{Tool: "a", Decision: DecisionDeny}, {Tool: "b", Decision: DecisionApprove}, {Tool: "c", Decision: DecisionDeny}})
	if cfg.Rules[0].GetTool() != "a" || cfg.Rules[1].GetTool() != "b" || cfg.Rules[2].GetTool() != "c" {
		t.Fatal("rule order not preserved")
	}
}

func TestPolicyConfigDynamicRules(t *testing.T) {
	policies := []Policy{
		{Tool: "a", Decision: DecisionDeny},
		{Tool: "b", Decision: DecisionDeny, When: always, Name: "block_rm"},
		{Tool: "c", Decision: DecisionApprove},
		{Tool: "d", Decision: DecisionAskUser, AskUser: approve},
	}
	cfg, dynamic, err := policyConfig(policies)
	if err != nil {
		t.Fatal(err)
	}
	if len(dynamic) != 2 || dynamic["rule_1"] == nil || dynamic["rule_3"] == nil || dynamic["rule_1"].Name != "block_rm" {
		t.Fatalf("dynamic map %v", dynamic)
	}
	if !cfg.Rules[1].GetIsDynamic() || cfg.Rules[1].GetRuleID() != "rule_1" || cfg.Rules[3].GetDecision() != wire.PolicyDecisionAskUser || cfg.Rules[0].GetIsDynamic() {
		t.Fatalf("rules %+v", cfg.Rules)
	}

	cfg, dynamic, _ = policyConfig(ConfirmRunCommandPolicies(nil))
	if len(cfg.Rules) != 2 || cfg.Rules[0].GetTool() != "run_command" || cfg.Rules[0].GetDecision() != wire.PolicyDecisionDeny ||
		cfg.Rules[0].GetIsDynamic() || cfg.Rules[1].GetTool() != "*" || len(dynamic) != 0 {
		t.Fatalf("confirm_run_command rules %+v", cfg.Rules)
	}
	if cfg.GetWorkspaceContainment() != wire.PolicyConfigWorkspaceContainmentUnspecified {
		t.Fatalf("containment %s", cfg.GetWorkspaceContainment())
	}
	cfg, dynamic, _ = policyConfig(ConfirmRunCommandPolicies(approve))
	if !cfg.Rules[0].GetIsDynamic() || cfg.Rules[0].GetDecision() != wire.PolicyDecisionAskUser || cfg.Rules[1].GetIsDynamic() || len(dynamic) != 1 {
		t.Fatalf("confirm_run_command with handler %+v", cfg.Rules)
	}
}

func TestPolicyConfigWorkspaceContainment(t *testing.T) {
	allowAll := AllowAllPolicy()
	cfg, _, _ := policyConfig([]Policy{allowAll})
	if cfg.GetWorkspaceContainment() != wire.PolicyConfigWorkspaceContainmentDisabled {
		t.Fatalf("allow_all containment %s", cfg.GetWorkspaceContainment())
	}
	// Only the constructors mark these policies; the names alone do not.
	cfg, _, _ = policyConfig([]Policy{{Tool: "*", Decision: DecisionApprove, Name: "allow_all"}})
	if cfg.GetWorkspaceContainment() != wire.PolicyConfigWorkspaceContainmentUnspecified {
		t.Fatalf("policy named allow_all containment %s", cfg.GetWorkspaceContainment())
	}
	workspaceOnly := WorkspaceOnlyPolicies()[0]
	workspaceOnly.When = always
	cfg, dynamic, _ := policyConfig([]Policy{allowAll, workspaceOnly})
	if cfg.GetWorkspaceContainment() != wire.PolicyConfigWorkspaceContainmentUnspecified {
		t.Fatalf("allow_all + workspace_only containment %s", cfg.GetWorkspaceContainment())
	}
	if cfg.Rules[1].GetIsDynamic() || len(dynamic) != 0 {
		t.Fatal("workspace_only rule must never be dynamic")
	}
}

func TestPolicyConfigAuto(t *testing.T) {
	auto := Policy{Auto: true, AutoModel: "gemini-3.5-flash-lite"}
	userRule := Policy{Tool: "run_command", Decision: DecisionApprove, Name: "auto", When: always}
	cfg, dynamic, err := policyConfig([]Policy{userRule, auto})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.GetAutoConfig().GetEnabled() || cfg.GetAutoConfig().GetModel() != "gemini-3.5-flash-lite" {
		t.Fatalf("auto config %+v", cfg.GetAutoConfig())
	}
	if len(cfg.Rules) != 1 || cfg.Rules[0].GetName() != "auto" || cfg.Rules[0].GetRuleID() != "rule_0" {
		t.Fatalf("rules %+v", cfg.Rules)
	}
	if dynamic["rule_0"].Auto || !dynamic["auto"].Auto || dynamic["auto"].Decision != DecisionDeny || dynamic["auto"].Name != "auto" {
		t.Fatalf("dynamic %+v", dynamic)
	}
	_, dynamic, _ = policyConfig([]Policy{{Auto: true, AskUser: approve}})
	if dynamic["auto"].Decision != DecisionAskUser {
		t.Fatalf("auto with handler %+v", dynamic["auto"])
	}
	if _, _, err := policyConfig([]Policy{{Auto: true}, {Auto: true}}); err == nil || !strings.Contains(err.Error(), "Multiple AutoPolicy") {
		t.Fatalf("multiple auto: %v", err)
	}
}

func TestMatchesTarget(t *testing.T) {
	for _, tc := range []struct {
		tool string
		call ToolCall
		want bool
	}{
		{"*", ToolCall{Name: "x"}, true},
		{"*", ToolCall{Name: "x", ServerName: "s"}, true},
		{"x", ToolCall{Name: "x"}, true},
		{"x", ToolCall{Name: "x", ServerName: "s"}, false},
		{"s/x", ToolCall{Name: "x", ServerName: "s"}, true},
		{"s/*", ToolCall{Name: "y", ServerName: "s"}, true},
		{"s/*", ToolCall{Name: "y", ServerName: "s2"}, false},
		{"s/x", ToolCall{Name: "x"}, false},
	} {
		if got := matchesTarget(tc.tool, &tc.call); got != tc.want {
			t.Errorf("matchesTarget(%q, %+v) = %v", tc.tool, tc.call, got)
		}
	}
}
