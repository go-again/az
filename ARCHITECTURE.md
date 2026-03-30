# az — Architecture & Technical Reference

## Overview

`az` is a lossless general-purpose compression algorithm combining:

- **LZ77 back-reference matching** for redundancy elimination
- **Huffman coding** (huff0) for literal byte sequences
- **FSE (Finite State Entropy)** coding for sequence metadata
- **Five compression levels** with independent encoder designs
- **Parallel block compression** using goroutines for all levels
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
| Raw seqs | `0x00` | Raw literals + LZ4-style token sequences (legacy/fallback) |
| Huff seqs | `0x01` | Huffman-compressed literals + LZ4-style tokens (legacy/fallback) |
| FSE block | `0x02` | Huffman literals + FSE-coded compact sequence codes (L2–L5) |
| RLE | `0x03` | Single repeated byte |
| Compact seqs | `0x04` | Raw literals + compact token sequences, 3-byte offset (L1) |

---

## Block Type 0x04 — Compact Seqs (Level 1)

```
[0x04]                               block type
[litLen:  u24 LE]                    uncompressed literal count
[lits: litLen bytes]                 raw literal bytes
[seqCount: u24 LE]                   number of match sequences
-- per sequence (seqCount times): --
[token: 1 byte]                      high-nibble=litTag, low-nibble=matchTag
[litLen overflow bytes...]           present only when litTag == 15
[matchLen overflow bytes...]         present only when matchTag == 15
[offset: u24 LE]                     back-reference distance (1–16 777 215)
-- after all sequences: --
remaining lits[sum_litLen:] appended implicitly by the decoder
```

Compared to block type 0x00/0x01:
- No per-sequence flag byte (saves 1 byte/seq; no rep-match encoding at L1)
- 3-byte offset instead of 4-byte (saves 1 byte/seq; 8 MB window fits in 24 bits)
- `seqCount` header replaces the `isLast` flag sentinel

---

## Block Type 0x02 — FSE Block (Levels 2–5)

```
[0x02]                               block type
[litLen:  u24 LE]                    uncompressed literal count
[huffSize: u24 LE]                   0 = raw literals follow; >0 = Huffman-compressed size
[lits: huffSize bytes or litLen bytes]
[seqCount: u24 LE]
-- if seqCount > 0: --
[llStream: 3-byte-prefixed]          litLen codes, 1 byte/seq (FSE or raw)
[mlStream: 3-byte-prefixed]          matchLen-excess codes, 1 byte/seq (FSE or raw)
[ofStream: 3-byte-prefixed]          offset codes, 1 byte/seq (FSE or raw)
[extraBitsLen: u24 LE]
[extraBitsBuf: extraBitsLen bytes]   packed LE bits: ll_extra ‖ ml_extra ‖ of_extra per seq
```

Each 3-byte-prefixed stream: `[size: u24 LE | isRaw<<23]` followed by `size & 0x7FFFFF` bytes.
`isRaw` (bit 23 of the prefix) = 1 when stored uncompressed (FSE yielded no benefit).

### Compact Sequence Codes

Literal-length and match-length values are mapped to compact codes using
zstd-compatible bucket tables, reducing sequence metadata from 6 bytes/seq
(raw uint24 × 2) to ~1–2 bytes/seq after FSE.

**litLen codes (36 codes):**

```
code  0–15: base = code,          extraBits = 0
code 16–19: base = 16,18,20,22,   extraBits = 1
code 20–21: base = 24,28,          extraBits = 2
code 22–23: base = 32,40,          extraBits = 3
code 24–25: base = 48,64,          extraBits = 4,6  (llBits[24]=4, llBits[25]=6)
code 26–35: base = 128..65536,     extraBits = 7..16
```

**matchLen-excess codes (53 codes):** match `excess = matchLen − minMatch`:

```
code  0–31: base = code,          extraBits = 0
code 32–33: base = 32,34,          extraBits = 1
code 34–35: base = 36,40,          extraBits = 2,3
code 36–37: base = 48,64,          extraBits = 4,6
code 38–52: base = 128..2097152,   extraBits = 7..21
```

