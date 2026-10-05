// Package transcript holds the parts of the Claude Code CLI's on-disk
// transcript layout shared by package claude (resume materialization and
// the transcript mirror) and package sessions: the config and projects
// directories, project directory naming and keys, path canonicalization,
// the agent metadata sidecar convention and JSONL serialization of
// transcript entries.
package transcript

import (
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/ironpark/gelati/claude/internal/unicodenorm"
)

// MaxSanitizedLength is the longest sanitized project directory name kept
// verbatim. Most filesystems limit a path component to 255 bytes; 200
// leaves room for the hash suffix and separator.
const MaxSanitizedLength = 200

var sanitizeRE = regexp.MustCompile(`[^a-zA-Z0-9]`)

// ---------------------------------------------------------------------------
// Path sanitization
// ---------------------------------------------------------------------------

// simpleHash is the CLI's 32-bit djb2-style string hash rendered in base 36,
// used to suffix truncated project directory names. It walks code points as
// Python does; bytes that are not valid UTF-8 hash as Python's
// surrogateescape code points (U+DC80..U+DCFF).
func simpleHash(s string) string {
	var h int32
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size <= 1 {
			r = 0xDC00 + rune(s[i])
			size = 1
		}
		h = h<<5 - h + int32(r) // wraps like JS `hash |= 0`
		i += size
	}
	return strconv.FormatInt(max(int64(h), -int64(h)), 36)
}

// SanitizeFull replaces every character of name other than an ASCII letter
// or digit with '-', without SanitizePath's truncation.
func SanitizeFull(name string) string {
	return sanitizeRE.ReplaceAllLiteralString(name, "-")
}

// SanitizePath makes name safe for use as a directory name: every
// character other than an ASCII letter or digit becomes '-'. Results longer
// than MaxSanitizedLength are truncated and suffixed with "-<hash>".
func SanitizePath(name string) string {
	sanitized := SanitizeFull(name)
	if len(sanitized) <= MaxSanitizedLength {
		return sanitized
	}
	return sanitized[:MaxSanitizedLength] + "-" + simpleHash(name)
}

// ---------------------------------------------------------------------------
// Config directories
// ---------------------------------------------------------------------------

// UserHomeDir is Python's Path.home(): $HOME, then the password database.
func UserHomeDir() (string, error) {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return home, nil
	}
	u, err := user.Current()
	if err != nil {
		return "", err
	}
	return u.HomeDir, nil
}

// ConfigHomeDir returns the Claude config directory: CLAUDE_CONFIG_DIR when
// set, else ~/.claude, NFC-normalized.
func ConfigHomeDir() string {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return filepath.Clean(unicodenorm.NFC(dir))
	}
	home, _ := UserHomeDir()
	return unicodenorm.NFC(filepath.Join(home, ".claude"))
}

// ProjectsDir returns the directory holding the per-project transcript
// directories. env is consulted before the process environment, so callers
// that pass CLAUDE_CONFIG_DIR to the CLI subprocess through
// claude.Options.Env resolve the directory that subprocess will write to.
// env may be nil.
func ProjectsDir(env map[string]string) string {
	if dir := env["CLAUDE_CONFIG_DIR"]; dir != "" {
		return filepath.Join(unicodenorm.NFC(dir), "projects")
	}
	return filepath.Join(ConfigHomeDir(), "projects")
}

// CanonicalizePath resolves a directory to its canonical form: absolute,
// symlinks resolved (Python's non-strict os.path.realpath, so missing
// components are kept) and NFC-normalized.
func CanonicalizePath(d string) string {
	resolved, err := Realpath(d)
	if err != nil {
		return unicodenorm.NFC(d)
	}
	return unicodenorm.NFC(resolved)
}

