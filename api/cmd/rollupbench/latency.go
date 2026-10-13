package main

import (
	"context"
	"fmt"
	"runtime"
	"sync/atomic"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Cell is one (backend, shape, stratum, readers) latency measurement.
type Cell struct {
	Backend    string  `json:"backend"`
	Shape      string  `json:"shape"`
	Stratum    string  `json:"stratum"`
	Readers    int     `json:"readers"`
	Iterations int     `json:"iterations"`
	Errors     int     `json:"errors"`
	FirstError string  `json:"first_error,omitempty"`
	Latency    Latency `json:"latency"`

	RowsPerReq        float64 `json:"rows_per_request"`
	PayloadBytesPerRq float64 `json:"payload_bytes_returned_per_request"`
	// Archived-only decode accounting.
	ChunksFetchedPerReq float64 `json:"chunks_fetched_per_request,omitempty"`
	ChunksMatchedPerReq float64 `json:"chunks_matched_per_request,omitempty"`
	DecodedBytesPerRow  float64 `json:"decoded_bytes_per_returned_row,omitempty"`
	// Single-reader resource use (process-wide deltas, so only meaningful at 1 reader).
	AllocBytesPerReq float64 `json:"alloc_bytes_per_request,omitempty"`
	CPUMsPerReq      float64 `json:"go_cpu_ms_per_request,omitempty"`
	// Peak heap, sampled at the highest reader count only: HeapInuse after a forced GC right
	// before the cell is the baseline, the C6 input is the peak above it, and the absolute
	// peak is kept as an extra.
	PeakHeapInuse        uint64 `json:"peak_heap_inuse_bytes,omitempty"`
	HeapBaseline         uint64 `json:"heap_inuse_baseline_bytes,omitempty"`
	PeakHeapOverBaseline uint64 `json:"peak_heap_inuse_over_baseline_bytes,omitempty"`
}

// MatrixOpts controls one backend's matrix.
type MatrixOpts struct {
	Backend    string
	Readers    []int
	Iterations int
	PeakAt     int // reader count at which the heap sampler runs
	Seed       uint64
}

// stratumPool returns the pool a shape runs over for a stratum name.
func (e *Env) stratumPool(sh *Shape, stratum string) []*RunInfo {
	if sh.RunsOnly == "largest" {
		if e.Strata.Largest == nil {
			return nil
		}
		return []*RunInfo{e.Strata.Largest}
	}
	return e.Strata.Pools[stratum]
}

func strataFor(sh *Shape) []string {
	if sh.RunsOnly == "largest" {
		return []string{"largest"}
	}
	return []string{"small", "median", "large"}
}

// measureMatrix runs every shape over every stratum and reader count for one backend.
func (e *Env) measureMatrix(ctx context.Context, rd Reader, mo MatrixOpts) ([]Cell, error) {
	var cells []Cell
	shapes := readerShapes()
	for si := range shapes {
		sh := &shapes[si]
		for _, stratum := range strataFor(sh) {
			pool := e.stratumPool(sh, stratum)
			if len(pool) == 0 {
				continue
			}
			for _, readers := range mo.Readers {
				if err := ctx.Err(); err != nil {
					return cells, err
				}
				c := e.measureCell(ctx, rd, sh, stratum, pool, readers, mo)
				cells = append(cells, c)
			}
		}
	}
	e.Lg.f("matrix %s: %d cells", mo.Backend, len(cells))
	return cells, nil
}

func (e *Env) measureCell(ctx context.Context, rd Reader, sh *Shape, stratum string, pool []*RunInfo, readers int, mo MatrixOpts) Cell {
	iters := max(readers*2, mo.Iterations/sh.Heavy)
	if readers != critReaders(mo.Readers) {
		iters = max(readers*2, iters/2) // the bars are evaluated at one reader count; the others are context
	} else {
		iters = max(iters, sh.MinIters) // a p95 needs enough samples at the evaluated reader count
	}
	c := Cell{Backend: mo.Backend, Shape: sh.Name, Stratum: stratum, Readers: readers}
	// A fixed walk over the stratum's runs: every worker visits different runs at each step,
	// and two backends see the same sequence, so they are compared on the same requests.
	pick := func(w, i int) *RunInfo {
		return pool[(w*131+i*31+len(sh.Name))%len(pool)]
	}
	// Untimed warm-up so the first requests do not pay for cold caches or statement parsing.
	for i := 0; i < min(max(iters/10, 3), 50); i++ {
		run := pick(0, i)
		_, _, _ = rd.Read(ctx, sh, run.ID, sh.Spec(run.MaxSeq))
	}
	var rows, payload, fetched, matched, decoded atomic.Int64
	var m0, m1 runtime.MemStats
	var sampler *heapSampler
	runtime.GC()
	runtime.ReadMemStats(&m0)
	if readers == mo.PeakAt {
		sampler = startHeapSampler()
	}
	cpu0 := cpuSeconds()
	durs, nerr, first := loopWorkers(ctx, readers, iters, func(w, i int) error {
		run := pick(w, i)
		out, st, err := rd.Read(ctx, sh, run.ID, sh.Spec(run.MaxSeq))
		if err != nil {
			return err
		}
		rows.Add(int64(len(out)))
		payload.Add(st.PayloadBytes)
		fetched.Add(int64(st.ChunksFetched))
		matched.Add(int64(st.ChunksMatched))
		decoded.Add(st.DecodedBytes)
		return nil
	})
	cpu1 := cpuSeconds()
	runtime.ReadMemStats(&m1)
	if sampler != nil {
		c.PeakHeapInuse = max(sampler.finish(), m0.HeapInuse)
		c.HeapBaseline = m0.HeapInuse
		c.PeakHeapOverBaseline = c.PeakHeapInuse - c.HeapBaseline
	}
	c.Iterations = len(durs)
	c.Errors = nerr
	if first != nil {
		c.FirstError = scrub(first.Error())
	}
	c.Latency = summarize(durs)
	if n := float64(len(durs)); n > 0 {
		c.RowsPerReq = float64(rows.Load()) / n
		c.PayloadBytesPerRq = float64(payload.Load()) / n
		c.ChunksFetchedPerReq = float64(fetched.Load()) / n
		c.ChunksMatchedPerReq = float64(matched.Load()) / n
		if rows.Load() > 0 {
			c.DecodedBytesPerRow = float64(decoded.Load()) / float64(rows.Load())
		}
		if readers == 1 {
			c.AllocBytesPerReq = float64(m1.TotalAlloc-m0.TotalAlloc) / n
			c.CPUMsPerReq = (cpu1 - cpu0) * 1000 / n
		}
	}
	return c
}

// ChatCell is the ListChatRunsForUser measurement.
type ChatCell struct {
	Backend string  `json:"backend"`
	Chats   int     `json:"chat_runs"`
	Readers int     `json:"readers"`
	Latency Latency `json:"latency"`
	Errors  int     `json:"errors"`
}

func (e *Env) measureChat(ctx context.Context, pool *pgxpool.Pool, backend string, archived, inTx bool, readers, iters int) ChatCell {
	c := ChatCell{Backend: backend, Readers: readers}
	for _, in := range e.Infos {
		if in.Chat && in.Terminal {
			c.Chats++
		}
	}
	for i := 0; i < 3; i++ {
		_, _ = ListChats(ctx, pool, e.User, archived, inTx)
	}
	durs, nerr, _ := loopWorkers(ctx, readers, max(readers*2, iters/10), func(_, _ int) error {
		_, err := ListChats(ctx, pool, e.User, archived, inTx)
		return err
	})
	c.Latency = summarize(durs)
	c.Errors = nerr
	return c
}

// shapeKey indexes a reference digest.
func shapeKey(shape string, run uuid.UUID) string { return shape + "|" + run.String() }

// captureDigests records, on the unrolled corpus, the result digest of every shape for every
// pooled run, and of the full every-field replay of every terminal run.
func (e *Env) captureDigests(ctx context.Context, pool *pgxpool.Pool) error {
	live := &LiveReader{Pool: pool}
	e.ShapeDigest = map[string]string{}
	e.RunDigest = map[uuid.UUID]string{}
	e.Seqs = map[uuid.UUID][]int{}
	shapes := readerShapes()
	seen := map[string]bool{}
	for si := range shapes {
		sh := &shapes[si]
		for _, stratum := range strataFor(sh) {
			for _, run := range e.stratumPool(sh, stratum) {
				k := shapeKey(sh.Name, run.ID)
				if seen[k] {
					continue
				}
				seen[k] = true
				out, _, err := live.Read(ctx, sh, run.ID, sh.Spec(run.MaxSeq))
				if err != nil {
					return fmt.Errorf("capture %s: %w", sh.Name, err)
				}
				e.ShapeDigest[k] = digestRecords(out)
			}
		}
	}
	full := ReadSpec{}
	fullShape := &Shape{LiveSQL: sqlAfter, Args: func(run uuid.UUID, sp ReadSpec) []any { return []any{run, sp.After} }}
	for _, in := range e.Infos {
		if !in.Terminal {
			continue
		}
		out, _, err := live.Read(ctx, fullShape, in.ID, full)
		if err != nil {
			return fmt.Errorf("capture run digest: %w", err)
		}
		e.RunDigest[in.ID] = digestRecords(out)
		seqs := make([]int, len(out))
		for i := range out {
			seqs[i] = out[i].Seq
		}
		e.Seqs[in.ID] = seqs
	}
	chats, err := ListChats(ctx, pool, e.User, false, false)
	if err != nil {
		return err
	}
	e.ChatDigest = digestChats(chats)
	return nil
}

// checkDigests re-runs every captured shape against the archived reader and reports mismatches.
type DigestCheck struct {
	ShapeRuns  int      `json:"shape_run_combinations_checked"`
	Mismatches int      `json:"mismatches"`
	Errors     int      `json:"errors"`
	Samples    []string `json:"first_mismatches,omitempty"`
}

func (e *Env) checkDigests(ctx context.Context, arch Reader, pool *pgxpool.Pool, stragglers bool) DigestCheck {
	var dc DigestCheck
	shapes := readerShapes()
	seen := map[string]bool{}
	note := func(s string) {
		if len(dc.Samples) < 5 {
			dc.Samples = append(dc.Samples, s)
		}
	}
	for si := range shapes {
		sh := &shapes[si]
		for _, stratum := range strataFor(sh) {
			for _, run := range e.stratumPool(sh, stratum) {
				k := shapeKey(sh.Name, run.ID)
				if seen[k] {
					continue
				}
				seen[k] = true
				dc.ShapeRuns++
				out, _, err := arch.Read(ctx, sh, run.ID, sh.Spec(run.MaxSeq))
				switch {
				case err != nil:
					dc.Errors++
					note(sh.Name + ": " + scrub(err.Error()))
				case digestRecords(trimStragglers(out, run.MaxSeq)) != e.ShapeDigest[k]:
					dc.Mismatches++
					note(sh.Name + ": result differs from the unrolled corpus (stratum " + stratum + ")")
				}
			}
		}
	}
	if stragglers {
		// Straggler rows carry a later created_at, which legitimately moves a chat run's
		// last_message_at, so the chat list is verified on the configs that run without them.
		return dc
	}
	dc.ShapeRuns++
	if chats, err := ListChats(ctx, pool, e.User, true, true); err != nil || digestChats(chats) != e.ChatDigest {
		dc.Mismatches++
		note("chat list differs from the unrolled corpus")
	}
	return dc
}

// trimStragglers drops rows above the run's original last seq: the concurrency check inserts
// late rows strictly above every source row, so removing them restores the source result.
func trimStragglers(recs []Record, maxSeq int) []Record {
	out := recs[:0:0]
	for i := range recs {
		if recs[i].Seq <= maxSeq {
			out = append(out, recs[i])
		}
	}
	return out
}
