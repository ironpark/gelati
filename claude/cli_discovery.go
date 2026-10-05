package claude

import (
	"strings"

	"github.com/ironpark/gelati/internal/proc"
)

// findCLI locates the claude executable on PATH or in the usual install
// locations.
func findCLI() (string, error) {
	if path, ok := proc.Find("claude", cliCandidatesFn()...); ok {
		return path, nil
	}
	return "", newCLINotFoundError(
		"Claude Code not found. Install with:\n"+
			"  npm install -g @anthropic-ai/claude-code\n"+
			"\nIf already installed locally, try:\n"+
			"  export PATH=\"$HOME/node_modules/.bin:$PATH\"\n"+
			"\nOr provide the path via Options.CLIPath", "")
}

// cliCandidatesFn is the candidate list used by findCLI; tests replace it.
var cliCandidatesFn = cliCandidates

// cliCandidates lists the usual install locations for the CLI, in the order
// they are probed.
func cliCandidates() []string {
	return proc.InstallCandidates("claude", "node_modules/.bin", ".yarn/bin", ".claude/local")
}

// jsExtensions mark a CLI path that must run under a JavaScript runtime.
var jsExtensions = []string{".js", ".mjs", ".tsx", ".ts", ".jsx"}

// resolveCommand picks the program and argument list for cliPath, as the
// TypeScript SDK does: a native CLI runs directly with ExecutableArgs before
// the flags; a JavaScript CLI runs under Executable (default "node") with
// ExecutableArgs, then the script path, then the flags.
func resolveCommand(cliPath string, opts *Options, flags []string) (string, []string) {
	args := make([]string, 0, len(opts.ExecutableArgs)+1+len(flags))
	args = append(args, opts.ExecutableArgs...)
	for _, ext := range jsExtensions {
		if strings.HasSuffix(cliPath, ext) {
			runtime := opts.Executable
			if runtime == "" {
				runtime = "node"
			}
			args = append(args, cliPath)
			return runtime, append(args, flags...)
		}
	}
	return cliPath, append(args, flags...)
}
