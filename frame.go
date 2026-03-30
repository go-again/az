package az

import (
	"encoding/binary"
	"io"
)

const (
	frameMagic uint32 = 0x415A0001 // 'A','Z',0x00,0x01

	// FLG byte bit positions
	flgChecksumPresent uint8 = 1 << 6
	flgSizePresent     uint8 = 1 << 5

	// BlockHeader bit positions (uint32)
	blkIsLast uint32 = 1 << 31
	blkIsRaw  uint32 = 1 << 30
)

// frameHeader holds the decoded frame header fields.
type frameHeader struct {
	level       Level
	blockSize   int
	contentSize uint64
	hasSize     bool
	checksum    bool
}

// writeFrameHeader serialises the frame header into w.
func writeFrameHeader(w io.Writer, opts Options, contentSize int64) error {
	cfg := levelConfigs[opts.Level]

	var flg uint8
	flg |= uint8(opts.Level) & 0x0F
	if opts.Checksum {
		flg |= flgChecksumPresent
	}
	if opts.ContentSize && contentSize >= 0 {
		flg |= flgSizePresent
	}

	blk := cfg.bsID << 4

	// Build header bytes for checksum computation
	var hdr []byte
	hdr = append(hdr, flg, blk)
	if opts.ContentSize && contentSize >= 0 {
		var buf [8]byte
		binary.LittleEndian.PutUint64(buf[:], uint64(contentSize))
		hdr = append(hdr, buf[:]...)
	}

	d := newXXH64()
	_, _ = d.Write(hdr)
	hdrCksum := d.Sum32()

	// Magic
	var magic [4]byte
	binary.LittleEndian.PutUint32(magic[:], frameMagic)
	if _, err := w.Write(magic[:]); err != nil {
		return err
	}
	if _, err := w.Write(hdr); err != nil {
		return err
	}
	var cs [4]byte
	binary.LittleEndian.PutUint32(cs[:], hdrCksum)
	_, err := w.Write(cs[:])
	return err
}

// readFrameHeader reads and validates the frame header from r.
func readFrameHeader(r io.Reader) (frameHeader, error) {
	var magic [4]byte
	if _, err := io.ReadFull(r, magic[:]); err != nil {
		return frameHeader{}, err
	}
	if binary.LittleEndian.Uint32(magic[:]) != frameMagic {
		return frameHeader{}, ErrInvalidMagic
	}

	var flgBlk [2]byte
	if _, err := io.ReadFull(r, flgBlk[:]); err != nil {
		return frameHeader{}, ErrCorrupted
	}
	flg := flgBlk[0]
	blk := flgBlk[1]

	fh := frameHeader{
		level:    Level(flg & 0x0F),
		checksum: flg&flgChecksumPresent != 0,
		hasSize:  flg&flgSizePresent != 0,
	}
	fh.blockSize = blockSizeFromID(blk >> 4)

	hdrBytes := []byte{flg, blk}

	if fh.hasSize {
		var sz [8]byte
		if _, err := io.ReadFull(r, sz[:]); err != nil {
			return frameHeader{}, ErrCorrupted
		}
		fh.contentSize = binary.LittleEndian.Uint64(sz[:])
		hdrBytes = append(hdrBytes, sz[:]...)
	}

	var cs [4]byte
	if _, err := io.ReadFull(r, cs[:]); err != nil {
		return frameHeader{}, ErrCorrupted
	}
	stored := binary.LittleEndian.Uint32(cs[:])

	d := newXXH64()
	_, _ = d.Write(hdrBytes)
	if d.Sum32() != stored {
		return frameHeader{}, ErrChecksumFail
	}

	if fh.level < minLevel || fh.level > maxLevel {
		return frameHeader{}, ErrCorrupted
	}
	return fh, nil
}

// writeBlockHeader encodes a 4-byte block header into dst and returns the
// updated slice.  compSize must fit in 30 bits.
func writeBlockHeader(dst []byte, isLast, isRaw bool, compSize int) []byte {
	h := uint32(compSize)
	if isLast {
		h |= blkIsLast
	}
	if isRaw {
		h |= blkIsRaw
	}
	return binary.LittleEndian.AppendUint32(dst, h)
}

type blockHeader struct {
	isLast  bool
	isRaw   bool
	size    int
}

func readBlockHeader(r io.Reader) (blockHeader, error) {
	var buf [4]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return blockHeader{}, err
	}
	h := binary.LittleEndian.Uint32(buf[:])
	return blockHeader{
		isLast: h&blkIsLast != 0,
		isRaw:  h&blkIsRaw != 0,
		size:   int(h & 0x3FFFFFFF),
	}, nil
}

// writeEndOfStream writes the final zero-size block and optional content checksum.
func writeEndOfStream(w io.Writer, contentCksum uint32, writeChecksum bool) error {
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], uint32(blkIsLast))
	if _, err := w.Write(buf[:]); err != nil {
		return err
	}
	if writeChecksum {
		binary.LittleEndian.PutUint32(buf[:], contentCksum)
		_, err := w.Write(buf[:])
		return err
	}
	return nil
}
