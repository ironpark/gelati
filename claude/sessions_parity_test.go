package claude

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// Cross-checks of the local session readers against the TypeScript SDK's
// runtime: both read the same CLAUDE_CONFIG_DIR and must return the same
// JSON. The test needs node and the TS SDK bundle (GELATI_TS_SDK, default
// /tmp/casdk/package/sdk.mjs) and is skipped without them.

const defaultTSSDKPath = "/tmp/casdk/package/sdk.mjs"

func tsSDKPath(t *testing.T) string {
	t.Helper()
	p := os.Getenv("GELATI_TS_SDK")
	if p == "" {
		p = defaultTSSDKPath
	}
	if _, err := os.Stat(p); err != nil {
		t.Skipf("TS SDK not available at %s", p)
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed")
	}
	return p
}

type tsCall struct {
	Fn      string         `json:"fn"`
	ID      string         `json:"id,omitempty"`
	AgentID string         `json:"agentId,omitempty"`
	Opts    map[string]any `json:"opts,omitempty"`
	// Store passes the in-memory store built from tsInput.Appends as
	// opts.sessionStore.
	Store bool `json:"store,omitempty"`
	// Key, Entries and MTime are the foldSessionSummary arguments.
	Key     *SessionKey         `json:"key,omitempty"`
	Entries []SessionStoreEntry `json:"entries,omitempty"`
	MTime   int64               `json:"mtime,omitempty"`
}

type tsAppend struct {
	Key     SessionKey          `json:"key"`
	Entries []SessionStoreEntry `json:"entries"`
}

const tsRunner = `
import * as sdk from %q;
let s = "";
for await (const c of process.stdin) s += c;
const input = JSON.parse(s);
const store = new sdk.InMemorySessionStore();
const tsKey = (k) => ({ projectKey: k.project_key, sessionId: k.session_id, ...(k.subpath ? { subpath: k.subpath } : {}) });
for (const a of input.appends ?? []) await store.append(tsKey(a.key), a.entries);
const out = [];
for (const c of input.calls) {
  const opts = c.store ? { ...c.opts, sessionStore: store } : c.opts;
  let r;
  switch (c.fn) {
    case "listSessions": r = await sdk.listSessions(opts); break;
    case "getSessionInfo": r = await sdk.getSessionInfo(c.id, opts); break;
    case "getSessionMessages": r = await sdk.getSessionMessages(c.id, opts); break;
    case "getSubagentMessages": r = await sdk.getSubagentMessages(c.id, c.agentId, opts); break;
    case "listSubagents": r = await sdk.listSubagents(c.id, opts); break;
    case "fold": r = sdk.foldSessionSummary(undefined, tsKey(c.key), c.entries, { mtime: c.mtime }); break;
  }
  out.push(r === undefined ? null : r);
}
process.stdout.write(JSON.stringify(out));
`

// runTS runs calls against the TS SDK with CLAUDE_CONFIG_DIR=configDir and
// returns each result decoded as generic JSON. appends seed the in-memory
// store used by calls with Store set.
func runTS(t *testing.T, sdk, configDir string, calls []tsCall, appends []tsAppend, env ...string) []any {
	t.Helper()
	in, err := json.Marshal(map[string]any{"calls": calls, "appends": appends})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("node", "--input-type=module", "-e", fmt.Sprintf(tsRunner, sdk))
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + configDir, "CLAUDE_CONFIG_DIR=" + configDir}, env...)
	cmd.Stdin = bytes.NewReader(in)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, stderr.String())
	}
	var res []any
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("node output: %v\n%s", err, out)
	}
	return res
}

