// Command hello_world is the simplest Claude Code run: send one prompt and
// print the final response.
//
// It needs the claude CLI on PATH (or Options.CLIPath) and a signed-in
// account.
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/ironpark/gelati/claude"
)

func main() {
	ctx := context.Background()
	prompt := "Say 'Hello World!'"
	fmt.Println("  User:", prompt)

	// Run drains the stream and returns the ResultMessage; an error result
	// or a failed exit comes back as the error.
	res, err := claude.Run(ctx, prompt, &claude.Options{MaxTurns: new(1)})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("  Claude:", res.Result)
	if res.TotalCostUSD != nil {
		fmt.Printf("  (cost: $%.4f)\n", *res.TotalCostUSD)
	}
}
