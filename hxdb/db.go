// Package hxdb is the world provider an engine runs on: a Helix world (package
// hx) behind the same interfaces as mcdb's BDS provider (mcdb.WorldStore,
// mcdb.PlayerStore, mcdb.BlockEntityLoader), so an engine switches format
// without changing how it loads and saves. Chunk keys, entities, block
// entities and level keys keep BDS's bytes, as hx stores them, so a world
// converts back to BDS without loss (package convert).
//
// Writes collect in a save round that is committed every FlushInterval, as
// one atomic step across every file: chunks, players, level keys and
// level.dat land on disk together or not at all.
package hxdb

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/helix-bedrock/world/chunk"
	"github.com/helix-bedrock/world/hx"
	"github.com/helix-bedrock/world/hx/store"
	"github.com/helix-bedrock/world/mcdb"
	"github.com/helix-bedrock/world/mcdb/leveldat"
)

// Config configures a DB.
type Config struct {
	// Log receives errors of background saves. If nil, nothing is logged.
	Log *slog.Logger
	// Blocks is the server's block registry. Required for chunks.
	Blocks chunk.BlockRegistry
	// Upgrader upgrades block states saved by older versions. If nil, Open
	// creates one.
	Upgrader *chunk.Upgrader
	// FlushInterval is how often a save round is committed. Zero means
	// mcdb.DefaultFlushInterval; negative means only Flush and Close commit.
	FlushInterval time.Duration
	// RoundSize is how many bytes a round collects before it is committed
	// early. Zero means 64 MB.
	RoundSize int
	// SkipBlockEntities makes LoadColumn leave block entities out, for the
	// caller to read one at a time (EachBlockEntity, LoadBlockEntity).
	SkipBlockEntities bool
	// Store configures the record store.
	Store store.Options
}

// DB is a Helix world opened for an engine. LoadColumn, StoreColumn, the
// level, player and block entity methods and Flush may be called from any
// goroutine; one column is never stored from two goroutines at once.
// LevelDat and Close must not run concurrently with anything else.
type DB struct {
	conf Config
	w    *hx.World
	dir  string
	meta *mcdb.ChunkMetaData

	ldat     *leveldat.Data
	ldatFile *leveldat.LevelDat

	// mu guards the round being filled and what it holds.
	mu    sync.Mutex
	round *hx.Round
	held  pending
	// committing is what the round being committed holds, until its commit
	// returns.
	committing pending
	// spare is the round not being filled, reused.
	spare *hx.Round
	// flushMu serialises flushes.
	flushMu sync.Mutex

	scratch sync.Pool

	kick          chan struct{}
	stop, stopped chan struct{}
}

// pending is what a round holds that reads must see: chunks and players are
// committed before they are read again, level keys are read from here.
type pending struct {
	chunks  map[chunkID]struct{}
	players map[mcdb.PlayerIDs]struct{}
	// level holds level key writes; a nil value is a delete.
	level map[string][]byte
}

type chunkID struct {
	dim mcdb.Dimension
	pos mcdb.ChunkPos
}

func newPending() pending {
	return pending{chunks: map[chunkID]struct{}{}, players: map[mcdb.PlayerIDs]struct{}{}, level: map[string][]byte{}}
}

func (p *pending) clear() {
	clear(p.chunks)
	clear(p.players)
	clear(p.level)
}

// Open opens the Helix world in dir, creating it if the folder holds none.
func (conf Config) Open(dir string) (*DB, error) {
	if conf.Log == nil {
		conf.Log = slog.New(slog.DiscardHandler)
	}
	if conf.Upgrader == nil {
		up, err := chunk.NewUpgrader()
		if err != nil {
			return nil, fmt.Errorf("hxdb: open: %w", err)
		}
		conf.Upgrader = up
	}
	if conf.FlushInterval == 0 {
		conf.FlushInterval = mcdb.DefaultFlushInterval()
	}
	if conf.RoundSize <= 0 {
		conf.RoundSize = 64 << 20
	}
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // world folders are meant to be readable
		return nil, fmt.Errorf("hxdb: open: %w", err)
	}
	w, err := hx.Open(dir, hx.Config{Blocks: conf.Blocks, Upgrader: conf.Upgrader, Store: conf.Store})
	if err != nil {
		return nil, fmt.Errorf("hxdb: open: %w", err)
	}
	db := &DB{conf: conf, w: w, dir: dir, held: newPending(), committing: newPending(),
		kick: make(chan struct{}, 1), stop: make(chan struct{}), stopped: make(chan struct{})}
	db.round, db.spare = w.NewRound(), w.NewRound()
	db.scratch.New = func() any { return new(scratch) }
	if err := db.openLevelDat(); err != nil {
		_ = w.Close()
		return nil, err
	}
	if err := db.openMeta(); err != nil {
		_ = w.Close()
		return nil, err
	}
	go db.flushLoop()
	return db, nil
}

