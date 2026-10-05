package sessions

import (
	"bufio"
	"context"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"

	"github.com/ironpark/gelati/claude/internal/ids"
	"github.com/ironpark/gelati/claude/internal/transcript"
	"github.com/ironpark/gelati/internal/jsonx"
)

// Replaying a local transcript into a Store, the inverse of resume
// materialization. Ported from _internal/session_import.py.

// ImportToStore replays a local session transcript into store: it streams
// <project>/<sessionID>.jsonl and calls store.Append every opts.BatchSize
// entries (default 500) or 1 MiB of line bytes, whichever comes first. Use
// it to migrate local sessions to a remote store, or to catch a store up
// after a claude.MirrorErrorMessage reported a gap in the live mirror.
// Adapters treat entry uuids as idempotency keys, so re-importing is safe.
//
// The destination project key is ProjectKey(opts.Directory), as in the
// TypeScript SDK: with opts.Directory set it is the key the transcript
// mirror uses for sessions of that directory, so an imported session can be
// resumed through the store from there. With opts.Directory empty, every
// project is searched for the transcript but the key is that of the current
// working directory, not of the project the transcript was found in; pass
// opts.Directory to import a session of another project under its own key.
//
// opts may be nil. opts.Directory is the project path, with the same
// semantics as ListOptions.Directory; empty searches every project. Unless
// opts.ExcludeSubagents is set, the transcripts under <sessionID>/subagents/
// (recursively, in name order) are imported too, under subpaths such as
// "subagents/agent-<id>", each followed by its agent-<id>.meta.json sidecar
// as an "agent_metadata" entry. A missing, corrupt or non-object sidecar is
// skipped.
//
// Blank lines and lines that are not a JSON object are skipped; invalid
// UTF-8 inside strings becomes U+FFFD. It returns an error wrapping
// ErrInvalidID when sessionID is not a UUID and one wrapping ErrNotFound
// when the transcript is not found; store and I/O errors are returned.
func ImportToStore(ctx context.Context, sessionID string, store Store, opts *ImportOptions) error {
	return localSessionsFromEnv().importSession(ctx, sessionID, store, opts, os.Getenv)
}

// importSession implements ImportToStore; getenv is the environment the
// destination project key is derived from (see transcript.ProjectKey).
func (s localSessions) importSession(ctx context.Context, sessionID string, store Store, opts *ImportOptions, getenv func(string) string) error {
	if opts == nil {
		opts = &ImportOptions{}
	}
	if !ids.IsUUID(sessionID) {
		return invalidSessionIDError(sessionID)
	}
	resolved := s.resolveSessionFilePath(sessionID, opts.Directory)
	if resolved == "" {
		return sessionNotFoundError(sessionID, opts.Directory)
	}
	projectKey := transcript.ProjectKey(opts.Directory, getenv)
	batchSize := opts.BatchSize
	if batchSize <= 0 {
		batchSize = transcript.AppendBatchEntries
	}

	mainKey := Key{ProjectKey: projectKey, SessionID: sessionID}
	if err := appendJSONLFileInBatches(ctx, resolved, mainKey, store, batchSize); err != nil {
		return err
	}
	if opts.ExcludeSubagents {
		return nil
	}

	sessionDir := strings.TrimSuffix(resolved, ".jsonl")
	for _, path := range collectJSONLFiles(filepath.Join(sessionDir, "subagents")) {
		// The subpath is relative to the session directory, "/"-joined and
		// without .jsonl, matching the live mirror's mapping, e.g.
		// subagents/workflows/run-1/agent-def.
		rel, err := filepath.Rel(sessionDir, path)
		if err != nil {
			return err
		}
		subKey := Key{
			ProjectKey: projectKey,
			SessionID:  sessionID,
			Subpath:    strings.TrimSuffix(filepath.ToSlash(rel), ".jsonl"),
		}
		if err := appendJSONLFileInBatches(ctx, path, subKey, store, batchSize); err != nil {
			return err
		}

		// Agent metadata is not in the transcript: it is only sent to live
		// mirrors and kept in the .meta.json sidecar. Import the sidecar so
		// resume can recreate it.
		meta, err := readAgentMetadataSidecar(path)
		if err != nil {
			return err
		}
		if meta != nil {
			entry := maps.Clone(meta)
			entry["type"] = "agent_metadata" // a stray "type" in the sidecar cannot shadow it
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := store.Append(ctx, subKey, []Entry{entry}); err != nil {
				return err
			}
		}
	}
	return nil
}

// appendJSONLFileInBatches streams a JSONL file into store.Append in batches
// of batchSize entries or transcript.AppendBatchBytes of line bytes,
// whichever comes first. Blank lines and lines that are not a JSON object
// are skipped; "\r\n" endings are accepted.
func appendJSONLFileInBatches(ctx context.Context, path string, key Key, store Store, batchSize int) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	var batch []Entry
	nbytes := 0
	flush := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := store.Append(ctx, key, batch)
		batch, nbytes = nil, 0 // the store may retain the slice
		return err
	}

	r := bufio.NewReader(f)
	for {
		line, readErr := r.ReadString('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		var entry Entry
		if line != "" && jsonx.Unmarshal([]byte(line), &entry) == nil && entry != nil {
			batch = append(batch, entry)
			nbytes += len(line)
			if len(batch) >= batchSize || nbytes >= transcript.AppendBatchBytes {
				if err := flush(); err != nil {
					return err
				}
			}
		}
		if readErr != nil {
			break
		}
	}
	if len(batch) > 0 {
		return flush()
	}
	return nil
}
