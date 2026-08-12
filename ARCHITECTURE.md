# az — Architecture & Technical Reference

## Overview

`az` is a thin Go facade over two battle-tested compression algorithms:

- **Levels 1–2** delegate to **LZ4** (`internal/lz4/`) — near-memory-bandwidth speed with reasonable compression.
- **Levels 3–5** delegate to **Zstandard** (`internal/zstd/`) — high compression ratio at multi-hundred MB/s throughput.

The root `az` package provides a unified API (`Compress`, `Decompress`, `NewWriter`, `NewReader`) that hides the format difference.  No external Go module imports; all algorithm code is vendored under `internal/`.

---

## Package Structure

```
az/                         Public API
├── az.go                   Writer, Reader, Compress, Decompress
├── encoder.go              Pooled Encoder/Decoder (EncodeAll/DecodeAll)
├── options.go              Level constants, Options struct, Option helpers
├── errors.go               ErrCorrupted, ErrChecksumFail, ErrLevel
├── azhttp/                 HTTP middleware (response/request coding, client transport)
├── cmd/az/main.go          CLI tool
├── examples/http/          Runnable azhttp demo
├── internal/
│   ├── lz4/                Adapted from github.com/pierrec/lz4/v4
│   │   ├── lz4.go, writer.go, reader.go, options.go, ...
│   │   ├── block/          Core LZ4 block compression (Compressor, CompressorHC)
│   │   ├── stream/         LZ4 frame format
│   │   └── xxh32/          XXHash32 for frame checksums
│   ├── zstd/               Adapted from github.com/klauspost/compress/zstd
│   │   ├── encoder.go, decoder.go, ...
│   │   └── internal/xxhash/ XXHash64 for zstd frame checksums
│   ├── huff0/              Huffman codec used by zstd
│   ├── fse/                Finite State Entropy codec used by zstd and huff0
│   ├── compress/           ShannonEntropyBits helper used by zstd encoder
│   ├── le/                 Little-endian read helpers
│   └── cpuinfo/            CPU feature detection for SIMD paths
```

---

## Wire Formats

### LZ4 Frame (levels 1–2)

Standard LZ4 framing as specified by the LZ4 frame format:

```
Magic (4B)          0x184D2204
Frame Descriptor    Flags, block size, optional content size, header checksum
Data blocks         [Size(4B)] [Compressed data] [Optional block checksum]
End mark            0x00000000
Content checksum    Optional XXHash32
```

- Block checksum and content checksum are enabled by `WithChecksum(true)` (default).
- Block independence is set; blocks can be decompressed in parallel.

### Zstandard Frame (levels 3–5)

Standard Zstandard framing:

```
Magic (4B)          0xFD2FB528
Frame Header        Flags, window descriptor, optional content size, optional dict ID
Blocks              [Block header (3B)] [Block data]
Checksum            Optional XXHash64 (lower 32 bits)
```

- CRC checksum is enabled by `WithChecksum(true)` (default).
- **`Frame_Content_Size` is always present in one-shot output.** `Compress` and
  `Encoder.EncodeAll` declare the input length (`ResetContentSize`), so the
  header records it at every size: 2 bytes where the compact encoding fits,
  4 or 8 otherwise, and 4 bytes for sizes < 256 (that encoding is biased by 256,
  so a windowed frame cannot express them in 2). Frames stay windowed above
  ~128 KB rather than single-segment, so a large frame never forces a decoder to
  adopt a window as big as the payload. The streaming `Writer` emits its header
  before it has seen the input, so its frames carry no size.

---

## Level Mapping

| az level | Backend | Internal encoder | Compression depth |
|----------|---------|-----------------|-------------------|
| 1 | lz4 | `CompressBlock` (hash-only, no chain) | fastest |
| 2 | lz4 | `CompressorHC` depth 1024 | moderate HC |
| 3 | zstd | `doubleFastEncoder` (SpeedDefault) | dual hash table |
| 4 | zstd | `betterFastEncoder` (SpeedBetterCompression) | dual hash + chains |
| 5 | zstd | `bestFastEncoder` (SpeedBestCompression) | exhaustive search |

---

## Auto-Detection

`Reader.Read` peeks at the first 4 bytes of the stream using `bufio.Reader.Peek`
(non-consuming) and matches against the magic:

