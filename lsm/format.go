package lsm

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

// ErrNotFound is returned for a key that is not in the database.
var ErrNotFound = errors.New("lsm: not found")

// ErrClosed is returned by calls on a closed database.
var ErrClosed = errors.New("lsm: closed")

// ErrReadOnly is returned by writes to a database opened ReadOnly.
var ErrReadOnly = errors.New("lsm: opened read-only")

// kind is what an internal key records: a value or a deletion.
type kind uint8

const (
	kindDelete kind = 0
	kindValue  kind = 1
	// kindSeek sorts first among the entries of one user key and sequence
	// number, so a seek to (key, seq, kindSeek) lands on the newest entry at
	// or below seq.
	kindSeek = kindValue
)

// trailerLen is the size of the sequence number and kind after a user key.
const trailerLen = 8

// maxSeq is the largest sequence number: 56 bits.
const maxSeq = 1<<56 - 1

// appendInternalKey appends ukey with its trailer to dst.
func appendInternalKey(dst, ukey []byte, seq uint64, k kind) []byte {
	dst = append(dst, ukey...)
	return binary.LittleEndian.AppendUint64(dst, seq<<8|uint64(k))
}

// splitInternalKey splits an internal key into its user key, sequence number and kind.
func splitInternalKey(ik []byte) (ukey []byte, seq uint64, k kind, ok bool) {
	if len(ik) < trailerLen {
		return nil, 0, 0, false
	}
	n := len(ik) - trailerLen
	t := binary.LittleEndian.Uint64(ik[n:])
	k = kind(t & 0xff)
	if k > kindValue {
		return nil, 0, 0, false
	}
	return ik[:n], t >> 8, k, true
}

// userKey returns the user key of an internal key, or ik itself if it is too short.
func userKey(ik []byte) []byte {
	if len(ik) < trailerLen {
		return ik
	}
	return ik[:len(ik)-trailerLen]
}

// compareInternal orders internal keys: user key ascending, then sequence number and
// kind descending, so the newest entry of a key comes first.
func compareInternal(a, b []byte) int {
	if c := bytes.Compare(userKey(a), userKey(b)); c != 0 {
		return c
	}
	if len(a) < trailerLen || len(b) < trailerLen {
		return len(a) - len(b)
	}
	ta := binary.LittleEndian.Uint64(a[len(a)-trailerLen:])
	tb := binary.LittleEndian.Uint64(b[len(b)-trailerLen:])
	switch {
	case ta > tb:
		return -1
	case ta < tb:
		return 1
	}
	return 0
}

// castagnoli is the CRC-32 table LevelDB checksums with. crc32 keeps one shared copy,
// so asking for it again costs nothing after the first time.
func castagnoli() *crc32.Table { return crc32.MakeTable(crc32.Castagnoli) }

// maskDelta is added to rotated checksums, as LevelDB does, so a checksum stored inside
// checksummed data does not checksum to itself.
const maskDelta = 0xa282ead8

// maskCRC hides a checksum the way LevelDB stores it.
func maskCRC(c uint32) uint32 { return (c>>15 | c<<17) + maskDelta }

// unmaskCRC reverses maskCRC.
func unmaskCRC(m uint32) uint32 {
	r := m - maskDelta
	return r>>17 | r<<15
}

// appendUvarintBytes appends b with its length first.
func appendUvarintBytes(dst, b []byte) []byte {
	dst = binary.AppendUvarint(dst, uint64(len(b)))
	return append(dst, b...)
}

// readUvarintBytes reads a length-prefixed byte string from b, returning it and the rest.
func readUvarintBytes(b []byte) (v, rest []byte, err error) {
	n, m := binary.Uvarint(b)
	if m <= 0 || n > uint64(len(b)-m) {
		return nil, nil, errCorrupt("length-prefixed bytes")
	}
	return b[m : m+int(n)], b[m+int(n):], nil
}

// readUvarint reads one varint from b, returning it and the rest.
func readUvarint(b []byte) (uint64, []byte, error) {
	n, m := binary.Uvarint(b)
	if m <= 0 {
		return 0, nil, errCorrupt("varint")
	}
	return n, b[m:], nil
}

// corruptError is data on disk that does not decode.
type corruptError struct{ what string }

func (e *corruptError) Error() string { return "lsm: corrupt " + e.what }

func errCorrupt(what string) error { return &corruptError{what: what} }

// IsCorrupt reports whether err comes from data on disk that does not decode.
func IsCorrupt(err error) bool {
	var c *corruptError
	return errors.As(err, &c)
}

// File names, as LevelDB names them.

func logName(num uint64) string      { return fmt.Sprintf("%06d.log", num) }
func tableName(num uint64) string    { return fmt.Sprintf("%06d.ldb", num) }
func oldTableName(num uint64) string { return fmt.Sprintf("%06d.sst", num) }
func manifestName(num uint64) string { return fmt.Sprintf("MANIFEST-%06d", num) }

const (
	currentName = "CURRENT"
	lockName    = "LOCK"
)

// fileType is what a file in the database folder is.
type fileType int

const (
	fileOther fileType = iota
	fileLog
	fileTable
	fileManifest
	fileTemp
)

// parseFileName reads the type and number of a database file name.
func parseFileName(name string) (fileType, uint64) {
	var num uint64
	var rest string
	if n, _ := fmt.Sscanf(name, "MANIFEST-%d%s", &num, &rest); n >= 1 && rest == "" {
		return fileManifest, num
	}
	var ext string
	if n, _ := fmt.Sscanf(name, "%d.%s", &num, &ext); n == 2 {
		switch ext {
		case "log":
			return fileLog, num
		case "ldb", "sst":
			return fileTable, num
		case "dbtmp", "tmp":
			return fileTemp, num
		}
	}
	return fileOther, 0
}
