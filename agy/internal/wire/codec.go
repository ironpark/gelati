package wire

import (
	"encoding/json/v2"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// Marshal encodes msg in the protobuf JSON mapping Python's json_format
// writes: lowerCamelCase member names and only the fields that are set.
func Marshal(msg proto.Message) ([]byte, error) {
	return protojson.Marshal(msg)
}

// Unmarshal decodes the protobuf JSON mapping into msg, accepting both JSON
// and original proto field names and ignoring unknown members, so a newer
// harness cannot break an older SDK.
func Unmarshal(data []byte, msg proto.Message) error {
	return protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(data, msg)
}

// ProtoMap returns msg as the generic map Python's
// json_format.MessageToDict(msg, preserving_proto_field_name=True) produces:
// object keys are the original proto field names, only set fields appear,
// 64-bit integers are decimal strings, enums are value names and bytes are
// base64 text. Other numbers decode as float64. A nil msg yields nil.
func ProtoMap(msg proto.Message) (map[string]any, error) {
	if msg == nil || !msg.ProtoReflect().IsValid() {
		return nil, nil
	}
	b, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(msg)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}
