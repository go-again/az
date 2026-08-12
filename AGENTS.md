# AGENTS.md — developing `az`

Onboarding for agents working **on** the `az` codebase. If you are integrating
`az` as a dependency in another project, you don't need this file — use the
skills under [`skills/`](skills/) instead.

## What az is

`az` is a small, dependency-free Go compression library + CLI. It is a **thin
facade** over two vendored codecs:

- **Levels 1–2 → LZ4** (`internal/lz4`, a vendored fork of `pierrec/lz4`)
- **Levels 3–5 → Zstandard** (`internal/zstd`, a vendored fork of `klauspost/compress/zstd`)

There is no custom compression algorithm in the current tree — earlier versions
had a hand-written LZ77+entropy engine, but commit `377a7a5` replaced it with
LZ4/Zstd. Pantry/memory notes describing block types `0x01/0x02/0x04`, FSE block
layouts, etc. are **historical** and no longer describe the shipping code.

The public package (`github.com/go-again/az`) auto-detects the format on decode
from the frame magic, so `.az` files are plain lz4 or zstd streams that standard
`lz4`/`zstd` CLIs can read.

## Repo layout

| Path | What |
|------|------|
| `az.go` | Public facade: `Compress`/`Decompress`, `Writer`/`Reader`. |
| `encoder.go` | Public pooled codecs: `Encoder`/`Decoder` (`EncodeAll`/`DecodeAll`). |
| `options.go` | `Level` constants, `Options`, `With*` functional options. |
| `errors.go` | `ErrCorrupted`, `ErrChecksumFail`, `ErrLevel`. |
| `*_test.go` | Round-trip, format-identity, fuzz, and benchmarks. |
| `azhttp/` | HTTP middleware: response compression, request decompression, client transport. |
| `cmd/az/` | The CLI (`-1`..`-5`, `-d`, `-c`, `-k`, `-t`, `-o`, `--no-checksum`). |
| `examples/http/` | Runnable azhttp demo (`just example-http`). |
| `internal/lz4`, `internal/zstd` | Vendored codecs az actually compiles against. |
| `internal/fse`, `internal/huff0`, `internal/le`, `internal/cpuinfo` | Dependencies of the vendored zstd. |
| `internal/fuzz`, `internal/testdata` | Fuzz corpus / fixtures. |
| `.compress/`, `.lz4/` | **Reference checkouts** of the upstream projects. Dot-prefixed, so Go tooling ignores them. Use for diffing against upstream / cherry-picking fixes; they are NOT built. |
| `scripts/bench.sh`, `justfile` | Benchmark + comparison tooling. |
| `skills/` | Consumer-facing agent skills (`az-integration`, `az-pooling`, `az-http`) — keep in sync with the public API. |

## Build / test / bench

Use `just` (recipes in `justfile`):

```sh
just                # default gate: build + test + lint
just --list         # every recipe, grouped (build/test/lint/docs/bench/clean)
just build          # build ./cmd/az → ./az
just run -3 -c F    # build, then run the CLI
just test           # go test -count=1 -timeout 5m ./...
just test-race      # race detector
just test-one PAT   # go test -run PAT -v ./...
just coverage       # coverage summary + /tmp/az-cover.html
just lint           # fmt-check + vet + staticcheck + golangci + modernize + tidy-check + deps-check
just lint-fix       # apply what the linters can fix, then gofmt + go mod tidy
just fmt            # gofmt -w (apply)
just tools          # warm the module cache for the pinned lint tools
just fuzz           # 30s fuzz of FuzzRoundtrip
just bench          # go test -bench=. -benchmem -benchtime=3s ./...
just compare <dir>  # size/speed table vs lz4/zstd/gzip/xz — wraps scripts/bench.sh
just cross          # compile-only build of every GOOS/GOARCH CI covers
just ci             # everything CI runs: build + test + race + lint + cross
just doc ./azhttp   # go doc -all for a package
just api            # exported API of every public package (diff target for "is this additive?")
just example-http   # run the azhttp demo (add -serve to keep it up)
```

**Lint tools are pinned in the justfile** (`golangci`, `staticcheck`,
`modernize` variables) and invoked with `go run`, so there is nothing to install
and a green `just lint` locally means a green lint job in CI. The same versions
are hard-coded in `.github/workflows/ci.yml` — **bump both together**.

Two checks exist to protect stated invariants, and both are in `lint` and CI:
`tidy-check` (go.mod stays tidy, without leaving it modified) and `deps-check`
(az stays dependency-free — the whole reason the codecs are vendored).

