#!/usr/bin/env bash
# scripts/bench.sh — compression/decompression benchmark
#
# Measures compressed size, ratio, compression speed, and decompression speed
# for az (all levels) plus lz4, gzip, zstd, and xz via a common tar pipeline.
#
# Usage:
#   ./scripts/bench.sh [OPTIONS] <file-or-directory>
#
# Options:
#   -n N        iterations per algorithm (default: 5; use 1 for a quick pass)
#   -w N        warmup iterations before timing (default: 1)
#   --full      include slow configs (zstd -19, xz -9)
#   --az-only   benchmark az levels only
#   --csv FILE  also write results to FILE in CSV format
#   --no-color  disable ANSI color output
#   -h, --help  show this message
#
# Output:
#   A table sorted by compressed size showing for each algorithm:
#     compressed size, ratio (smaller=better), compress MB/s, decompress MB/s
#
# Requirements: az (built with 'just build'), lz4, gzip, zstd, xz, python3

set -euo pipefail

# ── Defaults ──────────────────────────────────────────────────────────────────

ITERS=5
WARMUP=1
FULL=false
AZ_ONLY=false
CSV_FILE=""
COLOR=true
AZ_BIN="${AZ_BIN:-./az}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
WORK_DIR="$(mktemp -d /tmp/az-bench-XXXXXX)"
TARGET=""

# ── Argument parsing ──────────────────────────────────────────────────────────

usage() {
    sed -n '2,/^$/p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
    exit 0
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        -n)       ITERS="$2";    shift 2 ;;
        -w)       WARMUP="$2";   shift 2 ;;
        --full)   FULL=true;     shift   ;;
        --az-only) AZ_ONLY=true; shift   ;;
        --csv)    CSV_FILE="$2"; shift 2 ;;
        --no-color) COLOR=false; shift   ;;
        -h|--help) usage         ;;
        -*)       echo "Unknown option: $1" >&2; exit 1 ;;
        *)        TARGET="$1";   shift   ;;
    esac
done

if [[ -z "$TARGET" ]]; then
    echo "Error: no target specified." >&2
    echo "Usage: $0 [OPTIONS] <file-or-directory>" >&2
    exit 1
fi

if [[ ! -e "$TARGET" ]]; then
    echo "Error: '$TARGET' not found." >&2
    exit 1
fi

# ── Color helpers ─────────────────────────────────────────────────────────────

if $COLOR && [[ -t 1 ]]; then
    BOLD='\033[1m'; RESET='\033[0m'; DIM='\033[2m'
    GREEN='\033[32m'; CYAN='\033[36m'; YELLOW='\033[33m'; RED='\033[31m'
else
    BOLD=''; RESET=''; DIM=''; GREEN=''; CYAN=''; YELLOW=''; RED=''
fi

info()    { echo -e "${CYAN}→${RESET} $*"; }
success() { echo -e "${GREEN}✓${RESET} $*"; }
warn()    { echo -e "${YELLOW}!${RESET} $*"; }

# ── Prerequisite checks ───────────────────────────────────────────────────────

check_cmd() {
    if ! command -v "$1" &>/dev/null; then
        warn "Command '$1' not found — skipping those algorithms."
        return 1
    fi
    return 0
}

HAS_LZ4=true;  check_cmd lz4   || HAS_LZ4=false
HAS_GZIP=true; check_cmd gzip  || HAS_GZIP=false
HAS_ZSTD=true; check_cmd zstd  || HAS_ZSTD=false
HAS_XZ=true;   check_cmd xz    || HAS_XZ=false

if [[ ! -x "$AZ_BIN" ]]; then
    info "Building az..."
    (cd "$REPO_DIR" && go build -trimpath -ldflags '-s -w' -o az ./cmd/az)
    AZ_BIN="$REPO_DIR/az"
fi

# ── Timing helper ─────────────────────────────────────────────────────────────
#
# time_stdin_to_file <in> <out> <cmd...>
#   Runs: cmd < in > out
#   Prints elapsed seconds (float) to stdout.
#
# time_file_to_null <in> <cmd...>
#   Runs: cmd < in > /dev/null
#   Prints elapsed seconds (float) to stdout.

_time_py='
import subprocess, sys, time, os
args = sys.argv[1:]
# Split on separator "--"
sep = args.index("--")
in_file, out_file = args[:sep]
cmd = args[sep+1:]
fin  = open(in_file,  "rb")
fout = open(out_file, "wb") if out_file != "/dev/null" else open(os.devnull, "wb")
t0 = time.perf_counter()
subprocess.run(cmd, stdin=fin, stdout=fout, check=True)
t1 = time.perf_counter()
print(f"{t1-t0:.6f}")
fin.close(); fout.close()
'

