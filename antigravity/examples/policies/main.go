// Command policies secures an agent with declarative tool-call policies:
// deny by default, allowlists, a denylist predicate, ask-user confirmation
// and a rule for a custom tool (upstream
// examples/getting_started/policies.py).
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/ironpark/gelati/antigravity"
	"github.com/ironpark/gelati/antigravity/policy"
)

// runCommandArgs and fileArgs decode builtin tool arguments for predicates;
// the harness may use either spelling.
type runCommandArgs struct {
	CommandLine string `json:"CommandLine"`
}

type fileArgs struct {
	Path       string `json:"path"`
	FilePath   string `json:"file_path"`
	TargetFile string `json:"TargetFile"`
}

func (a fileArgs) path() string { return a.Path + a.FilePath + a.TargetFile }

func blockRM(args runCommandArgs) bool { return strings.Contains(args.CommandLine, "rm") }

func criticalFile(args fileArgs) bool {
	p := args.path()
	return strings.HasSuffix(p, ".key") || strings.Contains(p, "production")
}

var lookupSecret = antigravity.NewTool("lookup_secret", "Looks up a secret by name.",
	func(_ context.Context, _ *antigravity.ToolContext, in struct {
		SecretName string `json:"secret_name"`
	}) (string, error) {
		return "SUPER_SECRET_VALUE_FOR_" + in.SecretName, nil
	})

// approve simulates a user reviewing an ASK_USER call; it always denies.
func approve(_ context.Context, call antigravity.ToolCall, reason string) (bool, error) {
	fmt.Printf("\n  [ASK_USER] %s %v (%s) -> DENY\n", call.Name, call.Args, reason)
	return false, nil
}

func main() {
	listDir, runCommand := string(antigravity.BuiltinListDir), string(antigravity.BuiltinRunCommand)
	editFile, createFile := string(antigravity.BuiltinEditFile), string(antigravity.BuiltinCreateFile)
	policies := []antigravity.Policy{
		policy.DenyAll(),
		policy.Allow(listDir),
		policy.Allow(runCommand),
		policy.Deny(runCommand, policy.WhenTyped(blockRM), policy.Name("block-rm")),
		policy.Allow(editFile),
		policy.Allow(createFile),
		policy.AskUser(editFile, policy.Handler(approve), policy.WhenTyped(criticalFile), policy.Name("ask-for-critical-edits")),
		policy.AskUser(createFile, policy.Handler(approve), policy.WhenTyped(criticalFile), policy.Name("ask-for-critical-creates")),
		policy.Deny(lookupSecret.Name(), policy.Name("block-secret-lookup")),
	}

	// Work in a scratch directory: this demo asks the agent to delete files.
	workspace, err := os.MkdirTemp("", "policies_demo_")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(workspace)

	ctx := context.Background()
	agent, err := antigravity.NewAgent(antigravity.Config{
		Tools:      []*antigravity.Tool{lookupSecret},
		Policies:   policies,
		Workspaces: []string{workspace},
	})
	if err != nil {
		log.Fatal(err)
	}
	if err := agent.Start(ctx); err != nil {
		log.Fatal(err)
	}
	defer agent.Close()

	for _, prompt := range []string{
		"List the files in the current directory.",
		"Delete all files using rm -rf.",
		"Create a new configuration file named production.key with content 'debug=true'.",
		"Look up the secret named 'api_key' using lookup_secret.",
	} {
		fmt.Printf("\n  User: %s\n", prompt)
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
