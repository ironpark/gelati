//go:build unix

package claude

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/ironpark/gelati/claude/sessions"
)

// A FIFO where settings.json is expected would block a plain read forever;
// it must be skipped like any other non-regular file.
func TestMaterializeResumeFIFOSeedFileSkipped(t *testing.T) {
	t.Parallel()
	f := newResumeFixture(t)
	f.writeFile(t, filepath.Join(f.home, ".claude", "placeholder"), "")
	if err := syscall.Mkfifo(filepath.Join(f.home, ".claude", "settings.json"), 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	store := newResumeListingStore()
	store.put(t, f.key(resumeSID), sessions.Entry{"type": "user"})

	done := make(chan *materializedResume, 1)
	go func() {
		m, _ := materializeResumeSession(t.Context(), &Options{Cwd: f.cwd, SessionStore: store, Resume: resumeSID}, f.env)
		done <- m
	}()
	select {
	case m := <-done:
		if m == nil {
			t.Fatal("not materialized")
		}
		defer m.cleanup()
		assertNotExist(t, filepath.Join(m.configDir, "settings.json"))
	case <-time.After(5 * time.Second):
		t.Fatal("materialization hung on a FIFO")
	}
}
