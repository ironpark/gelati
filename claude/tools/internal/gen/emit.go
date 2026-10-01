package main

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"go/format"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// typeNames maps the TypeScript path of a nested type (as printed in the
// generated doc comments, e.g. "TodoWriteInput.todos[]") to the Go name to
// use instead of the automatically derived Parent+Field name. Shapes that are
// structurally identical share the first name they were generated under.
var typeNames = map[string]string{
	"AgentOutput<completed>.content[]":                   "AgentTextBlock",
	"AgentOutput<completed>.usage":                       "AgentUsage",
	"AgentOutput<completed>.toolStats":                   "AgentToolStats",
	"AgentOutput<completed>.usage.server_tool_use":       "AgentServerToolUse",
	"AgentOutput<completed>.usage.cache_creation":        "AgentCacheCreation",
	"AgentOutput<completed>.usage.output_tokens_details": "AgentOutputTokensDetails",
	"FileReadOutput<text>.file":                          "FileReadTextFile",
	"FileReadOutput<text>.artifactRead":                  "FileReadArtifactRead",
	"FileReadOutput<image>.file":                         "FileReadImageFile",
	"FileReadOutput<image>.file.type":                    "ImageMediaType",
	"FileReadOutput<image>.file.dimensions":              "ImageDimensions",
	"FileReadOutput<notebook>.file":                      "FileReadNotebookFile",
	"FileReadOutput<pdf>.file":                           "FileReadPDFFile",
	"FileReadOutput<parts>.file":                         "FileReadPartsFile",
	"FileReadOutput<parts>.pages[]":                      "FileReadPage",
	"FileReadOutput<file_unchanged>.file":                "FileReadUnchangedFile",
	"ListMcpResourcesOutput[]":                           "McpResource",
	"RefreshMcpToolsOutput[]":                            "McpToolsRefresh",
	"McpOutput<Array>[]":                                 "McpContentBlock",
	"ArtifactOutput.artifactRead":                        "ArtifactReadRef",
	"ArtifactOutput.artifacts[]":                         "ArtifactListEntry",
	"ArtifactOutput.watches[]":                           "ArtifactWatchEntry",
	"ArtifactOutput.arms[]":                              "ArtifactWatchArm",
	"ArtifactOutput.asset_uploads.results[]":             "ArtifactAssetUploadResult",
	"ArtifactOutput.asset_list.assets[]":                 "ArtifactAsset",
	"ArtifactOutput.asset_list.usage":                    "ArtifactAssetUsage",
	"ProjectsOutput<project_info>.docs[]":                "ProjectDoc",
	"ProjectsOutput<project_info>.files[]":               "ProjectFile",
	"ProjectsOutput<project_info>.sync_sources[]":        "ProjectSyncSource",
	"ProjectsOutput<project_info>.knowledge":             "ProjectKnowledge",
	"ProjectsOutput<project_search>.hits[]":              "ProjectSearchHit",
	"ProjectsOutput<project_memory_list>.files[]":        "ProjectMemoryFile",
	"AgentInput.model":                                   "AgentModel",
	"AgentInput.mode":                                    "AgentPermissionMode",
	"AgentInput.isolation":                               "AgentIsolation",
	"ExitPlanModeInput.allowedPrompts[]":                 "AllowedPrompt",
	"GrepInput.output_mode":                              "GrepOutputMode",
	"NotebookEditInput.cell_type":                        "NotebookCellType",
	"NotebookEditInput.edit_mode":                        "NotebookEditMode",
	"ReportFindingsInput.level":                          "ReviewLevel",
	"ReportFindingsInput.findings[]":                     "Finding",
	"TodoWriteInput.todos[]":                             "Todo",
	"AskUserQuestionInput.questions[]":                   "Question",
	"AskUserQuestionInput.annotations{}":                 "QuestionAnnotation",
	"AskUserQuestionInput.metadata":                      "QuestionMetadata",
	"SendFeedbackInput.type":                             "FeedbackType",
	"SendFeedbackInput.failure_mode":                     "FeedbackFailureMode",
	"SendFeedbackInput.task_category":                    "FeedbackTaskCategory",
	"ProjectsInput.method":                               "ProjectsMethod",
	"TaskUpdateInput.status":                             "TaskUpdateStatus",
	"RemoteTriggerInput.action":                          "RemoteTriggerAction",
	"MonitorInput.ws":                                    "MonitorWebSocket",
	"ProposeSkillsInput.proposals[]":                     "SkillProposal",
	"ArtifactInput.action":                               "ArtifactAction",
	"ArtifactInput.scope":                                "ArtifactListScope",
	"ExitWorktreeInput.action":                           "WorktreeExitAction",
	"BashOutput.gitOperation":                            "GitOperation",
	"BashOutput.gitOperation.pr":                         "GitPullRequest",
	"FileEditOutput.structuredPatch[]":                   "PatchHunk",
	"FileEditOutput.gitDiff":                             "GitDiff",
	"FileWriteOutput.type":                               "FileWriteType",
	"ReadMcpResourceDirOutput.resources[]":               "McpResourceDirEntry",
	"ReadMcpResourceOutput.contents[]":                   "McpResourceContent",
	"WebSearchOutput.results[]":                          "WebSearchResult",
	"WebSearchOutput.results[]<Object>":                  "WebSearchResultBlock",
	"WebSearchOutput.results[]<Object>.content[]":        "WebSearchHit",
	"TaskCreateOutput.task":                              "TaskRef",
	"TaskGetOutput.task":                                 "TaskDetail",
	"TaskUpdateOutput.statusChange":                      "TaskStatusChange",
	"TaskListOutput.tasks[]":                             "TaskSummary",
	"ReadNotificationsOutput.notifications[]":            "Notification",
	"WorkflowOutput.status":                              "WorkflowStatus",
	"WorkflowOutput.taskType":                            "WorkflowTaskType",
	"CronListOutput.jobs[]":                              "CronJob",
	"PushNotificationOutput.disabledReason":              "PushDisabledReason",
}

