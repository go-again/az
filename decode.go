package az

import (
	"encoding/binary"
	"fmt"

	"az/pkg/fse"
	"az/pkg/huff0"
)

// decompressBlock dispatches to the correct decoder based on the block-type
// byte (first byte of the compressed block body).
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
	case 0x04:
		return decodeBlockCompact(src, uncompressedHint)
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
// sequence stream produced by buildRawSeqBlock (block types 0x00 and 0x01).
// largeWindow=true means offsets are 4 bytes.
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
			recent[2] = recent[1]
			recent[1] = recent[0]
			recent[0] = uint32(offset)
		}

		if offset <= 0 || offset > len(dst) {
			return nil, ErrCorrupted
		}

		dst = copyMatch(dst, offset, mLen)
	}

	return dst, nil
}

// ─── Block type 0x02: Huffman literals + FSE sequences ──────────────────────

// decodeBlockFull decodes a block-type-0x02 payload.
//
// Sequence section format:
//   [seqCount:u24]
//   [llCodes: 3-byte-prefixed, FSE or raw]
//   [mlCodes: 3-byte-prefixed, FSE or raw]
//   [ofCodes: 3-byte-prefixed, FSE or raw]
//   [extraBitsLen:u24]
//   [extraBitsBuf: packed LE bits, per seq: ll_extra || ml_extra || of_extra]
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

	// Read three FSE/raw code streams: ll, ml, of
	var err error
	llCodes, src, err := readSeqStream(src, seqCount)
	if err != nil {
		return nil, fmt.Errorf("%w: fse llCodes: %v", ErrCorrupted, err)
	}
	mlCodes, src, err := readSeqStream(src, seqCount)
	if err != nil {
		return nil, fmt.Errorf("%w: fse mlCodes: %v", ErrCorrupted, err)
	}
	ofCodes, src, err := readSeqStream(src, seqCount)
	if err != nil {
		return nil, fmt.Errorf("%w: fse ofCodes: %v", ErrCorrupted, err)
	}

	// Read packed extra bits
	if len(src) < 3 {
		return nil, ErrCorrupted
	}
	ebLen := int(src[0]) | int(src[1])<<8 | int(src[2])<<16
	src = src[3:]
	if len(src) < ebLen {
		return nil, ErrCorrupted
	}
	var ebr extraBitReader
	ebr.init(src[:ebLen])
	// src beyond the extra bits is ignored (should be empty for a well-formed block)

	// Decode sequences
	dst := make([]byte, 0, hint)
	litPos := 0
	var recent [3]uint32 = [3]uint32{1, 4, 8}

	for i := 0; i < seqCount; i++ {
		llCode := llCodes[i]
		mlCode := mlCodes[i]
		ofCode := ofCodes[i]

		// Decode litLen
		if int(llCode) >= len(llBase) {
			return nil, ErrCorrupted
		}
		lLen := int(llBase[llCode])
		if n := uint(llBits[llCode]); n > 0 {
			extra, e := ebr.readBits(n)
			if e != nil {
				return nil, e
			}
			lLen += int(extra)
		}

		// Decode matchLen excess → mLen
		if int(mlCode) >= len(mlBase) {
			return nil, ErrCorrupted
		}
		mlExcess := int(mlBase[mlCode])
		if n := uint(mlBits[mlCode]); n > 0 {
			extra, e := ebr.readBits(n)
			if e != nil {
				return nil, e
			}
			mlExcess += int(extra)
		}
		mLen := mlExcess + minMatch

		// Decode offset
		var offset int
		if ofCode < 3 {
			// Rep match (not produced by current encoder but handled for completeness)
			idx := int(ofCode)
			offset = int(recent[idx])
			if idx > 0 {
				recent[idx], recent[idx-1] = recent[idx-1], recent[idx]
			}
		} else {
			extraBits := uint(ofCode) - 3
			base := uint32(1) << extraBits
			var extra uint32
			if extraBits > 0 {
				extra, err = ebr.readBits(extraBits)
				if err != nil {
					return nil, err
				}
			}
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

// ─── Block type 0x04: compact raw seqs (no flag byte, 3-byte offset) ─────────

// decodeBlockCompact decodes a block-type-0x04 payload produced by buildCompactSeqBlock.
// Format: [litLen:u24][lits][seqCount:u24] then per-seq [token][overflow...][offset:3].
// After seqCount sequences, remaining lits are appended as trailing literals.
func decodeBlockCompact(src []byte, hint int) ([]byte, error) {
	if len(src) < 3 {
		return nil, ErrCorrupted
	}
	litLen := int(src[0]) | int(src[1])<<8 | int(src[2])<<16
	src = src[3:]
	if len(src) < litLen {
		return nil, ErrCorrupted
	}
	lits := src[:litLen]
	src = src[litLen:]

	if len(src) < 3 {
		return nil, ErrCorrupted
	}
	seqCount := int(src[0]) | int(src[1])<<8 | int(src[2])<<16
	src = src[3:]

	dst := make([]byte, 0, hint)
	litPos := 0

	for i := 0; i < seqCount; i++ {
		if len(src) < 1 {
			return nil, ErrCorrupted
		}
		token := src[0]
		src = src[1:]

		litTag := int(token >> 4)
		matchTag := int(token & 0x0F)

		lLen := litTag
		if litTag == 15 {
			for len(src) > 0 {
				extra := int(src[0])
				src = src[1:]
				lLen += extra
				if extra < 255 {
					break
				}
			}
		}

		mLenEx := matchTag
		if matchTag == 15 {
			for len(src) > 0 {
				extra := int(src[0])
				src = src[1:]
				mLenEx += extra
				if extra < 255 {
					break
				}
			}
		}
		mLen := mLenEx + minMatch

		if len(src) < 3 {
			return nil, ErrCorrupted
		}
		offset := int(src[0]) | int(src[1])<<8 | int(src[2])<<16
		src = src[3:]

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

	dst = append(dst, lits[litPos:]...)
	return dst, nil
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
		return dst
	}
	for mLen > 0 {
		n := mLen
		avail := len(dst) - start
		if n > avail {
			n = avail
		}
		dst = append(dst, dst[start:start+n]...)
		start += n
		mLen -= n
	}
	return dst
}
