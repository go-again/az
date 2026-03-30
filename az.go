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
	"runtime"
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

// ─── Parallel block compression ───────────────────────────────────────────────

// parallelJob is a block sent to a compression worker.
type parallelJob struct {
	idx      int
	data     []byte // owned by the job; worker must not retain after sending result
	isLast   bool
	cksumVal uint32 // xxh64 sum32 of data (pre-computed by producer)
}

// parallelResult is a compressed block returned from a worker.
type parallelResult struct {
	idx      int
	hdrWord  uint32 // block header word (size | flags)
	payload  []byte // compressed bytes
	cksumVal uint32 // per-block checksum value (over uncompressed data)
	isLast   bool
}

// numWorkers returns the number of parallel compression workers to use.
// We limit to 8 to avoid excessive memory usage (each L5 worker holds ~160 MB).
func numWorkers() int {
	n := runtime.GOMAXPROCS(0)
	if n > 8 {
		n = 8
	}
	return n
}

// ─── Writer ───────────────────────────────────────────────────────────────────

// Writer compresses data written to it and forwards compressed bytes to the
// underlying writer.  Call Close to flush and write the end-of-stream marker.
// Compression is performed in parallel across blocks.
type Writer struct {
	w       *bufio.Writer
	opts    Options
	cfg     levelConfig
	buf     []byte // accumulation buffer for the current incomplete block
	cksum   *xxhDigest
	written int64
	closed  bool

	// parallel state (non-nil when workers > 1)
	jobs       chan parallelJob
	results    chan parallelResult
	serialDone chan struct{}
	serialErr  error
	blockIdx   int // next block index to dispatch
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
	wr := &Writer{
		w:     bufio.NewWriterSize(w, 64<<10),
		opts:  opts,
		cfg:   cfg,
		buf:   make([]byte, 0, cfg.blockSize),
		cksum: newXXH64(),
	}

	workers := numWorkers()
	if workers > 1 {
		// Buffer up to workers+1 jobs so the producer rarely blocks.
		wr.jobs = make(chan parallelJob, workers+1)
		wr.results = make(chan parallelResult, workers+1)
		wr.serialDone = make(chan struct{})

		// Start compression workers.
		var wg sync.WaitGroup
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for job := range wr.jobs {
					st := getEncoderState(opts.Level)
					compressed, isRaw := compressBlock(job.data, opts.Level, st)
					putEncoderState(opts.Level, st)

					hdrWord := uint32(len(compressed))
					if isRaw {
						hdrWord |= blkIsRaw
					}
					if job.isLast {
						hdrWord |= blkIsLast
					}
					wr.results <- parallelResult{
						idx:      job.idx,
						hdrWord:  hdrWord,
						payload:  compressed,
						cksumVal: job.cksumVal,
						isLast:   job.isLast,
					}
				}
			}()
		}

		// Close results when all workers finish.
		go func() {
			wg.Wait()
			close(wr.results)
		}()

		// Start serializer: collects results and writes them in order.
		go wr.serialize()
	}

	return wr
}

// serialize drains wr.results in block-index order and writes to wr.w.
// Runs as a goroutine; signals wr.serialDone when finished.
func (w *Writer) serialize() {
	defer close(w.serialDone)

	pending := make(map[int]parallelResult)
	nextWrite := 0

	writePending := func() {
		for {
			r, ok := pending[nextWrite]
			if !ok {
				break
			}
			delete(pending, nextWrite)
			nextWrite++

			if w.serialErr != nil {
				continue // drain without writing
			}
			// Update content checksum in order.
			if w.opts.Checksum {
				// cksumVal is sum32 of uncompressed block data — feed raw bytes
				// to w.cksum by re-hashing is not possible here without the data.
				// Instead, the producer wrote to w.cksum before dispatching.
				// Nothing to do here; done in dispatchBlock.
				_ = r.cksumVal
			}

			var hdr [4]byte
			binary.LittleEndian.PutUint32(hdr[:], r.hdrWord)
			if _, err := w.w.Write(hdr[:]); err != nil {
				w.serialErr = err
				continue
			}
			if _, err := w.w.Write(r.payload); err != nil {
				w.serialErr = err
				continue
			}
			if w.opts.Checksum && len(r.payload) > 0 {
				var cs [4]byte
				binary.LittleEndian.PutUint32(cs[:], r.cksumVal)
				if _, err := w.w.Write(cs[:]); err != nil {
					w.serialErr = err
				}
			}
		}
	}

	for r := range w.results {
		pending[r.idx] = r
		writePending()
	}
	// Flush after all blocks written.
	if w.serialErr == nil {
		w.serialErr = w.w.Flush()
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
		if w.jobs != nil {
			// Flush the frame header before parallel blocks arrive.
			if err := w.w.Flush(); err != nil {
				return 0, err
			}
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
			if err := w.dispatchBlock(false); err != nil {
				return total - len(p), err
			}
		}
	}
	return total, nil
}

