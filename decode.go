package az

import (
	"encoding/binary"
	"fmt"

	"az/pkg/fse"
	"az/pkg/huff0"
)

// decompressBlock dispatches to the correct decoder based on the block-type
// byte (first byte of the compressed block body).
// dst must be pre-allocated to the expected uncompressed size, or nil to let
// the decoder allocate.
func decompressBlock(src []byte, uncompressedHint int) ([]byte, error) {
	if len(src) == 0 {
		return nil, ErrCorrupted
	}
	blockType := src[0]
	src = src[1:]

	switch blockType {
	case 0x00:
		return decodeBlockRaw(src, uncompressedHint)
	case 0x01:
		return decodeBlockHuffRaw(src, uncompressedHint)
	case 0x02:
		return decodeBlockFull(src, uncompressedHint)
	case 0x03:
		return decodeRLE(src)
	default:
		return nil, fmt.Errorf("%w: unknown block type 0x%02x", ErrCorrupted, blockType)
	}
}

// ─── Block type 0x00: raw literals + raw token sequences ────────────────────

func decodeBlockRaw(src []byte, hint int) ([]byte, error) {
	if len(src) < 3 {
		return nil, ErrCorrupted
	}
	litLen := int(src[0]) | int(src[1])<<8 | int(src[2])<<16
	src = src[3:]
	if len(src) < litLen {
		return nil, ErrCorrupted
	}
	lits := src[:litLen]
	seqSrc := src[litLen:]
	return decodeRawSeqs(lits, seqSrc, hint, true)
}

// ─── Block type 0x01: Huffman literals + raw token sequences ────────────────

func decodeBlockHuffRaw(src []byte, hint int) ([]byte, error) {
	if len(src) < 6 {
		return nil, ErrCorrupted
	}
	litLen := int(src[0]) | int(src[1])<<8 | int(src[2])<<16
	huffSize := int(src[3]) | int(src[4])<<8 | int(src[5])<<16
	src = src[6:]
	if len(src) < huffSize {
		return nil, ErrCorrupted
	}
	huffSrc := src[:huffSize]
	seqSrc := src[huffSize:]

	sc, remain, err := huff0.ReadTable(huffSrc, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: huff0 read table: %v", ErrCorrupted, err)
	}
	sc.MaxDecodedSize = litLen
	lits, err := sc.Decompress1X(remain)
	if err != nil {
		return nil, fmt.Errorf("%w: huff0 decompress: %v", ErrCorrupted, err)
	}
	if len(lits) != litLen {
		return nil, ErrCorrupted
	}

	return decodeRawSeqs(lits, seqSrc, hint, true)
}

// decodeRawSeqs reconstructs the output from a literal buffer and a
// sequence stream produced by buildRawSeqBlock.
// largeWindow=true means offsets are 4 bytes; false means 2 bytes.
func decodeRawSeqs(lits, seqSrc []byte, hint int, largeWindow bool) ([]byte, error) {
	dst := make([]byte, 0, hint)
	litPos := 0
	var recent [3]uint32 = [3]uint32{1, 4, 8}

	for len(seqSrc) > 0 {
		flag := seqSrc[0]
		seqSrc = seqSrc[1:]

		isLast := flag&0x80 != 0
		if isLast {
			// Remaining literals
			dst = append(dst, lits[litPos:]...)
			break
		}

		isRep := flag&0x40 != 0
		repIdx := int(flag & 0x03)

		// Token byte
		if len(seqSrc) < 1 {
			return nil, ErrCorrupted
		}
		token := seqSrc[0]
		seqSrc = seqSrc[1:]
		litTag := int(token >> 4)
		matchTag := int(token & 0x0F)

		// Literal length
		lLen := litTag
		if litTag == 15 {
			for len(seqSrc) > 0 {
				extra := int(seqSrc[0])
				seqSrc = seqSrc[1:]
				lLen += extra
				if extra < 255 {
					break
				}
			}
		}

		// Match length
		mLenEx := matchTag
		if matchTag == 15 {
			for len(seqSrc) > 0 {
				extra := int(seqSrc[0])
				seqSrc = seqSrc[1:]
				mLenEx += extra
				if extra < 255 {
					break
				}
			}
		}
		mLen := mLenEx + minMatch

		// Emit literals
		if litPos+lLen > len(lits) {
			return nil, ErrCorrupted
		}
		dst = append(dst, lits[litPos:litPos+lLen]...)
		litPos += lLen

		// Decode offset
		var offset int
		if isRep {
			if repIdx > 2 {
				return nil, ErrCorrupted
			}
			offset = int(recent[repIdx])
			// LRU rotate: bring repIdx to front
			for i := repIdx; i > 0; i-- {
				recent[i] = recent[i-1]
			}
			recent[0] = uint32(offset)
		} else {
			if !largeWindow {
				if len(seqSrc) < 2 {
					return nil, ErrCorrupted
				}
				offset = int(seqSrc[0]) | int(seqSrc[1])<<8
				seqSrc = seqSrc[2:]
			} else {
				if len(seqSrc) < 4 {
					return nil, ErrCorrupted
				}
				offset = int(binary.LittleEndian.Uint32(seqSrc[:4]))
				seqSrc = seqSrc[4:]
			}
			// Update recent offsets
			recent[2] = recent[1]
			recent[1] = recent[0]
			recent[0] = uint32(offset)
		}

		if offset <= 0 || offset > len(dst) {
			return nil, ErrCorrupted
		}

		// Copy match (may overlap)
		dst = copyMatch(dst, offset, mLen)
	}

	return dst, nil
}

