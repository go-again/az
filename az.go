// Package az provides a general-purpose compression library and CLI tool.
//
// Levels 1–2 use the LZ4 algorithm; levels 3–5 use Zstandard.
// The streaming Writer and Reader implement io.WriteCloser / io.ReadCloser
// and are suitable for use with archive/tar or as a drop-in for gzip.
//
//	// One-shot
//	compressed, err := az.Compress(data, az.Level3)
//	original, err := az.Decompress(compressed)
//
//	// Streaming
//	w := az.NewWriter(dst, az.WithLevel(az.Level4))
//	w.Write(data)
//	w.Close()
//
//	r := az.NewReader(src)
//	io.Copy(dst, r)
//	r.Close()
package az

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"runtime"

	lz4pkg "github.com/go-again/az/internal/lz4"
	zstdpkg "github.com/go-again/az/internal/zstd"
)

// Magic numbers identifying the underlying stream format.
const (
	magicLZ4  uint32 = 0x184D2204
	magicZstd uint32 = 0xFD2FB528
)

// lz4Level maps az levels to lz4 CompressionLevel constants.
// Fast (0) uses the hash-only compressor; Level3 (1<<10 = depth 1024) uses HC.
var lz4Level = map[Level]lz4pkg.CompressionLevel{
	Level1: lz4pkg.Fast,   // hash-only, no chain — true lz4 speed
	Level2: lz4pkg.Level3, // HC depth 1024 — moderate compression
}


// zstdLevel maps az levels to zstd EncoderLevel constants.
var zstdLevel = map[Level]zstdpkg.EncoderLevel{
	Level3: zstdpkg.SpeedDefault,
	Level4: zstdpkg.SpeedBetterCompression,
	Level5: zstdpkg.SpeedBestCompression,
}

// ─── One-shot helpers ─────────────────────────────────────────────────────────

