// Command az compresses and decompresses files.
//
// Usage:
//
//	az [OPTIONS] [FILE...]
//
//	-1 ... -5         Compression level (default: -3)
//	-d, --decompress  Decompress mode
//	-k, --keep        Keep source file (default: remove after success)
//	-c, --stdout      Write to stdout
//	-f, --force       Overwrite existing output files
//	-t, --test        Test integrity (decompress to /dev/null)
//	-v, --verbose     Print statistics (ratio, speed)
//	-o FILE           Output filename (only valid with a single input file)
//	--no-checksum     Disable checksums
//
// With no FILE, or when FILE is -, read from stdin and write to stdout.
// Compressed files get the .az suffix; decompression removes it.
//
// Compression levels:
//
//	-1   Fastest (lz4 level 3)
//	-2   Fast    (lz4 level 6)
//	-3   Default (zstd level 6)
//	-4   Better  (zstd level 12)
//	-5   Best    (zstd level 18)
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-again/az"
)

func main() {
	os.Exit(run())
}

func run() int {
	// ── Flag parsing ──────────────────────────────────────────────────────────
	var (
		level1, level2, level3, level4, level5              bool
		decompress, keep, stdout, force, test, verbose      bool
		noChecksum                                          bool
		outputFile                                          string
	)
	flag.BoolVar(&level1, "1", false, "fastest (lz4 level 3)")
	flag.BoolVar(&level2, "2", false, "fast (lz4 level 6)")
	flag.BoolVar(&level3, "3", false, "default (zstd level 6)")
	flag.BoolVar(&level4, "4", false, "better (zstd level 12)")
	flag.BoolVar(&level5, "5", false, "best (zstd level 18)")
	flag.BoolVar(&decompress, "d", false, "decompress")
	flag.BoolVar(&decompress, "decompress", false, "decompress")
	flag.BoolVar(&keep, "k", false, "keep source files")
	flag.BoolVar(&keep, "keep", false, "keep source files")
	flag.BoolVar(&stdout, "c", false, "write to stdout")
	flag.BoolVar(&stdout, "stdout", false, "write to stdout")
	flag.BoolVar(&force, "f", false, "overwrite existing files")
	flag.BoolVar(&force, "force", false, "overwrite existing files")
	flag.BoolVar(&test, "t", false, "test integrity")
	flag.BoolVar(&test, "test", false, "test integrity")
	flag.BoolVar(&verbose, "v", false, "verbose output")
	flag.BoolVar(&verbose, "verbose", false, "verbose output")
	flag.BoolVar(&noChecksum, "no-checksum", false, "disable checksums")
	flag.StringVar(&outputFile, "o", "", "output file")
	flag.Parse()

	// Determine level
	level := az.DefaultLevel
	switch {
	case level5:
		level = az.Level5
	case level4:
		level = az.Level4
	case level2:
		level = az.Level2
	case level1:
		level = az.Level1
	case level3:
		level = az.Level3
	}

	args := flag.Args()

	// ── Stdin/stdout mode ─────────────────────────────────────────────────────
	if len(args) == 0 || (len(args) == 1 && args[0] == "-") {
		return runStream(os.Stdin, os.Stdout, decompress, level, noChecksum, verbose, "<stdin>")
	}

	// ── File mode ─────────────────────────────────────────────────────────────
	if outputFile != "" && len(args) > 1 {
		fmt.Fprintf(os.Stderr, "az: -o cannot be used with multiple input files\n")
		return 1
	}

	ok := true
	for _, src := range args {
		if err := processFile(src, outputFile, decompress, keep, stdout, force, test, verbose, noChecksum, level); err != nil {
			fmt.Fprintf(os.Stderr, "az: %s: %v\n", src, err)
			ok = false
		}
	}
	if !ok {
		return 1
	}
	return 0
}