**Offset codes:**

```
code =  bit_length(offset) + 2       (codes 0–2 reserved for future rep-match use)
extraBits = code − 3
base      = 1 << extraBits
offset    = base + extraBits_value
```

### Extra Bits Packing

All extra bits (ll, ml, of) for each sequence are packed LSB-first into a
forward byte stream using `extraBitWriter`. The decoder uses `extraBitReader`.
After each group that may produce > 32 bits, `flush32()` prevents uint64 overflow.

---

## Compression Levels

| Level | Block | Window | shortBits | longBits | lazyDepth | chainDepth | Parse   | litMode    | seqMode | Block type |
|-------|-------|--------|-----------|----------|-----------|------------|---------|------------|---------|------------|
| 1     | 8 MB  | 8 MB   | 20        | —        | 0         | 0          | greedy  | none       | none    | 0x04       |
| 2     | 8 MB  | 8 MB   | 20        | 18       | 0         | 0          | greedy  | adapt huff | FSE     | 0x02       |
| 3     | 8 MB  | 8 MB   | 19        | 21       | 2         | 8          | lazy    | adapt huff | FSE     | 0x02       |
| 4     | 8 MB  | 8 MB   | 19        | 22       | 4         | 32         | lazy    | adapt huff | FSE     | 0x02       |
| 5     | 8 MB  | 8 MB   | 20        | 22       | —         | 4          | optimal | adapt huff | FSE     | 0x02       |

`lazyDepth`: number of positions to look ahead before committing a match (0 = greedy).
`chainDepth`: hash-chain walk depth per candidate position.

All levels use 8 MB blocks, giving uniform parallelism (~7 blocks per 51 MB input →
one parallel round on an 8-worker pool). Quality differences come from algorithm alone.

### Level 1 — Fastest

**Algorithm:** Single 4-byte hash table, greedy match selection, adaptive skip.
Block type `0x04`: raw literals + compact token sequences with 3-byte offsets.

```
[0x04] [litLen:u24] [lits: litLen bytes] [seqCount:u24]
per sequence: [token(1)] [litLen-overflow...] [matchLen-overflow...] [offset(3)]
```

Token byte: `high-nibble=litTag (0–14 + overflow), low-nibble=matchTag (0–14 + overflow)`.
No rep-match encoding; no per-sequence flag byte. After `seqCount` sequences, the decoder
appends the remaining literals from the literal buffer. 3-byte offsets support up to 16 MB
distance (sufficient for the 8 MB window).

Using 8 MB blocks keeps L1 to ~7 blocks on a 51 MB corpus, so all blocks compress in a
single parallel round — the same as all other levels.

### Level 2 — Fast

**Algorithm:** Dual hash (4-byte + 8-byte), greedy, 3-slot recent-offset LRU.
Block type `0x02` (same FSE format as L3–L5): Huffman-compressed literals (1X or 4X),
FSE-coded compact sequence codes (ll/ml/of). Rep-match offsets are stored as actual
distances so the FSE sequence format can encode them without a special rep-match flag.

### Level 3 — Default

**Algorithm:** Dual hash + chain(8) + **lazy matching** (depth 2). Block type `0x02`.

After finding a match at position `i`, looks up to 2 positions ahead before
committing. The extra 2 candidate lookups per committed match are cheap relative
to the ratio improvement over greedy.

### Level 4 — Better

Same as Level 3 with lazy depth 4, longer chain (chainDepth=32 vs 8), and a larger
long-hash table (longBits=22 vs 21) for better 8-byte match coverage.

### Level 5 — Best

**Algorithm:** Dynamic-programming **optimal parse** over each 8 MB block.

```
cost[0] = 0;  cost[i>0] = ∞
for i in 0 .. n−inputMargin:
  cost[i+1] = min(cost[i+1], cost[i] + 8)           // literal: ~8 bits
  for each candidate match (ref, mLen) via chain+longHash:
    mc = cost[i] + matchPrice(mLen, i−ref)
    cost[i+mLen] = min(cost[i+mLen], mc)

traceback from n → build sequence list
```

