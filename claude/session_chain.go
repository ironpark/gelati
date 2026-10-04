package claude

import (
	"cmp"
	"maps"
	"math"
	"slices"
	"strings"
)

// Conversation reconstruction for GetSessionMessages and friends, ported
// from the TypeScript SDK's transcript reader (v0.3.286). It covers what the
// Python port lacked: compact-boundary relinking of preserved segments,
// recovery of split assistant messages and their tool results, queued
// commands delivered as attachments, completed local commands and the
// absorbed_mid_turn bookkeeping.
//
// The TS code keeps entries in a JS Map keyed by uuid; its iteration order
// (first insertion, even when a later duplicate replaces the value) is
// significant for leaf selection and is reproduced by entryMap.

// interruptMarkers are user-message prefixes that count as a reply to a
// queued command (an interrupted or skipped turn still consumed it).
var interruptMarkers = [...]string{
	"[Request interrupted by user]",
	"[Request interrupted by user for tool use]",
	"[Tool call did not complete: the turn was ended to deliver the message that follows. Nothing refused it; re-run it if still needed.]",
	"[Tool call interrupted: the session ended before this call's result was recorded, so its outcome is unknown. Check whether it took effect before relying on it or running it again.]",
	"[Tool call result not in this copy: this session was copied from another session before that session recorded this call's result. The call may have finished there, may still be running there, or may never have run. Check whether it took effect before relying on it or running it again.]",
	"The user doesn't want to take this action right now. STOP what you are doing and wait for the user to tell you how to proceed.",
	"[Tool call skipped: the turn was stopped before this call ran, by the check whose denial is on another call in this batch. Nothing refused this call and it had no effects; re-run it if still needed.]",
	"[Tool call skipped: the turn ended to deliver the message that follows before this call ran. Nothing refused it; re-run it if still needed.]",
}

// transcriptEntryTypes are the entry types that carry uuid + parentUuid
// chain links.
var transcriptEntryTypes = map[string]bool{
	"user": true, "assistant": true, "progress": true, "system": true, "attachment": true,
}

// isTranscriptEntry reports whether entry is a chain-linked transcript
// message: a transcript entry type with a string uuid.
func isTranscriptEntry(entry map[string]any) bool {
	t, _ := entry["type"].(string)
	_, ok := entry["uuid"].(string)
	return ok && transcriptEntryTypes[t]
}

// entryUUID returns the entry's uuid; chain entries always have one.
func entryUUID(e map[string]any) string { return str(e["uuid"]) }

// entryParent returns the parentUuid when it is a non-empty string.
func entryParent(e map[string]any) string { return str(e["parentUuid"]) }

func isUserOrAssistant(e map[string]any) bool {
	t := e["type"]
	return t == "user" || t == "assistant"
}

// withField returns a shallow copy of e with key set to v, or removed when
// v is absent.
func withField(e map[string]any, key string, v any, ok bool) map[string]any {
	c := maps.Clone(e)
	if ok {
		c[key] = v
	} else {
		delete(c, key)
	}
	return c
}

// messageContentBlocks returns the content array of e.message, or nil.
func messageContentBlocks(e map[string]any) []any {
	msg, ok := e["message"].(map[string]any)
	if !ok {
		return nil
	}
	blocks, _ := msg["content"].([]any)
	return blocks
}

