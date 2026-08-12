---
name: az-pooling
description: High-throughput compression with github.com/go-again/az using pooled Encoder/Decoder (EncodeAll/DecodeAll) to reuse heavy lz4/zstd codecs across many calls and cut GC/alloc churn, plus pooling a streaming Writer with WithConcurrency(1). Use when compressing or decompressing many independent buffers/chunks/messages at high frequency or concurrency (e.g. per-row, per-chunk, per-request, per-RPC), or when reusing a streaming Writer per request. For one-off compression, use the az-integration skill; for HTTP traffic, use az-http.
---

# High-throughput `az` with pooled Encoder/Decoder

When you compress **many independent buffers** (DB chunks, cache entries, RPC
payloads), calling `az.Compress`/`az.Decompress` per buffer constructs a fresh,
heavyweight codec each time — the zstd encoder/decoder objects especially cause
real GC pressure and CPU cost. `az.Encoder`/`az.Decoder` reuse those heavy
objects across calls.

**The output is byte-identical to `Compress`/`Decompress`**, so a store can mix
chunks written the old way with chunks written via the pool and round-trip both —
no migration, no at-rest format change.

## API

```go
type Encoder struct{ /* reusable lz4 writer + per-level zstd encoders */ }
func NewEncoder() *Encoder
func (e *Encoder) EncodeAll(dst, src []byte, level Level) ([]byte, error)

type Decoder struct{ /* reusable lz4 reader + zstd decoder */ }
func NewDecoder() *Decoder
func (d *Decoder) DecodeAll(dst, src []byte) ([]byte, error)
func (d *Decoder) DecodeAllLimit(dst, src []byte, max int) ([]byte, error)
```

Argument order is **`(dst, src)`** (append idiom): the frame is appended to `dst`
and the extended slice is returned; a `nil` dst allocates. `DecodeAll`
auto-detects lz4 vs zstd, exactly like `Decompress`.

- `EncodeAll(nil, src, lvl)` == `Compress(src, lvl)`, every level/input.
- `DecodeAll(nil, frame)` == `Decompress(frame)`.
- Errors: `ErrLevel` (bad level), `ErrCorrupted`/`ErrChecksumFail` (decode),
  `ErrTooLarge` (`DecodeAllLimit` only — output exceeds the cap).

## Decoding untrusted frames — `DecodeAllLimit` (bomb defense)

If the frames you decode could be **crafted or corrupt** (untrusted storage, a
network peer), use `DecodeAllLimit` instead of `DecodeAll`. It is `DecodeAll`
with a hard ceiling: it streams the decode and returns `ErrTooLarge` the moment
the output would exceed `max`, **without allocating materially beyond `max`** —
so a tiny frame that expands to gigabytes cannot exhaust memory.

```go
d := decPool.Get().(*az.Decoder)
plain, err := d.DecodeAllLimit(out[:0], frame, maxChunkSize)
decPool.Put(d)
if errors.Is(err, az.ErrTooLarge) {
    return nil, errors.New("frame exceeds chunk size (corrupt or hostile data)")
}
if err != nil {
    return nil, err // ErrCorrupted / ErrChecksumFail
}
```

- For a well-formed frame whose output is ≤ `max`, the result is identical to
  `DecodeAll(dst, src)`.
- It reuses the same pooled codecs as `DecodeAll` — so you keep the pooling win
  *and* the bound. (This is why you don't fall back to a fresh `az.Reader` +
  `io.LimitReader` per call.)
- `ErrTooLarge` is distinct from `ErrCorrupted` so you can tell a bomb from a
  malformed frame if you care to.

## Concurrency contract — pool one per goroutine

**An `Encoder`/`Decoder` is NOT safe for concurrent use.** Put them in a
`sync.Pool` and Get/Put around each operation:

```go
var (
    encPool = sync.Pool{New: func() any { return az.NewEncoder() }}
    decPool = sync.Pool{New: func() any { return az.NewDecoder() }}
)

func encodeChunk(dst, plain []byte, lvl az.Level) ([]byte, error) {
    e := encPool.Get().(*az.Encoder)
    defer encPool.Put(e)
    return e.EncodeAll(dst, plain, lvl) // dst can be a pooled scratch slice
}

func decodeChunk(dst, comp []byte) ([]byte, error) {
    d := decPool.Get().(*az.Decoder)
    defer decPool.Put(d)
    return d.DecodeAll(dst, comp)
}
```

