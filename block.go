package az

import (
	"encoding/binary"

	"az/pkg/fse"
	"az/pkg/huff0"
)

// Ensure huff0 import is used (the Compress4X path in buildFSEBlock needs it indirectly
// via entropy.go, but we reference it here to satisfy the import).
var _ = huff0.ErrIncompressible

// compressBlock compresses a single block of src using the given level.
// Returns (compressed, isRaw) where isRaw=true means the block should be
// stored uncompressed (incompressible input).
func compressBlock(src []byte, level Level, st *encoderState) ([]byte, bool) {
	if level < minLevel || level > maxLevel {
		return src, true
	}

	// Fast-path: detect RLE
	if rle, b := detectRLE(src); rle {
		return buildRLEBlock(b, len(src)), false
	}

	var compressed []byte

	switch level {
	case Level1:
		compressed = encodeL1(src, st)
	case Level2:
		compressed = encodeL2(src, st)
	default: // Level3, Level4, Level5
		var encode func([]byte, *encoderState) ([]byte, []sequence)
		switch level {
		case Level3:
			encode = encodeL3
		case Level4:
			encode = encodeL4
		default:
			encode = encodeL5
		}
		litBuf, seqs := encode(src, st)
		compressed = buildFSEBlock(litBuf, seqs, levelConfigs[level], st)
	}

	// If compressed is not smaller, store raw
	if len(compressed) >= len(src) {
		return src, true
	}
	return compressed, false
}

// detectRLE returns true if all bytes in src equal b.
func detectRLE(src []byte) (bool, byte) {
	if len(src) == 0 {
		return false, 0
	}
	b := src[0]
	for _, v := range src[1:] {
		if v != b {
			return false, 0
		}
	}
	return true, b
}

// buildRLEBlock creates a 5-byte block-type-0x03 payload.
func buildRLEBlock(b byte, count int) []byte {
	return []byte{
		0x03,
		b,
		byte(count),
		byte(count >> 8),
		byte(count >> 16),
	}
}

// buildFSEBlock assembles a block-type-0x02 payload from litBuf and seqs.
//
// Literal encoding: huffSize=0 means raw literals follow directly; huffSize>0
// means the literals are Huffman-compressed (table+data packed by huff0).
//
// Sequence layout (when seqCount>0):
//   litLen stream:  seqCount × 3 bytes, each a uint24 LE literal length
//   matchLen stream: seqCount × 3 bytes, each a uint24 LE (matchLen - minMatch)
//   offsetCode stream: size-prefixed (see appendSeqStream); 1 byte per seq,
//                      FSE-coded or raw with high-bit flag in size word.
//   offsetExtras: seqCount × 0 or 4 bytes; 4-byte LE extra bits for codes ≥ 3.
func buildFSEBlock(litBuf []byte, seqs []sequence, cfg levelConfig, st *encoderState) []byte {
	// Compress literals
	huffOut, litIsRaw := compressLiterals(litBuf, cfg.litMode, st.hScratch)

	// Count real sequences (those with a back-reference)
	seqCount := 0
	for _, s := range seqs {
		if s.matchLen > 0 {
			seqCount++
		}
	}

	// Build per-sequence streams.
	// litLen and matchLen are stored as full uint24 values (no code table needed).
	llBuf := make([]byte, 0, seqCount*3)
	mlBuf := make([]byte, 0, seqCount*3)
	ofCodes := make([]byte, 0, seqCount)
	var ofExtras []byte

	for _, s := range seqs {
		if s.matchLen == 0 {
			continue
		}
		ll := s.litLen
		llBuf = append(llBuf, byte(ll), byte(ll>>8), byte(ll>>16))
		mle := s.matchLen - minMatch
		mlBuf = append(mlBuf, byte(mle), byte(mle>>8), byte(mle>>16))
		ofc := seqToOFCode(s.offset)
		ofCodes = append(ofCodes, ofc)
		if ofc >= 3 {
			// code = bit_length(offset) + 2; extra bits = code - 3
			extraBits := int(ofc) - 3
			if extraBits > 0 {
				mask := uint32((1 << extraBits) - 1)
				extra := s.offset & mask
				ofExtras = binary.LittleEndian.AppendUint32(ofExtras, extra)
			}
		}
	}

	// FSE-compress only the offset code stream.
	var fsc fse.Scratch
	ofComp, ofIsRaw := compressSequenceCodes(ofCodes, &fsc)

	// Assemble output
	dst := make([]byte, 0, len(litBuf)+len(llBuf)+len(mlBuf)+len(ofComp)+64)

	dst = append(dst, 0x02) // block type

	// Literal count (uint24 LE)
	litLen := len(litBuf)
	dst = append(dst, byte(litLen), byte(litLen>>8), byte(litLen>>16))

	if litIsRaw {
		// huffSize=0 signals raw literals: litLen bytes follow directly.
		dst = append(dst, 0, 0, 0)
		dst = append(dst, litBuf...)
	} else {
		// huffSize>0: Huffman-compressed literal data (table + bitstream). uint24 LE.
		dst = append(dst, byte(len(huffOut)), byte(len(huffOut)>>8), byte(len(huffOut)>>16))
		dst = append(dst, huffOut...)
	}

	// Sequence count (uint24 LE)
	dst = append(dst, byte(seqCount), byte(seqCount>>8), byte(seqCount>>16))

	if seqCount > 0 {
		// litLen values: seqCount * 3 bytes, no size prefix (size is deterministic)
		dst = append(dst, llBuf...)
		// matchLen-excess values: seqCount * 3 bytes, no size prefix
		dst = append(dst, mlBuf...)
		// Offset code stream: size-prefixed, high bit = isRaw
		dst = appendSeqStream(dst, ofComp, ofIsRaw)
		// Offset extra bits
		dst = append(dst, ofExtras...)
	}

	return dst
}

// appendSeqStream writes a 3-byte length-prefixed sequence code stream.
// High bit of byte[2] (bit 23) is set when the data is stored raw (not FSE).
func appendSeqStream(dst, data []byte, isRaw bool) []byte {
	sz := uint32(len(data))
	if isRaw {
		sz |= 0x800000
	}
	dst = append(dst, byte(sz), byte(sz>>8), byte(sz>>16))
	return append(dst, data...)
}

// decompressBlockData decompresses one block.  If isRaw, src is returned as-is.
func decompressBlockData(src []byte, isRaw bool, uncompressedHint int) ([]byte, error) {
	if isRaw {
		out := make([]byte, len(src))
		copy(out, src)
		return out, nil
	}
	return decompressBlock(src, uncompressedHint)
}

