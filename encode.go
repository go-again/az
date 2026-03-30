package az

import (
	"az/pkg/huff0"
)

// sequence represents one LZ77 back-reference with its preceding literal run.
type sequence struct {
	litLen   uint32
	matchLen uint32 // actual match length (>= minMatch)
	offset   uint32 // backward distance; 0 means rep-match, use repIdx
	repIdx   uint8  // index into recentOffsets (only when offset==0)
}

const (
	minMatch    = 4
	adaptSkip   = 7    // shift for adaptive skipping: skip >>= adaptSkip
	inputMargin = 10   // bytes to keep clear at end of src during match finding
)

// encoderState holds reusable allocations shared across block encodes.
type encoderState struct {
	ht1       hashTable[uint32]   // single-entry table (level 1, uint32 pos)
	dual      dualHashTable       // dual table (levels 2-5)
	recentOff [3]uint32           // recent-offset LRU (level 2+)
	litBuf    []byte
	seqs      []sequence
	hScratch  *huff0.Scratch
	// chain tables for levels 4-5
	chainTable []int32  // chainTable[pos & chainMask] = previous pos with same hash
	chainMask  int
	// pre-allocated DP arrays for optimalParse (reused across blocks)
	dpCost    []int
	dpFrom    []int32
	dpFromRef []int32
}

func newEncoderState(cfg levelConfig) *encoderState {
	st := &encoderState{
		recentOff: [3]uint32{1, 4, 8},
		// ReusePolicyNone: always emit a full table header so the decoder can
		// decompress each block independently with a fresh Scratch.
		hScratch: &huff0.Scratch{Reuse: huff0.ReusePolicyNone},
	}
	if cfg.longBits == 0 {
		st.ht1 = newHashTable[uint32](cfg.shortBits)
	} else {
		st.dual = newDualHashTable(cfg.shortBits, cfg.longBits)
	}
	if cfg.chainDepth > 0 {
		// chain covers the full window
		mask := cfg.windowSize - 1
		st.chainTable = make([]int32, cfg.windowSize)
		st.chainMask = mask
		// Pre-allocate DP arrays for optimalParse (blockSize+1 entries each).
		sz := cfg.blockSize + 1
		st.dpCost = make([]int, sz)
		st.dpFrom = make([]int32, sz)
		st.dpFromRef = make([]int32, sz)
	}
	return st
}

func (st *encoderState) resetBlock() {
	st.litBuf = st.litBuf[:0]
	st.seqs = st.seqs[:0]
}

func (st *encoderState) resetFull(cfg levelConfig) {
	st.resetBlock()
	if cfg.longBits == 0 {
		st.ht1.reset()
	} else {
		st.dual.reset()
	}
	st.recentOff = [3]uint32{1, 4, 8}
	if st.chainTable != nil {
		clear(st.chainTable)
	}
}

// updateRecent pushes a new offset to the front of the LRU.
func (st *encoderState) updateRecent(off uint32) {
	if off == st.recentOff[0] {
		return
	}
	if off == st.recentOff[1] {
		st.recentOff[1], st.recentOff[0] = st.recentOff[0], off
		return
	}
	st.recentOff[2] = st.recentOff[1]
	st.recentOff[1] = st.recentOff[0]
	st.recentOff[0] = off
}

// naivePushRecent always pushes off to the front of the LRU (no dedup).
// Must match the decoder's naive push semantics used in FSE blocks (L3+).
func (st *encoderState) naivePushRecent(off uint32) {
	st.recentOff[2] = st.recentOff[1]
	st.recentOff[1] = st.recentOff[0]
	st.recentOff[0] = off
}

// ─── Level 1: single hash, greedy, no entropy coding ────────────────────────

