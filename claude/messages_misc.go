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
	Heartbeat    bool   `json:"heartbeat,omitempty"`
	SubagentType string `json:"subagent_type,omitempty"`
	// SubagentRetry is set while a subagent waits to retry a failed API call.
	SubagentRetry *SubagentRetry `json:"subagent_retry,omitempty"`
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

// ResultTiming carries the latency measurements on a success result. All
// durations are in milliseconds; a zero value means "not reported".
type ResultTiming struct {
	TTFTMS                      float64 `json:"ttft_ms,omitempty"`
	TTFTStreamMS                float64 `json:"ttft_stream_ms,omitempty"`
	TimeToRequestMS             float64 `json:"time_to_request_ms,omitempty"`
	RequestSentWallMS           float64 `json:"request_sent_wall_ms,omitempty"`
	FirstContentFrameMS         float64 `json:"first_content_frame_ms,omitempty"`
	FirstStreamPostMS           float64 `json:"first_stream_post_ms,omitempty"`
	FirstStreamPostAckMS        float64 `json:"first_stream_post_ack_ms,omitempty"`
	FirstStreamPostQueueWaitMS  float64 `json:"first_stream_post_queue_wait_ms,omitempty"`
	FirstStreamPostQueuedBehind string  `json:"first_stream_post_queued_behind,omitempty"`
	FirstStreamPostWallMS       float64 `json:"first_stream_post_wall_ms,omitempty"`
	FirstTextPostMS             float64 `json:"first_text_post_ms,omitempty"`
	FirstTextPostWallMS         float64 `json:"first_text_post_wall_ms,omitempty"`
	TimeToRequestFromSpawnMS    float64 `json:"time_to_request_from_spawn_ms,omitempty"`
	WarmSpareClaimed            bool    `json:"warm_spare_claimed,omitempty"`
	TimeOriginMS                float64 `json:"time_origin_ms,omitempty"`
}

// ContextUsageReport is the structured twin of a /context report, carried on
// AssistantMessage.ContextUsage.
type ContextUsageReport struct {
	// Model is the main-loop model the usage was computed for.
	Model string `json:"model"`
	// TotalTokens is the estimated usage; it may exceed RawMaxTokens.
	TotalTokens  int     `json:"total_tokens"`
	RawMaxTokens int     `json:"raw_max_tokens"`
	Percentage   float64 `json:"percentage"`
	// OverLimit is set when TotalTokens exceeds RawMaxTokens.
	OverLimit   *ContextReportOverLimit   `json:"over_limit,omitempty"`
	Categories  []ContextReportCategory   `json:"categories"`
	MCPTools    []ContextReportMCPTool    `json:"mcp_tools"`
	MemoryFiles []ContextReportMemoryFile `json:"memory_files"`
	Agents      []ContextReportAgent      `json:"agents"`
	Skills      []ContextReportSkill      `json:"skills,omitempty"`
}

// ContextReportOverLimit says by how much a context window is exceeded.
type ContextReportOverLimit struct {
	TokensOver int `json:"tokens_over"`
	// Kind is "hard_limit" or "compaction_window".
	Kind string `json:"kind"`
}

// ContextReportCategory is one row of the usage-by-category breakdown.
type ContextReportCategory struct {
	Name   string `json:"name"`
	Tokens int    `json:"tokens"`
	// Kind is used, free, buffer or deferred; classify rows by it.
	Kind string `json:"kind"`
}

// ContextReportMCPTool is the context cost of one MCP tool.
type ContextReportMCPTool struct {
	Name       string `json:"name"`
	ServerName string `json:"server_name"`
	Tokens     int    `json:"tokens"`
}

// ContextReportMemoryFile is the context cost of one memory file.
type ContextReportMemoryFile = ContextUsageMemoryFile

// ContextReportAgent is the context cost of one custom agent.
type ContextReportAgent struct {
	AgentType string `json:"agent_type"`
	Source    string `json:"source"`
	Tokens    int    `json:"tokens"`
}

// ContextReportSkill is the context cost of one skill.
type ContextReportSkill struct {
	Name       string `json:"name"`
	Source     string `json:"source"`
	PluginName string `json:"plugin_name,omitempty"`
	Tokens     int    `json:"tokens"`
}
