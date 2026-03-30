package az

import (
	"math/bits"

	"az/pkg/fse"
	"az/pkg/huff0"
)

// ─── Literal-length code table (36 codes, zstd-compatible) ──────────────────

// llBase[c] is the base litLen value for code c.
// llBits[c] is the number of extra bits needed to reconstruct the exact value.
var llBase = [36]uint32{
	0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15,
	16, 18, 20, 22, 24, 28, 32, 40, 48, 64, 128, 256, 512, 1024,
	2048, 4096, 8192, 16384, 32768, 65536,
}
var llBits = [36]uint8{
	0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
	1, 1, 1, 1, 2, 2, 3, 3, 4, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16,
}

// ─── Match-length excess code table (53 codes) ───────────────────────────────

// mlBase[c] is the base excess (matchLen - minMatch) for code c.
// mlBits[c] is the number of extra bits needed.
var mlBase = [53]uint32{
	0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15,
	16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31,
	32, 34, 36, 40, 48, 64, 128, 256, 512, 1024, 2048, 4096,
	8192, 16384, 32768, 65536, 131072, 262144, 524288, 1048576, 2097152,
}
var mlBits = [53]uint8{
	0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
	0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
	1, 1, 2, 3, 4, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21,
}

// Precomputed direct tables for values 0..255 (cover the common case).
var llCodeTable [256]uint8
var mlCodeTable [256]uint8

func init() {
	// llCodeTable[v] = largest code c such that llBase[c] <= v
	ci := 0
	for v := 0; v < 256; v++ {
		for ci+1 < len(llBase) && llBase[ci+1] <= uint32(v) {
			ci++
		}
		llCodeTable[v] = uint8(ci)
	}
	ci = 0
	for v := 0; v < 256; v++ {
		for ci+1 < len(mlBase) && mlBase[ci+1] <= uint32(v) {
			ci++
		}
		mlCodeTable[v] = uint8(ci)
	}
}

// llToCode maps a literal length to its compact code and extra bits.
func llToCode(lLen uint32) (code uint8, extra uint32) {
	if lLen < 256 {
		code = llCodeTable[lLen]
	} else {
		// Binary search for lLen >= 256 (codes 24+)
		lo, hi := 24, 35
		for lo < hi {
			mid := (lo + hi + 1) / 2
			if llBase[mid] <= lLen {
				lo = mid
			} else {
				hi = mid - 1
			}
		}
		code = uint8(lo)
	}
	extra = lLen - llBase[code]
	return
}

// mlToCode maps a match-length excess (matchLen - minMatch) to its code and extra bits.
func mlToCode(excess uint32) (code uint8, extra uint32) {
	if excess < 256 {
		code = mlCodeTable[excess]
	} else {
		// Binary search for excess >= 256 (codes 38+)
		lo, hi := 38, 52
		for lo < hi {
			mid := (lo + hi + 1) / 2
			if mlBase[mid] <= excess {
				lo = mid
			} else {
				hi = mid - 1
			}
		}
		code = uint8(lo)
	}
	extra = excess - mlBase[code]
	return
}

// ─── extraBitWriter ───────────────────────────────────────────────────────────

// extraBitWriter packs variable-width values LSB-first into a byte slice.
// It is a simple forward bit accumulator, distinct from the FSE reverse bitwriter.
type extraBitWriter struct {
	buf   uint64
	nBits uint
	out   []byte
}

func (w *extraBitWriter) reset(dst []byte) {
	w.buf = 0
	w.nBits = 0
	w.out = dst
}

// addBits appends the low n bits of val to the stream (LSB first).
// Call flush32 when accumulated bits might exceed 56.
func (w *extraBitWriter) addBits(val uint64, n uint) {
	w.buf |= val << w.nBits
	w.nBits += n
}

// flush32 writes 4 bytes when at least 32 bits are buffered.
func (w *extraBitWriter) flush32() {
	if w.nBits < 32 {
		return
	}
	w.out = append(w.out,
		byte(w.buf),
		byte(w.buf>>8),
		byte(w.buf>>16),
		byte(w.buf>>24),
	)
	w.buf >>= 32
	w.nBits -= 32
}

