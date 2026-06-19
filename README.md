# az

A general-purpose compression CLI tool and library for Go, built on top of
**LZ4** (levels 1–2) and **Zstandard** (levels 3–5).  No external imports.

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
  -1              Fastest  — lz4 fast  (ratio ~0.36, ~430 MB/s on text)
  -2              Fast     — lz4 HC    (ratio ~0.29, ~340 MB/s on text)
  -3              Default  — zstd-6    (ratio ~0.18, ~340 MB/s on text)
  -4              Better   — zstd-12   (ratio ~0.16, ~290 MB/s on text)
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

# Compress/decompress with tar (entire directory)
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
| `-1` fastest | lz4 fast | ~430 MB/s | ~860 MB/s | 0.359 |
| `-2` fast | lz4 HC | ~344 MB/s | ~860 MB/s | 0.294 |
| `-3` default | zstd-6 | ~344 MB/s | ~737 MB/s | 0.183 |
| `-4` better | zstd-12 | ~286 MB/s | ~860 MB/s | 0.162 |
| `-5` best | zstd-18 | ~61 MB/s | ~860 MB/s | 0.147 |

## Comparison

Measured on Apple M2 Max. Source: `/usr/share/man` tar (51 MB, text).

| Algorithm | Ratio | Compress | Decompress |
|-----------|-------|----------|------------|
| **az -1** | 0.359 | 0.12s | 0.06s |
| **az -2** | 0.294 | 0.15s | 0.06s |
| **az -3** | 0.183 | 0.15s | 0.07s |
| **az -4** | 0.162 | 0.18s | 0.06s |
| **az -5** | 0.147 | 0.85s | 0.06s |
| lz4 -3 | 0.292 | 0.07s | 0.04s |
| lz4 -6 | 0.282 | 0.11s | 0.04s |
| zstd -6 | 0.189 | 0.16s | 0.06s |
| zstd -12 | 0.161 | 0.42s | 0.05s |
| zstd -18 | 0.134 | 4.01s | 0.05s |
| gzip -1 | 0.305 | 0.35s | 0.07s |
| gzip -6 | 0.250 | 1.05s | 0.06s |
| gzip -9 | 0.249 | 1.51s | 0.06s |
| xz -1 | 0.204 | 0.25s | 0.07s |
| xz -6 | 0.135 | 6.25s | 0.18s |

Source: `az` repo tar (255 MB, Go source).

| Algorithm | Ratio | Compress | Decompress |
|-----------|-------|----------|------------|
| **az -1** | 0.940 | 0.17s | 0.13s |
| **az -2** | 0.934 | 0.36s | 0.13s |
| **az -3** | 0.847 | 0.22s | 0.12s |
| **az -4** | 0.815 | 0.43s | 0.12s |
| **az -5** | 0.798 | 3.02s | 0.12s |
| lz4 -3 | 0.919 | 0.52s | 0.09s |
| lz4 -6 | 0.918 | 0.57s | 0.09s |
| zstd -6 | 0.844 | 0.26s | 0.09s |
| zstd -12 | 0.828 | 0.54s | 0.09s |
| zstd -18 | 0.791 | 9.59s | 0.10s |
| gzip -1 | 0.910 | 3.93s | 0.17s |
| gzip -6 | 0.904 | 5.03s | 0.17s |
| gzip -9 | 0.904 | 5.80s | 0.17s |
| xz -1 | 0.842 | 4.74s | 0.32s |
| xz -6 | 0.792 | 8.90s | 0.55s |

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
