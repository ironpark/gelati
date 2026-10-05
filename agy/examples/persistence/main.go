// Command persistence resumes a conversation in a second session: both
// share a SaveDir, and the second passes the first one's conversation ID
// (upstream examples/getting_started/persistence.py).
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/ironpark/gelati/agy"
)

func main() {
	ctx := context.Background()
	saveDir, err := os.MkdirTemp("", "agent_session_")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("  Save directory:", saveDir)

	fmt.Println("\n  === Session 1: establishing context ===")
	id := session(ctx, agy.Config{SaveDir: saveDir}, "Remember this: my favorite color is blue.")
	fmt.Println("  Assigned conversation ID:", id)

	fmt.Println("\n  === Session 2: resuming and verifying recall ===")
	session(ctx, agy.Config{SaveDir: saveDir, ConversationID: id}, "What is my favorite color?")
}

// session runs one agent session with a single prompt and returns its
// conversation ID.
func session(ctx context.Context, cfg agy.Config, prompt string) string {
	agent, err := agy.NewAgent(cfg)
	if err != nil {
		log.Fatal(err)
	}
	if err := agent.Start(ctx); err != nil {
		log.Fatal(err)
	}
	defer agent.Close()

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
	return agent.ConversationID()
}
