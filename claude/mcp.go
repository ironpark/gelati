package claude

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/ironpark/gelati/internal/jsonx"
	"github.com/ironpark/gelati/internal/safecall"
)

// DefaultMCPProtocolVersion is offered when the client does not name one.
const DefaultMCPProtocolVersion = "2024-11-05"

// JSON-RPC error codes used by the in-process server.
const (
	jsonRPCInvalidRequest = -32600
	jsonRPCMethodNotFound = -32601
	jsonRPCInternalError  = -32603
)

// ---------------------------------------------------------------------------
// Tool results
// ---------------------------------------------------------------------------

// ToolContent is one block of a tool result. Implementations are ToolText,
// ToolImage, ToolAudio, ToolResourceLink and ToolResource.
type ToolContent interface {
	// wire renders the block as MCP content, or reports false when the block
	// has no representation the CLI can render.
	wire() (map[string]any, bool)
}

// ToolText is a plain text result block.
type ToolText struct {
	Text string
}

func (t ToolText) wire() (map[string]any, bool) {
	return map[string]any{"type": "text", "text": t.Text}, true
}

// ToolImage is an image result block. Data is base64-encoded.
type ToolImage struct {
	Data     string
	MimeType string
}

func (i ToolImage) wire() (map[string]any, bool) {
	return map[string]any{"type": "image", "data": i.Data, "mimeType": i.MimeType}, true
}

// ToolAudio is an audio result block. Data is base64-encoded.
type ToolAudio struct {
	Data     string
	MimeType string
}

func (a ToolAudio) wire() (map[string]any, bool) {
	return map[string]any{"type": "audio", "data": a.Data, "mimeType": a.MimeType}, true
}

// ToolResourceLink points at a resource the client can fetch. It is sent as
// an MCP resource_link block.
type ToolResourceLink struct {
	Name        string
	URI         string
	Title       string
	Description string
	MimeType    string
}

func (l ToolResourceLink) wire() (map[string]any, bool) {
	out := map[string]any{"type": "resource_link", "uri": l.URI, "name": l.Name}
	putNonEmpty(out, "title", l.Title)
	putNonEmpty(out, "description", l.Description)
	putNonEmpty(out, "mimeType", l.MimeType)
	return out, true
}

// ToolResource embeds a resource's contents in the result. It is sent as an
// MCP resource block: Blob (base64) when set, Text otherwise.
type ToolResource struct {
	URI      string
	MimeType string
	Text     string
	Blob     string
}

func (r ToolResource) wire() (map[string]any, bool) {
	resource := map[string]any{"uri": r.URI}
	putNonEmpty(resource, "mimeType", r.MimeType)
	if r.Blob != "" {
		resource["blob"] = r.Blob
	} else {
		resource["text"] = r.Text
	}
	return map[string]any{"type": "resource", "resource": resource}, true
}

func putNonEmpty(m map[string]any, key, value string) {
	if value != "" {
		m[key] = value
	}
}

// ToolResult is what a tool handler returns: an MCP CallToolResult.
type ToolResult struct {
	Content []ToolContent
	// IsError marks the result as a failure the model should read and react
	// to, as opposed to a protocol error.
	IsError bool
	// StructuredContent is the result as a JSON object, for tools that
	// declare an output schema. nil leaves it out.
	StructuredContent any
	// Meta is the result's _meta, for client-specific hints such as
	// "claude/endTurn".
	Meta map[string]any
}

// TextResult is a ToolResult with a single text block.
func TextResult(format string, args ...any) ToolResult {
	if len(args) > 0 {
		format = fmt.Sprintf(format, args...)
	}
	return ToolResult{Content: []ToolContent{ToolText{Text: format}}}
}

// ErrorResult is a ToolResult reporting a failure to the model.
func ErrorResult(format string, args ...any) ToolResult {
	result := TextResult(format, args...)
	result.IsError = true
	return result
}

