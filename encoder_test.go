package az

import (
	"bytes"
	"testing"
)

// allLevels returns every valid level.
func allLevels() []Level {
	return []Level{Level1, Level2, Level3, Level4, Level5}
}

// levelsFor mirrors TestRoundTrip's level limiting so large inputs stay fast.
func levelsFor(data []byte) []Level {
	if len(data) > 512<<10 {
		return []Level{Level1, Level2, Level3}
	}
	return allLevels()
}

// ─── Round-trip via Encoder/Decoder ─────────────────────────────────────────────

func TestEncodeDecodeRoundTrip(t *testing.T) {
	enc := NewEncoder()
	dec := NewDecoder()
	for _, tc := range testCases {
		for _, level := range levelsFor(tc.data) {
			tc, level := tc, level
			t.Run(tc.name+"/L"+string(rune('0'+level)), func(t *testing.T) {
				frame, err := enc.EncodeAll(nil, tc.data, level)
				if err != nil {
					t.Fatalf("EncodeAll: %v", err)
				}
				got, err := dec.DecodeAll(nil, frame)
				if err != nil {
					t.Fatalf("DecodeAll: %v", err)
				}
				if !bytes.Equal(tc.data, got) {
					t.Fatalf("round-trip mismatch: input %d bytes, got %d bytes", len(tc.data), len(got))
				}
			})
		}
	}
}

// ─── Format identity: the load-bearing test ─────────────────────────────────────

func TestEncodeAllFormatIdentity(t *testing.T) {
	enc := NewEncoder()
	for _, tc := range testCases {
		for _, level := range levelsFor(tc.data) {
			tc, level := tc, level
			t.Run(tc.name+"/L"+string(rune('0'+level)), func(t *testing.T) {
				want, err := Compress(tc.data, level)
				if err != nil {
					t.Fatalf("Compress: %v", err)
				}
				got, err := enc.EncodeAll(nil, tc.data, level)
				if err != nil {
					t.Fatalf("EncodeAll: %v", err)
				}
				if !bytes.Equal(want, got) {
					t.Fatalf("EncodeAll != Compress: want %d bytes, got %d bytes", len(want), len(got))
				}
			})
		}
	}
}

func TestDecodeAllFormatIdentity(t *testing.T) {
	dec := NewDecoder()
	for _, tc := range testCases {
		for _, level := range levelsFor(tc.data) {
			tc, level := tc, level
			t.Run(tc.name+"/L"+string(rune('0'+level)), func(t *testing.T) {
				frame, err := Compress(tc.data, level)
				if err != nil {
					t.Fatalf("Compress: %v", err)
				}
				want, err := Decompress(frame)
				if err != nil {
					t.Fatalf("Decompress: %v", err)
				}
				got, err := dec.DecodeAll(nil, frame)
				if err != nil {
					t.Fatalf("DecodeAll: %v", err)
				}
				if !bytes.Equal(want, got) {
					t.Fatalf("DecodeAll != Decompress: want %d bytes, got %d bytes", len(want), len(got))
				}
			})
		}
	}
}

// ─── Reuse correctness: no state bleed across calls ──────────────────────────────

func TestEncoderReuseNoStateBleed(t *testing.T) {
	enc := NewEncoder()
	dec := NewDecoder()
	// Interleave levels and inputs through a single Encoder/Decoder pair and
	// confirm every frame is independent and correct.
	inputs := [][]byte{
		makePatterned(4096),
		randBytes(8192),
		nil,
		make([]byte, 32<<10),
		makePatterned(1 << 16),
		[]byte("hello world"),
	}
	for round := 0; round < 3; round++ {
		for i, in := range inputs {
			for _, level := range allLevels() {
				frame, err := enc.EncodeAll(nil, in, level)
				if err != nil {
					t.Fatalf("EncodeAll round %d input %d L%d: %v", round, i, level, err)
				}
				// Cross-check against a fresh Compress to catch any bleed.
				want, err := Compress(in, level)
				if err != nil {
					t.Fatalf("Compress: %v", err)
				}
				if !bytes.Equal(want, frame) {
					t.Fatalf("reuse bleed at round %d input %d L%d: frame differs from Compress", round, i, level)
				}
				got, err := dec.DecodeAll(nil, frame)
				if err != nil {
					t.Fatalf("DecodeAll round %d input %d L%d: %v", round, i, level, err)
				}
				if !bytes.Equal(in, got) {
					t.Fatalf("reuse round-trip mismatch at round %d input %d L%d", round, i, level)
				}
			}
		}
	}
}

// TestDecoderInterleavedFormats confirms one Decoder handles lz4- and
// zstd-format inputs interleaved without state bleed.
func TestDecoderInterleavedFormats(t *testing.T) {
	dec := NewDecoder()
	data := makePatterned(50000)
	// L2 is lz4, L4 is zstd; alternate them.
	seq := []Level{Level2, Level4, Level2, Level4, Level1, Level5, Level3}
	for i, level := range seq {
		frame, err := Compress(data, level)
		if err != nil {
			t.Fatalf("Compress: %v", err)
		}
		got, err := dec.DecodeAll(nil, frame)
		if err != nil {
			t.Fatalf("DecodeAll step %d L%d: %v", i, level, err)
		}
		if !bytes.Equal(data, got) {
			t.Fatalf("interleaved decode mismatch at step %d L%d", i, level)
		}
	}
}

// ─── dst append semantics ────────────────────────────────────────────────────────

func TestEncodeDecodeAppendToNonEmptyDst(t *testing.T) {
	enc := NewEncoder()
	dec := NewDecoder()
	src := makePatterned(20000)
	prefix := []byte("PREFIX")

	frame, err := enc.EncodeAll(append([]byte(nil), prefix...), src, Level3)
	if err != nil {
		t.Fatalf("EncodeAll: %v", err)
	}
	if !bytes.HasPrefix(frame, prefix) {
		t.Fatalf("EncodeAll did not preserve dst prefix")
	}
	bare, err := enc.EncodeAll(nil, src, Level3)
	if err != nil {
		t.Fatalf("EncodeAll: %v", err)
	}
	if !bytes.Equal(frame[len(prefix):], bare) {
		t.Fatalf("appended frame differs from bare frame")
	}

	out, err := dec.DecodeAll(append([]byte(nil), prefix...), bare)
	if err != nil {
		t.Fatalf("DecodeAll: %v", err)
	}
	if !bytes.HasPrefix(out, prefix) || !bytes.Equal(out[len(prefix):], src) {
		t.Fatalf("DecodeAll did not append into dst correctly")
	}
}

// ─── Error handling ──────────────────────────────────────────────────────────────

func TestEncodeAllInvalidLevel(t *testing.T) {
	enc := NewEncoder()
	for _, level := range []Level{0, -1, 6, 100} {
		if _, err := enc.EncodeAll(nil, []byte("x"), level); err != ErrLevel {
			t.Fatalf("level %d: want ErrLevel, got %v", level, err)
		}
	}
}

func TestDecodeAllCorrupted(t *testing.T) {
	dec := NewDecoder()
	// Unknown magic.
	if _, err := dec.DecodeAll(nil, []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x00}); err != ErrCorrupted {
		t.Fatalf("bad magic: want ErrCorrupted, got %v", err)
	}
	// Too short to hold a magic: decodes to nothing, no error (matches Reader).
	for _, short := range [][]byte{nil, {0x01}, {0x01, 0x02, 0x03}} {
		got, err := dec.DecodeAll(nil, short)
		if err != nil || len(got) != 0 {
			t.Fatalf("short input %v: want (empty,nil), got (%d bytes, %v)", short, len(got), err)
		}
	}
}
