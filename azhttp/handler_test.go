package azhttp

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-again/az"
)

// body returns compressible test data of n bytes.
func body(n int) []byte {
	const pattern = "the quick brown fox jumps over the lazy dog\n"
	out := make([]byte, n)
	for i := range out {
		out[i] = pattern[i%len(pattern)]
	}
	return out
}

// echoHandler writes the given payload, optionally after setting headers.
func echoHandler(payload []byte, headers map[string]string, status int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		if status != 0 {
			w.WriteHeader(status)
		}
		w.Write(payload)
	})
}

// serve runs one request through a wrapper built with opts.
func serve(t *testing.T, h http.Handler, req *http.Request, opts ...Option) *http.Response {
	t.Helper()
	wrapper, err := NewWrapper(opts...)
	if err != nil {
		t.Fatalf("NewWrapper: %v", err)
	}
	rec := httptest.NewRecorder()
	wrapper(h).ServeHTTP(rec, req)
	return rec.Result()
}

// get builds a GET request with the given Accept-Encoding.
func get(acceptEnc string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if acceptEnc != "" {
		r.Header.Set("Accept-Encoding", acceptEnc)
	}
	return r
}

// decodeBody reads a response body, decompressing it when it is an az coding.
func decodeBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	switch resp.Header.Get("Content-Encoding") {
	case "":
		return raw
	case EncodingZstd, EncodingLZ4:
		plain, err := az.Decompress(raw)
		if err != nil {
			t.Fatalf("decompress %s body: %v", resp.Header.Get("Content-Encoding"), err)
		}
		return plain
	default:
		t.Fatalf("unexpected Content-Encoding %q", resp.Header.Get("Content-Encoding"))
		return nil
	}
}

// ─── Round-trip ───────────────────────────────────────────────────────────────

func TestCompressesAndRoundTrips(t *testing.T) {
	want := body(64 << 10)
	for _, tc := range []struct {
		name       string
		acceptEnc  string
		wantCoding string
	}{
		{"zstd", "zstd", EncodingZstd},
		{"lz4", "lz4", EncodingLZ4},
		{"both_prefers_zstd", "lz4, zstd", EncodingZstd},
		{"qvalue_picks_lz4", "zstd;q=0.5, lz4;q=0.9", EncodingLZ4},
		{"wildcard_picks_zstd", "*", EncodingZstd},
		{"gzip_only", "gzip", ""},
		{"none", "", ""},
		{"zstd_disabled_by_qvalue", "zstd;q=0", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := serve(t, echoHandler(want, nil, 0), get(tc.acceptEnc))

			if got := resp.Header.Get("Content-Encoding"); got != tc.wantCoding {
				t.Fatalf("Content-Encoding = %q, want %q", got, tc.wantCoding)
			}
			if got := decodeBody(t, resp); !bytes.Equal(got, want) {
				t.Fatalf("body round-trip mismatch: got %d bytes, want %d", len(got), len(want))
			}
			if vary := resp.Header.Get("Vary"); vary != "Accept-Encoding" {
				t.Errorf("Vary = %q, want Accept-Encoding (needed even uncompressed)", vary)
			}
			if tc.wantCoding != "" {
				if resp.Header.Get("Content-Length") != "" {
					t.Error("Content-Length must not describe a compressed body")
				}
				raw, _ := io.ReadAll(httptest.NewRecorder().Result().Body)
				_ = raw
			}
		})
	}
}

func TestCompressedBodyIsSmaller(t *testing.T) {
	want := body(64 << 10)
	resp := serve(t, echoHandler(want, nil, 0), get("zstd"))
	raw, _ := io.ReadAll(resp.Body)
	if len(raw) >= len(want) {
		t.Fatalf("compressed body (%d) is not smaller than input (%d)", len(raw), len(want))
	}
}

// ─── When not to compress ─────────────────────────────────────────────────────

