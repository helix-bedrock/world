package lsm

import (
	"container/list"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// blockKey names a block: its table and offset.
type blockKey struct{ table, offset uint64 }

// blockCache keeps decompressed small blocks under a byte budget, least recently used
// out first. Big blocks never enter it.
type blockCache struct {
	mu     sync.Mutex
	budget int
	used   int
	lru    list.List // of *cachedBlock, most recent at the front
	blocks map[blockKey]*list.Element
}

type cachedBlock struct {
	key  blockKey
	data []byte
}

func newBlockCache(budget int) *blockCache {
	return &blockCache{budget: budget, blocks: make(map[blockKey]*list.Element)}
}

func (c *blockCache) get(table, offset uint64) ([]byte, bool) {
	if c == nil || c.budget <= 0 {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.blocks[blockKey{table, offset}]
	if !ok {
		return nil, false
	}
	c.lru.MoveToFront(e)
	cb, _ := e.Value.(*cachedBlock)
	return cb.data, true
}

func (c *blockCache) put(table, offset uint64, data []byte) {
	if c == nil || len(data) > c.budget/8 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	k := blockKey{table, offset}
	if _, ok := c.blocks[k]; ok {
		return
	}
	c.blocks[k] = c.lru.PushFront(&cachedBlock{key: k, data: data})
	c.used += len(data)
	for c.used > c.budget {
		c.evictOldest()
	}
}

func (c *blockCache) evictOldest() {
	e := c.lru.Back()
	if e == nil {
		c.used = 0
		return
	}
	cb, _ := e.Value.(*cachedBlock)
	c.lru.Remove(e)
	delete(c.blocks, cb.key)
	c.used -= len(cb.data)
}

// dropTable removes a deleted table's blocks.
func (c *blockCache) dropTable(table uint64) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for e := c.lru.Front(); e != nil; {
		next := e.Next()
		if cb, _ := e.Value.(*cachedBlock); cb.key.table == table {
			c.lru.Remove(e)
			delete(c.blocks, cb.key)
			c.used -= len(cb.data)
		}
		e = next
	}
}

// tableCache keeps up to max tables open, least recently used closed first. A table in
// use is closed only when its last user releases it.
type tableCache struct {
	dir      string
	max      int
	blocks   *blockCache
	bigBlock uint64

	mu     sync.Mutex
	lru    list.List // of *openTableEntry
	tables map[uint64]*list.Element
}

type openTableEntry struct {
	t       *table
	file    *os.File
	refs    int
	evicted bool
}

func newTableCache(dir string, maxOpen int, blocks *blockCache, bigBlock uint64) *tableCache {
	return &tableCache{dir: dir, max: maxOpen, blocks: blocks, bigBlock: bigBlock, tables: make(map[uint64]*list.Element)}
}

// acquire returns the open table num, size bytes, opening it if needed. Call release
// with the returned handle when done.
func (c *tableCache) acquire(num, size uint64) (*openTableEntry, error) {
	c.mu.Lock()
	if e, ok := c.tables[num]; ok {
		c.lru.MoveToFront(e)
		ent, _ := e.Value.(*openTableEntry)
		ent.refs++
		c.mu.Unlock()
		return ent, nil
	}
	c.mu.Unlock()
	ent, err := c.open(num, size)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.tables[num]; ok { // opened meanwhile by another reader
		_ = ent.file.Close()
		c.lru.MoveToFront(e)
		ent, _ = e.Value.(*openTableEntry)
		ent.refs++
		return ent, nil
	}
	ent.refs = 1
	c.tables[num] = c.lru.PushFront(ent)
	c.trim()
	return ent, nil
}

func (c *tableCache) open(num, size uint64) (*openTableEntry, error) {
	f, err := os.Open(filepath.Join(c.dir, tableName(num)))
	if os.IsNotExist(err) {
		f, err = os.Open(filepath.Join(c.dir, oldTableName(num)))
	}
	if err != nil {
		return nil, fmt.Errorf("lsm: open table %d: %w", num, err)
	}
	t, err := openTable(num, f, size, c.blocks, c.bigBlock)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return &openTableEntry{t: t, file: f}, nil
}

// trim closes unused tables past the limit.
func (c *tableCache) trim() {
	for e := c.lru.Back(); e != nil && len(c.tables) > c.max; {
		prev := e.Prev()
		ent, _ := e.Value.(*openTableEntry)
		c.lru.Remove(e)
		delete(c.tables, ent.t.num)
		ent.evicted = true
		if ent.refs == 0 {
			_ = ent.file.Close()
		}
		e = prev
	}
}

// release gives back a table from acquire.
func (c *tableCache) release(ent *openTableEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ent.refs--
	if ent.refs == 0 && ent.evicted {
		_ = ent.file.Close()
	}
}

// evict closes a table that was deleted, once nobody uses it.
func (c *tableCache) evict(num uint64) {
	c.mu.Lock()
	if e, ok := c.tables[num]; ok {
		ent, _ := e.Value.(*openTableEntry)
		c.lru.Remove(e)
		delete(c.tables, num)
		ent.evicted = true
		if ent.refs == 0 {
			_ = ent.file.Close()
		}
	}
	c.mu.Unlock()
	c.blocks.dropTable(num)
}

// close closes every table.
func (c *tableCache) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for num, e := range c.tables {
		ent, _ := e.Value.(*openTableEntry)
		_ = ent.file.Close()
		delete(c.tables, num)
	}
	c.lru.Init()
}
