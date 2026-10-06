package codex

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"sync"

	"github.com/ironpark/gelati/internal/jsonx"
)

// Server-initiated request methods handled by this client.
const (
	MethodCommandApproval      = "item/commandExecution/requestApproval"
	MethodFileChangeApproval   = "item/fileChange/requestApproval"
	MethodPermissionsApproval  = "item/permissions/requestApproval"
	MethodItemRequestUserInput = "item/tool/requestUserInput"
	MethodChatGPTTokenRefresh  = "account/chatgptAuthTokens/refresh"
	MethodMcpElicitation       = "mcpServer/elicitation/request"
	MethodDynamicToolCall      = "item/tool/call"
)

// Decision is the client's answer to an approval request.
type Decision string

// Approval decisions accepted for command execution and file changes.
const (
	// DecisionAccept runs the proposed action once.
	DecisionAccept Decision = "accept"
	// DecisionAcceptForSession runs it and stops asking for this session.
	DecisionAcceptForSession Decision = "acceptForSession"
	// DecisionDecline refuses the action; the turn continues.
	DecisionDecline Decision = "decline"
	// DecisionCancel refuses the action and cancels the turn.
	DecisionCancel Decision = "cancel"
)

// AcceptWithExecpolicyAmendment approves a command and adds the amendment,
// usually CommandApprovalRequest.ProposedExecpolicyAmendment, so matching
// commands later run without asking.
func AcceptWithExecpolicyAmendment(amendment []string) Decision {
	return objectDecision("acceptWithExecpolicyAmendment", "execpolicy_amendment", amendment)
}

// ApplyNetworkPolicyAmendment records a persistent network rule for a host;
// action is "allow" or "deny".
func ApplyNetworkPolicyAmendment(host, action string) Decision {
	return objectDecision("applyNetworkPolicyAmendment", "network_policy_amendment",
		map[string]string{"host": host, "action": action})
}

// objectDecision encodes an object-form decision. Decision stays a string so
// that the plain decisions remain untyped constants; MarshalJSON emits the
// object unquoted.
func objectDecision(kind, field string, value any) Decision {
	b, _ := jsonx.Marshal(map[string]any{kind: map[string]any{field: value}}) // cannot fail
	return Decision(b)
}

// MarshalJSON emits plain decisions as strings and object-form decisions,
// built by AcceptWithExecpolicyAmendment or ApplyNetworkPolicyAmendment, as
// objects.
func (d Decision) MarshalJSON() ([]byte, error) {
	if strings.HasPrefix(string(d), "{") {
		return []byte(d), nil
	}
	return json.Marshal(string(d))
}

// NetworkApprovalContext is present when a command approval prompt is really a
// managed network-access prompt.
type NetworkApprovalContext struct {
	Host string `json:"host,omitempty"`
	// Protocol is "http", "https", "socks5Tcp", or "socks5Udp".
	Protocol string `json:"protocol,omitempty"`
}

// CommandApprovalRequest asks whether the agent may run a command.
type CommandApprovalRequest struct {
	ItemID   string `json:"itemId"`
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	Reason   string `json:"reason,omitempty"`
	Command  string `json:"command,omitempty"`
	Cwd      string `json:"cwd,omitempty"`
	// CommandActions describes the parsed command actions, when available.
	CommandActions jsontext.Value `json:"commandActions,omitempty"`
	// ProposedExecpolicyAmendment is an amendment the client may accept with
	// the acceptWithExecpolicyAmendment decision.
	ProposedExecpolicyAmendment []string `json:"proposedExecpolicyAmendment,omitempty"`
	// NetworkApprovalContext marks a managed network-access prompt.
	NetworkApprovalContext *NetworkApprovalContext `json:"networkApprovalContext,omitzero"`
	// ProposedNetworkPolicyAmendments are network rules the client may
	// accept with the applyNetworkPolicyAmendment decision.
	ProposedNetworkPolicyAmendments jsontext.Value `json:"proposedNetworkPolicyAmendments,omitempty"`
	// Kind is "command" or "writeStdin".
	Kind string `json:"kind,omitempty"`
	// ApprovalID distinguishes several prompts for one item.
	ApprovalID    string `json:"approvalId,omitempty"`
	EnvironmentID string `json:"environmentId,omitempty"`
	StartedAtMs   int64  `json:"startedAtMs,omitzero"`
	// Params is the raw request payload.
	Params jsontext.Value `json:"-"`
}

