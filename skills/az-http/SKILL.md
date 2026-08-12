---
name: az-http
description: Compress HTTP responses (and decode compressed request bodies) with github.com/go-again/az/azhttp — server middleware that negotiates Accept-Encoding and emits zstd (or lz4 for peers that ask for it), a client RoundTripper that requests and decodes those codings, MinSize/content-type/ETag/Vary handling, and streaming with Flush. Use when adding response compression to a Go HTTP server or handler chain, accepting compressed uploads, or consuming an az-compressed API from a Go client. For compressing buffers or files rather than HTTP traffic, use the az-integration skill.
---

# HTTP compression with `azhttp`

`github.com/go-again/az/azhttp` is middleware around an `http.Handler`: it picks
a content coding from the request's `Accept-Encoding`, compresses the response
body, and sets `Content-Encoding`. It is the `gzhttp` shape, with az's codecs.

```go
import "github.com/go-again/az/azhttp"

// Server: defaults (MinSize 1 KiB, zstd level 3).
http.ListenAndServe(":8080", azhttp.Handler(mux))

// Client: request and decode az codings transparently.
client := &http.Client{Transport: azhttp.Transport(http.DefaultTransport)}
```

## Which coding goes on the wire

| az levels | Frame | `Content-Encoding` | Sent to |
|-----------|-------|--------------------|---------|
| 3–5 | Zstandard | `zstd` | Anyone who accepts it — standard coding (RFC 8878); current browsers, curl, CDNs |
| 1–2 | LZ4 | `lz4` | **Only** a client that names `lz4` in `Accept-Encoding` — the token is not standardised |

`lz4` is never inferred from `Accept-Encoding: *`, so enabling it cannot break a
browser. It is there for service-to-service traffic where CPU matters more than
bytes. When a client accepts both at equal quality, zstd wins unless you set
`PreferLZ4(true)`. An explicit q-value from the client always wins.

**There is no gzip here.** A client that only accepts `gzip` gets an
uncompressed response. If you need gzip for legacy clients, chain
`klauspost/compress/gzhttp` or terminate compression at your proxy.

## Configured middleware

Build the wrapper **once** at start-up (it holds the compressor pools) and apply
it to as many handlers as you like; it is safe for concurrent use.

```go
compress, err := azhttp.NewWrapper(
    azhttp.MinSize(2048),                 // default 1024 (azhttp.DefaultMinSize)
    azhttp.ZstdLevel(az.Level4),          // default Level3; must be 3–5
    azhttp.LZ4Level(az.Level1),           // default Level1; must be 1–2
    azhttp.EnableLZ4(false),              // public site: standard codings only
    azhttp.PreferLZ4(true),               // internal service: cheap CPU wins ties
    azhttp.Checksum(false),               // skip the frame checksum (TLS/TCP already cover it)
    azhttp.ContentTypes([]string{"application/json", "text/html"}), // allow-list
    azhttp.ExceptContentTypes([]string{"text/event-stream"}),       // deny-list
    azhttp.ContentTypeFilter(azhttp.CompressAllContentTypeFilter),  // or your own predicate
    azhttp.SuffixETag("-az"),             // `"v1"` → `"v1-az-zstd"` on compressed responses
    azhttp.DropETag(),                    // or drop it entirely
    azhttp.KeepAcceptRanges(),            // default: removed on compressed responses
    azhttp.SetContentType(false),         // default: sniff Content-Type if unset
)
if err != nil {
    log.Fatal(err) // invalid option — fails at start-up, not per request
}
http.Handle("/", compress(mux))
```

## What it does without being asked

- Adds `Vary: Accept-Encoding` to **every** response, compressed or not, so a
  cache can't serve a zstd body to a client that didn't ask for one.
- Skips bodies under `MinSize`, and bodies whose `Content-Length` says they will
  be. Small responses cost more in CPU than they save in packets.
