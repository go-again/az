package azhttp

import (
	"fmt"
	"mime"
	"strings"

	"github.com/go-again/az"
)

// An Option configures the response-compressing middleware. Options are
// validated when the wrapper is built, so a bad one fails at start-up rather
// than on the first request.
type Option func(*config) error

type config struct {
	minSize           int
	zstdLevel         az.Level
	lz4Level          az.Level
	zstdEnabled       bool
	lz4Enabled        bool
	preferLZ4         bool
	checksum          bool
	contentTypeFilter func(ct string) bool
	keepAcceptRanges  bool
	setContentType    bool
	suffixETag        string
	dropETag          bool

	zstdWriters *writerPool
	lz4Writers  *writerPool
}

func defaultConfig() *config {
	return &config{
		minSize:           DefaultMinSize,
		zstdLevel:         az.Level3,
		lz4Level:          az.Level1,
		zstdEnabled:       true,
		lz4Enabled:        true,
		checksum:          true,
		contentTypeFilter: DefaultContentTypeFilter,
		setContentType:    true,
	}
}

// MinSize sets the smallest response body that will be compressed, in bytes.
// Smaller bodies are written through untouched. Defaults to DefaultMinSize.
func MinSize(size int) Option {
	return func(c *config) error {
		if size < 0 {
			return fmt.Errorf("azhttp: MinSize must be >= 0, got %d", size)
		}
		c.minSize = size
		return nil
	}
}

// ZstdLevel sets the az level used for the "zstd" coding. It must be a
// Zstandard level (az.Level3–az.Level5). Defaults to az.Level3.
func ZstdLevel(level az.Level) Option {
	return func(c *config) error {
		if level < az.Level3 || level > az.Level5 {
			return fmt.Errorf("azhttp: ZstdLevel must be az.Level3–az.Level5, got %d", level)
		}
		c.zstdLevel = level
		return nil
	}
}

// LZ4Level sets the az level used for the "lz4" coding. It must be an LZ4 level
// (az.Level1 or az.Level2). Defaults to az.Level1.
func LZ4Level(level az.Level) Option {
	return func(c *config) error {
		if level < az.Level1 || level > az.Level2 {
			return fmt.Errorf("azhttp: LZ4Level must be az.Level1–az.Level2, got %d", level)
		}
		c.lz4Level = level
		return nil
	}
}

// EnableZstd turns the "zstd" coding on or off. On by default; turning it off
// leaves lz4 as the only coding, which effectively disables compression for
// standard clients.
func EnableZstd(enable bool) Option {
	return func(c *config) error {
		c.zstdEnabled = enable
		return nil
	}
}

// EnableLZ4 turns the "lz4" coding on or off. On by default, but only ever
// selected for a client that names lz4 in Accept-Encoding.
func EnableLZ4(enable bool) Option {
	return func(c *config) error {
		c.lz4Enabled = enable
		return nil
	}
}

// PreferLZ4 decides the tie when a client accepts zstd and lz4 at the same
// quality: false (the default) picks zstd for its ratio, true picks lz4 for its
// speed. An explicit q-value from the client always wins over this.
func PreferLZ4(prefer bool) Option {
	return func(c *config) error {
		c.preferLZ4 = prefer
		return nil
	}
}

// Checksum enables or disables the frame checksum in compressed responses.
// On by default. Turning it off saves a few bytes and a little CPU; TCP and TLS
// already protect the bytes in transit.
func Checksum(on bool) Option {
	return func(c *config) error {
		c.checksum = on
		return nil
	}
}

// ContentTypeFilter replaces the predicate that decides, from the response's
// Content-Type, whether a body is worth compressing. Use
// CompressAllContentTypeFilter to compress everything.
func ContentTypeFilter(filter func(ct string) bool) Option {
	return func(c *config) error {
		if filter == nil {
			return fmt.Errorf("azhttp: ContentTypeFilter must not be nil")
		}
		c.contentTypeFilter = filter
		return nil
	}
}

// ContentTypes restricts compression to the given media types, replacing the
// default filter. Parameters are compared when given, so "text/html" matches
// any charset while "text/html; charset=utf-8" matches only that one.
func ContentTypes(types []string) Option {
	return func(c *config) error {
		parsed, err := parseContentTypes(types)
		if err != nil {
			return err
		}
		c.contentTypeFilter = func(ct string) bool { return matchesContentType(parsed, ct) }
		return nil
	}
}

// ExceptContentTypes compresses everything except the given media types,
// replacing the default filter.
func ExceptContentTypes(types []string) Option {
	return func(c *config) error {
		parsed, err := parseContentTypes(types)
		if err != nil {
			return err
		}
		c.contentTypeFilter = func(ct string) bool { return !matchesContentType(parsed, ct) }
		return nil
	}
}

// KeepAcceptRanges keeps any Accept-Ranges header the handler set on a
// compressed response. By default it is removed, because the ranges a client
// would ask for index the uncompressed body, which is not what it will receive.
func KeepAcceptRanges() Option {
	return func(c *config) error {
		c.keepAcceptRanges = true
		return nil
	}
}

// SetContentType controls whether a missing Content-Type is sniffed from the
// first bytes of the body and set on the response. On by default, matching what
// net/http would have done had the body not been compressed.
func SetContentType(on bool) Option {
	return func(c *config) error {
		c.setContentType = on
		return nil
	}
}

// SuffixETag appends suffix (plus the coding name) inside a compressed
// response's ETag, so the compressed and identity representations cannot share
// a cache entry. Ignored when DropETag is set.
func SuffixETag(suffix string) Option {
	return func(c *config) error {
		c.suffixETag = suffix
		return nil
	}
}

// DropETag removes the ETag from compressed responses. Blunter than SuffixETag
// — it costs revalidation — but it cannot collide.
func DropETag() Option {
	return func(c *config) error {
		c.dropETag = true
		return nil
	}
}

// parsedContentType is a media type with its parameters, as given to
// ContentTypes/ExceptContentTypes.
type parsedContentType struct {
	mediaType string
	params    map[string]string
}

func parseContentTypes(types []string) ([]parsedContentType, error) {
	out := make([]parsedContentType, 0, len(types))
	for _, v := range types {
		mediaType, params, err := mime.ParseMediaType(strings.ToLower(v))
		if err != nil {
			return nil, fmt.Errorf("azhttp: invalid content type %q: %w", v, err)
		}
		out = append(out, parsedContentType{mediaType: mediaType, params: params})
	}
	return out, nil
}

// matchesContentType reports whether ct matches any of the configured types.
// A configured type with no parameters matches on media type alone; with
// parameters, every one of them must be present and equal.
func matchesContentType(types []parsedContentType, ct string) bool {
	mediaType, params, err := mime.ParseMediaType(strings.ToLower(strings.TrimSpace(ct)))
	if err != nil {
		return false
	}
	for _, t := range types {
		if t.mediaType != mediaType {
			continue
		}
		if len(t.params) == 0 {
			return true
		}
		match := true
		for k, v := range t.params {
			if params[k] != v {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
