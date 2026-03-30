package az

import (
	"bytes"
	"fmt"
	"testing"
)

func TestFuzzCaseL2(t *testing.T) {
	data := []byte(" the qu000100000000000azy 00g\nthe 00000000000000000000000g\nth0000000")
	fmt.Printf("Input len: %d\n", len(data))

	comp, err := Compress(data, Level2)
	if err != nil {
		t.Fatalf("Compress: %v", err)
	}
	fmt.Printf("Compressed len: %d\n", len(comp))

	got, err := Decompress(comp)
	fmt.Printf("Decompress err: %v, got len: %d\n", err, len(got))
	
	if !bytes.Equal(data, got) {
		for i := 0; i < len(got) && i < len(data); i++ {
			if data[i] != got[i] {
				fmt.Printf("First diff at byte %d: want 0x%02x (%c) got 0x%02x (%c)\n", i, data[i], data[i], got[i], got[i])
				break
			}
		}
	}

	// Also trace the sequences manually
	st := newEncoderState(levelConfigs[Level2])
	result := encodeL2(data, st)
	fmt.Printf("Block type: 0x%02x, seqs count: %d\n", result[0], len(st.seqs))
	for i, s := range st.seqs {
		fmt.Printf("  seq[%d]: litLen=%d matchLen=%d offset=%d repIdx=%d\n", i, s.litLen, s.matchLen, s.offset, s.repIdx)
	}
	fmt.Printf("litBuf: %q\n", st.litBuf)
	fmt.Printf("data:   %q\n", data)
}
