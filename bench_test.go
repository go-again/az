package az

import (
	"fmt"
	"testing"
)

// Benchmark corpora — generated in-process (no testdata files required).
var benchCorpora = []struct {
	name string
	data func() []byte
}{
	{"zeros_1M", func() []byte { return make([]byte, 1<<20) }},
	{"pattern_1M", func() []byte { return makePatterned(1 << 20) }},
	{"random_1M", func() []byte { return randBytes(1 << 20) }},
}

func BenchmarkCompress(b *testing.B) {
	for _, c := range benchCorpora {
		data := c.data()
		for _, level := range []Level{Level1, Level2, Level3, Level4, Level5} {
			name := fmt.Sprintf("%s/L%d", c.name, level)
			b.Run(name, func(b *testing.B) {
				b.SetBytes(int64(len(data)))
				b.ResetTimer()
				for range b.N {
					_, _ = Compress(data, level)
				}
			})
		}
	}
}

func BenchmarkDecompress(b *testing.B) {
	for _, c := range benchCorpora {
		data := c.data()
		for _, level := range []Level{Level1, Level2, Level3} {
			comp, _ := Compress(data, level)
			name := fmt.Sprintf("%s/L%d", c.name, level)
			b.Run(name, func(b *testing.B) {
				b.SetBytes(int64(len(data)))
				b.ResetTimer()
				for range b.N {
					_, _ = Decompress(comp)
				}
			})
		}
	}
}

// BenchmarkEncodeAll exercises the pooled Encoder, reusing one instance and a
// scratch dst across iterations — the intended high-throughput usage. Compare
// against BenchmarkCompress to see the drop in allocs/op and B/op, especially on
// the zstd levels (L3–L5).
func BenchmarkEncodeAll(b *testing.B) {
	for _, c := range benchCorpora {
		data := c.data()
		for _, level := range []Level{Level1, Level2, Level3, Level4, Level5} {
			name := fmt.Sprintf("%s/L%d", c.name, level)
			b.Run(name, func(b *testing.B) {
				enc := NewEncoder()
				var scratch []byte
				b.SetBytes(int64(len(data)))
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					scratch, _ = enc.EncodeAll(scratch[:0], data, level)
				}
			})
		}
	}
}

// BenchmarkCompressOneShot is the per-call baseline (fresh codec each call) for
// the zstd levels, to contrast with BenchmarkEncodeAll's pooled reuse.
func BenchmarkCompressOneShot(b *testing.B) {
	for _, c := range benchCorpora {
		data := c.data()
		for _, level := range []Level{Level1, Level2, Level3, Level4, Level5} {
			name := fmt.Sprintf("%s/L%d", c.name, level)
			b.Run(name, func(b *testing.B) {
				b.SetBytes(int64(len(data)))
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					_, _ = Compress(data, level)
				}
			})
		}
	}
}

// BenchmarkDecodeAll exercises the pooled Decoder, reusing one instance and a
// scratch dst across iterations.
func BenchmarkDecodeAll(b *testing.B) {
	for _, c := range benchCorpora {
		data := c.data()
		for _, level := range []Level{Level1, Level2, Level3, Level4, Level5} {
			comp, _ := Compress(data, level)
			name := fmt.Sprintf("%s/L%d", c.name, level)
			b.Run(name, func(b *testing.B) {
				dec := NewDecoder()
				var scratch []byte
				b.SetBytes(int64(len(data)))
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					scratch, _ = dec.DecodeAll(scratch[:0], comp)
				}
			})
		}
	}
}

func BenchmarkCompressRatio(b *testing.B) {
	for _, c := range benchCorpora {
		data := c.data()
		for _, level := range []Level{Level1, Level2, Level3, Level4, Level5} {
			data := data
			level := level
			name := fmt.Sprintf("%s/L%d", c.name, level)
			b.Run(name, func(b *testing.B) {
				b.StopTimer()
				comp, _ := Compress(data, level)
				b.ReportMetric(float64(len(comp))/float64(len(data)), "ratio")
				b.SetBytes(int64(len(data)))
				b.StartTimer()
				for range b.N {
					_, _ = Compress(data, level)
				}
			})
		}
	}
}