// wire renders the result as an MCP CallToolResult.
func (r ToolResult) wire() map[string]any {
	content := make([]map[string]any, 0, len(r.Content))
	for _, block := range r.Content {
		if block == nil {
			continue
		}
		if encoded, ok := block.wire(); ok {
			content = append(content, encoded)
		}
	}
	out := map[string]any{"content": content, "isError": r.IsError}
	if r.StructuredContent != nil {
		out["structuredContent"] = r.StructuredContent
	}
	if len(r.Meta) > 0 {
		out["_meta"] = r.Meta
	}
	return out
}

// ---------------------------------------------------------------------------
// Tool definitions
// ---------------------------------------------------------------------------

// ToolAnnotations are hints about a tool's behavior, plus MaxResultSizeChars.
type ToolAnnotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    *bool  `json:"readOnlyHint,omitzero"`
	DestructiveHint *bool  `json:"destructiveHint,omitzero"`
	IdempotentHint  *bool  `json:"idempotentHint,omitzero"`
	OpenWorldHint   *bool  `json:"openWorldHint,omitzero"`
	// MaxResultSizeChars is the size up to which Claude Code keeps a result
	// inline rather than persisting it and showing a preview. It is not an
	// MCP hint: it travels in the tool's _meta, because MCP clients drop
	// annotation fields they do not know.
	MaxResultSizeChars *int `json:"-"`
}

// ToolHandler runs one tool call. args is the raw JSON arguments object.
// Returning an error produces an isError result, never a protocol error.
type ToolHandler func(ctx context.Context, args jsontext.Value) (ToolResult, error)

// ToolDef declares one tool of an in-process MCP server.
type ToolDef struct {
	Name        string
	Description string
	// InputSchema is the tool's JSON Schema. A nil schema means "an object
	// with no declared properties".
	InputSchema map[string]any
	Handler     ToolHandler
	Annotations *ToolAnnotations
	// SearchHint is a short phrase that helps tool search find a deferred
	// tool. It is sent as _meta["anthropic/searchHint"].
	SearchHint string
	// AlwaysLoad keeps the tool in the prompt instead of deferring it behind
	// tool search. It is sent as _meta["anthropic/alwaysLoad"].
	AlwaysLoad bool
	// Meta is merged into the tool's _meta last, so it can override the
	// keys the fields above produce.
	Meta map[string]any
}

// NewTool builds a ToolDef whose handler receives decoded arguments. Arguments
// that do not decode into T become an isError result, so a malformed call from
// the model is reported to it rather than failing the request.
func NewTool[T any](name, description string, schema map[string]any, handler func(ctx context.Context, args T) (ToolResult, error)) ToolDef {
	return ToolDef{
		Name:        name,
		Description: description,
		InputSchema: schema,
		Handler: func(ctx context.Context, raw jsontext.Value) (ToolResult, error) {
			var args T
			if len(raw) > 0 {
				if err := jsonx.Unmarshal(raw, &args); err != nil {
					return ErrorResult("Input validation error: %s", err), nil
				}
			}
			return handler(ctx, args)
		},
	}
}

// ---------------------------------------------------------------------------
// Server
// ---------------------------------------------------------------------------

// MCPServer is an in-process MCP server. Build one with NewSDKMCPServer and
// register it through Options.MCPServers or Client.SetMCPServers; the SDK
// serves its JSON-RPC traffic over the control protocol, with no subprocess
// or socket in between.
//
// It serves initialize, ping, tools/list and tools/call. Its tool set may be
// changed at any time with AddTools and RemoveTools, which tell every
// connected session to re-list the tools. An MCPServer may be shared by
// several sessions and is safe for concurrent use.
type MCPServer struct {
	name         string
	version      string
	instructions string
	alwaysLoad   bool

	mu       sync.RWMutex
	tools    []ToolDef
	byName   map[string]ToolDef
	peers    map[uint64]MCPSendFunc
	nextPeer uint64
}

