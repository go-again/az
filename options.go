package az

// Level controls the compression level.
type Level int

const (
	Level1       Level = 1 // Fastest — lz4 level 3
	Level2       Level = 2 // Fast    — lz4 level 6
	Level3       Level = 3 // Default — zstd level 6
	Level4       Level = 4 // Better  — zstd level 12
	Level5       Level = 5 // Best    — zstd level 18
	DefaultLevel       = Level3

	minLevel = Level1
	maxLevel = Level5
)

// Options controls compression behaviour.
type Options struct {
	Level       Level
	Checksum    bool // include checksums (default true)
	ContentSize bool // no-op: one-shot frames always store the content size
	Concurrency int  // codec worker goroutines; 0 = GOMAXPROCS (default)
}

// defaultOptions returns sensible defaults.
func defaultOptions() Options {
	return Options{
		Level:    DefaultLevel,
		Checksum: true,
	}
}

// Option is a functional option for NewWriter.
type Option func(*Options)

// WithLevel sets the compression level.
func WithLevel(l Level) Option {
	return func(o *Options) { o.Level = l }
}

// WithChecksum enables or disables checksums.
func WithChecksum(on bool) Option {
	return func(o *Options) { o.Checksum = on }
}

// WithConcurrency caps the number of worker goroutines a streaming Writer may
// use for block compression. n <= 0 (the default) means GOMAXPROCS.
//
// Use WithConcurrency(1) when you create one Writer per unit of work that is
// already parallel — an HTTP response, say — so the codec does not multiply the
// server's goroutine and memory footprint by GOMAXPROCS. It also makes a Writer
// safe to pool: only a single-threaded lz4 Writer can be Reset and reused after
// Close.
//
// Concurrency does not change the bytes produced, only how they are produced.
func WithConcurrency(n int) Option {
	return func(o *Options) { o.Concurrency = n }
}

// WithContentSize is a no-op, kept for compatibility.
//
// The one-shot paths (Compress and Encoder.EncodeAll) know the input length and
// now always record it in the frame header, so there is nothing to opt into.
// The streaming Writer never can: it must emit the header before it has seen
// the whole input, so it always writes a frame with no content size.
func WithContentSize(on bool) Option {
	return func(o *Options) { o.ContentSize = on }
}