// encodeL1 compresses src using a single hash table, greedy matching, adaptive
// skip, and raw (no Huffman) literal output. 8MB blocks mean only ~7 goroutine
// jobs on a 51MB corpus → all process in one parallel round, maximising speed.
// The differentiator from L2: no entropy coding, single hash, far fewer jobs.
func encodeL1(src []byte, st *encoderState) []byte {
	cfg := levelConfigs[Level1]
	st.ht1.reset()

	seqs := st.seqs[:0]
	litBuf := st.litBuf[:0]
	anchor := 0
	si := 0
	sn := len(src) - inputMargin
	window := cfg.windowSize

	for si < sn {
		cv := load64(src, si)
		h := hash4(cv, cfg.shortBits)
		ref := st.ht1.get(h)
		st.ht1.set(h, si)

		if ref < si && si-ref < window && load32(src, ref) == load32(src, si) {
			// Extend backward
			for si > anchor && ref > 0 && src[si-1] == src[ref-1] {
				si--
				ref--
			}
			mLen := minMatch + extendMatch(src, si+minMatch, ref+minMatch, len(src)-5)
			seqs = append(seqs, sequence{
				litLen:   uint32(si - anchor),
				matchLen: uint32(mLen),
				offset:   uint32(si - ref),
			})
			litBuf = append(litBuf, src[anchor:si]...)
			si += mLen
			anchor = si
		} else {
			si += 1 + (si-anchor)>>adaptSkip
		}
	}

	litBuf = append(litBuf, src[anchor:]...)
	seqs = append(seqs, sequence{litLen: uint32(len(src) - anchor)})

	st.seqs = seqs
	st.litBuf = litBuf
	return buildCompactSeqBlock(seqs, litBuf)
}

// buildCompactSeqBlock assembles a block-type-0x04 payload.
// No per-sequence flag byte; 3-byte offset (sufficient for ≤16MB window).
// Layout: [0x04][litLen:u24][lits][seqCount:u24] then per-seq: [token][overflow...][offset:3]
// Trailing literals (those in the last sequence with matchLen==0) are encoded implicitly
// as lits[sum_of_lLen:] — the decoder emits them after processing seqCount sequences.
func buildCompactSeqBlock(seqs []sequence, litBuf []byte) []byte {
	seqCount := 0
	for _, s := range seqs {
		if s.matchLen > 0 {
			seqCount++
		}
	}

	dst := make([]byte, 0, 1+3+len(litBuf)+3+seqCount*4+32)
	dst = append(dst, 0x04)
	litLen := len(litBuf)
	dst = append(dst, byte(litLen), byte(litLen>>8), byte(litLen>>16))
	dst = append(dst, litBuf...)
	dst = append(dst, byte(seqCount), byte(seqCount>>8), byte(seqCount>>16))

	for _, s := range seqs {
		if s.matchLen == 0 {
			continue
		}
		dst = appendToken(dst, int(s.litLen), int(s.matchLen-minMatch))
		dst = append(dst, byte(s.offset), byte(s.offset>>8), byte(s.offset>>16))
	}

	return dst
}

// ─── Level 2: dual hash + recent-offset LRU + Huffman literals + FSE seqs ────

// encodeL2 compresses src using dual hash tables and rep-offset matching.
// Returns (litBuf, seqs) for assembly via buildFSEBlock (block type 0x02).
// Rep-match offsets are stored as actual distances so the FSE sequence format
// (which has no rep-match shorthand in L2) can encode them directly.
func encodeL2(src []byte, st *encoderState) (litBuf []byte, seqs []sequence) {
	cfg := levelConfigs[Level2]
	st.dual.reset()
	st.recentOff = [3]uint32{1, 4, 8}
	st.litBuf = st.litBuf[:0]

	seqs = st.seqs[:0]
	anchor := 0
	si := 0
	sn := len(src) - inputMargin
	window := cfg.windowSize

	for si < sn {
		cv := load64(src, si)
		hs := hash4(cv, cfg.shortBits)
		hl := hash8(cv, cfg.longBits)

		refS := st.dual.short.get(hs)
		refL := st.dual.long.get(hl)
		st.dual.short.set(hs, si)
		st.dual.long.set(hl, si)

		bestRef, bestLen := findBestMatch2(src, si, refS, refL, window, st.recentOff)
		if bestLen >= minMatch {
			var off uint32
			if bestRef < 0 { // rep match: store actual offset so FSE can encode it
				ri := uint8(-bestRef - 1)
				off = st.recentOff[ri]
			} else {
				off = uint32(si - bestRef)
			}
			st.naivePushRecent(off)
			seqs = append(seqs, sequence{
				litLen:   uint32(si - anchor),
				matchLen: uint32(bestLen),
				offset:   off,
			})
			si += bestLen
			anchor = si
		} else {
			si++
		}
	}
	seqs = append(seqs, sequence{litLen: uint32(len(src) - anchor)})
	st.seqs = seqs

	litBuf = st.litBuf
	pos := 0
	for _, s := range seqs {
		litBuf = append(litBuf, src[pos:pos+int(s.litLen)]...)
		pos += int(s.litLen) + int(s.matchLen)
	}
	st.litBuf = litBuf
	return litBuf, seqs
}

