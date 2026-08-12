// Package azhttp compresses HTTP responses with az.
//
// The server side is middleware around an http.Handler: it negotiates a
// content coding from the request's Accept-Encoding header, compresses the
// response body, and sets the matching Content-Encoding. The client side is an
// http.RoundTripper wrapper that asks for those codings and transparently
// decodes them.
//
//	http.Handle("/", azhttp.Handler(mux))
//
//	client := &http.Client{Transport: azhttp.Transport(http.DefaultTransport)}
//
// # Content codings
//
// az levels 3–5 emit Zstandard frames, which travel as the standard "zstd"
// coding (RFC 8878, registered with IANA and understood by current browsers and
// CDNs). Levels 1–2 emit LZ4 frames, which travel as "lz4" — that token is NOT
// standardised, so it is only ever sent to a client that named it in
// Accept-Encoding. A browser never will; a Go service using azhttp.Transport
// with TransportLZ4 will. This makes lz4 a safe opt-in for internal traffic
// (cheap CPU, decent ratio) and invisible to the public web.
//
// When a client accepts both at the same quality, zstd wins by default; see
// PreferLZ4.
//
// # Scope
//
// Responses are compressed on the fly with a pooled streaming az.Writer per
// response (concurrency 1, so a request costs one goroutine, not GOMAXPROCS).
// Bodies below MinSize, already-encoded bodies, ranged responses, HEAD
// requests, bodiless status codes, and content types that are already
// compressed are passed through untouched.
package azhttp

import (
	"net/http"
	"strings"
)

// Content codings azhttp can emit. Zstd is the standard token from RFC 8878;
// LZ4 is non-standard and only used when a client explicitly asks for it.
const (
	EncodingZstd = "zstd"
	EncodingLZ4  = "lz4"
)

// HeaderNoCompression disables compression for a single response when the
// handler sets it to any value. Use it for bodies you know are incompressible
// or already encoded. It is always removed before the response goes out.
const HeaderNoCompression = "X-No-Compression"

// DefaultMinSize is the smallest response body that is compressed by default.
// Below roughly one MTU the compressed body travels in the same packet as the
// original would have, so the CPU buys nothing.
const DefaultMinSize = 1024

// Header names, canonical form.
const (
	acceptEncoding  = "Accept-Encoding"
	acceptRanges    = "Accept-Ranges"
	contentEncoding = "Content-Encoding"
	contentLength   = "Content-Length"
	contentRange    = "Content-Range"
	contentType     = "Content-Type"
	eTag            = "Etag"
	vary            = "Vary"
)

// qualities holds the parsed quality values of one Accept-Encoding header.
// A negative value means the coding was not mentioned.
type qualities struct {
	zstd, lz4, star float64
}

// parseAcceptEncoding extracts the quality values azhttp cares about. Unknown
// codings are ignored; the "*" wildcard is remembered as a fallback, as
// RFC 9110 §12.5.3 says it matches any coding not listed explicitly.
func parseAcceptEncoding(header string) qualities {
	q := qualities{zstd: -1, lz4: -1, star: -1}
	for part := range strings.SplitSeq(header, ",") {
		name, quality, ok := parseCoding(part)
		if !ok {
			continue
		}
		switch name {
		case EncodingZstd:
			q.zstd = quality
		case EncodingLZ4:
			q.lz4 = quality
		case "*":
			q.star = quality
		}
	}
	return q
}

// parseCoding splits one Accept-Encoding element into a lowercased coding name
// and its quality value, defaulting to q=1 when no q parameter is present.
// A malformed element is reported as not ok, which drops it from negotiation.
func parseCoding(s string) (name string, quality float64, ok bool) {
	name, params, _ := strings.Cut(s, ";")
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return "", 0, false
	}
	quality = 1
	for param := range strings.SplitSeq(params, ";") {
		key, value, found := strings.Cut(param, "=")
		if !found || !strings.EqualFold(strings.TrimSpace(key), "q") {
			continue
		}
		quality, ok = parseQValue(strings.TrimSpace(value))
		if !ok {
			return "", 0, false
		}
	}
	return name, quality, true
}

