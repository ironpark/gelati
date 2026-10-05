// Command cancellation aborts turns in two ways: TurnStream.Cancel, which
// ends the stream with *agy.CancelledError, and cancelling the
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

	"github.com/ironpark/gelati/agy"
)

// render streams the thoughts and the answer of stream.
func render(ctx context.Context, stream *agy.TurnStream) error {
	fmt.Print("  Agent thoughts: ")
	for thought, err := range stream.Thoughts(ctx) {
		if err != nil {
			return err
		}
		fmt.Print(thought)
	}
	fmt.Print("\n  Agent response: ")
	for delta, err := range stream.Text(ctx) {
		if err != nil {
			return err
		}
		fmt.Print(delta)
	}
	fmt.Println()
	return nil
}

func report(err error) {
	var cancelled *agy.CancelledError
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
	agent, err := agy.New(ctx, agy.Options{})
	if err != nil {
		log.Fatal(err)
	}
	defer agent.Close()

	fmt.Println("\n=== Scenario 1: programmatic cancellation (TurnStream.Cancel) ===")
	stream, err := agent.Send(ctx, agy.Text("Write a very long story about a character named cancellation."))
	if err != nil {
		log.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- render(ctx, stream) }()
	time.Sleep(2 * time.Second)
	fmt.Println("\n  [Aborting the turn via stream.Cancel]")
	if err := stream.Cancel(ctx); err != nil {
		log.Fatal(err)
	}
	report(<-done)

	fmt.Println("\n=== Scenario 2: context cancellation ===")
	stream, err = agent.Send(ctx, agy.Text("Write a very long poem about a character named interruption."))
	if err != nil {
		log.Fatal(err)
	}
	readCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	time.AfterFunc(2*time.Second, cancel)
	report(render(readCtx, stream))
	// Cancelling the reader's context stops reading but not the turn; stop
	// it too before the session closes. (Agent.Run does both when its
	// context ends.)
	if err := stream.Cancel(ctx); err != nil {
		log.Fatal(err)
	}
	fmt.Println("\n  Finished cancellation example.")
}
