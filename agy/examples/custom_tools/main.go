// Command custom_tools defines custom tools, one of them stateful through the
// session-scoped ToolContext state (upstream
// examples/getting_started/custom_tools.py).
package main

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/ironpark/gelati/agy"
	"github.com/ironpark/gelati/agy/policy"
)

type lookupArgs struct {
	FruitName string `json:"fruit_name" description:"The name of the fruit."`
}

type recordArgs struct {
	SKU   string `json:"sku" description:"The SKU of the fruit."`
	Count int    `json:"count" description:"The number of fruits to record."`
}

var lookupFruitSKU = agy.NewTool("lookup_fruit_sku", "Looks up the SKU for a given fruit.",
	func(_ context.Context, _ *agy.ToolContext, in lookupArgs) (string, error) {
		skus := map[string]string{"apple": "SKU-APP-123", "banana": "SKU-BAN-456", "orange": "SKU-ORA-789"}
		name := strings.ToLower(in.FruitName)
		if _, ok := skus[name]; !ok {
			name = strings.TrimSuffix(name, "s")
		}
		sku, ok := skus[name]
		if !ok {
			sku = "SKU-GEN-000"
		}
		return fmt.Sprintf("SKU for %s is %s. Order ID for restocking: ORD-%s-NEW", in.FruitName, sku, sku), nil
	})

// recordFruit keeps running totals per SKU in the session state, which
// persists across turns.
var recordFruit = agy.NewTool("record_fruit", "Records the count of fruits by SKU.",
	func(_ context.Context, tc *agy.ToolContext, in recordArgs) (string, error) {
		var total int
		tc.Atomically(func(tx *agy.StateTx) {
			counts, _ := tx.GetState("fruit_counts")
			m, _ := counts.(map[string]int)
			if m == nil {
				m = map[string]int{}
			}
			m[in.SKU] += in.Count
			total = m[in.SKU]
			tx.SetState("fruit_counts", m)
		})
		return fmt.Sprintf("Recorded %d units for %s. Total count is now %d.", in.Count, in.SKU, total), nil
	})

func main() {
	ctx := context.Background()
	agent, err := agy.New(ctx, agy.Options{
		Tools: []*agy.Tool{lookupFruitSKU, recordFruit},
		SystemInstructions: agy.TextSystemInstructions("You keep track of fruit inventory. " +
			"To record fruits, you MUST first look up the fruit's SKU using lookup_fruit_sku, " +
			"and then use that SKU with record_fruit."),
		Policies: []agy.Policy{
			policy.DenyAll(),
			policy.Allow(lookupFruitSKU.Name()),
			policy.Allow(recordFruit.Name()),
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer agent.Close()

	fmt.Println("  === Custom Tools Demo ===")
	chat(ctx, agent, "What is the SKU for apples? We need to order more.")

	fmt.Println("\n  === Stateful Tool (Fruit Counter) Demo ===")
	for _, prompt := range []string{"I have 5 apples.", "And I just got 3 bananas.", "Oh, and another 2 apples."} {
		chat(ctx, agent, prompt)
	}
}

func chat(ctx context.Context, agent *agy.Agent, prompt string) {
	fmt.Printf("\n  User: %s\n", prompt)
	stream, err := agent.Chat(ctx, agy.Text(prompt))
	if err != nil {
		log.Fatal(err)
	}
	res, err := stream.Result(ctx)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("  Agent:", res.Text())
}