func TestPassthrough(t *testing.T) {
	small := body(100)
	big := body(64 << 10)

	for _, tc := range []struct {
		name    string
		handler http.Handler
		req     *http.Request
		// wantCE is the Content-Encoding the client should see: empty unless
		// the handler set one of its own, which we must not touch.
		wantCE string
	}{
		{"below_min_size", echoHandler(small, nil, 0), get("zstd"), ""},
		{
			"declared_length_below_min_size",
			echoHandler(big, map[string]string{"Content-Length": "100"}, 0),
			get("zstd"),
			"",
		},
		{
			"already_encoded",
			echoHandler(big, map[string]string{"Content-Encoding": "br"}, 0),
			get("zstd"),
			"br",
		},
		{
			"byte_range_response",
			echoHandler(big, map[string]string{"Content-Range": "bytes 0-100/1000"}, 0),
			get("zstd"),
			"",
		},
		{
			"compressed_content_type",
			echoHandler(big, map[string]string{"Content-Type": "image/png"}, 0),
			get("zstd"),
			"",
		},
		{
			"handler_opted_out",
			echoHandler(big, map[string]string{HeaderNoCompression: "1"}, 0),
			get("zstd"),
			"",
		},
		{"head_request", echoHandler(big, nil, 0), httptest.NewRequest(http.MethodHead, "/", nil), ""},
		{"range_request", echoHandler(big, nil, 0), func() *http.Request {
			r := get("zstd")
			r.Header.Set("Range", "bytes=0-99")
			return r
		}(), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "head_request" {
				tc.req.Header.Set("Accept-Encoding", "zstd")
			}
			resp := serve(t, tc.handler, tc.req)
			if got := resp.Header.Get("Content-Encoding"); got != tc.wantCE {
				t.Fatalf("Content-Encoding = %q, want %q", got, tc.wantCE)
			}
			if got := resp.Header.Get(HeaderNoCompression); got != "" {
				t.Errorf("%s leaked to the client: %q", HeaderNoCompression, got)
			}
			// The body must reach the client byte for byte.
			raw, _ := io.ReadAll(resp.Body)
			want := big
			if tc.name == "below_min_size" {
				want = small
			}
			if !bytes.Equal(raw, want) {
				t.Fatalf("body was rewritten: got %d bytes, want %d", len(raw), len(want))
			}
		})
	}
}

func TestNoCompressionHeaderStrippedOnUncompressedPath(t *testing.T) {
	h := echoHandler(body(64<<10), map[string]string{HeaderNoCompression: "1"}, 0)
	resp := serve(t, h, get("gzip")) // no az coding accepted → passthrough writer
	if got := resp.Header.Get(HeaderNoCompression); got != "" {
		t.Fatalf("%s leaked to the client: %q", HeaderNoCompression, got)
	}
}

func TestLZ4NeverInferredFromWildcard(t *testing.T) {
	resp := serve(t, echoHandler(body(64<<10), nil, 0), get("*"), EnableZstd(false))
	if got := resp.Header.Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q; lz4 is non-standard and must not be inferred from *", got)
	}
}

func TestPreferLZ4OnTie(t *testing.T) {
	resp := serve(t, echoHandler(body(64<<10), nil, 0), get("zstd, lz4"), PreferLZ4(true))
	if got := resp.Header.Get("Content-Encoding"); got != EncodingLZ4 {
		t.Fatalf("Content-Encoding = %q, want %q", got, EncodingLZ4)
	}
}

// ─── Headers ──────────────────────────────────────────────────────────────────

