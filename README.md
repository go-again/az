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
git clone https://github.com/go-again/az.git
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

// Poolable one-shot codecs — reuse heavy lz4/zstd objects across many
// independent buffers (e.g. one per pooled worker). EncodeAll output is
// byte-identical to Compress; DecodeAll output equals Decompress, so the
// at-rest format is unchanged.
enc := az.NewEncoder()
frame, err := enc.EncodeAll(dst[:0], data, az.Level3) // appends into dst
dec := az.NewDecoder()
plain, err := dec.DecodeAll(out[:0], frame)           // auto-detects lz4/zstd

// Bounded decode for untrusted frames (decompression-bomb defense): stops with
// az.ErrTooLarge once output would exceed max, without allocating beyond it.
plain, err = dec.DecodeAllLimit(out[:0], frame, maxBytes)
if errors.Is(err, az.ErrTooLarge) { /* reject: frame too large / corrupt */ }
```

An `Encoder`/`Decoder` is **not** safe for concurrent use; pool one per
goroutine:

```go
var encPool = sync.Pool{New: func() any { return az.NewEncoder() }}

e := encPool.Get().(*az.Encoder)
frame, err := e.EncodeAll(scratch[:0], data, az.Level3)
encPool.Put(e)
```

### Options

| Option | Default | Description |
|--------|---------|-------------|
| `WithLevel(l)` | `Level3` | Compression level 1–5 |
| `WithChecksum(b)` | `true` | Enable/disable frame checksums |
| `WithConcurrency(n)` | `GOMAXPROCS` | Cap the codec's worker goroutines; use `1` for one writer per request |
| `WithContentSize(b)` | — | No-op; one-shot frames always embed the uncompressed size |

`Writer` also has `Flush()`, which pushes everything written so far to the
underlying writer without ending the stream — what a streaming response needs.

---

## HTTP middleware

`azhttp` compresses HTTP responses with az, the way `gzhttp` does for gzip.

```go
import "github.com/go-again/az/azhttp"

// Server: compress responses for clients that accept it.
http.ListenAndServe(":8080", azhttp.Handler(mux))

// Client: request and decode az codings transparently.
client := &http.Client{Transport: azhttp.Transport(http.DefaultTransport)}
```

Levels 3–5 travel as the standard **`zstd`** content coding (RFC 8878 — current
browsers, curl and CDNs speak it). Levels 1–2 travel as **`lz4`**, which is
*not* a registered coding, so azhttp only ever sends it to a client that named
`lz4` in `Accept-Encoding` — a safe opt-in for internal traffic, invisible to
the public web.

```go
compress, err := azhttp.NewWrapper(
    azhttp.MinSize(2048),        // skip bodies too small to be worth it
    azhttp.ZstdLevel(az.Level4), // trade CPU for ratio
    azhttp.EnableLZ4(false),     // public site: standard codings only
    azhttp.SuffixETag("-az"),    // keep encoded and identity ETags apart
)
http.Handle("/", compress(mux))

// Accept compressed uploads, capped at 8 MiB decompressed.
http.Handle("/upload", azhttp.DecompressRequests(8<<20)(uploadHandler))
```

Out of the box it negotiates `Accept-Encoding` q-values, always sets
`Vary: Accept-Encoding`, buffers until `MinSize`, sniffs and skips
already-compressed content types, honours `Flush` for streaming responses, and
leaves HEAD, ranged, bodiless and already-encoded responses alone. One pooled
`az.Writer` per response at concurrency 1, so a request costs one goroutine.

Run `just example-http` for a live comparison, or see
[`examples/http`](examples/http/main.go) and the package examples.

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

### Uncompressed size in the header

The one-shot paths (`Compress`, `Encoder.EncodeAll`) know the input length, so
**every** frame they emit records it — LZ4 `Content_Size` for levels 1–2,
Zstandard `Frame_Content_Size` for levels 3–5, at every input size including
empty. Decoders can therefore size the output buffer from the header alone
(`ZSTD_getFrameContentSize` → allocate → `ZSTD_decompress`) instead of streaming,
and `zstd -l` / `lz4 --list` report the real size.

Zstd frames stay *windowed* (no single-segment flag) above ~128 KB, so a large
frame does not force a decoder to adopt a window as big as the whole payload;
the size is carried in a 4- or 8-byte `Frame_Content_Size` field instead. Inputs
under 256 bytes also use the 4-byte field — the 2-byte encoding is biased by 256
and cannot express them — which costs 4 bytes per small frame.

The streaming `Writer` emits the header before it has seen the input, so its
frames have no content size. Use `Compress`/`Encoder` when consumers need it.

---

## Development

Recipes live in the [`justfile`](justfile) ([`just`](https://just.systems)):

```sh
just              # pre-commit gate: build + test + lint
just --list       # every recipe, grouped
just test-race    # race detector
just lint         # fmt + vet + staticcheck + golangci-lint + modernize + tidy/deps checks
just lint-fix     # apply what the linters can fix on their own
just ci           # everything CI runs, including the cross-build matrix
just example-http # run the azhttp demo
```

Lint tools are pinned in the justfile and run via `go run`, so there is nothing
to install and CI runs the same versions. Plain `go build ./...` /
`go test ./...` work too.

Agent-facing docs: [`AGENTS.md`](AGENTS.md) for working *on* az,
[`skills/`](skills/) for integrating it into another project.

---

## License

See LICENSE file.
