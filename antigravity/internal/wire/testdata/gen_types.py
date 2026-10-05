"""Generates the message and enum types of package wire (content.go and
localharness_*.go) from the descriptors in the upstream google-antigravity
wheel, so field numbers, JSON names, presence and defaults match exactly.

Set up $PBROOT as described in gen_golden.py, then run from this directory:

    PBROOT=/path uv run --with 'protobuf>=7.35' python gen_types.py && gofmt -w ..
"""
import os, re, sys, collections
sys.path.insert(0, os.environ.get("PBROOT", "/tmp/agpb"))
from google.antigravity.proto import localharness_pb2 as lh
from google.protobuf.descriptor import FieldDescriptor as FD

OUT = sys.argv[1] if len(sys.argv) > 1 else os.path.join(os.path.dirname(os.path.abspath(__file__)), "..")
INITIALISMS = {"id":"ID","url":"URL","uri":"URI","json":"JSON","api":"API","http":"HTTP","mcp":"MCP","os":"OS"}

def word(w):
    lw = w.lower()
    if lw in INITIALISMS: return INITIALISMS[lw]
    return lw[:1].upper() + lw[1:]

def camel_words(s):
    return re.findall(r"[A-Z]+(?![a-z])|[A-Z][a-z0-9]*|[a-z0-9]+", s)

def go_type_name(full):
    for p in ("antigravity.localharness.", "genai."):
        if full.startswith(p): full = full[len(p):]; break
    else: raise SystemExit("unexpected package "+full)
    return "".join(word(w) for part in full.split(".") for w in camel_words(part))

def go_field_name(name):
    return "".join(word(w) for w in name.split("_"))

def snake_of_camel(s):
    return re.sub(r"([A-Z])", lambda m: "_"+m.group(1).lower(), s)

ROOTS = [lh.InputConfig, lh.OutputConfig, lh.InitializeConversationEvent, lh.InputEvent, lh.OutputEvent]
msgs = collections.OrderedDict(); enums = collections.OrderedDict()
WKT = {"google.protobuf.Duration": "Duration"}
def walk(d):
    if d.full_name in msgs or d.full_name in WKT: return
    if d.full_name.startswith("google.protobuf."): raise SystemExit("unhandled WKT "+d.full_name)
    msgs[d.full_name] = d
    for f in d.fields:
        if f.message_type is not None:
            if f.message_type.GetOptions().map_entry:
                for sub in f.message_type.fields:
                    assert sub.type == FD.TYPE_STRING, f.full_name
                continue
            walk(f.message_type)
        if f.enum_type is not None and f.enum_type.full_name != "google.protobuf.NullValue":
            enums[f.enum_type.full_name] = f.enum_type
for r in ROOTS: walk(r.DESCRIPTOR)

names = {}
for full in list(msgs) + list(enums):
    n = go_type_name(full)
    assert n not in names, (n, full, names.get(n))
    names[n] = full

STEPS = set("StepUpdate ActionGenerateImage ActionSearchWeb ActionReadUrlContent ActionFinish ActionError ActionListDirectory ActionFindFile ActionSearchDirectory ActionViewFile ActionCreateFile ActionEditFile ActionRunCommand ActionCompaction ActionInvokeSubagent ToolConfirmationRequest UserQuestionsRequest UserQuestion MultipleChoice ActionMcpTool ActionCustomTool ActionSkillLookup".split())
HOOKS = set("CallHookRequest CallHookResponse PreToolArgs PostToolArgs OnToolErrorArgs PreTurnArgs PostTurnArgs OnCompactionArgs StopArgs EmptyResult OnToolErrorResult PreToolResult PreTurnResult StopResult PolicyDecisionRequest PolicyDecisionResponse LifecycleHook PolicyEvaluationOutcome".split())
EVENTS = set("OutputEvent InputEvent SandboxStatus InitializeConversationResponse UserInput ToolConfirmation TrajectoryStateUpdate TrajectoryUsageEntry UsageUpdate ToolCall ToolResponse UserQuestionsResponse UserQuestionAnswer MultipleChoiceAnswer Media ModalityTokenCount UsageMetadata Modality".split())
def file_of(full):
    if full.startswith("genai."): return "content"
    top = full[len("antigravity.localharness."):].split(".")[0]
    if top in STEPS: return "localharness_steps"
    if top in HOOKS: return "localharness_hooks"
    if top in EVENTS: return "localharness_events"
    return "localharness_config"

SCALAR = {FD.TYPE_DOUBLE:("float64","0"), FD.TYPE_FLOAT:("float32","0"), FD.TYPE_INT32:("int32","0"),
          FD.TYPE_UINT32:("uint32","0"), FD.TYPE_BOOL:("bool","false"), FD.TYPE_STRING:("string",'""'),
          FD.TYPE_INT64:("Int64","0"), FD.TYPE_UINT64:("Uint64","0")}
GETTER_RET = {"Int64":"int64","Uint64":"uint64"}

