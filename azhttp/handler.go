package azhttp

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/go-again/az"
)

// Handler wraps h with the default configuration: compress bodies of at least
// DefaultMinSize bytes whose content type is not already compressed, using zstd
// (az.Level3) or, for clients that ask for it by name, lz4 (az.Level1).
//
// It is the one-liner form of NewWrapper with no options.
func Handler(h http.Handler) http.Handler {
	defaultWrapperOnce.Do(func() {
		wrapper, err := NewWrapper()
		if err != nil {
			// Unreachable: the default config is valid by construction.
			panic("azhttp: default wrapper: " + err.Error())
		}
		defaultWrapper = wrapper
	})
	return defaultWrapper(h)
}

var (
	defaultWrapperOnce sync.Once
	defaultWrapper     func(http.Handler) http.Handler
)

// NewWrapper builds a reusable middleware from opts. Build it once at start-up
// and apply it to as many handlers as you like; the returned function is safe
// for concurrent use and shares its pool of compressors across handlers.
//
// It returns an error if an option is invalid.
func NewWrapper(opts ...Option) (func(http.Handler) http.Handler, error) {
	c := defaultConfig()
	for _, opt := range opts {
		if err := opt(c); err != nil {
			return nil, err
		}
	}
	c.zstdWriters = newWriterPool(c.zstdLevel, c.checksum)
	c.lz4Writers = newWriterPool(c.lz4Level, c.checksum)

	return func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Any response from here varies by Accept-Encoding, including the
			// ones we decide not to compress — a cache must not serve a zstd
			// body to a client that did not ask for one.
			w.Header().Add(vary, acceptEncoding)

			enc := c.selectEncoding(r)
			if enc == "" {
				h.ServeHTTP(&passthroughWriter{ResponseWriter: w}, r)
				w.Header().Del(HeaderNoCompression)
				return
			}

			cw := getResponseWriter(c, enc, w)
			defer func() {
				_ = cw.Close()
				cw.ResponseWriter = nil
				responseWriterPool.Put(cw)
			}()
			h.ServeHTTP(cw, r)
			w.Header().Del(HeaderNoCompression)
		})
	}, nil
}

var responseWriterPool = sync.Pool{New: func() any { return new(responseWriter) }}

// getResponseWriter takes a responseWriter from the pool and initialises it for
// one response, keeping the buffer the pooled value already had.
func getResponseWriter(c *config, enc string, w http.ResponseWriter) *responseWriter {
	cw := responseWriterPool.Get().(*responseWriter)
	*cw = responseWriter{
		ResponseWriter: w,
		cfg:            c,
		enc:            enc,
		buf:            cw.buf[:0],
	}
	return cw
}

// responseWriter buffers the start of a response until it knows whether the
// body is worth compressing, then either starts a compressed stream or replays
// the buffer to the underlying writer and gets out of the way.
type responseWriter struct {
	http.ResponseWriter

	cfg  *config
	enc  string     // negotiated coding: EncodingZstd or EncodingLZ4
	cw   *az.Writer // non-nil once compression started
	buf  []byte     // body bytes held back while deciding
	code int        // status from WriteHeader, 0 until set

	ignore   bool // decided not to compress; writes pass straight through
	hijacked bool // handler took over the connection
}

// Write implements http.ResponseWriter.
func (w *responseWriter) Write(b []byte) (int, error) {
	if w.cw != nil {
		return w.cw.Write(b)
	}
	if w.ignore {
		return w.ResponseWriter.Write(b)
	}

	// Hold back the first bytes: they decide whether we compress, and they are
	// also what Content-Type sniffing needs.
	want := max(w.cfg.minSize, 512)
	add := min(len(b), want-len(w.buf))
	w.buf = append(w.buf, b[:add]...)
	remain := b[add:]

	if w.headersAllowCompression() {
		// Nothing buffered means there is nothing to act on yet (b was empty).
		// Committing to a coding here would put a Content-Encoding on a body
		// that may never arrive.
		if len(w.buf) == 0 || !w.enoughToDecide() {
			// Not enough bytes yet and no Content-Length to go on: keep waiting.
			return len(b), nil
		}
		// Now that the body is worth compressing, its type gets the last word.
		if w.cfg.contentTypeFilter(w.detectContentType()) {
			if err := w.startCompress(); err != nil {
				return 0, err
			}
			if len(remain) > 0 {
				if _, err := w.cw.Write(remain); err != nil {
					return 0, err
				}
			}
			return len(b), nil
		}
	}

	if err := w.startPlain(); err != nil {
		return 0, err
	}
	if len(remain) > 0 {
		if _, err := w.ResponseWriter.Write(remain); err != nil {
			return 0, err
		}
	}
	return len(b), nil
}