// findBestMatch2 tries rep offsets first, then short/long hash candidates.
// Returns (refPos, matchLen); if it's a rep match, refPos = -(repIdx+1).
func findBestMatch2(src []byte, si, refS, refL, window int, recent [3]uint32) (int, int) {
	bestRef := -1
	bestLen := 0

	// 1. Recent offsets (rep matches)
	for i, ro := range recent {
		ref := si - int(ro)
		if ref < 0 {
			continue
		}
		if load32(src, ref) != load32(src, si) {
			continue
		}
		mLen := minMatch + extendMatch(src, si+minMatch, ref+minMatch, len(src)-5)
		if mLen > bestLen {
			bestLen = mLen
			bestRef = -(i + 1) // negative = rep match index
		}
	}

	// 2. Short hash candidate
	if refS < si && si-refS < window {
		if load32(src, refS) == load32(src, si) {
			mLen := minMatch + extendMatch(src, si+minMatch, refS+minMatch, len(src)-5)
			if mLen > bestLen {
				bestLen = mLen
				bestRef = refS
			}
		}
	}

	// 3. Long hash candidate (verified with 8 bytes)
	if refL < si && si-refL < window && si+8 <= len(src) {
		if load64(src, refL) == load64(src, si) {
			mLen := 8 + extendMatch(src, si+8, refL+8, len(src)-5)
			if mLen > bestLen {
				bestLen = mLen
				bestRef = refL
			}
		}
	}

	return bestRef, bestLen
}

// buildRawSeqBlock assembles a block-type-0x00 or 0x01 payload.
// Sequences use LZ4-style tokens; rep matches carry a flag byte.
// Offsets are always stored as 4-byte LE regardless of window size.
func buildRawSeqBlock(src []byte, seqs []sequence, litBuf, huffOut []byte, useHuff bool) []byte {
	// Estimate output size
	dst := make([]byte, 0, len(src)/2+256)

	if useHuff {
		dst = append(dst, 0x01) // block type: huffman literals + raw seqs
		// litLen (uint24 LE)
		litLen := len(litBuf)
		dst = append(dst, byte(litLen), byte(litLen>>8), byte(litLen>>16))
		// huffman table+data size (uint24 LE)
		hs := len(huffOut)
		dst = append(dst, byte(hs), byte(hs>>8), byte(hs>>16))
		dst = append(dst, huffOut...)
	} else {
		dst = append(dst, 0x00) // block type: raw literals + raw seqs
		litLen := len(litBuf)
		dst = append(dst, byte(litLen), byte(litLen>>8), byte(litLen>>16))
		dst = append(dst, litBuf...)
	}

	// Sequences: one flag byte per seq followed by token+offset
	// Flag byte: bit7=isLast, bit6=isRep, bits1-0=repIdx (if isRep)
	litPos := 0
	srcPos := 0
	_ = srcPos
	for i, s := range seqs {
		isLast := i == len(seqs)-1
		if isLast && s.matchLen == 0 {
			// Sentinel: only literals remain, no match — emit flag only
			flag := uint8(0x80) // isLast, no match
			dst = append(dst, flag)
			break
		}

		isRep := s.offset == 0 && s.matchLen > 0
		flag := uint8(0)
		if isRep {
			flag |= 0x40
			flag |= s.repIdx & 0x03
		}
		dst = append(dst, flag)

		mlenEx := s.matchLen - minMatch
		dst = appendToken(dst, int(s.litLen), int(mlenEx))
		_ = litPos

		if !isRep {
			dst = append(dst, byte(s.offset), byte(s.offset>>8), byte(s.offset>>16), byte(s.offset>>24))
		}
		litPos += int(s.litLen)
	}

	return dst
}

