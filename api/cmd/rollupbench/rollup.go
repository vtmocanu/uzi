package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	chunkTable   = "m0_run_message_chunks"
	archiveTable = "m0_run_archive"
)

// createChunkTables adds the harness-owned chunk and watermark tables (not a migration) and
// turns autovacuum off for the tables the backfill rewrites, so the measured disk and WAL
// numbers are not shaped by a background worker.
func createChunkTables(ctx context.Context, pool *pgxpool.Pool) error {
	stmts := []string{
		`CREATE TABLE ` + chunkTable + ` (
			run_id uuid NOT NULL REFERENCES runs ON DELETE CASCADE,
			first_seq int NOT NULL,
			last_seq int NOT NULL,
			codec text NOT NULL,
			kinds text[] NOT NULL,
			last_created_at timestamptz NOT NULL,
			raw_bytes int NOT NULL,
			n_rows int NOT NULL,
			data bytea NOT NULL,
			PRIMARY KEY (run_id, first_seq))`,
		`ALTER TABLE ` + chunkTable + ` ALTER COLUMN data SET STORAGE EXTERNAL`,
		`CREATE TABLE ` + archiveTable + ` (
			run_id uuid PRIMARY KEY REFERENCES runs ON DELETE CASCADE,
			archived_through int NOT NULL)`,
		`ALTER TABLE ` + chunkTable + ` SET (autovacuum_enabled = false, toast.autovacuum_enabled = false)`,
		`ALTER TABLE run_messages SET (autovacuum_enabled = false, toast.autovacuum_enabled = false)`,
	}
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s); err != nil {
			return fmt.Errorf("chunk tables: %w", err)
		}
	}
	return nil
}

// RollupConfig is one materialised configuration.
type RollupConfig struct {
	Spec           CodecSpec
	Bounds         ChunkBounds
	MaxChunksPerTx int
}

// Name is the stable config label used in results.
func (c RollupConfig) Name() string {
	return fmt.Sprintf("%s/%dKiB/%d", c.Spec.Name(), c.Bounds.ByteBound>>10, c.Bounds.CountCeiling)
}

// TxStat describes one rollup transaction.
type TxStat struct {
	Rows      int
	Chunks    int
	RawBytes  int64
	CompBytes int64
	Duration  time.Duration
	LockWait  time.Duration
}

// errRoundTrip marks a decode-verify mismatch inside the rollup tx. The tx is rolled back, so
// nothing is deleted, and the caller counts it as a C4 failure.
var errRoundTrip = errors.New("rollup: decoded chunk differs from the source rows")

const rowCols = `id, seq, kind, agent, payload::text, created_at, agent_instance, agent_label, claim_generation`

// scanSourceRow reads one row of rowCols.
func scanSourceRow(rows pgx.Rows) (Record, error) {
	var r Record
	var payload string
	var ts time.Time
	if err := rows.Scan(&r.ID, &r.Seq, &r.Kind, &r.Agent, &payload, &ts, &r.AgentInstance, &r.AgentLabel, &r.ClaimGeneration); err != nil {
		return r, err
	}
	r.Payload = []byte(payload)
	r.CreatedAtMicro = ts.UnixMicro()
	return r, nil
}

// rollupRun archives one terminal run, in one transaction or (for a big run) several, each
// publishing a prefix. It returns a TxStat per committed tx and whether the run was skipped
// because it was not terminal.
func rollupRun(ctx context.Context, pool *pgxpool.Pool, eng *Engine, cfg RollupConfig, runID uuid.UUID, pause time.Duration, onTx func(TxStat)) (skipped bool, err error) {
	for {
		st, more, skip, err := rollupTx(ctx, pool, eng, cfg, runID)
		if err != nil {
			return false, err
		}
		if skip {
			return true, nil
		}
		if st.Rows > 0 && onTx != nil {
			onTx(st)
		}
		if !more {
			return false, nil
		}
		if pause > 0 {
			time.Sleep(pause) // lets concurrent readers observe the run between its published prefixes
		}
	}
}

