package claude_test

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ironpark/gelati/claude"
)

// TestE2EQuery runs a trivial prompt against a real, installed CLI. It is
// skipped unless GELATI_CLAUDE_E2E=1, since it costs money and needs
// credentials.
func TestE2EQuery(t *testing.T) {
	if os.Getenv("GELATI_CLAUDE_E2E") != "1" {
		t.Skip("set GELATI_CLAUDE_E2E=1 to run against a real claude CLI")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	opts := claude.Options{
		SystemPrompt: claude.SystemPromptText("Answer with a single word."),
		Tools:        claude.ToolList{},
		Stderr:       func(line string) { t.Log("cli stderr:", line) },
	}

	var text strings.Builder
	var result *claude.ResultMessage
	for msg, err := range claude.Query(ctx, "What is the capital of France?", opts) {
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		switch m := msg.(type) {
		case *claude.AssistantMessage:
			for _, block := range m.Content {
				if block, ok := block.(*claude.TextBlock); ok {
					text.WriteString(block.Text)
				}
			}
		case *claude.ResultMessage:
			result = m
		}
	}
	if result == nil {
		t.Fatal("no result message")
	}
	if result.IsError {
		t.Fatalf("result reported an error: %+v", result)
	}
	if !strings.Contains(strings.ToLower(text.String()), "paris") {
		t.Fatalf("answer = %q", text.String())
	}
}

// TestE2EClient exercises a two-turn interactive session against a real CLI.
func TestE2EClient(t *testing.T) {
	if os.Getenv("GELATI_CLAUDE_E2E") != "1" {
		t.Skip("set GELATI_CLAUDE_E2E=1 to run against a real claude CLI")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()

	client, err := claude.New(ctx, claude.Options{Tools: claude.ToolList{}})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer client.Close()

	if info := client.ServerInfo(); info == nil {
		t.Fatal("no server info after New")
	}
	for _, prompt := range []string{"Remember the number 7.", "What number did I ask you to remember?"} {
		turn, err := client.Send(ctx, claude.Text(prompt))
		if err != nil {
			t.Fatalf("send: %v", err)
		}
		var sawResult bool
		for msg, err := range turn.Events(ctx) {
			if err != nil {
				t.Fatalf("receive: %v", err)
			}
			if _, ok := msg.(*claude.ResultMessage); ok {
				sawResult = true
			}
		}
		if !sawResult {
			t.Fatal("turn ended without a result message")
		}
	}
}

// TestE2ECallbacks checks the control-protocol callbacks against a real CLI:
// an in-process MCP tool, a typed PreToolUse hook and the permission
// callback all take part in one turn. Same gating as TestE2EQuery.
func TestE2ECallbacks(t *testing.T) {
	if os.Getenv("GELATI_CLAUDE_E2E") != "1" {
		t.Skip("set GELATI_CLAUDE_E2E=1 to run against a real claude CLI")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()

	type addArgs struct {
		A int `json:"a"`
		B int `json:"b"`
	}
	var toolCalls, hookCalls, permCalls atomic.Int32
	add := claude.NewTool("add", "Add two integers and return the sum.", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"a": map[string]any{"type": "integer"},
			"b": map[string]any{"type": "integer"},
		},
		"required": []any{"a", "b"},
	}, func(ctx context.Context, args addArgs) (claude.ToolResult, error) {
		toolCalls.Add(1)
		return claude.TextResult(strconv.Itoa(args.A + args.B)), nil
	})
	opts := claude.Options{
		SystemPrompt: claude.SystemPromptText(
			"You are a calculator. Always use the calc add tool for arithmetic. Reply with only the number."),
		MCPServers: map[string]claude.MCPServerConfig{"calc": claude.NewSDKMCPServer("calc", "1.0.0", add)},
		CanUseTool: func(ctx context.Context, name string, input map[string]any, pc claude.ToolPermissionContext) (claude.PermissionResult, error) {
			if pc.MCPServer != nil && pc.MCPServer.Name == "calc" {
				permCalls.Add(1)
			}
			return &claude.PermissionResultAllow{}, nil
		},
		Hooks: map[claude.HookEvent][]claude.HookMatcher{
			claude.HookPreToolUse: {{Hooks: []claude.HookCallback{claude.TypedHook(
				func(ctx context.Context, in *claude.PreToolUseHookInput, toolUseID string, hc claude.HookContext) (claude.HookOutput, error) {
					if in.ToolName == "mcp__calc__add" {
						hookCalls.Add(1)
					}
					return claude.HookOutput{}, nil
				})}}},
		},
		Stderr: func(line string) { t.Log("cli stderr:", line) },
	}

	var result string
	var sawInit bool
	for msg, err := range claude.Query(ctx, "What is 1234 + 4321?", opts) {
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		switch m := msg.(type) {
		case *claude.InitMessage:
			sawInit = true
		case *claude.ResultMessage:
			result = m.Text()
		}
	}
	if !sawInit {
		t.Error("no typed init message")
	}
	if toolCalls.Load() == 0 || hookCalls.Load() == 0 || permCalls.Load() == 0 {
		t.Errorf("tool=%d hook=%d permission=%d, want all > 0",
			toolCalls.Load(), hookCalls.Load(), permCalls.Load())
	}
	if !strings.Contains(result, "5555") {
		t.Errorf("result = %q, want 5555", result)
	}
}
