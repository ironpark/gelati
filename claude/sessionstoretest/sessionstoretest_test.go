package sessionstoretest_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/ironpark/gelati/claude"
	"github.com/ironpark/gelati/claude/sessionstoretest"
)

func TestInMemorySessionStore(t *testing.T) {
	sessionstoretest.Run(t, func(*testing.T) claude.SessionStore {
		return claude.NewInMemorySessionStore()
	})
}

func TestSkipOptional(t *testing.T) {
	sessionstoretest.Run(t, func(*testing.T) claude.SessionStore {
		return claude.NewInMemorySessionStore()
	}, sessionstoretest.ListSessions, sessionstoretest.Delete, sessionstoretest.ListSubkeys, sessionstoretest.ListSessionSummaries)
}

// minimalStore implements only the required methods, persisting entries as
// JSON like a real backend (numbers come back as float64).
type minimalStore struct {
	mu   sync.Mutex
	data map[claude.SessionKey][]byte
}

func (s *minimalStore) Append(_ context.Context, key claude.SessionKey, entries []claude.SessionStoreEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var all []claude.SessionStoreEntry
	if b, ok := s.data[key]; ok {
		if err := json.Unmarshal(b, &all); err != nil {
			return err
		}
	}
	b, err := json.Marshal(append(all, entries...))
	if err != nil {
		return err
	}
	if all == nil && len(entries) == 0 {
		b = []byte("[]")
	}
	s.data[key] = b
	return nil
}

func (s *minimalStore) Load(_ context.Context, key claude.SessionKey) ([]claude.SessionStoreEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.data[key]
	if !ok {
		return nil, nil
	}
	var out []claude.SessionStoreEntry
	return out, json.Unmarshal(b, &out)
}

func TestMinimalStoreAutoSkipsOptionalContracts(t *testing.T) {
	sessionstoretest.Run(t, func(*testing.T) claude.SessionStore {
		return &minimalStore{data: map[claude.SessionKey][]byte{}}
	})
}

// partialStore exposes ListSessions and Delete of an InMemorySessionStore,
// hiding the summary and subkey listers.
type partialStore struct {
	claude.SessionStore
	claude.SessionLister
	claude.SessionDeleter
}

func TestPartialStore(t *testing.T) {
	sessionstoretest.Run(t, func(*testing.T) claude.SessionStore {
		s := claude.NewInMemorySessionStore()
		return partialStore{s, s, s}
	})
}
