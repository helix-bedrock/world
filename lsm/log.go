package lsm

import (
	"encoding/binary"
	"hash/crc32"
	"io"
)

// The log format (LevelDB doc/log_format.md): the file is cut into 32 KB blocks; a record
// is stored as one or more fragments, each with a 7 byte header: masked CRC-32C of the
// type and data (4), data length (2), type (1). A fragment never crosses a block; a block
// tail too short for a header is zero-filled.

const (
	logBlockSize  = 32 << 10
	logHeaderSize = 7
)

// Fragment types.
const (
	logZero   = 0 // preallocated or zero-filled space
	logFull   = 1
	logFirst  = 2
	logMiddle = 3
	logLast   = 4
)

// logWriter appends records to a log.
type logWriter struct {
	w io.Writer
	// offset is where the next write lands within the current block.
	offset int
	header [logHeaderSize]byte
	zeros  [logHeaderSize]byte
}

func newLogWriter(w io.Writer, size int64) *logWriter {
	return &logWriter{w: w, offset: int(size % logBlockSize)}
}

// add appends one record.
func (l *logWriter) add(rec []byte) error {
	first := true
	for {
		if err := l.padBlock(); err != nil {
			return err
		}
		room := logBlockSize - l.offset - logHeaderSize
		n := min(len(rec), room)
		last := n == len(rec)
		if err := l.fragment(fragmentType(first, last), rec[:n]); err != nil {
			return err
		}
		rec, first = rec[n:], false
		if last {
			return nil
		}
	}
}

// padBlock moves to a new block when the current one has no room for a header.
func (l *logWriter) padBlock() error {
	left := logBlockSize - l.offset
	if left >= logHeaderSize {
		return nil
	}
	if left > 0 {
		if _, err := l.w.Write(l.zeros[:left]); err != nil {
			return err
		}
	}
	l.offset = 0
	return nil
}

func fragmentType(first, last bool) byte {
	switch {
	case first && last:
		return logFull
	case first:
		return logFirst
	case last:
		return logLast
	}
	return logMiddle
}

// fragment writes one fragment.
func (l *logWriter) fragment(t byte, data []byte) error {
	c := crc32.Update(crc32.Checksum([]byte{t}, castagnoli()), castagnoli(), data)
	binary.LittleEndian.PutUint32(l.header[0:4], maskCRC(c))
	binary.LittleEndian.PutUint16(l.header[4:6], uint16(len(data))) //nolint:gosec // at most a block
	l.header[6] = t
	if _, err := l.w.Write(l.header[:]); err != nil {
		return err
	}
	if _, err := l.w.Write(data); err != nil {
		return err
	}
	l.offset += logHeaderSize + len(data)
	return nil
}

// logReader reads records from a log. A torn or corrupt tail ends the log, as LevelDB
// treats the last records of a crash.
type logReader struct {
	r     io.Reader
	block [logBlockSize]byte
	data  []byte // what is left of the current block
	eof   bool
	rec   []byte
}

func newLogReader(r io.Reader) *logReader { return &logReader{r: r} }

// next returns the next record, valid until the next call, or io.EOF at the end. A
// corrupt fragment in the middle of the log returns a corrupt error.
func (l *logReader) next() ([]byte, error) {
	l.rec = l.rec[:0]
	inRecord := false
	for {
		t, frag, err := l.fragment()
		if err != nil {
			return nil, err
		}
		switch t {
		case logFull:
			if inRecord {
				return nil, errCorrupt("log: record cut short")
			}
			return frag, nil
		case logFirst:
			if inRecord {
				return nil, errCorrupt("log: record cut short")
			}
			l.rec, inRecord = append(l.rec, frag...), true
		case logMiddle, logLast:
			if !inRecord {
				return nil, errCorrupt("log: fragment without its start")
			}
			l.rec = append(l.rec, frag...)
			if t == logLast {
				return l.rec, nil
			}
		}
	}
}

// fragment returns the next fragment's type and data.
func (l *logReader) fragment() (byte, []byte, error) {
	for {
		if len(l.data) < logHeaderSize {
			if err := l.readBlock(); err != nil {
				return 0, nil, err
			}
			continue
		}
		h := l.data[:logHeaderSize]
		n := int(binary.LittleEndian.Uint16(h[4:6]))
		t := h[6]
		if t == logZero && n == 0 {
			l.data = nil // zero-filled rest of the block
			continue
		}
		if logHeaderSize+n > len(l.data) {
			return 0, nil, l.tornOrCorrupt()
		}
		data := l.data[logHeaderSize : logHeaderSize+n]
		c := crc32.Update(crc32.Checksum([]byte{t}, castagnoli()), castagnoli(), data)
		if unmaskCRC(binary.LittleEndian.Uint32(h[0:4])) != c {
			return 0, nil, l.tornOrCorrupt()
		}
		l.data = l.data[logHeaderSize+n:]
		return t, data, nil
	}
}

// tornOrCorrupt is a bad fragment: the end of the log if it is in the last block (a write
// cut by a crash), corruption otherwise.
func (l *logReader) tornOrCorrupt() error {
	if l.eof {
		return io.EOF
	}
	return errCorrupt("log fragment")
}

// readBlock reads the next block.
func (l *logReader) readBlock() error {
	if l.eof {
		return io.EOF
	}
	n, err := io.ReadFull(l.r, l.block[:])
	switch err {
	case nil:
	case io.EOF:
		return io.EOF
	case io.ErrUnexpectedEOF:
		l.eof = true
	default:
		return err
	}
	l.data = l.block[:n]
	return nil
}
