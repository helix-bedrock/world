package mcdb

import (
	"sync"

	"github.com/helix-bedrock/world/leveldb/util"
)

const (
	// DefaultLoadBudget is Config.LoadBudget when it is 0: 512 MB.
	DefaultLoadBudget = 512 << 20
	// loadGateFrom is the stored size from which a chunk load counts against the
	// budget. Ordinary chunks are a few kilobytes and never wait.
	loadGateFrom = 1 << 20
	// loadCostFactor is about how many times its stored size a chunk takes while it
	// loads: LevelDB's raw and inflated block, the copy read out and the decoded data.
	loadCostFactor = 4
)

// kvSizer is a KV that can say about how many bytes the keys under a prefix take.
type kvSizer interface {
	SizeOf(prefix []byte) (int64, error)
}

// SizeOf is LevelDB's estimate of the bytes stored under prefix.
func (l levelDB) SizeOf(prefix []byte) (int64, error) {
	sizes, err := l.db.SizeOf([]util.Range{*util.BytesPrefix(prefix)})
	if err != nil {
		return 0, err
	}
	return sizes.Sum(), nil
}

// loadGate keeps the memory of chunks loading at once under a budget, so loads of
// huge chunks (chests stuffed with books: hundreds of megabytes each) on every load
// worker at once cannot take all the machine's memory. A load bigger than the whole
// budget runs alone. Safe for concurrent use.
type loadGate struct {
	mu     sync.Mutex
	cond   *sync.Cond
	budget int64
	used   int64
}

func newLoadGate(budget int64) *loadGate {
	g := &loadGate{budget: budget}
	g.cond = sync.NewCond(&g.mu)
	return g
}

// acquire waits until n more bytes fit the budget, or nothing else is loading, and
// returns what to release.
func (g *loadGate) acquire(n int64) int64 {
	n = min(n, g.budget)
	g.mu.Lock()
	for g.used > 0 && g.used+n > g.budget {
		g.cond.Wait()
	}
	g.used += n
	g.mu.Unlock()
	return n
}

// release gives back what acquire took.
func (g *loadGate) release(n int64) {
	g.mu.Lock()
	g.used -= n
	g.mu.Unlock()
	g.cond.Broadcast()
}

// enterLoad waits, for a big chunk, until it may load, and returns what to give back
// to leaveLoad. Without a gate or a store that knows sizes, nothing waits.
func (db *DB) enterLoad(k dbKey) int64 {
	s, ok := db.kv.(kvSizer)
	if db.gate == nil || !ok {
		return 0
	}
	size, err := s.SizeOf(k.Sum())
	if err != nil || size < loadGateFrom {
		return 0
	}
	return db.gate.acquire(size * loadCostFactor)
}

// leaveLoad gives back what enterLoad took.
func (db *DB) leaveLoad(n int64) {
	if n > 0 {
		db.gate.release(n)
	}
}