// openLevelDat reads level.dat, or fills in a new world's.
func (db *DB) openLevelDat() error {
	db.ldat, db.ldatFile = &leveldat.Data{}, &leveldat.LevelDat{}
	ld, err := db.w.LevelDat()
	if errors.Is(err, hx.ErrNotFound) {
		db.ldat.FillDefault()
		return nil
	}
	if err != nil {
		return fmt.Errorf("hxdb: open: %w", err)
	}
	if ver := ld.Ver(); ver > leveldat.Version {
		return fmt.Errorf("hxdb: open: level.dat version %v is unsupported", ver)
	}
	if err := ld.Unmarshal(db.ldat); err != nil {
		return fmt.Errorf("hxdb: open: %w", err)
	}
	db.ldatFile = ld
	return nil
}

// openMeta reads the chunk metadata dictionary.
func (db *DB) openMeta() error {
	b, err := db.w.Level(mcdb.LevelKeyChunkMetaData, nil)
	if errors.Is(err, hx.ErrNotFound) {
		db.meta = mcdb.NewChunkMetaData()
		return nil
	}
	if err != nil {
		return fmt.Errorf("hxdb: open: read chunk metadata: %w", err)
	}
	if db.meta, err = mcdb.DecodeChunkMetaData(b); err != nil {
		// Keep what decoded; chunks pointing at the rest keep their hash.
		db.conf.Log.Error("hxdb: open: " + err.Error())
	}
	return nil
}

// World returns the Helix world under the DB, for snapshots, backups and
// exports. Writes must go through the DB.
func (db *DB) World() *hx.World { return db.w }

// Dir returns the world's folder.
func (db *DB) Dir() string { return db.dir }

// LevelDat returns the world's level.dat values. Changes are saved by
// SaveLevelDat and Close.
func (db *DB) LevelDat() *leveldat.Data { return db.ldat }

// SaveLevelDat saves level.dat as it is now, in a round of its own with
// everything stored so far. Do not change LevelDat while it runs.
func (db *DB) SaveLevelDat() error {
	if err := db.putLevelDat(); err != nil {
		return err
	}
	return db.Flush()
}

