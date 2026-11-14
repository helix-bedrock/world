package convert

import (
	"time"

	"github.com/helix-bedrock/world/hx"
)

// Phase is the step a conversion is at.
type Phase string

// Phases of Import and Export, in order.
const (
	// PhaseFiles copies level.dat and BDS's other files.
	PhaseFiles Phase = "files"
	// PhaseChunks converts chunks (Import also reads players and level keys
	// here, in BDS's key order).
	PhaseChunks Phase = "chunks"
	// PhaseOrphans keeps entities and digp lists no chunk took (Import).
	PhaseOrphans Phase = "orphans"
	// PhaseRecords writes level keys and players (Export).
	PhaseRecords Phase = "records"
	// PhaseDone is reported once, when the conversion has finished.
	PhaseDone Phase = "done"
)

// Progress is how far a conversion has got.
type Progress struct {
	Phase Phase
	// Keys counts BDS keys read (Import) or written (Export); Bytes their
	// values' bytes.
	Keys, Bytes int64
	// Chunks counts chunks done; BlockEntities and Entities what they held.
	Chunks, BlockEntities, Entities int64
	// Dim and Pos are the chunk being converted, so a chunk that takes long
	// (a chest bomb) shows which one it is.
	Dim hx.Dimension
	Pos hx.ChunkPos
	// Elapsed is the time since the conversion started.
	Elapsed time.Duration
}

const defaultProgressEvery = time.Second

// progress calls Options.Progress at each phase and at most once per
// interval, from the converting goroutine.
type progress struct {
	fn    func(Progress)
	every time.Duration
	start time.Time
	last  time.Time
	p     Progress
}

func newProgress(o Options) progress {
	now := time.Now()
	return progress{fn: o.Progress, every: o.ProgressEvery, start: now, last: now}
}

// chunk notes the chunk being started.
func (p *progress) chunk(id chunkID) {
	p.p.Dim, p.p.Pos = id.dim, id.pos
	p.tick()
}

// tick reports if the interval has passed.
func (p *progress) tick() {
	if p.fn == nil {
		return
	}
	if now := time.Now(); now.Sub(p.last) >= p.every {
		p.last = now
		p.report()
	}
}

// phase starts a phase and reports it at once.
func (p *progress) phase(ph Phase) {
	p.p.Phase = ph
	if p.fn != nil {
		p.last = time.Now()
		p.report()
	}
}

func (p *progress) report() {
	p.p.Elapsed = time.Since(p.start)
	p.fn(p.p)
}