// blockStrings returns field of every content block of type blockType
// whose value is a string (TS tu()).
func blockStrings(e map[string]any, blockType, field string) []string {
	var out []string
	for _, b := range messageContentBlocks(e) {
		block, ok := b.(map[string]any)
		if !ok || block["type"] != blockType {
			continue
		}
		if s, ok := block[field].(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// assistantMessageID returns message.id of an assistant entry (TS jf()).
func assistantMessageID(e map[string]any) string {
	if e["type"] != "assistant" {
		return ""
	}
	msg, ok := e["message"].(map[string]any)
	if !ok {
		return ""
	}
	return str(msg["id"])
}

// isToolResultUser reports whether e is a user message carrying a
// tool_result block (TS LM()).
func isToolResultUser(e map[string]any) bool {
	if e["type"] != "user" || entryParent(e) == "" {
		return false
	}
	for _, b := range messageContentBlocks(e) {
		if block, ok := b.(map[string]any); ok && block["type"] == "tool_result" {
			return true
		}
	}
	return false
}

// isInterruptMessage reports whether e is a user message made only of
// interrupt markers (TS qP()).
func isInterruptMessage(e map[string]any) bool {
	if e["type"] != "user" {
		return false
	}
	hasMarker := func(s string) bool {
		for _, m := range interruptMarkers {
			if strings.HasPrefix(s, m) {
				return true
			}
		}
		return false
	}
	msg, _ := e["message"].(map[string]any)
	switch content := msg["content"].(type) {
	case string:
		return hasMarker(content)
	case []any:
		if len(content) == 0 {
			return false
		}
		for _, b := range content {
			block, _ := b.(map[string]any)
			var text any
			switch {
			case block["type"] == "text":
				text = block["text"]
			case block["type"] == "tool_result" && block["is_error"] == true:
				text = block["content"]
			}
			s, ok := text.(string)
			if !ok || !hasMarker(s) {
				return false
			}
		}
		return true
	}
	return false
}

// keepsMetaOrigin reports whether a meta message from origin is still shown
// (TS dC(): channel, observer, observer-activity, slack-ping and peer
// messages).
func keepsMetaOrigin(origin any) bool {
	o, ok := origin.(map[string]any)
	if !ok {
		return false
	}
	switch o["kind"] {
	case "channel", "observer", "observer-activity", "slack-ping", "peer":
		return true
	}
	return false
}

// normalizeOrigin trims a task-notification origin to its public fields
// and returns any other origin unchanged (TS dS()).
func normalizeOrigin(origin any) any {
	o, ok := origin.(map[string]any)
	if !ok || o["kind"] != "task-notification" {
		return origin
	}
	out := map[string]any{"kind": "task-notification"}
	for _, k := range [...]string{"subkind", "fireReason", "producer"} {
		if v, ok := o[k]; ok {
			out[k] = v
		}
	}
	return out
}

// queuedCommandAttachment returns the attachment of a queued_command
// attachment entry; ok is false for any other entry.
func queuedCommandAttachment(e map[string]any) (att map[string]any, ok bool) {
	if e["type"] != "attachment" {
		return nil, false
	}
	att, ok = e["attachment"].(map[string]any)
	if !ok || att["type"] != "queued_command" {
		return nil, false
	}
	return att, true
}

// queuedCommandField returns a non-empty string field of a queued_command
// attachment entry (TS qL()).
func queuedCommandField(e map[string]any, field string) string {
	att, ok := queuedCommandAttachment(e)
	if !ok {
		return ""
	}
	return str(att[field])
}

// ---------------------------------------------------------------------------
// absorbed_mid_turn bookkeeping (TS kM/IM/DM)
// ---------------------------------------------------------------------------

// deliveryTracker collects, over every parsed transcript line, which queued
// commands were absorbed mid-turn: such a command is delivered both as an
// attachment and inside the running turn, and only the absorbed copies are
// rendered.
type deliveryTracker struct {
	counts            map[string]int
	copies            map[string]string
	mixed             map[string]bool
	mixedOrder        []string
	deliveryCopies    map[string][]string
	deliveryKeys      []string
	deliveryCopyUUIDs map[string]bool
}

func newDeliveryTracker() *deliveryTracker {
	return &deliveryTracker{
		counts:            map[string]int{},
		copies:            map[string]string{},
		mixed:             map[string]bool{},
		deliveryCopies:    map[string][]string{},
		deliveryCopyUUIDs: map[string]bool{},
	}
}

// observe records one parsed transcript line.
func (t *deliveryTracker) observe(e map[string]any) {
	if e["type"] == "queue-operation" && e["operation"] == "remove" && e["reason"] == "absorbed_mid_turn" {
		deliveryID, dOK := e["deliveryId"].(string)
		commandUUID, cOK := e["commandUuid"].(string)
		if dOK || cOK {
			key := ""
			switch {
			case dOK && deliveryID != "":
				key = "delivery:" + deliveryID
			case cOK:
				key = "uuid:" + commandUUID
			}
			if key != "" {
				t.counts[key]++
			}
			return
		}
	}
	_, typeOK := e["type"].(string)
	uuid, uuidOK := e["uuid"].(string)
	if !typeOK || !uuidOK {
		return
	}
	if id := queuedCommandField(e, "delivery_id"); id != "" {
		if !t.deliveryCopyUUIDs[uuid] {
			t.deliveryCopyUUIDs[uuid] = true
			key := "delivery:" + id
			if _, ok := t.deliveryCopies[key]; !ok {
				t.deliveryKeys = append(t.deliveryKeys, key)
			}
			t.deliveryCopies[key] = append(t.deliveryCopies[key], uuid)
		}
		return
	}
	src := queuedCommandField(e, "source_uuid")
	if src == "" {
		return
	}
	serialized := jsJSONStringify(e["attachment"])
	if prev, ok := t.copies[src]; !ok {
		t.copies[src] = serialized
	} else if prev != serialized && !t.mixed[src] {
		t.mixed[src] = true
		t.mixedOrder = append(t.mixedOrder, src)
	}
}

// delivered finalizes the bookkeeping: "uuid:<source>" keys count absorbed
// copies, "entry:<uuid>" keys mark absorbed delivery copies.
func (t *deliveryTracker) delivered() map[string]int {
	for _, src := range t.mixedOrder {
		delete(t.counts, "uuid:"+src)
	}
	for _, key := range t.deliveryKeys {
		uuids := t.deliveryCopies[key]
		n := t.counts[key]
		for _, u := range uuids[max(0, len(uuids)-n):] {
			t.counts["entry:"+u] = 1
		}
	}
	return t.counts
}

// ---------------------------------------------------------------------------
// Chain building (TS nu)
// ---------------------------------------------------------------------------

// entryMap is a uuid-keyed map that iterates in first-insertion order, like
// a JS Map.
type entryMap struct {
	order  []string
	byUUID map[string]map[string]any
}

func newEntryMap(entries []map[string]any) *entryMap {
	m := &entryMap{byUUID: make(map[string]map[string]any, len(entries))}
	for _, e := range entries {
		m.set(entryUUID(e), e)
	}
	return m
}

func (m *entryMap) set(uuid string, e map[string]any) {
	if _, ok := m.byUUID[uuid]; !ok {
		m.order = append(m.order, uuid)
	}
	m.byUUID[uuid] = e
}

func (m *entryMap) parentOf(e map[string]any) map[string]any {
	if p := entryParent(e); p != "" {
		return m.byUUID[p]
	}
	return nil
}

// relinkCompactBoundaries rewrites parentUuid links around compact
// boundaries that preserved messages (compactMetadata.preservedMessages)
// or a segment (compactMetadata.preservedSegment), so the chain walk passes
// through the preserved messages instead of stopping at the boundary.
func (m *entryMap) relinkCompactBoundaries() {
	for i := 0; i < len(m.order); i++ {
		b := m.byUUID[m.order[i]]
		if b["type"] != "system" || b["subtype"] != "compact_boundary" {
			continue
		}
		meta, _ := b["compactMetadata"].(map[string]any)
		if pm, ok := meta["preservedMessages"].(map[string]any); ok && pm != nil {
			m.relinkPreservedMessages(pm)
		} else if ps, ok := meta["preservedSegment"].(map[string]any); ok && ps != nil {
			m.relinkPreservedSegment(ps)
		}
	}
}

func (m *entryMap) relinkPreservedMessages(pm map[string]any) {
	rawUUIDs, _ := pm["uuids"].([]any)
	if len(rawUUIDs) == 0 {
		return
	}
	uuids := make([]string, len(rawUUIDs))
	for i, u := range rawUUIDs {
		s, ok := u.(string)
		if !ok || m.byUUID[s] == nil {
			return
		}
		uuids[i] = s
	}
	anchor, anchorOK := pm["anchorUuid"]
	prev, prevOK := anchor, anchorOK
	for _, u := range uuids {
		m.byUUID[u] = withField(m.byUUID[u], "parentUuid", prev, prevOK)
		prev, prevOK = u, true
	}
	first, last := uuids[0], uuids[len(uuids)-1]
	for _, u := range m.order {
		e := m.byUUID[u]
		p, pok := e["parentUuid"]
		if u != first && jsStrictEqual(p, pok, anchor, anchorOK) {
			m.byUUID[u] = withField(e, "parentUuid", last, true)
		}
	}
}

func (m *entryMap) relinkPreservedSegment(ps map[string]any) {
	head := ps["headUuid"]
	anchor, anchorOK := ps["anchorUuid"]
	tail, tailOK := ps["tailUuid"]
	headUUID, headIsString := head.(string)
	if h := m.byUUID[headUUID]; headIsString && h != nil {
		m.byUUID[headUUID] = withField(h, "parentUuid", anchor, anchorOK)
	}
	for _, u := range m.order {
		e := m.byUUID[u]
		p, pok := e["parentUuid"]
		if jsStrictEqual(p, pok, anchor, anchorOK) && !(headIsString && headUUID == u) {
			m.byUUID[u] = withField(e, "parentUuid", tail, tailOK)
		}
	}
}

// chainResult is the reconstructed conversation.
type chainResult struct {
	chain []map[string]any // root first, including recovered siblings
	leaf  map[string]any   // nil when there is no user/assistant message
	// inChain holds the uuids of chain entries.
	inChain map[string]bool
	byUUID  *entryMap
}

// buildConversationChain picks the conversation's leaf (see selectLeaf) and
// walks its (relinked) parentUuid links back to the root, returning entries
// root first. Assistant messages split across entries that fell off the
// chain, and their tool results, are spliced back in after the chain's copy
// of the message.
func buildConversationChain(entries []map[string]any) chainResult {
	if len(entries) == 0 {
		return chainResult{}
	}
	m := newEntryMap(entries)
	m.relinkCompactBoundaries()

	leaf := m.selectLeaf(entries)
	if leaf == nil {
		return chainResult{byUUID: m}
	}
	var chain []map[string]any
	inChain := map[string]bool{}
	for cur := m.byUUID[entryUUID(leaf)]; cur != nil && !inChain[entryUUID(cur)]; cur = m.parentOf(cur) {
		inChain[entryUUID(cur)] = true
		chain = append(chain, cur)
	}
	slices.Reverse(chain)
	chain = recoverSplitAssistantSiblings(m, chain, inChain)
	return chainResult{chain: chain, leaf: leaf, inChain: inChain, byUUID: m}
}

// selectLeaf picks the conversation's leaf: the user/assistant message
// reached first from the latest main-chain terminal (see
// mainTerminalLeaf), else the latest leaf of any terminal (see
// fallbackLeaf). It returns nil when no terminal reaches a user/assistant
// message. Recency is the position in entries.
func (m *entryMap) selectLeaf(entries []map[string]any) map[string]any {
	position := make(map[string]int, len(entries))
	for i, e := range entries {
		position[entryUUID(e)] = i
	}
	pos := func(e map[string]any) int {
		if p, ok := position[entryUUID(e)]; ok {
			return p
		}
		return -1
	}
	if leaf := m.mainTerminalLeaf(pos); leaf != nil {
		return leaf
	}
	return m.fallbackLeaf(pos)
}

// isMainChainEntry reports whether e belongs to the main chain: it is not a
// sidechain, team, progress or fork-briefing entry.
func isMainChainEntry(e map[string]any) bool {
	if jsTruthy(e["isSidechain"]) || jsTruthy(e["teamName"]) || e["type"] == "progress" {
		return false
	}
	if e["type"] == "attachment" {
		if att, ok := e["attachment"].(map[string]any); ok && att["type"] == "fork_briefing" {
			return false
		}
	}
	return true
}

// mainTerminalLeaf walks back from each main-chain terminal (a main-chain
// entry that no main-chain entry points at), latest first, and returns the
// first user/assistant message reached, or nil. Entries walked from an
// earlier terminal are not walked again.
func (m *entryMap) mainTerminalLeaf(pos func(map[string]any) int) map[string]any {
	mainParents := map[string]bool{}
	for _, u := range m.order {
		e := m.byUUID[u]
		if p := entryParent(e); p != "" && isMainChainEntry(e) {
			mainParents[p] = true
		}
	}
	var mainTerminals []map[string]any
	for _, u := range m.order {
		if e := m.byUUID[u]; isMainChainEntry(e) && !mainParents[u] {
			mainTerminals = append(mainTerminals, e)
		}
	}
	slices.SortStableFunc(mainTerminals, func(a, b map[string]any) int { return pos(b) - pos(a) })

	visited := map[string]bool{}
	for _, terminal := range mainTerminals {
		var walked []string
		seen := map[string]bool{}
		for cur := terminal; cur != nil && !visited[entryUUID(cur)] && !seen[entryUUID(cur)]; cur = m.parentOf(cur) {
			if isUserOrAssistant(cur) {
				return cur
			}
			seen[entryUUID(cur)] = true
			walked = append(walked, entryUUID(cur))
		}
		for _, u := range walked {
			visited[u] = true
		}
	}
	return nil
}

// fallbackLeaf returns the latest of the user/assistant messages reached
// first from each terminal (an entry nothing points at), preferring main
// (not sidechain, team or meta) messages, or nil when there is none.
func (m *entryMap) fallbackLeaf(pos func(map[string]any) int) map[string]any {
	parents := map[string]bool{}
	for _, u := range m.order {
		if p := entryParent(m.byUUID[u]); p != "" {
			parents[p] = true
		}
	}
	var leaves []map[string]any
	for _, u := range m.order {
		if parents[u] {
			continue
		}
		seen := map[string]bool{}
		for cur := m.byUUID[u]; cur != nil && !seen[entryUUID(cur)]; cur = m.parentOf(cur) {
			seen[entryUUID(cur)] = true
			if isUserOrAssistant(cur) {
				leaves = append(leaves, cur)
				break
			}
		}
	}
	if len(leaves) == 0 {
		return nil
	}
	latest := func(c []map[string]any) map[string]any {
		best := c[0]
		for _, e := range c[1:] {
			if pos(e) > pos(best) {
				best = e
			}
		}
		return best
	}
	var main []map[string]any
	for _, e := range leaves {
		if !jsTruthy(e["isSidechain"]) && !jsTruthy(e["teamName"]) && !jsTruthy(e["isMeta"]) {
			main = append(main, e)
		}
	}
	if len(main) > 0 {
		return latest(main)
	}
	return latest(leaves)
}

// ---------------------------------------------------------------------------
// Split assistant messages (TS VSe)
// ---------------------------------------------------------------------------

// cmpTimestamp orders entries by their timestamp strings.
func cmpTimestamp(a, b map[string]any) int {
	return strings.Compare(str(a["timestamp"]), str(b["timestamp"]))
}

// recoverSplitAssistantSiblings splices back the parts of a streamed
// assistant message (entries sharing message.id) that are not on the chain,
// together with the tool results answering them, right after the chain's
// copy of the message (TS VSe). inChain is extended with the added uuids.
func recoverSplitAssistantSiblings(m *entryMap, chain []map[string]any, inChain map[string]bool) []map[string]any {
	var chainAssistants []map[string]any
	for _, e := range chain {
		if e["type"] == "assistant" {
			chainAssistants = append(chainAssistants, e)
		}
	}
	if len(chainAssistants) == 0 {
		return chain
	}
	chainByMsgID := map[string]map[string]any{}
	for _, e := range chainAssistants {
		if id := assistantMessageID(e); id != "" {
			chainByMsgID[id] = e
		}
	}
	x := indexSplitSiblings(m)
	answered := map[string]bool{}
	for _, e := range chain {
		for _, id := range blockStrings(e, "tool_result", "tool_use_id") {
			answered[id] = true
		}
	}

	done := map[string]bool{}
	extras := map[string][]map[string]any{}
	added := 0
	for _, e := range chainAssistants {
		id := assistantMessageID(e)
		if id == "" || done[id] {
			continue
		}
		done[id] = true
		spliced := x.offChainSiblings(id, e, inChain, answered)
		if len(spliced) == 0 {
			continue
		}
		for _, s := range spliced {
			inChain[entryUUID(s)] = true
		}
		added += len(spliced)
		anchor := entryUUID(chainByMsgID[id])
		extras[anchor] = append(extras[anchor], spliced...)
	}
	if added == 0 {
		return chain
	}
	out := make([]map[string]any, 0, len(chain)+added)
	for _, e := range chain {
		out = append(out, e)
		out = append(out, extras[entryUUID(e)]...)
	}
	return out
}

// splitSiblingIndex indexes an entryMap's assistant parts and tool results
// for recoverSplitAssistantSiblings.
type splitSiblingIndex struct {
	m *entryMap
	// byMsgID holds the assistant entries of each message.id, in map order.
	byMsgID map[string][]map[string]any
	// toolUseOwner maps a tool_use id to its assistant entry, or nil when
	// entries of different messages claim it.
	toolUseOwner map[string]map[string]any
	// children holds the tool results linked to each assistant uuid.
	children map[string][]map[string]any
	// keyPos is the map position of each uuid, built on first use.
	keyPos map[string]int
}

// indexSplitSiblings builds the index. A tool result is linked to its
// parent, to its sourceToolAssistantUUID and to the owners of the tool uses
// it answers, the latter two only within the same sidechain scope.
func indexSplitSiblings(m *entryMap) *splitSiblingIndex {
	x := &splitSiblingIndex{
		m:            m,
		byMsgID:      map[string][]map[string]any{},
		toolUseOwner: map[string]map[string]any{},
		children:     map[string][]map[string]any{},
	}
	var toolResults []map[string]any
	for _, u := range m.order {
		e := m.byUUID[u]
		if id := assistantMessageID(e); id != "" {
			x.byMsgID[id] = append(x.byMsgID[id], e)
			for _, tu := range blockStrings(e, "tool_use", "id") {
				prev, seen := x.toolUseOwner[tu]
				if !seen || (prev != nil && assistantMessageID(prev) == id) {
					x.toolUseOwner[tu] = e
				} else {
					x.toolUseOwner[tu] = nil
				}
			}
		} else if isToolResultUser(e) {
			toolResults = append(toolResults, e)
		}
	}

	linked := map[string]bool{}
	link := func(parent string, e map[string]any) {
		k := parent + "\n" + entryUUID(e)
		if linked[k] {
			return
		}
		linked[k] = true
		x.children[parent] = append(x.children[parent], e)
	}
	for _, e := range toolResults {
		link(entryParent(e), e)
		if src, ok := e["sourceToolAssistantUUID"].(string); ok && src != entryParent(e) {
			if a := m.byUUID[src]; a != nil && sameSidechainScope(e, a) {
				link(entryUUID(a), e)
			}
		}
		for _, id := range blockStrings(e, "tool_result", "tool_use_id") {
			if a := x.toolUseOwner[id]; a != nil && sameSidechainScope(e, a) {
				link(entryUUID(a), e)
			}
		}
	}
	return x
}

// sameSidechainScope reports whether two entries share isSidechain (absent
// or null counting as false) and agentId.
func sameSidechainScope(a, b map[string]any) bool {
	as, aok := a["isSidechain"]
	bs, bok := b["isSidechain"]
	if !aok || as == nil {
		as, aok = false, true
	}
	if !bok || bs == nil {
		bs, bok = false, true
	}
	aa, aaok := a["agentId"]
	ba, baok := b["agentId"]
	return jsStrictEqual(as, aok, bs, bok) && jsStrictEqual(aa, aaok, ba, baok)
}

// mapPos returns the map position of e, or math.MaxInt when it is not in the
// map.
func (x *splitSiblingIndex) mapPos(e map[string]any) int {
	if x.keyPos == nil {
		x.keyPos = make(map[string]int, len(x.m.order))
		for i, u := range x.m.order {
			x.keyPos[u] = i
		}
	}
	if p, ok := x.keyPos[entryUUID(e)]; ok {
		return p
	}
	return math.MaxInt
}

// offChainSiblings returns what to splice after the chain's copy e of
// message id: its parts that are off the chain, then the off-chain tool
// results answering them, each group sorted by timestamp. answered holds the
// tool_use ids the chain already answers.
func (x *splitSiblingIndex) offChainSiblings(id string, e map[string]any, inChain, answered map[string]bool) []map[string]any {
	parts := x.byMsgID[id]
	if parts == nil {
		parts = []map[string]any{e}
	}
	partUUIDs := map[string]bool{}
	for _, p := range parts {
		partUUIDs[entryUUID(p)] = true
	}
	var offChain, results, unlinked []map[string]any
	for _, p := range parts {
		if !inChain[entryUUID(p)] {
			offChain = append(offChain, p)
		}
	}
	seen := map[string]bool{}
	for _, p := range parts {
		for _, c := range x.children[entryUUID(p)] {
			cu := entryUUID(c)
			if inChain[cu] || seen[cu] {
				continue
			}
			seen[cu] = true
			if partUUIDs[entryParent(c)] {
				results = append(results, c)
			} else {
				unlinked = append(unlinked, c)
			}
		}
	}
	if len(unlinked) > 0 {
		results = x.claimUnlinkedResults(unlinked, results, partUUIDs, answered)
	}
	if len(offChain) == 0 && len(results) == 0 {
		return nil
	}
	slices.SortStableFunc(offChain, cmpTimestamp)
	slices.SortStableFunc(results, cmpTimestamp)
	return append(offChain, results...)
}

// claimUnlinkedResults appends to results, in map order, each tool result of
// unlinked (linked to the message only through a tool use id or
// sourceToolAssistantUUID) that answers one of the message's tool uses not
// yet answered by the chain, by results or by an earlier claim.
func (x *splitSiblingIndex) claimUnlinkedResults(unlinked, results []map[string]any, partUUIDs, answered map[string]bool) []map[string]any {
	claimed := maps.Clone(answered)
	for _, r := range results {
		for _, id := range blockStrings(r, "tool_result", "tool_use_id") {
			claimed[id] = true
		}
	}
	slices.SortStableFunc(unlinked, func(a, b map[string]any) int { return cmp.Compare(x.mapPos(a), x.mapPos(b)) })
	for _, c := range unlinked {
		ids := blockStrings(c, "tool_result", "tool_use_id")
		if !slices.ContainsFunc(ids, func(id string) bool {
			owner := x.toolUseOwner[id]
			return !claimed[id] && owner != nil && partUUIDs[entryUUID(owner)]
		}) {
			continue
		}
		for _, id := range ids {
			claimed[id] = true
		}
		results = append(results, c)
	}
	return results
}

// ---------------------------------------------------------------------------
// Trailing queued commands (TS QSe)
// ---------------------------------------------------------------------------

// trailingQueuedCommands returns the queued_command attachments hanging
// off the leaf through non-message entries: commands queued after the last
// turn that no turn reached (TS QSe). entries are the unrelinked entries.
func trailingQueuedCommands(entries []map[string]any, res chainResult) []map[string]any {
	if res.leaf == nil {
		return nil
	}
	nonMessageChildren := map[string][]map[string]any{}
	byUUID := make(map[string]map[string]any, len(entries))
	parentOf := func(e map[string]any) map[string]any {
		if p := entryParent(e); p != "" {
			return byUUID[p]
		}
		return nil
	}
	for _, e := range entries {
		byUUID[entryUUID(e)] = e
		if p := entryParent(e); p != "" && !isUserOrAssistant(e) {
			nonMessageChildren[p] = append(nonMessageChildren[p], e)
		}
	}
	seen := maps.Clone(res.inChain)
	if seen == nil {
		seen = map[string]bool{}
	}
	// Ancestors of off-chain messages belong to abandoned branches.
	abandoned := map[string]bool{}
	for _, e := range entries {
		if !isUserOrAssistant(e) || jsTruthy(e["isSidechain"]) || jsTruthy(e["teamName"]) || seen[entryUUID(e)] {
			continue
		}
		for p := parentOf(e); p != nil && !seen[entryUUID(p)] && !abandoned[entryUUID(p)]; p = parentOf(p) {
			abandoned[entryUUID(p)] = true
		}
	}

	var out []map[string]any
	stack := []map[string]any{res.leaf}
	first := true
	for len(stack) > 0 {
		d := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if !first {
			if seen[entryUUID(d)] {
				continue
			}
			seen[entryUUID(d)] = true
			if _, ok := queuedCommandAttachment(d); ok && !abandoned[entryUUID(d)] {
				out = append(out, d)
			}
		}
		first = false
		kids := nonMessageChildren[entryUUID(d)]
		if len(kids) > 1 {
			kids = slices.Clone(kids)
			slices.SortStableFunc(kids, cmpTimestamp)
		}
		for i := len(kids) - 1; i >= 0; i-- {
			if !seen[entryUUID(kids[i])] {
				stack = append(stack, kids[i])
			}
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Rendering queued commands and local commands (TS SC)
// ---------------------------------------------------------------------------

// localCommandKind classifies a user message by the tag its (last) text
// starts with: "record", "output" or "caveat" (TS JL()).
func localCommandKind(e map[string]any) string {
	if _, ok := e["promptSource"]; ok {
		return ""
	}
	msg, _ := e["message"].(map[string]any)
	var text string
	switch content := msg["content"].(type) {
	case string:
		text = content
	case []any:
		found := false
		for i := len(content) - 1; i >= 0; i-- {
			if block, ok := content[i].(map[string]any); ok && block["type"] == "text" {
				text, found = block["text"].(string)
				break
			}
		}
		if !found {
			return ""
		}
	default:
		return ""
	}
	switch {
	case strings.HasPrefix(text, "<command-name>"):
		return "record"
	case strings.HasPrefix(text, "<local-command-stdout>"), strings.HasPrefix(text, "<local-command-stderr>"):
		return "output"
	case strings.HasPrefix(text, "<local-command-caveat>"):
		return "caveat"
	}
	return ""
}

// completedLocalCommands returns the indices of the command record and
// output messages that follow a local-command caveat (TS ZSe).
func completedLocalCommands(entries []map[string]any) map[int]bool {
	out := map[int]bool{}
	for o, e := range entries {
		if e["type"] != "user" || !jsTruthy(e["isMeta"]) || localCommandKind(e) != "caveat" {
			continue
		}
		recorded := false
		for i := o + 1; i < len(entries); i++ {
			a := entries[i]
			if a["type"] == "assistant" {
				break
			}
			if a["type"] != "user" || jsTruthy(a["isMeta"]) {
				continue
			}
			kind := localCommandKind(a)
			if kind == "record" && !recorded {
				recorded = true
			} else if kind != "output" || !recorded {
				break
			}
			out[i] = true
		}
	}
	return out
}

// repliedTo reports, per index, whether the next significant entry after it
// is a reply (assistant message, tool result or interrupt) rather than a new
// prompt (TS qSe).
func repliedTo(entries []map[string]any, localCmds map[int]bool) []bool {
	out := make([]bool, len(entries))
	state := ""
	for o := len(entries) - 1; o >= 0; o-- {
		e := entries[o]
		out[o] = state == "reply"
		switch {
		case e["type"] == "assistant" || isToolResultUser(e) || isInterruptMessage(e):
			state = "reply"
		case e["type"] == "user" && !jsTruthy(e["isMeta"]) && !jsTruthy(e["isCompactSummary"]) && !localCmds[o]:
			state = "prompt"
		}
	}
	return out
}

// absorbedCopies decides, for queued commands absorbed mid-turn, which
// copies are rendered (TS YSe).
func absorbedCopies(entries []map[string]any, delivered map[string]int) map[int]bool {
	out := map[int]bool{}
	if len(delivered) == 0 {
		return out
	}
	groups := map[string][]int{}
	var keys []string
	for i, e := range entries {
		key := ""
		if id := queuedCommandField(e, "delivery_id"); id != "" {
			key = "delivery:" + id
		} else if src := queuedCommandField(e, "source_uuid"); src != "" {
			key = "uuid:" + src
		}
		if _, ok := delivered[key]; key == "" || !ok {
			continue
		}
		if _, ok := groups[key]; !ok {
			keys = append(keys, key)
		}
		groups[key] = append(groups[key], i)
	}
	for _, key := range keys {
		idx := groups[key]
		if strings.HasPrefix(key, "delivery:") {
			for _, i := range idx {
				_, ok := delivered["entry:"+entryUUID(entries[i])]
				out[i] = ok
			}
			continue
		}
		n := delivered[key]
		for l, i := range idx {
			out[i] = l >= len(idx)-n
		}
	}
	return out
}

// isForwardedIntent reports whether a queued command carries a valid
// forwardedIntent ({lineage: non-empty string}); such commands are never
// rendered.
func isForwardedIntent(att map[string]any) bool {
	fi, ok := att["forwardedIntent"].(map[string]any)
	if !ok {
		return false
	}
	lineage, ok := fi["lineage"].(string)
	return ok && lineage != ""
}

// queuedCommandAsUser converts a reached queued_command attachment into the
// user message it stands for (TS JSe), or returns e unchanged. uuids holds
// the uuids already present and is extended.
func queuedCommandAsUser(e map[string]any, render bool, uuids map[string]bool, keepMeta bool) map[string]any {
	if !render {
		return e
	}
	att, ok := queuedCommandAttachment(e)
	if !ok {
		return e
	}
	var origin any
	if o, ok := att["origin"].(map[string]any); ok {
		if _, kindOK := o["kind"].(string); kindOK {
			origin = o
		}
	}
	prompt := att["prompt"]
	_, promptIsString := prompt.(string)
	_, promptIsArray := prompt.([]any)
	if (jsTruthy(att["isMeta"]) && !keepMeta && !keepsMetaOrigin(origin)) || (!promptIsString && !promptIsArray) {
		return e
	}
	if isForwardedIntent(att) {
		return e
	}
	uuid := entryUUID(e)
	if src := str(att["source_uuid"]); src != "" {
		uuid = src
	}
	if origin != nil {
		origin = normalizeOrigin(origin)
	} else if att["commandMode"] == "task-notification" {
		origin = map[string]any{"kind": "task-notification"}
	}
	if uuid != entryUUID(e) && uuids[uuid] {
		return e
	}
	uuids[uuid] = true
	out := map[string]any{
		"type":            "user",
		"uuid":            uuid,
		"message":         map[string]any{"role": "user", "content": prompt},
		"isMeta":          jsTruthy(att["isMeta"]),
		"isQueuedCommand": true,
	}
	for _, k := range [...]string{"parentUuid", "sessionId", "timestamp", "isSidechain", "teamName"} {
		if v, ok := e[k]; ok {
			out[k] = v
		}
	}
	if origin != nil {
		out["origin"] = origin
	}
	return out
}

// renderTranscriptEntries applies the queued-command and local-command
// rendering to a chain (TS SC). trailing marks trailing queued commands,
// which render even though no reply followed them.
func renderTranscriptEntries(entries []map[string]any, keepMeta bool, trailing map[string]bool, delivered map[string]int) []map[string]any {
	localCmds := completedLocalCommands(entries)
	replied := repliedTo(entries, localCmds)
	absorbed := absorbedCopies(entries, delivered)
	uuids := make(map[string]bool, len(entries))
	for _, e := range entries {
		uuids[entryUUID(e)] = true
	}
	out := make([]map[string]any, len(entries))
	for i, e := range entries {
		if localCmds[i] {
			out[i] = withField(e, "isCompletedLocalCommand", true, true)
			continue
		}
		render, ok := absorbed[i]
		if !ok {
			render = replied[i] || trailing[entryUUID(e)]
		}
		out[i] = queuedCommandAsUser(e, render, uuids, keepMeta)
	}
	return out
}

// isVisibleMessage reports whether a rendered chain entry is returned:
// user/assistant messages (and system messages when includeSystem is set)
// that are not sidechain or team messages, and not meta unless they come
// from a channel, observer or peer origin. Compact summaries are kept.
func isVisibleMessage(e map[string]any, includeSystem bool) bool {
	switch e["type"] {
	case "user", "assistant":
	case "system":
		if !includeSystem {
			return false
		}
	default:
		return false
	}
	if jsTruthy(e["isMeta"]) && !keepsMetaOrigin(e["origin"]) {
		return false
	}
	return !jsTruthy(e["isSidechain"]) && !jsTruthy(e["teamName"])
}

// toSessionMessage converts a rendered transcript entry into a
// SessionMessage (TS EC()).
func toSessionMessage(e map[string]any, parentToolUseID, parentAgentID string) SessionMessage {
	msg := SessionMessage{
		Type:                    str(e["type"]),
		UUID:                    str(e["uuid"]),
		SessionID:               str(e["sessionId"]),
		Message:                 e["message"],
		ParentToolUseID:         parentToolUseID,
		ParentAgentID:           parentAgentID,
		Timestamp:               str(e["timestamp"]),
		IsMeta:                  e["isMeta"] == true || e["isCompactSummary"] == true || e["isVisibleInTranscriptOnly"] == true,
		IsCompactSummary:        e["isCompactSummary"] == true,
		IsQueuedCommand:         e["isQueuedCommand"] == true,
		IsCompletedLocalCommand: e["isCompletedLocalCommand"] == true,
		InterruptedByShutdown:   e["interruptedByShutdown"] == true,
	}
	if e["toolDenialUnanswered"] == "stream-closed" {
		msg.ToolDenialUnanswered = "stream-closed"
	}
	if o, ok := normalizeOrigin(e["origin"]).(map[string]any); ok && o != nil {
		msg.Origin = o
	}
	return msg
}

// sessionMessagesFromParsed turns every parsed line of a main transcript
// (local or store) into the visible SessionMessages, before paging.
func sessionMessagesFromParsed(parsed []map[string]any, includeSystem bool) []SessionMessage {
	tracker := newDeliveryTracker()
	var entries []map[string]any
	for _, e := range parsed {
		if e == nil {
			continue
		}
		tracker.observe(e)
		if isTranscriptEntry(e) {
			entries = append(entries, e)
		}
	}
	res := buildConversationChain(entries)
	trailing := trailingQueuedCommands(entries, res)
	trailingIDs := make(map[string]bool, len(trailing))
	for _, e := range trailing {
		trailingIDs[entryUUID(e)] = true
	}
	rendered := renderTranscriptEntries(append(slices.Clip(res.chain), trailing...), false, trailingIDs, tracker.delivered())
	var messages []SessionMessage
	for _, e := range rendered {
		if isVisibleMessage(e, includeSystem) {
			messages = append(messages, toSessionMessage(e, "", ""))
		}
	}
	return messages
}

// buildSubagentChain builds the chain of a subagent transcript from its
// user, assistant and attachment entries: subagent transcripts are linear,
// so the last user/assistant entry is the leaf (TS Ube).
func buildSubagentChain(entries []map[string]any) []map[string]any {
	byUUID := make(map[string]map[string]any, len(entries))
	for _, e := range entries {
		byUUID[entryUUID(e)] = e
	}
	for i := len(entries) - 1; i >= 0; i-- {
		if !isUserOrAssistant(entries[i]) {
			continue
		}
		var chain []map[string]any
		seen := map[string]bool{}
		for cur := entries[i]; cur != nil && !seen[entryUUID(cur)]; {
			seen[entryUUID(cur)] = true
			chain = append(chain, cur)
			cur = nil
			if p := entryParent(chain[len(chain)-1]); p != "" {
				cur = byUUID[p]
			}
		}
		slices.Reverse(chain)
		return chain
	}
	return nil
}

// subagentMessagesFromParsed turns the parsed lines of a subagent
// transcript into SessionMessages, before paging. Meta messages are kept.
func subagentMessagesFromParsed(parsed []map[string]any, parentToolUseID, parentAgentID string) []SessionMessage {
	var entries []map[string]any
	for _, e := range parsed {
		if e == nil {
			continue
		}
		switch e["type"] {
		case "user", "assistant", "attachment":
			if _, ok := e["uuid"].(string); ok {
				entries = append(entries, e)
			}
		}
	}
	var messages []SessionMessage
	for _, e := range renderTranscriptEntries(buildSubagentChain(entries), true, nil, nil) {
		if isUserOrAssistant(e) {
			messages = append(messages, toSessionMessage(e, parentToolUseID, parentAgentID))
		}
	}
	return messages
}
