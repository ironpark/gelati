// Package wire holds the messages exchanged with the Antigravity localharness
// binary, mirrored from the proto definitions shipped with the upstream
// google-antigravity Python SDK v0.1.20 (google/antigravity/proto): every
// message reachable from InputConfig, OutputConfig,
// InitializeConversationEvent, InputEvent and OutputEvent. That is all of
// localharness.proto plus the genai content.proto messages it reaches through
// genai.Struct (Struct, Value, Content and the content kinds). The message and
// enum declarations are generated from the upstream descriptors by
// testdata/gen_types.py; json.go, wellknown.go and binary.go are written by
// hand.
//
// genai.Struct is an ordinary message ({"fields":[{"name":..,"value":..}]}),
// not google.protobuf.Struct; [StructOf], [ValueOf], [Struct.AsMap] and
// [Value.AsAny] convert between it and plain Go values the way upstream's
// struct_converter does.
//
// The module is standard-library only, so there is no protobuf runtime. The
// types encode with encoding/json into the protobuf canonical JSON mapping
// that Python's json_format produces and parses:
//
//   - Members use the field's JSON name: lowerCamelCase, or the json_name
//     override (the media content mime_type fields are
//     "legacy_mime_type_enum"). [Unmarshal] also accepts the original proto
//     field names and ignores unknown members.
//   - Enums are Go string types holding the value name; decoding also accepts
//     the value number. An unknown number is kept as its decimal text and
//     re-encoded as a number.
//   - int64 and uint64 fields use [Int64] and [Uint64], which encode as
//     decimal strings and decode from strings or numbers.
//   - bytes fields are []byte, encoded as standard base64; nil is unset and a
//     non-nil empty slice is set (the tag uses omitzero).
//   - google.protobuf.Duration is [Duration] ("1.5s"); google.protobuf.NullValue
//     is [NullValue], encoded as JSON null.
//   - A oneof is a group of sibling optional fields, at most one of them set;
//     each member carries a "oneof <name>" comment.
//
// Both proto files use editions (2023 and 2024) with the default explicit
// field presence, so a singular field is either unset or set, even to its zero
// value, and json_format writes every set field. To keep that distinction,
// singular scalar and enum fields are pointers (see [Ptr]); nil means unset
// and is omitted. Each field has a nil-safe GetX method that returns the
// value, or the proto default when unset (for example
// [InputConfig.GetBindAddress] returns "localhost"). Repeated fields and maps
// have no presence and are omitted when empty.
//
// Non-finite doubles (NaN, ±Inf), which protobuf JSON writes as strings, are
// not supported.
//
// The stdin/stdout handshake uses the protobuf binary format for
// [InputConfig] and [OutputConfig]; see their MarshalBinary and
// UnmarshalBinary methods.
package wire