func TestHeaderHandling(t *testing.T) {
	big := body(64 << 10)

	t.Run("accept_ranges_dropped", func(t *testing.T) {
		h := echoHandler(big, map[string]string{"Accept-Ranges": "bytes"}, 0)
		resp := serve(t, h, get("zstd"))
		if got := resp.Header.Get("Accept-Ranges"); got != "" {
			t.Fatalf("Accept-Ranges = %q, want it dropped on a compressed body", got)
		}
	})

	t.Run("accept_ranges_kept", func(t *testing.T) {
		h := echoHandler(big, map[string]string{"Accept-Ranges": "bytes"}, 0)
		resp := serve(t, h, get("zstd"), KeepAcceptRanges())
		if got := resp.Header.Get("Accept-Ranges"); got != "bytes" {
			t.Fatalf("Accept-Ranges = %q, want bytes", got)
		}
	})

	t.Run("etag_suffixed", func(t *testing.T) {
		h := echoHandler(big, map[string]string{"Etag": `W/"abc"`}, 0)
		resp := serve(t, h, get("zstd"), SuffixETag("-az"))
		if got, want := resp.Header.Get("Etag"), `W/"abc-az-zstd"`; got != want {
			t.Fatalf("Etag = %q, want %q", got, want)
		}
	})

	t.Run("etag_dropped", func(t *testing.T) {
		h := echoHandler(big, map[string]string{"Etag": `"abc"`}, 0)
		resp := serve(t, h, get("zstd"), DropETag())
		if got := resp.Header.Get("Etag"); got != "" {
			t.Fatalf("Etag = %q, want it dropped", got)
		}
	})

	t.Run("status_code_preserved", func(t *testing.T) {
		h := echoHandler(big, nil, http.StatusNotFound)
		resp := serve(t, h, get("zstd"))
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
		if got := resp.Header.Get("Content-Encoding"); got != EncodingZstd {
			t.Fatalf("Content-Encoding = %q, want zstd", got)
		}
	})

	t.Run("content_type_sniffed", func(t *testing.T) {
		h := echoHandler([]byte(strings.Repeat("<html><body>hi</body></html>", 100)), nil, 0)
		resp := serve(t, h, get("zstd"))
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Fatalf("Content-Type = %q, want it sniffed as text/html", ct)
		}
	})

	t.Run("content_type_sniffing_disabled", func(t *testing.T) {
		// A plain writer, not httptest.ResponseRecorder: the recorder sniffs
		// Content-Type itself on first write (as net/http does), which would
		// hide whether the middleware set it.
		wrapper, err := NewWrapper(SetContentType(false))
		if err != nil {
			t.Fatal(err)
		}
		rw := &plainWriter{header: http.Header{}}
		wrapper(echoHandler(big, nil, 0)).ServeHTTP(rw, get("zstd"))
		if ct := rw.header.Get("Content-Type"); ct != "" {
			t.Fatalf("Content-Type = %q, want the middleware to leave it unset", ct)
		}
	})
}

// ─── Streaming ────────────────────────────────────────────────────────────────

// TestFlushStartsCompressionEarly checks that a handler that flushes below
// MinSize still gets its bytes out — a client waiting on a flush cannot also be
// waiting for the buffer to fill.
func TestFlushStartsCompressionEarly(t *testing.T) {
	const chunks = 20
	release := make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		for i := range chunks {
			fmt.Fprintf(w, "chunk %02d\n", i)
			w.(http.Flusher).Flush()
			if i == 0 {
				<-release // the first chunk must reach the wire on its own
			}
		}
	})

	wrapper, err := NewWrapper()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(wrapper(h))
	defer srv.Close()

	client := &http.Client{Transport: Transport(srv.Client().Transport)}
	respCh := make(chan *http.Response, 1)
	go func() {
		resp, err := client.Get(srv.URL)
		if err != nil {
			t.Error(err)
			close(respCh)
			return
		}
		respCh <- resp
	}()

	resp, ok := <-respCh
	if !ok {
		t.Fatal("request failed")
	}
	defer resp.Body.Close()

	// Headers are in, which means the first flush made it through the codec.
	got := make([]byte, len("chunk 00\n"))
	if _, err := io.ReadFull(resp.Body, got); err != nil {
		t.Fatalf("read first flushed chunk: %v", err)
	}
	if string(got) != "chunk 00\n" {
		t.Fatalf("first chunk = %q, want %q", got, "chunk 00\n")
	}
	close(release)

	rest, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read rest: %v", err)
	}
	var want strings.Builder
	for i := 1; i < chunks; i++ {
		fmt.Fprintf(&want, "chunk %02d\n", i)
	}
	if string(rest) != want.String() {
		t.Fatalf("streamed body mismatch:\ngot  %q\nwant %q", rest, want.String())
	}
}

// ─── Reuse ────────────────────────────────────────────────────────────────────

// TestPooledWritersReuse hammers one wrapper with sequential requests per
// coding. The lz4 writer in particular can only be reset and reused after Close
// because azhttp pins codec concurrency to 1; if that ever regresses, this
// deadlocks or corrupts output.
func TestPooledWritersReuse(t *testing.T) {
	wrapper, err := NewWrapper()
	if err != nil {
		t.Fatal(err)
	}
	for _, coding := range []string{EncodingZstd, EncodingLZ4} {
		t.Run(coding, func(t *testing.T) {
			for i := range 25 {
				want := body(4096 + i)
				rec := httptest.NewRecorder()
				wrapper(echoHandler(want, nil, 0)).ServeHTTP(rec, get(coding))
				resp := rec.Result()
				if got := resp.Header.Get("Content-Encoding"); got != coding {
					t.Fatalf("request %d: Content-Encoding = %q, want %q", i, got, coding)
				}
				if got := decodeBody(t, resp); !bytes.Equal(got, want) {
					t.Fatalf("request %d: body mismatch (%d bytes, want %d)", i, len(got), len(want))
				}
			}
		})
	}
}

