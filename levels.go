package az

type entropyMode uint8

const (
	entropyNone       entropyMode = 0 // raw literals, LZ4-style tokens
	entropyStaticHuff entropyMode = 1 // huff0 Compress1X on literals, raw tokens
	entropyAdaptHuff  entropyMode = 2 // huff0 adaptive (1X or 4X) on literals
	entropyFSE        entropyMode = 3 // FSE-coded sequence metadata
)

// blockSizeID encodes the block size in the frame BLK byte (bits 7-4).
const (
	bsID64K  = 4
	bsID256K = 5
	bsID1M   = 6
	bsID4M   = 7
	bsID8M   = 8
)

type levelConfig struct {
	blockSize  int
	windowSize int
	shortBits  uint8 // hash bits for short (4-byte) table
	longBits   uint8 // hash bits for long (8-byte) table; 0 = no long table
	lazyDepth  int   // 0=greedy, 1=lazy+1, 2=lazy+2
	chainDepth int   // 0=no chain, >0=chain depth for levels 4-5
	litMode    entropyMode
	seqMode    entropyMode
	bsID       uint8 // block-size-id for frame header
}

var levelConfigs = [6]levelConfig{
	{}, // index 0 unused
	{
		blockSize:  64 << 10,
		windowSize: 64 << 10,
		shortBits:  16,
		longBits:   0,
		lazyDepth:  0,
		chainDepth: 0,
		litMode:    entropyNone,
		seqMode:    entropyNone,
		bsID:       bsID64K,
	},
	{
		blockSize:  256 << 10,
		windowSize: 256 << 10,
		shortBits:  16,
		longBits:   18,
		lazyDepth:  0,
		chainDepth: 0,
		litMode:    entropyStaticHuff,
		seqMode:    entropyNone,
		bsID:       bsID256K,
	},
	{
		blockSize:  1 << 20,
		windowSize: 1 << 20,
		shortBits:  16,
		longBits:   20,
		lazyDepth:  1,
		chainDepth: 0,
		litMode:    entropyAdaptHuff,
		seqMode:    entropyFSE,
		bsID:       bsID1M,
	},
	{
		blockSize:  4 << 20,
		windowSize: 4 << 20,
		shortBits:  17,
		longBits:   22,
		lazyDepth:  2,
		chainDepth: 4,
		litMode:    entropyAdaptHuff,
		seqMode:    entropyFSE,
		bsID:       bsID4M,
	},
	{
		blockSize:  8 << 20,
		windowSize: 8 << 20,
		shortBits:  17,
		longBits:   23,
		lazyDepth:  0, // optimal parse (special case)
		chainDepth: 16,
		litMode:    entropyAdaptHuff,
		seqMode:    entropyFSE,
		bsID:       bsID8M,
	},
}

// blockSizeFromID maps a BLK bits7-4 value to bytes.
func blockSizeFromID(id uint8) int {
	switch id {
	case bsID64K:
		return 64 << 10
	case bsID256K:
		return 256 << 10
	case bsID1M:
		return 1 << 20
	case bsID4M:
		return 4 << 20
	case bsID8M:
		return 8 << 20
	default:
		return 1 << 20
	}
}
