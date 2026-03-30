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
		blockSize:  8 << 20, // 8MB: 7 blocks on 51MB → all process in 1 parallel round
		windowSize: 8 << 20,
		shortBits:  20, // 1M entries; 8:1 collision for 8MB block — fast, single hash
		longBits:   0,  // no long hash: simplest possible match finding
		lazyDepth:  0,
		chainDepth: 0,
		litMode:    entropyNone, // raw literals: no Huffman overhead, simpler/faster
		seqMode:    entropyNone,
		bsID:       bsID8M,
	},
	{
		blockSize:  8 << 20, // 8MB: same window as L1; dual hash + Huffman beats L1's ratio
		windowSize: 8 << 20,
		shortBits:  20, // 1M entries; 8:1 collision for 8MB block — greedy so still fast
		longBits:   18, // 256K entries for 8-byte hash; finds longer matches than L1
		lazyDepth:  0,
		chainDepth: 0,
		litMode:    entropyAdaptHuff, // Compress4X for large litBufs (8MB blocks)
		seqMode:    entropyFSE,
		bsID:       bsID8M,
	},
	{
		blockSize:  8 << 20, // 8MB block = widest practical matching scope
		windowSize: 8 << 20,
		shortBits:  19, // 512K entries; 16:1 collision for 8MB block
		longBits:   21,
		lazyDepth:  2,
		chainDepth: 8,
		litMode:    entropyAdaptHuff,
		seqMode:    entropyFSE,
		bsID:       bsID8M,
	},
	{
		blockSize:  8 << 20,
		windowSize: 8 << 20,
		shortBits:  19, // 512K entries; deeper search than L3
		longBits:   22,
		lazyDepth:  4,
		chainDepth: 32,
		litMode:    entropyAdaptHuff,
		seqMode:    entropyFSE,
		bsID:       bsID8M,
	},
	{
		blockSize:  8 << 20,
		windowSize: 8 << 20,
		shortBits:  20, // 1M entries for optimal parse; chain gives deep search
		longBits:   22,
		lazyDepth:  0, // optimal parse (special case)
		chainDepth: 4, // shallow chain: parallelism × fewer cache misses outweighs deep search
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