- Skips already-compressed content types (`video/*`, `audio/*`, `image/jp*`,
  `image/png|gif|webp|avif`, and types containing `zip`/`zstd`/`compress`/…).
  Sniffs `Content-Type` from the first bytes when the handler didn't set one.
- Skips responses that already have a `Content-Encoding`, ranged responses
  (`Content-Range`), ranged *requests* (`Range`), `HEAD`, and bodiless statuses
  (1xx, 204, 304).
- Drops `Content-Length` and `Accept-Ranges` from a compressed response — both
  describe the identity body.

## Opting one response out

Set the `azhttp.HeaderNoCompression` header (`X-No-Compression`) to any value.
It's for bodies you know are already compressed or must go out byte-for-byte.
The middleware strips it before the response leaves.

```go
func serveBlob(w http.ResponseWriter, r *http.Request) {
    w.Header().Set(azhttp.HeaderNoCompression, "1")
    w.Write(alreadyCompressedBlob)
}
```

## Streaming responses

`Flush` works: flushing forces the compress-or-not decision even below
`MinSize`, so a client waiting on a flush isn't blocked waiting for the buffer
to fill. Use `http.ResponseController` (or the `http.Flusher` assertion) as
usual. Frequent flushes cost ratio — each one ends a block group.

```go
rc := http.NewResponseController(w)
for ev := range events {
    fmt.Fprintf(w, "data: %s\n\n", ev)
    rc.Flush()
}
```

`Hijack` (WebSocket upgrades) and `Unwrap` (for `ResponseController` deadlines)
are forwarded to the underlying writer. `CloseNotifier` is **not** implemented —
use `r.Context().Done()`, which is the modern replacement anyway.

## Compressed request bodies

```go
// maxBody caps the DECOMPRESSED size; reads past it fail with az.ErrTooLarge.
// Pass 0 only for trusted clients — an unbounded decode is a bomb target.
http.Handle("/upload", azhttp.DecompressRequests(8<<20)(uploadHandler))
```

Bodies with `Content-Encoding: zstd` or `lz4` are decoded transparently; the
handler reads plain bytes and no longer sees the header. Any other coding
(`gzip`, …) is passed through untouched for something else to handle. Handle the
error from reading `r.Body`:

```go
data, err := io.ReadAll(r.Body)
if errors.Is(err, az.ErrTooLarge) {
    http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
    return
}
```

## Client side

```go
client := &http.Client{Transport: azhttp.Transport(http.DefaultTransport,
    azhttp.TransportLZ4(true),        // also accept lz4 (off by default)
    azhttp.TransportPreferLZ4(true),  // rank lz4 above zstd; implies the above
    azhttp.TransportZstd(false),      // stop accepting zstd
    azhttp.TransportAlwaysDecompress(true), // decode even codings we didn't request
)}
```

- A request that already has an `Accept-Encoding` header is left alone (and its
  response is not decoded unless `TransportAlwaysDecompress` is set).
- `HEAD` and ranged requests don't ask for compression, matching net/http.
- A decoded response has `Content-Encoding`/`Content-Length` stripped,
  `ContentLength == -1` and `Uncompressed == true` — same contract net/http uses
  for the gzip it handles itself. Don't read `Content-Length` for a body size.

## Checklist

- [ ] Build the wrapper once at start-up; don't call `NewWrapper` per request.
- [ ] Public traffic: leave zstd on; decide whether `lz4` is worth enabling
      (harmless, but only az-aware peers will ever use it).
- [ ] Serving pre-compressed bytes? Set `X-No-Compression`.
- [ ] Accepting uploads? `DecompressRequests(max)` with a real limit, and handle
      `az.ErrTooLarge`.
- [ ] Caching in front? `Vary: Accept-Encoding` is already set; add
      `SuffixETag`/`DropETag` if a cache keys on strong ETags.
- [ ] Streaming? Flush deliberately — every flush costs ratio.
- [ ] Don't expect gzip: az speaks zstd and lz4 only.
