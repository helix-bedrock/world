package leveldb

import (
	"bytes"
	"io"
	"sync"

	"github.com/helix-bedrock/world/leveldb/opt"
)

// Helix: streamed values. Bedrock keeps all block entities of a chunk in one value, which a
// chunk of chests full of books makes hundreds of megabytes. GetStream reads a value as a
// stream and Transaction.PutStream writes one, so such a value is never held whole: a big
// value is written as a block of its own, inflated from the file as it is read, and copied
// as a stream by compaction (table/stream.go).

// GetStream returns the value of key as a stream, with its size, or ErrNotFound. A value in a
// big table block is inflated from the file as it is read; others are copied. The tables
// read stay on disk until the reader is closed, which must be done. Safe for concurrent use.
func (db *DB) GetStream(key []byte, ro *opt.ReadOptions) (io.ReadCloser, int64, error) {
	if err := db.ok(); err != nil {
		return nil, 0, err
	}
	se := db.acquireSnapshot()
	defer db.releaseSnapshot(se)
	ikey := makeInternalKey(nil, key, se.seq, keyTypeSeek)

	em, fm := db.getMems()
	for _, m := range [...]*memDB{em, fm} {
		if m == nil {
			continue
		}
		defer m.decref()
		if ok, mv, err := memGet(m.DB, ikey, db.s.icmp); ok {
			if err != nil {
				return nil, 0, err
			}
			return io.NopCloser(bytes.NewReader(bytes.Clone(mv))), int64(len(mv)), nil
		}
	}

	v := db.s.version()
	rc, size, err := v.getStream(ikey, ro)
	if err != nil {
		v.release()
		return nil, 0, err
	}
	return &versionCloser{ReadCloser: rc, v: v}, size, nil
}

// versionCloser holds a version, and with it the tables a stream reads, until closed.
type versionCloser struct {
	io.ReadCloser
	v    *version
	once sync.Once
}

func (c *versionCloser) Close() error {
	err := c.ReadCloser.Close()
	c.once.Do(c.v.release)
	return err
}

// getStream is version.get with the value as a stream.
func (v *version) getStream(ikey internalKey, ro *opt.ReadOptions) (value io.ReadCloser, size int64, err error) {
	if v.closing {
		return nil, 0, ErrClosed
	}
	ukey := ikey.ukey()
	var (
		// The newest level-0 entry found so far: level-0 tables overlap.
		zfound bool
		zseq   uint64
		zkt    keyType
		zval   io.ReadCloser
		zsize  int64
	)
	closeQuietly := func(rc io.ReadCloser) {
		if rc != nil {
			_ = rc.Close()
		}
	}
	err = ErrNotFound
	v.walkOverlapping(nil, ikey, func(level int, t *tFile) bool {
		fikey, fval, fsize, ferr := v.s.tops.findStream(t, ikey, ro)
		switch {
		case ferr == nil:
		case ferr == ErrNotFound:
			return true
		default:
			err = ferr
			return false
		}
		fukey, fseq, fkt, fkerr := parseInternalKey(fikey)
		if fkerr != nil {
			closeQuietly(fval)
			err = fkerr
			return false
		}
		if v.s.icmp.uCompare(ukey, fukey) != 0 {
			closeQuietly(fval)
			return true
		}
		if level <= 0 {
			if !zfound || fseq >= zseq {
				closeQuietly(zval)
				zfound, zseq, zkt, zval, zsize = true, fseq, fkt, fval, fsize
			} else {
				closeQuietly(fval)
			}
			return true
		}
		if fkt == keyTypeVal {
			value, size, err = fval, fsize, nil
		} else {
			closeQuietly(fval)
		}
		return false
	}, func(level int) bool {
		if !zfound {
			return true
		}
		if zkt == keyTypeVal {
			value, size, err = zval, zsize, nil
		} else {
			closeQuietly(zval)
		}
		zval = nil
		return false
	})
	if err != nil {
		closeQuietly(value)
		closeQuietly(zval)
		return nil, 0, err
	}
	return value, size, nil
}

// PutStream sets key to a value of size bytes read from value, written into a table of its
// own as it is read, so it is never held whole. The transaction's earlier writes are flushed
// to a table first, keeping them older than this one.
func (tr *Transaction) PutStream(key []byte, size int64, value io.Reader) error {
	tr.lk.Lock()
	defer tr.lk.Unlock()
	if tr.closed {
		return errTransactionDone
	}
	if err := tr.flush(); err != nil {
		return err
	}
	w, err := tr.db.s.tops.create()
	if err != nil {
		return err
	}
	tr.ikScratch = makeInternalKey(tr.ikScratch, key, tr.seq+1, keyTypeVal)
	tr.stats.startTimer()
	err = w.appendStream(tr.ikScratch, size, value)
	var t *tFile
	if err == nil {
		t, err = w.finish()
	}
	tr.stats.stopTimer()
	if err != nil {
		_ = w.drop()
		return err
	}
	tr.tables = append(tr.tables, t)
	tr.rec.addTableFile(0, t)
	tr.stats.write += t.size
	tr.seq++
	tr.db.logf("transaction@putstream created L0@%d S·%s", t.fd.Num, shortenb(t.size))
	return nil
}
