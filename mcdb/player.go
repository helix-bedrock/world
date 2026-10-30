package mcdb

import (
	"crypto/rand"
	"errors"
	"fmt"

	"github.com/helix-bedrock/protocol/nbt"
)

// Player keys, as BDS 1.26.60 writes them: player_<id> lookup records, one
// per login ID, all pointing at the player_server_<uuid> record that holds
// the player's data. The server UUID is random, not a login ID.
const (
	playerLookupPrefix = "player_"
	playerServerPrefix = "player_server_"
)

// playerLookup is a player_<id> record. Fields are in the sorted order BDS
// writes them.
type playerLookup struct {
	MsaID        string `nbt:"MsaId"`
	SelfSignedID string `nbt:"SelfSignedId"`
	ServerID     string `nbt:"ServerId"`
}

// LoadPlayer returns the saved data of a player, found through the lookup
// record of either ID (the MSA ID first, as it does not change between
// devices).
func (db *DB) LoadPlayer(ids PlayerIDs) (map[string]any, bool, error) {
	look, ok, err := db.playerLookup(ids)
	if !ok || err != nil {
		return nil, ok, err
	}
	b, err := db.buf.get([]byte(look.ServerID))
	if errors.Is(err, ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("load player %v: %w", look.ServerID, err)
	}
	var data map[string]any
	if err := nbt.UnmarshalEncoding(b, &data, nbt.LittleEndian); err != nil {
		return nil, false, fmt.Errorf("load player %v: %w", look.ServerID, err)
	}
	return data, true, nil
}

// StorePlayer saves a player's data, creating its lookup records on the
// first save.
func (db *DB) StorePlayer(ids PlayerIDs, data map[string]any) error {
	if ids.MsaID == "" && ids.SelfSignedID == "" {
		return errors.New("store player: no ID")
	}
	look, ok, err := db.playerLookup(ids)
	if err != nil {
		return err
	}
	t := &txn{}
	if !ok || look.MsaID != ids.MsaID || look.SelfSignedID != ids.SelfSignedID {
		if !ok {
			id, err := randomUUID()
			if err != nil {
				return fmt.Errorf("store player: %w", err)
			}
			look.ServerID = playerServerPrefix + id
		}
		look.MsaID, look.SelfSignedID = ids.MsaID, ids.SelfSignedID
		b, err := nbt.MarshalEncoding(look, nbt.LittleEndian)
		if err != nil {
			return fmt.Errorf("store player: %w", err)
		}
		for _, id := range [...]string{ids.MsaID, ids.SelfSignedID} {
			if id != "" {
				t.Put([]byte(playerLookupPrefix+id), b)
			}
		}
	}
	b, err := nbt.MarshalEncoding(data, nbt.LittleEndian)
	if err != nil {
		return fmt.Errorf("store player %v: %w", look.ServerID, err)
	}
	t.Put([]byte(look.ServerID), b)
	db.buf.apply(t)
	return nil
}

// playerLookup reads the lookup record of the first ID that has one.
func (db *DB) playerLookup(ids PlayerIDs) (playerLookup, bool, error) {
	for _, id := range [...]string{ids.MsaID, ids.SelfSignedID} {
		if id == "" {
			continue
		}
		b, err := db.buf.get([]byte(playerLookupPrefix + id))
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return playerLookup{}, false, fmt.Errorf("player lookup %v: %w", id, err)
		}
		var look playerLookup
		if err := nbt.UnmarshalEncoding(b, &look, nbt.LittleEndian); err != nil {
			return playerLookup{}, false, fmt.Errorf("player lookup %v: %w", id, err)
		}
		if len(look.ServerID) <= len(playerServerPrefix) {
			return playerLookup{}, false, fmt.Errorf("player lookup %v: bad server ID %q", id, look.ServerID)
		}
		return look, true, nil
	}
	return playerLookup{}, false, nil
}

// randomUUID returns a random (version 4) UUID in text form.
func randomUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