// fieldNames overrides Go field names derived from JSON property names.
var fieldNames = map[string]string{
	"-A": "After",
	"-B": "Before",
	"-C": "ContextAlias",
	"-n": "LineNumbers",
	"-i": "CaseInsensitive",
	"-o": "OnlyMatching",
}

// intFields lists JSON property names whose TypeScript `number` is known to
// be an integer (counts, sizes, line numbers, token counts, HTTP codes,
// integer-valued inputs). All other numbers — durations and timestamps in
// particular, which may be fractional — map to float64.
var intFields = setOf(
	// inputs
	"timeout", "offset", "limit", "-A", "-B", "-C", "context", "head_limit",
	"line", "n", "delaySeconds", "timeout_ms",
	// counts, sizes and positions
	"numLines", "startLine", "totalLines", "originalSize", "originalWidth",
	"originalHeight", "displayWidth", "displayHeight", "count", "firstPage",
	"toolCount", "numFiles", "totalMatches", "numMatches", "totalFiles",
	"appliedLimit", "appliedOffset", "oldStart", "oldLines", "newStart",
	"newLines", "additions", "deletions", "changes", "persistedOutputSize",
	"number", "remaining", "cancelledWakeups", "discardedFiles",
	"discardedCommits", "proposalCount", "clampedDelaySeconds", "searchCount",
	"bytes", "code", "status", "size_bytes", "failures", "max_failures",
	"files", "max_files", "max_bytes", "knowledge_size", "max_knowledge_size",
	"publishesRemaining", "seq",
	// agent usage
	"totalToolUseCount", "totalTokens", "input_tokens", "output_tokens",
	"cache_creation_input_tokens", "cache_read_input_tokens",
	"web_search_requests", "web_fetch_requests", "ephemeral_1h_input_tokens",
	"ephemeral_5m_input_tokens", "thinking_tokens", "readCount", "bashCount",
	"editFileCount", "linesAdded", "linesRemoved", "otherToolCount",
	"frameCount",
)

var initialisms = setOf("id", "url", "uri", "json", "uuid", "sha256", "pr", "ws", "http", "html", "api", "ui", "pdf")

func setOf(s ...string) map[string]bool {
	m := make(map[string]bool, len(s))
	for _, v := range s {
		m[v] = true
	}
	return m
}