| Magic (LE) | Format | Backend |
|-----------|--------|---------|
| `0x184D2204` | LZ4 | `internal/lz4.Reader` |
| `0xFD2FB528` | Zstandard | `internal/zstd.Decoder` |
| other | — | `ErrCorrupted` |

---

## Streaming Design

### Writer

`Writer` holds either an `*internal/lz4.Writer` or `*internal/zstd.Encoder` depending on the level.  All `Write`, `Close`, and `Reset` calls are forwarded directly.

LZ4 and zstd each manage their own internal concurrency, both driven by
`WithConcurrency(n)` (`n <= 0` → `runtime.GOMAXPROCS`):
- LZ4: `ConcurrencyOption`.
- zstd: `WithEncoderConcurrency`.

`Flush` forwards to the codec's own flush, ending the current block group
without ending the frame.

### Reader

`Reader` wraps a `bufio.Reader` seeded from the provided `io.Reader`.  On the first `Read`, it peeks 4 bytes to detect format, creates the appropriate sub-reader pointing at the same `bufio.Reader` (so no bytes are consumed before the sub-reader sees them), then delegates all subsequent reads.

`Reset(src)` discards sub-reader state and re-initializes on the next `Read`.

---

## HTTP content codings (`azhttp`)

az frames are the payload of two content codings:

| az levels | Frame | `Content-Encoding` | Standard? |
|-----------|-------|--------------------|-----------|
| 3–5 | Zstandard | `zstd` | Yes — RFC 8878, IANA-registered, supported by current browsers/CDNs |
| 1–2 | LZ4 | `lz4` | No — sent only to a client that names `lz4` in `Accept-Encoding` |

The server middleware buffers up to `max(MinSize, 512)` body bytes before
deciding whether to compress, so it can still amend the response headers
(`Content-Encoding`, `Content-Length`, `Accept-Ranges`, `ETag`) and sniff
`Content-Type`. It compresses through a pooled `az.Writer` built with
`WithConcurrency(1)`: one goroutine per response, and a writer that can be
`Reset`-reused after `Close`.

The client transport sets `Accept-Encoding`, then swaps in an `az.Reader` over
the response body and clears `Content-Encoding`/`Content-Length`, mirroring what
net/http does for the gzip it handles itself.

---

## Checksums

- **LZ4:** XXHash32 per-block and content checksums (`ChecksumOption`).
- **zstd:** XXHash64 content checksum (`WithEncoderCRC`).

Both are enabled by default via `WithChecksum(true)`.

---

## Performance Notes

On Apple M2 Max (arm64):

| Level | Data | Speed | Ratio |
|-------|------|-------|-------|
| 1 (lz4-3) | 1 MB patterned | ~2400 MB/s | 0.004 |
| 2 (lz4-6) | 1 MB patterned | ~2400 MB/s | 0.004 |
| 3 (zstd-6) | 1 MB patterned | ~2000 MB/s | 0.0002 |
| 4 (zstd-12) | 1 MB patterned | ~2000 MB/s | 0.0002 |
| 5 (zstd-18) | 1 MB patterned | ~480 MB/s | 0.0001 |

`azhttp` response compression, whole middleware path including negotiation and
header handling (Apple M5, `go test -bench=Handler -benchtime=2s ./azhttp`):

| Coding | Body | Throughput | Allocs |
|--------|------|------------|--------|
| zstd (L3) | 2 KB | ~717 MB/s | 660 B/op, 6 allocs/op |
| zstd (L3) | 20 KB | ~4.2 GB/s | 596 B/op, 6 allocs/op |
| zstd (L3) | 100 KB | ~7.0 GB/s | 565 B/op, 6 allocs/op |
| lz4 (L1) | 2 KB | ~1.8 GB/s | 755 B/op, 7 allocs/op |
| lz4 (L1) | 20 KB | ~5.0 GB/s | 767 B/op, 7 allocs/op |
| lz4 (L1) | 100 KB | ~5.5 GB/s | 749 B/op, 7 allocs/op |

The near-flat allocation count is the point: the compressor and the
response-writer wrapper both come from pools, so a request costs a handful of
allocations regardless of body size (on very compressible test data — these are
throughput-per-input-byte figures, not ratios).
