# az — Architecture & Technical Reference

## Overview

`az` is a lossless general-purpose compression algorithm combining:

- **LZ77 back-reference matching** for redundancy elimination
- **Huffman coding** (huff0) for literal byte sequences
- **FSE (Finite State Entropy)** coding for sequence metadata
- **Five compression levels** with independent encoder designs
- **Independent blocks** enabling streaming, seeking, and parallel decompression

The design borrows match-finding ideas from LZ4 (fast path, greedy, small window)
and zstd (dual hash, lazy matching, rep-match LRU, optimal parse) while using
a simpler block format that avoids cross-block entropy state.

---

## Binary Format

### Frame Header

```
Offset  Size  Field
  0      4    Magic = 0x415A0001  ('A','Z',0x00,0x01)
  4      1    FLG byte
               bit 6 = CksumPresent   (per-block + content checksum)
               bit 5 = SizePresent    (uncompressed ContentSize follows)
               bits 3–0 = level (informational)
  5      1    BLK byte
               bits 7–4 = block-size-id
                 4 → 64 KB    5 → 256 KB   6 → 1 MB
                 7 → 4 MB     8 → 8 MB
  6     [8]   ContentSize (uint64 LE, if SizePresent)
  ?      4    HeaderChecksum (lower 32 bits of XXH64 over FLG+BLK+[ContentSize])
```

### Data Blocks

```
  0      4    BlockHeader (uint32 LE)
               bit 31 = IsLast
               bit 30 = IsRaw (store uncompressed)
               bits 29–0 = CompressedSize in bytes
  4      N    BlockData
  4+N   [4]   BlockChecksum (lower 32 bits of XXH64 of uncompressed block data,
               if CksumPresent)
```

### End of Stream

```
  0      4    0x80000000  (IsLast | size=0)
  4     [4]   ContentChecksum (lower 32 bits of XXH64 of all uncompressed data,
               if CksumPresent)
```

### Block Body Types (first byte of BlockData when not IsRaw)

| Type | Byte | Description |
|------|------|-------------|
| Raw seqs | `0x00` | Raw literals + LZ4-style token sequences |
| Huff seqs | `0x01` | Huffman-compressed literals + LZ4-style tokens |
| FSE block | `0x02` | Huffman literals + FSE offset codes |
| RLE | `0x03` | Single repeated byte |

---

## Compression Levels

### Level 1 — Fastest

**Goal:** Match LZ4 throughput at slightly better ratio.

**Algorithm:** Single 4-byte hash table, greedy match selection.

```
for each position i:
  ref = shortTable[hash4(src[i:i+4])]
  if ref is valid and src[ref:ref+minMatch] == src[i:i+minMatch]:
    extend match; emit (literal-run, match-offset, match-length)
  else:
    emit literal
```

**Encoding:** Block type `0x00`. Literals stored raw. Sequences use
LZ4-style token encoding with 4-byte offsets:

```
[flag(1)] [token(1)] [litLen-overflow...] [matchLen-overflow...] [offset(4)]
```

Flag byte: `bit7=isLast, bit6=isRep, bits1-0=repIdx`.
Token byte: `high-nibble=litTag (0–14 + overflow), low-nibble=matchTag`.

**No entropy coding.** Near-zero overhead per byte.

---

### Level 2 — Fast

**Goal:** ~10–15% better ratio than Level 1 at ~60% of the speed.

**Algorithm:** Dual 4-byte + 8-byte hash tables, greedy match selection,
3-slot recent-offset LRU for free rep-match detection.

```
for each position i:
  refS = shortTable[hash4(src[i:i+4])]
  refL = longTable[hash8(src[i:i+8])]
  bestRef, bestLen = best of: rep-matches(recent[0..2]), refS, refL
  emit match or literal
```

**Encoding:** Block type `0x01`. Literals Huffman-compressed with huff0 1X.
Sequences same LZ4-style token format as Level 1. When Huffman does not
compress (incompressible literals), fallback to `0x00` raw literals.

**Rep-match LRU** (3 slots, initial `[1, 4, 8]`):
A rep match references one of the three most recently used offsets, costing
zero offset bytes. The LRU is updated by rotating the referenced slot to
position 0 after each use.

---

### Level 3 — Default

**Goal:** Competitive ratio with high throughput; better than gzip, near zstd -3.