var camelBoundary = regexp.MustCompile(`([a-z0-9])([A-Z])`)

// pascal converts a JSON name or literal value to an exported Go identifier.
func pascal(s string) string {
	s = camelBoundary.ReplaceAllString(s, "${1} ${2}")
	words := strings.FieldsFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9')
	})
	var b strings.Builder
	for _, w := range words {
		lw := strings.ToLower(w)
		switch {
		case initialisms[lw]:
			b.WriteString(strings.ToUpper(lw))
		case w == strings.ToUpper(w):
			b.WriteString(strings.ToUpper(lw[:1]) + lw[1:])
		default:
			b.WriteString(strings.ToUpper(w[:1]) + w[1:])
		}
	}
	out := b.String()
	if out == "" || out[0] >= '0' && out[0] <= '9' {
		out = "V" + out
	}
	return out
}

func goFieldName(json string) string {
	if n, ok := fieldNames[json]; ok {
		return n
	}
	return pascal(json)
}

func singular(s string) string {
	switch {
	case strings.HasSuffix(s, "ies"):
		return s[:len(s)-3] + "y"
	case strings.HasSuffix(s, "ches"), strings.HasSuffix(s, "shes"), strings.HasSuffix(s, "xes"):
		return s[:len(s)-2]
	case strings.HasSuffix(s, "ss"), strings.HasSuffix(s, "us"):
		return s + "Item"
	case strings.HasSuffix(s, "s"):
		return s[:len(s)-1]
	}
	return s + "Item"
}

type gen struct {
	body     bytes.Buffer
	pending  []func() error
	used     map[string]string // Go type name -> key it was generated for
	nested   map[string]string // structural key -> Go type name
	enums    map[string]bool   // Go names of generated string enums
	sealed   map[string]bool
	inputs   []string
	outputs  []string
	declared map[string]bool
	names    map[string]string // TypeScript path -> Go type name overrides
	applied  map[string]bool   // names entries that were used
	debug    bool              // GEN_DEBUG set: log naming decisions
}

// Generate converts sdk-tools.d.ts into Go source for package tools, using
// typeNames for nested type names. Every typeNames entry must be used, so that
// schema changes that orphan an override are noticed.
func Generate(src []byte, version string) ([]byte, error) {
	return generate(src, version, typeNames)
}

func generate(src []byte, version string, names map[string]string) ([]byte, error) {
	decls, err := Parse(string(src))
	if err != nil {
		return nil, err
	}
	g := &gen{
		used:     map[string]string{},
		nested:   map[string]string{},
		enums:    map[string]bool{},
		sealed:   map[string]bool{},
		declared: map[string]bool{},
		names:    names,
		applied:  map[string]bool{},
		debug:    os.Getenv("GEN_DEBUG") != "",
	}
	for _, d := range decls {
		g.declared[d.Name] = true
	}
	for _, d := range decls {
		switch d.Name {
		case "ToolInputSchemas", "ToolOutputSchemas":
			for _, m := range d.Type.Members {
				if m.Kind != KRef {
					return nil, fmt.Errorf("%s: non-reference member", d.Name)
				}
				if m.Ref == "ToolOutputSchemas" {
					continue
				}
				if d.Name == "ToolInputSchemas" {
					g.inputs = append(g.inputs, m.Ref)
				} else {
					g.outputs = append(g.outputs, m.Ref)
				}
			}
			continue
		}
		if err := g.decl(d); err != nil {
			return nil, fmt.Errorf("%s: %w", d.Name, err)
		}
		for len(g.pending) > 0 {
			f := g.pending[0]
			g.pending = g.pending[1:]
			if err := f(); err != nil {
				return nil, fmt.Errorf("%s: %w", d.Name, err)
			}
		}
	}
	var unused []string
	for k := range names {
		if !g.applied[k] {
			unused = append(unused, k)
		}
	}
	if len(unused) > 0 && !g.debug {
		sort.Strings(unused)
		return nil, fmt.Errorf("unused typeNames entries (schema changed?): %s", strings.Join(unused, ", "))
	}
	for _, n := range append(append([]string(nil), g.inputs...), g.outputs...) {
		if !g.declared[n] {
			return nil, fmt.Errorf("schema union references undeclared type %s", n)
		}
	}

	var out bytes.Buffer
	sum := sha256.Sum256(src)
	fmt.Fprintf(&out, "// Code generated by internal/gen from @anthropic-ai/claude-agent-sdk@%s sdk-tools.d.ts. DO NOT EDIT.\n", version)
	fmt.Fprintf(&out, "// Source SHA-256: %x\n\n", sum)
	out.WriteString("package tools\n\n")
	out.WriteString("import (\n\t\"encoding/json\"\n\t\"fmt\"\n\t\"reflect\"\n)\n\n")
	fmt.Fprintf(&out, "// SchemaVersion is the @anthropic-ai/claude-agent-sdk version whose\n// sdk-tools.d.ts this file was generated from.\nconst SchemaVersion = %q\n\n", version)
	out.WriteString("// schemaInputTypes lists the members of ToolInputSchemas in schema order.\nvar schemaInputTypes = []reflect.Type{\n")
	for _, n := range g.inputs {
		fmt.Fprintf(&out, "\treflect.TypeFor[%s](),\n", n)
	}
	out.WriteString("}\n\n// schemaOutputTypes lists the members of ToolOutputSchemas in schema order.\nvar schemaOutputTypes = []reflect.Type{\n")
	for _, n := range g.outputs {
		fmt.Fprintf(&out, "\treflect.TypeFor[%s](),\n", n)
	}
	out.WriteString("}\n\n")
	out.Write(g.body.Bytes())
	formatted, err := format.Source(out.Bytes())
	if err != nil {
		return out.Bytes(), fmt.Errorf("gofmt generated code: %w", err)
	}
	return formatted, nil
}

