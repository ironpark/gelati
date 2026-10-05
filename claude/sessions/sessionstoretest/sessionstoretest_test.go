package sessionstoretest_test

import (
	"context"
	"encoding/json/v2"
	"sync"
	"testing"

	"github.com/ironpark/gelati/claude/sessions"
	"github.com/ironpark/gelati/claude/sessions/sessionstoretest"
	"github.com/ironpark/gelati/internal/jsonx"
)

func TestInMemoryStore(t *testing.T) {
	sessionstoretest.Run(t, func(*testing.T) sessions.Store {
		return sessions.NewInMemoryStore()
	})
}

func TestSkipOptional(t *testing.T) {
	sessionstoretest.Run(t, func(*testing.T) sessions.Store {
		return sessions.NewInMemoryStore()
	}, sessionstoretest.ListSessions, sessionstoretest.Delete, sessionstoretest.ListSubkeys, sessionstoretest.ListSessionSummaries)
}

// minimalStore implements only the required methods, persisting entries as
// JSON like a real backend (numbers come back as float64).
type minimalStore struct {
	mu   sync.Mutex
	data map[sessions.Key][]byte
}

func (s *minimalStore) Append(_ context.Context, key sessions.Key, entries []sessions.Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var all []sessions.Entry
	if b, ok := s.data[key]; ok {
		if err := json.Unmarshal(b, &all); err != nil {
			return err
		}
	}
	b, err := jsonx.Marshal(append(all, entries...))
	if err != nil {
		return err
	}
	if all == nil && len(entries) == 0 {
		b = []byte("[]")
	}
	s.data[key] = b
	return nil
}

func (s *minimalStore) Load(_ context.Context, key sessions.Key) ([]sessions.Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.data[key]
	if !ok {
		return nil, nil
	}
	var out []sessions.Entry
	return out, json.Unmarshal(b, &out)
}

func TestMinimalStoreAutoSkipsOptionalContracts(t *testing.T) {
	sessionstoretest.Run(t, func(*testing.T) sessions.Store {
		return &minimalStore{data: map[sessions.Key][]byte{}}
	})
}

// partialStore exposes ListSessions and Delete of an InMemoryStore,
// hiding the summary and subkey listers.
type partialStore struct {
	sessions.Store
	sessions.Lister
	sessions.Deleter
}

func TestPartialStore(t *testing.T) {
	sessionstoretest.Run(t, func(*testing.T) sessions.Store {
		s := sessions.NewInMemoryStore()
		return partialStore{s, s, s}
	})
}
