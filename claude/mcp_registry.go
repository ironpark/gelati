package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"sync"
)

// MCPHandler is an in-process MCP server as the SDK sees it: something that
// answers the JSON-RPC messages the CLI's MCP client sends. *MCPServer is the
// built-in implementation; an adapter over another MCP library can implement
// it to serve resources, prompts and other capabilities.
type MCPHandler interface {
	// HandleMCPMessage answers one inbound JSON-RPC message: a request, a
	// notification, or the CLI's response to a request the server sent
	// through MCPConnector. It returns the JSON-RPC response for a request
	// and nil otherwise. ctx is cancelled when the CLI cancels the request
	// (notifications/cancelled or a control cancel), when the server is
	// removed, and when the session ends. An error becomes a JSON-RPC
	// internal error.
	HandleMCPMessage(ctx context.Context, message json.RawMessage) (json.RawMessage, error)
}

// MCPSendFunc delivers one server-initiated JSON-RPC message (a notification
// or a request) to the CLI. It fails once the server has been removed or the
// session has ended.
type MCPSendFunc func(ctx context.Context, message json.RawMessage) error

// MCPConnector is implemented by an MCPHandler that sends messages of its own,
// such as notifications/tools/list_changed, progress or logging
// notifications, or requests whose responses then arrive through
// HandleMCPMessage. The SDK calls ConnectMCP when the server is registered
// with a session and the returned disconnect when it is removed or the
// session ends.
type MCPConnector interface {
	ConnectMCP(send MCPSendFunc) (disconnect func())
}

// mcpServerNotFoundError reports an mcp_message for a server that is not
// registered.
type mcpServerNotFoundError struct{ name string }

func (e *mcpServerNotFoundError) Error() string {
	return fmt.Sprintf("Server '%s' not found", e.name)
}

// sdkMCPRegistry is one session's set of in-process MCP servers. It routes
// the CLI's mcp_message requests, cancels in-flight requests on
// notifications/cancelled, and carries server-initiated messages back to the
// CLI. Servers can be added and removed while the session runs.
type sdkMCPRegistry struct {
	eng *engine

	mu      sync.Mutex
	servers map[string]*sdkMCPEntry
	closed  bool
}

// sdkMCPEntry is one registered server.
type sdkMCPEntry struct {
	name       string
	handler    MCPHandler
	timeout    int
	disconnect func()

	mu       sync.Mutex
	removed  bool
	inflight map[string]*mcpInflight
}

// mcpInflight is one JSON-RPC request being served.
type mcpInflight struct {
	cancel context.CancelFunc
}

func newSDKMCPRegistry(eng *engine) *sdkMCPRegistry {
	return &sdkMCPRegistry{eng: eng, servers: map[string]*sdkMCPEntry{}}
}

// connect registers a server under name. A name already registered is left
// alone; the caller decides whether that is a replacement.
func (r *sdkMCPRegistry) connect(name string, cfg *MCPSDKServerConfig) {
	entry := &sdkMCPEntry{
		name:     name,
		handler:  cfg.Instance,
		timeout:  positiveTimeout(cfg.Timeout),
		inflight: map[string]*mcpInflight{},
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	if _, exists := r.servers[name]; exists {
		r.mu.Unlock()
		return
	}
	r.servers[name] = entry
	r.mu.Unlock()

	if connector, ok := cfg.Instance.(MCPConnector); ok {
		disconnect := connector.ConnectMCP(func(ctx context.Context, message json.RawMessage) error {
			return r.sendToCLI(ctx, entry, message)
		})
		entry.mu.Lock()
		if entry.removed {
			entry.mu.Unlock()
			if disconnect != nil {
				disconnect()
			}
			return
		}
		entry.disconnect = disconnect
		entry.mu.Unlock()
	}
}

// remove unregisters a server, cancelling its in-flight requests.
func (r *sdkMCPRegistry) remove(name string) {
	r.mu.Lock()
	entry := r.servers[name]
	delete(r.servers, name)
	r.mu.Unlock()
	if entry != nil {
		entry.close()
	}
}

// closeAll unregisters every server; called when the session ends.
func (r *sdkMCPRegistry) closeAll() {
	r.mu.Lock()
	entries := slices.Collect(maps.Values(r.servers))
	r.servers = map[string]*sdkMCPEntry{}
	r.closed = true
	r.mu.Unlock()
	for _, entry := range entries {
		entry.close()
	}
}

func (r *sdkMCPRegistry) get(name string) *sdkMCPEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.servers[name]
}

