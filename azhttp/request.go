package azhttp

import (
	"net/http"
	"strings"

	"github.com/go-again/az"
)

// DecompressRequests returns middleware that transparently decodes request
// bodies sent with Content-Encoding: zstd or lz4 (both are az frames, and az
// detects which from the magic bytes). The handler downstream reads plain
// bytes, and no longer sees the Content-Encoding header.
//
// maxBody caps the *decompressed* body: reads past it fail with az.ErrTooLarge
// rather than letting a small upload expand into a large allocation. Pass 0 for
// no limit, which you should only do for trusted clients.
//
// Bodies with any other Content-Encoding — gzip, say — are passed through
// untouched for something else to handle.
func DecompressRequests(maxBody int64) func(http.Handler) http.Handler {
	return func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			enc := strings.ToLower(strings.TrimSpace(r.Header.Get(contentEncoding)))
			if r.Body == nil || (enc != EncodingZstd && enc != EncodingLZ4) {
				h.ServeHTTP(w, r)
				return
			}

			body := &requestBody{
				reader:  az.NewReader(r.Body),
				limited: maxBody > 0,
				// One byte of slack: reading it is what proves the body is over
				// the limit rather than exactly at it.
				remain: maxBody + 1,
			}
			defer func() { _ = body.Close() }()

			r.Body = body
			r.Header.Del(contentEncoding)
			// The length described the compressed bytes; it no longer describes
			// what the handler will read.
			r.Header.Del(contentLength)
			r.ContentLength = -1

			h.ServeHTTP(w, r)
		})
	}
}

// requestBody is the decompressing http.Request.Body: it decodes az frames and
// enforces the decompressed-size ceiling.
type requestBody struct {
	reader  *az.Reader
	remain  int64 // decompressed bytes still allowed, plus one byte of slack
	limited bool
	closed  bool
}

func (b *requestBody) Read(p []byte) (int, error) {
	if !b.limited {
		return b.reader.Read(p)
	}
	if b.remain <= 0 {
		return 0, az.ErrTooLarge
	}
	if int64(len(p)) > b.remain {
		p = p[:b.remain]
	}
	n, err := b.reader.Read(p)
	b.remain -= int64(n)
	if b.remain <= 0 {
		// We just read the slack byte, so the body is over the limit. Hand back
		// only the bytes that were within it.
		return n - 1, az.ErrTooLarge
	}
	return n, err
}

// Close releases the decoder. The underlying body belongs to net/http, which
// closes it itself, so this does not.
func (b *requestBody) Close() error {
	if b.closed {
		return nil
	}
	b.closed = true
	return b.reader.Close()
}
