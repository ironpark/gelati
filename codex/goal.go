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
	TokenBudget     *int64 `json:"tokenBudget,omitzero"`
	TokensUsed      int64  `json:"tokensUsed"`
	TimeUsedSeconds int64  `json:"timeUsedSeconds"`
	CreatedAt       int64  `json:"createdAt"`
	UpdatedAt       int64  `json:"updatedAt"`
}

// SetGoalParams are the parameters of Thread.SetGoal (thread/goal/set). Zero
// fields keep the stored value.
type SetGoalParams struct {
	Objective   string `json:"objective,omitempty"`
	Status      string `json:"status,omitempty"`
	TokenBudget *int64 `json:"tokenBudget,omitzero"`
}

// SetGoal creates or updates the thread's goal and returns it. Setting an
// active goal on an idle thread makes the server start turns toward it; their
// events arrive as turn notifications the client did not start.
func (t *Thread) SetGoal(ctx context.Context, params SetGoalParams) (*ThreadGoal, error) {
	wire := struct {
		ThreadID string `json:"threadId"`
		SetGoalParams
	}{ThreadID: t.ID(), SetGoalParams: params}
	var result struct {
		Goal ThreadGoal `json:"goal"`
	}
	if err := t.client.tr.Call(ctx, "thread/goal/set", wire, &result); err != nil {
		return nil, err
	}
	return &result.Goal, nil
}

// Goal returns the thread's goal, or nil when it has none.
func (t *Thread) Goal(ctx context.Context) (*ThreadGoal, error) {
	var result struct {
		Goal *ThreadGoal `json:"goal"`
	}
	if err := t.client.tr.Call(ctx, "thread/goal/get", t.idParams(), &result); err != nil {
		return nil, err
	}
	return result.Goal, nil
}

// ClearGoal removes the thread's goal and reports whether one existed.
func (t *Thread) ClearGoal(ctx context.Context) (bool, error) {
	var result struct {
		Cleared bool `json:"cleared"`
	}
	if err := t.client.tr.Call(ctx, "thread/goal/clear", t.idParams(), &result); err != nil {
		return false, err
	}
	return result.Cleared, nil
}
