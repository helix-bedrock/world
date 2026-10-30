package mcdb

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/helix-bedrock/world/chunk"
	"github.com/helix-bedrock/world/leveldb"
	"github.com/helix-bedrock/world/leveldb/util"
)

// Storage is pluggable at two levels. A backend that implements KV keeps
// every BDS key and value as they are and lets DB do the encoding: pass it
// as Config.KV. A backend that wants its own format implements WorldStore
// and/or PlayerStore directly; DB is the default implementation of both.
// Engines depend on the two interfaces, so a network can keep players in a
// shared database while each world stays in its own LevelDB.

// ErrNotFound is returned when a key, column or record does not exist.
// It is the same value as leveldb.ErrNotFound, so either can be used with
// errors.Is.
var ErrNotFound = leveldb.ErrNotFound

// WorldStore saves a world: chunk columns with everything in them, and the
// world-wide (level) keys. Implementations must be safe for concurrent use;
// a column is never stored from two goroutines at once.
type WorldStore interface {
	// LoadColumn returns the column at pos, or an error matching
	// ErrNotFound if there is none.
	LoadColumn(pos ChunkPos, dim Dimension) (*chunk.Column, error)
	// StoreColumn saves a column. It may buffer; a column and its entities
	// and block entities are always saved together.
	StoreColumn(pos ChunkPos, dim Dimension, col *chunk.Column) error
	// LoadLevel returns a level key's value, or ErrNotFound.
	LoadLevel(key string) ([]byte, error)
	// StoreLevel sets a level key.
	StoreLevel(key string, value []byte) error
	// DeleteLevel removes a level key.
	DeleteLevel(key string) error
	// ForEachLevel calls fn for every level key starting with prefix, in
	// key order, until fn returns false.
	ForEachLevel(prefix string, fn func(key string, value []byte) bool) error
	// Flush makes everything stored so far durable.
	Flush() error
	// Close flushes and releases the store.
	Close() error
}

// PlayerStore saves players the way BDS does: the player's data in one
// record, found through one lookup record per login ID.
type PlayerStore interface {
	// LoadPlayer returns the data of the player with these IDs, and false
	// if the player was never saved.
	LoadPlayer(ids PlayerIDs) (data map[string]any, ok bool, err error)
	// StorePlayer saves the player's data.
	StorePlayer(ids PlayerIDs, data map[string]any) error
}

// PlayerIDs are the IDs a player logs in with. BDS writes a lookup record
// for each one that is set.
type PlayerIDs struct {
	// MsaID is the Xbox (Microsoft account) UUID; empty for offline players.
	MsaID string
	// SelfSignedID is the UUID the client signs its own login with.
	SelfSignedID string
}

// KV is the key-value store under a DB. Keys and values are the ones BDS
// uses. Implementations must be safe for concurrent use.
type KV interface {
	// Get returns a copy of the value of key, or ErrNotFound.
	Get(key []byte) ([]byte, error)
	// Write applies all writes atomically.
	Write(writes []KVWrite) error
	// NewIterator iterates the keys starting with prefix in byte order.
	NewIterator(prefix []byte) KVIterator
	// Close releases the store.
	Close() error
}

// KVWrite is a Put of Value at Key, or a Delete of Key.
type KVWrite struct {
	Key, Value []byte
	Delete     bool
}

// KVIterator walks keys in order. Key and Value are only valid until the
// next call to Next.
type KVIterator interface {
	Next() bool
	Key() []byte
	Value() []byte
	Release()
	Error() error
}

// levelDB is the default KV.
type levelDB struct{ db *leveldb.DB }

func (l levelDB) Get(key []byte) ([]byte, error) { return l.db.Get(key, nil) }

func (l levelDB) Write(writes []KVWrite) error {
	batch := leveldb.MakeBatch(len(writes))
	for _, w := range writes {
		if w.Delete {
			batch.Delete(w.Key)
		} else {
			batch.Put(w.Key, w.Value)
		}
	}
	return l.db.Write(batch, nil)
}

