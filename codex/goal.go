package codex

import "context"

// Thread goal statuses.
const (
	GoalActive        = "active"
	GoalPaused        = "paused"
	GoalBlocked       = "blocked"
	GoalUsageLimited  = "usageLimited"
	GoalBudgetLimited = "budgetLimited"
	GoalComplete      = "complete"
)

// ThreadGoal is a thread's persisted objective. While a goal is active the
// server keeps starting turns toward it until its status leaves GoalActive.
type ThreadGoal struct {
	ThreadID  string `json:"threadId"`
	Objective string `json:"objective"`
	// Status is one of the Goal* constants.
	Status          string `json:"status"`
	TokenBudget     *int64 `json:"tokenBudget,omitempty"`
	TokensUsed      int64  `json:"tokensUsed"`
	TimeUsedSeconds int64  `json:"timeUsedSeconds"`
	CreatedAt       int64  `json:"createdAt"`
	UpdatedAt       int64  `json:"updatedAt"`
}

// SetThreadGoalParams are the parameters of thread/goal/set. Zero fields keep
// the stored value.
type SetThreadGoalParams struct {
	ThreadID    string `json:"threadId"`
	Objective   string `json:"objective,omitempty"`
	Status      string `json:"status,omitempty"`
	TokenBudget *int64 `json:"tokenBudget,omitempty"`
}

// SetThreadGoal creates or updates a thread's goal and returns it. Setting
// an active goal on an idle thread makes the server start turns toward it;
// their events arrive as turn notifications the client did not start.
func (c *Client) SetThreadGoal(ctx context.Context, params SetThreadGoalParams) (*ThreadGoal, error) {
	var result struct {
		Goal ThreadGoal `json:"goal"`
	}
	if err := c.call(ctx, "thread/goal/set", params, &result); err != nil {
		return nil, err
	}
	return &result.Goal, nil
}

// ThreadGoal returns a thread's goal, or nil when it has none.
func (c *Client) ThreadGoal(ctx context.Context, threadID string) (*ThreadGoal, error) {
	var result struct {
		Goal *ThreadGoal `json:"goal"`
	}
	if err := c.call(ctx, "thread/goal/get", ThreadIDParams{ThreadID: threadID}, &result); err != nil {
		return nil, err
	}
	return result.Goal, nil
}

// ClearThreadGoal removes a thread's goal and reports whether one existed.
func (c *Client) ClearThreadGoal(ctx context.Context, threadID string) (bool, error) {
	var result struct {
		Cleared bool `json:"cleared"`
	}
	if err := c.call(ctx, "thread/goal/clear", ThreadIDParams{ThreadID: threadID}, &result); err != nil {
		return false, err
	}
	return result.Cleared, nil
}
