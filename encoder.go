package az

import (
	"bytes"
	"encoding/binary"
	"io"

	lz4pkg "github.com/go-again/az/internal/lz4"
	zstdpkg "github.com/go-again/az/internal/zstd"
)

// ─── Poolable one-shot Encoder ─────────────────────────────────────────────────

// Encoder reuses its internal lz4/zstd writers across calls so that
// high-throughput callers compressing many independent buffers avoid
// constructing a fresh, heavyweight codec per call. The frames it produces are
// byte-identical to Compress at the same level, so encoded data is at-rest
// compatible with Compress/Decompress output.
//
// An Encoder is NOT safe for concurrent use; pool one per goroutine.
type Encoder struct {
	buf     bytes.Buffer
	lz4w    *lz4pkg.Writer
	zstdEnc map[Level]*zstdpkg.Encoder // lazily created, one per zstd level
}

// NewEncoder returns a reusable Encoder.
func NewEncoder() *Encoder {
	return &Encoder{zstdEnc: make(map[Level]*zstdpkg.Encoder)}
}

// EncodeAll compresses src at level and appends the resulting frame to dst,
// returning the extended slice (a nil dst allocates a fresh slice). The appended
// bytes are byte-identical to Compress(src, level). It returns ErrLevel for an
// out-of-range level.
func (e *Encoder) EncodeAll(dst, src []byte, level Level) ([]byte, error) {
	if level < minLevel || level > maxLevel {
		return nil, ErrLevel
	}
	e.buf.Reset()

	if level <= Level2 {
		// Mirror Compress's lz4 path exactly. The option set (and SizeOption in
		// particular) is what guarantees byte-identity with Compress.
		if e.lz4w == nil {
			e.lz4w = lz4pkg.NewWriter(&e.buf)
		}
		e.lz4w.Reset(&e.buf) // back to newState so Apply is accepted
		// ConcurrencyOption(1) is deliberate: the concurrent lz4 writer cannot be
		// Reset-reused after Close (its block-manager goroutine exits, so Reset's
		// Blocks.close deadlocks on a channel send). Concurrency only parallelises
		// block compression; the emitted frame bytes are identical, so this stays
		// byte-for-byte compatible with Compress (verified by the format-identity
		// test).
		if err := e.lz4w.Apply(
			lz4pkg.CompressionLevelOption(lz4Level[level]),
			lz4pkg.ChecksumOption(true),
			lz4pkg.SizeOption(uint64(len(src))),
			lz4pkg.ConcurrencyOption(1),
		); err != nil {
			return nil, err
		}
		if _, err := e.lz4w.Write(src); err != nil {
			return nil, err
		}
		if err := e.lz4w.Close(); err != nil {
			return nil, err
		}
		return append(dst, e.buf.Bytes()...), nil
	}

	// Mirror Compress's zstd path exactly, reusing the per-level encoder.
	enc := e.zstdEnc[level]
	if enc == nil {
		var err error
		enc, err = zstdpkg.NewWriter(nil,
			zstdpkg.WithEncoderLevel(zstdLevel[level]),
			zstdpkg.WithEncoderCRC(true),
		)
		if err != nil {
			return nil, err
		}
		e.zstdEnc[level] = enc
	}
	// ResetContentSize (not Reset) declares the known input length, so the frame
	// header always carries Frame_Content_Size: without it the streaming encoder
	// writes the header before it knows the total, and inputs larger than one
	// block (and empty ones) end up with no size recorded. Decoders that
	// single-shot via the header — alloc(FCS) then decompress — need it on every
	// frame. Compress does the same thing, keeping the two byte-identical.
	enc.ResetContentSize(&e.buf, int64(len(src)))
	if _, err := enc.Write(src); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return append(dst, e.buf.Bytes()...), nil
}

// ─── Poolable one-shot Decoder ─────────────────────────────────────────────────

// Decoder reuses its internal lz4 reader and zstd decoder across calls,
// auto-detecting the stream format the same way Reader does. The bytes it
// produces equal Decompress's output for the same frame.
//
// A Decoder is NOT safe for concurrent use; pool one per goroutine.
type Decoder struct {
	lz4r    *lz4pkg.Reader
	br      *bytes.Reader // reused source for the lz4 path
	zstdDec *zstdpkg.Decoder
}

// NewDecoder returns a reusable Decoder.
func NewDecoder() *Decoder {
	return &Decoder{}
}

// DecodeAll decompresses src (auto-detecting lz4 vs zstd, as Reader does) and
// appends the result to dst, returning the extended slice (a nil dst allocates).
// The appended bytes equal Decompress(src). It returns the same corruption
// errors as Reader.
func (d *Decoder) DecodeAll(dst, src []byte) ([]byte, error) {
	// Mirror Reader.init: a stream too short to hold the 4-byte magic decodes to
	// nothing without error (Peek(4) yields io.EOF, which ReadAll treats as a
	// clean end).
	if len(src) < 4 {
		return dst, nil
	}

	switch binary.LittleEndian.Uint32(src[:4]) {
	case magicLZ4:
		return readAllAppend(dst, d.lz4Reader(src))

	case magicZstd:
		dec, err := d.zstdDecoder()
		if err != nil {
			return nil, err
		}
		return dec.DecodeAll(src, dst)

	default:
		return nil, ErrCorrupted
	}
}