**Algorithm:** Dual hash + **lazy matching** (depth 1): after finding a match
at position `i`, look one position ahead (`i+1`) for a longer match before
committing.

```
for each position i:
  find bestMatch at i
  peek one position ahead; if better match found, advance i by 1
  commit match
```

**Encoding:** Block type `0x02` (FSE block).

- Literals: Huffman-compressed with huff0 (1X for < 1 KB, 4X for ≥ 1 KB).
- Literal lengths + match lengths: raw uint24 LE per sequence (3 bytes each).
- Offset codes: FSE-compressed (or raw if FSE yields no benefit). Codes ≥ 3
  are followed by `extraBits = code − 3` bits of offset residual.
- Rep matches are stored as the actual offset value (decoded from LRU context).

**Offset code scheme:**

```
offset → code = bit_length(offset) + 2
decoder: base = 1 << (code − 3);  extra = next (code−3) bits;  offset = base + extra
```

Codes 0–2 are reserved for potential future rep-match shortcuts.

---

### Level 4 — Better

Same as Level 3 but with:
- Lazy match depth **2** (look ahead two positions before committing)
- Chain table of depth **4** for resolving hash collisions
- 4 MB window / block size
- Huffman 4X for blocks ≥ 1 KB (parallel symbol streams)

The chain table stores previous positions with the same short hash,
enabling deeper match searches at the cost of O(chainDepth) extra
comparisons per position.

---

### Level 5 — Best

**Algorithm:** Dynamic-programming **optimal parse** over blocks up to 256 KB.
Falls back to lazy-2 (Level 4 strategy) for larger blocks.

```
cost[0] = 0;  cost[i>0] = ∞
for i in 0 .. n-inputMargin:
  cost[i+1] = min(cost[i+1], cost[i] + 8)           // literal: 8 bits
  for each candidate match (ref, mLen) at i:
    end = i + mLen
    cost[end] = min(cost[end], cost[i] + matchCost)  // match: 40 bits (approx)

traceback from n → build sequence list
```

The price model uses a flat estimate (`matchCost = 40 bits`) which is a
good approximation for typical data. A more accurate model would account
for the actual entropy of each offset, but the added complexity rarely
improves ratio by more than 1–2%.

Chain depth 16 is used to find longer matches during the DP.

---

## Data Structures

### encoderState

Per-Writer reusable allocation. Reset between blocks to avoid GC pressure.

```go
type encoderState struct {
    dual       dualHashTable  // short (4-byte hash) + long (8-byte hash)
    chainTable []int32        // collision chain for levels 4–5
    chainMask  int
    recentOff  [3]uint32      // LRU of recent match offsets
    litBuf     []byte         // accumulated literals for current block
    seqs       []sequence     // accumulated sequence list
    hScratch   *huff0.Scratch // reused Huffman scratch (ReusePolicyNone)
    fScratch   *fse.Scratch   // reused FSE scratch
}

type sequence struct {
    litLen   uint32  // number of literals preceding this match
    matchLen uint32  // match length (≥ minMatch = 4)
    offset   uint32  // back-reference distance (bytes)
}
```

### Hash Tables

Generic hash table parameterised on value type:

```go
type hashTable[V uint16 | uint32] struct {
    entries []V
}
```

Level 1 uses `uint32` for short only. Levels 2–5 use a `dualHashTable`
with both short (`uint32`, indexing up to 4 MB) and long (`uint32`).

**Hash functions:**

```go
// 4-byte position hash (Knuth multiplicative hash)
func hash4(x uint64, bits uint8) uint32 {
    return (uint32(x) * 2654435761) >> (32 - bits)
}

// 8-byte position hash (Fibonacci hashing on 64 bits)
func hash8(x uint64, bits uint8) uint32 {
    return uint32((x * 0xcf1bbcdcb7a56463) >> (64 - bits))
}
```

### LRU Semantics

The 3-slot recent-offset LRU tracks the three most recently used match
distances. Rotation rules differ between L2 (raw seqs) and L3+ (FSE seqs):

**L2 (raw token format):** Sequences carry `repIdx` (0–2) for rep matches.
The encoder rotates the LRU by bringing `recentOff[repIdx]` to slot 0.
The decoder mirrors this with an identical rotation.

**L3–L5 (FSE format):** Sequences carry the actual offset value for all
matches (both new and rep). The encoder uses a **naive push** for every
sequence (shift [1]→[2], [0]→[1], new→[0]), matching the decoder's
unconditional push-to-front.