// enoughToDecide reports whether enough of the body is in hand to commit to
// compressing it: either the buffer reached MinSize, or the handler declared a
// Content-Length that did.
func (w *responseWriter) enoughToDecide() bool {
	if len(w.buf) >= w.cfg.minSize {
		return true
	}
	cl, err := strconv.Atoi(w.Header().Get(contentLength))
	return err == nil && cl >= w.cfg.minSize
}

// headersAllowCompression reports whether the headers set so far leave
// compression on the table. It deliberately does not sniff the body: an unset
// Content-Type is only worth guessing at once we have bytes to guess from.
func (w *responseWriter) headersAllowCompression() bool {
	hdr := w.Header()
	if len(hdr[HeaderNoCompression]) != 0 {
		return false
	}
	// The handler already encoded the body itself, or is serving a byte range.
	if hdr.Get(contentEncoding) != "" || hdr.Get(contentRange) != "" {
		return false
	}
	// A declared length below MinSize settles it: no amount of buffering will
	// make this body big enough.
	if cl, err := strconv.Atoi(hdr.Get(contentLength)); err == nil && cl < w.cfg.minSize {
		return false
	}
	if ct := hdr.Get(contentType); ct != "" && !w.cfg.contentTypeFilter(ct) {
		return false
	}
	return true
}

// detectContentType returns the response Content-Type, sniffing it from the
// buffered body (and setting it, unless SetContentType is off) when the handler
// did not set one — which is what net/http itself would do.
func (w *responseWriter) detectContentType() string {
	hdr := w.Header()
	if ct := hdr.Get(contentType); ct != "" {
		return ct
	}
	if len(w.buf) == 0 || !bodyAllowedForStatus(w.code) {
		return ""
	}
	ct := http.DetectContentType(w.buf)
	if _, ok := hdr[contentType]; w.cfg.setContentType && !ok {
		hdr.Set(contentType, ct)
	}
	return ct
}

// startCompress commits to a compressed response: it fixes up the headers,
// sends them, and pushes whatever is buffered into a fresh compressor.
func (w *responseWriter) startCompress() error {
	hdr := w.Header()
	hdr.Set(contentEncoding, w.enc)
	hdr.Del(HeaderNoCompression)

	// The handler's length describes the identity body, and net/http would
	// refuse to change it later (golang/go#14975), so it has to go.
	hdr.Del(contentLength)
	if !w.cfg.keepAcceptRanges {
		hdr.Del(acceptRanges)
	}
	switch {
	case w.cfg.dropETag:
		hdr.Del(eTag)
	case w.cfg.suffixETag != "" && hdr.Get(eTag) != "":
		hdr.Set(eTag, suffixETag(hdr.Get(eTag), w.cfg.suffixETag, w.enc))
	}

	if w.code != 0 {
		w.ResponseWriter.WriteHeader(w.code)
		w.code = 0 // don't send it twice
	}

	// Callers only get here with a non-empty buffer, so the coding announced
	// above always has a frame behind it.
	w.cw = writerFor(w.cfg, w.enc, w.ResponseWriter)
	_, err := w.cw.Write(w.buf)
	w.buf = w.buf[:0]
	return err
}

// startPlain gives up on compressing this response and replays the buffer.
func (w *responseWriter) startPlain() error {
	w.Header().Del(HeaderNoCompression)
	if w.code != 0 {
		w.ResponseWriter.WriteHeader(w.code)
		w.code = 0
	}
	w.ignore = true
	if len(w.buf) == 0 {
		return nil
	}
	n, err := w.ResponseWriter.Write(w.buf)
	if err == nil && n < len(w.buf) {
		err = io.ErrShortWrite
	}
	w.buf = w.buf[:0]
	return err
}

// WriteHeader holds the status code until the compress-or-not decision is made,
// so the headers can still be adjusted. Informational (1xx) responses are
// forwarded immediately: they precede the real response and carry no body.
func (w *responseWriter) WriteHeader(code int) {
	if code >= 100 && code <= 199 {
		w.ResponseWriter.WriteHeader(code)
		return
	}
	if w.code == 0 {
		w.code = code
	}
}