// DecodeAllLimit is DecodeAll with a hard ceiling: it decompresses src and
// appends to dst, but stops and returns ErrTooLarge as soon as the output would
// exceed max bytes — without allocating materially beyond max. It reuses the
// Decoder's internal codecs exactly like DecodeAll, so it is suitable for
// decoding untrusted frames (a decompression-bomb defense) from a pool.
//
// For a well-formed frame whose output is ≤ max, the result is identical to
// DecodeAll(dst, src). For larger output it returns ErrTooLarge having buffered
// no more than max+1 bytes of output; corrupt frames return the same errors as
// Reader.
func (d *Decoder) DecodeAllLimit(dst, src []byte, max int) ([]byte, error) {
	if len(src) < 4 {
		return dst, nil
	}

	switch binary.LittleEndian.Uint32(src[:4]) {
	case magicLZ4:
		// The lz4 reader is single-threaded (ConcurrencyOption(1)), so stopping
		// early can't leak block-decode goroutines.
		return readLimitAppend(dst, d.lz4Reader(src), max)

	case magicZstd:
		dec, err := d.zstdDecoder()
		if err != nil {
			return nil, err
		}
		// Stream block-by-block rather than one-shot DecodeAll, so a bomb is
		// capped at max+1 output bytes instead of fully materialized. A
		// *bytes.Reader is not a zstd "byter" (no Bytes method), so Reset streams
		// instead of synchronously decoding the whole frame.
		d.resetBR(src)
		if err := dec.Reset(d.br); err != nil {
			return nil, ErrCorrupted
		}
		out, err := readLimitAppend(dst, dec, max)
		if err == ErrTooLarge {
			// We stopped before EOF, so the stream decoder may still be running;
			// Reset(nil) cancels it and reclaims its block decoders.
			_ = dec.Reset(nil)
		}
		return out, err

	default:
		return nil, ErrCorrupted
	}
}

// resetBR points the reusable bytes.Reader at src.
func (d *Decoder) resetBR(src []byte) {
	if d.br == nil {
		d.br = bytes.NewReader(src)
	} else {
		d.br.Reset(src)
	}
}

// lz4Reader returns the reusable lz4 reader pointed at src. It is created with
// ConcurrencyOption(1): single-threaded decode spawns no goroutines (so a
// bounded decode that stops early can't leak any) and the output bytes are
// identical regardless of concurrency.
func (d *Decoder) lz4Reader(src []byte) *lz4pkg.Reader {
	d.resetBR(src)
	if d.lz4r == nil {
		d.lz4r = lz4pkg.NewReader(d.br)
		_ = d.lz4r.Apply(lz4pkg.ConcurrencyOption(1))
	} else {
		d.lz4r.Reset(d.br)
	}
	return d.lz4r
}

// zstdDecoder returns the reusable zstd decoder, creating it lazily.
func (d *Decoder) zstdDecoder() (*zstdpkg.Decoder, error) {
	if d.zstdDec == nil {
		dec, err := zstdpkg.NewReader(nil)
		if err != nil {
			return nil, ErrCorrupted
		}
		d.zstdDec = dec
	}
	return d.zstdDec, nil
}

// readAllAppend reads everything from r, appending into dst and growing as
// needed. It mirrors io.ReadAll but reuses the caller's backing array.
func readAllAppend(dst []byte, r io.Reader) ([]byte, error) {
	for {
		if len(dst) == cap(dst) {
			dst = append(dst, 0)[:len(dst)] // grow capacity, keep length
		}
		n, err := r.Read(dst[len(dst):cap(dst)])
		dst = dst[:len(dst)+n]
		if err != nil {
			if err == io.EOF {
				return dst, nil
			}
			return dst, err
		}
	}
}

// readLimitAppend reads from r, appending into dst, but never buffers more than
// max output bytes: it reads one sentinel byte past max to detect overflow and
// returns ErrTooLarge (with dst truncated to base+max) the moment the output
// would exceed max. The backing array never grows beyond base+max+1, so a
// decompression bomb cannot force a large allocation here.
func readLimitAppend(dst []byte, r io.Reader, max int) ([]byte, error) {
	base := len(dst)
	hardCap := base + max + 1 // room for one byte beyond the allowed max
	for {
		end := min(cap(dst), hardCap)
		if len(dst) == end {
			// No room left within hardCap; grow, capped at hardCap.
			newCap := cap(dst) * 2
			if newCap < base+512 {
				newCap = base + 512
			}
			newCap = min(newCap, hardCap)
			buf := make([]byte, len(dst), newCap)
			copy(buf, dst)
			dst = buf
			end = min(cap(dst), hardCap)
		}
		n, err := r.Read(dst[len(dst):end])
		dst = dst[:len(dst)+n]
		if len(dst) == hardCap {
			// Buffered max+1 output bytes → the output exceeds the limit.
			return dst[:base+max], ErrTooLarge
		}
		if err != nil {
			if err == io.EOF {
				return dst, nil
			}
			return dst, err
		}
	}
}
