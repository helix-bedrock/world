package hx

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"

	"github.com/helix-bedrock/world/hx/store"
)

// A backup archive is one stream, for anywhere a folder does not fit: a file,
// a socket, cloud storage. It starts with archiveMagic, then frames, each a
// type byte and its fields; the last frame, Finish, ends with the CRC-32C of
// every byte before it. Records carry their own checksums too.
//
//	record: file, key (uvarint length + bytes each), kind, comp, uvarint size,
//	        uvarint stored, u32 crc, stored bytes
//	keys:   file, uvarint length, key list
//	file:   name, uvarint size, bytes
//	finish: uvarint length, BackupInfo JSON, u32 crc of the stream so far
const archiveMagic = "HXBK\x01"

const (
	frameRecord byte = 1
	frameKeys   byte = 2
	frameFile   byte = 3
	frameFinish byte = 4
)

// maxArchiveName caps file names and keys read from an archive.
const maxArchiveName = store.MaxKeyLen

// ArchiveWriter writes a backup as one stream. Finish flushes it; closing the
// underlying writer is the caller's job.
type ArchiveWriter struct {
	w       *bufio.Writer
	crc     hash.Hash32
	started bool
	tmp     [binary.MaxVarintLen64 + 8]byte
}

// NewArchiveWriter returns a target writing an archive to w.
func NewArchiveWriter(w io.Writer) *ArchiveWriter {
	a := &ArchiveWriter{crc: crc32.New(crc32.MakeTable(crc32.Castagnoli))}
	a.w = bufio.NewWriterSize(io.MultiWriter(w, a.crc), 256<<10)
	return a
}

func (a *ArchiveWriter) begin() error {
	if a.started {
		return nil
	}
	a.started = true
	_, err := a.w.WriteString(archiveMagic)
	return err
}

func (a *ArchiveWriter) uvarint(v uint64) error {
	_, err := a.w.Write(binary.AppendUvarint(a.tmp[:0], v))
	return err
}

func (a *ArchiveWriter) bytes(b []byte) error {
	if err := a.uvarint(uint64(len(b))); err != nil {
		return err
	}
	_, err := a.w.Write(b)
	return err
}

func (a *ArchiveWriter) str(s string) error {
	if err := a.uvarint(uint64(len(s))); err != nil {
		return err
	}
	_, err := a.w.WriteString(s)
	return err
}

// Record implements BackupTarget.
func (a *ArchiveWriter) Record(file string, key []byte, h store.RawHeader, body io.Reader) error {
	errs := []error{a.begin(), a.w.WriteByte(frameRecord), a.str(file), a.bytes(key), a.w.WriteByte(h.Kind), a.w.WriteByte(h.Comp),
		a.uvarint(uint64(h.Size)), a.uvarint(uint64(h.Stored))} //nolint:gosec // valid headers have no negative sizes
	if err := errors.Join(errs...); err != nil {
		return err
	}
	if _, err := a.w.Write(binary.LittleEndian.AppendUint32(a.tmp[:0], h.CRC)); err != nil {
		return err
	}
	n, err := io.Copy(a.w, io.LimitReader(body, h.Stored))
	if err == nil && n != h.Stored {
		err = io.ErrUnexpectedEOF
	}
	return err
}

// Keys implements BackupTarget.
func (a *ArchiveWriter) Keys(file string, keys []byte) error {
	return errors.Join(a.begin(), a.w.WriteByte(frameKeys), a.str(file), a.bytes(keys))
}

// File implements BackupTarget.
func (a *ArchiveWriter) File(name string, r io.Reader, size int64) error {
	if err := errors.Join(a.begin(), a.w.WriteByte(frameFile), a.str(name), a.uvarint(uint64(size))); err != nil { //nolint:gosec // file sizes
		return err
	}
	n, err := io.Copy(a.w, io.LimitReader(r, size))
	if err == nil && n != size {
		err = io.ErrUnexpectedEOF
	}
	return err
}

// Finish implements BackupTarget.
func (a *ArchiveWriter) Finish(info BackupInfo) error {
	b, err := json.Marshal(info)
	if err != nil {
		return err
	}
	if err := errors.Join(a.begin(), a.w.WriteByte(frameFinish), a.bytes(b), a.w.Flush()); err != nil {
		return err
	}
	// The checksum covers everything written so far, and is written past the
	// hash.
	sum := binary.LittleEndian.AppendUint32(nil, a.crc.Sum32())
	a.crc = crc32.New(crc32.MakeTable(crc32.Castagnoli))
	if _, err := a.w.Write(sum); err != nil {
		return err
	}
	return a.w.Flush()
}

var errArchive = errors.New("hx: corrupt backup archive")