// Flush implements http.Flusher. Data buffered for the MinSize decision forces
// that decision: a client waiting on a flush cannot also be waiting for us to
// see whether more bytes show up.
func (w *responseWriter) Flush() {
	if w.hijacked {
		return
	}
	if w.cw == nil && !w.ignore {
		if len(w.buf) == 0 {
			return // nothing written yet; nothing to flush
		}
		if w.headersAllowCompression() && w.cfg.contentTypeFilter(w.detectContentType()) {
			if err := w.startCompress(); err != nil {
				return
			}
		} else if err := w.startPlain(); err != nil {
			return
		}
	}
	if w.cw != nil {
		if err := w.cw.Flush(); err != nil {
			return
		}
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Close finishes the response: it ends the compressed stream, or writes out a
// body that turned out to be too small or too compressed to bother with. The
// wrapper calls it when the handler returns.
func (w *responseWriter) Close() error {
	if w.hijacked || w.ignore {
		return nil
	}
	if w.cw == nil {
		// The handler is done, so the buffer is the whole body; compress it
		// only if there is one and it is big enough to be worth it.
		if len(w.buf) == 0 ||
			len(w.buf) < w.cfg.minSize ||
			!w.headersAllowCompression() ||
			!w.cfg.contentTypeFilter(w.detectContentType()) {
			return w.startPlain()
		}
		if err := w.startCompress(); err != nil {
			return err
		}
	}
	if w.cw == nil {
		return nil
	}
	err := w.cw.Close()
	putWriter(w.cfg, w.enc, w.cw)
	w.cw = nil
	return err
}

// Hijack implements http.Hijacker for handlers that take over the connection
// (WebSocket upgrades, say). Anything buffered is dropped: from here on the
// handler owns the wire, and a compressed frame written under it would be
// framing garbage.
func (w *responseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("azhttp: underlying ResponseWriter is not an http.Hijacker")
	}
	conn, rw, err := hj.Hijack()
	if err == nil {
		w.hijacked = true
		w.buf = w.buf[:0]
	}
	return conn, rw, err
}

// Unwrap exposes the wrapped writer to http.ResponseController, so handlers can
// still set deadlines and reach other capabilities of the real writer.
func (w *responseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

var (
	_ http.Flusher  = (*responseWriter)(nil)
	_ http.Hijacker = (*responseWriter)(nil)
)

// suffixETag inserts suffix (plus the coding) inside an ETag's quotes, keeping
// any weak-validator prefix intact: `W/"abc"` becomes `W/"abc-zstd"`.
func suffixETag(tag, suffix, enc string) string {
	if !strings.Contains(suffix, enc) {
		suffix += "-" + enc
	}
	insert := strings.LastIndex(tag, `"`)
	if insert == -1 {
		insert = len(tag)
	}
	return tag[:insert] + suffix + tag[insert:]
}

// ─── Compressor pool ──────────────────────────────────────────────────────────

// writerPool recycles az.Writers configured for one coding. The writers are
// built with concurrency 1: a server is already parallel across requests, and
// only a single-threaded lz4 writer can be reused after Close.
type writerPool struct {
	pool sync.Pool
	opts []az.Option
}

func newWriterPool(level az.Level, checksum bool) *writerPool {
	return &writerPool{opts: []az.Option{
		az.WithLevel(level),
		az.WithChecksum(checksum),
		az.WithConcurrency(1),
	}}
}

func (p *writerPool) get(w io.Writer) *az.Writer {
	if v := p.pool.Get(); v != nil {
		cw := v.(*az.Writer)
		cw.Reset(w)
		return cw
	}
	return az.NewWriter(w, p.opts...)
}

// put returns a closed writer to the pool, first pointing it away from the
// response so a pooled writer cannot pin a finished request's memory.
func (p *writerPool) put(cw *az.Writer) {
	cw.Reset(io.Discard)
	p.pool.Put(cw)
}

func poolFor(c *config, enc string) *writerPool {
	if enc == EncodingLZ4 {
		return c.lz4Writers
	}
	return c.zstdWriters
}

func writerFor(c *config, enc string, w io.Writer) *az.Writer { return poolFor(c, enc).get(w) }

func putWriter(c *config, enc string, cw *az.Writer) { poolFor(c, enc).put(cw) }

// ─── Pass-through writer ──────────────────────────────────────────────────────

// passthroughWriter wraps the response for requests we will not compress. It
// exists only to strip HeaderNoCompression, which is an instruction to this
// middleware and has no business reaching the client.
type passthroughWriter struct {
	http.ResponseWriter
}

func (w *passthroughWriter) WriteHeader(code int) {
	w.Header().Del(HeaderNoCompression)
	w.ResponseWriter.WriteHeader(code)
}

func (w *passthroughWriter) Write(b []byte) (int, error) {
	w.Header().Del(HeaderNoCompression)
	return w.ResponseWriter.Write(b)
}

func (w *passthroughWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *passthroughWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := w.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, errors.New("azhttp: underlying ResponseWriter is not an http.Hijacker")
}

func (w *passthroughWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *passthroughWriter) ReadFrom(r io.Reader) (int64, error) {
	w.Header().Del(HeaderNoCompression)
	if rf, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		return rf.ReadFrom(r)
	}
	return io.Copy(w.ResponseWriter, r)
}