**`matchPrice` cost model:**

```go
func matchPrice(mLen int, offset uint32) int {
    ofBits := bits.Len32(offset)          // extra bits for offset code
    ofCost := ofBits + 5                  // + ~5 bits FSE overhead for ofCode
    mlExtraBits := int(mlBits[mlCodeTable[min(excess,255)]])
    mlCost := mlExtraBits + 3             // + ~3 bits FSE overhead for mlCode
    return ofCost + mlCost + 4            // + ~4 bits FSE overhead for llCode
}
```

DP arrays are pre-allocated in `encoderState` (one per pool slot) to avoid
per-block allocation of the 128 MB working set (cost[]+from[]+fromRef[]).

---

## Parallel Compression

All levels use goroutine-based parallel block compression. The `Writer` dispatches
full blocks to a worker pool (up to min(GOMAXPROCS, 8) workers), then an
in-order serializer goroutine writes compressed results to the underlying writer.

```
Write(p) → accumulate in buf →
  when buf full: dispatchBlock(idx, copy(buf)) → jobs channel
                  worker: getEncoderState → compressBlock → putEncoderState
                          → results channel (with block index)
  serializer: pending map ordered by idx → write to w.w in order
Close() → close(jobs) → wait serialDone → writeEndOfStream
```

Each worker obtains an `encoderState` from a per-level `sync.Pool`, which
pre-allocates hash tables, chain table, and DP arrays. States are returned
to the pool after each block for reuse.

---

## Data Structures

### encoderState

Per-worker reusable allocation. Obtained from `sync.Pool` before each block,
returned after.

```go
type encoderState struct {
    dual       dualHashTable   // short (4-byte hash) + long (8-byte hash)
    chainTable []int32         // collision chain; chainTable[pos&mask] = prev pos with same hash
    chainMask  int
    recentOff  [3]uint32       // LRU of recent match offsets
    litBuf     []byte          // accumulated literals for current block
    seqs       []sequence      // accumulated sequence list
    hScratch   *huff0.Scratch  // reused Huffman scratch (ReusePolicyNone)
    dpCost     []int           // pre-allocated DP cost array for optimalParse
    dpFrom     []int32         // pre-allocated DP from array
    dpFromRef  []int32         // pre-allocated DP fromRef array
}

type sequence struct {
    litLen   uint32  // number of literals preceding this match
    matchLen uint32  // match length (≥ minMatch = 4)
    offset   uint32  // back-reference distance (bytes)
}
```

### Hash Tables

```go
type hashTable[V uint16 | uint32] struct { entries []V }
```

Level 1 uses `uint32` (short only). Levels 2–5 use `dualHashTable` with
both short and long tables.

**Hash functions:**

```go
// 4-byte position hash (Knuth multiplicative)
func hash4(x uint64, bits uint8) uint32 {
    return (uint32(x) * 2654435761) >> (32 - bits)
}
// 8-byte position hash (Fibonacci)
func hash8(x uint64, bits uint8) uint32 {
    return uint32((x * 0xcf1bbcdcb7a56463) >> (64 - bits))
}
```

### Chain Table (Levels 3–5)

`chainTable[pos & windowMask] = int32(previousPositionWithSameShortHash)`

Enables depth-bounded traversal of all prior positions that collide in the
short hash bucket. Built incrementally: each `update(pos)` stores the previous
occupant of its short-hash slot as the chain predecessor.

### LRU Semantics

3-slot recent-offset LRU, initial state `[1, 4, 8]`.

**L2 (raw token format):** Sequences carry `repIdx` (0–2). The encoder rotates
by bringing `recentOff[repIdx]` to slot 0.

**L3–L5 (FSE format):** All sequences carry the actual offset value. The encoder
uses **naïve push** (shift [1]→[2], [0]→[1], new→[0]) for all matches,
matching the decoder's unconditional push-to-front.