func TestConcurrentRequests(t *testing.T) {
	wrapper, err := NewWrapper()
	if err != nil {
		t.Fatal(err)
	}
	want := body(32 << 10)
	handler := wrapper(echoHandler(want, nil, 0))

	var wg sync.WaitGroup
	for i := range 32 {
		wg.Go(func() {
			coding := EncodingZstd
			if i%2 == 0 {
				coding = EncodingLZ4
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, get(coding))
			resp := rec.Result()
			raw, _ := io.ReadAll(resp.Body)
			plain, err := az.Decompress(raw)
			if err != nil {
				t.Errorf("decompress: %v", err)
				return
			}
			if !bytes.Equal(plain, want) {
				t.Errorf("body mismatch: %d bytes, want %d", len(plain), len(want))
			}
		})
	}
	wg.Wait()
}

// ─── Interfaces ───────────────────────────────────────────────────────────────

func TestResponseControllerFlushAndUnwrap(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body(2048))
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("ResponseController.Flush: %v", err)
		}
	})
	wrapper, err := NewWrapper()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(wrapper(h))
	defer srv.Close()

	client := &http.Client{Transport: Transport(srv.Client().Transport)}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body(2048)) {
		t.Fatalf("body mismatch: %d bytes", len(got))
	}
}

func TestHijack(t *testing.T) {
	hijacked := make(chan error, 1)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			hijacked <- err
			return
		}
		defer conn.Close()
		buf.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nhi")
		hijacked <- buf.Flush()
	})
	wrapper, err := NewWrapper()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(wrapper(h))
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if err := <-hijacked; err != nil {
		t.Fatalf("hijack: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	if string(got) != "hi" {
		t.Fatalf("body = %q, want %q", got, "hi")
	}
}

// ─── Options ──────────────────────────────────────────────────────────────────

func TestOptionValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		opt  Option
	}{
		{"zstd_level_too_low", ZstdLevel(az.Level2)},
		{"lz4_level_too_high", LZ4Level(az.Level3)},
		{"negative_min_size", MinSize(-1)},
		{"nil_filter", ContentTypeFilter(nil)},
		{"bad_content_type", ContentTypes([]string{"not a media type/"})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewWrapper(tc.opt); err == nil {
				t.Fatal("NewWrapper accepted an invalid option")
			}
		})
	}
}

func TestContentTypesFilter(t *testing.T) {
	big := body(64 << 10)

	t.Run("allow_list", func(t *testing.T) {
		h := echoHandler(big, map[string]string{"Content-Type": "application/json"}, 0)
		resp := serve(t, h, get("zstd"), ContentTypes([]string{"text/plain"}))
		if got := resp.Header.Get("Content-Encoding"); got != "" {
			t.Fatalf("Content-Encoding = %q, want none for a type outside the allow list", got)
		}
	})

	t.Run("allow_list_match_ignores_params", func(t *testing.T) {
		h := echoHandler(big, map[string]string{"Content-Type": "text/plain; charset=utf-8"}, 0)
		resp := serve(t, h, get("zstd"), ContentTypes([]string{"text/plain"}))
		if got := resp.Header.Get("Content-Encoding"); got != EncodingZstd {
			t.Fatalf("Content-Encoding = %q, want zstd", got)
		}
	})

	t.Run("except_list", func(t *testing.T) {
		h := echoHandler(big, map[string]string{"Content-Type": "text/csv"}, 0)
		resp := serve(t, h, get("zstd"), ExceptContentTypes([]string{"text/csv"}))
		if got := resp.Header.Get("Content-Encoding"); got != "" {
			t.Fatalf("Content-Encoding = %q, want none", got)
		}
	})

	t.Run("compress_all", func(t *testing.T) {
		h := echoHandler(big, map[string]string{"Content-Type": "image/png"}, 0)
		resp := serve(t, h, get("zstd"), ContentTypeFilter(CompressAllContentTypeFilter))
		if got := resp.Header.Get("Content-Encoding"); got != EncodingZstd {
			t.Fatalf("Content-Encoding = %q, want zstd", got)
		}
	})
}

