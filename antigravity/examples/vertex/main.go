// Command vertex authenticates against Vertex AI in express mode (API key)
// or standard mode (project and location) (upstream
// examples/getting_started/vertex.py).
//
//	VERTEX_API_KEY=... go run ./antigravity/examples/vertex
//	GOOGLE_CLOUD_PROJECT=my-project go run ./antigravity/examples/vertex
package main

import (
	"cmp"
	"context"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/ironpark/gelati/antigravity"
)

func main() {
	apiKey := flag.String("api_key", os.Getenv("VERTEX_API_KEY"), "API key for Vertex express mode")
	project := flag.String("project", os.Getenv("GOOGLE_CLOUD_PROJECT"), "Google Cloud project ID")
	location := flag.String("location", cmp.Or(os.Getenv("GOOGLE_CLOUD_LOCATION"), "us-central1"), "Google Cloud location")
	flag.Parse()

	var cfg antigravity.Config
	switch {
	case *apiKey != "" && *project != "":
		log.Fatal("api_key (express mode) and project (standard mode) are mutually exclusive")
	case *apiKey != "":
		fmt.Println("Authenticating via Vertex AI express mode (API key)...")
		cfg = antigravity.Config{Vertex: true, APIKey: *apiKey}
	case *project != "":
		fmt.Printf("Authenticating via Vertex AI standard mode (project %s, location %s)...\n", *project, *location)
		cfg = antigravity.Config{Vertex: true, Project: *project, Location: *location}
	default:
		log.Fatal("no Vertex AI credentials: pass -api_key or set VERTEX_API_KEY (express mode), " +
			"or pass -project/-location or set GOOGLE_CLOUD_PROJECT (standard mode)")
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

	prompt := "Tell me a software engineering joke."
	fmt.Println("  User:", prompt)
	resp, err := agent.Chat(ctx, antigravity.Text(prompt))
	if err != nil {
		log.Fatal(err)
	}
	text, err := resp.WaitText(ctx)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("  Agent:", text)
}
