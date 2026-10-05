package claude

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
)

// ---------------------------------------------------------------------------
// Decoding
// ---------------------------------------------------------------------------

// newBlock returns a new, empty block of type T.
func newBlock[T any, P interface {
	*T
	ContentBlock
}]() ContentBlock {
	return P(new(T))
}

// blockFactories maps a block's wire type to a constructor of its struct. The
// keys come from the blocks' own BlockType methods, so they cannot drift.
var blockFactories = func() map[string]func() ContentBlock {
	m := map[string]func() ContentBlock{}
	for _, f := range []func() ContentBlock{
		newBlock[TextBlock], newBlock[ThinkingBlock], newBlock[RedactedThinkingBlock],
		newBlock[ToolUseBlock], newBlock[ToolResultBlock], newBlock[ServerToolUseBlock],
		newBlock[MCPToolUseBlock], newBlock[MCPToolResultBlock], newBlock[MCPToolListingBlock],
		newBlock[ContainerUploadBlock], newBlock[CompactionBlock], newBlock[FallbackBlock],
		newBlock[ImageBlock], newBlock[DocumentBlock],
	} {
		m[f().BlockType()] = f
	}
	// The server-side tool results share one struct.
	for _, t := range []string{
		BlockAdvisorToolResult, BlockWebSearchToolResult, BlockWebFetchToolResult,
		BlockCodeExecutionToolResult, BlockBashCodeExecutionToolResult,
		BlockTextEditorCodeExecutionToolResult, BlockToolSearchToolResult,
	} {
		m[t] = newBlock[ServerToolResultBlock]
	}
	return m
}()

// parseContent decodes a message's content: a string, a block array, or (for
// anything else that is not null) its JSON text.
func parseContent(raw jsontext.Value) (*string, []ContentBlock) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	switch raw[0] {
	case '"':
		var s string
		decodeLenient(raw, &s)
		return &s, nil
	case '[':
		var items []jsontext.Value
		decodeLenient(raw, &items)
		return nil, parseBlocks(items)
	}
	s := string(raw)
	return &s, nil
}

// parseBlocks decodes a content array. Items that are not objects are
// skipped; objects of an unknown type become UnknownBlock.
func parseBlocks(items []jsontext.Value) []ContentBlock {
	blocks := make([]ContentBlock, 0, len(items))
	for _, item := range items {
		if b := parseBlock(item); b != nil {
			blocks = append(blocks, b)
		}
	}
	return blocks
}

// parseBlock decodes one content block, or returns nil when item is not a
// JSON object.
func parseBlock(item jsontext.Value) ContentBlock {
	item = bytes.TrimSpace(item)
	if len(item) == 0 || item[0] != '{' {
		return nil
	}
	var head struct {
		Type string `json:"type"`
	}
	decodeLenient(item, &head)
	f, ok := blockFactories[head.Type]
	if !ok {
		f = newBlock[UnknownBlock]
	}
	b := f()
	decodeLenient(item, b)
	return b
}

// splitContent splits string-or-list content. A list yields a non-nil slice of
// its object items, possibly empty.
func splitContent(v any) (*string, []map[string]any) {
	switch c := v.(type) {
	case string:
		return &c, nil
	case []any:
		return nil, objectItems(c)
	}
	return nil, nil
}

// splitObjectContent splits object-or-list content.
func splitObjectContent(v any) (map[string]any, []map[string]any) {
	switch c := v.(type) {
	case map[string]any:
		return c, nil
	case []any:
		return nil, objectItems(c)
	}
	return nil, nil
}

// ---------------------------------------------------------------------------
// Encoding
// ---------------------------------------------------------------------------

// contentBlockWire renders a block as its stream-json wire object.
func contentBlockWire(b ContentBlock) (map[string]any, error) {
	if u, ok := b.(*UnknownBlock); ok {
		return u.wire(), nil
	}
	out, err := toWireMap(b, "content block")
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = map[string]any{}
	}
	out["type"] = b.BlockType()
	return out, nil
}

// wireBlock marshals a block in its stream-json wire form, "type" included.
type wireBlock struct{ ContentBlock }

func (w wireBlock) MarshalJSON() ([]byte, error) {
	m, err := contentBlockWire(w.ContentBlock)
	if err != nil {
		return nil, err
	}
	return json.Marshal(m, marshalOpts)
}

// textOrList picks the populated one of a text-or-list content pair, or nil.
func textOrList(text *string, list []map[string]any) any {
	switch {
	case text != nil:
		return *text
	case list != nil:
		return list
	}
	return nil
}

// contentMember returns content as the "content" member to merge into a
// block's encoding, or nil when content is nil.
func contentMember(content any) map[string]any {
	if content == nil {
		return nil
	}
	return map[string]any{"content": content}
}