func processFile(srcPath, dstPath string, decompress, keep, toStdout, force, test, verbose, noChecksum bool, level az.Level) error {
	// Determine output path
	if !toStdout && !test {
		if dstPath == "" {
			if decompress {
				if !strings.HasSuffix(srcPath, ".az") {
					return fmt.Errorf("unknown suffix, ignoring")
				}
				dstPath = strings.TrimSuffix(srcPath, ".az")
			} else {
				dstPath = srcPath + ".az"
			}
		}
		if !force {
			if _, err := os.Stat(dstPath); err == nil {
				return fmt.Errorf("output file %s already exists (use -f to overwrite)", dstPath)
			}
		}
	}

	in, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer in.Close()

	var out io.WriteCloser
	if toStdout || test {
		if test {
			out = nopCloser{io.Discard}
		} else {
			out = nopCloser{os.Stdout}
		}
	} else {
		// Write to a temp file then rename atomically.
		tmp, err2 := os.CreateTemp(filepath.Dir(dstPath), ".az-tmp-*")
		if err2 != nil {
			return err2
		}
		tmpName := tmp.Name()
		defer func() {
			os.Remove(tmpName)
		}()
		out = &renameCloser{File: tmp, dst: dstPath}
	}

	start := time.Now()
	var inBytes, outBytes int64

	cw := &countWriter{w: out}
	if decompress {
		inBytes, outBytes, err = copyDecompress(in, cw)
	} else {
		inBytes, outBytes, err = copyCompress(in, cw, level, noChecksum)
	}
	_ = outBytes // outBytes tracked via cw.n

	if err != nil {
		out.Close()
		return err
	}

	if err = out.Close(); err != nil {
		return err
	}

	if verbose {
		elapsed := time.Since(start).Seconds()
		speed := float64(inBytes) / elapsed / (1 << 20)
		var ratio float64
		if decompress {
			if cw.n > 0 {
				ratio = float64(inBytes) / float64(cw.n)
			}
		} else {
			if inBytes > 0 {
				ratio = float64(cw.n) / float64(inBytes)
			}
		}
		fmt.Fprintf(os.Stderr, "%s: %d → %d bytes (%.3f ratio, %.1f MB/s)\n",
			srcPath, inBytes, cw.n, ratio, speed)
	}

	if !keep && !toStdout && !test {
		os.Remove(srcPath)
	}
	return nil
}

func runStream(in io.Reader, out io.Writer, decompress bool, level az.Level, noChecksum, verbose bool, name string) int {
	start := time.Now()
	var inBytes int64
	var err error

	cw := &countWriter{w: out}
	if decompress {
		inBytes, _, err = copyDecompress(in, cw)
	} else {
		inBytes, _, err = copyCompress(in, cw, level, noChecksum)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "az: %s: %v\n", name, err)
		return 1
	}

	if verbose {
		elapsed := time.Since(start).Seconds()
		speed := float64(inBytes) / elapsed / (1 << 20)
		fmt.Fprintf(os.Stderr, "%s: %d → %d bytes (%.1f MB/s)\n", name, inBytes, cw.n, speed)
	}
	return 0
}

func copyCompress(in io.Reader, out io.Writer, level az.Level, noChecksum bool) (inBytes, outBytes int64, err error) {
	opts := []az.Option{az.WithLevel(level)}
	if noChecksum {
		opts = append(opts, az.WithChecksum(false))
	}
	cw := az.NewWriter(out, opts...)
	inBytes, err = io.Copy(cw, in)
	if err != nil {
		return
	}
	err = cw.Close()
	return
}

func copyDecompress(in io.Reader, out io.Writer) (inBytes, outBytes int64, err error) {
	cr := az.NewReader(in)
	defer cr.Close()
	outBytes, err = io.Copy(out, cr)
	if err != nil && !errors.Is(err, io.EOF) {
		return
	}
	err = nil
	return
}

// ── Helpers ────────────────────────────────────────────────────────────────────

type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }

// renameCloser wraps an *os.File; on Close it renames the temp file to dst.
type renameCloser struct {
	*os.File
	dst string
}

func (rc *renameCloser) Close() error {
	if err := rc.File.Close(); err != nil {
		return err
	}
	return os.Rename(rc.File.Name(), rc.dst)
}

// countWriter counts bytes written through it.
type countWriter struct {
	w io.Writer
	n int64
}

func (cw *countWriter) Write(p []byte) (int, error) {
	n, err := cw.w.Write(p)
	cw.n += int64(n)
	return n, err
}

func (cw *countWriter) Close() error {
	if c, ok := cw.w.(io.Closer); ok {
		return c.Close()
	}
	return nil
}