func (db *DB) putLevelDat() error {
	if err := db.ldatFile.Marshal(*db.ldat); err != nil {
		return fmt.Errorf("hxdb: save level.dat: %w", err)
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	if err := db.round.PutLevelDat(db.ldatFile); err != nil {
		return fmt.Errorf("hxdb: save level.dat: %w", err)
	}
	return nil
}

// flushLoop commits a round every FlushInterval, or sooner when one grows
// past RoundSize, until Close.
func (db *DB) flushLoop() {
	defer close(db.stopped)
	var tick <-chan time.Time
	if db.conf.FlushInterval > 0 {
		t := time.NewTicker(db.conf.FlushInterval)
		defer t.Stop()
		tick = t.C
	}
	for {
		select {
		case <-db.stop:
			return
		case <-tick:
		case <-db.kick:
		}
		if err := db.Flush(); err != nil {
			db.conf.Log.Error(err.Error())
		}
	}
}

// grown asks for an early commit when the round is big. mu must be held.
func (db *DB) grown() {
	if db.round.Size() < db.conf.RoundSize {
		return
	}
	select {
	case db.kick <- struct{}{}:
	default:
	}
}

// Flush commits everything stored so far. Stores may go on meanwhile: they
// go into the next round.
func (db *DB) Flush() error {
	db.flushMu.Lock()
	defer db.flushMu.Unlock()
	meta, metaChanged, err := db.meta.Encode()
	if err != nil {
		db.conf.Log.Error("hxdb: flush: " + err.Error())
	}
	db.mu.Lock()
	if metaChanged {
		db.round.PutLevel(mcdb.LevelKeyChunkMetaData, meta)
	}
	if db.round.Len() == 0 {
		db.mu.Unlock()
		return nil
	}
	r := db.round
	db.round, db.spare = db.spare, nil
	db.held, db.committing = db.committing, db.held
	db.mu.Unlock()

	_, err = db.w.Commit(r)

	db.mu.Lock()
	db.committing.clear()
	db.spare = r
	db.mu.Unlock()
	if err != nil {
		if metaChanged {
			db.meta.MarkDirty()
		}
		return fmt.Errorf("hxdb: flush: %w", err)
	}
	return nil
}

// settle commits the rounds holding the chunk, if any, so a read sees it.
func (db *DB) settleChunk(id chunkID) error {
	db.mu.Lock()
	_, a := db.held.chunks[id]
	_, b := db.committing.chunks[id]
	db.mu.Unlock()
	if a || b {
		return db.Flush()
	}
	return nil
}

// Close commits everything, saves level.dat and closes the world.
func (db *DB) Close() error {
	close(db.stop)
	<-db.stopped
	db.ldat.LastPlayed = time.Now().Unix()
	var errs []error
	if err := db.putLevelDat(); err != nil {
		errs = append(errs, err)
	}
	if err := db.Flush(); err != nil {
		errs = append(errs, err)
	}
	if err := os.WriteFile(filepath.Join(db.dir, "levelname.txt"), []byte(db.ldat.LevelName), 0o644); err != nil { //nolint:gosec // world files are meant to be readable
		errs = append(errs, fmt.Errorf("hxdb: close: write levelname.txt: %w", err))
	}
	if err := db.w.Close(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// notFound turns hx's not-found into mcdb's, which engines test for.
func notFound(err error) error {
	if errors.Is(err, hx.ErrNotFound) {
		return mcdb.ErrNotFound
	}
	return err
}

// LoadLevel returns a level key's value, or mcdb.ErrNotFound.
func (db *DB) LoadLevel(key string) ([]byte, error) {
	db.mu.Lock()
	v, ok := db.held.level[key]
	if !ok {
		v, ok = db.committing.level[key]
	}
	db.mu.Unlock()
	if ok {
		if v == nil {
			return nil, mcdb.ErrNotFound
		}
		return append([]byte(nil), v...), nil
	}
	b, err := db.w.Level(key, nil)
	if err != nil {
		return nil, notFound(err)
	}
	return b, nil
}

// StoreLevel sets a level key.
func (db *DB) StoreLevel(key string, value []byte) error {
	if err := checkLevelKey(key); err != nil {
		return err
	}
	v := append(make([]byte, 0, len(value)), value...)
	db.mu.Lock()
	defer db.mu.Unlock()
	db.round.PutLevel(key, v)
	db.held.level[key] = v
	db.grown()
	return nil
}

// DeleteLevel removes a level key.
func (db *DB) DeleteLevel(key string) error {
	if err := checkLevelKey(key); err != nil {
		return err
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	db.round.DeleteLevel(key)
	db.held.level[key] = nil
	return nil
}

// checkLevelKey refuses keys that are not level keys: empty ones and the
// chunk metadata dictionary, which the DB keeps itself.
func checkLevelKey(key string) error {
	if key == "" {
		return errors.New("hxdb: empty level key")
	}
	if key == mcdb.LevelKeyChunkMetaData {
		return fmt.Errorf("hxdb: level key %q is kept by the world store", key)
	}
	return nil
}

// ForEachLevel calls fn for every level key starting with prefix, in key
// order, until fn returns false.
func (db *DB) ForEachLevel(prefix string, fn func(key string, value []byte) bool) error {
	db.mu.Lock()
	dirty := len(db.held.level) > 0 || len(db.committing.level) > 0
	db.mu.Unlock()
	if dirty {
		if err := db.Flush(); err != nil {
			return err
		}
	}
	var keys []string
	if err := db.w.EachLevel(func(key []byte) bool {
		if strings.HasPrefix(string(key), prefix) {
			keys = append(keys, string(key))
		}
		return true
	}); err != nil {
		return fmt.Errorf("hxdb: level keys: %w", err)
	}
	for _, k := range keys {
		v, err := db.w.Level(k, nil)
		if errors.Is(err, hx.ErrNotFound) {
			continue
		}
		if err != nil {
			return fmt.Errorf("hxdb: level key %q: %w", k, err)
		}
		if !fn(k, v) {
			return nil
		}
	}
	return nil
}

// LoadPlayer returns the data of the player with these IDs, and false if the
// player was never saved.
func (db *DB) LoadPlayer(ids mcdb.PlayerIDs) (map[string]any, bool, error) {
	db.mu.Lock()
	_, a := db.held.players[ids]
	_, b := db.committing.players[ids]
	db.mu.Unlock()
	if a || b {
		if err := db.Flush(); err != nil {
			return nil, false, err
		}
	}
	return db.w.LoadPlayer(hx.PlayerIDs(ids))
}

// StorePlayer saves the player's data. A player saved again before the round
// holding it is committed commits it first: a round finds a player's records
// on disk.
func (db *DB) StorePlayer(ids mcdb.PlayerIDs, data map[string]any) error {
	db.mu.Lock()
	_, a := db.held.players[ids]
	_, b := db.committing.players[ids]
	db.mu.Unlock()
	if a || b {
		if err := db.Flush(); err != nil {
			return err
		}
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	if err := db.round.PutPlayer(hx.PlayerIDs(ids), data); err != nil {
		return err
	}
	db.held.players[ids] = struct{}{}
	db.grown()
	return nil
}

var (
	_ mcdb.WorldStore        = (*DB)(nil)
	_ mcdb.PlayerStore       = (*DB)(nil)
	_ mcdb.BlockEntityLoader = (*DB)(nil)
)