// flushFinal writes all remaining buffered bits, zero-padding to a byte boundary.
func (w *extraBitWriter) flushFinal() {
	for w.nBits >= 8 {
		w.out = append(w.out, byte(w.buf))
		w.buf >>= 8
		w.nBits -= 8
	}
	if w.nBits > 0 {
		w.out = append(w.out, byte(w.buf))
	}
}

// ─── extraBitReader ───────────────────────────────────────────────────────────

// extraBitReader reads LSB-first variable-width values from a packed byte slice.
type extraBitReader struct {
	src   []byte
	off   int
	buf   uint64
	nBits uint
}

func (r *extraBitReader) init(src []byte) {
	r.src = src
	r.off = 0
	r.buf = 0
	r.nBits = 0
}

// readBits reads n bits (n ≤ 32) LSB-first.
func (r *extraBitReader) readBits(n uint) (uint32, error) {
	if n == 0 {
		return 0, nil
	}
	for r.nBits < n {
		if r.off >= len(r.src) {
			return 0, ErrCorrupted
		}
		r.buf |= uint64(r.src[r.off]) << r.nBits
		r.off++
		r.nBits += 8
	}
	mask := uint64((1 << n) - 1)
	v := uint32(r.buf & mask)
	r.buf >>= n
	r.nBits -= n
	return v, nil
}

// ─── Sequence code helpers ────────────────────────────────────────────────────

// seqToOFCode returns the offset code for the given offset value.
// Codes 0-2 are reserved for rep-match indices (not produced by the encoder currently).
// For actual offsets >= 1: code = bit_length(offset) + 2, extra bits = code - 3.
func seqToOFCode(offset uint32) byte {
	if offset == 0 {
		return 0
	}
	b := uint32(bits.Len32(offset)) // number of significant bits
	if b+2 > 255 {
		return 255
	}
	return byte(b + 2) // +2 to leave 0-2 for rep codes
}

// matchPrice returns an estimated bit cost for encoding a match of the given
// length and backward offset. Used by optimalParse to compare parse decisions.
func matchPrice(mLen int, offset uint32) int {
	ofBits := bits.Len32(offset) // extra bits for offset code
	ofCost := ofBits + 5        // extra bits + ~5 bits average FSE cost for ofCode

	excess := uint32(mLen - minMatch)
	var mlExtraBits int
	if excess < 256 {
		mlExtraBits = int(mlBits[mlCodeTable[excess]])
	} else {
		// rare long match — estimate
		mlExtraBits = bits.Len32(excess)
	}
	mlCost := mlExtraBits + 3 // extra bits + ~3 bits average FSE cost for mlCode

	return ofCost + mlCost + 4 // +4: ~4 bits average FSE cost for llCode (ll=0 most common)
}

// ─── Literals compression ─────────────────────────────────────────────────────

// compressLiterals compresses a literal byte slice using the appropriate
// entropy mode. Returns (compressed, isRaw) where isRaw=true means the
// literals were stored verbatim (incompressible).
func compressLiterals(lits []byte, mode entropyMode, sc *huff0.Scratch) (out []byte, isRaw bool) {
	if len(lits) == 0 || mode == entropyNone {
		return lits, true
	}

	var err error
	switch mode {
	case entropyStaticHuff:
		out, _, err = huff0.Compress1X(lits, sc)
		if err != nil || len(out) >= len(lits) {
			return lits, true
		}
	case entropyAdaptHuff:
		if len(lits) >= 1024 {
			out, _, err = huff0.Compress4X(lits, sc)
		} else {
			out, _, err = huff0.Compress1X(lits, sc)
		}
		if err != nil || len(out) >= len(lits) {
			return lits, true
		}
	default:
		return lits, true
	}
	return out, false
}

// compressSequenceCodes FSE-encodes a slice of symbol codes.
// Returns (compressed, isRaw).
func compressSequenceCodes(codes []byte, sc *fse.Scratch) (out []byte, isRaw bool) {
	if len(codes) == 0 {
		return nil, true
	}
	out, err := fse.Compress(codes, sc)
	if err != nil || len(out) >= len(codes) {
		return codes, true
	}
	return out, false
}
