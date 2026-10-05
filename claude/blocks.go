package claude

import (
	"encoding/json/v2"
	"maps"

	"github.com/ironpark/gelati/internal/jsonx"
)

// ContentBlock is one block inside a message's content array. The set of
// implementations is closed: TextBlock, ThinkingBlock, RedactedThinkingBlock,
// ToolUseBlock, ToolResultBlock, ServerToolUseBlock, ServerToolResultBlock,
// MCPToolUseBlock, MCPToolResultBlock, MCPToolListingBlock,
// ContainerUploadBlock, CompactionBlock, FallbackBlock, ImageBlock,
// DocumentBlock and UnknownBlock. A block kind this SDK version does not model
// arrives as an UnknownBlock carrying the raw payload.
//
// A block's JSON encoding is its wire object without the "type" key, which
// BlockType reports.
type ContentBlock interface {
	isContentBlock()
	// BlockType reports the wire discriminator of the block.
	BlockType() string
}

// TextBlock is a plain text content block.
type TextBlock struct {
	Text string `json:"text"`
	// Citations lists the sources the text cites, when the API attached any.
	Citations []TextCitation `json:"citations,omitempty"`
}

func (*TextBlock) isContentBlock()   {}
func (*TextBlock) BlockType() string { return "text" }

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
	DocumentIndex     int    `json:"document_index,omitzero"`
	DocumentTitle     string `json:"document_title,omitempty"`
	FileID            string `json:"file_id,omitempty"`
	StartCharIndex    int    `json:"start_char_index,omitzero"`
	EndCharIndex      int    `json:"end_char_index,omitzero"`
	StartPageNumber   int    `json:"start_page_number,omitzero"`
	EndPageNumber     int    `json:"end_page_number,omitzero"`
	StartBlockIndex   int    `json:"start_block_index,omitzero"`
	EndBlockIndex     int    `json:"end_block_index,omitzero"`
	SearchResultIndex int    `json:"search_result_index,omitzero"`
	Source            string `json:"source,omitempty"`
	Title             string `json:"title,omitempty"`
	URL               string `json:"url,omitempty"`
	EncryptedIndex    string `json:"encrypted_index,omitempty"`
}

// ThinkingBlock is an extended-thinking content block.
type ThinkingBlock struct {
	Thinking  string `json:"thinking"`
	Signature string `json:"signature"`
}

func (*ThinkingBlock) isContentBlock()   {}
func (*ThinkingBlock) BlockType() string { return "thinking" }

// RedactedThinkingBlock is a thinking block whose content the API encrypted.
// Data must be passed back unchanged.
type RedactedThinkingBlock struct {
	Data string `json:"data"`
}

func (*RedactedThinkingBlock) isContentBlock()   {}
func (*RedactedThinkingBlock) BlockType() string { return "redacted_thinking" }

// ToolUseBlock records a tool invocation requested by the model.
type ToolUseBlock struct {
	ID    string         `json:"id"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`
	// Caller identifies who invoked the tool (direct, or a server tool such as
	// code execution), when the API reports it.
	Caller map[string]any `json:"caller,omitempty"`
	// ToolsetName names the toolset the tool belongs to, when it has one.
	ToolsetName string `json:"toolset_name,omitempty"`
}

func (*ToolUseBlock) isContentBlock()   {}
func (*ToolUseBlock) BlockType() string { return "tool_use" }

// ToolResultBlock carries the result of a tool invocation. The CLI sends the
// content either as a plain string or as a list of nested content dicts, so
// exactly one of ContentText and ContentList is set (both may be nil/empty when
// the CLI omitted the field).
type ToolResultBlock struct {
	ToolUseID   string           `json:"tool_use_id"`
	ContentText *string          `json:"-"`
	ContentList []map[string]any `json:"-"`
	IsError     *bool            `json:"is_error,omitzero"`
}

func (*ToolResultBlock) isContentBlock()   {}
func (*ToolResultBlock) BlockType() string { return "tool_result" }

// MarshalJSON writes the wire shape, with "content" holding whichever of
// ContentText and ContentList is set.
func (b ToolResultBlock) MarshalJSON() ([]byte, error) {
	type alias ToolResultBlock
	return marshalWithContent(alias(b), textOrList(b.ContentText, b.ContentList))
}

// marshalWithContent encodes block followed by content, when it is not nil,
// as its "content" member. The content fields of the result blocks are tagged
// "-", so their own encoding never has that key and nothing is checked.
func marshalWithContent(block, content any) ([]byte, error) {
	return jsonx.MarshalWithExtra(block, contentMember(content), func(string) bool { return false }, jsonx.LegacyEncode)
}

// UnmarshalJSON reads the wire shape produced by MarshalJSON. A member of an
// unexpected JSON type is left at its zero value.
func (b *ToolResultBlock) UnmarshalJSON(data []byte) error {
	type alias ToolResultBlock
	var w struct {
		alias
		Content any `json:"content"`
	}
	if err := unmarshalLenient(data, &w); err != nil {
		return err
	}
	*b = ToolResultBlock(w.alias)
	b.ContentText, b.ContentList = splitContent(w.Content)
	return nil
}