// configs snapshots the registered servers as configs, keyed by name, for
// declaring the live set to the CLI. It is safe on a nil registry.
func (r *sdkMCPRegistry) configs() map[string]*MCPSDKServerConfig {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]*MCPSDKServerConfig, len(r.servers))
	for name, entry := range r.servers {
		out[name] = &MCPSDKServerConfig{Name: name, Instance: entry.handler, Timeout: entry.timeout}
	}
	return out
}

// hasServers reports whether any server is registered. It is safe on a nil
// registry.
func (r *sdkMCPRegistry) hasServers() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.servers) > 0
}

// names lists the registered servers, sorted.
func (r *sdkMCPRegistry) names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Sorted(maps.Keys(r.servers))
}

func (e *sdkMCPEntry) close() {
	e.mu.Lock()
	if e.removed {
		e.mu.Unlock()
		return
	}
	e.removed = true
	disconnect := e.disconnect
	inflight := e.inflight
	e.inflight = map[string]*mcpInflight{}
	e.mu.Unlock()
	for _, call := range inflight {
		call.cancel()
	}
	if disconnect != nil {
		disconnect()
	}
}

// route answers one mcp_message for serverName.
func (r *sdkMCPRegistry) route(ctx context.Context, serverName string, message json.RawMessage) (_ json.RawMessage, err error) {
	entry := r.get(serverName)
	if entry == nil {
		return nil, &mcpServerNotFoundError{name: serverName}
	}

	var msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			RequestID json.RawMessage `json:"requestId"`
		} `json:"params"`
	}
	_ = json.Unmarshal(message, &msg)

	if msg.Method == "notifications/cancelled" && len(msg.Params.RequestID) > 0 {
		entry.cancelRequest(jsonIDKey(msg.Params.RequestID))
	}
	if msg.Method != "" && len(msg.ID) > 0 && string(msg.ID) != "null" {
		var done func()
		ctx, done = entry.track(ctx, jsonIDKey(msg.ID))
		defer done()
	}

	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("MCP server '%s' panicked: %v", serverName, p)
		}
	}()
	return entry.handler.HandleMCPMessage(ctx, message)
}

// track registers an in-flight request so notifications/cancelled can cancel
// it. done releases it.
func (e *sdkMCPEntry) track(ctx context.Context, key string) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	call := &mcpInflight{cancel: cancel}
	e.mu.Lock()
	if e.removed {
		e.mu.Unlock()
		cancel()
		return ctx, func() {}
	}
	e.inflight[key] = call
	e.mu.Unlock()
	return ctx, func() {
		e.mu.Lock()
		if e.inflight[key] == call {
			delete(e.inflight, key)
		}
		e.mu.Unlock()
		cancel()
	}
}

func (e *sdkMCPEntry) cancelRequest(key string) {
	e.mu.Lock()
	call := e.inflight[key]
	delete(e.inflight, key)
	e.mu.Unlock()
	if call != nil {
		call.cancel()
	}
}

// jsonIDKey normalizes a JSON-RPC id so 7, 7.0 and "7" compare as their JSON
// values do.
func jsonIDKey(raw json.RawMessage) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return string(raw)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return string(raw)
	}
	return string(b)
}

