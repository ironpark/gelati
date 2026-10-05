package claude

import (
	"cmp"
	"maps"
)

// Incremental session summaries for SessionStore adapters. Ported from
// _internal/session_summary.py and aligned with the TypeScript SDK's
// foldSessionSummary (data keys, relocation handling, mtime option).
//
// A store that implements SessionSummaryLister keeps one SessionSummaryEntry
// per session up to date by calling FoldSessionSummary inside Append, so that
// ListSessionsFromStore can fetch every session's metadata in a single call
// instead of loading each transcript. Every derived field is set-once or
// last-wins, so a fold never needs previously appended entries.

// summaryLastWinsFields maps transcript entry keys to SessionSummaryEntry
// data keys for string fields where each appended value replaces the
// previous one.
var summaryLastWinsFields = [...]struct{ src, dst string }{
	{"customTitle", "customTitle"},
	{"aiTitle", "aiTitle"},
	{"lastPrompt", "lastPrompt"},
	{"summary", "summaryHint"},
	{"gitBranch", "gitBranch"},
}

// legacySummaryKeys maps the snake_case data keys written by the Python SDK
// (and earlier versions of this package) to the TypeScript SDK's camelCase
// keys, which FoldSessionSummary writes.
var legacySummaryKeys = [...]struct{ legacy, key string }{
	{"is_sidechain", "isSidechain"},
	{"created_at", "createdAt"},
	{"first_prompt", "firstPrompt"},
	{"first_prompt_locked", "firstPromptLocked"},
	{"command_fallback", "commandFallback"},
	{"custom_title", "customTitle"},
	{"ai_title", "aiTitle"},
	{"last_prompt", "lastPrompt"},
	{"summary_hint", "summaryHint"},
	{"git_branch", "gitBranch"},
}

// summaryValue reads a summary data field by its camelCase key, falling
// back to the legacy snake_case key.
func summaryValue(data map[string]any, key string) any {
	if v, ok := data[key]; ok {
		return v
	}
	for _, k := range legacySummaryKeys {
		if k.key == key {
			return data[k.legacy]
		}
	}
	return nil
}

// FoldSessionOptions configures FoldSessionSummary.
type FoldSessionOptions struct {
	// MTime, when non-zero, stamps the returned summary's MTime (Unix
	// epoch milliseconds): the storage write time of the summary, on the
	// same clock as SessionStoreListEntry.MTime. Zero keeps prev's MTime.
	MTime int64
}

