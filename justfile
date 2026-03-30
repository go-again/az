# az justfile — build, test, and benchmark recipes
# requires: just, go, zstd, lz4, gzip, xz

az_bin := "./az"
tmp_dir := "/tmp/az-bench"

# ── Development ───────────────────────────────────────────────────────────────

# Build the az CLI binary
build:
    go build -trimpath -ldflags '-s -w' -o {{ az_bin }} ./cmd/az

# Run all tests
test:
    go test ./...

# Run tests with verbose output
test-v:
    go test -v ./...

# Run tests with race detector
test-race:
    go test -race ./...

# Run short fuzz test (30 s)
fuzz:
    go test -fuzz=FuzzRoundtrip -fuzztime=30s

# Run long fuzz test (10 min)
fuzz-long:
    go test -fuzz=FuzzRoundtrip -fuzztime=10m

# Run Go benchmarks (all levels × corpus)
bench:
    go test -bench=. -benchmem -benchtime=3s ./...

# Run benchmarks and save results to bench.txt
bench-save:
    go test -bench=. -benchmem -benchtime=3s ./... | tee bench.txt

# Compare az against lz4/gzip/zstd/xz on a file or directory
# Usage: just bench-compare <target>
#   just bench-compare ./mydir
#   just bench-compare /usr/share/man
bench-compare target: build
    ./scripts/bench.sh -n 5 -w 2 "{{ target }}"

# ── Installation ──────────────────────────────────────────────────────────────

# Install az CLI to $GOPATH/bin
install:
    go install ./cmd/az

# ── Compression comparison ───────────────────────────────────────────────────
#
# These recipes compress a target using different algorithms so you can compare
# size and speed side-by-side.
#
# Usage examples:
#   just compress-all myfile.bin
#   just compress-all ./mydir
#   just compare myfile.bin
#   just compare ./mydir
# Compress a file or directory with all algorithms and print a size comparison.

# Usage: just compare <target>
compare target: build
    #!/usr/bin/env bash
    set -euo pipefail
    TARGET="{{ target }}"
    OUT="{{ tmp_dir }}"
    mkdir -p "$OUT"

    if [ -d "$TARGET" ]; then
        NAME=$(basename "$TARGET")
        SRC="$OUT/${NAME}.tar"
        echo "→ Creating tar of directory $TARGET ..."
        tar -cf "$SRC" -C "$(dirname "$TARGET")" "$NAME"
    else
        SRC="$TARGET"
    fi

    ORIG=$(wc -c < "$SRC" | tr -d ' ')
    AZ="{{ az_bin }}"
    echo ""
    echo "Source: $SRC  ($ORIG bytes)"
    echo ""
    printf "%-20s %-12s %-12s %-12s %-10s\n" "Algorithm" "Output (B)" "Ratio" "Compress" "Decompress"
    printf "%-20s %-12s %-12s %-12s %-10s\n" "---------" "----------" "-----" "--------" "----------"

    _bench() {
        local label="$1" out="$2" decomp="$3"; shift 3
        local t0 t1 ctime dtime size ratio
        t0=$(python3 -c "import time; print(time.time())")
        "$@" < "$SRC" > "$out"
        t1=$(python3 -c "import time; print(time.time())")
        ctime=$(python3 -c "print(f'{$t1-$t0:.2f}s')")
        t0=$(python3 -c "import time; print(time.time())")
        eval "$decomp \"$out\"" > /dev/null 2>&1
        t1=$(python3 -c "import time; print(time.time())")
        dtime=$(python3 -c "print(f'{$t1-$t0:.2f}s')")
        size=$(wc -c < "$out" | tr -d ' ')
        ratio=$(python3 -c "print(f'{$size/$ORIG:.3f}')")
        printf "%-20s %-12s %-12s %-12s %-10s\n" "$label" "$size" "$ratio" "$ctime" "$dtime"
    }

    _bench "az -1"    "$OUT/out.az1"   "$AZ -d -c"    $AZ -c -1
    _bench "az -2"    "$OUT/out.az2"   "$AZ -d -c"    $AZ -c -2
    _bench "az -3"    "$OUT/out.az3"   "$AZ -d -c"    $AZ -c -3
    _bench "az -4"    "$OUT/out.az4"   "$AZ -d -c"    $AZ -c -4
    _bench "az -5"    "$OUT/out.az5"   "$AZ -d -c"    $AZ -c -5
    _bench "lz4"      "$OUT/out.lz4"   "lz4 -d -c"    lz4 -c
    _bench "lz4 -9"   "$OUT/out.lz4-9" "lz4 -d -c"    lz4 -9 -c
    _bench "gzip -1"  "$OUT/out.gz1"   "gzip -d -c"   gzip -1 -c
    _bench "gzip -6"  "$OUT/out.gz6"   "gzip -d -c"   gzip -6 -c
    _bench "gzip -9"  "$OUT/out.gz9"   "gzip -d -c"   gzip -9 -c
    _bench "zstd -1"  "$OUT/out.zst1"  "zstd -d -c"   zstd -1 -c
    _bench "zstd -3"  "$OUT/out.zst3"  "zstd -d -c"   zstd -3 -c
    _bench "zstd -9"  "$OUT/out.zst9"  "zstd -d -c"   zstd -9 -c
    _bench "zstd -19" "$OUT/out.zst19" "zstd -d -c"   zstd -19 -c
    _bench "xz -1"    "$OUT/out.xz1"   "xz -d -c"     xz -1 -c
    _bench "xz -6"    "$OUT/out.xz6"   "xz -d -c"     xz -6 -c
    echo ""
    echo "Output files in $OUT"