**Lint scope:** `go vet` runs over the whole module (`./...`) — it's clean on the
vendored `internal/` trees, so they get vet coverage for free. The stricter
linters (`staticcheck`/`golangci`/`modernize`) run only over az's own packages
(the `gopkgs` variable in the justfile: `.`, `./azhttp/...`, `./cmd/...`,
`./examples/...` — keep it in sync when adding a package); the vendored
`internal/` forks would otherwise flag ~19 upstream
idioms (`SA4004`/`U1000`/…) that aren't bugs and shouldn't drift from upstream
(see `.golangci.yml`). `gofmt` covers everything except `internal/` and dot-dirs.
CI (`.github/workflows/ci.yml`, modeled on gosqlite.org) runs test
(ubuntu/macos/windows × Go 1.25/1.26), race, the same lint gate, and a
cross-build matrix. The lint job runs on Go 1.26 specifically: the pinned
gopls/modernize needs it to build, and `setup-go` sets `GOTOOLCHAIN=local`, so
the job can't fetch a newer toolchain the way a developer's default
`GOTOOLCHAIN=auto` does. az itself still targets 1.25 (`go.mod`).

**`.gitattributes` sets `* -text`, and that is load-bearing.** Git for Windows
defaults to `core.autocrlf=true`; the fixtures in `internal/testdata` are
compressed and compared byte for byte, and `internal/fse`'s `ExampleCompress`
asserts an exact size. A CRLF-translated `e.txt` is 100004 bytes instead of
100003 and the example fails on Windows only, looking like a codec bug. Don't
"clean up" that file.

Plain `go build ./...` / `go test ./...` also work. Note: first compile of the
vendored zstd/lz4/fse packages is slow (tens of seconds) — be patient and
**always pass `-timeout`** when running tests in the background so a hang can't
sit at the default 10-minute limit (see the deadlock gotcha below).

## The load-bearing invariant: format identity

Three code paths must produce **byte-identical** frames for the same input+level:

1. `Compress(src, level)`
2. `Encoder.EncodeAll(nil, src, level)`
3. (decode side) `Decompress` and `Decoder.DecodeAll` must agree.

Consumers (e.g. `gosqlite/blobstore`) store `Compress`-format data at rest and
later mix it with `EncodeAll`-format data in the same store. **Any divergence is
a silent at-rest format break with no migration path.** This is enforced by
`TestEncodeAllFormatIdentity` / `TestDecodeAllFormatIdentity` in
`encoder_test.go` — never weaken those tests; if you change encoder options, the
change must be made in *both* `Compress` and `EncodeAll` (and `Writer` if it
should match) and the identity tests must stay green.

## Gotchas (read before touching `encoder.go` or the codec wiring)

- **lz4 concurrent writer can't be `Reset`-reused after `Close`.** The
  concurrent lz4 `Writer` starts a block-manager goroutine that exits after
  `Close`; the next `Reset` → `Blocks.close` then deadlocks on a channel send
  (`internal/lz4/stream/block.go`). That's why the pooled `Encoder` uses
  `ConcurrencyOption(1)` for lz4. Concurrency does **not** change output bytes,
  so identity with `Compress` (which uses `GOMAXPROCS`) holds; the only effect is
  no intra-call block parallelism on lz4 inputs >4 MB (single chunk per encoder
  is the intended usage anyway). Do not "restore concurrency" on the pooled lz4
  writer without also fixing the upstream reset deadlock.
- **zstd: do not use klauspost `EncodeAll` for the pooled encoder.** It would
  make big frames single-segment (window = whole payload) and is serial. Reuse a
  per-level `*zstd.Encoder` via `ResetContentSize`+`Write`+`Close` instead —
  `Compress` does exactly the same, which is what keeps the two byte-identical.
