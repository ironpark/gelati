// Command streaming prints a response token by token as it arrives, and the
// tools Claude calls along the way.
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/ironpark/gelati/claude"
)

func main() {
	ctx := context.Background()
	prompt := "List the files in the current directory, then describe them in one sentence."
	fmt.Printf("  User: %s\n\n", prompt)

	opts := &claude.Options{
		// StreamEvent messages carry the raw API stream events, including
		// the text deltas.
		IncludePartialMessages: true,
		AllowedTools:           []string{"Bash(ls:*)", "Glob"},
	}
	for msg, err := range claude.Query(ctx, prompt, opts) {
		if err != nil {
			log.Fatal(err)
		}
		switch m := msg.(type) {
		case *claude.StreamEvent:
			if text, ok := m.TextDelta(); ok {
				fmt.Print(text)
			}
		case *claude.AssistantMessage:
			// The complete message follows its deltas; only the tool
			// calls are printed here, since the text was already streamed.
			for _, block := range m.Content {
				if use, ok := block.(*claude.ToolUseBlock); ok {
					fmt.Printf("\n  [tool] %s %v\n", use.Name, use.Input)
				}
			}
		case *claude.ResultMessage:
			fmt.Printf("\n\n  (%d turns, %d ms)\n", m.NumTurns, m.DurationMS)
		}
	}
}
