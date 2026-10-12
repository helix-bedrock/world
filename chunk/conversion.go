// Ported from Dragonfly (MIT):
// https://github.com/df-mc/dragonfly/blob/1793369709ceed934675d69e86ff6c828e4216c8/server/world/chunk/conversion.go
//
// Changed for Helix: the legacy table and the block upgrader live in an
// Upgrader value instead of package state loaded in init.

package chunk

import (
	"bytes"
	_ "embed"
	"fmt"

	"github.com/helix-bedrock/protocol/nbt"
	"github.com/helix-bedrock/world/blockupgrader"
)

// legacyBlockEntry is a block entry as saved before Bedrock 1.13: a name and
// a metadata value.
type legacyBlockEntry struct {
	Name string `nbt:"name"`
	Meta int16  `nbt:"meta"`
}

// legacyMappingsData maps pre-1.13 entries to 1.13 block states. It is never
// written to.
//
//go:embed legacy_states.nbt
var legacyMappingsData []byte

// Upgrader turns block states saved by older versions into current ones: the
// pre-1.13 name and meta format through an embedded table, and everything
// newer through blockupgrader.
//
// Create one with NewUpgrader and share it. It is safe for concurrent use.
type Upgrader struct {
	legacy map[legacyBlockEntry]blockEntry
	blocks *blockupgrader.Upgrader
}

// NewUpgrader loads the embedded legacy table and upgrade schemas.
func NewUpgrader() (*Upgrader, error) {
	blocks, err := blockupgrader.New()
	if err != nil {
		return nil, fmt.Errorf("chunk: %w", err)
	}
	u := &Upgrader{legacy: make(map[legacyBlockEntry]blockEntry), blocks: blocks}

	r := bytes.NewReader(legacyMappingsData)
	dec := nbt.NewDecoder(r)
	for r.Len() > 0 {
		// Fresh each time: the decoder may reuse a map it decodes into.
		var entry struct {
			Legacy  legacyBlockEntry `nbt:"legacy"`
			Updated blockEntry       `nbt:"updated"`
		}
		if err := dec.Decode(&entry); err != nil {
			return nil, fmt.Errorf("chunk: decode legacy block table entry %d: %w", len(u.legacy), err)
		}
		u.legacy[entry.Legacy] = entry.Updated
	}
	return u, nil
}

// upgradeLegacyEntry returns the 1.13 block state for a pre-1.13 entry.
func (u *Upgrader) upgradeLegacyEntry(name string, meta int16) (blockEntry, bool) {
	entry, ok := u.legacy[legacyBlockEntry{Name: name, Meta: meta}]
	if !ok {
		// Some entries apply to every meta value.
		entry, ok = u.legacy[legacyBlockEntry{Name: name}]
	}
	return entry, ok
}
