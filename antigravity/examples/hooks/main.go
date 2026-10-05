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

	"github.com/ironpark/gelati/antigravity"
)

var hooks = []antigravity.Hook{
	antigravity.OnSessionStartHook(func(context.Context, *antigravity.HookContext) error {
		fmt.Println("\n  [Hook] Session started")
		return nil
	}),
	antigravity.OnSessionEndHook(func(context.Context, *antigravity.HookContext) error {
		fmt.Println("\n  [Hook] Session ended")
		return nil
	}),
	antigravity.PreTurnHook(func(_ context.Context, _ *antigravity.HookContext, prompt []antigravity.Content) (antigravity.HookResult, error) {
		fmt.Printf("\n  [Hook] Pre-turn: intercepted prompt -> %v\n", prompt)
		return antigravity.HookResult{}, nil // the zero HookResult allows
	}),
	antigravity.PostTurnHook(func(_ context.Context, _ *antigravity.HookContext, response string) error {
		fmt.Printf("\n  [Hook] Post-turn: final response -> %q\n", response)
		return nil
	}),
	antigravity.PreToolCallHook(func(_ context.Context, _ *antigravity.HookContext, call *antigravity.ToolCall) (antigravity.HookResult, error) {
		fmt.Printf("\n  [Hook] Pre-tool-call: approving %s (call_id=%q)\n", call.Name, call.ID)
		return antigravity.HookResult{}, nil
	}),
	antigravity.PostToolCallHook(func(_ context.Context, _ *antigravity.HookContext, r *antigravity.ToolResult) error {
		fmt.Printf("\n  [Hook] Post-tool-call: %s -> %v (call_id=%q)\n", r.Name, r.Result, r.ID)
		return nil
	}),
	antigravity.OnToolErrorHook(func(_ context.Context, _ *antigravity.HookContext, err *antigravity.ToolExecutionError) (string, error) {
		fmt.Printf("\n  [Hook] Tool error: %v (call_id=%q)\n", err, err.CallID)
		return "", nil // keep the default error message
	}),
	antigravity.OnInteractionHook(func(_ context.Context, _ *antigravity.HookContext, spec antigravity.AskQuestionInteractionSpec) (*antigravity.QuestionHookResult, error) {
		fmt.Printf("\n  [Hook] Interaction requested: %+v\n", spec.Questions)
		var res antigravity.QuestionHookResult
		for _, q := range spec.Questions {
			if len(q.Options) > 0 {
				res.Responses = append(res.Responses, antigravity.QuestionResponse{SelectedOptionIDs: []string{q.Options[0].ID}})
			} else {
				res.Responses = append(res.Responses, antigravity.QuestionResponse{FreeformResponse: "Auto-response"})
			}
		}
		return &res, nil
	}),
	antigravity.OnCompactionHook(func(_ context.Context, _ *antigravity.HookContext, step *antigravity.Step) error {
		fmt.Printf("\n  [Hook] Context compaction occurred at step %s\n", step.ID)
		return nil
	}),
	// The stop hook pushes the agent to elaborate once on the Mars prompt.
	antigravity.StopHook(func(_ context.Context, _ *antigravity.HookContext, args antigravity.StopArgs) (antigravity.StopHookResult, error) {
		fmt.Printf("\n  [Stop Hook] Fired (continuation_count=%d)\n", args.ContinuationCount)
		if strings.Contains(strings.ToLower(args.ResponseText), "mars") && args.ContinuationCount == 0 {
			fmt.Println("  [Stop Hook] -> CONTINUE: pushing the agent to dig deeper.")
			return antigravity.StopHookResult{
				Decision: antigravity.StopDecisionContinue,
				Reason: "Great start. Now pick the single most surprising fact you mentioned " +
					"and explain why it matters for future space exploration. Be concise.",
			}, nil
		}
		fmt.Println("  [Stop Hook] -> ALLOW_STOP")
		return antigravity.StopHookResult{Decision: antigravity.StopDecisionAllowStop}, nil
	}),
}

type greetArgs struct {
	Name string `json:"name"`
}

var greet = antigravity.NewTool("greet", "Greets a person by name.",
	func(_ context.Context, _ *antigravity.ToolContext, in greetArgs) (string, error) {
		return "Hello, " + in.Name + "!", nil
	})

var brokenTool = antigravity.NewTool("broken_tool", "Fails always.",
	func(context.Context, *antigravity.ToolContext, struct{}) (string, error) {
		return "", errors.New("this tool is intentionally broken")
	})

func main() {
	ctx := context.Background()
	agent, err := antigravity.NewAgent(antigravity.Config{
		Hooks:        hooks,
		Tools:        []*antigravity.Tool{greet, brokenTool},
		Capabilities: &antigravity.CapabilitiesConfig{AgentBehavior: antigravity.AgentBehaviorInteractive},
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
		resp, err := agent.Chat(ctx, antigravity.Text(prompt))
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