// Realpath ports Python's non-strict posixpath.realpath: symlinks are
// resolved component by component, ".." is applied to the resolved prefix,
// and components that do not exist are appended unresolved. Symlink loops
// stop resolution at the looping link.
func Realpath(name string) (string, error) {
	if runtime.GOOS == "windows" {
		abs, err := filepath.Abs(name)
		if err != nil {
			return "", err
		}
		if resolved, err := filepath.EvalSymlinks(abs); err == nil {
			return resolved, nil
		}
		return abs, nil
	}
	if !strings.HasPrefix(name, "/") {
		// Python starts from the physical getcwd(); Go's Getwd may return
		// a logical $PWD, which the component walk below resolves.
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		name = wd + "/" + name
	}
	type part struct {
		name     string
		resolved bool   // marker: the symlink below has been resolved
		link     string // symlink path for resolved markers
	}
	var rest []part // stack; the next part is at the end
	pushParts := func(p string) int {
		fields := strings.Split(p, "/")
		for i := len(fields) - 1; i >= 0; i-- {
			rest = append(rest, part{name: fields[i]})
		}
		return len(fields)
	}
	count := pushParts(name)
	path := "/"
	seen := map[string]*string{}
	for count > 0 {
		p := rest[len(rest)-1]
		rest = rest[:len(rest)-1]
		if p.resolved {
			resolved := path
			seen[p.link] = &resolved
			continue
		}
		count--
		switch p.name {
		case "", ".":
			continue
		case "..":
			if i := strings.LastIndex(path, "/"); i > 0 {
				path = path[:i]
			} else {
				path = "/"
			}
			continue
		}
		newpath := path + "/" + p.name
		if path == "/" {
			newpath = "/" + p.name
		}
		fi, err := os.Lstat(newpath)
		if err != nil || fi.Mode()&fs.ModeSymlink == 0 {
			path = newpath
			continue
		}
		if cached, ok := seen[newpath]; ok {
			if cached != nil {
				path = *cached
			} else {
				path = newpath // symlink loop
			}
			continue
		}
		target, err := os.Readlink(newpath)
		if err != nil {
			path = newpath
			continue
		}
		if strings.HasPrefix(target, "/") {
			path = "/"
		}
		seen[newpath] = nil
		rest = append(rest, part{resolved: true, link: newpath})
		count += pushParts(target)
	}
	return path, nil
}

// ---------------------------------------------------------------------------
// Project directories and keys
// ---------------------------------------------------------------------------

// projectDirNameRE and reservedDirNameRE validate the
// CLAUDE_CODE_PROJECT_DIR_NAME override.
var (
	projectDirNameRE  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	reservedDirNameRE = regexp.MustCompile(`(?i)^(?:con|prn|aux|nul|com[0-9]|lpt[0-9])$`)
)

// validProjectDirName returns name when it is a usable
// CLAUDE_CODE_PROJECT_DIR_NAME: 1-64 ASCII letters, digits, '_' or '-' and
// not a reserved Windows device name. Otherwise it returns "".
func validProjectDirName(name string) string {
	if !projectDirNameRE.MatchString(name) || reservedDirNameRE.MatchString(name) {
		return ""
	}
	return name
}

// ProjectDirNameOverride returns the project directory name override of an
// environment: CLAUDE_CODE_PROJECT_DIR_NAME, honored only when
// CLAUDE_CONFIG_DIR is set too (the CLI applies it to every working
// directory under that config directory).
func ProjectDirNameOverride(getenv func(string) string) string {
	if getenv("CLAUDE_CONFIG_DIR") == "" {
		return ""
	}
	return validProjectDirName(getenv("CLAUDE_CODE_PROJECT_DIR_NAME"))
}

// ProjectKey returns the store project key for a directory against an
// explicit environment; empty means the current working directory. It is
// the project directory name override when one applies, else the sanitized
// canonical path.
func ProjectKey(directory string, getenv func(string) string) string {
	if override := ProjectDirNameOverride(getenv); override != "" {
		return override
	}
	return SanitizePath(StoreProjectPath(directory))
}

// StoreProjectPath is the canonical project path for a store call's
// directory argument; empty means the current working directory.
func StoreProjectPath(directory string) string {
	if directory == "" {
		directory = "."
	}
	return CanonicalizePath(directory)
}

// ---------------------------------------------------------------------------
// Transcript files
// ---------------------------------------------------------------------------

// AgentMetadataSidecarPath maps agent-<id>.jsonl to agent-<id>.meta.json in
// the same directory. It is the single definition of the sidecar naming
// convention, shared by reading, import and resume.
func AgentMetadataSidecarPath(transcriptPath string) string {
	return strings.TrimSuffix(transcriptPath, ".jsonl") + ".meta.json"
}
