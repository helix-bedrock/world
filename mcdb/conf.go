// Ported from Dragonfly (MIT):
// https://github.com/df-mc/dragonfly/blob/1793369709ceed934675d69e86ff6c828e4216c8/server/world/mcdb/conf.go
//
// Changed for Helix: no default logger or block registry, an explicit chunk
// Upgrader, and errors instead of ignored ones.

package mcdb

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/helix-bedrock/world/chunk"
	"github.com/helix-bedrock/world/leveldb"
	"github.com/helix-bedrock/world/leveldb/opt"
	"github.com/helix-bedrock/world/mcdb/leveldat"
)

// Config holds the parameters of a DB.
type Config struct {
	// Log receives errors and debug messages. If nil, nothing is logged.
	Log *slog.Logger
	// LDBOptions holds LevelDB options such as block size or compression.
	// If nil, Bedrock-compatible defaults are used.
	LDBOptions *opt.Options

	// Blocks maps block states to runtime IDs for chunk encoding. Only
	// columns need it: without it, level keys, players and level.dat work
	// and column calls return ErrNoBlocks, so an engine can open its world
	// before its block registry is built.
	Blocks chunk.BlockRegistry
	// Upgrader upgrades block states saved by older versions. If nil, Open
	// creates one.
	Upgrader *chunk.Upgrader
	// KV is the key-value store the world is kept in. If nil, the LevelDB
	// database in the world folder is used, as BDS does. level.dat is
	// always read from and written to the world folder.
	KV KV
	// FlushInterval is how often buffered writes are written to LevelDB as
	// one batch. Zero means DefaultFlushInterval; negative means only Flush
	// and Close write them.
	FlushInterval time.Duration
}

// Open opens the world in dir, creating it if needed. If a level.dat is
// present it is read; otherwise default values are used.
func (conf Config) Open(dir string) (*DB, error) {
	if conf.Log == nil {
		conf.Log = slog.New(slog.DiscardHandler)
	}
	conf.Log = conf.Log.With("provider", "mcdb")
	if conf.Upgrader == nil {
		up, err := chunk.NewUpgrader()
		if err != nil {
			return nil, fmt.Errorf("open db: %w", err)
		}
		conf.Upgrader = up
	}
	ldbOpts := opt.Options{}
	if conf.LDBOptions != nil {
		ldbOpts = *conf.LDBOptions
	}
	if ldbOpts.BlockSize == 0 {
		ldbOpts.BlockSize = 16 * opt.KiB
	}
	if ldbOpts.Compression == opt.DefaultCompression {
		// Bedrock only reads its own compression.
		ldbOpts.Compression = opt.FlateCompression
	}

	if err := os.MkdirAll(filepath.Join(dir, "db"), 0o755); err != nil { //nolint:gosec // world folders are meant to be readable by other tools
		return nil, fmt.Errorf("open db: create directory: %w", err)
	}

	db := &DB{conf: conf, dir: dir, ldat: &leveldat.Data{}, ldatFile: &leveldat.LevelDat{}}
	ldatPath := filepath.Join(dir, "level.dat")
	if _, err := os.Stat(ldatPath); errors.Is(err, os.ErrNotExist) {
		db.ldat.FillDefault()
	} else if err != nil {
		return nil, fmt.Errorf("open db: stat level.dat: %w", err)
	} else {
		ldat, err := leveldat.ReadFile(ldatPath)
		if err != nil {
			return nil, fmt.Errorf("open db: %w", err)
		}
		// Bedrock has written storage version 10 for years; older worlds
		// (below 10) are read anyway, newer ones may have changed format.
		if ver := ldat.Ver(); ver > leveldat.Version {
			return nil, fmt.Errorf("open db: level.dat version %v is unsupported", ver)
		}
		if err = ldat.Unmarshal(db.ldat); err != nil {
			return nil, fmt.Errorf("open db: %w", err)
		}
		db.ldatFile = ldat
	}
	if conf.KV == nil {
		ldb, err := leveldb.OpenFile(filepath.Join(dir, "db"), &ldbOpts)
		if err != nil {
			return nil, fmt.Errorf("open db: leveldb: %w", err)
		}
		db.conf.KV = levelDB{db: ldb}
	}
	db.kv = db.conf.KV
	db.buf = newWriteBuffer(db.kv)
	db.meta = newMetaDictionary()
	if b, err := db.kv.Get([]byte(keyChunkMetaData)); err == nil {
		meta, err := decodeMetaDictionary(b)
		if err != nil {
			// Keep what decoded; chunks pointing at the rest keep their hash.
			conf.Log.Error("open db: " + err.Error())
		}
		db.meta = meta
	} else if !errors.Is(err, leveldb.ErrNotFound) {
		_ = db.kv.Close()
		return nil, fmt.Errorf("open db: read chunk metadata: %w", err)
	}
	if conf.FlushInterval == 0 {
		db.conf.FlushInterval = DefaultFlushInterval()
	}
	db.stop, db.stopped = make(chan struct{}), make(chan struct{})
	go db.flushLoop()
	return db, nil
}
