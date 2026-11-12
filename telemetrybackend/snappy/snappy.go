// Package snappy implements the Snappy block compression format, the
// one Prometheus remote write uses.
//
// The format is small and well-specified, so implementing it here keeps
// the harness free of external dependencies. The encoder produces the
// same bytes as the reference implementation; the tests check that
// against the reference, so the two cannot drift apart.
//
// The format is a sequence of tags:
//
//   - A literal: a length and the bytes themselves.
//   - A copy: an offset and a length, meaning "the bytes at offset
//     back are repeated".
//
// The block starts with the varint-encoded length of the decoded bytes.
package snappy

import (
	"encoding/binary"
	"errors"
)

// ErrCorrupt reports that the input is not a valid snappy block.
var ErrCorrupt = errors.New("snappy: corrupt input")

// The tag types of the block format.
const (
	tagLiteral = 0x00
	tagCopy1   = 0x01
	tagCopy2   = 0x02
	tagCopy4   = 0x03
)

// maxBlockSize is the largest block the format allows.
const maxBlockSize = 65536

// maxEncodedLen returns the largest encoded size of a block of the
// given length.
//
// The worst case is a literal-only encoding: every 60 bytes of input
// costs one tag byte plus one length byte, so the blowup is 62/60.
func maxEncodedLen(srcLen int) int {
	n := uint64(srcLen)
	if n > 0xffffffff {
		return -1
	}
	n = 32 + n + n/6
	if n > 0xffffffff {
		return -1
	}
	return int(n)
}

// Encode compresses src into a snappy block.
//
// The result is the format's "block" form, which is what remote write
// uses: the length-prefixed payload with no framing.
func Encode(src []byte) ([]byte, error) {
	n := maxEncodedLen(len(src))
	if n < 0 {
		return nil, errors.New("snappy: block too large")
	}
	dst := make([]byte, 0, n)

	// The block starts with the varint-encoded length of the decoded
	// bytes.
	dst = binary.AppendUvarint(dst, uint64(len(src)))

	for len(src) > 0 {
		p := src
		if len(p) > maxBlockSize {
			p, src = p[:maxBlockSize], p[maxBlockSize:]
		} else {
			src = nil
		}
		if len(p) < minNonLiteralBlockSize {
			dst = emitLiteral(dst, p)
		} else {
			dst = encodeBlock(dst, p)
		}
	}
	return dst, nil
}

// minNonLiteralBlockSize is the smallest block that can hold a copy.
//
// The encoded output must start with a literal, and a copy needs four
// bytes to look ahead, so a block shorter than this is all literal.
const minNonLiteralBlockSize = 1 + 1 + inputMargin

// inputMargin is the number of extra input bytes the encoder keeps in
// hand, so the literal emitter can copy in 16-byte chunks.
const inputMargin = 16 - 1

// emitLiteral appends a literal chunk.
func emitLiteral(dst []byte, lit []byte) []byte {
	n := len(lit) - 1
	switch {
	case n < 60:
		dst = append(dst, uint8(n)<<2|tagLiteral)
	case n < 1<<8:
		dst = append(dst, 60<<2|tagLiteral, uint8(n))
	default:
		dst = append(dst, 61<<2|tagLiteral, uint8(n), uint8(n>>8))
	}
	return append(dst, lit...)
}

// emitCopy appends a copy chunk of the given offset and length.
//
// The length must be at least 4 and the offset must fit in 16 bits.
func emitCopy(dst []byte, offset, length int) []byte {
	for length >= 68 {
		// A 64-byte copy, encoded as 3 bytes.
		dst = append(dst, 63<<2|tagCopy2, uint8(offset), uint8(offset>>8))
		length -= 64
	}
	if length > 64 {
		// A 60-byte copy, encoded as 3 bytes.
		dst = append(dst, 59<<2|tagCopy2, uint8(offset), uint8(offset>>8))
		length -= 60
	}
	if length >= 12 || offset >= 2048 {
		// The remaining copy, encoded as 3 bytes.
		dst = append(dst, uint8(length-1)<<2|tagCopy2, uint8(offset), uint8(offset>>8))
		return dst
	}
	// The remaining copy, encoded as 2 bytes.
	dst = append(dst, uint8(offset>>8)<<5|uint8(length-4)<<2|tagCopy1, uint8(offset))
	return dst
}

