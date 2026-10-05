// Command permissions answers permission prompts in Go, observes tool calls
// with a hook, and serves an in-process MCP tool.
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/ironpark/gelati/claude"
)

type addArgs struct {
	A int `json:"a"`
	B int `json:"b"`
}

func main() {
	ctx := context.Background()

	add := claude.NewTool("add", "Add two integers.", func(_ context.Context, args addArgs) (claude.ToolResult, error) {
		return claude.TextResult("%d", args.A+args.B), nil
	})

	opts := claude.Options{
		MCPServers: map[string]claude.MCPServerConfig{
			"calc": claude.NewSDKMCPServer("calc", "1.0.0", add),
		},
		// CanUseTool answers every prompt the CLI would otherwise show a
		// user. Without it, the CLI's PermissionMode decides.
		CanUseTool: func(_ context.Context, tool string, input map[string]any,
			_ claude.ToolPermissionContext) (claude.PermissionResult, error) {
			if tool == "mcp__calc__add" {
				fmt.Printf("  [permission] allow %s %v\n", tool, input)
				return &claude.PermissionResultAllow{}, nil
			}
			fmt.Printf("  [permission] deny %s\n", tool)
			return &claude.PermissionResultDeny{Message: "only the calculator is allowed"}, nil
		},
		Hooks: map[claude.HookEvent][]claude.HookMatcher{
			claude.HookPostToolUse: {{Hooks: []claude.HookCallback{
				claude.TypedHook(func(_ context.Context, in *claude.PostToolUseHookInput,
					_ string, _ claude.HookContext) (claude.HookOutput, error) {
					fmt.Printf("  [hook] %s returned %v\n", in.ToolName, in.ToolResponse)
					return claude.HookOutput{}, nil
				}),
			}}},
		},
	}

	res, err := claude.Run(ctx, "Use the add tool to compute 1234 + 5678, then reply with the sum.", opts)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("  Claude:", res.Text())
}
