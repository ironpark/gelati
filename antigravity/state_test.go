package antigravity

import (
	"sync"
	"testing"
	"time"
)

func TestStateStoreBasics(t *testing.T) {
	s := NewStateStore(nil)
	if _, ok := s.GetState("missing"); ok {
		t.Fatal("missing key found")
	}
	if v := s.GetStateOr("missing", "default"); v != "default" {
		t.Fatalf("GetStateOr = %v", v)
	}
	s.SetState("key", "val")
	if v, _ := s.GetState("key"); v != "val" {
		t.Fatalf("GetState = %v", v)
	}
	if v, ok := StateAs[string](s, "key"); !ok || v != "val" {
		t.Fatalf("StateAs = %v %v", v, ok)
	}
	if _, ok := StateAs[int](s, "key"); ok {
		t.Fatal("StateAs with the wrong type succeeded")
	}
}

func TestStateStoreHierarchy(t *testing.T) {
	parent := NewStateStore(nil)
	parent.SetState("shared", "parent_val")
	parent.SetState("count", 10)
	child := NewStateStore(parent)
	if v, _ := child.GetState("shared"); v != "parent_val" {
		t.Fatalf("inherited %v", v)
	}
	child.SetState("shared", "child_val")
	if v, _ := child.GetState("shared"); v != "child_val" {
		t.Fatalf("shadowed %v", v)
	}
	if v, _ := parent.GetState("shared"); v != "parent_val" {
		t.Fatalf("parent modified: %v", v)
	}
	if got := child.UpdateState("count", func(v any) any { return v.(int) + 5 }, nil); got != 15 {
		t.Fatalf("UpdateState = %v", got)
	}
	if v, _ := parent.GetState("count"); v != 10 {
		t.Fatalf("parent count modified: %v", v)
	}
}

func TestStateStoreUpdatePanicLeavesStateIntact(t *testing.T) {
	s := NewStateStore(nil)
	s.SetState("key", "initial")
	func() {
		defer func() { _ = recover() }()
		s.UpdateState("key", func(any) any { panic("failed") }, nil)
	}()
	if v, _ := s.GetState("key"); v != "initial" {
		t.Fatalf("state after panic %v", v)
	}
	// The lock was released.
	s.Atomically(func(tx *StateTx) {
		if v, _ := tx.GetState("key"); v != "initial" {
			t.Errorf("in transaction %v", v)
		}
	})
}

func TestStateStoreAtomically(t *testing.T) {
	s := NewStateStore(nil)
	s.Atomically(func(tx *StateTx) { tx.SetState("in_lock", true) })
	if v, _ := s.GetState("in_lock"); v != true {
		t.Fatal("transaction write lost")
	}
	var order []int
	var mu sync.Mutex
	entered := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		s.Atomically(func(*StateTx) {
			close(entered)
			time.Sleep(20 * time.Millisecond)
			mu.Lock()
			order = append(order, 1)
			mu.Unlock()
		})
	})
	<-entered
	wg.Go(func() {
		s.Atomically(func(*StateTx) {
			mu.Lock()
			order = append(order, 2)
			mu.Unlock()
		})
	})
	wg.Wait()
	if len(order) != 2 || order[0] != 1 {
		t.Fatalf("order %v", order)
	}
}

func TestStateStoreConcurrentUpdates(t *testing.T) {
	s := NewStateStore(nil)
	var wg sync.WaitGroup
	for i := range 25 {
		wg.Go(func() {
			s.UpdateState("counter", func(v any) any {
				time.Sleep(100 * time.Microsecond)
				return v.(int) + 1
			}, 0)
			s.SetState("key", i)
		})
	}
	wg.Wait()
	if v, _ := s.GetState("counter"); v != 25 {
		t.Fatalf("counter = %v", v)
	}
}
