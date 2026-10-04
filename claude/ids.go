package claude

import (
	"crypto/rand"
	"fmt"
)

// randomUUID returns a random RFC 4122 version 4 UUID, used for message,
// request and session ids.
func randomUUID() string {
	var u [16]byte
	_, _ = rand.Read(u[:])
	u[6] = u[6]&0x0f | 0x40
	u[8] = u[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}
