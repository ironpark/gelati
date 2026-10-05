// Command local_models runs an agent on a local OpenAI-compatible server
// such as Ollama or LM Studio, with the Lightweight preset that trims the
// tool set and prompt overhead for small models (upstream
// examples/getting_started/local_models.py, OpenAI provider; the LiteRT
// provider is not ported). No Gemini API key is needed.
//
//	ollama pull gemma4:26b
//	go run ./antigravity/examples/local_models -model gemma4:26b
package main

import (
	"context"
	"flag"
	"fmt"
	"log"

	"github.com/ironpark/gelati/antigravity"
)

func main() {
	baseURL := flag.String("base_url", "http://localhost:11434/v1", "OpenAI-compatible server endpoint")
	model := flag.String("model", "gemma-4-26B-A4B-it", "model identifier registered on the server")
	prompt := flag.String("prompt", "Explain how local LLM inference works in two sentences.", "prompt to send")
	dryRun := flag.Bool("dry_run", false, "validate the configuration without starting a session")
	flag.Parse()

	cfg := antigravity.Config{
		Model:  *model,
		OpenAI: &antigravity.OpenAIEndpoint{BaseURL: *baseURL},
	}.Lightweight()

	fmt.Println("  Server URL:  ", *baseURL)
	fmt.Println("  Model:       ", *model)
	fmt.Println("  Preset:       Lightweight()")
	if *dryRun {
		if err := cfg.Validate(); err != nil {
			log.Fatal(err)
		}
		fmt.Println("\n[Dry run] configuration is valid:")
		fmt.Println("  Enabled tools:    ", cfg.Capabilities.EnabledTools)
		fmt.Println("  Subagents enabled:", !cfg.Capabilities.DisableSubagents)
		fmt.Println("  Agent behavior:   ", cfg.Capabilities.AgentBehavior)
		fmt.Println("  Compaction at:    ", cfg.Compaction.TokenThreshold, "tokens")
		return
	}

	ctx := context.Background()
	agent, err := antigravity.NewAgent(cfg)
	if err != nil {
		log.Fatal(err)
	}
	if err := agent.Start(ctx); err != nil {
		log.Fatal(err)
	}
	defer agent.Close()

	fmt.Println("\n  User:", *prompt)
	fmt.Print("  Agent: ")
	resp, err := agent.Chat(ctx, antigravity.Text(*prompt))
	if err != nil {
		log.Fatal(err)
	}
	for delta, err := range resp.Text(ctx) {
		if err != nil {
			log.Fatal(err)
		}
		fmt.Print(delta)
	}
	fmt.Println()
}
