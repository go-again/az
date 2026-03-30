package az

// Pure-Go xxh64 implementation used for frame and block checksums.
// We only need the lower 32 bits (Sum32), matching the LZ4/zstd convention.

// Use var (not const) so arithmetic on these values wraps at uint64 boundaries
// rather than being evaluated as exact (overflowing) constant expressions.
var (
	xxhPrime1 uint64 = 0x9E3779B185EBCA87
	xxhPrime2 uint64 = 0xC2B2AE3D27D4EB4F
	xxhPrime3 uint64 = 0x165667B19E3779F9
	xxhPrime4 uint64 = 0x85EBCA77C2B2AE63
	xxhPrime5 uint64 = 0x27D4EB2F165667C5
)

type xxhDigest struct {
	v1, v2, v3, v4 uint64
	total          uint64
	mem            [32]byte
	memUsed        int
}

func newXXH64() *xxhDigest {
	d := &xxhDigest{}
	d.reset()
	return d
}

func (d *xxhDigest) reset() {
	d.v1 = xxhPrime1 + xxhPrime2 // wraps at uint64: OK, xxhSeed==0
	d.v2 = xxhPrime2
	d.v3 = 0
	d.v4 = -xxhPrime1
	d.total = 0
	d.memUsed = 0
}

func xxhRound(acc, input uint64) uint64 {
	acc += input * xxhPrime2
	acc = (acc<<31 | acc>>(64-31))
	acc *= xxhPrime1
	return acc
}

func xxhMergeRound(acc, val uint64) uint64 {
	val = xxhRound(0, val)
	acc ^= val
	acc = acc*xxhPrime1 + xxhPrime4
	return acc
}

func (d *xxhDigest) Write(b []byte) (int, error) {
	n := len(b)
	d.total += uint64(n)

	if d.memUsed+n < 32 {
		copy(d.mem[d.memUsed:], b)
		d.memUsed += n
		return n, nil
	}

	p := b
	if d.memUsed > 0 {
		// fill internal buffer
		fill := 32 - d.memUsed
		copy(d.mem[d.memUsed:], p[:fill])
		p = p[fill:]
		d.v1 = xxhRound(d.v1, leU64(d.mem[0:]))
		d.v2 = xxhRound(d.v2, leU64(d.mem[8:]))
		d.v3 = xxhRound(d.v3, leU64(d.mem[16:]))
		d.v4 = xxhRound(d.v4, leU64(d.mem[24:]))
		d.memUsed = 0
	}

	for len(p) >= 32 {
		d.v1 = xxhRound(d.v1, leU64(p[0:]))
		d.v2 = xxhRound(d.v2, leU64(p[8:]))
		d.v3 = xxhRound(d.v3, leU64(p[16:]))
		d.v4 = xxhRound(d.v4, leU64(p[24:]))
		p = p[32:]
	}

	if len(p) > 0 {
		copy(d.mem[:], p)
		d.memUsed = len(p)
	}
	return n, nil
}

func (d *xxhDigest) Sum64() uint64 {
	var h uint64
	if d.total >= 32 {
		h = (d.v1<<1 | d.v1>>(64-1)) +
			(d.v2<<7 | d.v2>>(64-7)) +
			(d.v3<<12 | d.v3>>(64-12)) +
			(d.v4<<18 | d.v4>>(64-18))
		h = xxhMergeRound(h, d.v1)
		h = xxhMergeRound(h, d.v2)
		h = xxhMergeRound(h, d.v3)
		h = xxhMergeRound(h, d.v4)
	} else {
		h = xxhPrime5
	}
	h += d.total

	p := d.mem[:d.memUsed]
	for len(p) >= 8 {
		k1 := xxhRound(0, leU64(p))
		h ^= k1
		h = (h<<27|h>>(64-27))*xxhPrime1 + xxhPrime4
		p = p[8:]
	}
	if len(p) >= 4 {
		h ^= uint64(leU32(p)) * xxhPrime1
		h = (h<<23|h>>(64-23))*xxhPrime2 + xxhPrime3
		p = p[4:]
	}
	for len(p) > 0 {
		h ^= uint64(p[0]) * xxhPrime5
		h = (h<<11|h>>(64-11)) * xxhPrime1
		p = p[1:]
	}

	h ^= h >> 33
	h *= xxhPrime2
	h ^= h >> 29
	h *= xxhPrime3
	h ^= h >> 32
	return h
}

// Sum32 returns the lower 32 bits of the xxh64 digest, used in frame checksums.
func (d *xxhDigest) Sum32() uint32 {
	return uint32(d.Sum64())
}

func leU64(b []byte) uint64 {
	return uint64(b[0]) | uint64(b[1])<<8 | uint64(b[2])<<16 | uint64(b[3])<<24 |
		uint64(b[4])<<32 | uint64(b[5])<<40 | uint64(b[6])<<48 | uint64(b[7])<<56
}

func leU32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}
