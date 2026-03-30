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

| Level | Name | Block/Window | Strategy | Literals | Sequences |
|-------|------|-------------|----------|----------|-----------|
| 1 | Fastest | 8 MB | Single hash, greedy | Raw | Compact tokens, 3-byte offset |
| 2 | Fast | 8 MB | Dual hash + rep×3, greedy | Huffman 1X/4X | FSE compact codes |
| 3 | Default | 8 MB | Dual hash + chain(8), lazy(2) | Huffman 1X/4X | FSE compact codes |
| 4 | Better | 8 MB | Dual hash + chain(32), lazy(4) | Huffman 1X/4X | FSE compact codes |
| 5 | Best | 8 MB | Dual hash + chain(4), optimal parse | Huffman 1X/4X | FSE compact codes |

All levels use goroutine-based parallel block compression (up to 8 workers).
Incompressible blocks are stored verbatim regardless of level.

---

## Comparison with Other Algorithms

Run `just compare <target>` or `just bench-compare <target>` to measure on your machine.

```sh
just compare /usr/share/man
just compare ./myproject
```

> Ratio = compressed / original (lower is better).
> Results vary by data type, file size, and CPU.

---

## Format

The `.az` format is a simple framed block format:

```
Frame:  [Magic(4)] [FLG(1)] [BLK(1)] [ContentSize(8)?] [HdrCksum(4)]
Block:  [BlockHeader(4)] [BlockData(N)] [BlockCksum(4)?]  ...
EOS:    [0x80000000(4)] [ContentCksum(4)?]
```

- **Magic:** `0x415A0001`
- **Block types:** compact tokens (0x04, L1), Huffman+FSE (0x02, L2–L5), raw literals+tokens (0x00/0x01, fallback), RLE (0x03)
- **Checksums:** lower 32 bits of XXH64, per-block and end-of-stream
- **Independent blocks:** each block decompresses without prior blocks → seekable, parallelisable

See [ARCHITECTURE.md](ARCHITECTURE.md) for the full specification.

---

## License

See LICENSE file.
