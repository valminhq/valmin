package store

import (
	"crypto/rand"
	"encoding/hex"
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

	var buf [36]byte

	hex.Encode(buf[0:8], b[0:4])
	buf[8] = '-'
	hex.Encode(buf[9:13], b[4:6])
	buf[13] = '-'
	hex.Encode(buf[14:18], b[6:8])
	buf[18] = '-'
	hex.Encode(buf[19:23], b[8:10])
	buf[23] = '-'
	hex.Encode(buf[24:36], b[10:16])

	return string(buf[:])
}
