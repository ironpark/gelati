"""Writes the FileDescriptorSet of localharness.proto and its imports, taken
from the descriptors in the google-antigravity v0.1.20 wheel, to the path given
as the first argument. See main.go for the whole extraction.
"""
import os, sys
sys.path.insert(0, os.environ.get("PBROOT", "/tmp/agpb"))
from google.antigravity.proto import localharness_pb2 as lh
from google.protobuf import descriptor_pb2

fds = descriptor_pb2.FileDescriptorSet()
seen = set()

def walk(fd):
    for dep in fd.dependencies:
        walk(dep)
    if fd.name not in seen:
        seen.add(fd.name)
        fds.file.append(descriptor_pb2.FileDescriptorProto.FromString(fd.serialized_pb))

walk(lh.DESCRIPTOR)
with open(sys.argv[1], "wb") as f:
    f.write(fds.SerializeToString())