// tsShape round-trips v through JSON into generic values.
func tsShape(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// localSessionsSkipping is newLocalSessions(root) with the pre-compact skip
// set explicitly instead of from the process environment.
func localSessionsSkipping(root string, skipPrecompact bool) localSessions {
	s := newLocalSessions(root)
	s.skipPrecompact = skipPrecompact
	return s
}

// tsSessionInfo renders a SessionInfo as the TS SDKSessionInfo.
func tsSessionInfo(i *SessionInfo) map[string]any {
	if i == nil {
		return nil
	}
	m := map[string]any{"sessionId": i.SessionID, "summary": i.Summary, "lastModified": i.LastModified}
	if i.FileSize != 0 {
		m["fileSize"] = i.FileSize
	}
	for k, v := range map[string]string{"customTitle": i.CustomTitle, "firstPrompt": i.FirstPrompt, "gitBranch": i.GitBranch, "cwd": i.Cwd, "tag": i.Tag} {
		if v != "" {
			m[k] = v
		}
	}
	if i.CreatedAt != 0 {
		m["createdAt"] = i.CreatedAt
	}
	return m
}

func tsSessionInfos(infos []SessionInfo) []any {
	out := []any{}
	for i := range infos {
		out = append(out, tsSessionInfo(&infos[i]))
	}
	return out
}

// tsSessionMessages renders SessionMessages as the TS SessionMessage
// objects, including the runtime-only fields.
func tsSessionMessages(msgs []SessionMessage) []any {
	out := []any{}
	for _, m := range msgs {
		o := map[string]any{"type": m.Type, "uuid": m.UUID, "session_id": m.SessionID, "parent_tool_use_id": nil, "parent_agent_id": nil}
		if m.Message != nil {
			o["message"] = m.Message
		}
		if m.ParentToolUseID != "" {
			o["parent_tool_use_id"] = m.ParentToolUseID
		}
		if m.ParentAgentID != "" {
			o["parent_agent_id"] = m.ParentAgentID
		}
		if m.Timestamp != "" {
			o["timestamp"] = m.Timestamp
		}
		for k, v := range map[string]bool{"is_meta": m.IsMeta, "isCompactSummary": m.IsCompactSummary, "isQueuedCommand": m.IsQueuedCommand,
			"isCompletedLocalCommand": m.IsCompletedLocalCommand, "interruptedByShutdown": m.InterruptedByShutdown} {
			if v {
				o[k] = true
			}
		}
		if m.ToolDenialUnanswered != "" {
			o["toolDenialUnanswered"] = m.ToolDenialUnanswered
		}
		if m.Origin != nil {
			o["origin"] = m.Origin
		}
		out = append(out, o)
	}
	return out
}

// cliLine renders a JSON object with ordered keys and unescaped HTML, like
// the CLI's transcript writer.
func cliLine(kv ...any) string {
	var sb strings.Builder
	sb.WriteByte('{')
	for i := 0; i < len(kv); i += 2 {
		if i > 0 {
			sb.WriteByte(',')
		}
		for j, v := range []any{kv[i], kv[i+1]} {
			var buf bytes.Buffer
			enc := json.NewEncoder(&buf)
			enc.SetEscapeHTML(false)
			if err := enc.Encode(v); err != nil {
				panic(err)
			}
			sb.Write(bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
			if j == 0 {
				sb.WriteByte(':')
			}
		}
	}
	sb.WriteByte('}')
	return sb.String()
}

// transcriptBuilder writes CLI-shaped transcript lines.
type transcriptBuilder struct {
	t     *testing.T
	sid   string
	cwd   string
	lines []string
	clock time.Time
	n     int
}

func newTranscript(t *testing.T, cwd string) *transcriptBuilder {
	return &transcriptBuilder{t: t, sid: randomUUID(), cwd: cwd, clock: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (b *transcriptBuilder) ts() string {
	b.n++
	return b.clock.Add(time.Duration(b.n) * time.Second).Format("2006-01-02T15:04:05.000Z")
}

func (b *transcriptBuilder) raw(line string) { b.lines = append(b.lines, line) }

// entry appends a chain entry and returns its uuid. parent "" is null.
func (b *transcriptBuilder) entry(typ, parent string, extra ...any) string {
	uid := randomUUID()
	var p any
	if parent != "" {
		p = parent
	}
	kv := []any{"parentUuid", p, "isSidechain", false, "userType", "external", "cwd", b.cwd, "sessionId", b.sid,
		"version", "2.1.0", "gitBranch", "main", "type", typ}
	kv = append(kv, extra...)
	kv = append(kv, "uuid", uid, "timestamp", b.ts())
	b.raw(cliLine(kv...))
	return uid
}

func (b *transcriptBuilder) user(parent string, content any, extra ...any) string {
	return b.entry("user", parent, append([]any{"message", map[string]any{"role": "user", "content": content}}, extra...)...)
}

func (b *transcriptBuilder) assistant(parent string, content any, extra ...any) string {
	return b.assistantID(parent, "msg_"+randomUUID()[:8], content, extra...)
}

func (b *transcriptBuilder) assistantID(parent, msgID string, content any, extra ...any) string {
	if s, ok := content.(string); ok {
		content = []any{map[string]any{"type": "text", "text": s}}
	}
	return b.entry("assistant", parent, append([]any{"message", map[string]any{
		"id": msgID, "type": "message", "role": "assistant", "model": "claude", "content": content, "stop_reason": "end_turn",
	}}, extra...)...)
}

func (b *transcriptBuilder) attachment(parent string, att map[string]any) string {
	return b.entry("attachment", parent, "attachment", att)
}

func (b *transcriptBuilder) write(projectDir string) string {
	b.t.Helper()
	path := filepath.Join(projectDir, b.sid+".jsonl")
	writeJSONL(b.t, path, b.lines...)
	return path
}

func toolUse(id string) map[string]any {
	return map[string]any{"type": "tool_use", "id": id, "name": "Bash", "input": map[string]any{"command": "ls"}}
}

func toolResult(id string) []any {
	return []any{map[string]any{"type": "tool_result", "tool_use_id": id, "content": "ok"}}
}

func queued(prompt any, extra ...any) map[string]any {
	m := map[string]any{"type": "queued_command", "prompt": prompt}
	for i := 0; i < len(extra); i += 2 {
		m[extra[i].(string)] = extra[i+1]
	}
	return m
}

func TestSessionParityWithTypeScript(t *testing.T) {
	t.Parallel()
	sdk := tsSDKPath(t)
	configDir := t.TempDir()
	root := filepath.Join(configDir, "projects")
	work := canonicalizePath(t.TempDir())
	project := filepath.Join(work, "my-app")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	projectDir := filepath.Join(root, sanitizePath(project))
	var sessions []string
	var builders []*transcriptBuilder
	add := func(b *transcriptBuilder, dir string) string {
		b.write(dir)
		sessions = append(sessions, b.sid)
		builders = append(builders, b)
		return b.sid
	}

	// Plain conversation with titles, tag, relocation and an HTML-ish prompt.
	basic := newTranscript(t, project)
	u := basic.user("", "Fix the <b>bug</b> in main.go")
	a := basic.assistant(u, "Looking")
	u = basic.user(a, "thanks")
	basic.assistant(u, "done")
	basic.raw(cliLine("type", "ai-title", "aiTitle", "AI named", "sessionId", basic.sid))
	basic.raw(cliLine("type", "custom-title", "customTitle", "User named", "sessionId", basic.sid))
	basic.raw(cliLine("type", "tag", "tag", "wip", "sessionId", basic.sid))
	basic.raw(cliLine("type", "relocated", "relocatedCwd", filepath.Join(work, "moved"), "sessionId", basic.sid))
	add(basic, projectDir)

	// Compaction preserving a segment.
	seg := newTranscript(t, project)
	u1 := seg.user("", "first question")
	a1 := seg.assistant(u1, "first answer")
	u2 := seg.user(a1, "second question")
	a2 := seg.assistant(u2, "second answer")
	b := randomUUID()
	s := randomUUID()
	seg.raw(cliLine("parentUuid", nil, "logicalParentUuid", a2, "isSidechain", false, "sessionId", seg.sid, "type", "system",
		"subtype", "compact_boundary", "content", "Conversation compacted", "isMeta", false, "level", "info",
		"compactMetadata", map[string]any{"trigger": "manual", "preTokens": 1000,
			"preservedSegment": map[string]any{"headUuid": u2, "anchorUuid": s, "tailUuid": a2}},
		"uuid", b, "timestamp", seg.ts()))
	seg.raw(cliLine("parentUuid", b, "isSidechain", false, "sessionId", seg.sid, "type", "user",
		"message", map[string]any{"role": "user", "content": "Summary of the conversation"}, "isCompactSummary", true,
		"isVisibleInTranscriptOnly", true, "uuid", s, "timestamp", seg.ts()))
	u3 := seg.user(s, "after compaction")
	seg.assistant(u3, "post answer")
	seg.entry("system", u3, "subtype", "informational", "content", "note", "level", "info")
	add(seg, projectDir)

	// Compaction preserving messages.
	pm := newTranscript(t, project)
	p1 := pm.user("", "keep me?")
	p2 := pm.assistant(p1, "no")
	p3 := pm.user(p2, "keep this")
	p4 := pm.assistant(p3, "kept")
	b2, s2 := randomUUID(), randomUUID()
	pm.raw(cliLine("parentUuid", nil, "logicalParentUuid", p4, "sessionId", pm.sid, "type", "system", "subtype", "compact_boundary",
		"content", "Conversation compacted", "compactMetadata", map[string]any{"trigger": "auto",
			"preservedMessages": map[string]any{"anchorUuid": s2, "uuids": []any{p3, p4}}},
		"uuid", b2, "timestamp", pm.ts()))
	pm.raw(cliLine("parentUuid", b2, "sessionId", pm.sid, "type", "user", "message", map[string]any{"role": "user", "content": "summary"},
		"isCompactSummary", true, "uuid", s2, "timestamp", pm.ts()))
	p5 := pm.user(s2, "continue")
	pm.assistant(p5, "continuing")
	add(pm, projectDir)

	// Queued commands: reached, trailing, meta with and without a kept
	// origin, forwarded intents and task notifications.
	q := newTranscript(t, project)
	qu := q.user("", "run the tests")
	qa := q.assistant(qu, []any{toolUse("toolu_1")})
	qq := q.attachment(qa, queued("also lint please"))
	qr := q.user(qq, toolResult("toolu_1"), "sourceToolAssistantUUID", qa)
	qa2 := q.assistant(qr, "all green")
	q.attachment(qa2, queued("trailing one"))
	q.attachment(qa2, queued("from slack", "isMeta", true, "origin", map[string]any{"kind": "channel", "server": "slack"}))
	q.attachment(qa2, queued("hidden meta", "isMeta", true))
	q.attachment(qa2, queued("forwarded", "forwardedIntent", map[string]any{"lineage": "abc"}))
	q.attachment(qa2, queued([]any{map[string]any{"type": "text", "text": "task done"}}, "commandMode", "task-notification"))
	q.entry("system", qa2, "subtype", "stop_hook_summary", "content", "hooks ran")
	add(q, projectDir)

	// Mid-turn absorbed commands, by source uuid and by delivery id.
	ab := newTranscript(t, project)
	au := ab.user("", "start")
	src := randomUUID()
	aq1 := ab.attachment(au, queued("absorbed", "source_uuid", src))
	aa := ab.assistant(aq1, "working")
	aq2 := ab.attachment(aa, queued("absorbed", "source_uuid", src))
	ab.raw(cliLine("type", "queue-operation", "operation", "remove", "reason", "absorbed_mid_turn", "commandUuid", src, "sessionId", ab.sid))
	ad1 := ab.attachment(aq2, queued("delivered", "delivery_id", "d1"))
	ad2 := ab.attachment(ad1, queued("delivered", "delivery_id", "d1"))
	ab.raw(cliLine("type", "queue-operation", "operation", "remove", "reason", "absorbed_mid_turn", "deliveryId", "d1", "sessionId", ab.sid))
	ab.assistant(ad2, "done")
	add(ab, projectDir)

	// A streamed assistant message split across entries, one part on a
	// side branch with its tool result.
	sp := newTranscript(t, project)
	su := sp.user("", "do two things")
	sa1 := sp.assistantID(su, "msg_split", "plan")
	sa2 := sp.assistantID(sa1, "msg_split", []any{toolUse("toolu_a")})
	sa3 := sp.assistantID(sa1, "msg_split", []any{toolUse("toolu_b")})
	sr2 := sp.user(sa3, toolResult("toolu_b"))
	sr1 := sp.user(sa2, toolResult("toolu_a"))
	_ = sr2
	sp.assistant(sr1, "both done")
	add(sp, projectDir)

	// Local slash command, meta from a peer, a bash-mode prompt.
	lc := newTranscript(t, project)
	l1 := lc.user("", "<local-command-caveat>Caveat: generated by local commands.</local-command-caveat>", "isMeta", true)
	l2 := lc.user(l1, "<command-name>/model</command-name>\n<command-message>model</command-message>")
	l3 := lc.user(l2, "<local-command-stdout>Set model to opus</local-command-stdout>")
	l4 := lc.user(l3, "<bash-input>ls -la</bash-input>")
	l5 := lc.user(l4, "peer says hi", "isMeta", true, "origin", map[string]any{"kind": "peer", "from": "other"})
	l6 := lc.user(l5, "hidden", "isMeta", true, "origin", map[string]any{"kind": "task-notification", "subkind": "x", "secret": 1})
	l7 := lc.assistant(l6, "ok", "interruptedByShutdown", true)
	lc.user(l7, "[Request interrupted by user]", "toolDenialUnanswered", "stream-closed")
	add(lc, projectDir)

	// Listing-only variations.
	img := newTranscript(t, project)
	img.user("", []any{map[string]any{"type": "image", "source": map[string]any{"type": "base64", "data": "AAAA"}}})
	add(img, projectDir)

	pasted := newTranscript(t, project)
	pasted.user("", "Look at this:\n\n<pasted_content id=\"ab12\">\npasted body\n</pasted_content id=\"ab12\">\n\nplease")
	add(pasted, projectDir)

	sdkTS := newTranscript(t, project)
	sdkTS.raw(cliLine("type", "permission-mode", "permissionMode", "default", "sessionId", sdkTS.sid))
	sdkTS.entry("user", "", "message", map[string]any{"role": "user", "content": "from the ts sdk"}, "entrypoint", "sdk-ts")
	add(sdkTS, projectDir)

	daemon := newTranscript(t, project)
	daemon.entry("user", "", "message", map[string]any{"role": "user", "content": "daemon job"}, "sessionKind", "daemon")
	add(daemon, projectDir)

	side := newTranscript(t, project)
	side.entry("user", "", "message", map[string]any{"role": "user", "content": "sidechain"})
	side.lines[0] = strings.Replace(side.lines[0], `"isSidechain":false`, `"isSidechain":true`, 1)
	add(side, projectDir)

	// A session continued in another one is hidden.
	next := newTranscript(t, project)
	next.user("", "continued here")
	add(next, projectDir)
	cont := newTranscript(t, project)
	cu := cont.user("", "will continue elsewhere")
	cont.assistant(cu, "ok")
	cont.raw(cliLine("type", "continued-in", "continuedInSessionId", next.sid, "sessionId", cont.sid))
	add(cont, projectDir)
	// ...unless the target has no chain entries.
	dangling := newTranscript(t, project)
	du := dangling.user("", "dangling continuation")
	dangling.assistant(du, "ok")
	ghost := randomUUID()
	writeJSONL(t, filepath.Join(projectDir, ghost+".jsonl"), cliLine("type", "summary", "summary", "no chain"))
	dangling.raw(cliLine("type", "continued-in", "continuedInSessionId", ghost, "sessionId", dangling.sid))
	add(dangling, projectDir)

	// Title from the custom-title.json sidecar.
	sc := newTranscript(t, project)
	sc.user("", "sidecar session")
	add(sc, projectDir)
	writeFile(t, filepath.Join(projectDir, sc.sid, "custom-title.json"), `{"customTitle":"  Side\u0007car\ttitle  "}`)

	// Recorded in a different directory with the same sanitized name.
	other := filepath.Join(work, "my", "app")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	foreign := newTranscript(t, other)
	foreign.user("", "foreign session")
	add(foreign, projectDir)

	// Subagent transcript with metadata.
	agentDir := filepath.Join(projectDir, q.sid, "subagents")
	sub := newTranscript(t, project)
	sub.sid = q.sid
	g1 := sub.user("", "subagent task", "isMeta", true, "isSidechain", true, "agentId", "abc")
	g2 := sub.assistant(g1, []any{toolUse("toolu_s")})
	g3 := sub.attachment(g2, queued("sub queued"))
	g4 := sub.user(g3, toolResult("toolu_s"))
	sub.entry("system", g4, "subtype", "x", "content", "breaks the chain")
	sub.assistant(g4, "sub done")
	writeJSONL(t, filepath.Join(agentDir, "agent-abc.jsonl"), sub.lines...)
	writeFile(t, filepath.Join(agentDir, "agent-abc.meta.json"), `{"agentType":"general","toolUseId":"toolu_parent","parentAgentId":"bad id!"}`)

	// A second project, for the all-projects listing.
	otherProject := filepath.Join(work, "other-project")
	op := newTranscript(t, otherProject)
	op.user("", "other project")
	add(op, filepath.Join(root, sanitizePath(otherProject)))

	var calls []tsCall
	type goCall func() any
	var goCalls []goCall
	call := func(c tsCall, g goCall) {
		calls = append(calls, c)
		goCalls = append(goCalls, g)
	}
	list := func(opts map[string]any, o *ListSessionsOptions) {
		call(tsCall{Fn: "listSessions", Opts: opts}, func() any { return tsSessionInfos(newLocalSessions(root).listSessions(o)) })
	}
	list(map[string]any{"dir": project}, &ListSessionsOptions{Directory: project})
	list(map[string]any{"dir": project, "includeProgrammatic": false}, &ListSessionsOptions{Directory: project, ExcludeProgrammatic: true})
	list(map[string]any{}, &ListSessionsOptions{})
	list(map[string]any{"dir": project, "limit": 3, "offset": 2}, &ListSessionsOptions{Directory: project, Limit: 3, Offset: 2})
	for _, sid := range sessions {
		call(tsCall{Fn: "getSessionInfo", ID: sid, Opts: map[string]any{"dir": project}},
			func() any { return tsSessionInfo(newLocalSessions(root).getSessionInfo(sid, project)) })
		call(tsCall{Fn: "getSessionInfo", ID: sid},
			func() any { return tsSessionInfo(newLocalSessions(root).getSessionInfo(sid, "")) })
		for _, sys := range []bool{false, true} {
			call(tsCall{Fn: "getSessionMessages", ID: sid, Opts: map[string]any{"dir": project, "includeSystemMessages": sys}},
				func() any {
					return tsSessionMessages(localSessionsSkipping(root, true).getSessionMessages(sid, &SessionMessagesOptions{Directory: project, IncludeSystemMessages: sys}))
				})
		}
	}
	call(tsCall{Fn: "getSessionMessages", ID: q.sid, Opts: map[string]any{"limit": 2, "offset": 1}},
		func() any {
			return tsSessionMessages(localSessionsSkipping(root, true).getSessionMessages(q.sid, &SessionMessagesOptions{Limit: 2, Offset: 1}))
		})
	call(tsCall{Fn: "getSubagentMessages", ID: q.sid, AgentID: "abc", Opts: map[string]any{"dir": project}},
		func() any {
			return tsSessionMessages(newLocalSessions(root).getSubagentMessages(q.sid, "abc", &SessionMessagesOptions{Directory: project}))
		})

	// The same transcripts through a SessionStore (the CLI's mirror shape).
	ctx := t.Context()
	store := NewInMemorySessionStore()
	projectKey := ProjectKeyForDirectory(project)
	var appends []tsAppend
	appendLines := func(key SessionKey, lines []string) []SessionStoreEntry {
		var entries []SessionStoreEntry
		for _, l := range lines {
			var e SessionStoreEntry
			if err := json.Unmarshal([]byte(l), &e); err != nil {
				t.Fatal(err)
			}
			entries = append(entries, e)
		}
		if err := store.Append(ctx, key, entries); err != nil {
			t.Fatal(err)
		}
		appends = append(appends, tsAppend{Key: key, Entries: entries})
		return entries
	}
	// dropLastModified removes lastModified, which comes from the store's
	// clock (or the current time) and differs between the runs.
	dropLastModified := func(v any) any {
		switch x := v.(type) {
		case map[string]any:
			delete(x, "lastModified")
		case []any:
			for _, e := range x {
				if m, ok := e.(map[string]any); ok {
					delete(m, "lastModified")
				}
			}
		}
		return v
	}
	storeOpts := func(sys bool) map[string]any { return map[string]any{"dir": project, "includeSystemMessages": sys} }
	for _, b := range builders {
		key := SessionKey{ProjectKey: projectKey, SessionID: b.sid}
		entries := appendLines(key, b.lines)
		sid := b.sid
		for _, sys := range []bool{false, true} {
			call(tsCall{Fn: "getSessionMessages", ID: sid, Opts: storeOpts(sys), Store: true}, func() any {
				msgs, err := GetSessionMessagesFromStore(ctx, store, sid, &SessionMessagesOptions{Directory: project, IncludeSystemMessages: sys})
				if err != nil {
					t.Fatal(err)
				}
				return tsSessionMessages(msgs)
			})
		}
		call(tsCall{Fn: "fold", Key: &key, Entries: entries, MTime: 1234}, func() any {
			f := FoldSessionSummaryWithOptions(nil, key, entries, &FoldSessionOptions{MTime: 1234})
			return map[string]any{"sessionId": f.SessionID, "mtime": f.MTime, "data": f.Data}
		})
	}
	appendLines(SessionKey{ProjectKey: projectKey, SessionID: q.sid, Subpath: "subagents/agent-abc"}, append(slices.Clone(sub.lines),
		cliLine("type", "agent_metadata", "agentType", "general", "toolUseId", "toolu_parent", "parentAgentId", "nested-1")))
	call(tsCall{Fn: "getSubagentMessages", ID: q.sid, AgentID: "abc", Opts: map[string]any{"dir": project}, Store: true}, func() any {
		msgs, err := GetSubagentMessagesFromStore(ctx, store, q.sid, "abc", &SessionMessagesOptions{Directory: project})
		if err != nil {
			t.Fatal(err)
		}
		return tsSessionMessages(msgs)
	})
	nLocal := len(calls)
	for _, sid := range sessions {
		call(tsCall{Fn: "getSessionInfo", ID: sid, Opts: map[string]any{"dir": project}, Store: true}, func() any {
			info, err := GetSessionInfoFromStore(ctx, store, sid, project)
			if err != nil {
				t.Fatal(err)
			}
			return tsSessionInfo(info)
		})
	}
	call(tsCall{Fn: "listSessions", Opts: map[string]any{"dir": project}, Store: true}, func() any {
		infos, err := ListSessionsFromStore(ctx, store, &ListSessionsOptions{Directory: project})
		if err != nil {
			t.Fatal(err)
		}
		return tsSessionInfos(infos)
	})

	want := runTS(t, sdk, configDir, calls, appends)
	for i, c := range calls {
		got := tsShape(t, goCalls[i]())
		w := want[i]
		if i >= nLocal {
			got, w = dropLastModified(got), dropLastModified(w)
		}
		if l, ok := w.([]any); ok && len(l) == 0 {
			w = []any{}
		}
		if !reflect.DeepEqual(got, w) {
			gj, _ := json.MarshalIndent(got, "", " ")
			wj, _ := json.MarshalIndent(w, "", " ")
			t.Errorf("%s %s %v:\nGo: %s\nTS: %s", c.Fn, c.ID, c.Opts, gj, wj)
		}
	}
}

// TestSessionParityLargeTranscript checks the >5 MiB pre-compact skip (and
// its opt-out) against the TS SDK.
func TestSessionParityLargeTranscript(t *testing.T) {
	t.Parallel()
	sdk := tsSDKPath(t)
	configDir := t.TempDir()
	root := filepath.Join(configDir, "projects")
	project := canonicalizePath(t.TempDir())
	projectDir := filepath.Join(root, sanitizePath(project))

	b := newTranscript(t, project)
	big := strings.Repeat("x", precompactSkipThreshold)
	u1 := b.user("", "huge "+big)
	a1 := b.assistant(u1, "ok")
	b.raw(cliLine("type", "attribution-snapshot", "messageId", "m1", "sessionId", b.sid))
	// A boundary that preserved a segment does not cut the transcript.
	pb := randomUUID()
	b.raw(cliLine("parentUuid", nil, "logicalParentUuid", a1, "sessionId", b.sid, "type", "system", "subtype", "compact_boundary",
		"compactMetadata", map[string]any{"preservedSegment": map[string]any{"headUuid": u1, "anchorUuid": "none", "tailUuid": a1}},
		"uuid", pb, "timestamp", b.ts()))
	u2 := b.user(a1, "before the real boundary")
	a2 := b.assistant(u2, "answer")
	bd := randomUUID()
	b.raw(cliLine("parentUuid", nil, "logicalParentUuid", a2, "sessionId", b.sid, "type", "system", "subtype", "compact_boundary",
		"content", "Conversation compacted", "compactMetadata", map[string]any{"trigger": "auto"}, "uuid", bd, "timestamp", b.ts()))
	sm := randomUUID()
	b.raw(cliLine("parentUuid", bd, "sessionId", b.sid, "type", "user", "message", map[string]any{"role": "user", "content": "summary"},
		"isCompactSummary", true, "uuid", sm, "timestamp", b.ts()))
	b.raw(cliLine("type", "attribution-snapshot", "messageId", "m2", "sessionId", b.sid))
	// Continuing from a pre-boundary message shows the cut: with the skip
	// the chain stops at the missing parent.
	u3 := b.user(a2, "after")
	b.assistant(u3, "done")
	b.write(projectDir)

	opts := &SessionMessagesOptions{Directory: project, IncludeSystemMessages: true}
	tsOpts := map[string]any{"dir": project, "includeSystemMessages": true}
	calls := []tsCall{{Fn: "getSessionMessages", ID: b.sid, Opts: tsOpts}}
	for _, tt := range []struct {
		name string
		skip bool
		env  []string
	}{
		{"skip", true, nil},
		{"opt-out", false, []string{"CLAUDE_CODE_DISABLE_PRECOMPACT_SKIP=1"}},
	} {
		got := tsShape(t, tsSessionMessages(localSessionsSkipping(root, tt.skip).getSessionMessages(b.sid, opts)))
		want := runTS(t, sdk, configDir, calls, nil, tt.env...)[0]
		if !reflect.DeepEqual(got, want) {
			gj, _ := json.Marshal(got)
			wj, _ := json.Marshal(want)
			t.Errorf("%s:\nGo: %.2000s\nTS: %.2000s", tt.name, gj, wj)
		}
		if n := len(got.([]any)); (tt.skip && n != 2) || (!tt.skip && n != 6) {
			t.Errorf("%s: %d messages", tt.name, n)
		}
	}
}
