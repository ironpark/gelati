// Command subagents shows three delegation workflows: dynamic
// self-delegation, a custom static subagent with its own tools and
// instructions, and a nested hierarchy bounded by MaxSubagentDepth and
// AllowedSubagents (upstream examples/getting_started/subagents.py).
//
// The agents work in the directory given by -workspace (default: the
// current directory).
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"strings"
	"sync/atomic"

	"github.com/ironpark/gelati/agy"
)

var subagentActive atomic.Bool

// logHooks print every tool call, indenting those made while a subagent
// runs.
var logHooks = []agy.Hook{
	agy.PreToolCallHook(func(_ context.Context, _ *agy.HookContext, call *agy.ToolCall) (agy.HookResult, error) {
		if call.Name == string(agy.BuiltinStartSubagent) {
			subagentActive.Store(true)
			fmt.Printf("\n  --- [Hook] Spawning subagent ---\n  Arguments: %v\n\n", call.Args)
		} else {
			fmt.Printf("%s- [Start]: %s (ID: %s)\n", indent(), call.Name, call.ID)
		}
		return agy.HookResult{}, nil
	}),
	agy.PostToolCallHook(func(_ context.Context, _ *agy.HookContext, r *agy.ToolResult) error {
		if r.Name == string(agy.BuiltinStartSubagent) {
			subagentActive.Store(false)
			fmt.Printf("\n  --- [Hook] Subagent finished ---\n  Result: %v\n\n", r.Result)
		} else {
			fmt.Printf("%s- [Done]: %s (ID: %s)\n", indent(), r.Name, r.ID)
		}
		return nil
	}),
}

func indent() string {
	if subagentActive.Load() {
		return "    "
	}
	return "  "
}

var getReviewerBadge = agy.NewTool("get_reviewer_badge", "Returns the reviewer's official certification badge name.",
	func(context.Context, *agy.ToolContext, struct{}) (string, error) {
		return "Senior-L3-Auditor-Badge", nil
	})

var getRootAdminSecret = agy.NewTool("get_root_admin_secret", "Returns the root admin super secret password for root administration only.",
	func(context.Context, *agy.ToolContext, struct{}) (string, error) {
		return "SUPER_SECRET_ROOT_PASSWORD_12345", nil
	})

func main() {
	workspace := flag.String("workspace", ".", "directory the agents work in")
	flag.Parse()
	ctx := context.Background()
	workspaces := []string{*workspace}

	fmt.Println("\n=== Dynamic subagent (self clone) ===")
	run(ctx, agy.Config{Workspaces: workspaces, Hooks: logHooks},
		"Use a subagent to research the files in the workspace. Delegate the task of listing and reading "+
			"the files to the subagent, and then generate a lesson plan for me based on its findings.")

	fmt.Println("\n=== Custom static subagent ===")
	reviewer := agy.SubagentConfig{
		Name:        "code_reviewer",
		Description: "Audits source code files and reports missing doc comments.",
		SystemInstructions: agy.TextSystemInstructions("You are a code reviewer. Read source files in the workspace " +
			"and check that all functions have doc comments. For each function missing one, output a warning prefixed " +
			"with '[AUDIT_WARNING]'. Use the 'get_reviewer_badge' tool to sign your final report with your badge name. " +
			"State explicitly whether you have access to 'get_root_admin_secret'. Output your report directly in your final response."),
		Tools: []*agy.Tool{getReviewerBadge},
	}
	text := run(ctx, agy.Config{
		Subagents:  []agy.SubagentConfig{reviewer},
		Workspaces: workspaces,
		Tools:      []*agy.Tool{getReviewerBadge, getRootAdminSecret},
		Hooks:      logHooks,
	}, "Ask the 'code_reviewer' subagent to review the source files, sign the report with their reviewer badge name, "+
		"and verify whether they have access to the 'get_root_admin_secret' tool. Show me the warnings verbatim, "+
		"the badge signature, and its verification.")
	fmt.Println("\n  === Verification ===")
	check("'[AUDIT_WARNING]' prefix", strings.Contains(text, "[AUDIT_WARNING]"))
	check("badge signature", strings.Contains(text, "Senior-L3-Auditor-Badge"))
	check("root secret isolation", !strings.Contains(text, "SUPER_SECRET_ROOT_PASSWORD_12345"))

	fmt.Println("\n=== Hierarchical nested subagents ===")
	factChecker := agy.SubagentConfig{
		Name:        "fact_checker",
		Description: "Reads specific files and verifies factual claims. Reports findings back to the caller.",
		Capabilities: &agy.SubagentCapabilities{
			EnabledTools: []agy.BuiltinTool{agy.BuiltinViewFile, agy.BuiltinFindFile},
		},
	}
	leadResearcher := agy.SubagentConfig{
		Name:        "lead_researcher",
		Description: "Researches a topic by reading files and delegating fact-checking to the 'fact_checker' subagent.",
		Capabilities: &agy.SubagentCapabilities{
			EnabledTools: []agy.BuiltinTool{
				agy.BuiltinViewFile, agy.BuiltinFindFile, agy.BuiltinListDir, agy.BuiltinStartSubagent,
			},
			AllowedSubagents: []string{"fact_checker"},
		},
	}
	run(ctx, agy.Config{
		Subagents:  []agy.SubagentConfig{leadResearcher, factChecker},
		Workspaces: workspaces,
		Capabilities: &agy.CapabilitiesConfig{
			MaxSubagentDepth: 3,
			AllowedSubagents: []string{"lead_researcher"},
		},
		Hooks: logHooks,
	}, "Use the 'lead_researcher' subagent to investigate the workspace. The lead_researcher should delegate "+
		"fact-checking of specific claims to 'fact_checker'. Give me a summary and the verified facts.")
}

func check(name string, ok bool) {
	status := "[FAIL]"
	if ok {
		status = "[PASS]"
	}
	fmt.Println(" ", status, name)
}

// run starts a session with cfg, sends prompt and returns the answer.
func run(ctx context.Context, cfg agy.Config, prompt string) string {
	agent, err := agy.NewAgent(cfg)
	if err != nil {
		log.Fatal(err)
	}
	if err := agent.Start(ctx); err != nil {
		log.Fatal(err)
	}
	defer agent.Close()
	fmt.Println("  User:", prompt)
	resp, err := agent.Chat(ctx, agy.Text(prompt))
	if err != nil {
		log.Fatal(err)
	}
	text, err := resp.WaitText(ctx)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("\n  Agent:\n%s\n", text)
	return text
}
