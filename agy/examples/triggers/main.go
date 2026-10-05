// Command triggers runs background triggers alongside a session: a periodic
// trigger built with Every, and a custom trigger (any function of the
// Trigger type) simulating a webhook listener (upstream
// examples/getting_started/triggers.py).
package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ironpark/gelati/agy"
)

func main() {
	ctx := context.Background()
	periodic(ctx)
	fmt.Println("\n" + strings.Repeat("=", 60) + "\n")
	custom(ctx)
}

// periodic polls a simulated ticket queue every second.
func periodic(ctx context.Context) {
	fmt.Println("  === Support queue trigger demo ===")
	var standby atomic.Bool
	var polls atomic.Int32
	poll := agy.Every(time.Second, func(ctx context.Context, tc *agy.TriggerContext) error {
		if !standby.Load() {
			return nil
		}
		if polls.Add(1) == 2 {
			fmt.Println("\n  [TRIGGER EVENT] New ticket detected in the queue...")
			return tc.Send(ctx, "[SYSTEM ALERT] New critical ticket assigned: (internal issue). Title: Database Connection Leak in Prod.")
		}
		return nil
	})
	session(ctx, agy.Options{
		SystemInstructions: agy.TextSystemInstructions("You are a system operations and support assistant. " +
			"You monitor a queue of incoming support tickets. When the user asks for updates, you must check and " +
			"report any tickets that came in from the background system alert trigger."),
		Triggers: []agy.Trigger{poll},
	}, &standby,
		"Your task will be to standby and simply let me know if there are any critical tickets received.",
		"I'm back. Did anything critical come in while I was working?")
}

// custom runs a long-lived trigger that pushes a CI failure after three
// ticks. A trigger runs until its context is cancelled at session end.
func custom(ctx context.Context) {
	fmt.Println("  === Custom webhook trigger demo ===")
	var active atomic.Bool
	webhook := agy.Trigger(func(ctx context.Context, tc *agy.TriggerContext) error {
		fmt.Println("\n  [WEBHOOK TRIGGER] Listener started...")
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for tick := 0; ; {
			select {
			case <-ctx.Done():
				return nil
			case <-ticker.C:
			}
			if !active.Load() {
				continue
			}
			if tick++; tick == 3 {
				fmt.Println("\n  [WEBHOOK TRIGGER] Event received: 'AppBuild-42' status FAILED.")
				if err := tc.Send(ctx, "[WEBHOOK ALERT] CI/CD Build Pipeline 'AppBuild-42' FAILED on branch 'main'. "+
					"Reason: Lint errors in routes.py."); err != nil {
					return err
				}
			}
		}
	})
	session(ctx, agy.Options{
		SystemInstructions: agy.TextSystemInstructions("You are a CI/CD operations assistant. You monitor " +
			"pipeline status via an external webhook trigger. When the user asks for updates, you must check and " +
			"report any failures that came in from the webhook alert trigger."),
		Triggers: []agy.Trigger{webhook},
	}, &active,
		"Your task will be to standby and simply let me know if there are any critical pipeline webhook alerts received.",
		"I'm back. Any updates on my builds?")
}

// session sends first, arms the trigger, waits five seconds and sends
// second.
func session(ctx context.Context, opts agy.Options, arm *atomic.Bool, first, second string) {
	agent, err := agy.New(ctx, opts)
	if err != nil {
		log.Fatal(err)
	}
	defer agent.Close()

	ask(ctx, agent, first)
	arm.Store(true)
	fmt.Println("\n  Sleeping for 5 seconds; an event will be simulated in the background...")
	time.Sleep(5 * time.Second)
	ask(ctx, agent, second)
	fmt.Println("\n  Ending session; triggers stop automatically.")
}

func ask(ctx context.Context, agent *agy.Agent, prompt string) {
	fmt.Printf("\n  User: %s\n", prompt)
	res, err := agent.Run(ctx, agy.Text(prompt))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("  Agent:", res.Text())
}
