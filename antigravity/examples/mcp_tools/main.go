// Command mcp_tools connects an agent to MCP servers over stdio and
// streamable HTTP, filters their tools, and restricts them with policies
// (upstream examples/getting_started/mcp_tools.py).
//
// The "pirate math" MCP server is built into this program (see server.go):
// run with -serve-stdio it speaks MCP on stdin and stdout, which is how the
// harness launches it for the stdio transport.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"

	"github.com/ironpark/gelati/antigravity"
	"github.com/ironpark/gelati/antigravity/policy"
)

func main() {
	serveStdio := flag.Bool("serve-stdio", false, "run as the pirate math MCP server on stdio")
	flag.Parse()
	if *serveStdio {
		if err := serveMCPStdio(os.Stdin, os.Stdout); err != nil {
			log.Fatal(err)
		}
		return
	}

	self, err := os.Executable()
	if err != nil {
		log.Fatal(err)
	}
	stdioServer := func() *antigravity.MCPStdioServer {
		return &antigravity.MCPStdioServer{Name: "pirate_math", Command: self, Args: []string{"-serve-stdio"}}
	}
	ctx := context.Background()

	fmt.Println("\n  --- Stdio transport ---")
	run(ctx, antigravity.Config{MCPServers: []antigravity.MCPServer{stdioServer()}},
		"Use the pirate_multiply tool to multiply 5 and 7.")

	fmt.Println("\n  --- Tool filtering (DisabledTools) ---")
	filtered := stdioServer()
	filtered.DisabledTools = []string{"pirate_divide"}
	run(ctx, antigravity.Config{MCPServers: []antigravity.MCPServer{filtered}},
		"Use the pirate_multiply tool to multiply 6 and 8.",
		"Use the pirate_divide tool to divide 10 by 2.")

	fmt.Println("\n  --- MCP safety policies ---")
	server := stdioServer()
	policies := []antigravity.Policy{policy.DenyAll()}
	policies = append(policies, policy.AllowMCP(server, []string{"pirate_multiply"})...)
	policies = append(policies, policy.DenyMCP(server, []string{"pirate_divide"})...)
	run(ctx, antigravity.Config{MCPServers: []antigravity.MCPServer{server}, Policies: policies},
		"Multiply 4 and 9 using the pirate_multiply tool.",
		"Divide 12 by 3 using the pirate_divide tool.")

	fmt.Println("\n  --- Streamable HTTP transport ---")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}
	go http.Serve(ln, http.HandlerFunc(serveMCPHTTP))
	defer ln.Close()
	httpServer := &antigravity.MCPStreamableHTTPServer{Name: "pirate_math", URL: fmt.Sprintf("http://%s/mcp", ln.Addr())}
	run(ctx, antigravity.Config{MCPServers: []antigravity.MCPServer{httpServer}},
		"Use the pirate_multiply tool to multiply 5 and 7.")
}

// run starts a session with cfg and sends each prompt in turn.
func run(ctx context.Context, cfg antigravity.Config, prompts ...string) {
	agent, err := antigravity.NewAgent(cfg)
	if err != nil {
		log.Fatal(err)
	}
	if err := agent.Start(ctx); err != nil {
		log.Fatal(err)
	}
	defer agent.Close()
	for _, prompt := range prompts {
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
}
