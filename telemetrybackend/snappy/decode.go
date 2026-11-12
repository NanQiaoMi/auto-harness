package snappy

import (
	"encoding/binary"
)

// Decode decompresses a snappy block.
//
// It is the inverse of Encode. The harness needs it to verify what it
// sends: the bytes that leave the process are the bytes the collector
// decodes.
func Decode(src []byte) ([]byte, error) {
	length, headerLen := binary.Uvarint(src)
	if headerLen <= 0 {
		return nil, ErrCorrupt
	}
	if length > 0xffffffff {
		return nil, ErrCorrupt
	}
	dst := make([]byte, 0, length)

	src = src[headerLen:]
	for len(src) > 0 {
		tag := src[0]
		switch tag & 0x03 {
		case tagLiteral:
			n := int(tag >> 2)
			switch {
			case n < 60:
				src = src[1:]
			case n == 60:
				if len(src) < 2 {
					return nil, ErrCorrupt
				}
				n = int(src[1])
				src = src[2:]
			case n == 61:
				if len(src) < 3 {
					return nil, ErrCorrupt
				}
				n = int(src[1]) | int(src[2])<<8
				src = src[3:]
			case n == 62:
				if len(src) < 4 {
					return nil, ErrCorrupt
				}
				n = int(src[1]) | int(src[2])<<8 | int(src[3])<<16
				src = src[4:]
			default:
				return nil, ErrCorrupt
			}
			// The length in the tag is one less than the count.
			n++
			if n > len(src) {
				return nil, ErrCorrupt
			}
			dst = append(dst, src[:n]...)
			src = src[n:]

		case tagCopy1:
			if len(src) < 2 {
				return nil, ErrCorrupt
			}
			length := 4 + int(src[0]>>2)&0x7
			offset := int(uint32(src[0])&0xe0<<3 | uint32(src[1]))
			src = src[2:]
			if err := copyMatch(&dst, offset, length); err != nil {
				return nil, err
			}

		case tagCopy2:
			if len(src) < 3 {
				return nil, ErrCorrupt
			}
			length := 1 + int(src[0]>>2)
			offset := int(src[1]) | int(src[2])<<8
			src = src[3:]
			if err := copyMatch(&dst, offset, length); err != nil {
				return nil, err
			}

		case tagCopy4:
			if len(src) < 5 {
				return nil, ErrCorrupt
			}
			length := 1 + int(src[0]>>2)
			offset := int(src[1]) | int(src[2])<<8 | int(src[3])<<16 | int(src[4])<<24
			src = src[5:]
			if err := copyMatch(&dst, offset, length); err != nil {
				return nil, err
			}
		}
	}
	return dst, nil
}

// copyMatch appends length bytes taken from offset bytes back.
//
// The copy may overlap itself: the bytes are appended one at a time,
// so a copy of "ababab" from an offset of 2 repeats correctly.
func copyMatch(dst *[]byte, offset, length int) error {
	if offset <= 0 || offset > len(*dst) {
		return ErrCorrupt
	}
	start := len(*dst) - offset
	for i := 0; i < length; i++ {
		*dst = append(*dst, (*dst)[start+i])
	}
	return nil
}

// DecodedLen returns the length of the decoded block.
func DecodedLen(src []byte) (int, error) {
	length, headerLen := binary.Uvarint(src)
	if headerLen <= 0 || length > 0xffffffff {
		return 0, ErrCorrupt
	}
	return int(length), nil
}
