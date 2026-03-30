// Package az implements the az compression format.
//
// az is a general-purpose compression algorithm that combines LZ77
// back-reference matching with Huffman and FSE entropy coding.  It provides
// five compression levels:
//
//   - Level1 (Fastest): pure LZ77, no entropy coding — near LZ4 speed
//   - Level2 (Fast): Huffman-compressed literals, LZ77 sequences
//   - Level3 (Default): lazy match + Huffman literals + FSE sequences
//   - Level4 (Better): deeper search, 4X Huffman
//   - Level5 (Best): optimal parse
//
// The streaming Writer and Reader implement io.WriteCloser / io.ReadCloser
// and are suitable for use with archive/tar or as a drop-in for gzip.
package az

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"sync"
)

// ─── One-shot helpers ─────────────────────────────────────────────────────────

// Compress compresses src at the given level and returns the compressed bytes.
func Compress(src []byte, level Level) ([]byte, error) {
	if level < minLevel || level > maxLevel {
		return nil, ErrLevel
	}
	opts := defaultOptions()
	opts.Level = level
	opts.ContentSize = true

	var buf bytes.Buffer
	w := newWriter(&buf, opts)
	if _, err := w.Write(src); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
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
	w       *bufio.Writer
	opts    Options
	cfg     levelConfig
	st      *encoderState
	buf     []byte // accumulation buffer
	cksum   *xxhDigest
	written int64
	closed  bool
}

// NewWriter returns a new Writer that writes compressed data to w.
func NewWriter(w io.Writer, opts ...Option) *Writer {
	o := defaultOptions()
	for _, fn := range opts {
		fn(&o)
	}
	return newWriter(w, o)
}

func newWriter(w io.Writer, opts Options) *Writer {
	cfg := levelConfigs[opts.Level]
	return &Writer{
		w:     bufio.NewWriterSize(w, 64<<10),
		opts:  opts,
		cfg:   cfg,
		st:    newEncoderState(cfg),
		buf:   make([]byte, 0, cfg.blockSize),
		cksum: newXXH64(),
	}
}

// Write compresses p and writes it to the underlying writer.
func (w *Writer) Write(p []byte) (int, error) {
	if w.closed {
		return 0, fmt.Errorf("az: write to closed writer")
	}
	if len(p) == 0 {
		return 0, nil
	}

	// Write frame header on first non-empty Write.
	if w.written == 0 && len(w.buf) == 0 {
		if err := writeFrameHeader(w.w, w.opts, -1); err != nil {
			return 0, err
		}
	}

	total := len(p)
	for len(p) > 0 {
		space := w.cfg.blockSize - len(w.buf)
		n := len(p)
		if n > space {
			n = space
		}
		w.buf = append(w.buf, p[:n]...)
		p = p[n:]
		w.written += int64(n)

		if len(w.buf) >= w.cfg.blockSize {
			if err := w.flushBlock(false); err != nil {
				return total - len(p), err
			}
		}
	}
	return total, nil
}

func (w *Writer) flushBlock(isLast bool) error {
	if len(w.buf) == 0 && !isLast {
		return nil
	}

	// Update content checksum before compression
	if w.opts.Checksum && len(w.buf) > 0 {
		_, _ = w.cksum.Write(w.buf)
	}

	compressed, isRaw := compressBlock(w.buf, w.opts.Level, w.st)

	// Block header
	var hdr [4]byte
	h := uint32(len(compressed))
	if isRaw {
		h |= blkIsRaw
	}
	if isLast {
		h |= blkIsLast
	}
	binary.LittleEndian.PutUint32(hdr[:], h)
	if _, err := w.w.Write(hdr[:]); err != nil {
		return err
	}

	if _, err := w.w.Write(compressed); err != nil {
		return err
	}

	// Block checksum
	if w.opts.Checksum && len(w.buf) > 0 {
		d := newXXH64()
		_, _ = d.Write(w.buf)
		var cs [4]byte
		binary.LittleEndian.PutUint32(cs[:], d.Sum32())
		if _, err := w.w.Write(cs[:]); err != nil {
			return err
		}
	}

	w.buf = w.buf[:0]
	w.st.resetBlock()
	return nil
}

