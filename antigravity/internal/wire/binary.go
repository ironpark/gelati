package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"maps"
	"slices"
	"unicode/utf8"
)

// The stdin/stdout handshake carries InputConfig and OutputConfig in the
// protobuf binary format. This file implements just enough of that format for
// those two messages (and ClientInfo inside InputConfig). Fields are written
// in field-number order; set fields are written even when they hold the zero
// value, matching explicit field presence.

// Protobuf wire types.
const (
	wireVarint = 0
	wireI64    = 1
	wireLen    = 2
	wireStartG = 3
	wireEndG   = 4
	wireI32    = 5
)

var errTruncated = errors.New("wire: truncated protobuf message")

// MarshalBinary encodes c in the protobuf binary format.
func (c *InputConfig) MarshalBinary() ([]byte, error) {
	var b []byte
	if c.StorageDirectory != nil {
		b = appendLen(b, 1, *c.StorageDirectory)
	}
	if c.Port != nil {
		b = appendVarintField(b, 2, uint64(*c.Port))
	}
	if c.BindAddress != nil {
		b = appendLen(b, 3, *c.BindAddress)
	}
	if c.ClientInfo != nil {
		inner, err := c.ClientInfo.MarshalBinary()
		if err != nil {
			return nil, err
		}
		b = appendLen(b, 4, inner)
	}
	for _, k := range slices.Sorted(maps.Keys(c.Env)) {
		var entry []byte
		entry = appendLen(entry, 1, k)
		entry = appendLen(entry, 2, c.Env[k])
		b = appendLen(b, 5, entry)
	}
	if c.UseInteractionsAPI != nil {
		b = appendVarintField(b, 6, boolToVarint(*c.UseInteractionsAPI))
	}
	return b, nil
}

// UnmarshalBinary decodes the protobuf binary form of an InputConfig,
// skipping unknown fields.
func (c *InputConfig) UnmarshalBinary(data []byte) error {
	*c = InputConfig{}
	return walkFields(data, func(num int32, typ int, v uint64, raw []byte) error {
		switch {
		case num == 1 && typ == wireLen:
			c.StorageDirectory = new(string(raw))
		case num == 2 && typ == wireVarint:
			c.Port = new(uint32(v))
		case num == 3 && typ == wireLen:
			c.BindAddress = new(string(raw))
		case num == 4 && typ == wireLen:
			if c.ClientInfo == nil {
				c.ClientInfo = &ClientInfo{}
			}
			return c.ClientInfo.unmarshalMerge(raw)
		case num == 5 && typ == wireLen:
			var key, val string
			err := walkFields(raw, func(n int32, t int, _ uint64, r []byte) error {
				switch {
				case n == 1 && t == wireLen:
					key = string(r)
				case n == 2 && t == wireLen:
					val = string(r)
				}
				return nil
			})
			if err != nil {
				return err
			}
			if c.Env == nil {
				c.Env = make(map[string]string)
			}
			c.Env[key] = val
		case num == 6 && typ == wireVarint:
			c.UseInteractionsAPI = new(v != 0)
		}
		return nil
	})
}

// MarshalBinary encodes c in the protobuf binary format.
func (c *ClientInfo) MarshalBinary() ([]byte, error) {
	var b []byte
	for i, s := range []*string{c.Language, c.Version, c.LanguageVersion, c.OS, c.OSVersion} {
		if s != nil {
			b = appendLen(b, int32(i+1), *s)
		}
	}
	return b, nil
}

// UnmarshalBinary decodes the protobuf binary form of a ClientInfo.
func (c *ClientInfo) UnmarshalBinary(data []byte) error {
	*c = ClientInfo{}
	return c.unmarshalMerge(data)
}

func (c *ClientInfo) unmarshalMerge(data []byte) error {
	fields := []**string{&c.Language, &c.Version, &c.LanguageVersion, &c.OS, &c.OSVersion}
	return walkFields(data, func(num int32, typ int, _ uint64, raw []byte) error {
		if typ == wireLen && num >= 1 && int(num) <= len(fields) {
			*fields[num-1] = new(string(raw))
		}
		return nil
	})
}

