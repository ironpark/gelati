"""Regenerates the golden files in this directory from the upstream protos.

The goldens are produced by Python's protobuf json_format from the descriptors
shipped in the google-antigravity v0.1.20 wheel. To regenerate, extract the
wheel's google/antigravity/proto package into $PBROOT/google/antigravity/proto
(with empty __init__.py files and no google/__init__.py) and run:

    PBROOT=/path uv run --with 'protobuf>=7.35' python gen_golden.py

Each root message is auto-filled: every field is set, and variant N picks
member N of every oneof (modulo its size), so together the variants cover
every field. The "zero" variant sets every scalar to its zero value to check
explicit presence.
"""
import os, sys
sys.path.insert(0, os.environ.get("PBROOT", "/tmp/agpb"))
from google.antigravity.proto import localharness_pb2 as lh
from google.protobuf import json_format
from google.protobuf.descriptor import FieldDescriptor as FD

HERE = os.path.dirname(os.path.abspath(__file__))
OUT = os.path.join(HERE, "golden")
os.makedirs(OUT, exist_ok=True)

def scalar(f, zero):
    t = f.type
    if t == FD.TYPE_STRING: return "" if zero else f"{f.name}-é<&>"
    if t == FD.TYPE_BYTES: return b"" if zero else b"\x00\xffhi"
    if t == FD.TYPE_BOOL: return False if zero else True
    if t == FD.TYPE_DOUBLE: return 0.0 if zero else 1.5
    if t == FD.TYPE_INT32: return 0 if zero else -7
    if t == FD.TYPE_UINT32: return 0 if zero else 7
    if t == FD.TYPE_INT64: return 0 if zero else -9007199254740993
    if t == FD.TYPE_UINT64: return 0 if zero else 18446744073709551615
    if t == FD.TYPE_ENUM:
        return f.enum_type.values[0].number if zero else f.enum_type.values[-1].number
    raise SystemExit(f"unhandled {f.full_name}")

def fill(m, variant, depth, zero):
    d = m.DESCRIPTOR
    if d.full_name == "google.protobuf.Duration":
        if not zero:
            m.seconds, m.nanos = 1, 500000000
        return
    chosen = {o.name: o.fields[variant % len(o.fields)].name for o in d.oneofs}
    for f in d.fields:
        o = f.containing_oneof
        if o is not None and chosen[o.name] != f.name:
            continue
        if f.message_type is not None and f.message_type.GetOptions().map_entry:
            getattr(m, f.name).update({"K_1": "v", "b": ""} if not zero else {})
            continue
        if f.message_type is not None:
            if depth <= 0:
                continue
            if f.is_repeated:
                for _ in range(2 if depth > 4 else 1):
                    fill(getattr(m, f.name).add(), variant, depth - 1, zero)
            else:
                getattr(m, f.name).SetInParent()
                fill(getattr(m, f.name), variant, depth - 1, zero)
            continue
        if f.enum_type is not None and f.enum_type.full_name == "google.protobuf.NullValue":
            setattr(m, f.name, 0)
            continue
        if f.is_repeated:
            getattr(m, f.name).extend([scalar(f, False), scalar(f, True)])
        else:
            setattr(m, f.name, scalar(f, zero))

ROOTS = {"init": lh.InitializeConversationEvent, "input": lh.InputEvent, "output": lh.OutputEvent}
for name, cls in ROOTS.items():
    # InputEvent's oneof has 9 members, the most of any reachable message.
    variants = [(str(v), v, False) for v in range(9)] + [("zero", 0, True)]
    for vname, v, zero in variants:
        m = cls()
        fill(m, v, 7, zero)
        with open(os.path.join(OUT, f"{name}_{vname}.json"), "w") as fh:
            fh.write(json_format.MessageToJson(m, indent=None) + "\n")
        if vname in ("0", "1"):
            # Proto field names instead of JSON names, to check that the
            # decoder accepts both.
            with open(os.path.join(OUT, f"{name}_{vname}.snake.json"), "w") as fh:
                fh.write(json_format.MessageToJson(
                    m, indent=None, preserving_proto_field_name=True) + "\n")

# Handshake messages in the binary format. InputConfig mirrors what the
# Python SDK sends: storage_directory explicitly "" and no bind_address.
ic = lh.InputConfig(
    storage_directory="",
    client_info=lh.ClientInfo(language="python", version="0.1.20",
        language_version="3.12.1", os="darwin", os_version="24.6.0"),
    env={"GEMINI_API_KEY": "k"},
)
full = lh.InputConfig(storage_directory="/tmp/s", port=4242, bind_address="127.0.0.1",
    client_info=lh.ClientInfo(language="go"), env={"A": "1"}, use_interactions_api=False)
oc = lh.OutputConfig(port=54321, api_key="secret-key")
neg = lh.OutputConfig(port=-1)
for fname, msg in [("inputconfig_python", ic), ("inputconfig_full", full),
                   ("outputconfig", oc), ("outputconfig_negative", neg)]:
    with open(os.path.join(OUT, fname + ".hex"), "w") as fh:
        fh.write(msg.SerializeToString().hex() + "\n")