func TestMinSizeAndLevels(t *testing.T) {
	data := body(2000)
	resp := serve(t, echoHandler(data, nil, 0), get("zstd"), MinSize(4096))
	if got := resp.Header.Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want none below MinSize", got)
	}

	resp = serve(t, echoHandler(data, nil, 0), get("zstd"), MinSize(1000), ZstdLevel(az.Level5))
	if got := decodeBody(t, resp); !bytes.Equal(got, data) {
		t.Fatalf("level 5 body mismatch")
	}
	resp = serve(t, echoHandler(data, nil, 0), get("lz4"), MinSize(1000), LZ4Level(az.Level2))
	if got := decodeBody(t, resp); !bytes.Equal(got, data) {
		t.Fatalf("lz4 level 2 body mismatch")
	}
}

func TestChecksumOption(t *testing.T) {
	data := body(8192)
	withSum := serve(t, echoHandler(data, nil, 0), get("zstd"))
	without := serve(t, echoHandler(data, nil, 0), get("zstd"), Checksum(false))

	a, _ := io.ReadAll(withSum.Body)
	b, _ := io.ReadAll(without.Body)
	if len(b) >= len(a) {
		t.Fatalf("checksum-free frame (%d) should be smaller than checksummed (%d)", len(b), len(a))
	}
	if got, err := az.Decompress(b); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("checksum-free body did not round-trip: err=%v", err)
	}
}

// ─── Empty and bodiless responses ─────────────────────────────────────────────

func TestBodilessResponses(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler http.Handler
	}{
		{"no_write", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})},
		{"no_content", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})},
		{"not_modified", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotModified)
		})},
		{"empty_write", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write(nil)
		})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := serve(t, tc.handler, get("zstd"))
			if got := resp.Header.Get("Content-Encoding"); got != "" {
				t.Fatalf("Content-Encoding = %q on a bodiless response", got)
			}
			raw, _ := io.ReadAll(resp.Body)
			if len(raw) != 0 {
				t.Fatalf("body = %q, want empty", raw)
			}
		})
	}
}

// TestNoCodingWithoutAFrame pins that a Content-Encoding is never announced for
// a body that never materialises: a handler that promises a big Content-Length
// and then writes nothing (or nothing at all, with MinSize 0) must not leave the
// client trying to decode an empty body.
func TestNoCodingWithoutAFrame(t *testing.T) {
	for _, tc := range []struct {
		name    string
		opts    []Option
		handler http.Handler
	}{
		{
			"declared_length_never_written",
			nil,
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Length", "65536")
				w.Header().Set("Content-Type", "text/plain")
				w.Write(nil)
			}),
		},
		{
			"min_size_zero_empty_body",
			[]Option{MinSize(0)},
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/plain")
			}),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wrapper, err := NewWrapper(tc.opts...)
			if err != nil {
				t.Fatal(err)
			}
			rw := &plainWriter{header: http.Header{}}
			wrapper(tc.handler).ServeHTTP(rw, get("zstd"))

			if ce := rw.header.Get("Content-Encoding"); ce != "" && rw.body.Len() == 0 {
				t.Fatalf("Content-Encoding = %q on an empty body", ce)
			}
		})
	}
}

// ─── Accept-Encoding parsing ──────────────────────────────────────────────────

func TestParseAcceptEncoding(t *testing.T) {
	for _, tc := range []struct {
		header     string
		zstd, lz4  float64
		wantCoding string
	}{
		{"zstd", 1, -1, EncodingZstd},
		{"  ZSTD  ", 1, -1, EncodingZstd},
		{"zstd;q=0.5", 0.5, -1, EncodingZstd},
		{"gzip, deflate, zstd;q=0.001", 0.001, -1, EncodingZstd},
		{"zstd;q=0", 0, -1, ""},
		{"lz4;q=1.0, zstd;q=0.9", 0.9, 1, EncodingLZ4},
		{"identity", -1, -1, ""},
		{"zstd;q=bogus", -1, -1, ""},
	} {
		t.Run(tc.header, func(t *testing.T) {
			q := parseAcceptEncoding(tc.header)
			if q.zstd != tc.zstd || q.lz4 != tc.lz4 {
				t.Fatalf("parsed q = {zstd:%v lz4:%v}, want {zstd:%v lz4:%v}", q.zstd, q.lz4, tc.zstd, tc.lz4)
			}
			c := defaultConfig()
			if got := c.selectEncoding(get(tc.header)); got != tc.wantCoding {
				t.Fatalf("selectEncoding = %q, want %q", got, tc.wantCoding)
			}
		})
	}
}

