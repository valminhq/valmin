package store

import (
	"crypto/rand"
	"fmt"
	"time"
)

// NewID returns a UUIDv7: a 48-bit millisecond timestamp followed by random bits, so ids
// sort by creation time and carry no host or sequence state (06 §4, RFC 9562 §5.7).
func NewID() string {
	ms := time.Now().UnixMilli()
	if ms < 0 {
		// A clock set before 1970 is not a reason to mint a malformed id.
		ms = 0
	}
	t := uint64(ms)

	var b [16]byte
	b[0], b[1], b[2] = byte(t>>40), byte(t>>32), byte(t>>24)
	b[3], b[4], b[5] = byte(t>>16), byte(t>>8), byte(t)
	if _, err := rand.Read(b[6:]); err != nil {
		panic("crypto/rand failed: " + err.Error()) // unreachable on Linux; nothing sane to fall back to
	}
	b[6] = 0x70 | b[6]&0x0f
	b[8] = 0x80 | b[8]&0x3f

	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