// MarshalBinary encodes c in the protobuf binary format.
func (c *OutputConfig) MarshalBinary() ([]byte, error) {
	var b []byte
	if c.Port != nil {
		// int32 values are sign-extended to 64 bits on the wire.
		b = appendVarintField(b, 1, uint64(int64(*c.Port)))
	}
	if c.APIKey != nil {
		b = appendLen(b, 2, *c.APIKey)
	}
	return b, nil
}

// UnmarshalBinary decodes the protobuf binary form of an OutputConfig,
// skipping unknown fields.
func (c *OutputConfig) UnmarshalBinary(data []byte) error {
	*c = OutputConfig{}
	return walkFields(data, func(num int32, typ int, v uint64, raw []byte) error {
		switch {
		case num == 1 && typ == wireVarint:
			c.Port = new(int32(v))
		case num == 2 && typ == wireLen:
			if !utf8.Valid(raw) {
				return errors.New("wire: OutputConfig.api_key is not valid UTF-8")
			}
			c.APIKey = new(string(raw))
		}
		return nil
	})
}

func appendTag(b []byte, num int32, typ int) []byte {
	return binary.AppendUvarint(b, uint64(num)<<3|uint64(typ))
}

func appendVarintField(b []byte, num int32, v uint64) []byte {
	return binary.AppendUvarint(appendTag(b, num, wireVarint), v)
}

// appendLen appends a length-delimited field holding v.
func appendLen[T ~string | ~[]byte](b []byte, num int32, v T) []byte {
	b = binary.AppendUvarint(appendTag(b, num, wireLen), uint64(len(v)))
	return append(b, v...)
}

func boolToVarint(v bool) uint64 {
	if v {
		return 1
	}
	return 0
}

// walkFields calls fn for each field in data. For varint fields v holds the
// value; for length-delimited fields raw holds the payload. Fixed-width
// fields and groups are skipped.
func walkFields(data []byte, fn func(num int32, typ int, v uint64, raw []byte) error) error {
	for len(data) > 0 {
		num, typ, v, raw, rest, err := consumeField(data)
		if err != nil {
			return err
		}
		data = rest
		switch typ {
		case wireVarint, wireLen:
			if err := fn(num, typ, v, raw); err != nil {
				return err
			}
		case wireEndG:
			return fmt.Errorf("wire: unexpected wire type %d", typ)
		}
	}
	return nil
}

// consumeField reads one field from the front of data and returns it with
// the data that follows. For varint fields v holds the value; for
// length-delimited fields raw holds the payload. A group is skipped whole; an
// end-group tag is returned for the caller to match.
func consumeField(data []byte) (num int32, typ int, v uint64, raw, rest []byte, err error) {
	tag, n := binary.Uvarint(data)
	if n <= 0 {
		return 0, 0, 0, nil, nil, errTruncated
	}
	data = data[n:]
	num, typ = int32(tag>>3), int(tag&7)
	if num <= 0 {
		return 0, 0, 0, nil, nil, fmt.Errorf("wire: invalid field number %d", tag>>3)
	}
	switch typ {
	case wireVarint:
		v, n = binary.Uvarint(data)
		if n <= 0 {
			return 0, 0, 0, nil, nil, errTruncated
		}
		data = data[n:]
	case wireLen:
		l, n := binary.Uvarint(data)
		if n <= 0 || l > uint64(len(data)-n) {
			return 0, 0, 0, nil, nil, errTruncated
		}
		raw, data = data[n:n+int(l)], data[n+int(l):]
	case wireI64, wireI32:
		size := 8
		if typ == wireI32 {
			size = 4
		}
		if len(data) < size {
			return 0, 0, 0, nil, nil, errTruncated
		}
		data = data[size:]
	case wireStartG:
		if data, err = skipGroup(data, num); err != nil {
			return 0, 0, 0, nil, nil, err
		}
	case wireEndG:
	default:
		return 0, 0, 0, nil, nil, fmt.Errorf("wire: unexpected wire type %d", typ)
	}
	return num, typ, v, raw, data, nil
}

// skipGroup skips the body of a group started with field number num.
func skipGroup(data []byte, num int32) ([]byte, error) {
	for {
		n, typ, _, _, rest, err := consumeField(data)
		if err != nil {
			return nil, err
		}
		data = rest
		if typ == wireEndG {
			if n != num {
				return nil, errors.New("wire: mismatched group end")
			}
			return data, nil
		}
	}
}