// ─── Level 3: dual hash + lazy(1) + Huffman + FSE ────────────────────────────

// encodeL3 compresses src with lazy matching and full entropy coding.
// Returns (litBuf, seqs) for assembly by block.go.
func encodeL3(src []byte, st *encoderState) (litBuf []byte, seqs []sequence) {
	cfg := levelConfigs[Level3]
	return encodeLazy(src, st, cfg, cfg.lazyDepth)
}

func encodeL4(src []byte, st *encoderState) (litBuf []byte, seqs []sequence) {
	cfg := levelConfigs[Level4]
	return encodeLazy(src, st, cfg, cfg.lazyDepth)
}

// encodeLazy implements lazy match evaluation at the given depth (1 or 2).
// At each position it checks whether waiting one (or two) more literals yields
// a longer match.  The extra overhead versus greedy pays off in ~3-6% better ratio.
func encodeLazy(src []byte, st *encoderState, cfg levelConfig, lazyDepth int) ([]byte, []sequence) {
	st.dual.reset()
	st.recentOff = [3]uint32{1, 4, 8}
	if st.chainTable != nil {
		clear(st.chainTable)
	}
	seqs := st.seqs[:0]
	litBuf := st.litBuf[:0]
	window := cfg.windowSize
	n := len(src)
	sn := n - inputMargin

	// look up and update both hash tables, optionally updating the chain.
	update := func(pos int) (refS, refL int) {
		cv := load64(src, pos)
		hs := hash4(cv, cfg.shortBits)
		hl := hash8(cv, cfg.longBits)
		refS = st.dual.short.get(hs)
		refL = st.dual.long.get(hl)
		st.dual.short.set(hs, pos)
		st.dual.long.set(hl, pos)
		if st.chainTable != nil {
			st.chainTable[pos&st.chainMask] = int32(refS)
		}
		return
	}

	anchor := 0
	si := 0
	for si < sn {
		refS, refL := update(si)
		bestRef, bestLen := findBestMatch3(src, si, refS, refL, window, st.recentOff, st.chainTable, st.chainMask, cfg.chainDepth)

		if bestLen < minMatch {
			si++
			continue
		}

		// Lazy evaluation: try advancing si by 1 up to lazyDepth times.
		// Each iteration checks one position ahead of the current si.
		for depth := 0; depth < lazyDepth && si+1 < sn; depth++ {
			nextSi := si + 1
			lazyRefS := st.dual.short.get(hash4(load64(src, nextSi), cfg.shortBits))
			lazyRefL := st.dual.long.get(hash8(load64(src, nextSi), cfg.longBits))
			lazyRef, lazyLen := findBestMatch3(src, nextSi, lazyRefS, lazyRefL, window, st.recentOff, st.chainTable, st.chainMask, cfg.chainDepth)
			if lazyLen <= bestLen {
				break
			}
			// Better match found one position later — advance si and keep looking.
			si++
			bestRef = lazyRef
			bestLen = lazyLen
		}

		// Commit the match
		var off uint32
		if bestRef < 0 {
			// Rep match: store actual offset, naive-push to match decoder
			repIdx := uint8(-bestRef - 1)
			off = st.recentOff[repIdx]
		} else {
			off = uint32(si - bestRef)
		}
		st.naivePushRecent(off)

		seqs = append(seqs, sequence{
			litLen:   uint32(si - anchor),
			matchLen: uint32(bestLen),
			offset:   off,
		})
		litBuf = append(litBuf, src[anchor:si]...)

		// Update tables for positions covered by the match
		for k := 1; k < bestLen && si+k < sn; k++ {
			update(si + k)
		}

		si += bestLen
		anchor = si
	}

	// Tail literals
	litBuf = append(litBuf, src[anchor:]...)
	seqs = append(seqs, sequence{litLen: uint32(n - anchor)})

	st.litBuf = litBuf
	st.seqs = seqs
	return litBuf, seqs
}

