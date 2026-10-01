package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"
)

// Replaying a local transcript into a SessionStore, the inverse of resume
// materialization. Ported from _internal/session_import.py.

// ImportSessionToStore replays a local session transcript into store: it
// streams <project>/<sessionID>.jsonl and calls store.Append every
// opts.BatchSize entries (default 500) or 1 MiB of line bytes, whichever
// comes first. Use it to migrate local sessions to a remote store, or to
// catch a store up after a MirrorErrorMessage reported a gap in the live
// mirror. Adapters treat entry uuids as idempotency keys, so re-importing is
// safe.
//
// The destination project key is ProjectKeyForDirectory(opts.Directory),
// as in the TypeScript SDK: with opts.Directory set it is the key the
// transcript mirror uses for sessions of that directory, so an imported
// session can be resumed through the store from there. With opts.Directory
// empty, every project is searched for the transcript but the key is that
// of the current working directory, not of the project the transcript was
// found in; pass opts.Directory to import a session of another project
// under its own key.
//
// opts may be nil. opts.Directory is the project path, with the same
// semantics as ListSessionsOptions.Directory; empty searches every project.
// Unless opts.ExcludeSubagents is set, the transcripts under
// <sessionID>/subagents/ (recursively, in name order) are imported too,
// under subpaths such as "subagents/agent-<id>", each followed by its
// agent-<id>.meta.json sidecar as an "agent_metadata" entry. A missing,
// corrupt or non-object sidecar is skipped.
//
// Blank lines and lines that are not a JSON object are skipped; invalid
// UTF-8 inside strings becomes U+FFFD. It returns an error wrapping ErrInvalidSessionID when sessionID
// is not a UUID and one wrapping ErrSessionNotFound when the transcript is
// not found; store and I/O errors are returned.
func ImportSessionToStore(ctx context.Context, sessionID string, store SessionStore, opts *ImportSessionOptions) error {
	return importSessionIn(ctx, projectsDir(nil), sessionID, store, opts, os.Getenv)
}

func importSessionIn(ctx context.Context, root, sessionID string, store SessionStore, opts *ImportSessionOptions, getenv func(string) string) error {
	if opts == nil {
		opts = &ImportSessionOptions{}
	}
	if !validateUUID(sessionID) {
		return invalidSessionIDError(sessionID)
	}
	resolved := resolveSessionFilePath(root, sessionID, opts.Directory)
	if resolved == "" {
		return sessionNotFoundError(sessionID, opts.Directory)
	}
	projectKey := projectKeyForDirectory(opts.Directory, getenv)
	batchSize := opts.BatchSize
	if batchSize <= 0 {
		batchSize = mirrorMaxPendingEntries
	}

	mainKey := SessionKey{ProjectKey: projectKey, SessionID: sessionID}
	if err := appendJSONLFileInBatches(ctx, resolved, mainKey, store, batchSize); err != nil {
		return err
	}
	if opts.ExcludeSubagents {
		return nil
	}

	sessionDir := strings.TrimSuffix(resolved, ".jsonl")
	for _, path := range collectJSONLFiles(filepath.Join(sessionDir, "subagents")) {
		// The subpath is relative to the session directory, "/"-joined and
		// without .jsonl, matching filePathToSessionKey, e.g.
		// subagents/workflows/run-1/agent-def.
		rel, err := filepath.Rel(sessionDir, path)
		if err != nil {
			return err
		}
		subKey := SessionKey{
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
			if err := store.Append(ctx, subKey, []SessionStoreEntry{entry}); err != nil {
				return err
			}
		}
	}
	return nil
}

// appendJSONLFileInBatches streams a JSONL file into store.Append in
// batches of batchSize entries or mirrorMaxPendingBytes of line bytes,
// whichever comes first. Blank lines and lines that are not a JSON object
// are skipped; "\r\n" endings are accepted.
func appendJSONLFileInBatches(ctx context.Context, path string, key SessionKey, store SessionStore, batchSize int) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	var batch []SessionStoreEntry
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
		var entry SessionStoreEntry
		if line != "" && json.Unmarshal([]byte(line), &entry) == nil && entry != nil {
			batch = append(batch, entry)
			nbytes += len(line)
			if len(batch) >= batchSize || nbytes >= mirrorMaxPendingBytes {
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

// collectJSONLFiles returns every *.jsonl file under baseDir, recursively,
// sorted by name within each directory, following symlinks. It returns nil
// when baseDir cannot be read.
func collectJSONLFiles(baseDir string) []string {
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		switch {
		case direntIsDir(baseDir, e):
			out = append(out, collectJSONLFiles(filepath.Join(baseDir, e.Name()))...)
		case direntIsFile(baseDir, e) && strings.HasSuffix(e.Name(), ".jsonl"):
			out = append(out, filepath.Join(baseDir, e.Name()))
		}
	}
	return out
}
