package claude

import (
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestWriteEntriesJSONLRoundTrip(t *testing.T) {
	t.Parallel()
	var entries []map[string]any
	for i := range 100 {
		entries = append(entries, map[string]any{
			"uuid":    fmt.Sprintf("uuid-%d", i),
			"type":    []string{"user", "assistant"}[i%2],
			"message": map[string]any{"role": "user", "content": fmt.Sprintf("line %d \"q\" \n nl", i)},
			"nested":  map[string]any{"a": []any{float64(i), float64(i + 1)}, "b": nil},
		})
	}
	entries = append(entries, map[string]any{"no_type": true})
	path := filepath.Join(t.TempDir(), "deep", "stream.jsonl")
	if err := writeEntriesJSONL(path, entries); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	lines := strings.Split(string(raw), "\n")
	if lines[len(lines)-1] != "" || len(lines) != len(entries)+1 {
		t.Fatalf("lines = %d", len(lines))
	}
	for i, line := range lines[:len(entries)-1] {
		if !strings.HasPrefix(line, `{"type":`) {
			t.Fatalf("line %d does not start with type: %s", i, line)
		}
	}
	var got []map[string]any
	for _, line := range lines[:len(entries)] {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("line %q: %v", line, err)
		}
		got = append(got, m)
	}
	if !reflect.DeepEqual(got, entries) {
		t.Fatal("round trip mismatch")
	}
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("mode = %v, %v", info, err)
		}
	}
}
