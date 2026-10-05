package antigravity_test

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/ironpark/gelati/antigravity"
	"github.com/ironpark/gelati/antigravity/policy"
)

// These examples need the localharness binary and model credentials, so they
// are compiled but not run by `go test` (none declares an Output comment).

func ExampleAgent() {
	ctx := context.Background()
	agent, err := antigravity.NewAgent(antigravity.Config{
		SystemInstructions: antigravity.TextSystemInstructions("Answer in one sentence."),
	})
	if err != nil {
		log.Fatal(err)
	}
	if err := agent.Start(ctx); err != nil {
		log.Fatal(err)
	}
	defer agent.Close()

	resp, err := agent.Chat(ctx, antigravity.Text("Why is the sky blue?"))
	if err != nil {
		log.Fatal(err)
	}
	for delta, err := range resp.Text(ctx) {
		if err != nil {
			log.Fatal(err)
		}
		fmt.Print(delta)
	}
	fmt.Println()
	if u := resp.UsageMetadata(); u != nil && u.TotalTokenCount != nil {
		fmt.Println("tokens:", *u.TotalTokenCount)
	}
}

func ExampleNewTool() {
	type weatherArgs struct {
		City string `json:"city" description:"The city to look up."`
	}
	weather := antigravity.NewTool("get_weather", "Returns the current weather for a city.",
		func(_ context.Context, tc *antigravity.ToolContext, in weatherArgs) (map[string]any, error) {
			// Tools share a session-scoped state store.
			n := tc.UpdateState("lookups", func(v any) any { return v.(int) + 1 }, 0)
			return map[string]any{"city": in.City, "forecast": "sunny", "lookups": n}, nil
		})

	ctx := context.Background()
	agent, err := antigravity.NewAgent(antigravity.Config{
		Tools: []*antigravity.Tool{weather},
		// Read-only builtin tools need no policy; custom tools always run.
		Capabilities: &antigravity.CapabilitiesConfig{EnabledTools: antigravity.ReadOnlyTools()},
	})
	if err != nil {
		log.Fatal(err)
	}
	if err := agent.Start(ctx); err != nil {
		log.Fatal(err)
	}
	defer agent.Close()

	resp, err := agent.Chat(ctx, antigravity.Text("What's the weather in Seoul?"))
	if err != nil {
		log.Fatal(err)
	}
	for call, err := range resp.ToolCalls(ctx) {
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println("called", call.Name, call.Args)
	}
	text, err := resp.WaitText(ctx)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(text)
}

func ExampleConfig_policies() {
	confirm := func(_ context.Context, call antigravity.ToolCall, reason string) (bool, error) {
		fmt.Printf("allow %s %v? (%s)\n", call.Name, call.Args, reason)
		return true, nil
	}
	cfg := antigravity.Config{
		Policies: []antigravity.Policy{
			policy.DenyAll(),
			policy.Allow("view_file"),
			policy.Deny("run_command", policy.WhenArgs(func(args map[string]any) bool {
				cmd, _ := args["CommandLine"].(string)
				return strings.Contains(cmd, "rm ")
			}), policy.Reason("Deleting files is not allowed.")),
			policy.AskUser("run_command", policy.Handler(confirm)),
		},
	}
	if _, err := antigravity.NewAgent(cfg); err != nil {
		log.Fatal(err)
	}
}

func ExampleConfig_hooks() {
	audit := antigravity.PostToolCallHook(func(_ context.Context, _ *antigravity.HookContext, r *antigravity.ToolResult) error {
		if out, ok := r.Result.(*antigravity.RunCommandResult); ok {
			log.Printf("%s printed %d bytes", r.Name, len(out.Output))
		}
		return nil
	})
	keepGoing := antigravity.StopHook(func(_ context.Context, _ *antigravity.HookContext, args antigravity.StopArgs) (antigravity.StopHookResult, error) {
		if args.ContinuationCount == 0 && !strings.Contains(args.ResponseText, "DONE") {
			return antigravity.StopHookResult{Decision: antigravity.StopDecisionContinue, Reason: "Finish the task and end with DONE."}, nil
		}
		return antigravity.StopHookResult{}, nil
	})
	cfg := antigravity.Config{
		Hooks:    []antigravity.Hook{audit, keepGoing},
		Policies: []antigravity.Policy{policy.AllowAll()},
	}
	if _, err := antigravity.NewAgent(cfg); err != nil {
		log.Fatal(err)
	}
}

func ExampleChatResponse_StructuredOutput() {
	type answer struct {
		Capital    string `json:"capital"`
		Population int    `json:"population"`
	}
	ctx := context.Background()
	agent, err := antigravity.NewAgent(antigravity.Config{ResponseSchema: answer{}})
	if err != nil {
		log.Fatal(err)
	}
	if err := agent.Start(ctx); err != nil {
		log.Fatal(err)
	}
	defer agent.Close()
	resp, err := agent.Chat(ctx, antigravity.Text("Capital and population of France?"))
	if err != nil {
		log.Fatal(err)
	}
	var a answer
	if err := resp.DecodeStructuredOutput(ctx, &a); err != nil {
		log.Fatal(err)
	}
	fmt.Println(a.Capital, a.Population)
}

func ExampleEvery() {
	heartbeat := antigravity.Every(time.Hour, func(ctx context.Context, tc *antigravity.TriggerContext) error {
		return tc.Send(ctx, "Hourly check: summarize anything new in the workspace.")
	})
	cfg := antigravity.Config{Triggers: []antigravity.Trigger{heartbeat}}
	if _, err := antigravity.NewAgent(cfg); err != nil {
		log.Fatal(err)
	}
}
