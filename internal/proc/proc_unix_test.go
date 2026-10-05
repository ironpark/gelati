//go:build unix

package proc

import (
	"bufio"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestGroupTerminateReachesGrandchildren checks that terminating a process
// started with NewGroup also stops what it started.
func TestGroupTerminateReachesGrandchildren(t *testing.T) {
	cmd := exec.Command("sh", "-c", "sleep 60 & echo $!; wait")
	NewGroup(cmd)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	grandchild, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()

	if err := Stop(Group(cmd.Process), exited, 0, 5*time.Second); err != nil {
		t.Fatal("shell did not exit on SIGTERM")
	}
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(grandchild, 0) == nil {
		if time.Now().After(deadline) {
			_ = syscall.Kill(grandchild, syscall.SIGKILL)
			t.Fatal("grandchild survived the group SIGTERM")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
