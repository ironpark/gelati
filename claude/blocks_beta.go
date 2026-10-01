package claude

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
)

// TextCitation is one source cited by a TextBlock. Type selects which of the
// location fields are meaningful:
//
//   - "char_location": DocumentIndex, DocumentTitle, FileID, StartCharIndex,
//     EndCharIndex
//   - "page_location": DocumentIndex, DocumentTitle, FileID,
//     StartPageNumber, EndPageNumber
//   - "content_block_location": DocumentIndex, DocumentTitle, FileID,
//     StartBlockIndex, EndBlockIndex
//   - "web_search_result_location": URL, Title, EncryptedIndex
//   - "search_result_location": SearchResultIndex, Source, Title,
//     StartBlockIndex, EndBlockIndex
type TextCitation struct {
	Type              string `json:"type"`
	CitedText         string `json:"cited_text"`
	DocumentIndex     int    `json:"document_index,omitempty"`
	DocumentTitle     string `json:"document_title,omitempty"`
	FileID            string `json:"file_id,omitempty"`
	StartCharIndex    int    `json:"start_char_index,omitempty"`
	EndCharIndex      int    `json:"end_char_index,omitempty"`
	StartPageNumber   int    `json:"start_page_number,omitempty"`
	EndPageNumber     int    `json:"end_page_number,omitempty"`
	StartBlockIndex   int    `json:"start_block_index,omitempty"`
	EndBlockIndex     int    `json:"end_block_index,omitempty"`
	SearchResultIndex int    `json:"search_result_index,omitempty"`
	Source            string `json:"source,omitempty"`
	Title             string `json:"title,omitempty"`
	URL               string `json:"url,omitempty"`
	EncryptedIndex    string `json:"encrypted_index,omitempty"`
}

// RedactedThinkingBlock is a thinking block whose content the API encrypted.
// Data must be passed back unchanged.
type RedactedThinkingBlock struct {
	Data string `json:"data"`
}

func (*RedactedThinkingBlock) isContentBlock()   {}
func (*RedactedThinkingBlock) BlockType() string { return "redacted_thinking" }

// MCPToolUseBlock is a call the API made to a tool of a remote MCP server
// (the MCP connector).
type MCPToolUseBlock struct {
	ID         string         `json:"id"`
	Name       string         `json:"name"`
	ServerName string         `json:"server_name"`
	Input      map[string]any `json:"input"`
}

func (*MCPToolUseBlock) isContentBlock()   {}
func (*MCPToolUseBlock) BlockType() string { return "mcp_tool_use" }

// MCPToolResultBlock is the result of an MCPToolUseBlock. The API sends the
// content as a string or as a list of text blocks, so at most one of
// ContentText and ContentList is set.
type MCPToolResultBlock struct {
	ToolUseID   string           `json:"tool_use_id"`
	ContentText *string          `json:"-"`
	ContentList []map[string]any `json:"-"`
	IsError     bool             `json:"is_error"`
}

func (*MCPToolResultBlock) isContentBlock()   {}
func (*MCPToolResultBlock) BlockType() string { return "mcp_tool_result" }

// MarshalJSON writes the wire shape, with "content" holding whichever of
// ContentText and ContentList is set.
func (b MCPToolResultBlock) MarshalJSON() ([]byte, error) {
	type alias MCPToolResultBlock
	return marshalWithContent(alias(b), contentValue(b.ContentText, b.ContentList))
}

// UnmarshalJSON reads the wire shape produced by MarshalJSON.
func (b *MCPToolResultBlock) UnmarshalJSON(data []byte) error {
	type alias MCPToolResultBlock
	var a alias
	err := json.Unmarshal(data, &a)
	if fatalDecodeErr(err) {
		return err
	}
	*b = MCPToolResultBlock(a)
	var c struct {
		Content any `json:"content"`
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return err
	}
	b.ContentText, b.ContentList = splitContent(c.Content)
	return err
}

// MCPToolListingBlock lists the tools a remote MCP server offers.
type MCPToolListingBlock struct {
	MCPServerName string           `json:"mcp_server_name"`
	Tools         []map[string]any `json:"tools"`
}

func (*MCPToolListingBlock) isContentBlock()   {}
func (*MCPToolListingBlock) BlockType() string { return "mcp_tool_listing" }

// ContainerUploadBlock references a file uploaded to the code execution
// container.
type ContainerUploadBlock struct {
	FileID string `json:"file_id"`
}

func (*ContainerUploadBlock) isContentBlock()   {}
func (*ContainerUploadBlock) BlockType() string { return "container_upload" }

// CompactionBlock is a server-side context compaction summary. Content is
// empty when the API sent only EncryptedContent.
type CompactionBlock struct {
	Content          string           `json:"content"`
	EncryptedContent string           `json:"encrypted_content"`
	Signature        string           `json:"signature,omitempty"`
	ToolChanges      []map[string]any `json:"tool_changes,omitempty"`
}

func (*CompactionBlock) isContentBlock()   {}
func (*CompactionBlock) BlockType() string { return "compaction" }

// FallbackInfo names the model on one side of a FallbackBlock.
type FallbackInfo struct {
	Model string `json:"model"`
}

// FallbackTrigger explains why a FallbackBlock switched models.
type FallbackTrigger struct {
	// Type is "refusal".
	Type string `json:"type"`
	// Category is the refusal policy category, e.g. "cyber"; empty when the
	// refusal maps to none.
	Category string `json:"category,omitempty"`
}

