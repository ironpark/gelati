package codex_test

import (
	"context"
	"fmt"
	"log"

	"github.com/ironpark/gelati/codex"
)

// Example runs one turn and prints the agent's final answer. It needs a real
// `codex` binary on PATH, so it is compiled but not run by `go test`.
func Example() {
	ctx := context.Background()

	client, err := codex.New(ctx, codex.Options{})
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = client.Close() }()

	policy, reviewer := codex.ApprovalModeAutoReview.Settings()
	thread, err := client.StartThread(ctx, codex.StartThreadParams{ThreadSettings: codex.ThreadSettings{
		Cwd:               "/Users/me/project",
		Sandbox:           codex.SandboxModeWorkspaceWrite,
		ApprovalPolicy:    policy,
		ApprovalsReviewer: reviewer,
	}})
	if err != nil {
		log.Fatal(err)
	}

	result, err := client.Run(ctx, thread.ID, codex.Text("Summarize this repo."), nil)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(result.Text())
}

// ExampleTurnStream streams a turn's events as they arrive, answering command
// approvals in Go.
func ExampleTurnStream() {
	ctx := context.Background()

	client, err := codex.New(ctx, codex.Options{
		Approvals: codex.ApprovalFuncs{
			Command: func(ctx context.Context, req *codex.CommandApprovalRequest) (codex.Decision, error) {
				log.Printf("approving %q in %s", req.Command, req.Cwd)
				return codex.DecisionAccept, nil
			},
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = client.Close() }()

	thread, err := client.StartThread(ctx, codex.StartThreadParams{ThreadSettings: codex.ThreadSettings{
		Cwd:            "/Users/me/project",
		ApprovalPolicy: codex.ApprovalUntrusted,
	}})
	if err != nil {
		log.Fatal(err)
	}

	stream, err := client.StartTurn(ctx, thread.ID, codex.Text("Run the tests."), nil)
	if err != nil {
		log.Fatal(err)
	}
	for event, err := range stream.Events(ctx) {
		if err != nil {
			log.Fatal(err)
		}
		switch event.Kind {
		case codex.EventAgentMessageDelta, codex.EventCommandOutputDelta:
			fmt.Print(event.Delta)
		}
	}

	result, err := stream.Result(ctx)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("turn finished:", result.Turn.Status)
}
