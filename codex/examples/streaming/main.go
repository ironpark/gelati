// Command streaming prints a turn's reasoning, commands and answer as they
// arrive.
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/ironpark/gelati/codex"
)

func main() {
	ctx := context.Background()
	client, err := codex.New(ctx, codex.Options{})
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	cwd, err := os.Getwd()
	if err != nil {
		log.Fatal(err)
	}
	thread, err := client.StartThread(ctx, codex.StartThreadParams{
		ThreadSettings: codex.ThreadSettings{
			Cwd:            cwd,
			Sandbox:        codex.SandboxModeReadOnly,
			ApprovalPolicy: codex.ApprovalNever,
		},
		Ephemeral: true,
	})
	if err != nil {
		log.Fatal(err)
	}

	prompt := "List the files in the current directory, then describe them in one sentence."
	fmt.Printf("  User: %s\n\n", prompt)
	stream, err := client.StartTurn(ctx, thread.ID, codex.Text(prompt), nil)
	if err != nil {
		log.Fatal(err)
	}
	defer stream.Close()
	for event, err := range stream.Events(ctx) {
		if err != nil {
			log.Fatal(err)
		}
		switch event.Kind {
		case codex.EventReasoningDelta:
			if event.ReasoningSummary {
				fmt.Print(event.Delta)
			}
		case codex.EventAgentMessageDelta:
			fmt.Print(event.Delta)
		case codex.EventItemStarted:
			if cmd, ok := event.Item.Item.(*codex.CommandExecutionItem); ok {
				fmt.Printf("\n  [command] %s\n", cmd.Command)
			}
		case codex.EventItemCompleted:
			switch event.Item.Type() {
			case codex.ItemReasoning, codex.ItemAgentMessage:
				fmt.Println()
			}
		}
	}

	// The stream has ended; Result returns the collected turn.
	result, err := stream.Result(ctx)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("\n  (turn %s: %d items)\n", result.Turn.Status, len(result.Items))
}