// ─── Request decompression ────────────────────────────────────────────────────

func TestDecompressRequests(t *testing.T) {
	want := body(32 << 10)

	for _, level := range []az.Level{az.Level1, az.Level3} {
		t.Run(fmt.Sprintf("L%d", level), func(t *testing.T) {
			frame, err := az.Compress(want, level)
			if err != nil {
				t.Fatal(err)
			}
			coding := EncodingZstd
			if level <= az.Level2 {
				coding = EncodingLZ4
			}

			var got []byte
			h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if ce := r.Header.Get("Content-Encoding"); ce != "" {
					t.Errorf("handler still sees Content-Encoding %q", ce)
				}
				got, err = io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("read body: %v", err)
				}
			})

			req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(frame))
			req.Header.Set("Content-Encoding", coding)
			DecompressRequests(0)(h).ServeHTTP(httptest.NewRecorder(), req)

			if !bytes.Equal(got, want) {
				t.Fatalf("decoded body: %d bytes, want %d", len(got), len(want))
			}
		})
	}
}

func TestDecompressRequestsPassesThroughOtherCodings(t *testing.T) {
	raw := []byte("not compressed by az")
	var got []byte
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		if ce := r.Header.Get("Content-Encoding"); ce != "gzip" {
			t.Errorf("Content-Encoding = %q, want it left alone", ce)
		}
	})
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw))
	req.Header.Set("Content-Encoding", "gzip")
	DecompressRequests(0)(h).ServeHTTP(httptest.NewRecorder(), req)

	if !bytes.Equal(got, raw) {
		t.Fatalf("body = %q, want it passed through", got)
	}
}

func TestDecompressRequestsLimit(t *testing.T) {
	const limit = 4096
	for _, size := range []int{limit - 1, limit, limit + 1, 1 << 20} {
		t.Run(fmt.Sprintf("%d", size), func(t *testing.T) {
			frame, err := az.Compress(body(size), az.Level3)
			if err != nil {
				t.Fatal(err)
			}
			var readErr error
			var n int
			h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var buf []byte
				buf, readErr = io.ReadAll(r.Body)
				n = len(buf)
			})
			req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(frame))
			req.Header.Set("Content-Encoding", EncodingZstd)
			DecompressRequests(limit)(h).ServeHTTP(httptest.NewRecorder(), req)

			if size <= limit {
				if readErr != nil {
					t.Fatalf("read: %v", readErr)
				}
				if n != size {
					t.Fatalf("read %d bytes, want %d", n, size)
				}
				return
			}
			if !errors.Is(readErr, az.ErrTooLarge) {
				t.Fatalf("err = %v, want az.ErrTooLarge", readErr)
			}
			if n > limit {
				t.Fatalf("handler saw %d bytes, more than the %d limit", n, limit)
			}
		})
	}
}

// ─── Client transport ─────────────────────────────────────────────────────────

func TestTransportRoundTrip(t *testing.T) {
	want := body(64 << 10)
	wrapper, err := NewWrapper()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(wrapper(echoHandler(want, map[string]string{"Content-Type": "text/plain"}, 0)))
	defer srv.Close()

	for _, tc := range []struct {
		name       string
		opts       []TransportOption
		wantCoding string
	}{
		{"zstd_default", nil, EncodingZstd},
		{"lz4_enabled", []TransportOption{TransportLZ4(true)}, EncodingZstd},
		{"lz4_preferred", []TransportOption{TransportPreferLZ4(true)}, EncodingLZ4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Report what the server actually chose by peeking before the
			// transport strips the header.
			var serverCoding string
			probe := &probeTransport{
				parent: srv.Client().Transport,
				seen:   func(h http.Header) { serverCoding = h.Get("Content-Encoding") },
			}
			client := &http.Client{Transport: Transport(probe, tc.opts...)}
			resp, err := client.Get(srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()

			if serverCoding != tc.wantCoding {
				t.Errorf("server used %q, want %q", serverCoding, tc.wantCoding)
			}
			if !resp.Uncompressed {
				t.Error("Response.Uncompressed = false, want true")
			}
			if resp.Header.Get("Content-Encoding") != "" {
				t.Error("Content-Encoding should be stripped after decoding")
			}
			if resp.ContentLength != -1 {
				t.Errorf("ContentLength = %d, want -1", resp.ContentLength)
			}
			got, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("body mismatch: %d bytes, want %d", len(got), len(want))
			}
		})
	}
}

