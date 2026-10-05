// Command streaming streams an agent's thoughts and then its final answer
// as they arrive (upstream examples/getting_started/streaming.py).
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/ironpark/gelati/agy"
)

func main() {
	ctx := context.Background()
	agent, err := agy.New(ctx, agy.Options{})
	if err != nil {
		log.Fatal(err)
	}
	defer agent.Close()

	prompt := "Solve this riddle: I speak without a mouth and hear without ears. " +
		"I have no body, but I come alive with wind. What am I? Explain your reasoning."
	fmt.Printf("  User: %s\n\n", prompt)
	stream, err := agent.Send(ctx, agy.Text(prompt))
	if err != nil {
		log.Fatal(err)
	}

	// Thoughts and Text are independent cursors over the same response, so
	// reading the thoughts first does not lose any of the answer.
	fmt.Println("  Agent (streaming thoughts):")
	for thought, err := range stream.Thoughts(ctx) {
		if err != nil {
			log.Fatal(err)
		}
		fmt.Print(thought)
	}
	fmt.Println("\n\n  Agent (streaming final answer):")
	for delta, err := range stream.Text(ctx) {
		if err != nil {
			log.Fatal(err)
		}
		fmt.Print(delta)
	}
	fmt.Println()
}