func (g *gen) printf(format string, args ...any) { fmt.Fprintf(&g.body, format, args...) }

// claim reserves a Go type name for a structural key.
func (g *gen) claim(name, k string) error {
	if prev, ok := g.used[name]; ok && prev != k && !g.debug {
		return fmt.Errorf("Go type name %s generated for two different shapes; add a typeNames entry", name)
	}
	g.used[name] = k
	return nil
}

func (g *gen) decl(d *Decl) error {
	t := normalize(d.Type)
	name := d.Name
	if err := g.claim(name, "decl:"+name); err != nil {
		return err
	}
	header := fmt.Sprintf("%s mirrors the %s schema of sdk-tools.d.ts.", name, name)
	doc := joinDoc(header, d.Doc)
	switch t.Kind {
	case KObject:
		if len(t.Props) == 0 && t.Index != nil {
			g.writeDoc("", doc)
			v, err := g.expr(t.Index, name+"Value", "", name+"{}")
			if err != nil {
				return err
			}
			g.printf("type %s map[string]%s\n\n", name, v)
			return nil
		}
		return g.structDecl(name, t, name, doc)
	case KArray:
		g.writeDoc("", doc)
		elem, err := g.expr(t.Elem, name+"Item", "", name+"[]")
		if err != nil {
			return err
		}
		g.printf("type %s []%s\n\n", name, elem)
		return nil
	case KUnion:
		allObj := true
		for _, m := range t.Members {
			if m.Kind != KObject {
				allObj = false
			}
		}
		if allObj {
			if disc, vals := discriminator(t.Members); disc != "" {
				return g.sealedDecl(name, t.Members, disc, vals, doc)
			}
			doc = joinDoc(doc, fmt.Sprintf("The schema is an untagged union of %d object shapes; their properties are merged here and only those of the received shape are set.", len(t.Members)))
			return g.structDecl(name, mergeObjects(t.Members), name, doc)
		}
		return g.kindedDecl(name, t.Members, name, doc)
	}
	return fmt.Errorf("unsupported top-level kind %d", t.Kind)
}