// ─── Block type 0x02: Huffman literals + FSE sequences ──────────────────────

func decodeBlockFull(src []byte, hint int) ([]byte, error) {
	if len(src) < 6 {
		return nil, ErrCorrupted
	}

	litLen := int(src[0]) | int(src[1])<<8 | int(src[2])<<16
	huffSize := int(src[3]) | int(src[4])<<8 | int(src[5])<<16
	src = src[6:]

	var lits []byte
	if huffSize == 0 {
		// Raw literals: litLen bytes follow directly.
		if len(src) < litLen {
			return nil, ErrCorrupted
		}
		lits = src[:litLen]
		src = src[litLen:]
	} else {
		if len(src) < huffSize {
			return nil, ErrCorrupted
		}
		huffSrc := src[:huffSize]
		src = src[huffSize:]

		// Decode Huffman-compressed literals.
		hsc, remain, err := huff0.ReadTable(huffSrc, nil)
		if err != nil {
			return nil, fmt.Errorf("%w: huff0 read table: %v", ErrCorrupted, err)
		}
		hsc.MaxDecodedSize = litLen
		if litLen >= 1024 {
			lits, err = hsc.Decompress4X(remain, litLen)
		} else {
			lits, err = hsc.Decompress1X(remain)
		}
		if err != nil {
			return nil, fmt.Errorf("%w: huff0 decompress: %v", ErrCorrupted, err)
		}
		if len(lits) != litLen {
			return nil, ErrCorrupted
		}
	}

	// Sequence count (uint24 LE)
	if len(src) < 3 {
		return nil, ErrCorrupted
	}
	seqCount := int(src[0]) | int(src[1])<<8 | int(src[2])<<16
	src = src[3:]

	if seqCount == 0 {
		return lits, nil
	}

	// litLen values: seqCount * 3 bytes (uint24 LE each), no size prefix.
	llBufSize := seqCount * 3
	if len(src) < llBufSize {
		return nil, ErrCorrupted
	}
	llBuf := src[:llBufSize]
	src = src[llBufSize:]

	// matchLen-excess values: seqCount * 3 bytes (uint24 LE each), no size prefix.
	mlBufSize := seqCount * 3
	if len(src) < mlBufSize {
		return nil, ErrCorrupted
	}
	mlBuf := src[:mlBufSize]
	src = src[mlBufSize:]

	// Offset code stream: size-prefixed, high bit of size = isRaw flag.
	ofCodes, src, err := readSeqStream(src, seqCount)
	if err != nil {
		return nil, fmt.Errorf("%w: fse offset: %v", ErrCorrupted, err)
	}

	// Decode sequences
	dst := make([]byte, 0, hint)
	litPos := 0
	var recent [3]uint32 = [3]uint32{1, 4, 8}

	for i := 0; i < seqCount; i++ {
		base := i * 3
		lLen := int(llBuf[base]) | int(llBuf[base+1])<<8 | int(llBuf[base+2])<<16
		mleRaw := int(mlBuf[base]) | int(mlBuf[base+1])<<8 | int(mlBuf[base+2])<<16
		mLen := mleRaw + minMatch
		ofCode := ofCodes[i]

		var offset int
		if ofCode < 3 {
			// rep match
			idx := int(ofCode)
			offset = int(recent[idx])
			// rotate LRU
			if idx > 0 {
				recent[idx], recent[idx-1] = recent[idx-1], recent[idx]
			}
		} else {
			// code = bit_length(offset) + 2; base = 1 << (code-3); extra bits = code-3
			extraBits := int(ofCode) - 3
			var extra uint32
			if extraBits > 0 {
				if len(src) < 4 {
					return nil, ErrCorrupted
				}
				extra = binary.LittleEndian.Uint32(src[:4]) & ((1 << extraBits) - 1)
				src = src[4:]
			}
			base := uint32(1) << extraBits
			offset = int(base + extra)
			recent[2] = recent[1]
			recent[1] = recent[0]
			recent[0] = uint32(offset)
		}

		if litPos+lLen > len(lits) {
			return nil, ErrCorrupted
		}
		dst = append(dst, lits[litPos:litPos+lLen]...)
		litPos += lLen

		if offset <= 0 || offset > len(dst) {
			return nil, ErrCorrupted
		}
		dst = copyMatch(dst, offset, mLen)
	}

	// Remaining literals
	dst = append(dst, lits[litPos:]...)
	return dst, nil
}

