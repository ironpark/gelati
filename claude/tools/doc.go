// Package tools provides typed Go structs for the inputs and outputs of
// Claude Code's built-in tools, mirroring sdk-tools.d.ts from the TypeScript
// Claude Agent SDK (version SchemaVersion).
//
// The claude package exposes tool inputs and results as map[string]any
// (ToolUseBlock.Input, the CanUseTool input, PermissionResultAllow.UpdatedInput,
// UserMessage.ToolUseResult and hook tool_input/tool_response). This package
// converts between those maps and typed values; it does not import claude, so
// the two are combined by tool name.
//
// # Decoding tool calls
//
// DecodeInput dispatches on the tool name and returns the concrete input
// struct (a value, not a pointer):
//
//	for _, block := range msg.Content {
//		tu, ok := block.(*claude.ToolUseBlock)
//		if !ok {
//			continue
//		}
//		in, err := tools.DecodeInput(tu.Name, tu.Input)
//		if err != nil {
//			continue // errors.Is(err, tools.ErrUnknownTool) for tools without a schema
//		}
//		switch in := in.(type) {
//		case tools.BashInput:
//			fmt.Println("$", in.Command)
//		case tools.FileEditInput:
//			fmt.Println("edit", in.FilePath)
//		case tools.McpInput: // any "mcp__<server>__<tool>" call
//		}
//	}
//
// When the tool is already known, As decodes directly:
//
//	in, err := tools.As[tools.BashInput](tu.Input)
//
// DecodeOutput does the same for structured results such as
// UserMessage.ToolUseResult. Outputs that are discriminated unions in the
// schema (AgentOutput, FileReadOutput, ProjectsOutput) decode to one of their
// variant structs, e.g. AgentOutputCompleted.
//
// # Rewriting input in a permission callback
//
// Decode, modify, and encode back with ToMap for PermissionResultAllow:
//
//	opts.CanUseTool = func(ctx context.Context, name string, input map[string]any, _ claude.ToolPermissionContext) (claude.PermissionResult, error) {
//		if name != tools.Bash {
//			return &claude.PermissionResultAllow{}, nil
//		}
//		in, err := tools.As[tools.BashInput](input)
//		if err != nil {
//			return nil, err
//		}
//		in.Timeout = new(60_000)
//		updated, err := tools.ToMap(in)
//		if err != nil {
//			return nil, err
//		}
//		return &claude.PermissionResultAllow{UpdatedInput: updated}, nil
//	}
//
// Decoding never rejects unknown properties, so newer CLI versions keep
// working; note that properties absent from the schema are dropped when a
// struct is re-encoded, except for schema objects that allow additional
// properties (their Extra field) and McpInput, which is a plain map.
//
// # Type mapping
//
// The Go code in zz_generated.go is generated from sdk-tools.d.ts by
// internal/gen (see the go:generate directive in registry.go). The mapping is:
//
//   - JSON property names are kept verbatim in json tags; Go field names are
//     their PascalCase form with common initialisms (ID, URL, ...). Grep's
//     flag-style properties use descriptive names ("-A" is After, "-B"
//     Before, "-C" ContextAlias, "-n" LineNumbers, "-i" CaseInsensitive,
//     "-o" OnlyMatching).
//   - Required properties have no omitempty. Optional strings, slices and
//     maps use omitempty (absent and empty are not distinguished); optional
//     booleans, numbers and objects are pointers.
//   - "T | null" is *T; a required nullable property encodes as null when nil.
//   - Unions of string literals are named string types with constants (e.g.
//     GrepOutputMode); single literals are plain strings documented with
//     their value.
//   - TypeScript number has no integer/float distinction, so number is int
//     for properties known to hold integers (counts, sizes, line numbers,
//     token counts, HTTP codes, integer inputs such as Bash timeout) and
//     float64 otherwise, notably for durations and timestamps.
//   - unknown is any; "{[k: string]: T}" is map[string]T. Objects that also
//     declare properties keep the undeclared ones in an Extra map.
//   - Tuples produced from @minItems/@maxItems are slices; the bounds are
//     noted in the field documentation.
//   - Unions of objects discriminated by a literal property become a sealed
//     interface with one struct per variant and an UnmarshalX function.
//     Other unions of objects are merged into one struct whose properties
//     are all optional. Unions of different JSON kinds (McpOutput,
//     WebSearchResult) become a struct with one field per kind (String,
//     Number, Bool, Array, Object), exactly one of which is set.
//   - Fields documented as deprecated carry a "Deprecated:" note but are
//     kept, so that re-encoding an input does not drop them.
//
// Tool names are not part of sdk-tools.d.ts; the constants in this package
// (Bash, Read, Edit, ...) were derived from the schema type names, the
// TypeScript SDK's legacy-alias table and the Claude Code CLI. Canonical
// resolves legacy names such as Task (Agent) and KillShell (TaskStop).
package tools