// expr returns the Go type expression for t, generating named types on
// demand. hint is the auto-derived name for any named type created; field is
// the JSON property name (used to choose int vs float64).
func (g *gen) expr(t *Type, hint, field, path string) (string, error) {
	switch t.Kind {
	case KString:
		return "string", nil
	case KNumber:
		return numType(field), nil
	case KBool:
		return "bool", nil
	case KUnknown, KNull:
		return "any", nil
	case KLiteral:
		switch t.Lit.(type) {
		case string:
			return "string", nil
		case bool:
			return "bool", nil
		default:
			return numType(field), nil
		}
	case KRef:
		if !g.declared[t.Ref] {
			return "", fmt.Errorf("reference to undeclared type %s", t.Ref)
		}
		return t.Ref, nil
	case KArray:
		e, err := g.expr(t.Elem, singular(hint), field, path+"[]")
		if err != nil {
			return "", err
		}
		return "[]" + e, nil
	case KTuple:
		return "[]any", nil // heterogeneous tuples do not occur in sdk-tools.d.ts
	case KObject:
		if len(t.Props) == 0 && t.Index != nil {
			v, err := g.expr(t.Index, hint+"Value", "", path+"{}")
			if err != nil {
				return "", err
			}
			return "map[string]" + v, nil
		}
		return g.nestedStruct(t, hint, path)
	case KUnion:
		inner, null := splitNull(t)
		if null {
			return g.expr(inner, hint, field, path)
		}
		if lits := stringLiterals(t); lits != nil {
			return g.enum(lits, hint, path)
		}
		allStr, allBool, allNum, allObj := true, true, true, true
		for _, m := range t.Members {
			allStr = allStr && isStringish(m)
			allBool = allBool && isBoolish(m)
			allNum = allNum && isNumberish(m)
			allObj = allObj && m.Kind == KObject
		}
		switch {
		case allStr:
			return "string", nil
		case allBool:
			return "bool", nil
		case allNum:
			return numType(field), nil
		case allObj:
			return g.nestedStruct(mergeObjects(t.Members), hint, path)
		}
		return g.nestedKinded(t.Members, hint, path)
	}
	return "", fmt.Errorf("unsupported kind %d", t.Kind)
}

func numType(field string) string {
	if intFields[field] {
		return "int"
	}
	return "float64"
}

func (g *gen) rename(hint, path string) string {
	if n, ok := g.names[path]; ok {
		g.applied[path] = true
		return n
	}
	return hint
}

func (g *gen) nestedStruct(t *Type, hint, path string) (string, error) {
	k := "struct:" + key(t)
	if n, ok := g.nested[k]; ok {
		return n, nil
	}
	name := g.rename(hint, path)
	if g.debug {
		fmt.Fprintf(os.Stderr, "%-40s %-50s %s\n", name, hint, path)
	}
	if err := g.claim(name, k); err != nil {
		return "", err
	}
	g.nested[k] = name
	g.pending = append(g.pending, func() error {
		return g.structDecl(name, t, path, fmt.Sprintf("%s is a nested object of the tool schemas (first seen at %s).", name, path))
	})
	return name, nil
}

func (g *gen) nestedKinded(members []*Type, hint, path string) (string, error) {
	k := "kinded:" + key(&Type{Kind: KUnion, Members: members})
	if n, ok := g.nested[k]; ok {
		return n, nil
	}
	name := g.rename(hint, path)
	if g.debug {
		fmt.Fprintf(os.Stderr, "%-40s %-50s %s\n", name, hint, path)
	}
	if err := g.claim(name, k); err != nil {
		return "", err
	}
	g.nested[k] = name
	g.pending = append(g.pending, func() error {
		return g.kindedDecl(name, members, path, fmt.Sprintf("%s is a nested value of the tool schemas (first seen at %s).", name, path))
	})
	return name, nil
}

func (g *gen) enum(lits []string, hint, path string) (string, error) {
	k := "enum:" + strings.Join(lits, "|")
	if n, ok := g.nested[k]; ok {
		return n, nil
	}
	name := g.rename(hint, path)
	if g.debug {
		fmt.Fprintf(os.Stderr, "%-40s %-50s %s\n", name, hint, path)
	}
	if err := g.claim(name, k); err != nil {
		return "", err
	}
	g.nested[k] = name
	g.enums[name] = true
	g.pending = append(g.pending, func() error {
		g.printf("// %s enumerates the values allowed by the schema (first seen at %s).\n", name, path)
		g.printf("type %s string\n\n", name)
		g.printf("// Values of %s.\nconst (\n", name)
		for _, l := range lits {
			g.printf("\t%s%s %s = %q\n", name, pascal(l), name, l)
		}
		g.printf(")\n\n")
		return nil
	})
	return name, nil
}

