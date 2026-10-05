// Command approvals answers Codex's approval prompts in Go: commands are
// shown and allowed (the read-only sandbox still applies), and file edits are
// declined.
//
// Without Options.Approvals every prompt is declined.
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/ironpark/gelati/codex"
)

func main() {
	ctx := context.Background()
	client, err := codex.New(ctx, codex.Options{
		Approvals: codex.ApprovalFuncs{
			Command: func(_ context.Context, req *codex.CommandApprovalRequest) (codex.Decision, error) {
				fmt.Printf("  [approval] accept: %s\n", req.Command)
				return codex.DecisionAccept, nil
			},
			FileChange: func(_ context.Context, req *codex.FileChangeApprovalRequest) (codex.Decision, error) {
				fmt.Printf("  [approval] decline file change (%s)\n", req.Reason)
				return codex.DecisionDecline, nil
			},
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	// The untrusted policy asks before running anything outside the
	// trusted set, so the handler above sees most commands.
	thread, err := client.StartThread(ctx, codex.StartThreadParams{
		ThreadSettings: codex.ThreadSettings{
			Cwd:            "/tmp",
			Sandbox:        codex.SandboxModeReadOnly,
			ApprovalPolicy: codex.ApprovalUntrusted,
		},
		Ephemeral: true,
	})
	if err != nil {
		log.Fatal(err)
	}

	result, err := thread.Run(ctx,
		codex.Text("Run `ls -la` and summarize the output in one sentence."))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("  Codex:", result.Text())
}