---

## Entropy Coding

### Huffman Literals (huff0)

Literals for Levels 2–5 are Huffman-compressed using the `huff0` library
(vendored from klauspost/compress). Key details:

- **ReusePolicyNone**: each block carries its own full Huffman table.
  Required for independent block decompression.
- **1X vs 4X**: `Compress1X` / `Decompress1X` for blocks < 1 KB;
  `Compress4X` / `Decompress4X` for ≥ 1 KB (parallel sub-streams).
- **Fallback**: if Huffman output ≥ input size, literals stored raw
  (`huffSize = 0` in the block header).
- **entropyMode**: `entropyNone` (L1) → raw; `entropyAdaptHuff` (L2–L5) → 1X or 4X
  based on literal buffer size (< 1 KB → 1X; ≥ 1 KB → 4X). L2 previously used
  `entropyStaticHuff` (1X only), which failed silently on the multi-MB literal buffers
  that 8 MB blocks produce; switching to `entropyAdaptHuff` fixes this.

### Compressibility Probe

Before invoking the (potentially expensive) lazy or optimal-parse encoder, L3–L5
sample the first 4 KB of each block with a 1 K-entry hash table to estimate match
density (`matchFrac` = matches per 64 sampled positions):

- **`matchFrac == 0`** — no 4-byte repeats found. LZ work is skipped entirely; the
  block is passed directly to `buildFSEBlock` as a single all-literals sequence so
  Huffman coding still applies. This matters for already-LZ-compressed input (e.g.
  `.gz`, `.compress` files) whose byte distributions remain skewed even though LZ
  patterns are absent.
- **L5, `matchFrac < 8`** — very sparse LZ patterns. Falls back to the L3 (lazy)
  encoder to avoid the 128 MB DP working set of `optimalParse` for near-zero gain.

### FSE Sequence Coding (Levels 2–5)

All three code streams (llCodes, mlCodes, ofCodes) are FSE-compressed
independently using the `fse` library (vendored from klauspost/compress).
This applies to L2–L5; L1 uses block type 0x04 with no sequence entropy coding.

**Critical:** Each stream uses its own `fse.Scratch` instance. Reusing a single
scratch across streams causes FSE to emit "reuse table" markers (shorter bitstream)
that a fresh decoder scratch cannot parse, producing EOF errors.

If FSE output ≥ raw codes, the stream is stored raw (high bit of the 3-byte
size prefix, bit 23, signals raw vs. FSE).

---

## Checksums

**Algorithm:** Pure-Go XXH64 (64-bit, seed 0). Lower 32 bits used everywhere.

**Per-block checksum:** Computed over the **uncompressed** block content by the
producer goroutine before dispatch (to maintain sequential ordering of the
content checksum). Written after the compressed block data.

**Content checksum:** Running XXH64 over all uncompressed data in sequential
block order. Written in the end-of-stream marker.

**Header checksum:** Computed over `FLG || BLK || [ContentSize]`.

---

## Streaming Design

`Writer` and `Reader` implement `io.WriteCloser` / `io.ReadCloser`.

**Writer (parallel):**
- Wraps underlying `io.Writer` with a `bufio.Writer` (64 KB buffer).
- Accumulates input into `buf` (up to `blockSize`).
- On each full block: copies block data and sends to `jobs` channel with index.
- Worker goroutines compress and send `parallelResult{idx, payload, ...}` to
  `results` channel.
- Serializer goroutine drains `results` in `idx` order, writing to `w.w`.
- `Close()` closes `jobs`, waits for serializer via `serialDone`, writes EOS.
- Checksum of uncompressed data computed by producer (main goroutine) before
  dispatch to maintain sequential ordering.

**Reader (sequential):**
- Wraps underlying `io.Reader` with a `bufio.Reader` (64 KB buffer).
- Decodes one block per `Read` call when its internal buffer is drained.
- Decompressed data is buffered in `r.buf`; subsequent `Read` calls drain it.
- `Reset(src)` reuses allocations for a new stream.
