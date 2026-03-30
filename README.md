# az

A general-purpose compression library and CLI tool for Go.

`az` combines LZ77 back-reference matching with Huffman and FSE (Finite State
Entropy) coding to deliver competitive compression ratios at high throughput.
It has **no external dependencies** — entropy codecs are vendored under `pkg/`.

---

## Installation

```sh
go install az/cmd/az@latest
```

Or build from source:

```sh
git clone <repo>
cd az
go build -o az ./cmd/az
```

---

## CLI Usage

```
az [OPTIONS] [FILE...]

Compression levels:
  -1              Fastest  — pure LZ77, no entropy (≈ LZ4 speed)
  -2              Fast     — dual hash + Huffman literals
  -3              Default  — lazy match + Huffman + FSE sequences
  -4              Better   — deeper search + chain matching
  -5              Best     — optimal parse

Modes:
  -d, --decompress    Decompress
  -k, --keep          Keep source file (default: remove after success)
  -c, --stdout        Write to stdout
  -f, --force         Overwrite existing output files
  -t, --test          Test integrity (decompress to /dev/null)
  -v, --verbose       Print compression ratio and speed
  -o FILE             Output filename (single input only)
  --no-checksum       Disable block checksums

With no FILE, or when FILE is -, reads stdin and writes stdout.
Compressed files get the .az suffix; decompression removes it.
```

### Examples

```sh
# Compress a file (produces file.txt.az, removes file.txt)
az file.txt

# Compress at level 1 (fastest), keep original
az -1 -k file.txt

# Decompress
az -d file.txt.az

# Pipe
cat data.bin | az -c | az -d -c > data.bin

# Verbose output
az -v -3 largefile.bin
# → largefile.bin: 104857600 → 41943040 bytes (0.400 ratio, 312.5 MB/s)

# Test integrity without writing output
az -t archive.az

# Compress with tar (entire directory)
tar -cf - ./mydir | az -c > mydir.tar.az
tar -xf - < <(az -d -c mydir.tar.az)
```

---

## Go Package API

```go
import "az"

// One-shot compress/decompress
compressed, err := az.Compress(data, az.Level3)
original, err := az.Decompress(compressed)

// Streaming writer
w := az.NewWriter(dst,
    az.WithLevel(az.Level4),
    az.WithChecksum(true),
)
w.Write(data)
w.Close()

// Streaming reader
r := az.NewReader(src)
io.Copy(dst, r)
r.Close()

// Reset for reuse (avoids re-allocation)
w.Reset(newDst)
r.Reset(newSrc)
```

### Options

| Option | Default | Description |
|--------|---------|-------------|
| `WithLevel(l)` | `Level3` | Compression level 1–5 |
| `WithChecksum(b)` | `true` | Per-block + content XXH64 checksum |
| `WithContentSize(b)` | `false` | Embed uncompressed size in frame header |

---

## Compression Levels

| Level | Name | Window | Strategy | Literals | Sequences | Compress | Decompress |
|-------|------|--------|----------|----------|-----------|----------|------------|
| 1 | Fastest | 64 KB | Single hash, greedy | Raw | LZ4 tokens | ~295 MB/s | ~675 MB/s |
| 2 | Fast | 256 KB | Dual hash + rep×3 | Huffman 1X | LZ4 tokens | ~200 MB/s | ~590 MB/s |
| 3 | Default | 1 MB | Dual hash + lazy(1) | Huffman 4X | FSE | ~90 MB/s | ~680 MB/s |
| 4 | Better | 4 MB | Dual hash + lazy(2) + chain(4) | Huffman 4X | FSE | ~50 MB/s | ~780 MB/s |
| 5 | Best | 8 MB | Optimal parse + chain(16) | Huffman 4X | FSE | ~25 MB/s | ~850 MB/s |

Speeds measured on Apple M-series (ARM64) compressing 49 MB of text.
Incompressible data is stored verbatim and approaches memory bandwidth.

---

## Comparison with Other Algorithms

Results on two corpora on Apple M-series (ARM64). Run `just bench-compare <target>`
to reproduce on your machine.

### Text corpus — 49 MB (`/usr/share/man`, uncompressed troff)

| Algorithm | Ratio | Compress MB/s | Decompress MB/s |
|-----------|-------|--------------|-----------------|
| xz -9 | 0.116 | 4 | 191 |
| xz -6 | 0.135 | 8 | 316 |
| zstd -19 | 0.132 | 9 | 1473 |
| zstd -9 | 0.165 | 258 | 1409 |
| zstd -3 | 0.208 | 1012 | 1237 |
| gzip -9 | 0.249 | 33 | 1077 |
| gzip -6 | 0.250 | 48 | 1064 |
| zstd -1 | 0.251 | 1467 | 1319 |
| lz4 -9 | 0.282 | 367 | 2066 |
| gzip -1 | 0.305 | 151 | 969 |
| lz4 | 0.383 | 1881 | 2066 |
| **az -2** | **0.575** | **200** | **588** |
| **az -5** | **0.581** | **24** | **846** |
| **az -4** | **0.660** | **49** | **777** |
| **az -1** | **0.702** | **295** | **674** |
| **az -3** | **0.768** | **89** | **679** |

### Source code corpus — 44 MB (klauspost/compress Go source + test data)

| Algorithm | Ratio | Compress MB/s | Decompress MB/s |
|-----------|-------|--------------|-----------------|
| xz -6 | 0.692 | 8 | 129 |
| zstd -19 | 0.695 | 11 | 1945 |
| zstd -9 | 0.728 | 427 | 2306 |
| zstd -3 | 0.748 | 1273 | 2477 |
| zstd -1 | 0.810 | 2578 | 2885 |
| gzip -6 | 0.828 | 53 | 1045 |
| lz4 -9 | 0.832 | 293 | 2547 |
| gzip -1 | 0.837 | 71 | 1058 |
| **az -5** | **0.862** | **6** | **1516** |
| lz4 | 0.863 | 2308 | 2591 |
| **az -2** | **0.872** | **80** | **973** |
| **az -4** | **0.880** | **18** | **1499** |
| **az -3** | **0.895** | **70** | **1726** |
| **az -1** | **0.913** | **737** | **1928** |

> Ratio = compressed / original (lower is better). Speed = uncompressed bytes ÷ elapsed time.
> Results vary by data type, file size, and CPU. Run `just bench-compare <path>` to measure
> your own workload.

---

## Format

The `.az` format is a simple framed block format:

```
Frame:  [Magic(4)] [FLG(1)] [BLK(1)] [ContentSize(8)?] [HdrCksum(4)]
Block:  [BlockHeader(4)] [BlockData(N)] [BlockCksum(4)?]  ...
EOS:    [0x80000000(4)] [ContentCksum(4)?]
```

- **Magic:** `0x415A0001`
- **Block types:** raw literals + tokens (0x00/0x01), Huffman+FSE (0x02), RLE (0x03)
- **Checksums:** lower 32 bits of XXH64, per-block and end-of-stream
- **Independent blocks:** each block decompresses without prior blocks → seekable, parallelisable

See [ARCHITECTURE.md](ARCHITECTURE.md) for the full specification.

---

## License

See LICENSE file.
