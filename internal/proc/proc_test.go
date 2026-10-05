package proc

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

type fakeStopper struct {
	exited     chan struct{}
	exitOn     string // "terminate", "kill" or "" (never)
	terminated bool
	killed     bool
}

func (f *fakeStopper) Signal(os.Signal) error {
	f.terminated = true
	if f.exitOn == "terminate" {
		close(f.exited)
	}
	return nil
}

func (f *fakeStopper) Kill() error {
	f.killed = true
	if f.exitOn == "kill" {
		close(f.exited)
	}
	return nil
}

func TestStopEscalates(t *testing.T) {
	const d = 10 * time.Millisecond
	exited := make(chan struct{})
	close(exited)
	clean := &fakeStopper{exited: exited}
	if Stop(clean, clean.exited, d, d) != nil || clean.terminated || clean.killed {
		t.Fatalf("already exited: %+v", clean)
	}

	term := &fakeStopper{exited: make(chan struct{}), exitOn: "terminate"}
	if Stop(term, term.exited, d, d) != nil || !term.terminated || term.killed {
		t.Fatalf("exits on terminate: %+v", term)
	}

	stubborn := &fakeStopper{exited: make(chan struct{}), exitOn: "kill"}
	if Stop(stubborn, stubborn.exited, d, d) != nil || !stubborn.terminated || !stubborn.killed {
		t.Fatalf("needs kill: %+v", stubborn)
	}

	immortal := &fakeStopper{exited: make(chan struct{})}
	if Stop(immortal, immortal.exited, d, d) == nil {
		t.Fatal("expected an error for a process that survives the kill")
	}
}

func TestEnviron(t *testing.T) {
	t.Setenv("GELATI_PROC_A", "inherited")
	cmd := exec.Command("sh", "-c", "echo $GELATI_PROC_A $GELATI_PROC_C")
	cmd.Env = Environ(map[string]string{"GELATI_PROC_A": "override", "GELATI_PROC_C": "added"})
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("needs sh: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "override added" {
		t.Fatalf("child saw %q", got)
	}
	if len(Environ(nil)) != len(os.Environ()) {
		t.Fatal("nil overrides should inherit the environment unchanged")
	}
}
