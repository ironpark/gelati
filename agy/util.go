package agy

import (
	"encoding/json/v2"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ironpark/gelati/internal/jsonx"
)

// decodeJSONObject decodes s into a map when it holds a JSON object.
func decodeJSONObject(s string) (map[string]any, bool) {
	var m map[string]any
	if err := jsonx.Unmarshal([]byte(s), &m); err != nil || m == nil {
		return nil, false
	}
	return m, true
}

// argsFromJSON decodes tool call arguments sent as JSON: the object s
// holds, or an empty map when s is empty or not a JSON object.
func argsFromJSON(s string) map[string]any {
	if m, ok := decodeJSONObject(s); ok {
		return m
	}
	return map[string]any{}
}

// jsonConvert converts src into dst through encoding/json. src often holds
// what the model sent, so both directions tolerate invalid UTF-8 (replaced
// by U+FFFD, as encoding/json v1 did) and dst decodes with jsonx.Foreign.
func jsonConvert(src, dst any) error {
	b, err := json.Marshal(src, jsonx.Foreign)
	if err != nil {
		return err
	}
	return jsonx.Unmarshal(b, dst)
}

// toJSONValue converts v to its generic JSON form (nil, bool, float64,
// string, []any, map[string]any) through encoding/json.
func toJSONValue(v any) (any, error) {
	var out any
	if err := jsonConvert(v, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// onlyChars reports whether every rune of s is an ASCII letter or digit,
// or one of extra.
func onlyChars(s, extra string) bool {
	return !strings.ContainsFunc(s, func(r rune) bool {
		return !('a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9' || strings.ContainsRune(extra, r))
	})
}

// makeStepID builds a step identifier from a trajectory ID and step index.
func makeStepID(trajectoryID string, stepIndex int) string {
	if trajectoryID == "" {
		return strconv.Itoa(stepIndex)
	}
	return trajectoryID + ":" + strconv.Itoa(stepIndex)
}

// wirePathArgumentKeys are the tool argument keys that carry wire-format
// URIs (file:///..., cns://...) and are normalized to filesystem paths, in
// the order canonical paths are taken from.
var wirePathArgumentKeys = []string{"path", "file_path", "directory_path", "TargetFile", "output_path"}

// normalizeWirePath translates a file:// or cns:// URI to an absolute path;
// other strings are returned unchanged.
func normalizeWirePath(p string) string {
	u, err := url.Parse(p)
	if err != nil {
		return p
	}
	switch u.Scheme {
	case "file":
		return filepath.FromSlash(u.Path)
	case "cns":
		return "/cns/" + u.Host + u.Path
	}
	return p
}

// normalizePathArgs rewrites the wire path arguments of args in place and
// returns the first non-empty one as the canonical path. (Upstream takes
// the last one for steps and the first non-empty one for PreTool hooks;
// they differ only for calls with several path arguments, which no
// builtin tool has.)
func normalizePathArgs(args map[string]any) string {
	canonical := ""
	for _, key := range wirePathArgumentKeys {
		if s, ok := args[key].(string); ok {
			n := normalizeWirePath(s)
			args[key] = n
			if canonical == "" {
				canonical = n
			}
		}
	}
	return canonical
}

// normalizeWorkspacePath normalizes wire URIs, expands a leading "~" and
// makes relative paths absolute. CNS paths are kept as they are.
func normalizeWorkspacePath(p string) (string, error) {
	if p == "" {
		return "", nil
	}
	n := normalizeWirePath(p)
	if strings.HasPrefix(n, "/cns/") {
		return n, nil
	}
	if u, err := url.Parse(n); err == nil && u.Scheme != "" && u.Scheme != "file" && len(u.Scheme) > 1 {
		return n, nil
	}
	if n == "~" || strings.HasPrefix(n, "~/") || strings.HasPrefix(n, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("expand %q: %w", p, err)
		}
		n = filepath.Join(home, n[1:])
	}
	abs, err := filepath.Abs(n)
	if err != nil {
		return "", err
	}
	return resolvePath(abs), nil
}

// resolvePath resolves symlinks in the longest existing prefix of the
// absolute path p, like Python's Path.resolve(strict=False).
func resolvePath(p string) string {
	rest := ""
	for dir := p; ; {
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			if rest == "" {
				return resolved
			}
			return filepath.Join(resolved, rest)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return p
		}
		rest = filepath.Join(filepath.Base(dir), rest)
		dir = parent
	}
}

// defaultAppDataDir is the harness app data directory upstream defaults to:
// ~/.gemini/antigravity, resolved.
func defaultAppDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".gemini", "antigravity")
	}
	return resolvePath(filepath.Join(home, ".gemini", "antigravity"))
}
