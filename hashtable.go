package az

import "math/bits"

// hashTable is a simple direct-addressed hash table that maps a hash to a
// position within the current block.  The type parameter V is uint16 for
// level-1 (64 KB window) and uint32 for all other levels.
type hashTable[V uint16 | uint32] struct {
	data []V
	bits uint8
}

func newHashTable[V uint16 | uint32](hashBits uint8) hashTable[V] {
	return hashTable[V]{
		data: make([]V, 1<<hashBits),
		bits: hashBits,
	}
}

// get returns the stored position for the given hash key.
func (h *hashTable[V]) get(key uint32) int {
	return int(h.data[key])
}

// set stores a position for the given hash key.
func (h *hashTable[V]) set(key uint32, pos int) {
	h.data[key] = V(pos)
}

// reset clears the table without reallocating.
func (h *hashTable[V]) reset() {
	clear(h.data)
}

// dualHashTable holds two hash tables: a short-hash (4-byte) table and a
// long-hash (8-byte) table.  Used for levels 2-5.
type dualHashTable struct {
	short hashTable[uint32]
	long  hashTable[uint32]
}

func newDualHashTable(shortBits, longBits uint8) dualHashTable {
	return dualHashTable{
		short: newHashTable[uint32](shortBits),
		long:  newHashTable[uint32](longBits),
	}
}

func (d *dualHashTable) reset() {
	d.short.reset()
	d.long.reset()
}

// hash4 computes a hashBits-wide hash of the 4 least-significant bytes of x.
func hash4(x uint64, hashBits uint8) uint32 {
	const prime32 = 2654435761
	return (uint32(x) * prime32) >> (32 - hashBits)
}

// hash8 computes a hashBits-wide hash of all 8 bytes of x.
func hash8(x uint64, hashBits uint8) uint32 {
	const prime64 = 0xcf1bbcdcb7a56463
	return uint32((x * prime64) >> (64 - hashBits))
}

// load64 reads 8 bytes at position pos in src as a little-endian uint64.
// The caller must ensure pos+8 <= len(src).
func load64(src []byte, pos int) uint64 {
	_ = src[pos+7] // bounds check hint
	return uint64(src[pos]) | uint64(src[pos+1])<<8 | uint64(src[pos+2])<<16 |
		uint64(src[pos+3])<<24 | uint64(src[pos+4])<<32 | uint64(src[pos+5])<<40 |
		uint64(src[pos+6])<<48 | uint64(src[pos+7])<<56
}

// load32 reads 4 bytes at position pos in src as a little-endian uint32.
func load32(src []byte, pos int) uint32 {
	_ = src[pos+3]
	return uint32(src[pos]) | uint32(src[pos+1])<<8 | uint32(src[pos+2])<<16 | uint32(src[pos+3])<<24
}

// extendMatch returns the number of matching bytes starting at a and b,
// stopping before limit.
func extendMatch(src []byte, a, b, limit int) int {
	orig := a
	for a+8 <= limit {
		diff := load64(src, a) ^ load64(src, b+(a-orig))
		if diff != 0 {
			return a + bits.TrailingZeros64(diff)/8 - orig
		}
		a += 8
	}
	for a < limit && src[a] == src[b+(a-orig)] {
		a++
	}
	return a - orig
}
