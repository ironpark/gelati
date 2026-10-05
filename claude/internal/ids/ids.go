// Package ids generates and validates the UUIDs used for message, request
// and session ids.
package ids

import (
	"crypto/rand"
	"fmt"
	"regexp"
)

var uuidRE = regexp.MustCompile(
	`^(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// NewUUID returns a random RFC 4122 version 4 UUID, used for message, request
// and session ids.
func NewUUID() string {
	var u [16]byte
	_, _ = rand.Read(u[:])
	u[6] = u[6]&0x0f | 0x40
	u[8] = u[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}

// IsUUID reports whether s is a UUID (any case). Session ids are used as
// path components, so only UUIDs are accepted.
func IsUUID(s string) bool { return uuidRE.MatchString(s) }
