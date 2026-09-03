package instance

import (
	"encoding/binary"
	"errors"
	"fmt"
	"unicode/utf8"
)

// WorldInfo is what the panel reads out of a `.fwl` before touching a world: the format
// version 03 §4.1 rule 4 needs to refuse skew, and the internal world name rule 3 needs to
// keep in step with the filename.
type WorldInfo struct {
	// Version is the `.fwl` format version. Measured values: 33, 34, 37 (03 §4.2).
	Version int32
	// Name is the world name stored inside the file, which matched the filename in every
	// world measured.
	Name string
}

// ErrNotAWorldFile reports a `.fwl` that does not parse.
var ErrNotAWorldFile = errors.New("not a valid .fwl world file")

// fwlMinLen is the smallest byte count that could carry the fields ParseFWL reads: two
// int32s and a one-byte length prefix for an empty name.
const fwlMinLen = 4 + 4 + 1

// ParseFWL reads a `.fwl` header, per the layout measured in 03 §4.2.
//
// It deliberately stops after the world name, and that is not laziness. 03 §4.2 was
// derived from four worlds spanning three format versions; the version field sits at a
// fixed offset with nothing variable-length before it, and the name immediately follows —
// but everything after the name is version-dependent territory. A version field exists
// precisely because the rest may move, and three versions agreeing is not proof a fourth
// will. So the panel reads the two fields it needs, both of which are before that line, and
// refuses to guess at the rest.
func ParseFWL(data []byte) (WorldInfo, error) {
	if len(data) < fwlMinLen {
		return WorldInfo{}, fmt.Errorf("%w: %d bytes is too short", ErrNotAWorldFile, len(data))
	}

	// The leading int32 is the length of everything after it. A file whose own header
	// disagrees with its size is truncated or is not a `.fwl` at all — 03 §4.1 rule 5's
	// "sanity, not trust", and the cheapest possible check that the rest is worth reading.
	// The engine writes these as signed int32s; read them as unsigned and widen, so a
	// corrupt file cannot wrap into a plausible-looking value on the way in.
	//nolint:gosec // deliberate signed reinterpretation, widened to int64 immediately
	payload := int64(int32(binary.LittleEndian.Uint32(data[0:4])))
	if payload != int64(len(data))-4 {
		return WorldInfo{}, fmt.Errorf(
			"%w: header claims %d payload bytes, file carries %d", ErrNotAWorldFile, payload, len(data)-4)
	}

	//nolint:gosec // same signed reinterpretation as the length above
	info := WorldInfo{Version: int32(binary.LittleEndian.Uint32(data[4:8]))}
	if info.Version <= 0 {
		return WorldInfo{}, fmt.Errorf("%w: version %d is not plausible", ErrNotAWorldFile, info.Version)
	}

	name, _, err := readDotNetString(data, 8)
	if err != nil {
		return WorldInfo{}, fmt.Errorf("%w: world name: %w", ErrNotAWorldFile, err)
	}
	info.Name = name
	return info, nil
}

// readDotNetString reads a C# BinaryWriter string at off: a 7-bit-encoded length prefix,
// then that many bytes of UTF-8. It returns the string and the offset just past it.
func readDotNetString(data []byte, off int) (s string, next int, err error) {
	length, shift := 0, 0
	for {
		if off >= len(data) {
			return "", 0, errors.New("length prefix runs past the end of the file")
		}
		b := data[off]
		off++
		length |= int(b&0x7F) << shift
		if b&0x80 == 0 {
			break
		}
		shift += 7
		if shift > 28 {
			return "", 0, errors.New("length prefix is not a valid 7-bit encoding")
		}
	}
	if length < 0 || off+length > len(data) {
		return "", 0, fmt.Errorf("declared length %d runs past the end of the file", length)
	}
	out := string(data[off : off+length])
	if !utf8.ValidString(out) {
		return "", 0, errors.New("is not valid UTF-8")
	}
	return out, off + length, nil
}
