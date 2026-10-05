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

	"github.com/ironpark/gelati/agy"
)

func main() {
	baseURL := flag.String("base_url", "http://localhost:11434/v1", "OpenAI-compatible server endpoint")
	model := flag.String("model", "gemma-4-26B-A4B-it", "model identifier registered on the server")
	prompt := flag.String("prompt", "Explain how local LLM inference works in two sentences.", "prompt to send")
	dryRun := flag.Bool("dry_run", false, "validate the configuration without starting a session")
	flag.Parse()

	opts := agy.Options{
		Model:  *model,
		OpenAI: &agy.OpenAIEndpoint{BaseURL: *baseURL},
	}.Lightweight()

	fmt.Println("  Server URL:  ", *baseURL)
	fmt.Println("  Model:       ", *model)
	fmt.Println("  Preset:       Lightweight()")
	if *dryRun {
		if err := opts.Validate(); err != nil {
			log.Fatal(err)
		}
		fmt.Println("\n[Dry run] configuration is valid:")
		fmt.Println("  Enabled tools:    ", opts.Capabilities.EnabledTools)
		fmt.Println("  Subagents enabled:", !opts.Capabilities.DisableSubagents)
		fmt.Println("  Agent behavior:   ", opts.Capabilities.AgentBehavior)
		fmt.Println("  Compaction at:    ", opts.Compaction.TokenThreshold, "tokens")
		return
	}

	ctx := context.Background()
	agent, err := agy.New(ctx, opts)
	if err != nil {
		log.Fatal(err)
	}
	defer agent.Close()

	fmt.Println("\n  User:", *prompt)
	fmt.Print("  Agent: ")
	stream, err := agent.Chat(ctx, agy.Text(*prompt))
	if err != nil {
		log.Fatal(err)
	}
	for delta, err := range stream.Text(ctx) {
		if err != nil {
			log.Fatal(err)
		}
		fmt.Print(delta)
	}
	fmt.Println()
}