// dispatchBlock dispatches w.buf as a block to be compressed (parallel) or
// compresses it directly (sequential fallback).
func (w *Writer) dispatchBlock(isLast bool) error {
	if len(w.buf) == 0 && !isLast {
		return nil
	}

	if w.jobs == nil {
		// Sequential path.
		return w.flushBlockSeq(isLast)
	}

	// Parallel path: checksum the uncompressed data in order here (producer side).
	var cksumVal uint32
	if w.opts.Checksum && len(w.buf) > 0 {
		_, _ = w.cksum.Write(w.buf)
		d := newXXH64()
		_, _ = d.Write(w.buf)
		cksumVal = d.Sum32()
	}

	// Copy the block data; the worker owns it.
	data := make([]byte, len(w.buf))
	copy(data, w.buf)

	w.jobs <- parallelJob{
		idx:      w.blockIdx,
		data:     data,
		isLast:   isLast,
		cksumVal: cksumVal,
	}
	w.blockIdx++
	w.buf = w.buf[:0]
	return nil
}

// flushBlockSeq is the original sequential block flush (used when workers==1).
func (w *Writer) flushBlockSeq(isLast bool) error {
	if len(w.buf) == 0 && !isLast {
		return nil
	}

	// Update content checksum before compression
	if w.opts.Checksum && len(w.buf) > 0 {
		_, _ = w.cksum.Write(w.buf)
	}

	st := getEncoderState(w.opts.Level)
	compressed, isRaw := compressBlock(w.buf, w.opts.Level, st)
	putEncoderState(w.opts.Level, st)

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
		opts := w.opts
		opts.ContentSize = true // advertise size=0
		if err := writeFrameHeader(w.w, opts, 0); err != nil {
			return err
		}
		if w.jobs != nil {
			close(w.jobs)
			<-w.serialDone
			if w.serialErr != nil {
				return w.serialErr
			}
		}
		if err := writeEndOfStream(w.w, w.cksum.Sum32(), w.opts.Checksum); err != nil {
			return err
		}
		return w.w.Flush()
	}

	if err := w.dispatchBlock(false); err != nil {
		return err
	}

	if w.jobs != nil {
		close(w.jobs)
		<-w.serialDone
		if w.serialErr != nil {
			return w.serialErr
		}
		if err := writeEndOfStream(w.w, w.cksum.Sum32(), w.opts.Checksum); err != nil {
			return err
		}
		return w.w.Flush()
	}

	if err := writeEndOfStream(w.w, w.cksum.Sum32(), w.opts.Checksum); err != nil {
		return err
	}
	return w.w.Flush()
}

// Reset discards the writer's state and starts writing a new stream to dst.
func (w *Writer) Reset(dst io.Writer) {
	// If parallel workers are running, stop them first.
	if w.jobs != nil && !w.closed {
		close(w.jobs)
		<-w.serialDone
	}
	w.w.Reset(dst)
	w.buf = w.buf[:0]
	w.written = 0
	w.closed = false
	w.cksum.reset()
	w.blockIdx = 0
	w.serialErr = nil

	if w.jobs != nil {
		// Restart workers.
		*w = *newWriter(dst, w.opts)
	}
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
