package az

import (
	"bytes"
	"crypto/rand"
	"io"
	"testing"
)

// ─── Helpers ──────────────────────────────────────────────────────────────────

func randBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

func makePatterned(n int) []byte {
	// Repetitive but not a single byte — good compression target.
	const pattern = "the quick brown fox jumps over the lazy dog\n"
	out := make([]byte, n)
	for i := range out {
		out[i] = pattern[i%len(pattern)]
	}
	return out
}

// ─── Round-trip tests ─────────────────────────────────────────────────────────

var testCases = []struct {
	name string
	data []byte
}{
	{"empty", nil},
	{"single_byte", []byte{0x42}},
	{"two_bytes", []byte{0x01, 0x02}},
	{"all_zeros_1K", make([]byte, 1024)},
	{"all_zeros_64K", make([]byte, 64<<10)},
	{"all_zeros_256K", make([]byte, 256<<10)},
	{"all_same_65537", bytes.Repeat([]byte{0xAB}, 65537)},
	{"random_1K", randBytes(1024)},
	{"random_64K", randBytes(64 << 10)},
	{"random_256K", randBytes(256 << 10)},
	{"patterned_1M", makePatterned(1 << 20)},
	{"patterned_4K", makePatterned(4096)},
}

func TestRoundTrip(t *testing.T) {
	for _, tc := range testCases {
		// Limit levels 4-5 to small inputs to keep the test fast.
		maxLvl := Level5
		if len(tc.data) > 512<<10 {
			maxLvl = Level3
		}
		for level := Level1; level <= maxLvl; level++ {
			tc, level := tc, level
			t.Run(tc.name+"/L"+string(rune('0'+level)), func(t *testing.T) {
				comp, err := Compress(tc.data, level)
				if err != nil {
					t.Fatalf("Compress: %v", err)
				}
				got, err := Decompress(comp)
				if err != nil {
					t.Fatalf("Decompress: %v", err)
				}
				if !bytes.Equal(tc.data, got) {
					t.Fatalf("round-trip mismatch: input %d bytes, got %d bytes", len(tc.data), len(got))
				}
			})
		}
	}
}

func TestRoundTripStreaming(t *testing.T) {
	for _, level := range []Level{Level1, Level2, Level3} {
		data := makePatterned(3 * (1 << 20))
		var buf bytes.Buffer
		w := NewWriter(&buf, WithLevel(level))
		// Write in three chunks to exercise multi-block path.
		chunk := len(data) / 3
		for i := 0; i < 3; i++ {
			end := (i + 1) * chunk
			if end > len(data) {
				end = len(data)
			}
			if _, err := w.Write(data[i*chunk : end]); err != nil {
				t.Fatalf("L%d Write chunk %d: %v", level, i, err)
			}
		}
		if err := w.Close(); err != nil {
			t.Fatalf("L%d Close: %v", level, err)
		}

		r := NewReader(&buf)
		got, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("L%d ReadAll: %v", level, err)
		}
		if !bytes.Equal(data, got) {
			t.Fatalf("L%d streaming round-trip mismatch: want %d bytes, got %d", level, len(data), len(got))
		}
	}
}

func TestAutoDetectFormat(t *testing.T) {
	// Compress with each level; the same Decompress should handle all formats.
	src := []byte("auto-detect: lz4 (levels 1-2) and zstd (levels 3-5) use different magic bytes")
	for level := Level1; level <= Level5; level++ {
		comp, err := Compress(src, level)
		if err != nil {
			t.Fatalf("L%d Compress: %v", level, err)
		}
		got, err := Decompress(comp)
		if err != nil {
			t.Fatalf("L%d Decompress: %v", level, err)
		}
		if !bytes.Equal(src, got) {
			t.Fatalf("L%d auto-detect mismatch", level)
		}
	}
}

func TestInvalidMagic(t *testing.T) {
	_, err := Decompress([]byte{0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07})
	if err == nil {
		t.Fatal("expected error for invalid magic, got nil")
	}
}

func TestInvalidLevel(t *testing.T) {
	_, err := Compress([]byte("hello"), 0)
	if err == nil {
		t.Fatal("expected error for level 0")
	}
	_, err = Compress([]byte("hello"), 6)
	if err == nil {
		t.Fatal("expected error for level 6")
	}
}

func TestIncompressibleData(t *testing.T) {
	// Random data should not expand by more than a small constant overhead.
	data := randBytes(64 << 10)
	for _, level := range []Level{Level1, Level2, Level3} {
		comp, err := Compress(data, level)
		if err != nil {
			t.Fatalf("L%d Compress: %v", level, err)
		}
		// lz4/zstd store incompressible blocks verbatim; allow generous overhead
		if len(comp) > len(data)+512 {
			t.Errorf("L%d: incompressible data too large: %d vs input %d", level, len(comp), len(data))
		}
	}
}

func TestCompressRatio(t *testing.T) {
	// Highly compressible data should compress significantly.
	data := makePatterned(1 << 20)
	for level := Level1; level <= Level5; level++ {
		comp, err := Compress(data, level)
		if err != nil {
			t.Fatalf("L%d Compress: %v", level, err)
		}
		ratio := float64(len(comp)) / float64(len(data))
		t.Logf("L%d: %d → %d bytes (ratio %.3f)", level, len(data), len(comp), ratio)
		if ratio > 0.10 {
			t.Errorf("L%d: poor compression ratio %.3f on patterned data", level, ratio)
		}
	}
}

func TestWriterReset(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	data := makePatterned(10000)
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// Reset and compress different data
	var buf2 bytes.Buffer
	w.Reset(&buf2)
	data2 := makePatterned(20000)
	if _, err := w.Write(data2); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := Decompress(buf2.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data2, got) {
		t.Fatal("mismatch after Writer.Reset")
	}
}

func TestReaderReset(t *testing.T) {
	src := makePatterned(50000)
	comp, err := Compress(src, Level3)
	if err != nil {
		t.Fatal(err)
	}

	r := NewReader(bytes.NewReader(comp))
	got1, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}

	r.Reset(bytes.NewReader(comp))
	got2, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	r.Close()

	if !bytes.Equal(got1, src) || !bytes.Equal(got2, src) {
		t.Fatal("Reader.Reset: content mismatch")
	}
}

func TestChecksumDisabled(t *testing.T) {
	data := makePatterned(32 << 10)
	for _, level := range []Level{Level1, Level3} {
		var buf bytes.Buffer
		w := NewWriter(&buf, WithLevel(level), WithChecksum(false))
		w.Write(data)
		w.Close()

		got, err := Decompress(buf.Bytes())
		if err != nil {
			t.Fatalf("L%d no-checksum roundtrip: %v", level, err)
		}
		if !bytes.Equal(data, got) {
			t.Fatalf("L%d no-checksum mismatch", level)
		}
	}
}

// ─── Fuzz ─────────────────────────────────────────────────────────────────────

func FuzzRoundtrip(f *testing.F) {
	f.Add([]byte("hello world"))
	f.Add([]byte("aaaaaaaaaaaaa"))
	f.Add(makePatterned(256))
	f.Fuzz(func(t *testing.T, data []byte) {
		for _, l := range []Level{Level1, Level2, Level3} {
			comp, err := Compress(data, l)
			if err != nil {
				t.Fatalf("L%d Compress: %v", l, err)
			}
			got, err := Decompress(comp)
			if err != nil {
				t.Fatalf("L%d Decompress: %v", l, err)
			}
			if !bytes.Equal(data, got) {
				t.Fatalf("L%d mismatch: want %d bytes, got %d", l, len(data), len(got))
			}
		}
	})
}