func TestTransportNilParentUsesDefault(t *testing.T) {
	want := body(8 << 10)
	wrapper, err := NewWrapper()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(wrapper(echoHandler(want, map[string]string{"Content-Type": "text/plain"}, 0)))
	defer srv.Close()

	client := &http.Client{Transport: Transport(nil)} // nil → http.DefaultTransport
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("body mismatch: %d bytes, want %d", len(got), len(want))
	}
	if !resp.Uncompressed {
		t.Error("Response.Uncompressed = false, want true")
	}
}

func TestTransportLeavesExplicitAcceptEncodingAlone(t *testing.T) {
	var sentAE string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sentAE = r.Header.Get("Accept-Encoding")
		w.Write(body(2048))
	}))
	defer srv.Close()

	client := &http.Client{Transport: Transport(srv.Client().Transport)}
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if sentAE != "identity" {
		t.Fatalf("server saw Accept-Encoding %q, want the caller's %q", sentAE, "identity")
	}
}

func TestTransportDoesNotMutateRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body(2048))
	}))
	defer srv.Close()

	client := &http.Client{Transport: Transport(srv.Client().Transport)}
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if got := req.Header.Get("Accept-Encoding"); got != "" {
		t.Fatalf("RoundTrip mutated the caller's request: Accept-Encoding = %q", got)
	}
}

func TestTransportPassesThroughUnrequestedCoding(t *testing.T) {
	frame, err := az.Compress(body(4096), az.Level3)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", EncodingZstd)
		w.Write(frame)
	}))
	defer srv.Close()

	// The caller set Accept-Encoding, so the transport did not request the
	// coding and must not silently decode it.
	client := &http.Client{Transport: Transport(srv.Client().Transport)}
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	req.Header.Set("Accept-Encoding", "zstd")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, frame) {
		t.Fatal("body was decoded even though this transport did not request the coding")
	}

	// With TransportAlwaysDecompress it decodes anyway.
	client = &http.Client{Transport: Transport(srv.Client().Transport, TransportAlwaysDecompress(true))}
	req, _ = http.NewRequest(http.MethodGet, srv.URL, nil)
	req.Header.Set("Accept-Encoding", "zstd")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ = io.ReadAll(resp.Body)
	if !bytes.Equal(got, body(4096)) {
		t.Fatal("TransportAlwaysDecompress did not decode the body")
	}
}

// probeTransport reports response headers before the az transport rewrites them.
type probeTransport struct {
	parent http.RoundTripper
	seen   func(http.Header)
}

func (p *probeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := p.parent.RoundTrip(r)
	if err == nil {
		p.seen(resp.Header)
	}
	return resp, err
}

// ─── Benchmarks ───────────────────────────────────────────────────────────────

func BenchmarkHandler(b *testing.B) {
	wrapper, err := NewWrapper()
	if err != nil {
		b.Fatal(err)
	}
	for _, size := range []int{2 << 10, 20 << 10, 100 << 10} {
		data := body(size)
		h := wrapper(echoHandler(data, map[string]string{"Content-Type": "text/plain"}, 0))
		for _, coding := range []string{EncodingZstd, EncodingLZ4} {
			b.Run(fmt.Sprintf("%s/%dk", coding, size>>10), func(b *testing.B) {
				req := get(coding)
				b.SetBytes(int64(size))
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					h.ServeHTTP(discardWriter{http.Header{}}, req)
				}
			})
		}
	}
}

// discardWriter is an http.ResponseWriter that throws the response away.
type discardWriter struct{ header http.Header }

func (d discardWriter) Header() http.Header         { return d.header }
func (d discardWriter) Write(b []byte) (int, error) { return len(b), nil }
func (d discardWriter) WriteHeader(int)             {}

// plainWriter records headers and body without net/http's Content-Type
// sniffing, so tests can see exactly what the middleware set.
type plainWriter struct {
	header http.Header
	body   bytes.Buffer
	code   int
}

func (p *plainWriter) Header() http.Header         { return p.header }
func (p *plainWriter) Write(b []byte) (int, error) { return p.body.Write(b) }
func (p *plainWriter) WriteHeader(code int)        { p.code = code }