// SDKMCPServerOptions configures NewSDKMCPServerWithOptions.
type SDKMCPServerOptions struct {
	// Name is the server name reported in serverInfo.
	Name string
	// Version defaults to "1.0.0".
	Version string
	// Instructions are returned from initialize and shown to the model as
	// the server's instructions.
	Instructions string
	Tools        []ToolDef
	// AlwaysLoad keeps every tool of the server in the prompt instead of
	// deferring it behind tool search, by setting
	// _meta["anthropic/alwaysLoad"] on each tool. A tool's own Meta still
	// overrides it.
	AlwaysLoad bool
	// Timeout is the per-call tool timeout in milliseconds; see
	// MCPSDKServerConfig.Timeout.
	Timeout int
}

// NewSDKMCPServer builds an in-process MCP server configuration. An empty
// version defaults to "1.0.0".
//
//	calc := claude.NewSDKMCPServer("calculator", "", claude.NewTool(
//		"add", "Add two numbers",
//		map[string]any{"type": "object", "properties": map[string]any{
//			"a": map[string]any{"type": "number"},
//			"b": map[string]any{"type": "number"}},
//			"required": []string{"a", "b"}},
//		func(ctx context.Context, args struct{ A, B float64 }) (claude.ToolResult, error) {
//			return claude.TextResult("Sum: %v", args.A+args.B), nil
//		}))
//	opts := &claude.Options{MCPServers: map[string]claude.MCPServerConfig{"calc": calc}}
func NewSDKMCPServer(name, version string, tools ...ToolDef) *MCPSDKServerConfig {
	return NewSDKMCPServerWithOptions(SDKMCPServerOptions{Name: name, Version: version, Tools: tools})
}

// NewSDKMCPServerWithOptions is NewSDKMCPServer with the full option set of
// the TypeScript SDK's createSdkMcpServer.
func NewSDKMCPServerWithOptions(opts SDKMCPServerOptions) *MCPSDKServerConfig {
	version := opts.Version
	if version == "" {
		version = "1.0.0"
	}
	server := &MCPServer{
		name:         opts.Name,
		version:      version,
		instructions: opts.Instructions,
		alwaysLoad:   opts.AlwaysLoad,
	}
	server.setTools(opts.Tools)
	return &MCPSDKServerConfig{Name: opts.Name, Instance: server, Timeout: opts.Timeout}
}

// setTools replaces the tool set. The caller holds no lock.
func (s *MCPServer) setTools(tools []ToolDef) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tools = slices.Clone(tools)
	s.byName = make(map[string]ToolDef, len(tools))
	for _, tool := range tools {
		s.byName[tool.Name] = tool
	}
}

// AddTools adds tools to the server, replacing any tool of the same name, and
// notifies connected sessions that the tool list changed.
func (s *MCPServer) AddTools(tools ...ToolDef) {
	if len(tools) == 0 {
		return
	}
	s.mu.Lock()
	if s.byName == nil {
		s.byName = map[string]ToolDef{}
	}
	for _, tool := range tools {
		if _, exists := s.byName[tool.Name]; exists {
			i := slices.IndexFunc(s.tools, func(t ToolDef) bool { return t.Name == tool.Name })
			s.tools[i] = tool
		} else {
			s.tools = append(s.tools, tool)
		}
		s.byName[tool.Name] = tool
	}
	s.mu.Unlock()
	s.notifyToolsChanged()
}

// RemoveTools removes the named tools and, when any was present, notifies
// connected sessions that the tool list changed.
func (s *MCPServer) RemoveTools(names ...string) {
	s.mu.Lock()
	removed := false
	for _, name := range names {
		if _, ok := s.byName[name]; !ok {
			continue
		}
		delete(s.byName, name)
		s.tools = slices.DeleteFunc(s.tools, func(t ToolDef) bool { return t.Name == name })
		removed = true
	}
	s.mu.Unlock()
	if removed {
		s.notifyToolsChanged()
	}
}

// notifyToolsChanged sends notifications/tools/list_changed to every
// connected session. Delivery failures are ignored: a session that is gone
// re-lists the tools when it reconnects.
func (s *MCPServer) notifyToolsChanged() {
	_ = s.Notify(context.Background(), "notifications/tools/list_changed", nil)
}

