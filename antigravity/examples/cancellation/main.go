// Command cancellation aborts turns in two ways: ChatResponse.Cancel, which
// ends the stream with *antigravity.CancelledError, and cancelling the
// context the stream is read with (upstream
// examples/getting_started/cancellation.py, where the second case is
// asyncio task cancellation).
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/ironpark/gelati/antigravity"
)

// render streams the thoughts and the answer of resp.
func render(ctx context.Context, resp *antigravity.ChatResponse) error {
	fmt.Print("  Agent thoughts: ")
	for thought, err := range resp.Thoughts(ctx) {
		if err != nil {
			return err
		}
		fmt.Print(thought)
	}
	fmt.Print("\n  Agent response: ")
	for delta, err := range resp.Text(ctx) {
		if err != nil {
			return err
		}
		fmt.Print(delta)
	}
	fmt.Println()
	return nil
}

func report(err error) {
	var cancelled *antigravity.CancelledError
	switch {
	case err == nil:
		fmt.Println("\n  [Turn completed before cancellation]")
	case errors.As(err, &cancelled):
		fmt.Println("\n  [Programmatic cancel caught] turn aborted by the client:", err)
	case errors.Is(err, context.Canceled):
		// *CancelledError also matches context.Canceled, so it is checked
		// first above.
		fmt.Println("\n  [Context cancel caught] stopped reading the turn:", err)
	default:
		log.Fatal(err)
	}
}

func main() {
	ctx := context.Background()
	agent, err := antigravity.NewAgent(antigravity.Config{})
	if err != nil {
		log.Fatal(err)
	}
	if err := agent.Start(ctx); err != nil {
		log.Fatal(err)
	}
	defer agent.Close()

	fmt.Println("\n=== Scenario 1: programmatic cancellation (ChatResponse.Cancel) ===")
	resp, err := agent.Chat(ctx, antigravity.Text("Write a very long story about a character named cancellation."))
	if err != nil {
		log.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- render(ctx, resp) }()
	time.Sleep(2 * time.Second)
	fmt.Println("\n  [Aborting the turn via resp.Cancel]")
	if err := resp.Cancel(ctx); err != nil {
		log.Fatal(err)
	}
	report(<-done)

	fmt.Println("\n=== Scenario 2: context cancellation ===")
	resp, err = agent.Chat(ctx, antigravity.Text("Write a very long poem about a character named interruption."))
	if err != nil {
		log.Fatal(err)
	}
	readCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	time.AfterFunc(2*time.Second, cancel)
	report(render(readCtx, resp))
	// Cancelling the reader's context stops reading but not the turn; stop
	// it too before the session closes.
	if err := resp.Cancel(ctx); err != nil {
		log.Fatal(err)
	}
	fmt.Println("\n  Finished cancellation example.")
}