type field struct {
	goName, goType, tag, doc string
}

func (g *gen) fields(owner, path string, t *Type) ([]field, error) {
	var out []field
	seen := map[string]bool{}
	for _, p := range t.Props {
		if hasTag(p.Doc, "@deprecated") {
			continue
		}
		gn := goFieldName(p.Name)
		if seen[gn] || gn == "Extra" && t.Index != nil {
			return nil, fmt.Errorf("%s: duplicate Go field name %s", owner, gn)
		}
		seen[gn] = true
		inner, nullable := splitNull(p.Type)
		typ, err := g.expr(inner, owner+gn, p.Name, path+"."+p.Name)
		if err != nil {
			return nil, fmt.Errorf("%s.%s: %w", owner, p.Name, err)
		}
		ptr := false
		switch {
		case strings.HasPrefix(typ, "[]"), strings.HasPrefix(typ, "map["), typ == "any", g.sealed[typ]:
		case typ == "string", g.enums[typ]:
			ptr = nullable
		case inner.Kind == KLiteral && inner.Lit == true && !nullable:
			// optional `true` literal: presence is the signal.
		default:
			ptr = nullable || p.Optional
		}
		if ptr {
			typ = "*" + typ
		}
		tag := p.Name
		if p.Optional {
			tag += ",omitempty"
		}
		doc := fieldDoc(p)
		if inner.Kind == KLiteral && !nullable {
			lit := literalText(inner.Lit)
			if p.Optional {
				doc = joinDoc(doc, fmt.Sprintf("When present it is always %s.", lit))
			} else {
				doc = joinDoc(doc, fmt.Sprintf("Always %s.", lit))
			}
		}
		out = append(out, field{goName: gn, goType: typ, tag: tag, doc: doc})
	}
	return out, nil
}

func literalText(v any) string {
	switch v := v.(type) {
	case string:
		return strconv.Quote(v)
	case bool:
		return strconv.FormatBool(v)
	case float64:
		return strconv.FormatFloat(v, 'g', -1, 64)
	}
	return fmt.Sprint(v)
}

func (g *gen) structDecl(name string, t *Type, path, doc string) error {
	fs, err := g.fields(name, path, t)
	if err != nil {
		return err
	}
	if t.Index != nil {
		doc = joinDoc(doc, "The schema allows additional properties; they are preserved in Extra.")
	}
	g.writeDoc("", doc)
	if len(fs) == 0 && t.Index == nil {
		g.printf("type %s struct{}\n\n", name)
		return nil
	}
	g.printf("type %s struct {\n", name)
	for i, f := range fs {
		if i > 0 && f.doc != "" {
			g.printf("\n")
		}
		g.writeDoc("\t", f.doc)
		g.printf("\t%s %s `json:%q`\n", f.goName, f.goType, f.tag)
	}
	if t.Index != nil {
		if len(fs) > 0 {
			g.printf("\n")
		}
		g.printf("\t// Extra holds properties not declared by the schema.\n")
		g.printf("\tExtra map[string]any `json:\"-\"`\n")
	}
	g.printf("}\n\n")
	if t.Index != nil {
		known := make([]string, 0, len(t.Props))
		for _, p := range t.Props {
			known = append(known, strconv.Quote(p.Name))
		}
		lower := strings.ToLower(name[:1]) + name[1:]
		g.printf("var %sKnown = []string{%s}\n\n", lower, strings.Join(known, ", "))
		g.printf("// MarshalJSON encodes the declared fields followed by Extra.\n")
		g.printf("func (v %s) MarshalJSON() ([]byte, error) {\n\ttype plain %s\n\treturn marshalWithExtra(plain(v), v.Extra, %sKnown)\n}\n\n", name, name, lower)
		g.printf("// UnmarshalJSON decodes the declared fields and collects the rest in Extra.\n")
		g.printf("func (v *%s) UnmarshalJSON(data []byte) error {\n\ttype plain %s\n\tvar p plain\n\tif err := json.Unmarshal(data, &p); err != nil {\n\t\treturn err\n\t}\n", name, name)
		g.printf("\textra, err := extraFields(data, %sKnown)\n\tif err != nil {\n\t\treturn err\n\t}\n\t*v = %s(p)\n\tv.Extra = extra\n\treturn nil\n}\n\n", lower, name)
	}
	return nil
}