def field_info(f):
    """Returns (go field type, getter return type, getter body)."""
    gname = go_field_name(f.name)
    if f.message_type is not None and f.message_type.GetOptions().map_entry:
        return "map[string]string", "map[string]string", None
    rep = f.is_repeated
    if f.message_type is not None:
        t = WKT.get(f.message_type.full_name) or go_type_name(f.message_type.full_name)
        return ("[]*"+t, "[]*"+t, None) if rep else ("*"+t, "*"+t, None)
    if f.enum_type is not None:
        if f.enum_type.full_name == "google.protobuf.NullValue":
            assert not rep; return "*NullValue", "NullValue", ("NullValue", '""')
        t = go_type_name(f.enum_type.full_name)
        if rep: return "[]"+t, "[]"+t, None
        dflt = '""'
        if f.has_default_value: raise SystemExit("enum default "+f.full_name)
        return "*"+t, t, (t, dflt)
    if f.type == FD.TYPE_BYTES:
        return "[]byte", "[]byte", None
    t, zero = SCALAR[f.type]
    if rep: return "[]"+t, "[]"+t, None
    ret = GETTER_RET.get(t, t)
    dflt = zero
    if f.has_default_value and f.default_value not in ("", 0, False):
        v = f.default_value
        dflt = ('"%s"' % v) if isinstance(v, str) else ("true" if v is True else str(v))
    return "*"+t, ret, (t, dflt)

def ensure_present(f):
    assert f.is_repeated or f.has_presence, f.full_name

out = collections.defaultdict(list)
def emit(fname, s): out[fname].append(s)

def enum_const_names(e):
    tname = go_type_name(e.full_name)
    simple_words = camel_words(e.name)
    prefixes = ["_".join(w.upper() for w in simple_words)+"_", simple_words[-1].upper()+"_"]
    res = []
    for v in e.values:
        n = v.name
        for p in prefixes:
            if n.startswith(p) and len(n) > len(p): n = n[len(p):]; break
        res.append((tname + "".join(word(w) for w in n.split("_")), v.name, v.number))
    return tname, res

all_consts = set()
for full, e in enums.items():
    tname, vals = enum_const_names(e)
    fn = file_of(full)
    lines = [f"// {tname} mirrors the {full} enum.", f"type {tname} string", "", f"// {tname} values.", "const ("]
    for cn, vn, num in vals:
        assert cn not in all_consts, cn; all_consts.add(cn)
        lines.append(f'\t{cn} {tname} = "{vn}"')
    lines.append(")")
    lower = tname[0].lower()+tname[1:]
    lines.append("")
    lines.append(f"var {lower}Names = map[int32]string{{" + ", ".join(f'{num}: "{vn}"' for _, vn, num in vals) + "}")
    lines.append("")
    lines.append(f"// MarshalJSON encodes the value name; a numeric value outside the known set\n// encodes as a JSON number, as protobuf JSON does for open enums.")
    lines.append(f"func (e {tname}) MarshalJSON() ([]byte, error) {{ return marshalEnum(string(e)) }}")
    lines.append("")
    lines.append(f"// UnmarshalJSON accepts the value name or its number.")
    lines.append(f"func (e *{tname}) UnmarshalJSON(b []byte) error {{ return unmarshalEnum(b, (*string)(e), {lower}Names) }}")
    emit(fn, "\n".join(lines))

for full, d in msgs.items():
    tname = go_type_name(full)
    fn = file_of(full)
    fields = sorted(d.fields, key=lambda f: f.number)
    lines = [f"// {tname} mirrors the {full} message."]
    real_oneofs = [o for o in d.oneofs]
    if real_oneofs:
        for o in real_oneofs:
            lines.append(f"// At most one of the {o.name} oneof fields ({', '.join(go_field_name(f.name) for f in o.fields)}) is set.")
    lines.append(f"type {tname} struct {{")
    getters = []
    for f in fields:
        ensure_present(f)
        gname = go_field_name(f.name)
        ftype, rtype, scal = field_info(f)
        # omitzero keeps a set-but-empty bytes field (non-nil, len 0).
        tag = f.json_name + (",omitzero" if f.type == FD.TYPE_BYTES and not f.is_repeated else ",omitempty")
        extra = ""
        if snake_of_camel(f.json_name) != f.name:
            extra = f' proto:"{f.name}"'
        comment = ""
        if f.containing_oneof is not None:
            comment = f" // oneof {f.containing_oneof.name}"
        lines.append(f'\t{gname} {ftype} `json:"{tag}"{extra}`{comment}')
        if scal is not None:
            t, dflt = scal
            desc = f"or {dflt} when unset" if dflt not in ('""', "0", "false") else "or the zero value when unset"
            if dflt not in ('""', "0", "false"): desc = f"or the proto default {dflt} when unset"
            conv = f"{rtype}(*x.{gname})" if rtype != t else f"*x.{gname}"
            if t == "NullValue": conv = f"*x.{gname}"
            getters.append(f"// Get{gname} returns {gname}, {desc}.\nfunc (x *{tname}) Get{gname}() {rtype} {{\n\tif x != nil && x.{gname} != nil {{\n\t\treturn {conv}\n\t}}\n\treturn {dflt}\n}}")
        else:
            getters.append(f"// Get{gname} returns {gname}, or nil when x is nil.\nfunc (x *{tname}) Get{gname}() {rtype} {{\n\tif x != nil {{\n\t\treturn x.{gname}\n\t}}\n\treturn nil\n}}")
    lines.append("}")
    emit(fn, "\n".join(lines) + "\n\n" + "\n\n".join(getters))

HEAD = "// Code generated by testdata/gen_types.py from the google-antigravity v0.1.20\n// proto definitions (google/antigravity/proto/{src}). DO NOT EDIT.\n\npackage wire\n"
SRC = {"content":"content.proto","localharness_config":"localharness.proto","localharness_events":"localharness.proto","localharness_steps":"localharness.proto","localharness_hooks":"localharness.proto"}
for fn, parts in out.items():
    with open(f"{OUT}/{fn}.go", "w") as fh:
        fh.write(HEAD.format(src=SRC[fn]) + "\n" + "\n\n".join(parts) + "\n")
print(len(msgs), "messages", len(enums), "enums", file=sys.stderr)