func findBestMatch3(src []byte, si, refS, refL, window int, recent [3]uint32, chain []int32, chainMask, chainDepth int) (int, int) {
	bestRef := -1
	bestLen := 0

	// 1. Recent offsets
	for i, ro := range recent {
		ref := si - int(ro)
		if ref < 0 || ref >= si {
			continue
		}
		if load32(src, ref) != load32(src, si) {
			continue
		}
		mLen := minMatch + extendMatch(src, si+minMatch, ref+minMatch, len(src)-5)
		if mLen > bestLen {
			bestLen = mLen
			bestRef = -(i + 1)
		}
	}

	// 2. Short hash candidate + chain
	candidate := refS
	depth := chainDepth
	if depth == 0 {
		depth = 1
	}
	for k := 0; k < depth && candidate < si && si-candidate < window; k++ {
		if load32(src, candidate) == load32(src, si) {
			mLen := minMatch + extendMatch(src, si+minMatch, candidate+minMatch, len(src)-5)
			if mLen > bestLen {
				bestLen = mLen
				bestRef = candidate
			}
		}
		if chain == nil {
			break
		}
		next := int(chain[candidate&chainMask])
		if next >= candidate {
			break
		}
		candidate = next
	}

	// 3. Long hash candidate (8-byte verify)
	if refL < si && si-refL < window && si+8 <= len(src) {
		if load64(src, refL) == load64(src, si) {
			mLen := 8 + extendMatch(src, si+8, refL+8, len(src)-5)
			if mLen > bestLen {
				bestLen = mLen
				bestRef = refL
			}
		}
	}

	return bestRef, bestLen
}

// ─── Level 5: optimal parse ──────────────────────────────────────────────────

// encodeL5 uses a price-based optimal parse for maximum compression.
func encodeL5(src []byte, st *encoderState) (litBuf []byte, seqs []sequence) {
	if len(src) <= inputMargin {
		return encodeLazy(src, st, levelConfigs[Level5], 2)
	}
	return optimalParse(src, st, levelConfigs[Level5])
}