// Notify sends a JSON-RPC notification (for example
// "notifications/message" for logging) to every session the server is
// connected to. params may be nil. It returns the joined delivery errors.
func (s *MCPServer) Notify(ctx context.Context, method string, params any) error {
	message := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		message["params"] = params
	}
	raw, err := json.Marshal(message, jsonx.LegacyEncode)
	if err != nil {
		return fmt.Errorf("claude: encoding mcp notification: %w", err)
	}
	s.mu.RLock()
	peers := slices.Collect(maps.Values(s.peers))
	s.mu.RUnlock()
	var errs []error
	for _, send := range peers {
		if err := send(ctx, raw); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// ConnectMCP attaches the server to one session so Notify reaches it. The
// SDK calls it when the server is registered and calls the returned function
// when the server is removed or the session ends.
func (s *MCPServer) ConnectMCP(send MCPSendFunc) (disconnect func()) {
	s.mu.Lock()
	if s.peers == nil {
		s.peers = map[uint64]MCPSendFunc{}
	}
	id := s.nextPeer
	s.nextPeer++
	s.peers[id] = send
	s.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			delete(s.peers, id)
			s.mu.Unlock()
		})
	}
}

// jsonRPCRequest is one message from the CLI's MCP client.
type jsonRPCRequest struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      jsontext.Value `json:"id"`
	Method  string         `json:"method"`
	Params  jsontext.Value `json:"params"`
}

// HandleMCPMessage answers one JSON-RPC message from the CLI's MCP client,
// implementing MCPHandler. It returns nil for notifications and responses,
// which get no reply.
func (s *MCPServer) HandleMCPMessage(ctx context.Context, message jsontext.Value) (jsontext.Value, error) {
	var req jsonRPCRequest
	if err := jsonx.Unmarshal(message, &req); err != nil {
		return jsonRPCErrorReply(nil, jsonRPCInvalidRequest, "Invalid JSON-RPC message")
	}
	if len(req.ID) == 0 || string(req.ID) == "null" {
		// A notification: nothing to answer.
		return nil, nil
	}

	switch req.Method {
	case "initialize":
		result := map[string]any{
			"protocolVersion": negotiateProtocolVersion(req.Params),
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": true}},
			"serverInfo":      map[string]any{"name": s.name, "version": s.version},
		}
		if s.instructions != "" {
			result["instructions"] = s.instructions
		}
		return jsonRPCReply(req.ID, result)
	case "ping":
		return jsonRPCReply(req.ID, map[string]any{})
	case "tools/list":
		return jsonRPCReply(req.ID, map[string]any{"tools": s.toolDescriptors()})
	case "tools/call":
		return jsonRPCReply(req.ID, s.callTool(ctx, req.Params).wire())
	default:
		return jsonRPCErrorReply(req.ID, jsonRPCMethodNotFound, "Method not found: "+req.Method)
	}
}

// toolDescriptors renders the tool list for tools/list.
func (s *MCPServer) toolDescriptors() []map[string]any {
	s.mu.RLock()
	tools := slices.Clone(s.tools)
	s.mu.RUnlock()
	out := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		schema := tool.InputSchema
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		descriptor := map[string]any{
			"name":        tool.Name,
			"description": tool.Description,
			"inputSchema": schema,
		}
		if tool.Annotations != nil {
			descriptor["annotations"] = tool.Annotations
		}
		if meta := s.toolMeta(tool); len(meta) > 0 {
			descriptor["_meta"] = meta
		}
		out = append(out, descriptor)
	}
	return out
}

// toolMeta builds a tool's _meta. Client-specific hints ride there under
// namespaced keys because MCP clients drop annotation fields they do not
// know. The server-wide alwaysLoad comes first so a tool's own Meta can
// override it, as in the TypeScript SDK.
func (s *MCPServer) toolMeta(tool ToolDef) map[string]any {
	meta := map[string]any{}
	if s.alwaysLoad || tool.AlwaysLoad {
		meta["anthropic/alwaysLoad"] = true
	}
	if tool.SearchHint != "" {
		meta["anthropic/searchHint"] = tool.SearchHint
	}
	if tool.Annotations != nil && tool.Annotations.MaxResultSizeChars != nil {
		meta["anthropic/maxResultSizeChars"] = *tool.Annotations.MaxResultSizeChars
	}
	maps.Copy(meta, tool.Meta)
	return meta
}