// ReadArchive replays an archive into t: into a DirTarget to unpack it as a
// backup folder (then Restore), or into another archive. t.Finish is called
// only once the whole stream checked out.
func ReadArchive(r io.Reader, t BackupTarget) error {
	ar := &archiveReader{r: &hashReader{br: bufio.NewReaderSize(r, 256<<10), h: crc32.New(crc32.MakeTable(crc32.Castagnoli))}}
	magic := make([]byte, len(archiveMagic))
	if _, err := io.ReadFull(ar.r, magic); err != nil || string(magic) != archiveMagic {
		return fmt.Errorf("%w: not a backup archive", errArchive)
	}
	for {
		typ, err := ar.r.ReadByte()
		if err != nil {
			return fmt.Errorf("%w: cut short", errArchive)
		}
		switch typ {
		case frameRecord:
			err = ar.record(t)
		case frameKeys:
			err = ar.keys(t)
		case frameFile:
			err = ar.file(t)
		case frameFinish:
			return ar.finish(t)
		default:
			return fmt.Errorf("%w: unknown frame %d", errArchive, typ)
		}
		if err != nil {
			return err
		}
	}
}

type archiveReader struct {
	r   *hashReader
	buf []byte
}

// hashReader checksums the bytes it hands out (not what it buffers ahead).
type hashReader struct {
	br  *bufio.Reader
	h   hash.Hash32
	one [1]byte
}

func (hr *hashReader) Read(p []byte) (int, error) {
	n, err := hr.br.Read(p)
	hr.h.Write(p[:n])
	return n, err
}

func (hr *hashReader) ReadByte() (byte, error) {
	b, err := hr.br.ReadByte()
	if err == nil {
		hr.one[0] = b
		hr.h.Write(hr.one[:])
	}
	return b, err
}

func (ar *archiveReader) uvarint() (uint64, error) {
	v, err := binary.ReadUvarint(ar.r)
	if err != nil {
		return 0, fmt.Errorf("%w: cut short", errArchive)
	}
	return v, nil
}

// bytes reads a length-prefixed field of at most limit bytes into ar.buf.
func (ar *archiveReader) bytes(limit uint64) ([]byte, error) {
	n, err := ar.uvarint()
	if err != nil {
		return nil, err
	}
	if n > limit {
		return nil, fmt.Errorf("%w: field of %d bytes", errArchive, n)
	}
	if uint64(cap(ar.buf)) < n {
		ar.buf = make([]byte, n)
	}
	ar.buf = ar.buf[:n]
	if _, err := io.ReadFull(ar.r, ar.buf); err != nil {
		return nil, fmt.Errorf("%w: cut short", errArchive)
	}
	return ar.buf, nil
}

func (ar *archiveReader) record(t BackupTarget) error {
	file, err := ar.bytes(maxArchiveName)
	if err != nil {
		return err
	}
	name := string(file)
	k, err := ar.bytes(maxArchiveName)
	if err != nil {
		return err
	}
	key := append([]byte(nil), k...)
	var fixed [2]byte
	if _, err := io.ReadFull(ar.r, fixed[:]); err != nil {
		return fmt.Errorf("%w: cut short", errArchive)
	}
	size, err := ar.uvarint()
	if err != nil {
		return err
	}
	stored, err := ar.uvarint()
	if err != nil {
		return err
	}
	var crc [4]byte
	if _, err := io.ReadFull(ar.r, crc[:]); err != nil {
		return fmt.Errorf("%w: cut short", errArchive)
	}
	if size > 1<<62 || stored > 1<<62 {
		return fmt.Errorf("%w: record size", errArchive)
	}
	h := store.RawHeader{Kind: fixed[0], Comp: fixed[1], Size: int64(size), Stored: int64(stored), CRC: binary.LittleEndian.Uint32(crc[:])} //nolint:gosec // checked above
	body := io.LimitReader(ar.r, h.Stored)
	if err := t.Record(name, key, h, body); err != nil {
		return err
	}
	// Whatever the target did not read is skipped, so the next frame lines up.
	_, err = io.Copy(io.Discard, body)
	return err
}

func (ar *archiveReader) keys(t BackupTarget) error {
	file, err := ar.bytes(maxArchiveName)
	if err != nil {
		return err
	}
	name := string(file)
	list, err := ar.bytes(1 << 32)
	if err != nil {
		return err
	}
	return t.Keys(name, list)
}

func (ar *archiveReader) file(t BackupTarget) error {
	file, err := ar.bytes(maxArchiveName)
	if err != nil {
		return err
	}
	name := string(file)
	size, err := ar.uvarint()
	if err != nil {
		return err
	}
	if size > 1<<62 {
		return fmt.Errorf("%w: file size", errArchive)
	}
	body := io.LimitReader(ar.r, int64(size))               //nolint:gosec // checked above
	if err := t.File(name, body, int64(size)); err != nil { //nolint:gosec // checked above
		return err
	}
	_, err = io.Copy(io.Discard, body)
	return err
}

func (ar *archiveReader) finish(t BackupTarget) error {
	b, err := ar.bytes(1 << 20)
	if err != nil {
		return err
	}
	var info BackupInfo
	if err := json.Unmarshal(b, &info); err != nil {
		return fmt.Errorf("%w: backup info: %w", errArchive, err)
	}
	want := ar.r.h.Sum32()
	var sum [4]byte
	if _, err := io.ReadFull(ar.r, sum[:]); err != nil {
		return fmt.Errorf("%w: cut short", errArchive)
	}
	if binary.LittleEndian.Uint32(sum[:]) != want {
		return fmt.Errorf("%w: stream checksum", errArchive)
	}
	return t.Finish(info)
}