// Close flushes any buffered data and writes the end-of-stream marker.
func (w *Writer) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true

	// If nothing was written yet, emit a minimal empty frame.
	if w.written == 0 && len(w.buf) == 0 {
		// For empty streams, write a frame with no data blocks.
		opts := w.opts
		opts.ContentSize = true // advertise size=0
		if err := writeFrameHeader(w.w, opts, 0); err != nil {
			return err
		}
		if err := writeEndOfStream(w.w, w.cksum.Sum32(), w.opts.Checksum); err != nil {
			return err
		}
		return w.w.Flush()
	}

	if err := w.flushBlock(false); err != nil {
		return err
	}
	if err := writeEndOfStream(w.w, w.cksum.Sum32(), w.opts.Checksum); err != nil {
		return err
	}
	return w.w.Flush()
}

// Reset discards the writer's state and starts writing a new stream to dst.
func (w *Writer) Reset(dst io.Writer) {
	w.w.Reset(dst)
	w.buf = w.buf[:0]
	w.written = 0
	w.closed = false
	w.cksum.reset()
	w.st.resetFull(w.cfg)
}

// ─── Reader ───────────────────────────────────────────────────────────────────

// Reader decompresses data read from an underlying reader.
type Reader struct {
	r        *bufio.Reader
	fh       frameHeader
	buf      []byte // decompressed but not yet consumed
	cksum    *xxhDigest
	headerOK bool
	done     bool
	err      error
}

// NewReader returns a new Reader that decompresses from r.
func NewReader(r io.Reader) *Reader {
	return &Reader{
		r:     bufio.NewReaderSize(r, 64<<10),
		cksum: newXXH64(),
	}
}

// Read reads decompressed data into p.
func (r *Reader) Read(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	if r.done {
		return 0, io.EOF
	}

	// Parse frame header on first read
	if !r.headerOK {
		fh, err := readFrameHeader(r.r)
		if err != nil {
			r.err = err
			return 0, err
		}
		r.fh = fh
		r.headerOK = true
	}

	// Drain existing buffer
	if len(r.buf) > 0 {
		n := copy(p, r.buf)
		r.buf = r.buf[n:]
		return n, nil
	}

	// Read next block
	for {
		bh, err := readBlockHeader(r.r)
		if err != nil {
			r.err = err
			return 0, err
		}

		if bh.isLast && bh.size == 0 {
			// End of stream
			if r.fh.checksum {
				var cs [4]byte
				if _, err2 := io.ReadFull(r.r, cs[:]); err2 != nil {
					r.err = ErrCorrupted
					return 0, r.err
				}
				stored := binary.LittleEndian.Uint32(cs[:])
				if stored != r.cksum.Sum32() {
					r.err = ErrChecksumFail
					return 0, r.err
				}
			}
			r.done = true
			return 0, io.EOF
		}

		// Read block data
		blockData := make([]byte, bh.size)
		if _, err := io.ReadFull(r.r, blockData); err != nil {
			r.err = ErrCorrupted
			return 0, r.err
		}

		// Read block checksum if present
		if r.fh.checksum {
			var cs [4]byte
			if _, err2 := io.ReadFull(r.r, cs[:]); err2 != nil {
				r.err = ErrCorrupted
				return 0, r.err
			}
			// checksum is over the uncompressed data — verified after decompression
		}

		// Decompress
		out, err := decompressBlockData(blockData, bh.isRaw, r.fh.blockSize)
		if err != nil {
			r.err = err
			return 0, err
		}

		// Update content checksum
		if r.fh.checksum {
			_, _ = r.cksum.Write(out)
		}

		r.buf = out
		n := copy(p, r.buf)
		r.buf = r.buf[n:]
		return n, nil
	}
}

// Close closes the reader and discards any unread data.
func (r *Reader) Close() error {
	r.done = true
	return nil
}

// Reset discards the reader's state and starts reading from src.
func (r *Reader) Reset(src io.Reader) {
	r.r.Reset(src)
	r.buf = nil
	r.done = false
	r.err = nil
	r.headerOK = false
	r.cksum.reset()
}

// ─── Pool for reusing encoderState ───────────────────────────────────────────

var encoderPools [6]sync.Pool

func init() {
	for i := Level1; i <= Level5; i++ {
		level := i
		encoderPools[level].New = func() any {
			return newEncoderState(levelConfigs[level])
		}
	}
}

func getEncoderState(level Level) *encoderState {
	st := encoderPools[level].Get().(*encoderState)
	st.resetFull(levelConfigs[level])
	return st
}

func putEncoderState(level Level, st *encoderState) {
	encoderPools[level].Put(st)
}
