// Package sessionstoretest provides a conformance suite for [sessions.Store]
// adapters, ported from the Python SDK's
// claude_agent_sdk.testing.run_session_store_conformance.
//
// Call [Run] from a test of the adapter's package:
//
//	func TestConformance(t *testing.T) {
//		sessionstoretest.Run(t, func(t *testing.T) sessions.Store {
//			return newTestStore(t) // a fresh, empty store
//		})
//	}
//
// The contracts of the optional interfaces ([sessions.Lister],
// [sessions.SummaryLister], [sessions.Deleter] and [sessions.SubkeyLister])
// run only when the store implements them and they are not named in
// skipOptional.
//
// Entries are compared after a JSON round trip, so an adapter that persists
// entries as JSON (turning numbers into float64, for example) conforms as
// long as Load returns entries deep-equal to what was appended in JSON terms.
// For a key that was never written, Load may return nil or an empty slice.
package sessionstoretest

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ironpark/gelati/claude/sessions"
	"github.com/ironpark/gelati/internal/jsonx"
)

// Names accepted in Run's skipOptional: the Go method names of the optional
// Store interfaces.
const (
	ListSessions         = "ListSessions"         // sessions.Lister
	ListSessionSummaries = "ListSessionSummaries" // sessions.SummaryLister
	Delete               = "Delete"               // sessions.Deleter
	ListSubkeys          = "ListSubkeys"          // sessions.SubkeyLister
)

var optionalMethods = []string{ListSessions, ListSessionSummaries, Delete, ListSubkeys}

// key is the default key of the contracts.
var key = sessions.Key{ProjectKey: "proj", SessionID: "sess"}

// Run asserts the 14 behavioral contracts of sessions.Store as subtests of
// t, run sequentially. makeStore must return a fresh, empty store; it is
// called once to probe the optional interfaces and then once per contract,
// so contracts never observe each other's data. It may register cleanup with
// t.
//
// The contracts of an optional interface are skipped when the probed store
// does not implement it, or when skipOptional names it: one of
// ListSessions, ListSessionSummaries, Delete and ListSubkeys (the method
// names; see the constants). Skip a method the store type implements but
// the configured backend does not support. Any other name fails the test.
func Run(t *testing.T, makeStore func(t *testing.T) sessions.Store, skipOptional ...string) {
	t.Helper()
	skip := map[string]bool{}
	for _, name := range skipOptional {
		if !slices.Contains(optionalMethods, name) {
			t.Fatalf("sessionstoretest: unknown optional method %q in skipOptional (want one of %s)",
				name, strings.Join(optionalMethods, ", "))
		}
		skip[name] = true
	}

	probe := makeStore(t)
	_, hasList := probe.(sessions.Lister)
	_, hasSummaries := probe.(sessions.SummaryLister)
	_, hasDelete := probe.(sessions.Deleter)
	_, hasSubkeys := probe.(sessions.SubkeyLister)
	has := map[string]bool{
		ListSessions:         hasList && !skip[ListSessions],
		ListSessionSummaries: hasSummaries && !skip[ListSessionSummaries],
		Delete:               hasDelete && !skip[Delete],
		ListSubkeys:          hasSubkeys && !skip[ListSubkeys],
	}

	for _, c := range contracts {
		t.Run(c.name, func(t *testing.T) {
			if c.requires != "" && !has[c.requires] {
				t.Skipf("store does not implement or skips %s", c.requires)
			}
			c.run(&checker{T: t, store: makeStore(t), has: has})
		})
	}
}

type contract struct {
	name     string
	requires string // optional method the contract needs, or ""
	run      func(c *checker)
}

// checker wraps a store with fail-fast helpers.
type checker struct {
	*testing.T
	store sessions.Store
	has   map[string]bool
}

// e builds a test entry. Every entry has a "type"; its value is irrelevant
// to the contracts, since adapters treat entries as opaque.
func e(kv ...any) sessions.Entry {
	entry := sessions.Entry{"type": "x"}
	for i := 0; i < len(kv); i += 2 {
		entry[kv[i].(string)] = kv[i+1]
	}
	return entry
}

func sub(k sessions.Key, subpath string) sessions.Key {
	k.Subpath = subpath
	return k
}