func (l levelDB) NewIterator(prefix []byte) KVIterator {
	if len(prefix) == 0 {
		return l.db.NewIterator(nil, nil)
	}
	return l.db.NewIterator(util.BytesPrefix(prefix), nil)
}

func (l levelDB) Close() error { return l.db.Close() }

var (
	_ WorldStore  = (*DB)(nil)
	_ PlayerStore = (*DB)(nil)
)

// LoadLevel returns the value of a level key, such as LevelKeyScoreboard.
func (db *DB) LoadLevel(key string) ([]byte, error) {
	v, err := db.buf.get([]byte(key))
	if err != nil {
		return nil, fmt.Errorf("load level key %q: %w", key, err)
	}
	return v, nil
}

// StoreLevel sets a level key. Like every write it reaches the backend with
// the next flush.
func (db *DB) StoreLevel(key string, value []byte) error {
	if err := checkLevelKey(key); err != nil {
		return err
	}
	t := &txn{}
	t.Put([]byte(key), bytes.Clone(value))
	db.buf.apply(t)
	return nil
}

// DeleteLevel removes a level key.
func (db *DB) DeleteLevel(key string) error {
	if err := checkLevelKey(key); err != nil {
		return err
	}
	t := &txn{}
	t.Delete([]byte(key))
	db.buf.apply(t)
	return nil
}

// ForEachLevel calls fn for every key starting with prefix, as BDS scans
// prefix families (LevelPrefixVillage, LevelPrefixTickingArea, ...) when a
// world opens. Buffered writes are flushed first.
func (db *DB) ForEachLevel(prefix string, fn func(key string, value []byte) bool) error {
	if err := db.Flush(); err != nil {
		return err
	}
	it := db.kv.NewIterator([]byte(prefix))
	defer it.Release()
	for it.Next() {
		if !fn(string(it.Key()), it.Value()) {
			break
		}
	}
	if err := it.Error(); err != nil {
		return fmt.Errorf("iterate level keys %q: %w", prefix, err)
	}
	return nil
}

var errChunkKey = errors.New("key has the shape of a chunk key")

// checkLevelKey refuses keys the column iterator would take for chunk keys.
func checkLevelKey(key string) error {
	if n := len(key); n == 0 || ((n == 9 || n == 13) && (key[n-1] == keyVersion || key[n-1] == keyVersionOld)) {
		return fmt.Errorf("level key %q: %w", key, errChunkKey)
	}
	return nil
}

// namespacePrefix starts every plugin key. BDS ignores keys it does not
// know, so these never disturb a world opened in BDS.
const namespacePrefix = "helix_ns_"

// Namespace is a plugin's own key-value store inside a world's backend.
// Keys of different namespaces never collide. Safe for concurrent use.
type Namespace struct {
	db     *DB
	prefix string
}

// Namespace returns the store of the namespace name, usually a plugin name.
func (db *DB) Namespace(name string) *Namespace {
	return &Namespace{db: db, prefix: namespacePrefix + name + ":"}
}

// Get returns the value of key, or ErrNotFound.
func (n *Namespace) Get(key string) ([]byte, error) { return n.db.LoadLevel(n.prefix + key) }

// Set stores value under key.
func (n *Namespace) Set(key string, value []byte) error { return n.db.StoreLevel(n.prefix+key, value) }

// Delete removes key.
func (n *Namespace) Delete(key string) error { return n.db.DeleteLevel(n.prefix + key) }

// ForEach calls fn for every key in the namespace starting with prefix, with
// the namespace part removed, until fn returns false.
func (n *Namespace) ForEach(prefix string, fn func(key string, value []byte) bool) error {
	return n.db.ForEachLevel(n.prefix+prefix, func(k string, v []byte) bool {
		return fn(k[len(n.prefix):], v)
	})
}