Pair this with a pooled `dst` scratch buffer (e.g. `buf[:0]`) to also reuse the
output backing array. Copy the result out before returning the scratch to its
pool — the next `EncodeAll` will overwrite it.

## Pooling a streaming `Writer` (per-request/per-stream output)

`Encoder`/`Decoder` are for whole buffers. When the output is a *stream* you
compress as it is produced — an HTTP response, an export, a log shipper — pool
`az.Writer` instead, and create it with **`az.WithConcurrency(1)`**:

```go
var writerPool = sync.Pool{}

func getWriter(dst io.Writer) *az.Writer {
    if w, ok := writerPool.Get().(*az.Writer); ok {
        w.Reset(dst)
        return w
    }
    return az.NewWriter(dst, az.WithLevel(az.Level3), az.WithConcurrency(1))
}

func putWriter(w *az.Writer) {
    w.Reset(io.Discard) // don't let a pooled writer pin the finished stream
    writerPool.Put(w)
}

w := getWriter(dst)
defer putWriter(w)        // after Close
_, err := io.Copy(w, src)
err = w.Close()           // mandatory: writes the end-of-stream marker
```

Why concurrency 1 is not optional here:

- **Reuse correctness.** Only a single-threaded lz4 writer can be `Reset` and
  reused after `Close`; the concurrent one deadlocks. Levels 3–5 tolerate either,
  but one rule for both levels is simpler and always right.
- **Footprint.** The default is `GOMAXPROCS` workers *per writer*. If the work is
  already parallel (one writer per request), that multiplies goroutines and codec
  memory by the core count for no throughput gain.

Concurrency does not change the bytes produced, only how they are produced.
`w.Flush()` pushes what's buffered without ending the stream — use it only when
a reader is actually waiting, since each flush costs ratio.

For HTTP specifically, don't build this yourself: `github.com/go-again/az/azhttp`
is exactly this, plus `Accept-Encoding` negotiation — see the **az-http** skill.

## Why this is the right model for concurrent chunk workloads

Parallelism comes from running **many chunks across goroutines**, each with its
own pooled encoder — not from splitting a single chunk internally. So a
per-goroutine encoder is exactly what you want, and it avoids CPU
oversubscription you'd get if each call also spun up its own worker goroutines.

## Performance note (lz4 levels 1–2 only)

The pooled `Encoder` runs the lz4 path single-threaded (a deliberate, correct
choice — the concurrent lz4 writer can't be safely reused). Effects:

- Inputs ≤ 4 MB (one lz4 block): **no difference** vs `Compress`.
- Inputs > 4 MB at L1/L2: `EncodeAll` loses intra-call block parallelism, so a
  single large call is modestly slower in wall-clock than `Compress` — but uses
  far less memory and far fewer allocs, and under cross-goroutine concurrency the
  aggregate throughput is what matters.
- **zstd levels (3–5):** purely faster *and* lighter than `Compress` — this is
  where the big GC/alloc win lands. Typical: ~20× less B/op and ~3× fewer
  allocs/op, plus lower latency.

If you compress chunks that are both large (>4 MB) *and* lz4-level *and* latency
of a single call dominates, prefer `az.Compress` for those; otherwise the pool is
the better default.

## Checklist

- [ ] Many independent buffers at high frequency/concurrency? → use this.
- [ ] Streaming output per request instead? → pooled `az.Writer` with
      `az.WithConcurrency(1)`, and always `Close()` before returning it.
- [ ] One `sync.Pool` for `Encoder`, one for `Decoder`; Get/Put per call.
- [ ] Never share an `Encoder`/`Decoder` across goroutines concurrently.
- [ ] Reuse a `dst` scratch slice; copy the result out before reusing it.
- [ ] Rely on byte-identity: no migration needed when switching existing code
      from `Compress`/`Decompress` to `EncodeAll`/`DecodeAll`.
