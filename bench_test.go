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