func (g *gen) sealedDecl(name string, members []*Type, disc string, vals [][]string, doc string) error {
	g.sealed[name] = true
	variants := make([]string, len(members))
	for i := range members {
		variants[i] = name + pascal(vals[i][0])
	}
	doc = joinDoc(doc, fmt.Sprintf("It is a union discriminated by %q; the concrete types are %s. Use Unmarshal%s to decode one.", disc, strings.Join(variants, ", "), name))
	g.writeDoc("", doc)
	g.printf("type %s interface {\n\tis%s()\n}\n\n", name, name)
	for i, m := range members {
		vn := variants[i]
		quoted := make([]string, len(vals[i]))
		for j, v := range vals[i] {
			quoted[j] = strconv.Quote(v)
		}
		if err := g.claim(vn, "variant:"+name+":"+vals[i][0]); err != nil {
			return err
		}
		if err := g.structDecl(vn, m, fmt.Sprintf("%s<%s>", name, vals[i][0]), fmt.Sprintf("%s is the %s variant with %s %s.", vn, name, disc, strings.Join(quoted, " or "))); err != nil {
			return err
		}
		g.printf("func (%s) is%s() {}\n\n", vn, name)
	}
	g.printf("// Unmarshal%s decodes data into the %s variant selected by its %q property.\n", name, name, disc)
	g.printf("func Unmarshal%s(data []byte) (%s, error) {\n", name, name)
	g.printf("\tvar probe struct {\n\t\tD string `json:%q`\n\t}\n\tif err := json.Unmarshal(data, &probe); err != nil {\n\t\treturn nil, err\n\t}\n\tswitch probe.D {\n", disc)
	for i := range members {
		quoted := make([]string, len(vals[i]))
		for j, v := range vals[i] {
			quoted[j] = strconv.Quote(v)
		}
		g.printf("\tcase %s:\n\t\tvar v %s\n\t\terr := json.Unmarshal(data, &v)\n\t\treturn v, err\n", strings.Join(quoted, ", "), variants[i])
	}
	g.printf("\t}\n\treturn nil, fmt.Errorf(\"tools: unknown %s %s %%q\", probe.D)\n}\n\n", name, disc)
	return nil
}

func (g *gen) kindedDecl(name string, members []*Type, path, doc string) error {
	groups := map[string][]*Type{}
	var order []string
	for _, m := range members {
		k := jsonKind(m)
		if k == "Any" {
			return fmt.Errorf("%s: cannot classify union member %s", name, key(m))
		}
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], m)
	}
	sort.SliceStable(order, func(i, j int) bool { return kindRank[order[i]] < kindRank[order[j]] })
	type alt struct{ kind, typ string }
	var alts []alt
	for _, k := range order {
		var t *Type
		if len(groups[k]) == 1 {
			t = groups[k][0]
		} else {
			t = normalize(&Type{Kind: KUnion, Members: groups[k]})
		}
		typ, err := g.expr(t, name+k, "", path+"<"+k+">")
		if err != nil {
			return err
		}
		if !strings.HasPrefix(typ, "[]") && !strings.HasPrefix(typ, "map[") {
			typ = "*" + typ
		}
		alts = append(alts, alt{k, typ})
	}
	names := make([]string, len(alts))
	for i, a := range alts {
		names[i] = a.kind
	}
	doc = joinDoc(doc, fmt.Sprintf("The schema is an untagged union of JSON kinds; exactly one of %s is set after decoding.", strings.Join(names, ", ")))
	g.writeDoc("", doc)
	g.printf("type %s struct {\n", name)
	for _, a := range alts {
		g.printf("\t%s %s\n", a.kind, a.typ)
	}
	g.printf("}\n\n")
	g.printf("// MarshalJSON encodes whichever alternative is set (null if none).\n")
	g.printf("func (u %s) MarshalJSON() ([]byte, error) {\n\tswitch {\n", name)
	for _, a := range alts {
		g.printf("\tcase u.%s != nil:\n\t\treturn json.Marshal(u.%s)\n", a.kind, a.kind)
	}
	g.printf("\t}\n\treturn []byte(\"null\"), nil\n}\n\n")
	g.printf("// UnmarshalJSON selects the alternative from the JSON value kind.\n")
	g.printf("func (u *%s) UnmarshalJSON(data []byte) error {\n\t*u = %s{}\n\tswitch k := jsonValueKind(data); k {\n", name, name)
	for _, a := range alts {
		g.printf("\tcase %s:\n\t\treturn json.Unmarshal(data, &u.%s)\n", kindConst[a.kind], a.kind)
	}
	g.printf("\tcase kindNull:\n\t\treturn nil\n\tdefault:\n\t\treturn fmt.Errorf(\"tools: cannot decode JSON %%s into %s\", k)\n\t}\n}\n\n", name)
	return nil
}

