# az — compression library + CLI (lz4 levels 1–2, zstd levels 3–5).
#
#   just              pre-commit gate: build + test + lint (what CI enforces)
#   just --list       every recipe, by group
#   just lint-fix     apply every fix the linters can make on their own
#
# The lint tools are pinned below and run via `go run`, so there is nothing to
# install and everyone — including CI — runs the same versions. The first run
# fetches and builds them into the module cache; after that they are instant.
# Bump a version here and in .github/workflows/ci.yml together.
#
# The pinned modernize (gopls) needs Go >= 1.26 to build, one line ahead of the
# go.mod minimum. The default GOTOOLCHAIN=auto fetches it for you; if you have
# set GOTOOLCHAIN=local on an older Go, `just modernize` is the one recipe that
# will complain.

set quiet

az_bin := "./az"

# Packages az authors, and the only ones the strict linters see. Everything
# under internal/ is a vendored upstream fork (pierrec/lz4,
# klauspost/compress): built and tested, never linted, so it doesn't drift from
# upstream over our lint config. Mirrors the exclusion in .golangci.yml.
gopkgs := ". ./azhttp/... ./cmd/... ./examples/..."

# Files gofmt covers: az's own code, skipping vendored and dot-dirs.
own_go_files := "$(find . -name '*.go' -not -path './.*/*' -not -path './internal/*')"

golangci := "go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2"
staticcheck := "go run honnef.co/go/tools/cmd/staticcheck@v0.7.0"
modernize := "go run golang.org/x/tools/gopls/internal/analysis/modernize/cmd/modernize@v0.23.0"

# Pre-commit gate. Same steps CI runs, in the order that fails fastest.
default: build test lint

# List every recipe.
help:
    just --list

# ── Build ─────────────────────────────────────────────────────────────────────

[group('build')]
[doc('Build the az CLI to ./az')]
build:
    go build -trimpath -ldflags '-s -w' -o {{ az_bin }} ./cmd/az

# Build, then run the CLI. Usage: just run -3 -c README.md
[group('build')]
run *ARGS: build
    {{ az_bin }} {{ ARGS }}

# Install the az CLI to $GOPATH/bin.
[group('build')]
install:
    go install ./cmd/az

# Compile every package, including the vendored codecs.
[group('build')]
build-all:
    go build ./...

# Cross-compile for every target CI builds, to catch platform-specific breaks in
# the vendored asm fallbacks. Compile-only; pure Go, so CGO is off.
[group('build')]
[doc('Cross-compile every target CI builds (compile-only)')]
cross:
    #!/usr/bin/env bash
    set -euo pipefail
    targets=(
        darwin/amd64 darwin/arm64
        freebsd/amd64 freebsd/arm64
        linux/386 linux/amd64 linux/arm linux/arm64
        linux/loong64 linux/ppc64le linux/riscv64 linux/s390x
        windows/386 windows/amd64 windows/arm64
    )
    for t in "${targets[@]}"; do
        printf '%-18s' "$t"
        GOOS="${t%/*}" GOARCH="${t#*/}" CGO_ENABLED=0 go build ./... && echo ok
    done

# Run the azhttp demo: probe the middleware and print what each client gets.
# Pass -serve to keep it running (see examples/http/main.go).
[group('build')]
[doc('Run the azhttp demo (pass -serve to keep it running)')]
example-http *ARGS:
    go run ./examples/http {{ ARGS }}

# ── Test ──────────────────────────────────────────────────────────────────────

# Full test suite, all packages.
[group('test')]
test:
    go test -count=1 -timeout 5m ./...

# Verbose run, for diagnosing a flake.
[group('test')]
test-v:
    go test -count=1 -timeout 5m -v ./...

# Run tests matching a pattern. Usage: just test-one TestEncodeAll
[group('test')]
test-one PATTERN:
    go test -count=1 -timeout 2m -run '{{ PATTERN }}' -v ./...

# Race detector. Run this for anything touching concurrency or pooling.
[group('test')]
test-race:
    go test -race -count=1 -timeout 10m ./...

# Coverage for az's own packages: summary to stdout, HTML to /tmp/az-cover.html.
[group('test')]
coverage:
    go test -count=1 -coverprofile=/tmp/az-cover.out {{ gopkgs }}
    go tool cover -func=/tmp/az-cover.out | tail -n 15
    go tool cover -html=/tmp/az-cover.out -o /tmp/az-cover.html
    echo "open /tmp/az-cover.html"

# Short fuzz of the round-trip (30 s).
[group('test')]
fuzz:
    go test -run '^$' -fuzz=FuzzRoundtrip -fuzztime=30s .

# Long fuzz of the round-trip (10 min).
[group('test')]
fuzz-long:
    go test -run '^$' -fuzz=FuzzRoundtrip -fuzztime=10m .

# Everything CI runs, in one command.
[group('test')]
ci: build test test-race lint cross

# ── Lint ──────────────────────────────────────────────────────────────────────
#
# Cheapest first: fmt-check leads because it's the most common CI failure from a
# local-only push.

