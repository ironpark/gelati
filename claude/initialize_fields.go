package claude

// initializeExtras returns every option-derived field of the initialize
// control request, following the TypeScript SDK's buildInitializeRequest:
// agents, skills, dialog kinds, the system prompt, the structured-output
// schema, plan-mode instructions, tool aliases, the session title, plugins
// delivered over stdin and the opt-in UX features. The engine adds only the
// hook registrations and the in-process MCP server declarations.
func initializeExtras(opts *Options) map[string]any {
	if opts == nil {
		opts = &Options{}
	}
	fields := map[string]any{}
	if len(opts.Agents) > 0 {
		fields["agents"] = opts.Agents
	}
	// "all" and "unset" are the same on the wire (no filter), so only an
	// explicit list is sent.
	if list, ok := opts.Skills.(SkillList); ok {
		fields["skills"] = []string(list)
	}
	if opts.ForwardSubagentText {
		fields["forwardSubagentText"] = true
	}
	if len(opts.SupportedDialogKinds) > 0 {
		fields["supportedDialogKinds"] = opts.SupportedDialogKinds
	}
	addSystemPromptFields(fields, opts.SystemPrompt)
	if schema, ok := outputSchema(opts); ok {
		fields["jsonSchema"] = schema
	}
	if opts.PlanModeInstructions != "" {
		fields["planModeInstructions"] = opts.PlanModeInstructions
	}
	if len(opts.ToolAliases) > 0 {
		fields["toolAliases"] = opts.ToolAliases
	}
	if opts.Title != "" {
		fields["title"] = opts.Title
	}
	if opts.PromptSuggestions {
		fields["promptSuggestions"] = true
	}
	if opts.AgentProgressSummaries {
		fields["agentProgressSummaries"] = true
	}
	if opts.PerTaskStopAffordance {
		fields["perTaskStopAffordance"] = true
	}
	if pluginsViaInitialize(opts) {
		plugins := make([]map[string]any, 0, len(opts.Plugins))
		for _, p := range opts.Plugins {
			plugin := map[string]any{"type": "local", "path": p.Path}
			if p.SkipMCPDiscovery {
				plugin["skipMcpDiscovery"] = true
			}
			plugins = append(plugins, plugin)
		}
		fields["plugins"] = plugins
	}
	return fields
}

// addSystemPromptFields renders Options.SystemPrompt the way the TypeScript
// SDK does. nil becomes an empty custom prompt; SystemPromptFile travels as
// --system-prompt-file instead.
func addSystemPromptFields(fields map[string]any, prompt SystemPrompt) {
	switch sp := prompt.(type) {
	case nil:
		fields["systemPrompt"] = []string{""}
	case SystemPromptText:
		fields["systemPrompt"] = []string{string(sp)}
	case SystemPromptBlocks:
		fields["systemPrompt"] = nonNilStrings(sp)
	case *SystemPromptCustom:
		fields["systemPrompt"] = nonNilStrings(sp.Prompt)
		if sp.Snapshot != nil {
			fields["systemPromptSnapshot"] = *sp.Snapshot
		}
	case *SystemPromptPreset:
		if sp.Append != "" {
			fields["appendSystemPrompt"] = sp.Append
		}
		if sp.ExcludeDynamicSections {
			fields["excludeDynamicSections"] = true
		}
		if sp.Snapshot != nil {
			fields["systemPromptSnapshot"] = *sp.Snapshot
		}
	}
}

// nonNilStrings keeps an empty prompt list encoding as [] rather than null.
func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// pluginsViaInitialize reports whether Options.Plugins travel in the
// initialize request (with --await-initialize) instead of --plugin-dir.
func pluginsViaInitialize(opts *Options) bool {
	return opts.PluginDelivery == PluginDeliveryInitialize && len(opts.Plugins) > 0
}
