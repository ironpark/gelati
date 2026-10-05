// Command hello_world is the simplest Antigravity agent: start a session,
// send a prompt and print the full text response (upstream
// examples/getting_started/hello_world.py).
//
// It needs the localharness binary (ANTIGRAVITY_HARNESS_PATH or PATH) and
// GEMINI_API_KEY.
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/ironpark/gelati/agy"
)

func main() {
	ctx := context.Background()
	// The zero Config runs agy.DefaultModel on the Gemini API. Set
	// Config.Model to pick another model.
	agent, err := agy.NewAgent(agy.Config{})
	if err != nil {
		log.Fatal(err)
	}
	if err := agent.Start(ctx); err != nil {
		log.Fatal(err)
	}
	defer agent.Close()

	prompt := "Say 'Hello World!'"
	fmt.Println("  User:", prompt)
	resp, err := agent.Chat(ctx, agy.Text(prompt))
	if err != nil {
		log.Fatal(err)
	}
	text, err := resp.WaitText(ctx)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("  Agent:", text)
}
