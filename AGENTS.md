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
| `cmd/az/` | The CLI (`-1`..`-5`, `-d`, `-c`, `-k`, `-t`, `-o`, `--no-checksum`). |
| `internal/lz4`, `internal/zstd` | Vendored codecs az actually compiles against. |
| `internal/fse`, `internal/huff0`, `internal/le`, `internal/cpuinfo` | Dependencies of the vendored zstd. |
| `internal/fuzz`, `internal/testdata` | Fuzz corpus / fixtures. |
| `.compress/`, `.lz4/` | **Reference checkouts** of the upstream projects. Dot-prefixed, so Go tooling ignores them. Use for diffing against upstream / cherry-picking fixes; they are NOT built. |
| `scripts/bench.sh`, `justfile` | Benchmark + comparison tooling. |

## Build / test / bench

Use `just` (recipes in `justfile`):

```sh
just                # default gate: build + test + lint
just build          # build ./cmd/az → ./az
just test           # go test -count=1 -timeout 5m ./...
just test-race      # race detector
just lint           # fmt-check + vet + staticcheck + golangci + modernize
just fmt            # gofmt -w (apply)
just fuzz           # 30s fuzz of FuzzRoundtrip
just bench          # go test -bench=. -benchmem -benchtime=3s ./...
just compare <dir>  # size/speed table vs lz4/zstd/gzip/xz (needs those CLIs)
```

**Lint scope:** `go vet` runs over the whole module (`./...`) — it's clean on the
vendored `internal/` trees, so they get vet coverage for free. The stricter
linters (`staticcheck`/`golangci`/`modernize`) run only over az's own packages
(`. ./cmd/...`); the vendored `internal/` forks would otherwise flag ~19 upstream
idioms (`SA4004`/`U1000`/…) that aren't bugs and shouldn't drift from upstream
(see `.golangci.yml`). `gofmt` covers everything except `internal/` and dot-dirs. `staticcheck` and `golangci-lint` need to be installed
(`go install honnef.co/go/tools/cmd/staticcheck@latest`; `brew install
golangci-lint` — the config targets golangci-lint **v2**). CI (`.github/workflows/ci.yml`,
modeled on gosqlite.org) runs test (ubuntu/macos/windows × Go 1.25/1.26), race,
the same lint gate, and a cross-build matrix.

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
- **zstd: do not use klauspost `EncodeAll` for the pooled encoder.** Its
  one-shot path writes the frame content size into the header; streaming
  `Compress` (unknown size) does not — so `EncodeAll` output ≠ `Compress` output.
  Reuse a per-level `*zstd.Encoder` via `Reset`+`Write`+`Close` instead.
- **`Writer`/`Reader` are intentionally NOT byte-identical to `Compress`.**
  `Writer` enables block checksums and omits `SizeOption`; `Reader.Reset`
  discards its sub-decoder. Use `Compress`/`Encoder` when at-rest identity
  matters, `Writer`/`Reader` for general streaming.
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

## Public API contract (don't break)

`Compress`, `Decompress`, `Writer`/`NewWriter`, `Reader`/`NewReader`,
`Encoder`/`NewEncoder`/`EncodeAll`, `Decoder`/`NewDecoder`/`DecodeAll`/`DecodeAllLimit`,
the `Level` constants, `With*` options, and the sentinel errors
(`ErrCorrupted`, `ErrChecksumFail`, `ErrLevel`, `ErrTooLarge`) are the public
surface. Keep additions **additive**; the levels' on-disk format is a
compatibility boundary.

## Before you finish

- `just lint` and `just test` (and `just test-race` for concurrency-touching
  changes) green — this is the same gate CI enforces.
- If you changed level configs, codec options, or the wire path, re-run the
  format-identity tests and update the README levels/wire-format sections.
- Store a pantry note and update `~/.claude` memory for non-obvious decisions
  (the project uses both — see the pantry skill).