# Compress a file or directory with all algorithms (no comparison table)

# Usage: just compress-all <target>
compress-all target: build
    #!/usr/bin/env bash
    set -euo pipefail
    TARGET="{{ target }}"
    OUT="{{ tmp_dir }}"
    mkdir -p "$OUT"

    if [ -d "$TARGET" ]; then
        NAME=$(basename "$TARGET")
        echo "Compressing directory: $TARGET"
        just compress-dir-all "$TARGET"
    else
        echo "Compressing file: $TARGET"
        just compress-file-all "$TARGET"
    fi

# ── File compression ──────────────────────────────────────────────────────────

# Compress a single file with az (level 3, keeps original)
compress-az file: build
    {{ az_bin }} -3 -k -v "{{ file }}"

# Compress a single file with az at all levels
compress-file-all file: build
    #!/usr/bin/env bash
    set -euo pipefail
    F="{{ file }}"
    BASE=$(basename "$F")
    OUT="{{ tmp_dir }}"
    mkdir -p "$OUT"
    echo "File: $F"
    for lvl in 1 2 3 4 5; do
        {{ az_bin }} -c -$lvl "$F" > "$OUT/${BASE}.az${lvl}"
        echo "  az -$lvl → $OUT/${BASE}.az${lvl}"
    done
    lz4  -c       "$F" > "$OUT/${BASE}.lz4"  && echo "  lz4    → $OUT/${BASE}.lz4"
    gzip -6 -c    "$F" > "$OUT/${BASE}.gz"   && echo "  gzip   → $OUT/${BASE}.gz"
    zstd -3 -c    "$F" > "$OUT/${BASE}.zst"  && echo "  zstd   → $OUT/${BASE}.zst"
    xz   -6 -c    "$F" > "$OUT/${BASE}.xz"   && echo "  xz     → $OUT/${BASE}.xz"

# ── Directory / tar compression ───────────────────────────────────────────────

# Compress a directory into target.tar.az (level 3)
compress-dir-az dir: build
    tar -cf - "{{ dir }}" | {{ az_bin }} -c -3 > "{{ dir }}.tar.az"
    @echo "Created: {{ dir }}.tar.az"

# Decompress a .tar.az archive
decompress-dir-az archive:
    {{ az_bin }} -d -c "{{ archive }}" | tar -xf -
    @echo "Extracted: {{ archive }}"

# Compress a directory with az at all levels into /tmp/az-bench/
compress-dir-all dir: build
    #!/usr/bin/env bash
    set -euo pipefail
    DIR="{{ dir }}"
    NAME=$(basename "$DIR")
    OUT="{{ tmp_dir }}"
    mkdir -p "$OUT"
    echo "Directory: $DIR → $OUT/${NAME}.tar.*"

    tar -cf - "$DIR" | {{ az_bin }} -c -1 > "$OUT/${NAME}.tar.az1" && echo "  az -1  → $OUT/${NAME}.tar.az1"
    tar -cf - "$DIR" | {{ az_bin }} -c -2 > "$OUT/${NAME}.tar.az2" && echo "  az -2  → $OUT/${NAME}.tar.az2"
    tar -cf - "$DIR" | {{ az_bin }} -c -3 > "$OUT/${NAME}.tar.az3" && echo "  az -3  → $OUT/${NAME}.tar.az3"
    tar -cf - "$DIR" | {{ az_bin }} -c -4 > "$OUT/${NAME}.tar.az4" && echo "  az -4  → $OUT/${NAME}.tar.az4"
    tar -cf - "$DIR" | {{ az_bin }} -c -5 > "$OUT/${NAME}.tar.az5" && echo "  az -5  → $OUT/${NAME}.tar.az5"
    tar -cf - "$DIR" | lz4  -c          > "$OUT/${NAME}.tar.lz4" && echo "  lz4    → $OUT/${NAME}.tar.lz4"
    tar -czf        - "$DIR"             > "$OUT/${NAME}.tar.gz"  && echo "  gzip   → $OUT/${NAME}.tar.gz"
    tar -cf - "$DIR" | zstd -3 -c       > "$OUT/${NAME}.tar.zst" && echo "  zstd   → $OUT/${NAME}.tar.zst"
    tar -cJf        - "$DIR"             > "$OUT/${NAME}.tar.xz"  && echo "  xz     → $OUT/${NAME}.tar.xz"

# ── Decompression ─────────────────────────────────────────────────────────────

# Decompress a .az file (removes the .az extension)
decompress file: build
    {{ az_bin }} -d "{{ file }}"

# Test integrity of a .az file (decompress to /dev/null)
test-integrity file: build
    {{ az_bin }} -t -v "{{ file }}"

# ── Cleanup ───────────────────────────────────────────────────────────────────

# Remove the built binary
clean:
    rm -f {{ az_bin }}

# Remove benchmark output files
clean-bench:
    rm -rf {{ tmp_dir }}
    rm -f bench.txt

# Remove everything
clean-all: clean clean-bench
