package claude

// toolPermissionContext builds the CanUseTool context from a can_use_tool
// request, as the TypeScript SDK maps it.
func toolPermissionContext(requestID string, request map[string]any) ToolPermissionContext {
	permCtx := ToolPermissionContext{
		ToolUseID:               str(request["tool_use_id"]),
		AgentID:                 str(request["agent_id"]),
		BlockedPath:             str(request["blocked_path"]),
		DecisionReason:          str(request["decision_reason"]),
		Title:                   str(request["title"]),
		DisplayName:             str(request["display_name"]),
		Description:             str(request["description"]),
		DefaultToNo:             request["default_to_no"] == true,
		SuppressAlwaysAllowRule: request["suppress_always_allow_rule"] == true,
		RequestID:               requestID,
		Raw:                     request,
	}
	if suggestions, ok := request["permission_suggestions"].([]any); ok {
		for _, s := range suggestions {
			var update PermissionUpdate
			if m, ok := s.(map[string]any); ok && decodeResponse(m, &update) == nil {
				permCtx.Suggestions = append(permCtx.Suggestions, update)
			}
		}
	}
	if server, ok := request["mcp_server"].(map[string]any); ok {
		permCtx.MCPServer = &MCPServerProvenance{Name: str(server["name"]), Source: str(server["source"])}
	}
	if b, ok := request["requires_user_interaction"].(bool); ok {
		permCtx.RequiresUserInteraction = &b
	}
	if rule, ok := request["matched_ask_rule"].(map[string]any); ok {
		matched := &MatchedAskRule{Source: str(rule["source"]), ToolName: str(rule["tool_name"])}
		if content, ok := rule["rule_content"].(string); ok {
			matched.RuleContent = &content
		}
		permCtx.MatchedAskRule = matched
	}
	return permCtx
}

// stampPermissionReply adds the fields the TypeScript SDK puts on every
// permission reply: the request's tool_use_id echoed as toolUseID, and the
// optional decision classification.
func stampPermissionReply(out, request map[string]any, classification PermissionDecisionClassification) {
	if id, ok := request["tool_use_id"].(string); ok {
		out["toolUseID"] = id
	}
	if classification != "" {
		out["decisionClassification"] = classification
	}
}
