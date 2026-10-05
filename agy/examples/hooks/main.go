// Command hooks registers every lifecycle hook: session, turn, tool, tool
// error, interaction, compaction and stop (upstream
// examples/getting_started/hooks.py).
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/ironpark/gelati/agy"
)

var hooks = []agy.Hook{
	agy.OnSessionStartHook(func(context.Context, *agy.HookContext) error {
		fmt.Println("\n  [Hook] Session started")
		return nil
	}),
	agy.OnSessionEndHook(func(context.Context, *agy.HookContext) error {
		fmt.Println("\n  [Hook] Session ended")
		return nil
	}),
	agy.PreTurnHook(func(_ context.Context, _ *agy.HookContext, prompt []agy.Content) (agy.HookResult, error) {
		fmt.Printf("\n  [Hook] Pre-turn: intercepted prompt -> %v\n", prompt)
		return agy.HookResult{}, nil // the zero HookResult allows
	}),
	agy.PostTurnHook(func(_ context.Context, _ *agy.HookContext, response string) error {
		fmt.Printf("\n  [Hook] Post-turn: final response -> %q\n", response)
		return nil
	}),
	agy.PreToolCallHook(func(_ context.Context, _ *agy.HookContext, call *agy.ToolCall) (agy.HookResult, error) {
		fmt.Printf("\n  [Hook] Pre-tool-call: approving %s (call_id=%q)\n", call.Name, call.ID)
		return agy.HookResult{}, nil
	}),
	agy.PostToolCallHook(func(_ context.Context, _ *agy.HookContext, r *agy.ToolResult) error {
		fmt.Printf("\n  [Hook] Post-tool-call: %s -> %v (call_id=%q)\n", r.Name, r.Result, r.ID)
		return nil
	}),
	agy.OnToolErrorHook(func(_ context.Context, _ *agy.HookContext, err *agy.ToolExecutionError) (string, error) {
		fmt.Printf("\n  [Hook] Tool error: %v (call_id=%q)\n", err, err.CallID)
		return "", nil // keep the default error message
	}),
	agy.OnInteractionHook(func(_ context.Context, _ *agy.HookContext, spec agy.AskQuestionInteractionSpec) (*agy.QuestionHookResult, error) {
		fmt.Printf("\n  [Hook] Interaction requested: %+v\n", spec.Questions)
		var res agy.QuestionHookResult
		for _, q := range spec.Questions {
			if len(q.Options) > 0 {
				res.Responses = append(res.Responses, agy.QuestionResponse{SelectedOptionIDs: []string{q.Options[0].ID}})
			} else {
				res.Responses = append(res.Responses, agy.QuestionResponse{FreeformResponse: "Auto-response"})
			}
		}
		return &res, nil
	}),
	agy.OnCompactionHook(func(_ context.Context, _ *agy.HookContext, step *agy.Step) error {
		fmt.Printf("\n  [Hook] Context compaction occurred at step %s\n", step.ID)
		return nil
	}),
	// The stop hook pushes the agent to elaborate once on the Mars prompt.
	agy.StopHook(func(_ context.Context, _ *agy.HookContext, args agy.StopArgs) (agy.StopHookResult, error) {
		fmt.Printf("\n  [Stop Hook] Fired (continuation_count=%d)\n", args.ContinuationCount)
		if strings.Contains(strings.ToLower(args.ResponseText), "mars") && args.ContinuationCount == 0 {
			fmt.Println("  [Stop Hook] -> CONTINUE: pushing the agent to dig deeper.")
			return agy.StopHookResult{
				Decision: agy.StopDecisionContinue,
				Reason: "Great start. Now pick the single most surprising fact you mentioned " +
					"and explain why it matters for future space exploration. Be concise.",
			}, nil
		}
		fmt.Println("  [Stop Hook] -> ALLOW_STOP")
		return agy.StopHookResult{Decision: agy.StopDecisionAllowStop}, nil
	}),
}

type greetArgs struct {
	Name string `json:"name"`
}

var greet = agy.NewTool("greet", "Greets a person by name.",
	func(_ context.Context, _ *agy.ToolContext, in greetArgs) (string, error) {
		return "Hello, " + in.Name + "!", nil
	})

var brokenTool = agy.NewTool("broken_tool", "Fails always.",
	func(context.Context, *agy.ToolContext, struct{}) (string, error) {
		return "", errors.New("this tool is intentionally broken")
	})

func main() {
	ctx := context.Background()
	agent, err := agy.NewAgent(agy.Config{
		Hooks:        hooks,
		Tools:        []*agy.Tool{greet, brokenTool},
		Capabilities: &agy.CapabilitiesConfig{AgentBehavior: agy.AgentBehaviorInteractive},
	})
	if err != nil {
		log.Fatal(err)
	}
	if err := agent.Start(ctx); err != nil {
		log.Fatal(err)
	}
	defer agent.Close()

	for i, prompt := range []string{
		"Say 'Hello World!'",
		"Please greet Alice using the greet tool.",
		"Please call the broken_tool tool.",
		"Ask me a multiple-choice trivia question.",
		"Tell me 3 interesting facts about Mars.",
	} {
		fmt.Printf("\n  --- Prompt %d: %s ---\n", i+1, prompt)
		resp, err := agent.Chat(ctx, agy.Text(prompt))
		if err != nil {
			log.Fatal(err)
		}
		fmt.Print("  Agent: ")
		for delta, err := range resp.Text(ctx) {
			if err != nil {
				log.Fatal(err)
			}
			fmt.Print(delta)
		}
		fmt.Println()
	}
}