---

## Entropy Coding

### Huffman Literals (huff0)

Literals for Levels 2–5 are Huffman-compressed using the `huff0` library
(vendored from klauspost/compress). Key details:

- **ReusePolicyNone**: each block carries its own full Huffman table.
  This is required for independent block decompression; reuse policies
  emit a short "reuse" marker that a fresh decoder cannot interpret.
- **1X vs 4X**: `Compress1X` / `Decompress1X` for blocks < 1 KB;
  `Compress4X` / `Decompress4X` for ≥ 1 KB. The 4X variant splits the
  literal stream into four parallel sub-streams for better entropy coding.
- **Fallback**: if Huffman output ≥ input size, literals are stored raw
  (`huffSize = 0` sentinel in the block header).

### FSE Sequence Coding (Levels 3–5)

Offset codes (one byte per sequence) are FSE-compressed using the `fse`
library (vendored from klauspost/compress).

- If FSE output ≥ raw codes, codes are stored raw (high bit of the 2-byte
  size prefix signals raw vs. FSE).
- Literal lengths and match lengths are NOT FSE-coded: they are stored as
  raw uint24 LE (3 bytes per sequence). This simplifies the format and
  avoids FSE for values that are often large and uniformly distributed.

### Offset Residual Bits

For offset code `c ≥ 3`:

```
encoder:
  extraBits = c − 3
  base      = 1 << extraBits
  extra     = offset & ((1 << extraBits) − 1)
  emit: [extra as uint32 LE]   (only when extraBits > 0)

decoder:
  extraBits = c − 3
  base      = 1 << extraBits
  if extraBits > 0:
    extra = read_uint32_le() & ((1 << extraBits) − 1)
  offset = base + extra
```

This encodes offsets 1–∞ without a lookup table, using `c − 3` bits of
precision beyond the implicit base.

---

## Checksums

**Algorithm:** Pure-Go XXH64 (64-bit, seed 0). Lower 32 bits used everywhere.

**Per-block checksum:** Computed over the **uncompressed** block content.
Written after the compressed block data. Verified after decompression, before
returning data to the caller.

**Content checksum:** Running XXH64 over all uncompressed data in order.
Written in the end-of-stream marker. Verified when the reader reaches EOF.

**Header checksum:** Computed over `FLG || BLK || [ContentSize]`.
Protects frame metadata from corruption.

---

## Streaming Design

`Writer` and `Reader` implement `io.WriteCloser` / `io.ReadCloser`.

**Writer:**
- Wraps underlying `io.Writer` with a `bufio.Writer` (64 KB buffer).
- Accumulates input into `buf` (up to `blockSize`).
- Flushes a block when `buf` reaches `blockSize` or on `Close()`.
- `Close()` on an empty writer emits a minimal empty frame (valid `.az` file).
- `Reset(dst)` reuses all allocations for a new stream.

**Reader:**
- Wraps underlying `io.Reader` with a `bufio.Reader` (64 KB buffer).
- Decodes one block per `Read` call when its internal buffer is drained.
- Decompressed data is buffered in `r.buf`; subsequent `Read` calls drain it.
- `Reset(src)` reuses allocations for a new stream.

**Pool:**
```go
var encoderPools [6]sync.Pool
```
One pool per level. `getEncoderState` / `putEncoderState` are available for
high-throughput applications that need many concurrent streams.

---

## Incompressible Data

`compressBlock` returns `isRaw = true` when the compressed output is not
smaller than the input. The block header `IsRaw` flag is set; the block body
is the literal input bytes. The decoder returns a copy of the block body
directly. This ensures the format never expands data by more than the block
and frame header overhead (~20 bytes per block + ~12 bytes frame).

RLE detection runs before compression: if all bytes in a block are identical,
a 5-byte RLE block is emitted regardless of level.

---

## vendored Packages

| Path | Origin | Purpose |
|------|--------|---------|
| `pkg/fse` | klauspost/compress/fse | Finite State Entropy codec |
| `pkg/huff0` | klauspost/compress/huff0 | Huffman codec |
| `pkg/le` | klauspost/compress/internal/le | Little-endian helpers |
| `pkg/cpuinfo` | klauspost/compress/internal/cpuinfo | SIMD feature detection |

Import paths rewritten from `github.com/klauspost/compress/...` to `az/pkg/...`.
Non-test files only. No changes to logic.