// FileChangeApprovalRequest asks whether the agent may apply file edits.
type FileChangeApprovalRequest struct {
	ItemID   string `json:"itemId"`
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	Reason   string `json:"reason,omitempty"`
	// GrantRoot is the root the agent asks to be granted write access to.
	GrantRoot   string `json:"grantRoot,omitempty"`
	StartedAtMs int64  `json:"startedAtMs,omitzero"`
	// Changes are the proposed edits. The request does not carry them: the
	// client takes them from the item's item/started notification, and
	// leaves them nil when it saw none.
	Changes []FileChange `json:"-"`
	// Params is the raw request payload.
	Params jsontext.Value `json:"-"`
}

// PermissionsRequest is sent by the built-in request_permissions tool.
type PermissionsRequest struct {
	ItemID        string `json:"itemId"`
	ThreadID      string `json:"threadId"`
	TurnID        string `json:"turnId"`
	EnvironmentID string `json:"environmentId,omitempty"`
	Cwd           string `json:"cwd,omitempty"`
	Reason        string `json:"reason,omitempty"`
	// Permissions is the requested network or filesystem permission set.
	Permissions jsontext.Value `json:"permissions,omitempty"`
	// Params is the raw request payload.
	Params jsontext.Value `json:"-"`
}

// Permission grant scopes.
const (
	// ScopeTurn grants permissions for the current turn only.
	ScopeTurn = "turn"
	// ScopeSession persists the grant for later turns in the session.
	ScopeSession = "session"
)

// PermissionsResponse carries the granted subset of a permissions request.
// Permissions that were not requested are ignored by the server.
type PermissionsResponse struct {
	// Permissions is the granted profile, an object with optional
	// "fileSystem" and "network" members; {} grants nothing.
	Permissions jsontext.Value `json:"permissions"`
	// Scope is ScopeTurn (the default) or ScopeSession.
	Scope string `json:"scope,omitempty"`
	// StrictAutoReview reviews every later command in the turn before it
	// runs.
	StrictAutoReview *bool `json:"strictAutoReview,omitzero"`
}

// grantNothing is the fail-closed permissions answer.
func grantNothing() *PermissionsResponse {
	return &PermissionsResponse{Permissions: jsontext.Value("{}")}
}

// TokenRefreshRequest asks the host application for fresh externally managed
// ChatGPT tokens.
type TokenRefreshRequest struct {
	Reason            string         `json:"reason,omitempty"`
	PreviousAccountID string         `json:"previousAccountId,omitempty"`
	Params            jsontext.Value `json:"-"`
}

// ChatGPTAuthTokens are externally managed ChatGPT credentials.
type ChatGPTAuthTokens struct {
	AccessToken      string `json:"accessToken"`
	ChatGPTAccountID string `json:"chatgptAccountId"`
	ChatGPTPlanType  string `json:"chatgptPlanType,omitempty"`
}

// ApprovalHandler answers the approval requests the app-server sends during a
// turn. The context is canceled when the turn ends, so a handler that is
// waiting on a user must honor it.
type ApprovalHandler interface {
	// ApproveCommand decides whether a command may run.
	ApproveCommand(ctx context.Context, req *CommandApprovalRequest) (Decision, error)
	// ApproveFileChange decides whether proposed edits may be applied.
	ApproveFileChange(ctx context.Context, req *FileChangeApprovalRequest) (Decision, error)
}

// PermissionApprover is an optional ApprovalHandler extension for the
// request_permissions tool.
type PermissionApprover interface {
	ApprovePermissions(ctx context.Context, req *PermissionsRequest) (*PermissionsResponse, error)
}

// UserInputResponder is an optional ApprovalHandler extension for
// item/tool/requestUserInput. The returned value is marshaled as the JSON-RPC
// result.
type UserInputResponder interface {
	RequestUserInput(ctx context.Context, params jsontext.Value) (any, error)
}

