// Ported from Dragonfly (MIT):
// https://github.com/df-mc/dragonfly/blob/1793369709ceed934675d69e86ff6c828e4216c8/server/world/chunk/block_registry.go

package chunk

// BlockRegistry provides the minimal block registry API required by the chunk package.
//
// Implementations must be safe for concurrent read access after construction/finalization, as chunks and chunk
// encoding/decoding may happen from multiple goroutines.
type BlockRegistry interface {
	// BlockCount returns the number of runtime IDs known by the registry.
	BlockCount() int
	// AirRuntimeID returns the runtime ID representing air.
	AirRuntimeID() uint32
	// RuntimeIDToState resolves a runtime ID to its (name, properties) state representation.
	RuntimeIDToState(runtimeID uint32) (name string, properties map[string]any, found bool)
	// StateToRuntimeID resolves a (name, properties) state representation to a runtime ID.
	StateToRuntimeID(name string, properties map[string]any) (runtimeID uint32, found bool)
	// FilteringBlock returns the light filtering value for the runtime ID.
	FilteringBlock(rid uint32) uint8
	// LightBlock returns the light emission value for the runtime ID.
	LightBlock(rid uint32) uint8
	// HashToRuntimeID resolves a "network block hash" to a runtime ID.
	HashToRuntimeID(hash uint32) (rid uint32, ok bool)
	// RuntimeIDToHash resolves a runtime ID to its "network block hash".
	RuntimeIDToHash(runtimeID uint32) (hash uint32, ok bool)
}
