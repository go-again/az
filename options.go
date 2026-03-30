package az

// Level controls the compression level.
type Level int

const (
	Level1       Level = 1 // Fastest — pure LZ77, no entropy coding
	Level2       Level = 2 // Fast — dual hash + Huffman literals
	Level3       Level = 3 // Default — lazy match + Huffman + FSE sequences
	Level4       Level = 4 // Better — deeper search + 4X Huffman
	Level5       Level = 5 // Best — optimal parse
	DefaultLevel       = Level3

	minLevel = Level1
	maxLevel = Level5
)

// Options controls compression behaviour.
type Options struct {
	Level       Level
	Checksum    bool // include per-block and content checksums (default true)
	ContentSize bool // store uncompressed content size in frame header
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
func WithContentSize(on bool) Option {
	return func(o *Options) { o.ContentSize = on }
}