func rollupTx(ctx context.Context, pool *pgxpool.Pool, eng *Engine, cfg RollupConfig, runID uuid.UUID) (st TxStat, more bool, skipped bool, err error) {
	start := time.Now()
	tx, err := pool.Begin(ctx)
	if err != nil {
		return st, false, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	lockStart := time.Now()
	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM runs WHERE id = $1 FOR UPDATE`, runID).Scan(&status); err != nil {
		return st, false, false, err
	}
	st.LockWait = time.Since(lockStart)
	if status != "completed" && status != "failed" && status != "cancelled" {
		return st, false, true, nil
	}
	var wm int
	if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT archived_through FROM `+archiveTable+` WHERE run_id = $1), 0)`, runID).Scan(&wm); err != nil {
		return st, false, false, err
	}
	limit := cfg.MaxChunksPerTx * cfg.Bounds.CountCeiling
	rows, err := tx.Query(ctx, `SELECT `+rowCols+` FROM run_messages WHERE run_id = $1 AND seq > $2 ORDER BY seq ASC LIMIT $3`, runID, wm, limit)
	if err != nil {
		return st, false, false, err
	}
	var recs []Record
	for rows.Next() {
		r, err := scanSourceRow(rows)
		if err != nil {
			rows.Close()
			return st, false, false, err
		}
		recs = append(recs, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return st, false, false, err
	}
	if len(recs) == 0 {
		return st, false, false, nil
	}
	more = len(recs) == limit

	chunks := SplitChunks(recs, cfg.Bounds)
	batch := &pgx.Batch{}
	encoded := make([][]byte, len(chunks))
	for i, c := range chunks {
		data, err := eng.Encode(cfg.Spec, c)
		if err != nil {
			return st, false, false, err
		}
		encoded[i] = data
		var raw int
		for k := range c {
			raw += encodedSize(&c[k])
		}
		batch.Queue(`INSERT INTO `+chunkTable+` (run_id, first_seq, last_seq, codec, kinds, last_created_at, raw_bytes, n_rows, data)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			runID, c[0].Seq, c[len(c)-1].Seq, cfg.Spec.Name(), chunkKinds(c), time.UnixMicro(maxCreated(c)).UTC(), raw, len(c), data)
		st.RawBytes += int64(raw)
		st.CompBytes += int64(len(data))
	}
	br := tx.SendBatch(ctx, batch)
	for range chunks {
		if _, err := br.Exec(); err != nil {
			_ = br.Close()
			return st, false, false, err
		}
	}
	if err := br.Close(); err != nil {
		return st, false, false, err
	}
	// Decode-verify every chunk against the rows it was built from, before anything is deleted.
	for i, c := range chunks {
		got, _, err := eng.Decode(encoded[i])
		if err != nil || len(got) != len(c) {
			return st, false, false, errRoundTrip
		}
		for k := range c {
			if !recordsEqual(&c[k], &got[k]) {
				return st, false, false, errRoundTrip
			}
		}
	}
	for _, c := range chunks {
		tag, err := tx.Exec(ctx, `DELETE FROM run_messages WHERE run_id = $1 AND seq BETWEEN $2 AND $3`, runID, c[0].Seq, c[len(c)-1].Seq)
		if err != nil {
			return st, false, false, err
		}
		if int(tag.RowsAffected()) != len(c) {
			return st, false, false, fmt.Errorf("rollup: deleted %d rows for a %d-row chunk", tag.RowsAffected(), len(c))
		}
	}
	last := chunks[len(chunks)-1]
	if _, err := tx.Exec(ctx, `INSERT INTO `+archiveTable+` (run_id, archived_through) VALUES ($1, $2)
		ON CONFLICT (run_id) DO UPDATE SET archived_through = EXCLUDED.archived_through`, runID, last[len(last)-1].Seq); err != nil {
		return st, false, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return st, false, false, err
	}
	st.Rows = len(recs)
	st.Chunks = len(chunks)
	st.Duration = time.Since(start)
	return st, more, false, nil
}

func maxCreated(c []Record) int64 {
	m := c[0].CreatedAtMicro
	for i := range c {
		m = max(m, c[i].CreatedAtMicro)
	}
	return m
}
