package store

import (
	"fmt"
	"io"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// Level is how hard records are compressed.
type Level int

const (
	// LevelDefault is zstd's default level, about level 3.
	LevelDefault Level = iota
	// LevelFastest is about zstd level 1.
	LevelFastest
	// LevelBetter is about zstd level 7.
	LevelBetter
	// LevelBest is about zstd level 11.
	LevelBest
)

func (l Level) zstd() zstd.EncoderLevel {
	switch l {
	case LevelFastest:
		return zstd.SpeedFastest
	case LevelBetter:
		return zstd.SpeedBetterCompression
	case LevelBest:
		return zstd.SpeedBestCompression
	default:
		return zstd.SpeedDefault
	}
}

const (
	// minCompress is the body size under which compressing is not tried.
	minCompress = 64
	// maxDecoded caps what one Get decodes into memory; bigger records are read
	// with GetStream.
	maxDecoded = 1 << 30
)

// codec compresses and decompresses record bodies. EncodeAll and DecodeAll are
// safe for concurrent use; stream coders are pooled.
type codec struct {
	level      zstd.EncoderLevel
	enc        *zstd.Encoder
	dec        *zstd.Decoder
	streamEncs sync.Pool // *zstd.Encoder
	streamDecs sync.Pool // *zstd.Decoder
}

func newCodec(l Level) (*codec, error) {
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(l.zstd()), zstd.WithEncoderCRC(false))
	if err != nil {
		return nil, fmt.Errorf("store: zstd encoder: %w", err)
	}
	dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(0), zstd.WithDecoderMaxMemory(maxDecoded))
	if err != nil {
		_ = enc.Close()
		return nil, fmt.Errorf("store: zstd decoder: %w", err)
	}
	return &codec{level: l.zstd(), enc: enc, dec: dec}, nil
}

// compressAppend appends body to dst, compressed when that makes it smaller,
// and returns the compression used.
func (c *codec) compressAppend(dst, body []byte) ([]byte, byte) {
	if len(body) < minCompress {
		return append(dst, body...), compNone
	}
	start := len(dst)
	out := c.enc.EncodeAll(body, dst)
	if len(out)-start >= len(body) {
		return append(out[:start], body...), compNone
	}
	return out, compZstd
}

// decode writes the decompressed body into dst[:0].
func (c *codec) decode(dst, body []byte, comp byte, raw uint64) ([]byte, error) {
	if raw > maxDecoded {
		return dst[:0], fmt.Errorf("store: record of %d bytes is too big to read whole", raw)
	}
	if uint64(cap(dst)) < raw {
		dst = make([]byte, 0, raw)
	}
	switch comp {
	case compNone:
		if uint64(len(body)) != raw {
			return dst[:0], fmt.Errorf("%w: body length", ErrCorrupt)
		}
		return append(dst[:0], body...), nil
	case compZstd:
		out, err := c.dec.DecodeAll(body, dst[:0])
		if err != nil {
			return dst[:0], fmt.Errorf("%w: zstd: %w", ErrCorrupt, err)
		}
		if uint64(len(out)) != raw {
			return dst[:0], fmt.Errorf("%w: decoded length", ErrCorrupt)
		}
		return out, nil
	default:
		return dst[:0], fmt.Errorf("%w: unknown compression %d", ErrCorrupt, comp)
	}
}

// streamEncoder returns a zstd encoder writing to w, for a body of size bytes.
func (c *codec) streamEncoder(w io.Writer, size int64) (*zstd.Encoder, error) {
	if e, ok := c.streamEncs.Get().(*zstd.Encoder); ok {
		e.ResetContentSize(w, size)
		return e, nil
	}
	e, err := zstd.NewWriter(w, zstd.WithEncoderLevel(c.level), zstd.WithEncoderCRC(false), zstd.WithEncoderConcurrency(1))
	if err != nil {
		return nil, fmt.Errorf("store: zstd encoder: %w", err)
	}
	e.ResetContentSize(w, size)
	return e, nil
}

func (c *codec) putStreamEncoder(e *zstd.Encoder) { c.streamEncs.Put(e) }

// streamDecoder returns a zstd decoder reading r.
func (c *codec) streamDecoder(r io.Reader) (*zstd.Decoder, error) {
	if d, ok := c.streamDecs.Get().(*zstd.Decoder); ok {
		if err := d.Reset(r); err != nil {
			return nil, fmt.Errorf("store: zstd decoder: %w", err)
		}
		return d, nil
	}
	d, err := zstd.NewReader(r, zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true))
	if err != nil {
		return nil, fmt.Errorf("store: zstd decoder: %w", err)
	}
	return d, nil
}

func (c *codec) putStreamDecoder(d *zstd.Decoder) {
	_ = d.Reset(nil)
	c.streamDecs.Put(d)
}

func (c *codec) close() {
	_ = c.enc.Close()
	c.dec.Close()
}
