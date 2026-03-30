package az

import (
	"az/pkg/fse"
	"az/pkg/huff0"
)

// compressLiterals compresses a literal byte slice using the appropriate
// entropy mode.  Returns (compressed, isRaw) where isRaw=true means the
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

// compressSequenceCodes FSE-encodes a slice of symbol codes (litLen, matchLen,
// or offset codes).  Returns (compressed, isRaw).
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

// seqToLLCode maps a literal-length value to a compact code byte.
// Values 0-255 are stored directly.
func seqToLLCode(lLen uint32) byte {
	if lLen > 255 {
		return 255
	}
	return byte(lLen)
}

// seqToMLCode maps a match-length excess (matchLen - minMatch) to a code byte.
func seqToMLCode(mLenEx uint32) byte {
	if mLenEx > 255 {
		return 255
	}
	return byte(mLenEx)
}

// seqToOFCode returns the offset code for the given offset value.
// Codes 0-2 are reserved for rep-match indices.
// For actual offsets >= 1: code = bit_length(offset), base stored separately.
func seqToOFCode(offset uint32) byte {
	if offset == 0 {
		return 0
	}
	// Count significant bits; code = position of highest set bit + 1
	bits := uint32(0)
	v := offset
	for v > 0 {
		v >>= 1
		bits++
	}
	if bits+2 > 255 {
		return 255
	}
	return byte(bits + 2) // +2 to leave 0-2 for rep codes
}
