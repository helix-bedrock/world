package lsm

import (
	"bytes"
	"fmt"
	"io"

	"github.com/golang/snappy"
	"github.com/klauspost/compress/flate"
	"github.com/klauspost/compress/zlib"
)

// Block compression types as stored in a block trailer.
const (
	blockNone    = 0
	blockSnappy  = 1
	blockZlib    = 2 // zlib with header; older Bedrock worlds
	blockDeflate = 4 // raw deflate; what Bedrock writes
)

// deflateLevel trades a little size for speed, close to zlib's default ratio.
const deflateLevel = 5

// deflater compresses blocks as raw deflate, keeping its writer between blocks: a flate
// writer allocates several hundred kilobytes.
type deflater struct {
	buf bytes.Buffer
	w   *flate.Writer
}

// compress returns src compressed. The result is valid until the next call.
func (d *deflater) compress(src []byte) ([]byte, error) {
	d.buf.Reset()
	if err := d.reset(&d.buf); err != nil {
		return nil, err
	}
	if _, err := d.w.Write(src); err != nil {
		return nil, fmt.Errorf("lsm: deflate: %w", err)
	}
	if err := d.w.Close(); err != nil {
		return nil, fmt.Errorf("lsm: deflate: %w", err)
	}
	return d.buf.Bytes(), nil
}

// reset points the writer at w.
func (d *deflater) reset(w io.Writer) error {
	if d.w != nil {
		d.w.Reset(w)
		return nil
	}
	fw, err := flate.NewWriter(w, deflateLevel)
	if err != nil {
		return fmt.Errorf("lsm: deflate writer: %w", err)
	}
	d.w = fw
	return nil
}

// inflater decompresses blocks, keeping its readers between blocks.
type inflater struct {
	src   bytes.Reader
	flate io.ReadCloser
	zlib  io.ReadCloser
}

// reader returns a reader of the decompressed form of a block of type t read from src.
// src should be an io.ByteReader so nothing past the block is read.
func (in *inflater) reader(src io.Reader, t byte) (io.Reader, error) {
	switch t {
	case blockNone:
		return src, nil
	case blockDeflate:
		return in.flateReader(src)
	case blockZlib:
		return in.zlibReader(src)
	}
	return nil, errCorrupt(fmt.Sprintf("block compression type %d", t))
}

func (in *inflater) flateReader(src io.Reader) (io.Reader, error) {
	if in.flate == nil {
		in.flate = flate.NewReader(src)
		return in.flate, nil
	}
	r, ok := in.flate.(flate.Resetter)
	if !ok {
		return nil, fmt.Errorf("lsm: flate reader cannot be reset")
	}
	if err := r.Reset(src, nil); err != nil {
		return nil, fmt.Errorf("lsm: inflate: %w", err)
	}
	return in.flate, nil
}

func (in *inflater) zlibReader(src io.Reader) (io.Reader, error) {
	if in.zlib == nil {
		zr, err := zlib.NewReader(src)
		if err != nil {
			return nil, errCorrupt("zlib block header")
		}
		in.zlib = zr
		return zr, nil
	}
	r, ok := in.zlib.(zlib.Resetter)
	if !ok {
		return nil, fmt.Errorf("lsm: zlib reader cannot be reset")
	}
	if err := r.Reset(src, nil); err != nil {
		return nil, errCorrupt("zlib block header")
	}
	return in.zlib, nil
}

// inflateGuess is how many times its compressed size a block is guessed to inflate to.
// Bedrock's chunk data compresses about this well; a wrong guess only costs a regrow.
const inflateGuess = 4

// decompress returns the decompressed form of a whole block of type t.
func (in *inflater) decompress(src []byte, t byte) ([]byte, error) {
	switch t {
	case blockNone:
		return src, nil
	case blockSnappy:
		out, err := snappy.Decode(nil, src)
		if err != nil {
			return nil, errCorrupt("snappy block")
		}
		return out, nil
	}
	in.src.Reset(src)
	r, err := in.reader(&in.src, t)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	out.Grow(len(src) * inflateGuess)
	if _, err := out.ReadFrom(r); err != nil {
		return nil, errCorrupt(fmt.Sprintf("compressed block: %v", err))
	}
	return out.Bytes(), nil
}
