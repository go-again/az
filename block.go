package az

import (
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

	// Probe compressibility before expensive encoding.
	// matchFrac==0: skip LZ entirely but still apply Huffman — already-LZ-compressed
	// data (e.g. .gz/.compress) has no 4-byte repeats but can have skewed byte
	// distributions where Huffman saves 10–15%. Returning raw would be worse than L2.
	if level >= Level3 {
		matchFrac := probeCompressibility(src)
		if matchFrac == 0 {
			litSeq := [1]sequence{{litLen: uint32(len(src))}}
			compressed := buildFSEBlock(src, litSeq[:], levelConfigs[level], st)
			if len(compressed) >= len(src) {
				return src, true
			}
			return compressed, false
		}
		if level == Level5 && matchFrac < 8 {
			level = Level3
		}
	}

	var compressed []byte

	switch level {
	case Level1:
		compressed = encodeL1(src, st)
	case Level2:
		litBuf, seqs := encodeL2(src, st)
		compressed = buildFSEBlock(litBuf, seqs, levelConfigs[Level2], st)
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

// probeCompressibility samples the block and returns an estimate of match density.
// Uses a 1K-entry hash table over the first 4KB (or full block if shorter).
// Returns matches-per-64-positions (0 = no matches found; higher = more compressible).
// A return value of 0 means the block is likely incompressible and should be stored raw.
func probeCompressibility(src []byte) int {
	const (
		probeBits = 10       // 1K entries
		probeMask = (1 << probeBits) - 1
		probeLen  = 4 << 10 // probe first 4KB
		step      = 4       // sample every 4th byte position
	)
	probe := src
	if len(probe) > probeLen {
		probe = src[:probeLen]
	}
	if len(probe) < minMatch {
		return 64
	}
	var htab [1 << probeBits]int32
	for i := range htab {
		htab[i] = -1
	}
	matches := 0
	positions := 0
	n := len(probe) - minMatch + 1
	for i := 0; i < n; i += step {
		positions++
		v := uint32(probe[i]) | uint32(probe[i+1])<<8 | uint32(probe[i+2])<<16 | uint32(probe[i+3])<<24
		h := int((v * 2654435761) >> (32 - probeBits) & probeMask)
		prev := htab[h]
		htab[h] = int32(i)
		if prev >= 0 {
			p := int(prev)
			if probe[i] == probe[p] && probe[i+1] == probe[p+1] &&
				probe[i+2] == probe[p+2] && probe[i+3] == probe[p+3] {
				matches++
			}
		}
	}
	if positions == 0 {
		return 0
	}
	return matches * 64 / positions
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
//   llCodes stream:    size-prefixed (see appendSeqStream); 1 byte per seq, FSE or raw
//   mlCodes stream:    size-prefixed; 1 byte per seq, FSE or raw
//   ofCodes stream:    size-prefixed; 1 byte per seq, FSE or raw
//   extraBitsLen:      uint24 LE byte count of packed bit stream
//   extraBitsBuf:      packed LE bits, per seq: ll_extra || ml_extra || of_extra
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

	// FSE has ~60 bytes of fixed overhead per block (3 stream headers + tables).
	// For very few sequences the raw-seq format is more compact. Fall back to
	// huff-seq (block type 0x01) which uses 6 bytes/seq with no stream overhead.
	// Break-even: FSE overhead (60) < (6−1) × seqCount → seqCount > 12.
	if seqCount < 12 {
		useHuff := !litIsRaw
		return buildRawSeqBlock(nil, seqs, litBuf, huffOut, useHuff)
	}

	// Build compact code streams + packed extra bits.
	llCodes := make([]byte, 0, seqCount)
	mlCodes := make([]byte, 0, seqCount)
	ofCodes := make([]byte, 0, seqCount)
	var ebw extraBitWriter
	ebw.reset(make([]byte, 0, seqCount*2))

	for _, s := range seqs {
		if s.matchLen == 0 {
			continue
		}

		// litLen code
		llCode, llExtra := llToCode(s.litLen)
		llCodes = append(llCodes, llCode)

		// matchLen excess code
		mlCode, mlExtra := mlToCode(s.matchLen - minMatch)
		mlCodes = append(mlCodes, mlCode)

		// offset code
		ofCode := seqToOFCode(s.offset)
		ofCodes = append(ofCodes, ofCode)

		// Pack extra bits: ll_extra, then ml_extra, then of_extra.
		// flush32 after each group that can produce up to 16 bits (ll),
		// 21 bits (ml), or 29 bits (of) to prevent uint64 overflow.
		if n := uint(llBits[llCode]); n > 0 {
			ebw.addBits(uint64(llExtra), n)
		}
		ebw.flush32()
		if n := uint(mlBits[mlCode]); n > 0 {
			ebw.addBits(uint64(mlExtra), n)
		}
		ebw.flush32()
		if ofCode >= 3 {
			if n := uint(ofCode) - 3; n > 0 {
				mask := uint64((1 << n) - 1)
				ebw.addBits(uint64(s.offset)&mask, n)
			}
		}
		ebw.flush32()
	}
	ebw.flushFinal()
	extraBitsBuf := ebw.out

	// FSE-compress all three code streams independently.
	// Each stream must use its own Scratch so FSE never emits a "reuse table"
	// marker (which would require shared state between encoder and decoder).
	var llFsc, mlFsc, ofFsc fse.Scratch
	llComp, llIsRaw := compressSequenceCodes(llCodes, &llFsc)
	mlComp, mlIsRaw := compressSequenceCodes(mlCodes, &mlFsc)
	ofComp, ofIsRaw := compressSequenceCodes(ofCodes, &ofFsc)

	// Assemble output
	dst := make([]byte, 0, len(litBuf)+len(llComp)+len(mlComp)+len(ofComp)+len(extraBitsBuf)+64)

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
		dst = appendSeqStream(dst, llComp, llIsRaw)  // llCodes
		dst = appendSeqStream(dst, mlComp, mlIsRaw)  // mlCodes
		dst = appendSeqStream(dst, ofComp, ofIsRaw)  // ofCodes

		// Packed extra bits
		ebLen := len(extraBitsBuf)
		dst = append(dst, byte(ebLen), byte(ebLen>>8), byte(ebLen>>16))
		dst = append(dst, extraBitsBuf...)
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

// decompressBlockData decompresses one block. If isRaw, src is returned as-is.
func decompressBlockData(src []byte, isRaw bool, uncompressedHint int) ([]byte, error) {
	if isRaw {
		out := make([]byte, len(src))
		copy(out, src)
		return out, nil
	}
	return decompressBlock(src, uncompressedHint)
}