time_stdin_to_file() {
    local in_file="$1" out_file="$2"; shift 2
    python3 -c "$_time_py" "$in_file" "$out_file" -- "$@"
}

time_file_to_null() {
    local in_file="$1"; shift
    python3 -c "$_time_py" "$in_file" /dev/null -- "$@"
}

# ── Median helper (python3) ───────────────────────────────────────────────────

median() {
    # Reads space-separated floats from stdin, prints median
    python3 -c "
import sys, statistics
vals = [float(x) for x in sys.stdin.read().split()]
print(f'{statistics.median(vals):.6f}')
"
}

# ── Benchmark runner ──────────────────────────────────────────────────────────
#
# Usage: run_algo <label> <ext> <compress-cmd...> --- <decompress-cmd...>
#
# The function searches for '---' as a separator between compress and decompress
# command arrays. Both commands read from stdin / write to stdout.
#
# Results are appended to RESULTS (global array of tab-separated strings).

declare -a RESULTS=()

run_algo() {
    local label="$1" ext="$2"; shift 2

    # Split compress / decompress at '---'
    local -a compress_cmd=()
    local -a decompress_cmd=()
    local in_decomp=false
    for arg in "$@"; do
        if [[ "$arg" == "---" ]]; then
            in_decomp=true
        elif $in_decomp; then
            decompress_cmd+=("$arg")
        else
            compress_cmd+=("$arg")
        fi
    done

    local out_file="$WORK_DIR/compressed.${ext}"

    # ── Warmup (not timed) ─────────────────────────────────────────────────
    for _ in $(seq 1 "$WARMUP"); do
        "${compress_cmd[@]}"    < "$SRC_FILE" > "$out_file" 2>/dev/null
        "${decompress_cmd[@]}"  < "$out_file" > /dev/null  2>/dev/null
    done

    # ── Compression timing ─────────────────────────────────────────────────
    local compress_times=""
    for _ in $(seq 1 "$ITERS"); do
        t=$(time_stdin_to_file "$SRC_FILE" "$out_file" "${compress_cmd[@]}" 2>/dev/null)
        compress_times="$compress_times $t"
    done
    local compress_s
    compress_s=$(echo "$compress_times" | median)

    # ── Compressed size ────────────────────────────────────────────────────
    local compressed_bytes
    compressed_bytes=$(wc -c < "$out_file" | tr -d ' ')

    # ── Decompression timing ───────────────────────────────────────────────
    local decompress_times=""
    for _ in $(seq 1 "$ITERS"); do
        t=$(time_file_to_null "$out_file" "${decompress_cmd[@]}" 2>/dev/null)
        decompress_times="$decompress_times $t"
    done
    local decompress_s
    decompress_s=$(echo "$decompress_times" | median)

    # ── Derived metrics ────────────────────────────────────────────────────
    # speed = source_bytes / elapsed_seconds / 1_000_000  (MB/s, base-10)
    local c_speed d_speed ratio
    c_speed=$(python3 -c "print(f'{$SRC_BYTES/$compress_s/1e6:.1f}')")
    d_speed=$(python3 -c "print(f'{$SRC_BYTES/$decompress_s/1e6:.1f}')")
    ratio=$(python3 -c "print(f'{$compressed_bytes/$SRC_BYTES:.4f}')")

    RESULTS+=("${label}	${compressed_bytes}	${ratio}	${c_speed}	${d_speed}")
}

# ── Prepare source ─────────────────────────────────────────────────────────────

if [[ -d "$TARGET" ]]; then
    info "Tarring directory $TARGET ..."
    SRC_FILE="$WORK_DIR/source.tar"
    tar -cf "$SRC_FILE" -C "$(dirname "$(realpath "$TARGET")")" "$(basename "$TARGET")"
else
    SRC_FILE="$(realpath "$TARGET")"
fi