var kindRank = map[string]int{"String": 0, "Number": 1, "Bool": 2, "Array": 3, "Object": 4}

var kindConst = map[string]string{"String": "kindString", "Number": "kindNumber", "Bool": "kindBool", "Array": "kindArray", "Object": "kindObject"}

// ---- docs ----

var itemsTag = regexp.MustCompile(`^@(minItems|maxItems)\s+(\d+)\s*$`)

func hasTag(doc, tag string) bool {
	for _, l := range strings.Split(doc, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), tag) {
			return true
		}
	}
	return false
}

// fieldDoc renders a property's JSDoc: @minItems/@maxItems become a sentence
// and a leading "Deprecated" remark becomes a Go "Deprecated:" paragraph.
func fieldDoc(p *Prop) string {
	minN, maxN := -1, -1
	var lines []string
	for _, l := range strings.Split(p.Doc, "\n") {
		if m := itemsTag.FindStringSubmatch(strings.TrimSpace(l)); m != nil {
			n, _ := strconv.Atoi(m[2])
			if m[1] == "minItems" {
				minN = n
			} else {
				maxN = n
			}
			continue
		}
		lines = append(lines, l)
	}
	doc := strings.TrimSpace(strings.Join(lines, "\n"))
	if rest, ok := strings.CutPrefix(doc, "Deprecated"); ok {
		rest = strings.TrimLeft(rest, ":;., ")
		if rest != "" {
			rest = strings.ToUpper(rest[:1]) + rest[1:]
		}
		doc = "Deprecated: " + rest
	}
	var bounds string
	switch {
	case minN >= 0 && maxN >= 0:
		bounds = fmt.Sprintf("Holds %d to %d items.", minN, maxN)
	case minN >= 0:
		bounds = fmt.Sprintf("Holds at least %d items.", minN)
	case maxN >= 0:
		bounds = fmt.Sprintf("Holds at most %d items.", maxN)
	}
	if bounds != "" {
		if strings.HasPrefix(doc, "Deprecated:") {
			doc = bounds + "\n\n" + doc
		} else {
			doc = joinDoc(doc, bounds)
		}
	}
	return doc
}

func joinDoc(a, b string) string {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + "\n\n" + b
}

func (g *gen) writeDoc(indent, doc string) {
	doc = strings.TrimSpace(doc)
	if doc == "" {
		return
	}
	if indent == "" {
		// A single-line paragraph without terminal punctuation would be
		// reformatted into a heading by gofmt; terminate it instead.
		paras := strings.Split(doc, "\n\n")
		for i, para := range paras {
			if para != "" && !strings.Contains(para, "\n") && !strings.ContainsAny(para[len(para)-1:], ".!?:") {
				paras[i] = para + "."
			}
		}
		doc = strings.Join(paras, "\n\n")
	}
	blank := false
	for _, l := range strings.Split(doc, "\n") {
		l = strings.TrimRight(l, " \t")
		if l == "" {
			if blank {
				continue
			}
			blank = true
			g.printf("%s//\n", indent)
			continue
		}
		blank = false
		g.printf("%s// %s\n", indent, l)
	}
}
