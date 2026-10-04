package claude

import (
	"maps"
	"time"
)

// UserInput is one user turn written to the CLI's input stream: the
// TypeScript SDKUserMessage.
type UserInput struct {
	// Content is the prompt text.
	Content string
	// Blocks sends the message as content blocks, e.g. an *ImageBlock or
	// *DocumentBlock next to a *TextBlock; an *UnknownBlock passes any other
	// block kind through verbatim. When Blocks is set, a non-empty Content
	// becomes a leading text block.
	Blocks []ContentBlock
	// UUID identifies the message. Set it to correlate replies
	// (AssistantMessage.UserMessageUUID, ResultMessage.UserMessageUUID) and to
	// target the message later, e.g. with file rewinds.
	UUID string
	// SessionID targets a specific session; empty lets the CLI choose.
	SessionID string
	// ParentToolUseID attributes the turn to a tool call, when relevant.
	ParentToolUseID string
	// Origin stamps the message's provenance. Only the "human" kind is
	// honored from an SDK host.
	Origin *MessageOrigin
	// Priority queues the message: MessagePriorityNow, MessagePriorityNext
	// or MessagePriorityLater. Empty leaves the CLI default.
	Priority MessagePriority
	// ShouldQuery, when false, appends the message to the transcript without
	// starting a turn; it is merged into the next message that does. nil
	// leaves the CLI default (start a turn).
	ShouldQuery *bool
	// IsSynthetic marks content the user did not type.
	IsSynthetic bool
	// Timestamp is the message's creation time, for display; zero omits it.
	Timestamp time.Time
	// ClientComposed sends the text as written: no @-mention expansion, no
	// slash-command dispatch and no turn-start attachments. It is what
	// Options.VerbatimPrompts sets on every message.
	ClientComposed bool
	// PastedContent is text the user pasted rather than typed; the CLI
	// appends each entry after the typed text.
	PastedContent []string
	// InlinePastes marks pasted text that is still inside Content where the
	// user put it, one entry per paste.
	InlinePastes []string
	// Raw replaces the whole frame when set, for fields this struct does not
	// model. Content and the other fields are ignored.
	Raw map[string]any
}

// frame renders the input as a stream-json user message.
func (u UserInput) frame() map[string]any {
	if u.Raw != nil {
		return u.Raw
	}
	var content any = u.Content
	if len(u.Blocks) > 0 {
		blocks := make([]any, 0, len(u.Blocks)+1)
		if u.Content != "" {
			blocks = append(blocks, wireBlock{&TextBlock{Text: u.Content}})
		}
		for _, b := range u.Blocks {
			if b != nil {
				blocks = append(blocks, wireBlock{b})
			}
		}
		content = blocks
	}
	out := map[string]any{
		"type":               "user",
		"session_id":         u.SessionID,
		"message":            map[string]any{"role": "user", "content": content},
		"parent_tool_use_id": nil,
	}
	if u.ParentToolUseID != "" {
		out["parent_tool_use_id"] = u.ParentToolUseID
	}
	if u.Origin != nil {
		out["origin"] = u.Origin
	}
	if u.UUID != "" {
		out["uuid"] = u.UUID
	}
	if u.Priority != "" {
		out["priority"] = u.Priority
	}
	if u.ShouldQuery != nil {
		out["shouldQuery"] = *u.ShouldQuery
	}
	if u.IsSynthetic {
		out["isSynthetic"] = true
	}
	if !u.Timestamp.IsZero() {
		// The ISO 8601 form JavaScript's Date.toISOString produces.
		out["timestamp"] = u.Timestamp.UTC().Format("2006-01-02T15:04:05.000Z07:00")
	}
	if u.ClientComposed {
		out["client_composed"] = true
	}
	if len(u.PastedContent) > 0 {
		out["pasted_content"] = u.PastedContent
	}
	if len(u.InlinePastes) > 0 {
		out["inline_pastes"] = u.InlinePastes
	}
	return out
}

// stampUserMessage applies Options.VerbatimPrompts to an outgoing frame. The
// frame is returned unchanged when verbatim is off; otherwise a copy carries
// client_composed=true, so a caller's Raw map is never mutated.
func stampUserMessage(frame map[string]any, verbatim bool) map[string]any {
	if !verbatim {
		return frame
	}
	out := maps.Clone(frame)
	out["client_composed"] = true
	return out
}
