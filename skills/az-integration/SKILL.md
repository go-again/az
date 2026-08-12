---
name: az-integration
description: Integrate the github.com/go-again/az compression library into a Go project — one-shot Compress/Decompress, streaming Writer/Reader, level selection (1–5), HTTP response/request compression via azhttp, error handling, and lz4/zstd wire-format compatibility. Use when adding or wiring az-based compression into Go code, including HTTP middleware. For high-throughput per-chunk/per-request compression with object pooling, use the az-pooling skill instead.
---

# Integrating `az` (github.com/go-again/az)

`az` is a dependency-free Go compression library. **Levels 1–2 are LZ4, levels
3–5 are Zstandard.** Decompression auto-detects the format from the frame magic,
so any `az`-compressed blob round-trips without you tracking which level made it.
`.az` output is a plain lz4 or zstd stream — the system `lz4`/`zstd` CLIs can read
it directly.

## Install

```sh
go get github.com/go-again/az@latest
```

```go
import "github.com/go-again/az"
```

## Choosing a level

| Level | Algorithm | Use when |
|-------|-----------|----------|
| `az.Level1` | LZ4 fast | throughput-critical, ratio secondary |
| `az.Level2` | LZ4 HC | a bit more ratio, still fast |
| `az.Level3` (`az.DefaultLevel`) | zstd-6 | general default — good ratio, fast |
| `az.Level4` | zstd-12 | better ratio, slower compress |
| `az.Level5` | zstd-18 | best ratio, much slower compress (decompress stays fast) |

Decompression speed is roughly level-independent; only compression cost rises
with level. Default to `az.Level3` unless you have a measured reason.

## One-shot (whole buffer in memory)

```go
comp, err := az.Compress(data, az.Level3)
if err != nil { /* only az.ErrLevel for a bad level */ }

orig, err := az.Decompress(comp) // level auto-detected
if err != nil { /* az.ErrCorrupted / az.ErrChecksumFail */ }
```

`Compress` enables a content checksum and records the uncompressed size for the
lz4 levels. Use this (or the pooled `Encoder` — see az-pooling) whenever the
exact at-rest bytes matter, e.g. content-addressed storage.

## Streaming (io.Writer / io.Reader)

For data that doesn't fit in memory, or to plug into `io.Copy` / `archive/tar`:

```go
w := az.NewWriter(dst, az.WithLevel(az.Level4)) // dst is any io.Writer
if _, err := w.Write(p); err != nil { /* ... */ }
if err := w.Close(); err != nil { /* MUST close to flush + finalise */ }

r := az.NewReader(src) // src is any io.Reader; format auto-detected
_, err := io.Copy(out, r)
r.Close()
```

`Writer.Close()` is mandatory — it writes the end-of-stream marker. Reuse a
`Writer`/`Reader` across streams with `Reset(newDst)` / `Reset(newSrc)` to avoid
re-allocating.

## Options

```go
az.WithLevel(az.Level3)    // default Level3
az.WithChecksum(false)     // default true; disable to shave a little CPU/size
az.WithConcurrency(1)      // default GOMAXPROCS; use 1 for one Writer per request/job
az.WithContentSize(true)   // no-op; one-shot frames always embed the uncompressed size
```

`Writer.Flush()` pushes buffered input downstream without ending the stream —
use it when a reader is waiting (streamed responses), not on every write.

## HTTP (`github.com/go-again/az/azhttp`)

Don't hand-roll compression into an `http.Handler` — the subpackage does it:

```go
http.ListenAndServe(":8080", azhttp.Handler(mux))                 // server
client := &http.Client{Transport: azhttp.Transport(nil)}          // client
http.Handle("/upload", azhttp.DecompressRequests(8<<20)(handler)) // uploads
```

Levels 3–5 go out as the standard **`zstd`** content coding; levels 1–2 as
**`lz4`**, which is non-standard and only ever sent to a client that named it in
`Accept-Encoding`. Negotiation, `Vary`, `MinSize`, content-type filtering, ETag
handling and streaming `Flush` are all handled. **See the `az-http` skill** for
options and the full contract.

## Errors

- `az.ErrLevel` — level outside 1–5 (from `Compress`/`EncodeAll`).
- `az.ErrCorrupted` — malformed/truncated input or unknown magic (decode).
- `az.ErrChecksumFail` — checksum mismatch (decode).

Match with `errors.Is`.

## Wire-format compatibility (important)

- `Compress`/`EncodeAll` produce **byte-identical** output for the same
  input+level. The streaming `Writer` does **not** match `Compress` byte-for-byte
  (it uses different frame options). If you store data and require stable at-rest
  bytes, standardise on `Compress`/`Encoder`, not `Writer`.
- Levels 1–2 emit LZ4 frames (magic `0x184D2204`); levels 3–5 emit Zstd frames
  (magic `0xFD2FB528`). External tools work: `lz4 -d file.az` (L1–2),
  `zstd -d file.az` (L3–5).
- `Compress`/`EncodeAll` always record the uncompressed size in the frame header
  (zstd `Frame_Content_Size` / lz4 `Content_Size`), for every input size. A
  consumer can size its output buffer from the header alone — e.g.
  `ZSTD_getFrameContentSize` → allocate → `ZSTD_decompress` in one shot. The
  streaming `Writer` cannot know the size up front, so its frames omit it.

## Quick checklist

- [ ] `go get github.com/go-again/az@latest`, pin a tagged version.
- [ ] Pick a level (default `Level3`); document why if not.
- [ ] One-shot for in-memory blobs, streaming for large/streamed data.
- [ ] Always `Close()` a `Writer`.
- [ ] Handle `ErrCorrupted`/`ErrChecksumFail` on decode.
- [ ] If compressing many small buffers at high frequency → use **az-pooling**.
