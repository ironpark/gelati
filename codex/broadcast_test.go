package codex

// listenerCount reports how many iterators are ranging over b. Tests use it
// to wait for a subscription before producing events.
func (b *broadcast[T]) listenerCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.listeners)
}
