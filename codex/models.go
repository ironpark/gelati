package codex

import "context"

// Model is one entry of model/list.
type Model struct {
	// ID is the catalog id; Model is the name to pass as a model override.
	ID          string `json:"id"`
	Model       string `json:"model"`
	DisplayName string `json:"displayName"`
	Description string `json:"description"`
	Hidden      bool   `json:"hidden"`
	IsDefault   bool   `json:"isDefault"`
	// DefaultReasoningEffort is the effort used when a turn sets none.
	DefaultReasoningEffort string `json:"defaultReasoningEffort"`
	// SupportedReasoningEfforts lists the efforts the model accepts.
	SupportedReasoningEfforts []ReasoningEffortOption `json:"supportedReasoningEfforts"`
	// InputModalities lists accepted inputs, such as "text" and "image".
	InputModalities []string `json:"inputModalities,omitempty"`
	// Upgrade names the model this one should be upgraded to, if any.
	Upgrade string `json:"upgrade,omitempty"`
}

// ReasoningEffortOption is one reasoning effort a model supports.
type ReasoningEffortOption struct {
	ReasoningEffort string `json:"reasoningEffort"`
	Description     string `json:"description,omitempty"`
}

// ListModelsParams are the parameters of model/list.
type ListModelsParams struct {
	Cursor string `json:"cursor,omitempty"`
	Limit  int    `json:"limit,omitzero"`
	// IncludeHidden also lists models hidden from the default picker.
	IncludeHidden bool `json:"includeHidden,omitzero"`
}

// ListModelsResult is one page of model/list results.
type ListModelsResult struct {
	Data []Model `json:"data"`
	// NextCursor is empty on the final page.
	NextCursor string `json:"nextCursor,omitempty"`
}

// ListModels returns one page of the models Codex can use.
func (c *Client) ListModels(ctx context.Context, params ListModelsParams) (*ListModelsResult, error) {
	var result ListModelsResult
	if err := c.tr.Call(ctx, "model/list", params, &result); err != nil {
		return nil, err
	}
	return &result, nil
}
