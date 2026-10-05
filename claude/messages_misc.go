package claude

// Top-level messages other than user, assistant, system, result, stream_event,
// rate_limit_event and conversation_reset, plus the structured payloads some
// messages carry.

// ToolProgressMessage (type "tool_progress") reports that a tool call is still
// running.
type ToolProgressMessage struct {
	ToolUseID       string `json:"tool_use_id"`
	ToolName        string `json:"tool_name"`
	ParentToolUseID string `json:"parent_tool_use_id,omitempty"`
	// ElapsedTimeSeconds is how long the tool has been running.
	ElapsedTimeSeconds float64 `json:"elapsed_time_seconds"`
	TaskID             string  `json:"task_id,omitempty"`
	// Heartbeat marks a keep-alive tick rather than new progress.
	Heartbeat    bool   `json:"heartbeat,omitzero"`
	SubagentType string `json:"subagent_type,omitempty"`
	// SubagentRetry is set while a subagent waits to retry a failed API call.
	SubagentRetry *SubagentRetry `json:"subagent_retry,omitzero"`
	UUID          string         `json:"uuid"`
	SessionID     string         `json:"session_id"`
}

func (*ToolProgressMessage) isMessage() {}

// SubagentRetry details a subagent's pending API retry.
type SubagentRetry struct {
	AgentID      string `json:"agent_id"`
	Attempt      int    `json:"attempt"`
	MaxRetries   int    `json:"max_retries"`
	RetryDelayMS int    `json:"retry_delay_ms"`
	// ErrorStatus is the HTTP status; nil for connection errors.
	ErrorStatus   *int   `json:"error_status"`
	ErrorCategory string `json:"error_category"`
}

// ToolUseSummaryMessage (type "tool_use_summary") summarizes a run of tool
// calls.
type ToolUseSummaryMessage struct {
	Summary             string   `json:"summary"`
	PrecedingToolUseIDs []string `json:"preceding_tool_use_ids"`
	UUID                string   `json:"uuid"`
	SessionID           string   `json:"session_id"`
}

func (*ToolUseSummaryMessage) isMessage() {}

// AuthStatusMessage (type "auth_status") reports authentication progress.
type AuthStatusMessage struct {
	IsAuthenticating bool     `json:"isAuthenticating"`
	Output           []string `json:"output"`
	Error            string   `json:"error,omitempty"`
	UUID             string   `json:"uuid"`
	SessionID        string   `json:"session_id"`
}

func (*AuthStatusMessage) isMessage() {}

// PromptSuggestionMessage (type "prompt_suggestion") is a predicted next user
// prompt, emitted after each turn when prompt suggestions are enabled.
type PromptSuggestionMessage struct {
	Suggestion string `json:"suggestion"`
	UUID       string `json:"uuid"`
	SessionID  string `json:"session_id"`
}

func (*PromptSuggestionMessage) isMessage() {}

// ActiveGoalMessage (type "active_goal") reports a change to the /goal set
// for the session. Value is nil when the goal was cleared.
type ActiveGoalMessage struct {
	Value     *ActiveGoal `json:"value"`
	UUID      string      `json:"uuid"`
	SessionID string      `json:"session_id"`
}

func (*ActiveGoalMessage) isMessage() {}

// ActiveGoal is the session's current /goal.
type ActiveGoal struct {
	Condition string `json:"condition"`
	// Iterations counts the Stop-hook checks that found the goal not yet
	// met.
	Iterations    int   `json:"iterations"`
	SetAt         int64 `json:"set_at"`
	TokensAtStart int   `json:"tokens_at_start"`
	// LastReason is why the last check found the goal not yet met.
	LastReason string `json:"last_reason,omitempty"`
}

// UnknownMessage is a top-level message type this SDK version does not model,
// kept so a newer CLI loses nothing. Raw is the full frame, including its
// "type" key.
type UnknownMessage struct {
	// Type is the wire discriminator; empty when the frame has none.
	Type string
	// Raw is the full message object.
	Raw map[string]any
}

func (*UnknownMessage) isMessage() {}
