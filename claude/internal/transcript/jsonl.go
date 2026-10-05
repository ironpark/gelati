package transcript

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"maps"
	"slices"

	"github.com/ironpark/gelati/internal/jsonx"
)

// JSONL serialization of transcript entries, shared by the session readers
// and mutations, resume materialization and import. Every writer emits
// compact JSON without HTML escaping; entries whose key order is unknown
// (decoded maps) are written with "type" first, where the CLI writes it, and
// the other keys sorted: adapters may reorder keys (Postgres JSONB does).

const (
	// AppendBatchEntries and AppendBatchBytes bound the batches sent to a
	// session store's Append: they are the batched-mode mirror thresholds
	// past which a background flush starts, and the batch limits of
	// sessions.ImportToStore.
	AppendBatchEntries = 500
	AppendBatchBytes   = 1 << 20
)

// TypeFirstKeys returns the keys of e with "type" (when present) first and
// the others sorted.
func TypeFirstKeys(e map[string]any) []string {
	keys := make([]string, 1, len(e)+1)
	for k := range e {
		if k != "type" {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys[1:])
	if _, ok := e["type"]; ok {
		keys[0] = "type"
		return keys
	}
	return keys[1:]
}

// JSONAppender appends values to byte slices as compact JSON without HTML
// escaping or a trailing newline, map keys sorted. It reuses one scratch
// buffer across calls, so a batch of writes shares it. The zero value is
// ready to use.
type JSONAppender struct {
	buf bytes.Buffer
}

// Append appends v to dst. On error dst is returned unchanged.
func (a *JSONAppender) Append(dst []byte, v any) ([]byte, error) {
	a.buf.Reset()
	if err := json.MarshalWrite(&a.buf, v, jsonx.LegacyEncode); err != nil {
		return dst, err
	}
	return append(dst, a.buf.Bytes()...), nil
}

// AppendObject appends the n fields returned by field, in order, to dst as
// a JSON object. When strict is set, a value that cannot be encoded is an
// error naming its key after label; otherwise it is written as null.
func (a *JSONAppender) AppendObject(dst []byte, n int, field func(i int) (string, any),
	strict bool, label string,
) ([]byte, error) {
	dst = append(dst, '{')
	for i := range n {
		k, v := field(i)
		if i > 0 {
			dst = append(dst, ',')
		}
		var err error
		if dst, err = a.Append(dst, k); err != nil {
			return dst, err
		}
		dst = append(dst, ':')
		next, err := a.Append(dst, v)
		if err != nil {
			if strict {
				return dst, fmt.Errorf("%s %q: %w", label, k, err)
			}
			next = append(dst, "null"...)
		}
		dst = next
	}
	return append(dst, '}'), nil
}

// AppendEntry appends entry e to dst as a JSON object with its keys in
// TypeFirstKeys order, without a trailing newline; a nil entry is written as
// null. When strict is set, a value that cannot be encoded is an error;
// otherwise it is written as null.
func (a *JSONAppender) AppendEntry(dst []byte, e map[string]any, strict bool) ([]byte, error) {
	if e == nil {
		return append(dst, "null"...), nil
	}
	keys := TypeFirstKeys(e)
	return a.AppendObject(dst, len(keys), func(i int) (string, any) {
		return keys[i], e[keys[i]]
	}, strict, "entry field")
}

// SplitAgentMetadata separates the synthetic agent_metadata entries a
// subagent's session store stream carries in place of the .meta.json
// sidecar from its transcript lines. The last metadata entry wins (it is
// rewritten on resume) and is returned as a copy; metadata is nil when there
// is none.
func SplitAgentMetadata(entries []map[string]any) (metadata map[string]any, transcript []map[string]any) {
	for _, e := range entries {
		if e != nil && e["type"] == "agent_metadata" {
			metadata = maps.Clone(e)
		} else {
			transcript = append(transcript, e)
		}
	}
	return metadata, transcript
}
