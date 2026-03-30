# az

A general-purpose compression library and CLI tool for Go, built on top of
**LZ4** (levels 1–2) and **Zstandard** (levels 3–5).  No external imports —
the algorithm implementations are vendored under `internal/`.

---

## Installation

```sh
go install github.com/go-again/az/cmd/az@latest
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
  -1              Fastest  — lz4 fast  (ratio ~0.36, ~550 MB/s on text)
  -2              Fast     — lz4 HC    (ratio ~0.29, ~290 MB/s on text)
  -3              Default  — zstd-6    (ratio ~0.18, ~350 MB/s on text)
  -4              Better   — zstd-12   (ratio ~0.16, ~270 MB/s on text)
  -5              Best     — zstd-18   (ratio ~0.15,  ~60 MB/s on text)

Modes:
  -d, --decompress    Decompress
  -k, --keep          Keep source file (default: remove after success)
  -c, --stdout        Write to stdout
  -f, --force         Overwrite existing output files
  -t, --test          Test integrity (decompress to /dev/null)
  -v, --verbose       Print compression ratio and speed
  -o FILE             Output filename (single input only)
  --no-checksum       Disable checksums

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
# → largefile.bin: 104857600 → 19203051 bytes (0.183 ratio, 351.0 MB/s)

# Test integrity without writing output
az -t archive.az

# Compress with tar (entire directory)
tar -cf - ./mydir | az -c > mydir.tar.az
tar -xf - < <(az -d -c mydir.tar.az)
```

---

## Go Package API

```go
import "github.com/go-again/az"

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

// Streaming reader — auto-detects lz4 or zstd format
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
| `WithChecksum(b)` | `true` | Enable/disable frame checksums |
| `WithContentSize(b)` | `false` | Embed uncompressed size (one-shot only) |

---

## Compression Levels

| Level | Algorithm | Compress | Decompress | Ratio |
|-------|-----------|----------|------------|-------|
| `-1` fastest | lz4 fast | ~547 MB/s | ~736 MB/s | 0.359 |
| `-2` fast | lz4 HC | ~289 MB/s | ~736 MB/s | 0.294 |
| `-3` default | zstd-6 | ~351 MB/s | ~615 MB/s | 0.183 |
| `-4` better | zstd-12 | ~273 MB/s | ~703 MB/s | 0.162 |
| `-5` best | zstd-18 | ~59 MB/s | ~703 MB/s | 0.147 |

## Comparison

Measured on Apple M2 Max. Source: `/usr/share/man` tar (51 MB, text).

| Algorithm | Ratio | Compress | Decompress |
|-----------|-------|----------|------------|
| **az -1** | 0.359 | 0.09s | 0.07s |
| **az -2** | 0.294 | 0.17s | 0.07s |
| **az -3** | 0.183 | 0.14s | 0.07s |
| **az -4** | 0.162 | 0.18s | 0.07s |
| **az -5** | 0.147 | 0.83s | 0.07s |
| lz4 | 0.383 | 0.05s | 0.05s |
| lz4 -9 | 0.281 | 0.16s | 0.04s |
| gzip -1 | 0.305 | 0.36s | 0.07s |
| gzip -6 | 0.250 | 1.09s | 0.07s |
| gzip -9 | 0.249 | 1.56s | 0.06s |
| zstd -1 | 0.251 | 0.06s | 0.06s |
| zstd -3 | 0.208 | 0.07s | 0.06s |
| zstd -9 | 0.165 | 0.21s | 0.05s |
| zstd -19 | 0.132 | 6.21s | 0.05s |
| xz -1 | 0.204 | 0.26s | 0.08s |
| xz -6 | 0.135 | 6.39s | 0.19s |

Source: `.compress` tar (123 MB, Go source).

| Algorithm | Ratio | Compress | Decompress |
|-----------|-------|----------|------------|
| **az -1** | 0.946 | 0.23s | 0.07s |
| **az -2** | 0.942 | 0.09s | 0.07s |
| **az -3** | 0.860 | 0.12s | 0.07s |
| **az -4** | 0.828 | 0.21s | 0.06s |
| **az -5** | 0.814 | 1.65s | 0.07s |
| lz4 | 0.942 | 0.06s | 0.05s |
| lz4 -9 | 0.929 | 0.30s | 0.05s |
| gzip -1 | 0.931 | 1.91s | 0.09s |
| gzip -6 | 0.928 | 2.29s | 0.09s |
| gzip -9 | 0.928 | 2.61s | 0.09s |
| zstd -1 | 0.911 | 0.06s | 0.04s |
| zstd -3 | 0.859 | 0.08s | 0.05s |
| zstd -9 | 0.846 | 0.15s | 0.05s |
| zstd -19 | 0.807 | 7.57s | 0.06s |
| xz -1 | 0.860 | 2.46s | 0.18s |
| xz -6 | 0.812 | 7.37s | 0.42s |

Ratio = compressed / original (lower is better).

---

## Wire Format

Levels 1–2 produce native **LZ4 frames** (magic `0x184D2204`).
Levels 3–5 produce native **Zstandard frames** (magic `0xFD2FB528`).

The decompressor auto-detects the format from the magic bytes.
`.az` files are valid lz4 or zstd streams, so standard tools work directly:

```sh
lz4 -d file.az       # if compressed with az -1 or -2
zstd -d file.az      # if compressed with az -3, -4, or -5
```

---

## License

See LICENSE file.
