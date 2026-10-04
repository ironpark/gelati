package claude

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// findCLI locates the claude executable on PATH or in the usual install
// locations.
func findCLI() (string, error) {
	name := "claude"
	if runtime.GOOS == "windows" {
		name = "claude.exe"
	}
	if path, err := exec.LookPath(name); err == nil {
		return path, nil
	}
	for _, c := range cliCandidatesFn() {
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			return c, nil
		}
	}
	return "", NewCLINotFoundError(
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
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	if runtime.GOOS == "windows" {
		if home == "" {
			return nil
		}
		return []string{filepath.Join(home, ".local", "bin", "claude.exe")}
	}
	candidates := []string{"/usr/local/bin/claude"}
	if home == "" {
		return candidates
	}
	return append(candidates,
		filepath.Join(home, ".npm-global", "bin", "claude"),
		filepath.Join(home, ".local", "bin", "claude"),
		filepath.Join(home, "node_modules", ".bin", "claude"),
		filepath.Join(home, ".yarn", "bin", "claude"),
		filepath.Join(home, ".claude", "local", "claude"),
	)
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