// readSeqStream reads a 3-byte-prefixed sequence code stream.
// High bit of byte[2] (bit 23) signals raw storage; lower 23 bits are the byte count.
// Returns (codes, remaining src, error).
func readSeqStream(src []byte, count int) ([]byte, []byte, error) {
	if len(src) < 3 {
		return nil, src, ErrCorrupted
	}
	sw := uint32(src[0]) | uint32(src[1])<<8 | uint32(src[2])<<16
	src = src[3:]
	isRaw := sw&0x800000 != 0
	size := int(sw &^ 0x800000)
	if len(src) < size {
		return nil, src, ErrCorrupted
	}
	data := src[:size]
	src = src[size:]
	if isRaw {
		if size != count {
			return nil, src, ErrCorrupted
		}
		return data, src, nil
	}
	codes, err := fseDecompress(data, count)
	if err != nil {
		return nil, src, err
	}
	return codes, src, nil
}

// fseDecompress decodes FSE-coded byte symbols from src.
func fseDecompress(src []byte, count int) ([]byte, error) {
	var sc fse.Scratch
	out, err := fse.Decompress(src, &sc)
	if err != nil {
		return nil, err
	}
	if len(out) != count {
		return nil, ErrCorrupted
	}
	return out, nil
}

// decodeSeqValue maps a packed sequence code byte to its decoded value.
// Literal lengths and match lengths are stored directly in the byte (0-255).
func decodeSeqValue(code byte) uint32 {
	return uint32(code)
}

// ─── Block type 0x03: RLE ────────────────────────────────────────────────────

func decodeRLE(src []byte) ([]byte, error) {
	if len(src) < 4 {
		return nil, ErrCorrupted
	}
	b := src[0]
	count := int(src[1]) | int(src[2])<<8 | int(src[3])<<16
	out := make([]byte, count)
	for i := range out {
		out[i] = b
	}
	return out, nil
}

// ─── Match copy helper ────────────────────────────────────────────────────────

// copyMatch appends mLen bytes to dst by copying from offset bytes back.
// Handles overlapping copies (offset < mLen) correctly.
func copyMatch(dst []byte, offset, mLen int) []byte {
	start := len(dst) - offset
	if start < 0 {
		// Should not happen with valid data
		return dst
	}
	// Overlap-safe copy
	for mLen > 0 {
		n := mLen
		avail := len(dst) - start
		if n > avail {
			n = avail
		}
		dst = append(dst, dst[start:start+n]...)
		start += n // advance for potential next iteration (repeating pattern)
		mLen -= n
	}
	return dst
}
