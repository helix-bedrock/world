package lsm

import (
	"fmt"
	"os"
	"path/filepath"
)

// Write applies a batch. Small values go through the log and the memtable; big ones are
// written into tables of their own first, so no value of hundreds of megabytes ever sits
// in memory or in the log. Readers never wait for a write.
//
// A batch with big values is applied in two steps: the big values' tables, then the rest.
// A crash between them keeps the big values and loses the rest of that batch.
func (d *DB) Write(b *Batch) error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	if err := d.writable(); err != nil {
		return err
	}
	b.splitBig(d.opts.BigValue)
	if len(b.streams) > 0 {
		if err := d.writeStreams(b.streams); err != nil {
			return err
		}
	}
	if b.count == 0 {
		return nil
	}
	return d.writeSmall(b)
}

// Put sets one key.
func (d *DB) Put(key, value []byte) error {
	var b Batch
	b.Put(key, value)
	return d.Write(&b)
}

// Delete removes one key.
func (d *DB) Delete(key []byte) error {
	var b Batch
	b.Delete(key)
	return d.Write(&b)
}

func (d *DB) writable() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return ErrClosed
	}
	if d.opts.ReadOnly {
		return ErrReadOnly
	}
	return d.bgErr
}

// writeSmall appends the batch to the log and the memtable and makes it visible.
func (d *DB) writeSmall(b *Batch) error {
	if err := d.makeRoom(false); err != nil {
		return err
	}
	d.mu.Lock()
	seq := d.seq + 1
	mem := d.mem
	d.mu.Unlock()
	if err := d.log.add(b.logRecord(nil, seq)); err != nil {
		return d.fail(fmt.Errorf("lsm: write log: %w", err))
	}
	if d.opts.Sync {
		if err := d.logFile.Sync(); err != nil {
			return d.fail(fmt.Errorf("lsm: sync log: %w", err))
		}
	}
	i := uint64(0)
	err := eachEntry(b.small, func(k kind, key, value []byte) error {
		mem.add(seq+i, k, key, value)
		i++
		return nil
	})
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.seq = seq + i - 1
	d.mu.Unlock()
	return nil
}

// fail records a write error: the database refuses writes from then on, since the log
// may hold a partial record.
func (d *DB) fail(err error) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.bgErr == nil {
		d.bgErr = err
	}
	d.cond.Broadcast()
	return err
}

// makeRoom waits until the memtable may take more writes: when it is full (or force is
// set and it holds anything) it becomes the immutable memtable, written to a table in
// the background, and a new log starts. Writes also wait while L0 holds too many tables.
func (d *DB) makeRoom(force bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	for {
		switch {
		case d.closed:
			return ErrClosed
		case d.bgErr != nil:
			return d.bgErr
		case len(d.current.levels[0]) >= l0Stop:
			d.cond.Wait()
		case !force && d.mem.size() < d.opts.WriteBuffer:
			return nil
		case force && d.mem.empty():
			return nil
		case d.imm != nil:
			d.cond.Wait()
		default:
			if err := d.rotate(); err != nil {
				return err
			}
			if !force {
				return nil
			}
		}
	}
}

// rotate makes the memtable immutable and starts a new one and a new log. d.mu is held.
func (d *DB) rotate() error {
	oldLog := d.logNum
	if err := d.newLog(); err != nil {
		return err
	}
	d.imm, d.immLog = d.mem, oldLog
	d.mem = newMemtable(d.opts.WriteBuffer)
	d.cond.Broadcast()
	return nil
}

// writeStreams writes each big value into a table of its own and adds the tables to the
// database in one manifest record. Everything already in the memtables is written out
// first, so the new tables are newer than every table before them, as LevelDB's L0 order
// requires.
func (d *DB) writeStreams(streams []streamPut) error {
	if err := d.makeRoom(true); err != nil {
		return err
	}
	if err := d.waitImm(); err != nil {
		return err
	}
	d.mu.Lock()
	seq := d.seq
	d.mu.Unlock()
	edit := versionEdit{}
	var nums []uint64
	defer func() { d.unpend(nums) }()
	for _, s := range streams {
		seq++
		num := d.newFileNum()
		nums = append(nums, num)
		meta, err := d.writeStreamTable(num, s, seq)
		if err != nil {
			return err
		}
		// Always L0: deeper levels change only on the background goroutine, and a
		// compaction running now may be writing into them.
		edit.addFile(0, meta)
	}
	edit.setLastSeq(seq)
	if err := d.logAndApply(&edit); err != nil {
		return d.fail(err)
	}
	d.mu.Lock()
	d.seq = seq
	d.mu.Unlock()
	return nil
}

// waitImm waits until the immutable memtable is in a table.
func (d *DB) waitImm() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	for d.imm != nil && d.bgErr == nil && !d.closed {
		d.cond.Wait()
	}
	if d.closed {
		return ErrClosed
	}
	return d.bgErr
}

// newFileNum allocates a file number and marks it as being written.
func (d *DB) newFileNum() uint64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	num := d.next
	d.next++
	d.pending[num] = true
	return num
}

func (d *DB) unpend(nums []uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, num := range nums {
		delete(d.pending, num)
	}
}

// writeStreamTable writes a table holding one value read from a stream.
func (d *DB) writeStreamTable(num uint64, s streamPut, seq uint64) (fileMeta, error) {
	rc, size, err := s.open()
	if err != nil {
		return fileMeta{}, fmt.Errorf("lsm: open stream for %q: %w", s.key, err)
	}
	defer rc.Close()
	path := filepath.Join(d.dir, tableName(num))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fileMeta{}, fmt.Errorf("lsm: create table: %w", err)
	}
	ikey := appendInternalKey(nil, s.key, seq, kindValue)
	tw := newTableWriter(f, d.opts.tableOpts())
	err = tw.addStream(ikey, rc, size)
	var n uint64
	if err == nil {
		n, err = tw.finish()
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil && cerr != nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(path)
		return fileMeta{}, err
	}
	return fileMeta{num: num, size: n, smallest: ikey, largest: ikey}, nil
}
