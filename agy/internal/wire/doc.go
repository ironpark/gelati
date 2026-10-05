// Package wire holds the messages exchanged with the Antigravity localharness
// binary: the generated Go code for the proto definitions shipped with the
// upstream google-antigravity Python SDK v0.1.20 (google/antigravity/proto),
// that is localharness.proto, the genai content.proto it reaches, and their
// JSON annotation options.
//
// The .proto files under proto/ were printed from the descriptors in the
// upstream wheel (see testdata/protoextract). The Go code is generated from
// them with buf and protoc-gen-go using the opaque API: construct messages
// with the X_builder types and read them with the GetX, HasX and WhichX
// accessors.
//
// genai.Struct is an ordinary message ({"fields":[{"name":..,"value":..}]}),
// not google.protobuf.Struct; [StructOf], [ValueOf], [Struct.AsMap] and
// [Value.AsAny] convert between it and plain Go values the way upstream's
// struct_converter does.
//
// Events travel as protobuf JSON ([Marshal], [Unmarshal]), which matches
// Python's json_format; the stdin/stdout handshake uses the binary format for
// [InputConfig] and [OutputConfig].
package wire

//go:generate buf generate