// FoldSessionSummary folds a batch of appended entries into the running
// summary for key and returns the updated summary. prev is the previous
// summary for the same key, or nil for the first append; it is not
// modified. opts may be nil. Alpha.
//
// Stores call it from Append to maintain the summaries returned by
// SessionSummaryLister. Do not call it for keys with a Subpath: subagent
// transcripts must not contribute to the main session's summary. All
// derived state lives in the opaque Data map, which stores persist verbatim
// without interpreting it. Set-once fields (isSidechain, createdAt, cwd,
// firstPrompt) freeze on first sight; last-wins fields (customTitle,
// aiTitle, lastPrompt, summaryHint, gitBranch, tag) and relocations of the
// working directory overwrite on every appearance.
//
// Data uses the TypeScript SDK's camelCase keys, so a store shared with
// TypeScript processes stays readable by both. A prev written with the
// snake_case keys of the Python SDK or earlier versions of this package is
// migrated to camelCase; ListSessionsFromStore reads either form.
//
// MTime is not derived from the entries: it is carried over from prev (zero
// for a new session) and the adapter must stamp it with the storage write
// time when persisting, on the same clock as SessionStoreListEntry.MTime,
// either afterwards or through FoldSessionOptions.MTime. Entry timestamps
// would make every batched sidecar look older than its session and defeat
// the staleness check of ListSessionsFromStore.
func FoldSessionSummary(prev *SessionSummaryEntry, key SessionKey, entries []SessionStoreEntry, opts *FoldSessionOptions) SessionSummaryEntry {
	summary := SessionSummaryEntry{SessionID: key.SessionID, Data: map[string]any{}}
	if prev != nil {
		summary.SessionID = prev.SessionID
		summary.MTime = prev.MTime
		if prev.Data != nil {
			summary.Data = maps.Clone(prev.Data)
		}
	}
	if opts != nil && opts.MTime != 0 {
		summary.MTime = opts.MTime
	}
	data := summary.Data
	for _, k := range legacySummaryKeys {
		if v, ok := data[k.legacy]; ok {
			if _, exists := data[k.key]; !exists {
				data[k.key] = v
			}
			delete(data, k.legacy)
		}
	}

	for _, entry := range entries {
		if _, ok := data["isSidechain"]; !ok {
			data["isSidechain"] = entry["isSidechain"] == true
		}
		if _, ok := data["createdAt"]; !ok {
			if ts, ok := entry["timestamp"].(string); ok {
				if ms, ok := isoToEpochMillis(ts); ok {
					data["createdAt"] = ms
				}
			}
		}
		if _, ok := data["cwd"]; !ok {
			if cwd, ok := entry["cwd"].(string); ok && cwd != "" {
				data["cwd"] = cwd
			}
		}

		foldFirstPrompt(data, entry)

		for _, f := range summaryLastWinsFields {
			if v, ok := entry[f.src].(string); ok {
				data[f.dst] = v
			}
		}

		switch entry["type"] {
		case "tag":
			if tag, ok := entry["tag"].(string); ok && tag != "" {
				data["tag"] = tag
			} else {
				// An empty or absent tag clears it.
				delete(data, "tag")
			}
		case "relocated":
			if cwd, ok := entry["relocatedCwd"].(string); ok && cwd != "" {
				data["cwd"] = cwd
			}
		}
	}
	return summary
}

// foldFirstPrompt replicates extractFirstPromptFromHead for one parsed
// entry: it latches firstPrompt (and firstPromptLocked) on a real prompt,
// or remembers the first slash command as commandFallback.
func foldFirstPrompt(data map[string]any, entry SessionStoreEntry) {
	if jsTruthy(data["firstPromptLocked"]) {
		return
	}
	fallback, _ := data["commandFallback"].(string)
	prompt, ok := promptFromUserEntry(entry, &fallback)
	if fallback != "" && !jsTruthy(data["commandFallback"]) {
		data["commandFallback"] = fallback
	}
	if ok {
		data["firstPrompt"] = prompt
		data["firstPromptLocked"] = true
	}
}

// summaryEntryToSessionInfo converts a SessionSummaryEntry to SessionInfo,
// reading camelCase or legacy snake_case data keys. It returns nil for
// sidechain sessions and sessions with no extractable summary, matching
// parseSessionInfoFromLite. projectPath is the Cwd fallback and may be
// empty. FileSize is always zero: summaries carry no size.
func summaryEntryToSessionInfo(entry SessionSummaryEntry, projectPath string) *SessionInfo {
	data := entry.Data
	get := func(key string) string { return str(summaryValue(data, key)) }
	if summaryValue(data, "isSidechain") == true {
		return nil
	}
	var firstPrompt string
	if summaryValue(data, "firstPromptLocked") == true {
		firstPrompt = get("firstPrompt")
	} else {
		firstPrompt = get("commandFallback")
	}
	customTitle := cmp.Or(get("customTitle"), get("aiTitle"))
	summary := cmp.Or(customTitle, get("lastPrompt"), get("summaryHint"), firstPrompt)
	if summary == "" {
		return nil
	}
	createdAt, _ := toInt64(summaryValue(data, "createdAt"))
	return &SessionInfo{
		SessionID:    entry.SessionID,
		Summary:      summary,
		LastModified: entry.MTime,
		CustomTitle:  customTitle,
		FirstPrompt:  firstPrompt,
		GitBranch:    get("gitBranch"),
		Cwd:          cmp.Or(get("cwd"), projectPath),
		Tag:          get("tag"),
		CreatedAt:    createdAt,
	}
}
