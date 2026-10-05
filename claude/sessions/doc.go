// Package sessions reads and manages Claude Code session transcripts, both
// the CLI's local JSONL files and copies mirrored to a [Store]. It is the Go
// port of the session functions of the Claude Agent Python and TypeScript
// SDKs; package claude runs the sessions themselves and, through
// claude.Options.SessionStore, mirrors their transcripts to a Store and
// resumes them from it.
//
// # Local transcripts
//
// [List], [GetInfo], [GetMessages], [ListSubagents] and
// [GetSubagentMessages] read the transcripts the CLI writes under
// $CLAUDE_CONFIG_DIR/projects (default ~/.claude/projects), one directory
// per project; [Rename], [Tag], [Delete] and [Fork] change them. They need no
// running CLI and take no context.
//
// A directory argument (or the Directory option) names a project. With it
// set, the project's transcript directory is searched and, for lookups by
// session id, those of the repository's other git worktrees as well; [List]
// also merges the sessions of every worktree unless
// ListOptions.ExcludeWorktrees is set. With it empty, every project is
// searched. When CLAUDE_CONFIG_DIR and a valid CLAUDE_CODE_PROJECT_DIR_NAME
// are both set, the latter names the project directory of every working
// directory, as in the CLI.
//
// # Store-backed variants
//
// Each local function has a counterpart suffixed InStore ([ListInStore],
// [GetInfoInStore], [GetMessagesInStore], [ListSubagentsInStore],
// [GetSubagentMessagesInStore], [RenameInStore], [TagInStore],
// [DeleteInStore] and [ForkInStore]) that works on a [Store] instead, with
// the same metadata derivation so both paths agree for the same transcript.
// They differ from the local functions in that:
//
//   - they take a context and return the store's errors;
//   - a store holds one key space per project: the directory argument
//     selects the project key ([ProjectKey]) and empty means the current
//     working directory, not every project; there is no worktree merging
//     and ListOptions.ExcludeProgrammatic is ignored;
//   - Info.FileSize is the size of the transcript serialized as compact
//     JSONL, or zero for rows built from a session summary;
//   - optional capabilities are probed with type assertions: [ListInStore]
//     needs a [SummaryLister] or a [Lister], [ListSubagentsInStore] needs a
//     [SubkeyLister] ([GetSubagentMessagesInStore] uses one, when present,
//     to find nested subagent transcripts), and [DeleteInStore] is a no-op
//     without a [Deleter];
//   - [RenameInStore] and [TagInStore] append without checking that the
//     session exists.
//
// [ImportToStore] copies a local session, with its subagent transcripts,
// into a store; with ImportOptions.Directory set it uses the key the live
// transcript mirror uses for sessions of that directory.
//
// # Implementing a Store
//
// A [Store] needs only Append and Load; [Lister], [SummaryLister], [Deleter]
// and [SubkeyLister] are optional. Stores that implement [SummaryLister]
// maintain per-session summaries with [FoldSummary] inside Append, which lets
// [ListInStore] list a project in one call. [InMemoryStore] is the reference
// implementation, and package sessionstoretest checks an adapter against
// the Store contracts.
//
// # Errors
//
// The mutations and [ImportToStore] return an error wrapping [ErrInvalidID]
// when the session id is not a UUID (session ids become path components)
// and, where they need an existing transcript, one wrapping [ErrNotFound]
// when it does not exist. The readers report a missing session, or an id
// that is not a UUID, as a nil result instead.
package sessions
