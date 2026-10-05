package proc

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestFind(t *testing.T) {
	dir := t.TempDir()
	cand := filepath.Join(dir, "tool")
	if err := os.WriteFile(cand, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, ok := Find("gelati-no-such-tool", filepath.Join(dir, "nope"), dir); ok {
		t.Fatal("found a missing tool")
	}
	if p, ok := Find("gelati-no-such-tool", filepath.Join(dir, "nope"), cand); !ok || p != cand {
		t.Fatalf("candidate: %q, %v", p, ok)
	}
	if _, err := exec.Command(filepath.Join(dir, "nope")).Output(); !IsNotFound(err) {
		t.Fatalf("start of a missing path: %v", err)
	}
}
