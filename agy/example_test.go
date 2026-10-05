package agy_test

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/ironpark/gelati/agy"
	"github.com/ironpark/gelati/agy/policy"
)

// These examples need the localharness binary and model credentials, so they
// are compiled but not run by `go test` (none declares an Output comment).

func ExampleAgent() {
	ctx := context.Background()
	agent, err := agy.New(ctx, agy.Options{
		SystemInstructions: agy.TextSystemInstructions("Answer in one sentence."),
	})
	if err != nil {
		log.Fatal(err)
	}
	defer agent.Close()

	stream, err := agent.Chat(ctx, agy.Text("Why is the sky blue?"))
	if err != nil {
		log.Fatal(err)
	}
	for delta, err := range stream.Text(ctx) {
		if err != nil {
			log.Fatal(err)
		}
		fmt.Print(delta)
	}
	fmt.Println()
	res, err := stream.Result(ctx)
	if err != nil {
		log.Fatal(err)
	}
	if u := res.Usage; u != nil && u.TotalTokenCount != nil {
		fmt.Println("tokens:", *u.TotalTokenCount)
	}
}

func ExampleNewTool() {
	type weatherArgs struct {
		City string `json:"city" description:"The city to look up."`
	}
	weather := agy.NewTool("get_weather", "Returns the current weather for a city.",
		func(_ context.Context, tc *agy.ToolContext, in weatherArgs) (map[string]any, error) {
			// Tools share a session-scoped state store.
			n := tc.UpdateState("lookups", func(v any) any { return v.(int) + 1 }, 0)
			return map[string]any{"city": in.City, "forecast": "sunny", "lookups": n}, nil
		})

	ctx := context.Background()
	agent, err := agy.New(ctx, agy.Options{
		Tools: []*agy.Tool{weather},
		// Read-only builtin tools need no policy; custom tools always run.
		Capabilities: &agy.CapabilitiesConfig{EnabledTools: agy.ReadOnlyTools()},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer agent.Close()

	stream, err := agent.Chat(ctx, agy.Text("What's the weather in Seoul?"))
	if err != nil {
		log.Fatal(err)
	}
	for call, err := range stream.ToolCalls(ctx) {
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println("called", call.Name, call.Args)
	}
	res, err := stream.Result(ctx)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(res.Text())
}

func ExampleOptions_policies() {
	confirm := func(_ context.Context, call agy.ToolCall, reason string) (bool, error) {
		fmt.Printf("allow %s %v? (%s)\n", call.Name, call.Args, reason)
		return true, nil
	}
	cfg := agy.Options{
		Policies: []agy.Policy{
			policy.DenyAll(),
			policy.Allow("view_file"),
			policy.Deny("run_command", policy.WhenArgs(func(args map[string]any) bool {
				cmd, _ := args["CommandLine"].(string)
				return strings.Contains(cmd, "rm ")
			}), policy.Reason("Deleting files is not allowed.")),
			policy.AskUser("run_command", policy.Handler(confirm)),
		},
	}
	agent, err := agy.New(context.Background(), cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer agent.Close()
}

func ExampleOptions_hooks() {
	audit := agy.PostToolCallHook(func(_ context.Context, _ *agy.HookContext, r *agy.ToolResult) error {
		if out, ok := r.Result.(*agy.RunCommandResult); ok {
			log.Printf("%s printed %d bytes", r.Name, len(out.Output))
		}
		return nil
	})
	keepGoing := agy.StopHook(func(_ context.Context, _ *agy.HookContext, args agy.StopArgs) (agy.StopHookResult, error) {
		if args.ContinuationCount == 0 && !strings.Contains(args.ResponseText, "DONE") {
			return agy.StopHookResult{Decision: agy.StopDecisionContinue, Reason: "Finish the task and end with DONE."}, nil
		}
		return agy.StopHookResult{}, nil
	})
	cfg := agy.Options{
		Hooks:    []agy.Hook{audit, keepGoing},
		Policies: []agy.Policy{policy.AllowAll()},
	}
	agent, err := agy.New(context.Background(), cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer agent.Close()
}

func ExampleTurnResult_DecodeStructuredOutput() {
	type answer struct {
		Capital    string `json:"capital"`
		Population int    `json:"population"`
	}
	ctx := context.Background()
	agent, err := agy.New(ctx, agy.Options{ResponseSchema: answer{}})
	if err != nil {
		log.Fatal(err)
	}
	defer agent.Close()
	stream, err := agent.Chat(ctx, agy.Text("Capital and population of France?"))
	if err != nil {
		log.Fatal(err)
	}
	res, err := stream.Result(ctx)
	if err != nil {
		log.Fatal(err)
	}
	var a answer
	if err := res.DecodeStructuredOutput(&a); err != nil {
		log.Fatal(err)
	}
	fmt.Println(a.Capital, a.Population)
}

func ExampleEvery() {
	heartbeat := agy.Every(time.Hour, func(ctx context.Context, tc *agy.TriggerContext) error {
		return tc.Send(ctx, "Hourly check: summarize anything new in the workspace.")
	})
	cfg := agy.Options{Triggers: []agy.Trigger{heartbeat}}
	agent, err := agy.New(context.Background(), cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer agent.Close()
}
