package hxdb

import (
	"github.com/helix-bedrock/world/hx"
)

// StoreItems stores item contents records (hx.ItemsFile) at once, in their own commit,
// leaving out those already stored; sums[i] must be hx.SumItems(list[i]). A reference to
// one may be saved once it returns. Runs: any goroutine.
func (db *DB) StoreItems(list [][]byte, sums [][32]byte) error {
	s := make([]hx.ItemsSum, len(sums))
	for i, sum := range sums {
		s[i] = hx.ItemsSum(sum)
	}
	return db.w.CommitItems(list, s)
}

// LoadItems reads the item contents record of sum into dst[:0]. Runs: any goroutine.
func (db *DB) LoadItems(sum [32]byte, dst []byte) ([]byte, error) {
	return db.w.ItemsRecord(hx.ItemsSum(sum), dst)
}