SRC_BYTES=$(wc -c < "$SRC_FILE" | tr -d ' ')
SRC_HUMAN=$(python3 -c "
b=$SRC_BYTES
for u in ('B','KB','MB','GB'):
    if b<1024 or u=='GB': print(f'{b:.1f} {u}'); break
    b/=1024
")

info "Source: $SRC_FILE  ($SRC_HUMAN,  $ITERS iterations + $WARMUP warmup)"
echo ""

# ── Run algorithms ────────────────────────────────────────────────────────────

info "Benchmarking..."

# az levels 1–5
run_algo "az -1" az1  "$AZ_BIN" -c -1  ---  "$AZ_BIN" -d -c
run_algo "az -2" az2  "$AZ_BIN" -c -2  ---  "$AZ_BIN" -d -c
run_algo "az -3" az3  "$AZ_BIN" -c -3  ---  "$AZ_BIN" -d -c
run_algo "az -4" az4  "$AZ_BIN" -c -4  ---  "$AZ_BIN" -d -c
run_algo "az -5" az5  "$AZ_BIN" -c -5  ---  "$AZ_BIN" -d -c

if ! $AZ_ONLY; then
    $HAS_LZ4  && run_algo "lz4"     lz4    lz4 -1  -c  ---  lz4 -d -c
    $HAS_LZ4  && run_algo "lz4 -9"  lz49   lz4 -9  -c  ---  lz4 -d -c
    $HAS_GZIP && run_algo "gzip -1" gz1    gzip -1 -c  ---  gzip -d -c
    $HAS_GZIP && run_algo "gzip -6" gz6    gzip -6 -c  ---  gzip -d -c
    $HAS_GZIP && run_algo "gzip -9" gz9    gzip -9 -c  ---  gzip -d -c
    $HAS_ZSTD && run_algo "zstd -1" zst1   zstd -1 -c  ---  zstd -d -c
    $HAS_ZSTD && run_algo "zstd -3" zst3   zstd -3 -c  ---  zstd -d -c
    $HAS_ZSTD && run_algo "zstd -9" zst9   zstd -9 -c  ---  zstd -d -c
    $HAS_XZ   && run_algo "xz -3"   xz3    xz -3   -c  ---  xz -d -c
    $HAS_XZ   && run_algo "xz -6"   xz6    xz -6   -c  ---  xz -d -c
    if $FULL; then
        $HAS_ZSTD && run_algo "zstd -19" zst19  zstd -19 -c  ---  zstd -d -c
        $HAS_XZ   && run_algo "xz -9"   xz9    xz -9   -c  ---  xz -d -c
    fi
fi

# ── Sort results by compressed size (ascending) ───────────────────────────────

IFS=$'\n' SORTED=($(
    for row in "${RESULTS[@]}"; do echo "$row"; done \
    | sort -t$'\t' -k2 -n
))

# ── Print table ───────────────────────────────────────────────────────────────

FMT="%-12s  %12s  %7s  %14s  %14s\n"
SEP="────────────  ────────────  ───────  ──────────────  ──────────────"

echo ""
echo -e "${BOLD}Source: $(basename "$SRC_FILE")  (${SRC_HUMAN} uncompressed)${RESET}"
echo -e "${DIM}Median of $ITERS runs. Speed = source bytes / elapsed time.${RESET}"
echo ""
echo -e "${BOLD}$(printf "$FMT" "Algorithm" "Compressed" "Ratio" "Compress MB/s" "Decomp MB/s")${RESET}"
echo "$SEP"

# Find best (smallest) compressed size for highlighting
best_size=$(echo "${SORTED[0]}" | cut -f2)

for row in "${SORTED[@]}"; do
    IFS=$'\t' read -r label cbytes ratio cspeed dspeed <<< "$row"
    cbytes_human=$(python3 -c "
b=$cbytes
for u in ('B','KB','MB','GB'):
    if b<1024 or u=='GB': print(f'{b:.0f} {u}'); break
    b/=1024
")
    # Highlight the best ratio row
    if [[ "$cbytes" == "$best_size" ]]; then
        echo -e "${GREEN}$(printf "$FMT" "$label" "$cbytes_human" "$ratio" "$cspeed" "$dspeed")${RESET}"
    elif [[ "$label" == az* ]]; then
        echo -e "${CYAN}$(printf "$FMT" "$label" "$cbytes_human" "$ratio" "$cspeed" "$dspeed")${RESET}"
    else
        printf "$FMT" "$label" "$cbytes_human" "$ratio" "$cspeed" "$dspeed"
    fi
done

echo "$SEP"
echo -e "${DIM}Ratio < 1.0 means smaller than original. Lower ratio = better compression.${RESET}"

# ── CSV output ────────────────────────────────────────────────────────────────

if [[ -n "$CSV_FILE" ]]; then
    echo "algorithm,compressed_bytes,ratio,compress_mbs,decompress_mbs,source_bytes,source_file" > "$CSV_FILE"
    for row in "${SORTED[@]}"; do
        IFS=$'\t' read -r label cbytes ratio cspeed dspeed <<< "$row"
        echo "\"$label\",$cbytes,$ratio,$cspeed,$dspeed,$SRC_BYTES,\"$(basename "$SRC_FILE")\"" >> "$CSV_FILE"
    done
    echo ""
    success "CSV written to $CSV_FILE"
fi

# ── Cleanup ───────────────────────────────────────────────────────────────────

rm -rf "$WORK_DIR"
