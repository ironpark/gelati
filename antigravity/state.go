package antigravity

import "sync"

// StateStore is a concurrency-safe key-value store with an optional parent
// consulted for keys missing locally. It backs HookContext (session, turn
// and operation scopes) and ToolContext. Writes always go to the local
// store, so a narrower scope can shadow but never modify a broader one.
//
// Upstream guards the store with a reentrant lock and exposes it for custom
// critical sections ("with ctx:"). Go has no reentrant mutex, so critical
// sections spanning several operations use Atomically instead.
type StateStore struct {
	parent *StateStore

	mu sync.Mutex
	m  map[string]any
}

// NewStateStore returns an empty store whose lookups fall back to parent,
// which may be nil.
func NewStateStore(parent *StateStore) *StateStore {
	return &StateStore{parent: parent}
}

// GetState returns the value stored under key here or in an ancestor, and
// whether one was found.
func (s *StateStore) GetState(key string) (any, bool) {
	for st := s; st != nil; st = st.parent {
		st.mu.Lock()
		v, ok := st.m[key]
		st.mu.Unlock()
		if ok {
			return v, true
		}
	}
	return nil, false
}

// GetStateOr returns the value stored under key here or in an ancestor, or
// def when there is none.
func (s *StateStore) GetStateOr(key string, def any) any {
	if v, ok := s.GetState(key); ok {
		return v
	}
	return def
}

// SetState stores value under key in this store.
func (s *StateStore) SetState(key string, value any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setLocked(key, value)
}

func (s *StateStore) setLocked(key string, value any) {
	if s.m == nil {
		s.m = make(map[string]any)
	}
	s.m[key] = value
}

// getLocked looks key up with s.mu held: locally, then in the ancestors.
func (s *StateStore) getLocked(key string) (any, bool) {
	if v, ok := s.m[key]; ok {
		return v, true
	}
	if s.parent != nil {
		return s.parent.GetState(key)
	}
	return nil, false
}

// UpdateState atomically replaces the value under key with fn applied to
// the current value (from this store or an ancestor, or def when there is
// none) and returns the new value, which is stored locally. fn runs with
// the store locked, so it must be fast and must not call back into the
// store. If fn panics the store is left unchanged.
func (s *StateStore) UpdateState(key string, fn func(current any) any, def any) any {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.getLocked(key)
	if !ok {
		cur = def
	}
	v := fn(cur)
	s.setLocked(key, v)
	return v
}

// Atomically runs fn with the store locked, for critical sections spanning
// several reads and writes. fn must use tx, not s, to access the store.
func (s *StateStore) Atomically(fn func(tx *StateTx)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&StateTx{s: s})
}

// StateTx accesses a StateStore inside Atomically.
type StateTx struct{ s *StateStore }

// GetState is StateStore.GetState for the locked store.
func (tx *StateTx) GetState(key string) (any, bool) { return tx.s.getLocked(key) }

// SetState is StateStore.SetState for the locked store.
func (tx *StateTx) SetState(key string, value any) { tx.s.setLocked(key, value) }

// StateAs returns the value under key converted to T, and whether a value of
// that type was found.
func StateAs[T any](s *StateStore, key string) (T, bool) {
	v, ok := s.GetState(key)
	if !ok {
		var zero T
		return zero, false
	}
	t, ok := v.(T)
	return t, ok
}