- **`ResetContentSize`, not `Reset`, in the one-shot zstd paths.** Both
  `Compress` and `Encoder.EncodeAll` declare `len(src)` so every frame carries
  `Frame_Content_Size`; consumers single-shot decode off the header (get size →
  alloc → decompress). Plain `Reset` writes the header before the total is known,
  which silently drops the size for inputs larger than one block (~128 KB) and
  for empty input. Three small patches in the vendored zstd support this
  (all marked "az fork"): `frameHeader.ContentSizeKnown` widens `FCS_Field_Size`
  to 4 bytes when a windowed frame must carry a size < 256 (the 2-byte encoding
  is biased by 256 and can't express it); `encoderState.frameContentSizeKnown`
  distinguishes a declared size of 0 from "undeclared"; `encodeAll` marks the
  size known unconditionally. Pinned by `TestOneShotAlwaysStoresContentSize`.
- **`WithContentSize` is a no-op.** One-shot frames always store the size and the
  streaming `Writer` never can. Kept only so the option doesn't vanish from the
  public surface.
- **`Writer`/`Reader` are intentionally NOT byte-identical to `Compress`.**
  `Writer` enables block checksums and omits `SizeOption`. Use
  `Compress`/`Encoder` when at-rest identity matters, `Writer`/`Reader` for
  general streaming.
- **`Reader.Reset` must *release* the sub-reader, not re-point it.** Handing the
  old zstd decoder the shared `bufio.Reader` (`Reset(r.br)`) starts a stream
  goroutine on the buffer the next stream is about to read: a data race, a
  goroutine leak, and stolen bytes ("lz4: invalid block size" two resets later).
  `Close()` cancels it and waits; the next `Read` re-detects the format and
  builds a fresh sub-reader. Pinned by `TestReaderResetReleasesSubReader`, and
  `azhttp`'s pooled response bodies depend on it.
- **`DecodeAllLimit` must stream, never one-shot.** The bound is what makes it
  safe on untrusted frames, so it drives the per-format reader and caps `dst`
  growth at `max+1` (`readLimitAppend`). Two traps if you touch it: (1) the zstd
  `Decoder.Reset` *synchronously decodes the whole frame* when handed a `byter`
  (`Bytes()`+`Len()`) under ~128 KiB — `*bytes.Reader` lacks `Bytes()` so it
  streams, but don't "helpfully" switch to `*bytes.Buffer`/raw bytes or you
  re-arm the bomb; (2) on early stop (`ErrTooLarge`) the zstd stream goroutine is
  still running — `Reset(nil)` cancels it. The Decoder's lz4 reader is
  `ConcurrencyOption(1)` precisely so the lz4 path has no goroutine to leak on
  early stop. There's a goroutine-leak regression test for this.

## Gotchas (`azhttp`)

- **Codec concurrency is pinned to 1, and that is load-bearing.** `azhttp` pools
  one `az.Writer` per response and reuses it via `Reset` after `Close`. Only a
  single-threaded lz4 writer survives that (see the lz4 deadlock above), and a
  server is already parallel across requests, so GOMAXPROCS workers per response
  would just multiply goroutines and memory. That's what `az.WithConcurrency(1)`
  is for; don't drop it from `newWriterPool`.
- **`lz4` is not a registered content coding.** It is only ever selected when a
  client names `lz4` explicitly — never from `Accept-Encoding: *`, which is why
  `selectEncoding` treats the wildcard as zstd-only. Keep it that way: inferring
  lz4 from a wildcard would send LZ4 frames to browsers that asked for anything.
  `zstd` is the standard coding (RFC 8878) and needs no such care.
- **The decision is deferred, so header mutation has a deadline.** The wrapper
  buffers up to `max(MinSize, 512)` bytes before choosing, and only then writes
  the status line. Anything that must be adjusted on a compressed response
  (`Content-Length`, `Accept-Ranges`, `ETag`, `Content-Encoding`) has to happen in
  `startCompress`/`startPlain`, before `WriteHeader` reaches the real writer.
- **`Flush` forces the decision.** A handler that flushes below `MinSize` gets
  compression started anyway — a client waiting on a flush can't also wait for
  the buffer to fill. Test coverage: `TestFlushStartsCompressionEarly`.
- **Reference implementation:** klauspost's `gzhttp` (same author as the vendored
  zstd). It is not vendored here, but `~/go/pkg/mod/github.com/klauspost/compress@*/gzhttp`
  is worth diffing against when changing negotiation or the buffering state
  machine — azhttp deliberately mirrors its structure, minus the BREACH jitter,
  the `CloseNotifier` shim, and the pluggable writer factories.
## Public API contract (don't break)

`Compress`, `Decompress`, `Writer`/`NewWriter` (incl. `Flush`), `Reader`/`NewReader`,
`Encoder`/`NewEncoder`/`EncodeAll`, `Decoder`/`NewDecoder`/`DecodeAll`/`DecodeAllLimit`,
the `Level` constants, `With*` options, and the sentinel errors
(`ErrCorrupted`, `ErrChecksumFail`, `ErrLevel`, `ErrTooLarge`) are the public
surface. `azhttp` adds `Handler`, `NewWrapper`, its `Option`s, `Transport` and
its `TransportOption`s, and `DecompressRequests`. Keep additions **additive**;
the levels' on-disk format is a compatibility boundary, and the content-coding
tokens (`zstd`, `lz4`) are a wire boundary too.

## Before you finish

- `just lint` and `just test` (and `just test-race` for concurrency-touching
  changes) green — this is the same gate CI enforces. `just ci` runs the lot,
  cross-build included.
- If you changed level configs, codec options, or the wire path, re-run the
  format-identity tests and update the README levels/wire-format sections.
- If you changed the **public API**, update the consumer skills under `skills/`
  (`az-integration`, `az-pooling`, `az-http` — including the `description:`
  frontmatter, which is what an agent matches on) and the README. `just api`
  prints the exported surface if you need to see what moved.
- Store a pantry note and update `~/.claude` memory for non-obvious decisions
  (the project uses both — see the pantry skill).
