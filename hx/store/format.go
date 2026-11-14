package store

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

// File layout: a header, then two superblock slots, each in a 4 KB sector of
// its own so a torn write of one never reaches the other, then records.
const (
	fileMagic     = "HXST"
	fileVersion   = 1
	fileHeaderLen = 12
	slotSize      = 4096
	superAOff     = slotSize
	superBOff     = 2 * slotSize
	dataStart     = 3 * slotSize
	superMagic    = "HXSB"
	superLen      = 48
)

// Record layout: a 40-byte header, the key, then the body.
const (
	recMagic     = "HXR\x01"
	recHeaderLen = 40
	// recordAlign is where records start: small records share space well, and a
	// scan of a damaged file can find them.
	recordAlign = 64
	// MaxKeyLen is the longest key a record may have.
	MaxKeyLen = 4096
)

// Compression of a record's body.
const (
	compNone byte = 0
	compZstd byte = 1
)

// kindIndex marks the store's own index records; callers' kinds start at 1.
const kindIndex byte = 0

var (
	// ErrNotFound is returned for a key with no record.
	ErrNotFound = errors.New("store: not found")
	// ErrClosed is returned after Close.
	ErrClosed = errors.New("store: closed")
	// ErrCorrupt is wrapped by every error about damaged data.
	ErrCorrupt = errors.New("store: corrupt")
	// ErrReleased is returned by a snapshot after Release.
	ErrReleased = errors.New("store: snapshot released")
)

func errCorrupt(path, what string) error {
	return fmt.Errorf("%w: %s: %s", ErrCorrupt, path, what)
}

// castagnoli returns the CRC-32C table; the standard library builds it once.
func castagnoli() *crc32.Table { return crc32.MakeTable(crc32.Castagnoli) }

func alignUp(n uint64) uint64 { return (n + recordAlign - 1) &^ (recordAlign - 1) }

// recHeader is a record's header.
type recHeader struct {
	kind   byte
	comp   byte
	keyLen int
	round  uint64
	// stored is the body's length on disk, raw its length decompressed.
	stored  uint64
	raw     uint64
	bodyCRC uint32
}

// size is the record's length on disk, before alignment.
func (h recHeader) size() uint64 { return recHeaderLen + uint64(h.keyLen) + h.stored } //nolint:gosec // key length is bounded

// put writes the header into b[:recHeaderLen]; the header checksum covers the
// header and key.
func (h recHeader) put(b, key []byte) {
	copy(b[0:4], recMagic)
	b[4] = h.kind
	b[5] = h.comp
	binary.LittleEndian.PutUint16(b[6:8], uint16(h.keyLen)) //nolint:gosec // at most MaxKeyLen
	binary.LittleEndian.PutUint64(b[8:16], h.round)
	binary.LittleEndian.PutUint64(b[16:24], h.stored)
	binary.LittleEndian.PutUint64(b[24:32], h.raw)
	binary.LittleEndian.PutUint32(b[32:36], h.bodyCRC)
	binary.LittleEndian.PutUint32(b[36:40], headerCRC(b, key))
}

func headerCRC(b, key []byte) uint32 {
	t := castagnoli()
	return crc32.Update(crc32.Checksum(b[:36], t), t, key)
}

// parseHeader reads the header at the start of b and checks its magic and its
// checksum against the key that follows it in b.
func parseHeader(b []byte) (recHeader, bool) {
	if len(b) < recHeaderLen || string(b[0:4]) != recMagic {
		return recHeader{}, false
	}
	h := recHeader{
		kind:    b[4],
		comp:    b[5],
		keyLen:  int(binary.LittleEndian.Uint16(b[6:8])),
		round:   binary.LittleEndian.Uint64(b[8:16]),
		stored:  binary.LittleEndian.Uint64(b[16:24]),
		raw:     binary.LittleEndian.Uint64(b[24:32]),
		bodyCRC: binary.LittleEndian.Uint32(b[32:36]),
	}
	if len(b) < recHeaderLen+h.keyLen {
		return recHeader{}, false
	}
	if headerCRC(b, b[recHeaderLen:recHeaderLen+h.keyLen]) != binary.LittleEndian.Uint32(b[36:40]) {
		return recHeader{}, false
	}
	return h, true
}

// superblock says where a file's index record is, as of a round.
type superblock struct {
	// seq counts checkpoints; the slot with the higher valid seq is current.
	seq       uint64
	round     uint64
	indexOff  uint64
	indexSize uint64
	end       uint64
}

// slot is where this superblock is written: odd checkpoints in A, even in B.
func (sb superblock) slot() int64 {
	if sb.seq%2 == 1 {
		return superAOff
	}
	return superBOff
}

func (sb superblock) encode(b []byte) {
	copy(b[0:4], superMagic)
	binary.LittleEndian.PutUint64(b[4:12], sb.seq)
	binary.LittleEndian.PutUint64(b[12:20], sb.round)
	binary.LittleEndian.PutUint64(b[20:28], sb.indexOff)
	binary.LittleEndian.PutUint64(b[28:36], sb.indexSize)
	binary.LittleEndian.PutUint64(b[36:44], sb.end)
	binary.LittleEndian.PutUint32(b[44:48], crc32.Checksum(b[:44], castagnoli()))
}

func decodeSuper(b []byte) (superblock, bool) {
	if len(b) < superLen || string(b[0:4]) != superMagic {
		return superblock{}, false
	}
	if crc32.Checksum(b[:44], castagnoli()) != binary.LittleEndian.Uint32(b[44:48]) {
		return superblock{}, false
	}
	return superblock{
		seq:       binary.LittleEndian.Uint64(b[4:12]),
		round:     binary.LittleEndian.Uint64(b[12:20]),
		indexOff:  binary.LittleEndian.Uint64(b[20:28]),
		indexSize: binary.LittleEndian.Uint64(b[28:36]),
		end:       binary.LittleEndian.Uint64(b[36:44]),
	}, true
}

func encodeFileHeader(b []byte) {
	copy(b[0:4], fileMagic)
	binary.LittleEndian.PutUint16(b[4:6], fileVersion)
	b[6], b[7] = 0, 0
	binary.LittleEndian.PutUint32(b[8:12], crc32.Checksum(b[:8], castagnoli()))
}

// checkFileHeader reports whether b starts with a file header this package reads.
func checkFileHeader(b []byte) bool {
	if len(b) < fileHeaderLen || string(b[0:4]) != fileMagic {
		return false
	}
	if crc32.Checksum(b[:8], castagnoli()) != binary.LittleEndian.Uint32(b[8:12]) {
		return false
	}
	return binary.LittleEndian.Uint16(b[4:6]) == fileVersion
}