// normalize round-trips v through JSON.
func normalize(t *testing.T, v any) any {
	t.Helper()
	b, err := jsonx.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %v: %v", v, err)
	}
	var out any
	if err := jsonx.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func (c *checker) append(k sessions.Key, entries ...sessions.Entry) {
	c.Helper()
	if entries == nil {
		entries = []sessions.Entry{}
	}
	if err := c.store.Append(c.Context(), k, entries); err != nil {
		c.Fatalf("Append(%+v): %v", k, err)
	}
}

func (c *checker) load(k sessions.Key) []sessions.Entry {
	c.Helper()
	got, err := c.store.Load(c.Context(), k)
	if err != nil {
		c.Fatalf("Load(%+v): %v", k, err)
	}
	return got
}

// loadEqual asserts that k loads entries deep-equal to want.
func (c *checker) loadEqual(k sessions.Key, want ...sessions.Entry) {
	c.Helper()
	got := c.load(k)
	if len(got) != len(want) || !reflect.DeepEqual(normalize(c.T, got), normalize(c.T, want)) {
		c.Fatalf("Load(%+v) = %v, want %v", k, got, want)
	}
}

// loadMissing asserts that k loads nothing.
func (c *checker) loadMissing(k sessions.Key) {
	c.Helper()
	if got := c.load(k); len(got) != 0 {
		c.Fatalf("Load(%+v) = %v, want nil", k, got)
	}
}

func (c *checker) listSessions(projectKey string) []sessions.ListEntry {
	c.Helper()
	got, err := c.store.(sessions.Lister).ListSessions(c.Context(), projectKey)
	if err != nil {
		c.Fatalf("ListSessions(%q): %v", projectKey, err)
	}
	return got
}

func (c *checker) listSummaries(projectKey string) []sessions.SummaryEntry {
	c.Helper()
	got, err := c.store.(sessions.SummaryLister).ListSessionSummaries(c.Context(), projectKey)
	if err != nil {
		c.Fatalf("ListSessionSummaries(%q): %v", projectKey, err)
	}
	return got
}

func (c *checker) delete(k sessions.Key) {
	c.Helper()
	if err := c.store.(sessions.Deleter).Delete(c.Context(), k); err != nil {
		c.Fatalf("Delete(%+v): %v", k, err)
	}
}

func (c *checker) listSubkeys(projectKey, sessionID string) []string {
	c.Helper()
	got, err := c.store.(sessions.SubkeyLister).ListSubkeys(c.Context(),
		sessions.ListSubkeysKey{ProjectKey: projectKey, SessionID: sessionID})
	if err != nil {
		c.Fatalf("ListSubkeys(%q, %q): %v", projectKey, sessionID, err)
	}
	return got
}

func sessionIDs(listing []sessions.ListEntry) []string {
	ids := make([]string, len(listing))
	for i, e := range listing {
		ids[i] = e.SessionID
	}
	return ids
}

// minEpochMillis rules out epoch seconds: 1e12 ms is in 2001.
const minEpochMillis = 1e12