// sendToCLI writes a server-initiated JSON-RPC message as an SDK-to-CLI
// mcp_message control request. Like the TypeScript SDK it does not wait for
// the CLI's acknowledgement; a JSON-RPC response to a server request comes
// back as an inbound mcp_message.
func (r *sdkMCPRegistry) sendToCLI(ctx context.Context, entry *sdkMCPEntry, message json.RawMessage) error {
	entry.mu.Lock()
	removed := entry.removed
	entry.mu.Unlock()
	if removed {
		return NewConnectionError(fmt.Sprintf("MCP server '%s' is no longer registered", entry.name))
	}
	if r.eng.isClosed() {
		return NewConnectionError("connection closed")
	}
	if !json.Valid(message) {
		return fmt.Errorf("claude: MCP server '%s' sent invalid JSON", entry.name)
	}
	frame := map[string]any{
		"type":       "control_request",
		"request_id": randomUUID(),
		"request": map[string]any{
			"subtype":     "mcp_message",
			"server_name": entry.name,
			"message":     message,
		},
	}
	payload, err := json.Marshal(frame)
	if err != nil {
		return fmt.Errorf("claude: encoding mcp message: %w", err)
	}
	return r.eng.transport.Write(ctx, payload)
}

// positiveTimeout keeps a timeout only when it is a positive number of
// milliseconds, as the TypeScript SDK does.
func positiveTimeout(ms int) int {
	if ms > 0 {
		return ms
	}
	return 0
}

// ---------------------------------------------------------------------------
// Engine wiring
// ---------------------------------------------------------------------------

// attachSDKMCPServers registers the in-process MCP servers configured in opts
// with the engine. The registry always exists so Client.SetMCPServers can add
// servers later.
func attachSDKMCPServers(eng *engine, opts *Options) {
	registry := newSDKMCPRegistry(eng)
	configs := sdkMCPServers(opts)
	for _, name := range slices.Sorted(maps.Keys(configs)) {
		registry.connect(name, configs[name])
	}
	eng.mcpServers = registry
	go func() {
		<-eng.closed
		registry.closeAll()
	}()
}

// sdkMCPServers returns the in-process MCP servers configured on opts, keyed
// by the name they are registered under. Servers without an instance are not
// in-process servers and are left to --mcp-config.
func sdkMCPServers(opts *Options) map[string]*MCPSDKServerConfig {
	if opts == nil {
		return nil
	}
	var out map[string]*MCPSDKServerConfig
	for name, cfg := range opts.MCPServers {
		sdk, ok := asSDKServer(cfg)
		if !ok {
			continue
		}
		if out == nil {
			out = map[string]*MCPSDKServerConfig{}
		}
		out[name] = sdk
	}
	return out
}

// processMCPServers returns the servers the CLI runs itself, which go to
// --mcp-config. In-process SDK servers are declared in the initialize
// request instead.
func processMCPServers(servers map[string]MCPServerConfig) map[string]MCPServerConfig {
	out := make(map[string]MCPServerConfig, len(servers))
	for name, cfg := range servers {
		if _, ok := asSDKServer(cfg); ok {
			continue
		}
		out[name] = cfg
	}
	return out
}

// asSDKServer reports whether cfg is an in-process SDK server, one with a
// live handler. An SDK config without an instance is left to --mcp-config.
func asSDKServer(cfg MCPServerConfig) (*MCPSDKServerConfig, bool) {
	sdk, ok := cfg.(*MCPSDKServerConfig)
	if !ok || sdk == nil || isNilHandler(sdk.Instance) {
		return nil, false
	}
	return sdk, true
}

// isNilHandler reports whether h is nil or a typed nil pointer.
func isNilHandler(h MCPHandler) bool {
	if h == nil {
		return true
	}
	v := reflect.ValueOf(h)
	switch v.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Func, reflect.Interface, reflect.Slice, reflect.Chan:
		return v.IsNil()
	}
	return false
}
