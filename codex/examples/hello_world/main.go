// Command hello_world is the simplest Codex run: start a thread, run one turn
// and print the final response.
//
// It needs the codex CLI on PATH (or Options.CLIPath) and a signed-in
// account.
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/ironpark/gelati/codex"
)

func main() {
	ctx := context.Background()
	client, err := codex.New(ctx, codex.Options{})
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	thread, err := client.StartThread(ctx, codex.StartThreadParams{
		ThreadSettings: codex.ThreadSettings{Sandbox: codex.SandboxModeReadOnly},
		Ephemeral:      true,
	})
	if err != nil {
		log.Fatal(err)
	}

	prompt := "Say 'Hello World!'"
	fmt.Println("  User:", prompt)
	result, err := client.Run(ctx, thread.ID, codex.Text(prompt), nil)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("  Codex:", result.Text())
	if result.Usage != nil {
		fmt.Printf("  (%d tokens)\n", result.Usage.Total.TotalTokens)
	}
}