// FallbackBlock marks the point where the API switched to a fallback model.
type FallbackBlock struct {
	From    FallbackInfo    `json:"from"`
	To      FallbackInfo    `json:"to"`
	Trigger FallbackTrigger `json:"trigger"`
}

func (*FallbackBlock) isContentBlock()   {}
func (*FallbackBlock) BlockType() string { return "fallback" }

// StopDetails explains a "refusal" stop reason on an AssistantMessage.
type StopDetails struct {
	// Type is "refusal".
	Type string `json:"type,omitempty"`
	// Category is the refusal policy category (cyber, bio, frontier_llm,
	// reasoning_extraction, general_harms); empty when none applies.
	Category string `json:"category,omitempty"`
	// Explanation is display-only prose; never parse it.
	Explanation string `json:"explanation,omitempty"`
}

// Block source types for BlockSource.Type.
const (
	SourceBase64  = "base64"
	SourceURL     = "url"
	SourceFile    = "file"
	SourceText    = "text"
	SourceContent = "content"
)

// BlockSource is where an ImageBlock or DocumentBlock gets its bytes. Type
// selects the fields used:
//
//   - SourceBase64: MediaType and Data (base64), e.g. "image/png" or
//     "application/pdf"
//   - SourceURL: URL
//   - SourceFile: FileID, from the Files API
//   - SourceText (documents only): MediaType "text/plain" and Data (plain text)
//   - SourceContent (documents only): Content, a string or a list of text and
//     image block objects
type BlockSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
	FileID    string `json:"file_id,omitempty"`
	Content   any    `json:"content,omitempty"`
}

// ImageBlock is an image. It appears in user messages, and can be sent with
// UserInput.Blocks.
type ImageBlock struct {
	Source BlockSource `json:"source"`
}

func (*ImageBlock) isContentBlock()   {}
func (*ImageBlock) BlockType() string { return "image" }

// DocumentBlock is a document such as a PDF or plain text. It appears in user
// messages, and can be sent with UserInput.Blocks.
type DocumentBlock struct {
	Source BlockSource `json:"source"`
	// Title and Context describe the document to the model.
	Title   string `json:"title,omitempty"`
	Context string `json:"context,omitempty"`
	// Citations is the citations config, e.g. {"enabled": true}.
	Citations map[string]any `json:"citations,omitempty"`
}

func (*DocumentBlock) isContentBlock()   {}
func (*DocumentBlock) BlockType() string { return "document" }

// UnknownBlock is a content block this SDK version does not model, kept so a
// newer CLI or API loses nothing. Raw is the full wire object, including its
// "type" key; unlike other blocks, an UnknownBlock's JSON encoding is Raw
// verbatim. An UnknownBlock can also be sent with UserInput.Blocks to pass a
// block kind through unchanged.
type UnknownBlock struct {
	// Type is the wire discriminator.
	Type string
	// Raw is the full block object.
	Raw map[string]any
}

func (*UnknownBlock) isContentBlock() {}

// BlockType reports Type.
func (b *UnknownBlock) BlockType() string { return b.Type }

// MarshalJSON writes Raw, with Type as its "type" key.
func (b UnknownBlock) MarshalJSON() ([]byte, error) {
	return json.Marshal(b.wire())
}

// UnmarshalJSON keeps the whole object in Raw.
func (b *UnknownBlock) UnmarshalJSON(data []byte) error {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*b = UnknownBlock{Type: str(raw["type"]), Raw: raw}
	return nil
}

func (b UnknownBlock) wire() map[string]any {
	out := maps.Clone(b.Raw)
	if out == nil {
		out = map[string]any{}
	}
	if b.Type != "" {
		out["type"] = b.Type
	}
	return out
}

// ---------------------------------------------------------------------------
// Block encoding helpers
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
	return json.Marshal(m)
}

// contentValue picks the populated one of a text-or-list content pair, or nil.
func contentValue(text *string, list []map[string]any) any {
	switch {
	case text != nil:
		return *text
	case list != nil:
		return list
	}
	return nil
}

// marshalWithContent marshals v (a struct) and appends a "content" key when
// content is non-nil, preserving v's field order.
func marshalWithContent(v any, content any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil || content == nil {
		return b, err
	}
	c, err := json.Marshal(content)
	if err != nil {
		return nil, err
	}
	b = bytes.TrimSuffix(b, []byte("}"))
	if len(b) > 1 {
		b = append(b, ',')
	}
	b = append(b, `"content":`...)
	b = append(b, c...)
	return append(b, '}'), nil
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

func objectItems(items []any) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		if m, ok := it.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// decodeLenient fills v from data the way the TypeScript SDK reads frames:
// without validation. A field whose JSON type does not match is left at its
// zero value; the rest is still decoded.
func decodeLenient(data []byte, v any) {
	_ = json.Unmarshal(data, v)
}

// fatalDecodeErr reports whether err is worse than a field type mismatch.
// Custom UnmarshalJSON methods keep decoding past mismatches, so that lenient
// parsing still sees the rest of the block, and return the mismatch at the
// end.
func fatalDecodeErr(err error) bool {
	var te *json.UnmarshalTypeError
	return err != nil && !errors.As(err, &te)
}
