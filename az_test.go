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

func repeatByte(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
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
	{"all_same_65537", repeatByte(0xAB, 65537)},
	{"random_1K", randBytes(1024)},
	{"random_64K", randBytes(64 << 10)},
	{"random_256K", randBytes(256 << 10)},
	{"patterned_1M", makePatterned(1 << 20)},
	{"patterned_4K", makePatterned(4096)},
}

func TestRoundTrip(t *testing.T) {
	for _, tc := range testCases {
		// Limit levels 4-5 to small inputs to keep the test fast.
		maxLevel := Level5
		if len(tc.data) > 512<<10 {
			maxLevel = Level3
		}
		for level := Level1; level <= maxLevel; level++ {
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

func TestRLEDetection(t *testing.T) {
	// A block of identical bytes should compress extremely well (RLE).
	data := repeatByte(0x55, 1<<16)
	for _, level := range []Level{Level1, Level2, Level3} {
		comp, err := Compress(data, level)
		if err != nil {
			t.Fatalf("L%d: %v", level, err)
		}
		if len(comp) > 64 {
			t.Errorf("L%d: expected RLE compression (<64 bytes), got %d", level, len(comp))
		}
		got, err := Decompress(comp)
		if err != nil {
			t.Fatalf("L%d decompress: %v", level, err)
		}
		if !bytes.Equal(data, got) {
			t.Fatalf("L%d: mismatch after RLE round-trip", level)
		}
	}
}

func TestIncompressibleRaw(t *testing.T) {
	// Random data should pass through as a raw block without blowing up size.
	data := randBytes(64 << 10)
	comp, err := Compress(data, Level1)
	if err != nil {
		t.Fatal(err)
	}
	// Frame overhead + 1 block should be <= len(data) + 64 bytes
	if len(comp) > len(data)+128 {
		t.Errorf("incompressible data too large: %d vs input %d", len(comp), len(data))
	}
}

func TestChecksumMismatch(t *testing.T) {
	data := makePatterned(32 << 10)
	comp, err := Compress(data, Level1)
	if err != nil {
		t.Fatal(err)
	}
	// Flip a byte in the middle of the frame (after the 12-byte header)
	if len(comp) > 20 {
		comp[15] ^= 0xFF
	}
	_, err = Decompress(comp)
	// We expect either a checksum or corruption error
	if err == nil {
		t.Fatal("expected error after corruption, got nil")
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