# Full lint pass (matches CI).
[group('lint')]
lint: fmt-check vet staticcheck golangci modernize tidy-check deps-check

# Apply every fix the tools can make on their own, then re-format and tidy.
[group('lint')]
[doc('Auto-fix: golangci --fix, modernize -fix, gofmt, go mod tidy')]
lint-fix:
    {{ golangci }} run --fix --timeout 5m || true
    {{ modernize }} -fix {{ gopkgs }} || true
    gofmt -w {{ own_go_files }}
    go mod tidy

# gofmt diff, read-only. Fails if anything would be reformatted.
[group('lint')]
[doc('gofmt diff, read-only; fails if anything would be reformatted')]
fmt-check:
    out=$(gofmt -d {{ own_go_files }}); \
    if [ -n "$out" ]; then echo "$out"; echo "run: just fmt"; exit 1; fi

# Apply gofmt in place.
[group('lint')]
fmt:
    gofmt -w {{ own_go_files }}

# go vet over the whole module. It's clean on the vendored trees, so they get
# vet coverage for free — unlike the stricter linters below.
[group('lint')]
[doc('go vet over the whole module, vendored code included')]
vet:
    go vet ./...

# staticcheck over az's own packages (pinned above, so nothing to install).
[group('lint')]
[doc('staticcheck over az own packages')]
staticcheck:
    {{ staticcheck }} {{ gopkgs }}

# golangci-lint v2 — errcheck/govet/ineffassign/staticcheck/unused, configured
# in .golangci.yml (pinned above, so nothing to install).
[group('lint')]
[doc('golangci-lint v2 (errcheck/govet/ineffassign/staticcheck/unused)')]
golangci:
    {{ golangci }} run --timeout 5m

# Report modern-Go rewrites: min/max, range-over-int, slices/maps,
# strings.SplitSeq, WaitGroup.Go, …
[group('lint')]
[doc('Report modern-Go rewrites (apply them with: just lint-fix)')]
modernize:
    {{ modernize }} {{ gopkgs }}

# go mod tidy.
[group('lint')]
tidy:
    go mod tidy

# Fail if go.mod/go.sum are not tidy, without leaving them modified.
[group('lint')]
[doc('Fail if go.mod is not tidy (leaves it unmodified)')]
tidy-check:
    #!/usr/bin/env bash
    set -euo pipefail
    cp go.mod go.mod.bak
    trap 'mv go.mod.bak go.mod' EXIT
    go mod tidy
    if ! cmp -s go.mod go.mod.bak; then
        echo "go.mod not tidy — run: just tidy"; exit 1
    fi

# Fail if az gains an external dependency. "Dependency-free" is a promise the
# README makes and the reason the codecs are vendored under internal/, so a
# stray `go get` must not slip past review.
[group('lint')]
[doc('Fail if az gains an external dependency')]
deps-check:
    #!/usr/bin/env bash
    set -euo pipefail
    extra=$(go list -m all | grep -v '^github.com/go-again/az$' || true)
    if [ -n "$extra" ]; then
        echo "az must stay dependency-free, but the module graph has:"
        echo "$extra"
        exit 1
    fi
    echo "no external dependencies"

# Warm the module cache for the pinned lint tools (only the first run is slow).
[group('lint')]
[doc('Warm the module cache for the pinned lint tools')]
tools:
    {{ golangci }} version
    {{ staticcheck }} --version
    {{ modernize }} -V=full || true

# ── Docs ──────────────────────────────────────────────────────────────────────

# Print the full godoc for a package. Usage: just doc ./azhttp
[group('docs')]
doc PKG='.':
    go doc -all {{ PKG }}

# List the exported API of every public package — a quick diff target when
# checking that a change is additive.
[group('docs')]
[doc('List the exported API of every public package')]
api:
    #!/usr/bin/env bash
    set -euo pipefail
    for pkg in . ./azhttp; do
        echo "── $pkg ──"
        go doc -short "$pkg"
        echo
    done

# ── Benchmark ─────────────────────────────────────────────────────────────────

# Go benchmarks: all levels × corpus, plus the azhttp handler.
[group('bench')]
bench:
    go test -bench=. -benchmem -benchtime=3s ./...

# Benchmarks, saved to bench.txt.
[group('bench')]
bench-save:
    go test -bench=. -benchmem -benchtime=3s ./... | tee bench.txt

# Size/speed table for az vs lz4/gzip/zstd/xz on a file or directory. Extra args
# go to scripts/bench.sh (-n iterations, --full, --az-only, --csv FILE).
#
# Usage: just compare /usr/share/man
#        just compare -n 3 --csv out.csv ./mydir
[group('bench')]
[doc('Size/speed table: az vs lz4/gzip/zstd/xz on a file or dir')]
compare *ARGS: build
    ./scripts/bench.sh {{ ARGS }}

# ── Clean ─────────────────────────────────────────────────────────────────────

# Remove build and benchmark artifacts.
[group('clean')]
clean:
    rm -f {{ az_bin }} bench.txt
    rm -rf /tmp/az-bench /tmp/az-cover.out /tmp/az-cover.html