// encodeBlock encodes one block, appending to dst.
//
// It is the same algorithm as the reference implementation: a hash
// table finds four-byte matches, and the matches are extended.
func encodeBlock(dst, src []byte) []byte {
	// The table's size ranges from 1<<8 to 1<<14. Its elements are
	// uint16, because an offset into a block always fits in 16 bits.
	const (
		maxTableSize = 1 << 14
		tableMask    = maxTableSize - 1
	)
	shift := uint32(32 - 8)
	for tableSize := 1 << 8; tableSize < maxTableSize && tableSize < len(src); tableSize *= 2 {
		shift--
	}
	var table [maxTableSize]uint16

	// sLimit is where to stop looking for matches. The margin lets the
	// literal emitter use a fast path.
	sLimit := len(src) - inputMargin

	// nextEmit is where the next literal starts.
	nextEmit := 0

	// The output must start with a literal, so the search starts at 1.
	s := 1
	nextHash := hash(load32(src, s), shift)

	for {
		// Heuristic match skipping: after 32 bytes without a match,
		// look at every other byte, then every third, and so on. A match
		// resets the skip.
		skip := 32
		nextS := s
		candidate := 0
		for {
			s = nextS
			bytesBetweenHashLookups := skip >> 5
			nextS = s + bytesBetweenHashLookups
			skip += bytesBetweenHashLookups
			if nextS > sLimit {
				// The rest of the block is a literal — unless there is
				// no rest, in which case nothing is emitted.
				if nextEmit < len(src) {
					dst = emitLiteral(dst, src[nextEmit:])
				}
				return dst
			}
			candidate = int(table[nextHash&tableMask])
			table[nextHash&tableMask] = uint16(s)
			nextHash = hash(load32(src, nextS), shift)
			if load32(src, s) == load32(src, candidate) {
				break
			}
		}

		// A four-byte match was found. The bytes before it are a
		// literal.
		dst = emitLiteral(dst, src[nextEmit:s])

		for {
			// The match is at least four bytes; extend it.
			base := s
			s += 4
			for i := candidate + 4; s < len(src) && src[i] == src[s]; i, s = i+1, s+1 {
			}

			dst = emitCopy(dst, base-candidate, s-base)
			nextEmit = s
			if s >= sLimit {
				if nextEmit < len(src) {
					dst = emitLiteral(dst, src[nextEmit:])
				}
				return dst
			}

			// Update the table at s-1 and s, so a match right after
			// this one is found.
			x := load64(src, s-1)
			prevHash := hash(uint32(x>>0), shift)
			table[prevHash&tableMask] = uint16(s - 1)
			currHash := hash(uint32(x>>8), shift)
			candidate = int(table[currHash&tableMask])
			table[currHash&tableMask] = uint16(s)
			if load32(src, s) != load32(src, candidate) {
				s++
				break
			}
		}
	}
}

// load32 reads four bytes as a little-endian uint32.
func load32(b []byte, i int) uint32 {
	return uint32(b[i]) | uint32(b[i+1])<<8 | uint32(b[i+2])<<16 | uint32(b[i+3])<<24
}

// load64 reads eight bytes as a little-endian uint64.
func load64(b []byte, i int) uint64 {
	return uint64(load32(b, i)) | uint64(load32(b, i+4))<<32
}

// hash is the block format's hash function.
func hash(u, shift uint32) uint32 {
	return (u * 0x1e35a7bd) >> shift
}