// optimalParse implements a dynamic-programming optimal parse over src.
func optimalParse(src []byte, st *encoderState, cfg levelConfig) ([]byte, []sequence) {
	n := len(src)
	const inf = 1 << 30

	// Use pre-allocated DP arrays from encoderState when available.
	// cost[i] = minimum bit-cost to represent src[0:i]
	// from[i]: negative = literal (-1 means one literal); positive = match length
	// fromRef[i]: reference position for the match ending at i (valid when from[i] > 0)
	var cost []int
	var from, fromRef []int32
	if len(st.dpCost) >= n+1 {
		cost = st.dpCost[:n+1]
		from = st.dpFrom[:n+1]
		fromRef = st.dpFromRef[:n+1]
	} else {
		cost = make([]int, n+1)
		from = make([]int32, n+1)
		fromRef = make([]int32, n+1)
	}
	for i := range cost {
		cost[i] = inf
	}
	clear(from)
	clear(fromRef)
	cost[0] = 0

	st.recentOff = [3]uint32{1, 4, 8}
	st.dual.reset()
	if st.chainTable != nil {
		clear(st.chainTable)
	}
	window := cfg.windowSize

	tryMatch := func(i, ref int) {
		if ref < 0 || ref >= i || i-ref >= window {
			return
		}
		if load32(src, ref) != load32(src, i) {
			return
		}
		mLen := minMatch + extendMatch(src, i+minMatch, ref+minMatch, n-5)
		mc := cost[i] + matchPrice(mLen, uint32(i-ref))
		if mc < cost[i+mLen] {
			cost[i+mLen] = mc
			from[i+mLen] = int32(mLen)
			fromRef[i+mLen] = int32(ref)
		}
	}

	for i := 0; i < n; i++ {
		if cost[i] == inf {
			continue
		}
		base := cost[i]

		// Option 1: literal at i (8 bits). Always available.
		if i+1 <= n && base+8 < cost[i+1] {
			cost[i+1] = base + 8
			from[i+1] = -1 // one literal
		}

		// Option 2: matches — only safe when enough bytes remain for load64.
		if i >= n-inputMargin {
			continue
		}
		cv := load64(src, i)
		hs := hash4(cv, cfg.shortBits)
		hl := hash8(cv, cfg.longBits)
		refS := st.dual.short.get(hs)
		refL := st.dual.long.get(hl)
		st.dual.short.set(hs, i)
		st.dual.long.set(hl, i)
		if st.chainTable != nil {
			st.chainTable[i&st.chainMask] = int32(refS)
		}

		// Walk short hash chain up to chainDepth candidates.
		candidate := refS
		depth := cfg.chainDepth
		if depth == 0 {
			depth = 1
		}
		for k := 0; k < depth && candidate >= 0 && candidate < i && i-candidate < window; k++ {
			tryMatch(i, candidate)
			if st.chainTable == nil {
				break
			}
			next := int(st.chainTable[candidate&st.chainMask])
			if next >= candidate {
				break
			}
			candidate = next
		}

		// Try long hash candidate (single candidate, 8-byte fingerprint).
		if refL >= 0 && refL < i && i-refL < window && i+8 <= n {
			if load64(src, refL) == load64(src, i) {
				mLen := 8 + extendMatch(src, i+8, refL+8, n-5)
				mc := cost[i] + matchPrice(mLen, uint32(i-refL))
				if mc < cost[i+mLen] {
					cost[i+mLen] = mc
					from[i+mLen] = int32(mLen)
					fromRef[i+mLen] = int32(refL)
				}
			}
		}
	}

	// Traceback from n, building sequences directly using the stored references.
	litBuf := st.litBuf[:0]
	seqs := st.seqs[:0]

	// Collect the parse decisions in forward order.
	type seg struct{ si, mLen, ref int32 } // si = match start, mLen > 0, ref = reference pos
	var matches []seg
	pos := n
	for pos > 0 {
		f := from[pos]
		if f == 0 {
			// Unreachable: treat all remaining bytes as literals (shouldn't happen in practice).
			pos = 0
			break
		}
		if f < 0 {
			pos-- // literal
		} else {
			si := int32(pos) - f
			matches = append(matches, seg{si, f, fromRef[pos]})
			pos -= int(f)
		}
	}
	// Reverse to get forward order.
	for i, j := 0, len(matches)-1; i < j; i, j = i+1, j-1 {
		matches[i], matches[j] = matches[j], matches[i]
	}

	anchor := 0
	for _, m := range matches {
		si := int(m.si)
		length := int(m.mLen)
		ref := int(m.ref)
		off := uint32(si - ref)
		seqs = append(seqs, sequence{
			litLen:   uint32(si - anchor),
			matchLen: uint32(length),
			offset:   off,
		})
		litBuf = append(litBuf, src[anchor:si]...)
		st.naivePushRecent(off)
		anchor = si + length
	}
	litBuf = append(litBuf, src[anchor:]...)
	seqs = append(seqs, sequence{litLen: uint32(len(src) - anchor)})

	st.litBuf = litBuf
	st.seqs = seqs
	return litBuf, seqs
}

// ─── Token helpers ────────────────────────────────────────────────────────────

// appendToken appends an LZ4-style token byte plus overflow bytes for lLen and mLenEx.
// mLenEx = matchLen - minMatch (i.e. the excess beyond minMatch).
func appendToken(dst []byte, lLen, mLenEx int) []byte {
	litTag := lLen
	if litTag > 15 {
		litTag = 15
	}
	matchTag := mLenEx
	if matchTag > 15 {
		matchTag = 15
	}
	dst = append(dst, byte(litTag<<4)|byte(matchTag))

	// Literal length overflow
	if lLen >= 15 {
		lLen -= 15
		for lLen >= 255 {
			dst = append(dst, 255)
			lLen -= 255
		}
		dst = append(dst, byte(lLen))
	}

	// Match length overflow
	if mLenEx >= 15 {
		mLenEx -= 15
		for mLenEx >= 255 {
			dst = append(dst, 255)
			mLenEx -= 255
		}
		dst = append(dst, byte(mLenEx))
	}

	return dst
}

// max returns the larger of a and b.
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