var contracts = []contract{
	// --- Required: Append and Load --------------------------------------

	{"AppendThenLoad", "", func(c *checker) {
		// Deep equality is the contract, not byte-equal serialization
		// (Postgres JSONB reorders keys; the SDK never byte-compares).
		c.append(key, e("uuid", "b", "n", 1), e("uuid", "a", "n", 2))
		c.loadEqual(key, e("uuid", "b", "n", 1), e("uuid", "a", "n", 2))
	}},

	{"LoadUnknownKey", "", func(c *checker) {
		c.loadMissing(sessions.Key{ProjectKey: "proj", SessionID: "nope"})
		c.append(key, e("uuid", "x", "n", 1))
		c.loadMissing(sub(key, "nope"))
	}},

	{"AppendPreservesCallOrder", "", func(c *checker) {
		c.append(key, e("uuid", "z", "n", 1))
		c.append(key, e("uuid", "a", "n", 2), e("uuid", "m", "n", 3))
		c.append(key, e("uuid", "b", "n", 4))
		c.loadEqual(key, e("uuid", "z", "n", 1), e("uuid", "a", "n", 2), e("uuid", "m", "n", 3), e("uuid", "b", "n", 4))
	}},

	{"AppendEmptyIsNoOp", "", func(c *checker) {
		c.append(key, e("uuid", "a", "n", 1))
		c.append(key)
		c.loadEqual(key, e("uuid", "a", "n", 1))
	}},

	{"SubpathStoredIndependently", "", func(c *checker) {
		subKey := sub(key, "subagents/agent-1")
		c.append(key, e("uuid", "m", "n", 1))
		c.append(subKey, e("uuid", "s", "n", 1))
		c.loadEqual(key, e("uuid", "m", "n", 1))
		c.loadEqual(subKey, e("uuid", "s", "n", 1))
	}},

	{"ProjectKeyIsolation", "", func(c *checker) {
		a := sessions.Key{ProjectKey: "A", SessionID: "s1"}
		b := sessions.Key{ProjectKey: "B", SessionID: "s1"}
		c.append(a, e("from", "A"))
		c.append(b, e("from", "B"))
		c.loadEqual(a, e("from", "A"))
		c.loadEqual(b, e("from", "B"))
		if c.has[ListSessions] {
			if n := len(c.listSessions("A")); n != 1 {
				c.Errorf("ListSessions(A) has %d sessions, want 1", n)
			}
			if n := len(c.listSessions("B")); n != 1 {
				c.Errorf("ListSessions(B) has %d sessions, want 1", n)
			}
		}
	}},

	// --- Optional: ListSessions ------------------------------------------

	{"ListSessions", ListSessions, func(c *checker) {
		c.append(sessions.Key{ProjectKey: "proj", SessionID: "a"}, e("n", 1))
		c.append(sessions.Key{ProjectKey: "proj", SessionID: "b"}, e("n", 1))
		c.append(sessions.Key{ProjectKey: "other", SessionID: "c"}, e("n", 1))
		listing := c.listSessions("proj")
		if ids := sessionIDs(listing); !slices.Equal(sortedCopy(ids), []string{"a", "b"}) {
			c.Errorf("session ids = %v, want [a b]", ids)
		}
		for _, s := range listing {
			if s.MTime <= minEpochMillis {
				c.Errorf("%s: mtime %d is not in Unix epoch milliseconds", s.SessionID, s.MTime)
			}
		}
		if got := c.listSessions("never-appended-project"); len(got) != 0 {
			c.Errorf("unknown project = %v, want none", got)
		}
	}},

	{"ListSessionsExcludesSubpaths", ListSessions, func(c *checker) {
		main := sessions.Key{ProjectKey: "proj", SessionID: "main"}
		c.append(main, e("n", 1))
		c.append(sub(main, "subagents/agent-1"), e("n", 1))
		if ids := sessionIDs(c.listSessions("proj")); !slices.Equal(ids, []string{"main"}) {
			c.Errorf("session ids = %v, want [main]", ids)
		}
	}},

	// --- Optional: ListSessionSummaries ----------------------------------

	{"ListSessionSummaries", ListSessionSummaries, func(c *checker) {
		// Summaries are FoldSummary output persisted verbatim; they must
		// fold again. Stores must not interpret Data.
		k := sessions.Key{ProjectKey: "proj", SessionID: "summ-sess"}
		c.append(k,
			e("timestamp", "2024-01-01T00:00:00.000Z", "customTitle", "first"),
			e("timestamp", "2024-01-01T00:00:01.000Z"))
		c.append(k, e("timestamp", "2024-01-01T00:00:02.000Z", "customTitle", "second"))
		c.append(sessions.Key{ProjectKey: "other", SessionID: "elsewhere"}, e("timestamp", "2024-01-01T00:00:00.000Z"))

		summaries := c.listSummaries("proj")
		if len(summaries) != 1 || summaries[0].SessionID != "summ-sess" {
			c.Fatalf("summaries = %+v, want only summ-sess", summaries)
		}
		summ := summaries[0]
		if summ.MTime <= minEpochMillis {
			c.Errorf("mtime %d is not in Unix epoch milliseconds", summ.MTime)
		}
		// MTime is the storage write time, on the clock ListSessions uses.
		// Deriving it from entry timestamps would make every summary look
		// stale to ListInStore.
		if c.has[ListSessions] {
			for _, s := range c.listSessions("proj") {
				if s.SessionID == "summ-sess" && summ.MTime < s.MTime {
					c.Errorf("summary mtime %d is older than the ListSessions mtime %d", summ.MTime, s.MTime)
				}
			}
		}
		if summ.Data == nil {
			c.Fatal("summary Data is nil")
		}
		refolded := sessions.FoldSummary(&summ, k, []sessions.Entry{e("timestamp", "2024-01-01T00:00:03.000Z")}, nil)
		if refolded.SessionID != "summ-sess" || refolded.MTime != summ.MTime {
			c.Errorf("refolded = %+v", refolded)
		}

		// Subagent appends must not affect the main session's summary.
		c.append(sub(k, "subagents/agent-1"), e("timestamp", "2024-01-01T00:00:09.000Z", "customTitle", "subagent"))
		var after *sessions.SummaryEntry
		for _, s := range c.listSummaries("proj") {
			if s.SessionID == "summ-sess" {
				after = &s
			}
		}
		if after == nil || !reflect.DeepEqual(normalize(c.T, after.Data), normalize(c.T, summ.Data)) {
			c.Errorf("summary after a subagent append = %+v, want data %v", after, summ.Data)
		}
		if got := c.listSummaries("never-appended-project"); len(got) != 0 {
			c.Errorf("unknown project = %v, want none", got)
		}
		if c.has[Delete] {
			c.delete(k)
			if got := c.listSummaries("proj"); len(got) != 0 {
				c.Errorf("summaries after Delete = %+v, want none", got)
			}
		}
	}},

	// --- Optional: Delete ------------------------------------------------

	{"DeleteMain", Delete, func(c *checker) {
		c.delete(sessions.Key{ProjectKey: "proj", SessionID: "never-written"})
		c.append(key, e("n", 1))
		c.delete(key)
		c.loadMissing(key)
	}},

	{"DeleteMainCascadesToSubkeys", Delete, func(c *checker) {
		sub1, sub2 := sub(key, "subagents/agent-1"), sub(key, "subagents/agent-2")
		other := sessions.Key{ProjectKey: "proj", SessionID: "sess2"}
		otherProject := sessions.Key{ProjectKey: "other-proj", SessionID: key.SessionID}
		for _, k := range []sessions.Key{key, sub1, sub2, other, otherProject} {
			c.append(k, e("n", 1))
		}
		c.delete(key)
		c.loadMissing(key)
		c.loadMissing(sub1)
		c.loadMissing(sub2)
		c.loadEqual(other, e("n", 1))
		c.loadEqual(otherProject, e("n", 1))
		if c.has[ListSubkeys] {
			if got := c.listSubkeys(key.ProjectKey, key.SessionID); len(got) != 0 {
				c.Errorf("subkeys after Delete = %v, want none", got)
			}
		}
		if c.has[ListSessions] {
			if ids := sessionIDs(c.listSessions(key.ProjectKey)); slices.Contains(ids, key.SessionID) {
				c.Errorf("ListSessions after Delete = %v", ids)
			}
		}
	}},

	{"DeleteSubpathOnly", Delete, func(c *checker) {
		sub1, sub2 := sub(key, "subagents/agent-1"), sub(key, "subagents/agent-2")
		c.append(key, e("n", 1))
		c.append(sub1, e("n", 1))
		c.append(sub2, e("n", 1))
		c.delete(sub1)
		c.loadMissing(sub1)
		c.loadEqual(sub2, e("n", 1))
		c.loadEqual(key, e("n", 1))
		if c.has[ListSubkeys] {
			if got := c.listSubkeys(key.ProjectKey, key.SessionID); !slices.Equal(got, []string{"subagents/agent-2"}) {
				c.Errorf("subkeys = %v, want [subagents/agent-2]", got)
			}
		}
	}},

	// --- Optional: ListSubkeys -------------------------------------------

	{"ListSubkeys", ListSubkeys, func(c *checker) {
		c.append(key, e("n", 1))
		c.append(sub(key, "subagents/agent-1"), e("n", 1))
		c.append(sub(key, "subagents/agent-2"), e("n", 1))
		c.append(sessions.Key{ProjectKey: key.ProjectKey, SessionID: "other-sess", Subpath: "subagents/agent-x"}, e("n", 1))
		if got := c.listSubkeys(key.ProjectKey, key.SessionID); !slices.Equal(sortedCopy(got), []string{"subagents/agent-1", "subagents/agent-2"}) {
			c.Errorf("subkeys = %v, want [subagents/agent-1 subagents/agent-2]", got)
		}
	}},

	{"ListSubkeysExcludesMain", ListSubkeys, func(c *checker) {
		c.append(key, e("n", 1))
		if got := c.listSubkeys(key.ProjectKey, key.SessionID); len(got) != 0 {
			c.Errorf("subkeys = %v, want none", got)
		}
		if got := c.listSubkeys("proj", "never-appended"); len(got) != 0 {
			c.Errorf("subkeys of an unknown session = %v, want none", got)
		}
	}},
}

func sortedCopy(s []string) []string {
	s = slices.Clone(s)
	slices.Sort(s)
	return s
}