// ServerToolName enumerates the server-side tools the API may run on the
// model's behalf. Newer CLI versions may report names not listed here.
type ServerToolName = string

// Known server-side tool names.
const (
	ServerToolAdvisor                 ServerToolName = "advisor"
	ServerToolWebSearch               ServerToolName = "web_search"
	ServerToolWebFetch                ServerToolName = "web_fetch"
	ServerToolCodeExecution           ServerToolName = "code_execution"
	ServerToolBashCodeExecution       ServerToolName = "bash_code_execution"
	ServerToolTextEditorCodeExecution ServerToolName = "text_editor_code_execution"
	ServerToolSearchToolRegex         ServerToolName = "tool_search_tool_regex"
	ServerToolSearchToolBM25          ServerToolName = "tool_search_tool_bm25"
)

// ServerToolUseBlock is a server-side tool invocation. The caller never needs
// to return a result for one.
type ServerToolUseBlock struct {
	ID    string         `json:"id"`
	Name  ServerToolName `json:"name"`
	Input map[string]any `json:"input"`
	// Caller identifies who invoked the tool, when the API reports it.
	Caller map[string]any `json:"caller,omitempty"`
}

func (*ServerToolUseBlock) isContentBlock()   {}
func (*ServerToolUseBlock) BlockType() string { return "server_tool_use" }

// Wire types of the server-side tool result blocks, all decoded as
// ServerToolResultBlock.
const (
	BlockAdvisorToolResult                 = "advisor_tool_result"
	BlockWebSearchToolResult               = "web_search_tool_result"
	BlockWebFetchToolResult                = "web_fetch_tool_result"
	BlockCodeExecutionToolResult           = "code_execution_tool_result"
	BlockBashCodeExecutionToolResult       = "bash_code_execution_tool_result"
	BlockTextEditorCodeExecutionToolResult = "text_editor_code_execution_tool_result"
	BlockToolSearchToolResult              = "tool_search_tool_result"
)

// ServerToolResultBlock is the result of a server-side tool call: one of the
// Block*ToolResult wire types, which share this shape. Content is passed
// through from the API verbatim; it is an object for every kind except a
// successful web search, whose result list arrives in ContentList instead.
type ServerToolResultBlock struct {
	// Type is the wire block type, e.g. BlockWebSearchToolResult. Empty
	// means BlockAdvisorToolResult.
	Type        string           `json:"type,omitempty"`
	ToolUseID   string           `json:"tool_use_id"`
	Content     map[string]any   `json:"-"`
	ContentList []map[string]any `json:"-"`
	// Caller identifies who invoked the tool, when the API reports it.
	Caller map[string]any `json:"caller,omitempty"`
}

func (*ServerToolResultBlock) isContentBlock() {}

// BlockType reports Type, defaulting to BlockAdvisorToolResult.
func (b *ServerToolResultBlock) BlockType() string {
	if b.Type == "" {
		return BlockAdvisorToolResult
	}
	return b.Type
}

// MarshalJSON writes the wire shape, with "content" holding whichever of
// Content and ContentList is set.
func (b ServerToolResultBlock) MarshalJSON() ([]byte, error) {
	type alias ServerToolResultBlock
	var content any
	switch {
	case b.Content != nil:
		content = b.Content
	case b.ContentList != nil:
		content = b.ContentList
	}
	return marshalWithContent(alias(b), content)
}

// UnmarshalJSON reads the wire shape produced by MarshalJSON. A member of an
// unexpected JSON type is left at its zero value.
func (b *ServerToolResultBlock) UnmarshalJSON(data []byte) error {
	type alias ServerToolResultBlock
	var w struct {
		alias
		Content any `json:"content"`
	}
	if err := unmarshalLenient(data, &w); err != nil {
		return err
	}
	*b = ServerToolResultBlock(w.alias)
	b.Content, b.ContentList = splitObjectContent(w.Content)
	return nil
}

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
	return marshalWithContent(alias(b), textOrList(b.ContentText, b.ContentList))
}

// UnmarshalJSON reads the wire shape produced by MarshalJSON. A member of an
// unexpected JSON type is left at its zero value.
func (b *MCPToolResultBlock) UnmarshalJSON(data []byte) error {
	type alias MCPToolResultBlock
	var w struct {
		alias
		Content any `json:"content"`
	}
	if err := unmarshalLenient(data, &w); err != nil {
		return err
	}
	*b = MCPToolResultBlock(w.alias)
	b.ContentText, b.ContentList = splitContent(w.Content)
	return nil
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
	Content   any    `json:"content,omitzero"`
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
	return json.Marshal(b.wire(), jsonx.LegacyEncode)
}

// UnmarshalJSON keeps the whole object in Raw. A value that is not an object
// leaves b empty.
func (b *UnknownBlock) UnmarshalJSON(data []byte) error {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw, lenient); fatalDecodeErr(err) {
		return err
	}
	*b = UnknownBlock{Type: str(raw["type"]), Raw: raw}
	return nil
}

// wire returns a copy of Raw with Type as its "type" key.
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