// callTool dispatches one tools/call. Unknown tools, invalid arguments, handler
// errors and handler panics all become isError results the model can read,
// never protocol errors.
func (s *MCPServer) callTool(ctx context.Context, params jsontext.Value) (result ToolResult) {
	var call struct {
		Name      string         `json:"name"`
		Arguments jsontext.Value `json:"arguments"`
	}
	if len(params) > 0 {
		if err := jsonx.Unmarshal(params, &call); err != nil {
			return ErrorResult("Input validation error: %s", err)
		}
	}
	s.mu.RLock()
	tool, ok := s.byName[call.Name]
	s.mu.RUnlock()
	if !ok {
		return ErrorResult("Tool '%s' not found", call.Name)
	}
	if tool.Handler == nil {
		return ErrorResult("Tool '%s' has no handler", call.Name)
	}
	if err := validateAgainstSchema(tool.InputSchema, call.Arguments); err != nil {
		return ErrorResult("Input validation error: %s", err)
	}

	out, err := func() (out ToolResult, err error) {
		defer safecall.Recover(&err, "Tool '"+call.Name+"'")
		return tool.Handler(ctx, call.Arguments)
	}()
	if err != nil {
		return ErrorResult("%s", err.Error())
	}
	return out
}

// jsonRPCReply encodes a JSON-RPC success response.
func jsonRPCReply(id jsontext.Value, result any) (jsontext.Value, error) {
	return json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result}, jsonx.LegacyEncode)
}

// jsonRPCErrorReply encodes a JSON-RPC error response. A missing id is sent as
// null.
func jsonRPCErrorReply(id jsontext.Value, code int, message string) (jsontext.Value, error) {
	var rawID any
	if len(id) > 0 {
		rawID = id
	}
	return json.Marshal(jsonRPCError(rawID, code, message), jsonx.LegacyEncode)
}

// jsonRPCError builds a JSON-RPC error response object.
func jsonRPCError(id any, code int, message string) map[string]any {
	return map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": message},
	}
}

// negotiateProtocolVersion echoes the client's protocol version when it named
// one, so a newer CLI is not forced down to an older revision.
func negotiateProtocolVersion(params jsontext.Value) string {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if len(params) > 0 && jsonx.Unmarshal(params, &p) == nil && p.ProtocolVersion != "" {
		return p.ProtocolVersion
	}
	return DefaultMCPProtocolVersion
}

// validateAgainstSchema performs the minimal JSON Schema checks the SDK makes
// without a schema library: the argument object decodes, every required
// property is present, and declared primitive types match. Anything else in the
// schema is left to the handler.
func validateAgainstSchema(schema map[string]any, raw jsontext.Value) error {
	if schema == nil {
		return nil
	}
	args := map[string]any{}
	if len(raw) > 0 {
		if err := jsonx.Unmarshal(raw, &args); err != nil {
			return fmt.Errorf("arguments must be an object: %w", err)
		}
	}
	for _, name := range schemaRequired(schema) {
		if _, ok := args[name]; !ok {
			return fmt.Errorf("%q is a required property", name)
		}
	}
	properties, _ := schema["properties"].(map[string]any)
	for name, value := range args {
		spec, ok := properties[name].(map[string]any)
		if !ok {
			continue
		}
		want, ok := spec["type"].(string)
		if !ok {
			continue
		}
		if !matchesJSONType(want, value) {
			return fmt.Errorf("%q must be of type %s", name, want)
		}
	}
	return nil
}

func schemaRequired(schema map[string]any) []string {
	switch required := schema["required"].(type) {
	case []string:
		return required
	case []any:
		return stringItems(required)
	}
	return nil
}

func matchesJSONType(want string, value any) bool {
	switch want {
	case "string":
		_, ok := value.(string)
		return ok
	case "number":
		_, ok := value.(float64)
		return ok
	case "integer":
		f, ok := value.(float64)
		return ok && f == float64(int64(f))
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "null":
		return value == nil
	}
	return true
}