// TokenRefresher is an optional ApprovalHandler extension for hosts that own
// the ChatGPT auth lifecycle.
type TokenRefresher interface {
	RefreshChatGPTTokens(ctx context.Context, req *TokenRefreshRequest) (*ChatGPTAuthTokens, error)
}

// ServerRequestHandler is an optional ApprovalHandler extension that answers
// every server request the client does not model, such as
// MethodMcpElicitation or attestation/generate, and MethodDynamicToolCall
// when the handler is no DynamicToolHandler. The returned value is
// marshaled as the JSON-RPC result. Without it, MCP elicitations are
// declined and other requests fail with method-not-found.
type ServerRequestHandler interface {
	HandleServerRequest(ctx context.Context, method string, params jsontext.Value) (any, error)
}

// ApprovalFuncs adapts plain functions to ApprovalHandler. A nil field falls
// back to the default behavior for that request.
type ApprovalFuncs struct {
	Command      func(ctx context.Context, req *CommandApprovalRequest) (Decision, error)
	FileChange   func(ctx context.Context, req *FileChangeApprovalRequest) (Decision, error)
	Permissions  func(ctx context.Context, req *PermissionsRequest) (*PermissionsResponse, error)
	UserInput    func(ctx context.Context, params jsontext.Value) (any, error)
	TokenRefresh func(ctx context.Context, req *TokenRefreshRequest) (*ChatGPTAuthTokens, error)
	// DynamicTool answers MethodDynamicToolCall; nil passes it to Other,
	// whose result is read as a DynamicToolCallResponse.
	DynamicTool func(ctx context.Context, req *DynamicToolCallRequest) (*DynamicToolCallResponse, error)
	Other       func(ctx context.Context, method string, params jsontext.Value) (any, error)
}

// ApproveCommand implements ApprovalHandler.
func (f ApprovalFuncs) ApproveCommand(ctx context.Context, req *CommandApprovalRequest) (Decision, error) {
	if f.Command == nil {
		return DecisionDecline, nil
	}
	return f.Command(ctx, req)
}

// ApproveFileChange implements ApprovalHandler.
func (f ApprovalFuncs) ApproveFileChange(ctx context.Context, req *FileChangeApprovalRequest) (Decision, error) {
	if f.FileChange == nil {
		return DecisionDecline, nil
	}
	return f.FileChange(ctx, req)
}

// ApprovePermissions implements PermissionApprover.
func (f ApprovalFuncs) ApprovePermissions(ctx context.Context, req *PermissionsRequest) (*PermissionsResponse, error) {
	if f.Permissions == nil {
		return grantNothing(), nil
	}
	return f.Permissions(ctx, req)
}

// RequestUserInput implements UserInputResponder.
func (f ApprovalFuncs) RequestUserInput(ctx context.Context, params jsontext.Value) (any, error) {
	if f.UserInput == nil {
		return defaultServerRequest(MethodItemRequestUserInput)
	}
	return f.UserInput(ctx, params)
}

// RefreshChatGPTTokens implements TokenRefresher.
func (f ApprovalFuncs) RefreshChatGPTTokens(ctx context.Context, req *TokenRefreshRequest) (*ChatGPTAuthTokens, error) {
	if f.TokenRefresh == nil {
		_, err := defaultServerRequest(MethodChatGPTTokenRefresh)
		return nil, err
	}
	return f.TokenRefresh(ctx, req)
}

// HandleServerRequest implements ServerRequestHandler.
func (f ApprovalFuncs) HandleServerRequest(ctx context.Context, method string, params jsontext.Value) (any, error) {
	if f.Other == nil {
		return defaultServerRequest(method)
	}
	return f.Other(ctx, method, params)
}

