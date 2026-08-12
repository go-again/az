package azhttp

import (
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/go-again/az"
)

// Transport wraps an http.RoundTripper so outgoing requests advertise the
// codings azhttp can decode and matching responses are decompressed on the fly.
//
//	client := &http.Client{Transport: azhttp.Transport(http.DefaultTransport)}
//
// By default it asks for "zstd" only, which any azhttp server (and plenty of
// others) can serve. Add TransportLZ4 when talking to an azhttp server inside
// your own network and you would rather spend bandwidth than CPU.
//
// It leaves a request that already carries an Accept-Encoding header alone, and
// like net/http it does not ask for compression on HEAD or ranged requests. A
// decompressed response has its Content-Encoding and Content-Length stripped,
// ContentLength set to -1, and Uncompressed set to true, exactly as the
// standard library does for the gzip it handles itself.
//
// A nil parent means http.DefaultTransport.
func Transport(parent http.RoundTripper, opts ...TransportOption) http.RoundTripper {
	if parent == nil {
		parent = http.DefaultTransport
	}
	t := &transport{parent: parent, zstd: true}
	for _, opt := range opts {
		opt(t)
	}
	var codings []string
	if t.zstd {
		codings = append(codings, EncodingZstd)
	}
	if t.lz4 {
		if t.preferLZ4 {
			codings = append([]string{EncodingLZ4}, codings...)
			// Rank zstd below lz4 so a server that speaks both hands us the
			// cheap one; without a q-value the server would break the tie.
			for i, c := range codings {
				if c == EncodingZstd {
					codings[i] = EncodingZstd + ";q=0.9"
				}
			}
		} else {
			codings = append(codings, EncodingLZ4)
		}
	}
	t.acceptEncoding = strings.Join(codings, ", ")
	return t
}

// A TransportOption configures the client-side round tripper.
type TransportOption func(*transport)

// TransportZstd controls whether "zstd" is requested. On by default.
func TransportZstd(enable bool) TransportOption {
	return func(t *transport) { t.zstd = enable }
}

// TransportLZ4 controls whether the non-standard "lz4" coding is requested.
// Off by default; only azhttp servers (and other lz4-aware peers) can serve it.
func TransportLZ4(enable bool) TransportOption {
	return func(t *transport) { t.lz4 = enable }
}

// TransportPreferLZ4 asks the server to prefer lz4 over zstd when it can serve
// both, by ranking zstd lower. Implies TransportLZ4(true).
func TransportPreferLZ4(prefer bool) TransportOption {
	return func(t *transport) {
		t.preferLZ4 = prefer
		if prefer {
			t.lz4 = true
		}
	}
}

// TransportAlwaysDecompress decodes az codings on any response, even one whose
// compression this transport did not request (a server that compresses
// unconditionally, say). Off by default, so a response the caller may want
// verbatim is passed through.
func TransportAlwaysDecompress(enable bool) TransportOption {
	return func(t *transport) { t.alwaysDecompress = enable }
}

type transport struct {
	parent           http.RoundTripper
	acceptEncoding   string
	zstd, lz4        bool
	preferLZ4        bool
	alwaysDecompress bool
}

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	requested := false
	if t.acceptEncoding != "" &&
		req.Header.Get(acceptEncoding) == "" &&
		req.Header.Get("Range") == "" &&
		req.Method != http.MethodHead {
		// Cloning keeps RoundTrip free of side effects on the caller's request,
		// which http.RoundTripper requires.
		req = req.Clone(req.Context())
		req.Header.Set(acceptEncoding, t.acceptEncoding)
		requested = true
	}

	resp, err := t.parent.RoundTrip(req)
	if err != nil || (!requested && !t.alwaysDecompress) {
		return resp, err
	}

	switch strings.ToLower(strings.TrimSpace(resp.Header.Get(contentEncoding))) {
	case EncodingZstd:
		if !t.zstd && !t.alwaysDecompress {
			return resp, nil
		}
	case EncodingLZ4:
		if !t.lz4 && !t.alwaysDecompress {
			return resp, nil
		}
	default:
		return resp, nil
	}

	resp.Body = newResponseBody(resp.Body)
	resp.Header.Del(contentEncoding)
	resp.Header.Del(contentLength)
	resp.ContentLength = -1
	resp.Uncompressed = true
	return resp, nil
}

// responseBodyPool recycles the decoding wrappers (and their 64 KiB read
// buffers) across responses.
var responseBodyPool sync.Pool

// responseBody decodes an az-compressed response body. The az.Reader is created
// on the first Read so that a caller who closes a response without reading it
// pays nothing.
type responseBody struct {
	src    io.ReadCloser
	reader *az.Reader
}

func newResponseBody(src io.ReadCloser) *responseBody {
	if v := responseBodyPool.Get(); v != nil {
		b := v.(*responseBody)
		b.src = src
		if b.reader != nil {
			b.reader.Reset(src)
		}
		return b
	}
	return &responseBody{src: src}
}

func (b *responseBody) Read(p []byte) (int, error) {
	if b.src == nil {
		return 0, io.EOF // closed
	}
	if b.reader == nil {
		b.reader = az.NewReader(b.src)
	}
	return b.reader.Read(p)
}

func (b *responseBody) Close() error {
	if b.src == nil {
		return nil
	}
	if b.reader != nil {
		_ = b.reader.Close()
		// Point the reader at nothing before pooling it, so a pooled body
		// cannot pin this response's connection.
		b.reader.Reset(nil)
	}
	err := b.src.Close()
	b.src = nil
	responseBodyPool.Put(b)
	return err
}
