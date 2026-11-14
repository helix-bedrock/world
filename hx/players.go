package hx

import (
	"crypto/rand"
	"errors"
	"fmt"

	"github.com/helix-bedrock/protocol/nbt"
	"github.com/helix-bedrock/world/hx/store"
)

// Players are kept under BDS 1.26.60's keys, so conversion is a copy:
// player_<id> lookup records, one per login ID, all pointing at the
// player_server_<uuid> record that holds the player's data (LE NBT). The
// server UUID is random, not a login ID. Same layout as mcdb's.
const (
	playerLookupPrefix = "player_"
	playerServerPrefix = "player_server_"
)

// PlayerIDs are the IDs a player logs in with.
type PlayerIDs struct {
	// MsaID is the Xbox (Microsoft account) UUID; empty for offline players.
	MsaID string
	// SelfSignedID is the UUID the client signs its own login with.
	SelfSignedID string
}

// playerLookup is a player_<id> record, fields in the order BDS writes them.
type playerLookup struct {
	MsaID        string `nbt:"MsaId"`
	SelfSignedID string `nbt:"SelfSignedId"`
	ServerID     string `nbt:"ServerId"`
}

// LoadPlayer returns a player's saved data, found through the lookup record
// of either ID (the MSA ID first, as it does not change between devices), and
// false if the player was never saved.
func (v view) LoadPlayer(ids PlayerIDs) (map[string]any, bool, error) {
	look, ok, err := v.playerLookup(ids)
	if !ok || err != nil {
		return nil, ok, err
	}
	b, err := v.get(v.w.players, []byte(look.ServerID), nil)
	if errors.Is(err, store.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("hx: load player %v: %w", look.ServerID, err)
	}
	var data map[string]any
	if err := nbt.UnmarshalEncoding(b, &data, nbt.LittleEndian); err != nil {
		return nil, false, fmt.Errorf("hx: load player %v: %w", look.ServerID, err)
	}
	return data, true, nil
}

// PutPlayer saves a player's data in the round, with its lookup records on
// the first save or when its IDs changed.
func (r *Round) PutPlayer(ids PlayerIDs, data map[string]any) error {
	if ids.MsaID == "" && ids.SelfSignedID == "" {
		return errors.New("hx: store player: no ID")
	}
	look, ok := r.newPlayers[ids]
	if !ok {
		var err error
		if look, ok, err = r.w.playerLookup(ids); err != nil {
			return err
		}
	}
	if !ok || look.MsaID != ids.MsaID || look.SelfSignedID != ids.SelfSignedID {
		if err := r.putLookup(ids, &look, ok); err != nil {
			return err
		}
	}
	b, err := nbt.MarshalEncoding(data, nbt.LittleEndian)
	if err != nil {
		return fmt.Errorf("hx: store player %v: %w", look.ServerID, err)
	}
	r.b.Put(r.w.players, KindPlayer, []byte(look.ServerID), b)
	return nil
}

// putLookup writes the lookup records of ids, giving a new player a server
// ID. The round remembers it, so two saves of a new player in one round use
// one record.
func (r *Round) putLookup(ids PlayerIDs, look *playerLookup, known bool) error {
	if !known {
		id, err := randomUUID()
		if err != nil {
			return fmt.Errorf("hx: store player: %w", err)
		}
		look.ServerID = playerServerPrefix + id
	}
	look.MsaID, look.SelfSignedID = ids.MsaID, ids.SelfSignedID
	b, err := nbt.MarshalEncoding(*look, nbt.LittleEndian)
	if err != nil {
		return fmt.Errorf("hx: store player: %w", err)
	}
	for _, id := range [...]string{ids.MsaID, ids.SelfSignedID} {
		if id != "" {
			r.b.Put(r.w.players, KindPlayer, []byte(playerLookupPrefix+id), b)
		}
	}
	if r.newPlayers == nil {
		r.newPlayers = map[PlayerIDs]playerLookup{}
	}
	r.newPlayers[ids] = *look
	return nil
}

// playerLookup reads the lookup record of the first ID that has one.
func (v view) playerLookup(ids PlayerIDs) (playerLookup, bool, error) {
	for _, id := range [...]string{ids.MsaID, ids.SelfSignedID} {
		if id == "" {
			continue
		}
		b, err := v.get(v.w.players, []byte(playerLookupPrefix+id), nil)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return playerLookup{}, false, fmt.Errorf("hx: player lookup %v: %w", id, err)
		}
		var look playerLookup
		if err := nbt.UnmarshalEncoding(b, &look, nbt.LittleEndian); err != nil {
			return playerLookup{}, false, fmt.Errorf("hx: player lookup %v: %w", id, err)
		}
		if len(look.ServerID) <= len(playerServerPrefix) {
			return playerLookup{}, false, fmt.Errorf("hx: player lookup %v: bad server ID %q", id, look.ServerID)
		}
		return look, true, nil
	}
	return playerLookup{}, false, nil
}

// PlayerRecord reads a players.hx record by its BDS key into dst[:0], or
// returns ErrNotFound. For the converter and tools.
func (v view) PlayerRecord(key string, dst []byte) ([]byte, error) {
	return v.get(v.w.players, []byte(key), dst)
}

// EachPlayerRecord calls fn for every players.hx key, in key order, until fn
// returns false. The key is valid only during the call.
func (v view) EachPlayerRecord(fn func(key []byte) bool) error {
	return v.rangeKeys(v.w.players, func(key []byte, _ store.Info) bool { return fn(key) })
}

// PutPlayerRecord sets a players.hx record by its BDS key, as BDS stores it.
func (r *Round) PutPlayerRecord(key string, value []byte) {
	r.b.Put(r.w.players, KindPlayer, []byte(key), value)
}

// randomUUID returns a random (version 4) UUID in text form.
func randomUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}