// funcsOf returns the ApprovalFuncs that answer requests as h does. Keep it
// in step with the optional ApprovalHandler extensions.
func funcsOf(h ApprovalHandler) ApprovalFuncs {
	if f, ok := h.(ApprovalFuncs); ok || h == nil {
		return f
	}
	f := ApprovalFuncs{Command: h.ApproveCommand, FileChange: h.ApproveFileChange}
	if x, ok := h.(PermissionApprover); ok {
		f.Permissions = x.ApprovePermissions
	}
	if x, ok := h.(UserInputResponder); ok {
		f.UserInput = x.RequestUserInput
	}
	if x, ok := h.(TokenRefresher); ok {
		f.TokenRefresh = x.RefreshChatGPTTokens
	}
	if x, ok := h.(DynamicToolHandler); ok {
		f.DynamicTool = x.CallDynamicTool
	}
	if x, ok := h.(ServerRequestHandler); ok {
		f.Other = x.HandleServerRequest
	}
	return f
}

// defaultServerRequest answers a server request no handler takes: MCP
// elicitations are declined so the tool call fails cleanly, and anything else
// is method-not-found.
func defaultServerRequest(method string) (any, error) {
	message := "codex: unhandled server request " + method
	switch method {
	case MethodMcpElicitation:
		return map[string]string{"action": "decline"}, nil
	case MethodItemRequestUserInput:
		message = "codex: no user input handler registered"
	case MethodChatGPTTokenRefresh:
		message = "codex: no token refresh handler registered"
	}
	return nil, &RPCError{Code: CodeMethodNotFound, Message: message}
}

// decisionResult is the JSON-RPC result of an approval request.
type decisionResult struct {
	Decision Decision `json:"decision"`
}

// fileChangeCache keeps the changes of the file change items in progress,
// by turn and item, for their approval requests.
type fileChangeCache struct {
	mu     sync.Mutex
	byTurn map[string]map[string][]FileChange
}

// observe records the file change of an item/started notification, and
// forgets it on item/completed or when its turn completes. It runs on the
// transport reader, so it decodes only the items that can be file changes.
func (c *fileChangeCache) observe(method string, params jsontext.Value) {
	if method == MethodTurnCompleted {
		threadID, turnID := routeIDs(params)
		c.mu.Lock()
		delete(c.byTurn, turnKey(threadID, turnID))
		c.mu.Unlock()
		return
	}
	if !bytes.Contains(params, []byte(`"fileChange"`)) {
		return
	}
	var p ItemParams
	if err := jsonx.Unmarshal(params, &p); err != nil {
		return
	}
	item, ok := p.Item.Item.(*FileChangeItem)
	if !ok {
		return
	}
	key := turnKey(p.ThreadID, p.TurnID)
	c.mu.Lock()
	defer c.mu.Unlock()
	if method == MethodItemCompleted {
		delete(c.byTurn[key], item.ID)
		return
	}
	if c.byTurn == nil {
		c.byTurn = make(map[string]map[string][]FileChange)
	}
	if c.byTurn[key] == nil {
		c.byTurn[key] = make(map[string][]FileChange)
	}
	c.byTurn[key][item.ID] = item.Changes
}

// get returns the changes of an item, or nil.
func (c *fileChangeCache) get(threadID, turnID, itemID string) []FileChange {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.byTurn[turnKey(threadID, turnID)][itemID]
}

// pendingRequests tracks in-flight server requests so their handler contexts
// can be canceled when the owning turn ends.
type pendingRequests struct {
	mu     sync.Mutex
	next   int
	byTurn map[string]map[int]context.CancelFunc
}

func newPendingRequests() *pendingRequests {
	return &pendingRequests{byTurn: make(map[string]map[int]context.CancelFunc)}
}

// turnKey identifies a turn for pending-request bookkeeping.
func turnKey(threadID, turnID string) string { return threadID + "\x00" + turnID }

// add registers a cancel function and returns its release func.
func (p *pendingRequests) add(key string, cancel context.CancelFunc) func() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.next++
	id := p.next
	entries := p.byTurn[key]
	if entries == nil {
		entries = make(map[int]context.CancelFunc)
		p.byTurn[key] = entries
	}
	entries[id] = cancel
	return func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		if entries, ok := p.byTurn[key]; ok {
			delete(entries, id)
			if len(entries) == 0 {
				delete(p.byTurn, key)
			}
		}
	}
}

