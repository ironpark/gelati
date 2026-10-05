package main

import (
	"bufio"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
)

// A minimal MCP server exposing the two "pirate math" tools of upstream's
// examples/resources/mcp_server.py. It implements just enough of the
// protocol for tool discovery and calls: initialize, tools/list,
// tools/call and ping.

type rpcRequest struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      jsontext.Value `json:"id,omitempty"`
	Method  string         `json:"method"`
	Params  jsontext.Value `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      jsontext.Value `json:"id"`
	Result  any            `json:"result,omitzero"`
	Error   *rpcError      `json:"error,omitzero"`
}

var pirateTools = []map[string]any{
	{"name": "pirate_multiply", "description": "Does multiplication like a pirate.", "inputSchema": twoInts},
	{"name": "pirate_divide", "description": "Does division like a pirate.", "inputSchema": twoInts},
}

var twoInts = map[string]any{
	"type":       "object",
	"properties": map[string]any{"a": map[string]any{"type": "integer"}, "b": map[string]any{"type": "integer"}},
	"required":   []string{"a", "b"},
}

// handle answers one request; it returns nil for notifications.
func handle(req rpcRequest) *rpcResponse {
	if req.ID == nil {
		return nil
	}
	resp := &rpcResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(req.Params, &p)
		if p.ProtocolVersion == "" {
			p.ProtocolVersion = "2025-06-18"
		}
		resp.Result = map[string]any{
			"protocolVersion": p.ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "Pirate Math", "version": "1.0.0"},
		}
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		resp.Result = map[string]any{"tools": pirateTools}
	case "tools/call":
		var p struct {
			Name      string `json:"name"`
			Arguments struct {
				A int `json:"a"`
				B int `json:"b"`
			} `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			resp.Error = &rpcError{Code: -32602, Message: err.Error()}
			break
		}
		a, b := p.Arguments.A, p.Arguments.B
		var text string
		switch p.Name {
		case "pirate_multiply":
			text = fmt.Sprintf("Pirate Multiplication: %d x %d = %d (add 'em, multiply by 7, subtract 13!)", a, b, (a+b)*7-13)
		case "pirate_divide":
			text = fmt.Sprintf("Pirate Division: %d / %d = %d (triple the first, double the second, add 42!)", a, b, a*3+b*2+42)
		default:
			resp.Error = &rpcError{Code: -32602, Message: "unknown tool " + p.Name}
		}
		if resp.Error == nil {
			resp.Result = map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}
		}
	default:
		resp.Error = &rpcError{Code: -32601, Message: "method not found: " + req.Method}
	}
	return resp
}

// serveMCPStdio serves newline-delimited JSON-RPC on r and w.
func serveMCPStdio(r io.Reader, w io.Writer) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		var req rpcRequest
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			continue
		}
		if resp := handle(req); resp != nil {
			b, err := json.Marshal(resp)
			if err != nil {
				return err
			}
			if _, err := w.Write(append(b, '\n')); err != nil {
				return err
			}
		}
	}
	return sc.Err()
}

// serveMCPHTTP serves the streamable HTTP transport with plain JSON
// responses; it offers no server-initiated stream.
func serveMCPHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req rpcRequest
	if err := json.UnmarshalRead(r.Body, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	resp := handle(req)
	if resp == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.MarshalWrite(w, resp)
}
