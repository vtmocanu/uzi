package main

import (
	"context"
	"runtime"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// GridCell is one point of the offline size grid.
type GridCell struct {
	Codec          string  `json:"codec"`
	ByteBoundKiB   int     `json:"byte_bound_kib"`
	CountCeiling   int     `json:"count_ceiling"`
	Chunks         int     `json:"chunks"`
	RawBytes       int64   `json:"uncompressed_bytes"`
	CompBytes      int64   `json:"compressed_bytes"`
	Ratio          float64 `json:"compressed_over_uncompressed"`
	EncodeMBps     float64 `json:"encode_mb_per_s"`
	DecodeMBps     float64 `json:"decode_mb_per_s"`
	DecodeAllocPer float64 `json:"decode_alloc_bytes_per_chunk"`
}

var (
	gridBounds   = []int{64 << 10, 128 << 10, 256 << 10, 512 << 10}
	gridCeilings = []int{500, 1000, 2000}
	gridSpecs    = []CodecSpec{{codecFlate, 1}, {codecFlate, 6}, {codecFlate, 9}, {codecZstd, 1}, {codecZstd, 3}, {codecZstd, 9}}
)

// GridSample describes what the grid ran over.
type GridSample struct {
	Runs     int   `json:"runs"`
	Rows     int   `json:"rows"`
	RawBytes int64 `json:"uncompressed_bytes"`
	CapMiB   int   `json:"cap_mib"`
}

// sampleForGrid reads terminal runs (seeded random order) until the cap is reached, always
// including the run that holds the largest message so the oversized-record path is in the sample.
func sampleForGrid(ctx context.Context, pool *pgxpool.Pool, infos []*RunInfo, seed uint64, capMiB int) ([][]Record, GridSample, error) {
	var terminal []*RunInfo
	for _, in := range infos {
		if in.Terminal {
			terminal = append(terminal, in)
		}
	}
	rng := newRNG(seed, 77)
	rng.Shuffle(len(terminal), func(i, j int) { terminal[i], terminal[j] = terminal[j], terminal[i] })
	var big uuid.UUID
	_ = pool.QueryRow(ctx, `SELECT run_id FROM run_messages m JOIN runs r ON r.id = m.run_id WHERE r.status IN ('completed','failed','cancelled')
		ORDER BY octet_length(payload::text) DESC LIMIT 1`).Scan(&big)
	hoist := func() {
		for i, in := range terminal {
			if in.ID == big {
				terminal[0], terminal[i] = terminal[i], terminal[0]
				return
			}
		}
	}
	hoist()
	var out [][]Record
	sm := GridSample{CapMiB: capMiB}
	for _, in := range terminal {
		if sm.RawBytes >= int64(capMiB)<<20 {
			break
		}
		rows, err := pool.Query(ctx, `SELECT `+rowCols+` FROM run_messages WHERE run_id = $1 ORDER BY seq`, in.ID)
		if err != nil {
			return nil, sm, err
		}
		var recs []Record
		for rows.Next() {
			r, err := scanSourceRow(rows)
			if err != nil {
				rows.Close()
				return nil, sm, err
			}
			recs = append(recs, r)
			sm.RawBytes += int64(len(r.Payload))
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, sm, err
		}
		out = append(out, recs)
		sm.Runs++
		sm.Rows += len(recs)
	}
	return out, sm, nil
}

// runGrid measures ratio and speed for every bound x ceiling x codec level over the sample.
func runGrid(ctx context.Context, eng *Engine, runs [][]Record, lg *logger) ([]GridCell, error) {
	var cells []GridCell
	for _, bb := range gridBounds {
		for _, cc := range gridCeilings {
			var chunks [][]Record
			for _, r := range runs {
				chunks = append(chunks, SplitChunks(r, ChunkBounds{ByteBound: bb, CountCeiling: cc})...)
			}
			var raw int64
			for _, c := range chunks {
				for i := range c {
					raw += int64(encodedSize(&c[i]))
				}
			}
			for _, spec := range gridSpecs {
				if err := ctx.Err(); err != nil {
					return cells, err
				}
				encoded := make([][]byte, len(chunks))
				t0 := time.Now()
				var comp int64
				for i, c := range chunks {
					d, err := eng.Encode(spec, c)
					if err != nil {
						return cells, err
					}
					encoded[i] = d
					comp += int64(len(d))
				}
				encDur := time.Since(t0)
				var m0, m1 runtime.MemStats
				runtime.ReadMemStats(&m0)
				t1 := time.Now()
				for _, d := range encoded {
					if _, _, err := eng.Decode(d); err != nil {
						return cells, err
					}
				}
				decDur := time.Since(t1)
				runtime.ReadMemStats(&m1)
				cell := GridCell{Codec: spec.Name(), ByteBoundKiB: bb >> 10, CountCeiling: cc, Chunks: len(chunks), RawBytes: raw, CompBytes: comp,
					Ratio: float64(comp) / float64(raw), EncodeMBps: float64(raw) / encDur.Seconds() / 1e6, DecodeMBps: float64(raw) / decDur.Seconds() / 1e6,
					DecodeAllocPer: float64(m1.TotalAlloc-m0.TotalAlloc) / float64(len(chunks))}
				cells = append(cells, cell)
			}
			lg.f("grid %dKiB/%d done", bb>>10, cc)
		}
	}
	return cells, nil
}

// Selection is one config to materialise and why it was picked.
type Selection struct {
	Config RollupConfig
	Why    []string
}

// pickConfigs chooses the best flate and zstd cells (smallest ratio; within 0.2% of the best
// ratio the faster decode wins) plus the two reference configs, de-duplicated.
func pickConfigs(cells []GridCell, maxChunksPerTx int) []Selection {
	best := func(prefix string) GridCell {
		var b GridCell
		first := true
		for _, c := range cells {
			if len(c.Codec) < len(prefix) || c.Codec[:len(prefix)] != prefix {
				continue
			}
			switch {
			case first:
				b, first = c, false
			case c.Ratio < b.Ratio*0.998, c.Ratio <= b.Ratio*1.002 && c.DecodeMBps > b.DecodeMBps:
				b = c
			}
		}
		return b
	}
	toCfg := func(c GridCell) RollupConfig {
		spec := CodecSpec{ID: codecFlate}
		if len(c.Codec) > 4 && c.Codec[:4] == "zstd" {
			spec.ID = codecZstd
		}
		for _, s := range gridSpecs {
			if s.Name() == c.Codec {
				spec = s
			}
		}
		return RollupConfig{Spec: spec, Bounds: ChunkBounds{ByteBound: c.ByteBoundKiB << 10, CountCeiling: c.CountCeiling}, MaxChunksPerTx: maxChunksPerTx}
	}
	var out []Selection
	add := func(cfg RollupConfig, why string) {
		for i := range out {
			if out[i].Config.Name() == cfg.Name() {
				out[i].Why = append(out[i].Why, why)
				return
			}
		}
		out = append(out, Selection{Config: cfg, Why: []string{why}})
	}
	add(toCfg(best("flate")), "best flate by ratio")
	add(toCfg(best("zstd")), "best zstd by ratio")
	add(RollupConfig{Spec: CodecSpec{codecFlate, 6}, Bounds: ChunkBounds{ByteBound: 256 << 10, CountCeiling: 1000}, MaxChunksPerTx: maxChunksPerTx}, "reference flate-6 256KiB/1000")
	add(RollupConfig{Spec: CodecSpec{codecZstd, 3}, Bounds: ChunkBounds{ByteBound: 256 << 10, CountCeiling: 1000}, MaxChunksPerTx: maxChunksPerTx}, "reference zstd-3 256KiB/1000")
	return out
}
