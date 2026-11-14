package chunk

import (
	"fmt"
	"log/slog"

	"github.com/helix-bedrock/protocol/nbt"
	"github.com/helix-bedrock/world/cube"
)

// MaxTicks is the most entries a valid saved tick queue of a chunk in a dimension of range r
// can hold: one tick per block position and layer, each with a later tick it still waits for
// (as Helix saves a merged tick). No queue that ran can be longer, so a list past it came from
// a damaged or hostile file and the rest is left out instead of filling memory.
func MaxTicks(r cube.Range) int {
	const layers, timesPerTick = 2, 2
	return 16 * 16 * (r.Height() + 1) * layers * timesPerTick
}

// DecodeTicks reads a tick queue as BDS stores it (currentTick and tickList) for the chunk at
// pos in a dimension of range r. The list is decoded one entry at a time, entries outside the
// chunk are left out, and at most MaxTicks are kept, so a file cannot make it use more memory
// than a full chunk of ticks. Problems with single entries are logged on log, not returned.
func DecodeTicks(data []byte, bpe BlockPaletteEncoding, pos [2]int32, r cube.Range, log *slog.Logger) ([]ScheduledBlockUpdate, int64, error) {
	head, err := nbt.DecodeWithout(data, nbt.LittleEndian, func(name string) bool { return name == "tickList" })
	if err != nil {
		return nil, 0, fmt.Errorf("read nbt: %w", err)
	}
	current, _ := head["currentTick"].(int32)
	limit := MaxTicks(r)
	var updates []ScheduledBlockUpdate
	outside, over := 0, false
	err = nbt.EachListCompound(data, nbt.LittleEndian, "tickList", func(m map[string]any) bool {
		if len(updates) >= limit {
			over = true
			return false
		}
		p := tickPos(m)
		if p[0]>>4 != int(pos[0]) || p[2]>>4 != int(pos[1]) || p[1] < r[0] || p[1] > r[1] {
			outside++
			return true
		}
		bl, _ := m["blockState"].(map[string]any)
		block, err := bpe.DecodeBlockState(bl)
		if err != nil {
			log.Error("tick queue: decode block state: " + err.Error())
			return true
		}
		t, _ := m["time"].(int64)
		updates = append(updates, ScheduledBlockUpdate{Pos: p, Block: block, Tick: t})
		return true
	})
	if err != nil {
		return nil, 0, fmt.Errorf("read tick list: %w", err)
	}
	if outside > 0 || over {
		log.Warn("tick queue: entries left out", "chunk", pos, "outside", outside, "past limit", over, "limit", limit)
	}
	return updates, int64(current), nil
}

// tickPos reads a tick's x, y and z.
func tickPos(m map[string]any) cube.Pos {
	x, _ := m["x"].(int32)
	y, _ := m["y"].(int32)
	z, _ := m["z"].(int32)
	return cube.Pos{int(x), int(y), int(z)}
}