// cancelTurn cancels every handler context bound to a turn.
func (p *pendingRequests) cancelTurn(key string) {
	p.mu.Lock()
	entries := p.byTurn[key]
	delete(p.byTurn, key)
	p.mu.Unlock()
	for _, cancel := range entries {
		cancel()
	}
}

// cancelAll cancels every pending handler context.
func (p *pendingRequests) cancelAll() {
	p.mu.Lock()
	all := p.byTurn
	p.byTurn = make(map[string]map[int]context.CancelFunc)
	p.mu.Unlock()
	for _, entries := range all {
		for _, cancel := range entries {
			cancel()
		}
	}
}

// handleServerRequest answers a server-initiated request. Unknown methods are
// answered with a method-not-found error so a turn fails closed instead of
// hanging, and approvals default to declining when no handler is registered.
func (c *Client) handleServerRequest(ctx context.Context, method string, params jsontext.Value) (any, error) {
	threadID, turnID := routeIDs(params)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	release := c.pending.add(turnKey(threadID, turnID), cancel)
	defer release()

	switch method {
	case MethodCommandApproval:
		req := &CommandApprovalRequest{}
		if err := jsonx.Unmarshal(params, req); err != nil {
			return nil, &RPCError{Code: CodeInvalidParams, Message: err.Error()}
		}
		req.Params = params
		decision := DecisionDecline
		if c.opts.Approvals != nil {
			var err error
			if decision, err = c.opts.Approvals.ApproveCommand(ctx, req); err != nil {
				return nil, err
			}
		}
		return decisionResult{Decision: decision}, nil

	case MethodFileChangeApproval:
		req := &FileChangeApprovalRequest{}
		if err := jsonx.Unmarshal(params, req); err != nil {
			return nil, &RPCError{Code: CodeInvalidParams, Message: err.Error()}
		}
		req.Params = params
		req.Changes = c.fileChanges.get(req.ThreadID, req.TurnID, req.ItemID)
		decision := DecisionDecline
		if c.opts.Approvals != nil {
			var err error
			if decision, err = c.opts.Approvals.ApproveFileChange(ctx, req); err != nil {
				return nil, err
			}
		}
		return decisionResult{Decision: decision}, nil

	case MethodPermissionsApproval:
		req := &PermissionsRequest{}
		if err := jsonx.Unmarshal(params, req); err != nil {
			return nil, &RPCError{Code: CodeInvalidParams, Message: err.Error()}
		}
		req.Params = params
		if approver, ok := c.opts.Approvals.(PermissionApprover); ok {
			return approver.ApprovePermissions(ctx, req)
		}
		// Fail closed: grant nothing.
		return grantNothing(), nil

	case MethodItemRequestUserInput:
		if responder, ok := c.opts.Approvals.(UserInputResponder); ok {
			return responder.RequestUserInput(ctx, params)
		}
		return defaultServerRequest(MethodItemRequestUserInput)

	case MethodChatGPTTokenRefresh:
		if refresher, ok := c.opts.Approvals.(TokenRefresher); ok {
			req := &TokenRefreshRequest{}
			if err := jsonx.Unmarshal(params, req); err != nil {
				return nil, &RPCError{Code: CodeInvalidParams, Message: err.Error()}
			}
			req.Params = params
			return refresher.RefreshChatGPTTokens(ctx, req)
		}
		_, err := defaultServerRequest(MethodChatGPTTokenRefresh)
		return nil, err

	case MethodDynamicToolCall:
		if handler, ok := c.opts.Approvals.(DynamicToolHandler); ok {
			return callDynamicTool(ctx, handler.CallDynamicTool, params)
		}
		return c.otherServerRequest(ctx, method, params)

	default:
		return c.otherServerRequest(ctx, method, params)
	}
}

// otherServerRequest answers a request the client does not model.
func (c *Client) otherServerRequest(ctx context.Context, method string, params jsontext.Value) (any, error) {
	if handler, ok := c.opts.Approvals.(ServerRequestHandler); ok {
		return handler.HandleServerRequest(ctx, method, params)
	}
	return defaultServerRequest(method)
}