// Compress compresses src at the given level and returns the compressed bytes.
func Compress(src []byte, level Level) ([]byte, error) {
	if level < minLevel || level > maxLevel {
		return nil, ErrLevel
	}
	var buf bytes.Buffer
	buf.Grow(len(src)/2 + 256)

	if level <= Level2 {
		w := lz4pkg.NewWriter(&buf)
		if err := w.Apply(
			lz4pkg.CompressionLevelOption(lz4Level[level]),
			lz4pkg.ChecksumOption(true),
			lz4pkg.SizeOption(uint64(len(src))),
			lz4pkg.ConcurrencyOption(runtime.GOMAXPROCS(0)),
		); err != nil {
			return nil, err
		}
		if _, err := w.Write(src); err != nil {
			return nil, err
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
	} else {
		enc, err := zstdpkg.NewWriter(&buf,
			zstdpkg.WithEncoderLevel(zstdLevel[level]),
			zstdpkg.WithEncoderCRC(true),
		)
		if err != nil {
			return nil, err
		}
		if _, err := enc.Write(src); err != nil {
			return nil, err
		}
		if err := enc.Close(); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

// Decompress decompresses src and returns the original bytes.
func Decompress(src []byte) ([]byte, error) {
	r := NewReader(bytes.NewReader(src))
	defer r.Close()
	return io.ReadAll(r)
}

// ─── Writer ───────────────────────────────────────────────────────────────────

// Writer compresses data written to it and forwards compressed bytes to the
// underlying writer.  Call Close to flush and write the end-of-stream marker.
type Writer struct {
	opts    Options
	lz4w    *lz4pkg.Writer
	zstdEnc *zstdpkg.Encoder
}

// NewWriter returns a new Writer that writes compressed data to dst.
func NewWriter(dst io.Writer, opts ...Option) *Writer {
	o := defaultOptions()
	for _, fn := range opts {
		fn(&o)
	}
	return newWriter(dst, o)
}

func newWriter(dst io.Writer, opts Options) *Writer {
	w := &Writer{opts: opts}
	if opts.Level <= Level2 {
		w.lz4w = lz4pkg.NewWriter(dst)
		_ = w.lz4w.Apply(
			lz4pkg.CompressionLevelOption(lz4Level[opts.Level]),
			lz4pkg.ChecksumOption(opts.Checksum),
			lz4pkg.BlockChecksumOption(opts.Checksum),
			lz4pkg.ConcurrencyOption(runtime.GOMAXPROCS(0)),
		)
	} else {
		enc, err := zstdpkg.NewWriter(dst,
			zstdpkg.WithEncoderLevel(zstdLevel[opts.Level]),
			zstdpkg.WithEncoderCRC(opts.Checksum),
		)
		if err != nil {
			// NewWriter only fails on invalid options; our options are always valid.
			panic(fmt.Sprintf("az: zstd.NewWriter: %v", err))
		}
		w.zstdEnc = enc
	}
	return w
}

// Write compresses p and writes it to the underlying writer.
func (w *Writer) Write(p []byte) (int, error) {
	if w.lz4w != nil {
		return w.lz4w.Write(p)
	}
	return w.zstdEnc.Write(p)
}

// Close flushes any buffered data and finalises the stream.
func (w *Writer) Close() error {
	if w.lz4w != nil {
		return w.lz4w.Close()
	}
	return w.zstdEnc.Close()
}

// Reset discards the writer's state and starts a new stream writing to dst.
func (w *Writer) Reset(dst io.Writer) {
	if w.lz4w != nil {
		w.lz4w.Reset(dst)
		return
	}
	w.zstdEnc.Reset(dst)
}

// ─── Reader ───────────────────────────────────────────────────────────────────

// Reader decompresses data from an underlying reader.
// It auto-detects whether the stream uses the LZ4 or Zstandard format.
type Reader struct {
	br          *bufio.Reader
	src         io.Reader
	lz4r        *lz4pkg.Reader
	zstdDec     *zstdpkg.Decoder
	initialized bool
	err         error
}

// NewReader returns a new Reader that decompresses from src.
func NewReader(src io.Reader) *Reader {
	return &Reader{
		br:  bufio.NewReaderSize(src, 64<<10),
		src: src,
	}
}

// init detects the stream format from the first four magic bytes and creates
// the appropriate sub-reader without consuming bytes (bufio.Peek).
func (r *Reader) init() error {
	magic, err := r.br.Peek(4)
	if err != nil {
		if err == io.EOF {
			return io.EOF
		}
		return ErrCorrupted
	}
	m := binary.LittleEndian.Uint32(magic)
	switch m {
	case magicLZ4:
		r.lz4r = lz4pkg.NewReader(r.br)
	case magicZstd:
		dec, err := zstdpkg.NewReader(r.br)
		if err != nil {
			return fmt.Errorf("az: %w", ErrCorrupted)
		}
		r.zstdDec = dec
	default:
		return ErrCorrupted
	}
	r.initialized = true
	return nil
}

// Read reads decompressed data into p.
func (r *Reader) Read(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	if !r.initialized {
		if err := r.init(); err != nil {
			r.err = err
			return 0, err
		}
	}
	if r.lz4r != nil {
		return r.lz4r.Read(p)
	}
	return r.zstdDec.Read(p)
}

// Close closes the reader.
func (r *Reader) Close() error {
	if r.zstdDec != nil {
		r.zstdDec.Close()
	}
	return nil
}

// Reset discards the reader's state and starts reading a new stream from src.
func (r *Reader) Reset(src io.Reader) {
	r.src = src
	r.br.Reset(src)
	r.initialized = false
	r.err = nil
	if r.lz4r != nil {
		r.lz4r.Reset(r.br)
		r.lz4r = nil
	}
	if r.zstdDec != nil {
		r.zstdDec.Reset(r.br) //nolint // Reset returns error only on option changes
		r.zstdDec = nil
	}
}