// parseQValue parses a "qvalue" (RFC 9110 §12.4.2): 0-1 with up to three
// decimals. It is a hot-path parse of a tiny grammar, so it avoids strconv.
func parseQValue(s string) (float64, bool) {
	if s == "" {
		return 0, false
	}
	whole := s[0]
	if whole != '0' && whole != '1' {
		return 0, false
	}
	q := float64(whole - '0')
	if len(s) == 1 {
		return q, true
	}
	if s[1] != '.' || len(s) > 5 {
		return 0, false
	}
	scale := 0.1
	for _, c := range []byte(s[2:]) {
		if c < '0' || c > '9' {
			return 0, false
		}
		q += float64(c-'0') * scale
		scale /= 10
	}
	if q > 1 {
		return 0, false
	}
	return q, true
}

// quality returns the effective quality value for a coding: its own if the
// client named it, otherwise the "*" wildcard's, otherwise 0 (not acceptable).
func (q qualities) quality(coding string) float64 {
	var own float64
	switch coding {
	case EncodingZstd:
		own = q.zstd
	case EncodingLZ4:
		own = q.lz4
	}
	if own >= 0 {
		return own
	}
	if q.star >= 0 {
		return q.star
	}
	return 0
}

// selectEncoding picks the coding to use for a request, or "" for none.
func (c *config) selectEncoding(r *http.Request) string {
	// HEAD responses carry no body, and asking for a coding on HEAD trips a
	// long-standing nginx bug (nginx#358, golang/go#5522).
	if r.Method == http.MethodHead {
		return ""
	}
	// A ranged request wants bytes of the identity representation; compressing
	// would change what the offsets mean.
	if r.Header.Get("Range") != "" {
		return ""
	}
	header := r.Header.Get(acceptEncoding)
	if header == "" {
		return ""
	}
	q := parseAcceptEncoding(header)

	zstdQ, lz4Q := 0.0, 0.0
	if c.zstdEnabled {
		zstdQ = q.quality(EncodingZstd)
	}
	if c.lz4Enabled {
		// Never infer lz4 from a wildcard: it is not a registered coding, so a
		// client sending "*" is not claiming it can decode LZ4 frames.
		if lz4 := q.lz4; lz4 > 0 {
			lz4Q = lz4
		}
	}

	switch {
	case zstdQ <= 0 && lz4Q <= 0:
		return ""
	case zstdQ > lz4Q:
		return EncodingZstd
	case lz4Q > zstdQ:
		return EncodingLZ4
	case c.preferLZ4:
		return EncodingLZ4
	default:
		return EncodingZstd
	}
}

// Already-compressed media types, by prefix and by substring. Compressing these
// burns CPU for a fraction of a percent, and sometimes makes them bigger.
var (
	excludePrefixes = []string{"video/", "audio/", "image/jp", "image/png", "image/gif", "image/webp", "image/avif"}
	excludeContains = []string{"compress", "zip", "snappy", "lzma", "xz", "zstd", "brotli", "stuffit"}
)

// DefaultContentTypeFilter reports whether a Content-Type should be compressed.
// It accepts everything except media types that are already compressed. An
// empty Content-Type is accepted: the handler may simply not have set one yet,
// and the wrapper sniffs it before deciding.
func DefaultContentTypeFilter(ct string) bool {
	ct = strings.ToLower(strings.TrimSpace(ct))
	if ct == "" {
		return true
	}
	for _, s := range excludeContains {
		if strings.Contains(ct, s) {
			return false
		}
	}
	for _, p := range excludePrefixes {
		if strings.HasPrefix(ct, p) {
			return false
		}
	}
	return true
}

// CompressAllContentTypeFilter compresses every content type.
func CompressAllContentTypeFilter(string) bool { return true }

// bodyAllowedForStatus mirrors net/http: 1xx, 204 and 304 have no body.
func bodyAllowedForStatus(status int) bool {
	switch {
	case status >= 100 && status <= 199:
		return false
	case status == http.StatusNoContent:
		return false
	case status == http.StatusNotModified:
		return false
	}
	return true
}
