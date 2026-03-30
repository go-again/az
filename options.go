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
	ContentSize bool // store uncompressed content size in frame header (one-shot only)
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

// WithContentSize enables storing the uncompressed size in the frame header.
// This is honoured only when using the one-shot Compress helper.
func WithContentSize(on bool) Option {
	return func(o *Options) { o.ContentSize = on }
}
