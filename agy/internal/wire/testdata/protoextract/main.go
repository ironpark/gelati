// Command protoextract prints the google/antigravity/proto files of a
// FileDescriptorSet as .proto sources. The upstream wheel ships only compiled
// descriptors, so this is how ../../proto is produced. Set up $PBROOT as
// described in ../gen_golden.py, then run from this directory:
//
//	PBROOT=/path uv run --with 'protobuf>=7.35' python dump.py /tmp/ag.binpb
//	go run . /tmp/ag.binpb ../../proto
//	(cd ../.. && buf format -w && go generate)
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jhump/protoreflect/desc"
	"github.com/jhump/protoreflect/desc/protoprint"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/types/descriptorpb"
)

const header = "// Printed from the descriptors in the google-antigravity v0.1.20 wheel\n" +
	"// (google/antigravity/proto/%s_pb2.py) by testdata/protoextract.\n\n"

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: protoextract descriptors.binpb outdir")
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(in, outDir string) error {
	b, err := os.ReadFile(in)
	if err != nil {
		return err
	}
	var set descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(b, &set); err != nil {
		return err
	}
	files, err := protodesc.NewFiles(&set)
	if err != nil {
		return err
	}
	printer := &protoprint.Printer{Indent: "  "}
	for _, f := range set.GetFile() {
		if filepath.Dir(f.GetName()) != "google/antigravity/proto" {
			continue
		}
		fd, err := files.FindFileByPath(f.GetName())
		if err != nil {
			return err
		}
		d, err := desc.WrapFile(fd)
		if err != nil {
			return err
		}
		var sb strings.Builder
		if err := printer.PrintProtoFile(d, &sb); err != nil {
			return err
		}
		out := filepath.Join(outDir, f.GetName())
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return err
		}
		base := strings.TrimSuffix(filepath.Base(f.GetName()), ".proto")
		if err := os.WriteFile(out, []byte(fmt.Sprintf(header, base)+sb.String()), 0o644); err != nil {
			return err
		}
	}
	return nil
}
