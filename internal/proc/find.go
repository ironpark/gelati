package proc

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// Find returns name on PATH, or else the first of candidates that is an
// existing file.
func Find(name string, candidates ...string) (string, bool) {
	if p, err := exec.LookPath(name); err == nil {
		return p, true
	}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			return c, true
		}
	}
	return "", false
}

// InstallCandidates lists the usual install locations of the CLI name, for
// programs (such as macOS GUI apps) whose PATH lacks them: the system and
// Homebrew bin directories, npm's global prefix and ~/.local/bin, then each
// of homeDirs under the home directory.
func InstallCandidates(name string, homeDirs ...string) []string {
	home, _ := os.UserHomeDir()
	if runtime.GOOS == "windows" {
		if home == "" {
			return nil
		}
		return []string{filepath.Join(home, ".local", "bin", name+".exe")}
	}
	out := []string{filepath.Join("/opt/homebrew/bin", name), filepath.Join("/usr/local/bin", name)}
	if home == "" {
		return out
	}
	for _, dir := range append([]string{".npm-global/bin", ".local/bin"}, homeDirs...) {
		out = append(out, filepath.Join(home, filepath.FromSlash(dir), name))
	}
	return out
}

// IsNotFound reports whether a failed start means there is no executable at
// the given path.
func IsNotFound(err error) bool {
	return errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist)
}
