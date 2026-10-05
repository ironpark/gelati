// Command client holds a multi-turn conversation over one session: the
// second turn relies on what the first one established.
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/ironpark/gelati/claude"
)

func main() {
	ctx := context.Background()
	client, err := claude.New(ctx, claude.Options{MaxTurns: new(1)})
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	for _, prompt := range []string{
		"Pick a random fruit and remember it. Reply only with the fruit's name.",
		"What fruit did you pick? Name its color in one word.",
	} {
		fmt.Println("  User:", prompt)
		res, err := client.Run(ctx, claude.Text(prompt))
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println("  Claude:", res.Text())
	}
}
