package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// RunInfo is what the measurement needs to know about one run.
type RunInfo struct {
	ID       uuid.UUID
	Idx      int
	Count    int
	MaxSeq   int
	Terminal bool
	Chat     bool
}

// Strata are the run pools latency is measured over.
type Strata struct {
	LargeMin int
	Pools    map[string][]*RunInfo // small | median | large
	Largest  *RunInfo
	Note     string
}

// buildStrata draws up to 20 terminal runs per stratum. Large is Count >= largeMin; small is
// the 5th to 25th percentile and median the 40th to 60th percentile of the remaining terminal
// runs by message count.
func buildStrata(infos []*RunInfo, largeMin int) *Strata {
	s := &Strata{LargeMin: largeMin, Pools: map[string][]*RunInfo{}}
	var rest, large []*RunInfo
	for _, in := range infos {
		if !in.Terminal {
			continue
		}
		if s.Largest == nil || in.Count > s.Largest.Count {
			s.Largest = in
		}
		if in.Count >= largeMin {
			large = append(large, in)
		} else {
			rest = append(rest, in)
		}
	}
	sort.Slice(rest, func(i, j int) bool { return rest[i].Count < rest[j].Count })
	window := func(lo, hi float64) []*RunInfo {
		if len(rest) == 0 {
			return nil
		}
		a, b := int(lo*float64(len(rest))), int(hi*float64(len(rest)))
		b = max(b, a+1)
		b = min(b, len(rest))
		a = min(a, b-1)
		w := rest[a:b]
		if len(w) > 20 {
			step := float64(len(w)) / 20
			var picked []*RunInfo
			for i := 0; i < 20; i++ {
				picked = append(picked, w[int(float64(i)*step)])
			}
			return picked
		}
		return w
	}
	s.Pools["small"] = window(0.05, 0.25)
	s.Pools["median"] = window(0.40, 0.60)
	sort.Slice(large, func(i, j int) bool { return large[i].Count > large[j].Count })
	if len(large) == 0 {
		// No run reaches the threshold (a small real export): the three biggest stand in.
		all := append([]*RunInfo(nil), rest...)
		sort.Slice(all, func(i, j int) bool { return all[i].Count > all[j].Count })
		large = all[:min(3, len(all))]
		s.Note = fmt.Sprintf("no terminal run has >= %d messages; the large stratum is the %d largest runs", largeMin, len(large))
	}
	s.Pools["large"] = large[:min(20, len(large))]
	return s
}

// Describe is the strata summary for results.json.
func (s *Strata) Describe() map[string]any {
	out := map[string]any{"large_min_messages": s.LargeMin}
	for name, p := range s.Pools {
		lo, hi := 0, 0
		for i, r := range p {
			if i == 0 || r.Count < lo {
				lo = r.Count
			}
			hi = max(hi, r.Count)
		}
		out[name] = map[string]int{"runs": len(p), "min_messages": lo, "max_messages": hi}
	}
	if s.Largest != nil {
		out["largest_run_messages"] = s.Largest.Count
	}
	if s.Note != "" {
		out["note"] = s.Note
	}
	return out
}

// Env is the state shared by every phase.
type Env struct {
	Opt     Options
	Admin   *pgxpool.Pool
	BaseDSN string
	Eng     *Engine
	Src     Source
	Infos   []*RunInfo
	Strata  *Strata
	User    uuid.UUID
	Lg      *logger

	// Reference digests captured on the unrolled corpus: per shape+run for the reader checks,
	// and per run for the every-field round-trip check.
	ShapeDigest map[string]string
	RunDigest   map[uuid.UUID]string
	ChatDigest  string
	Seqs        map[uuid.UUID][]int

	// CompactTotal is pg_total_relation_size(run_messages) of a fresh corpus clone after VACUUM
	// FULL (0 when it could not be taken): the real-bytes pre-rollup size of every run.
	CompactTotal int64
}

// dsnFor swaps the database name in a DSN.
func dsnFor(dsn, db string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", errors.New("cannot parse --dsn")
	}
	u.Path = "/" + db
	return u.String(), nil
}

func (e *Env) dropDB(ctx context.Context, name string) error {
	_, err := e.Admin.Exec(ctx, `DROP DATABASE IF EXISTS `+pgx.Identifier{name}.Sanitize()+` WITH (FORCE)`)
	return err
}

// createDB drops and recreates name, optionally from a template. A template is cloned with
// STRATEGY FILE_COPY (a file-level copy, no per-block WAL for the clone). A template cannot be
// copied while any session is connected to it, so a busy refusal is retried briefly.
func (e *Env) createDB(ctx context.Context, name, template string) error {
	if err := e.dropDB(ctx, name); err != nil {
		return err
	}
	stmt := `CREATE DATABASE ` + pgx.Identifier{name}.Sanitize()
	if template != "" {
		stmt += ` TEMPLATE ` + pgx.Identifier{template}.Sanitize() + ` STRATEGY FILE_COPY`
	}
	var err error
	for i := 0; i < 20; i++ {
		if _, err = e.Admin.Exec(ctx, stmt); err == nil || !strings.Contains(err.Error(), "being accessed by other users") {
			return err
		}
		time.Sleep(time.Second)
	}
	return err
}

// openDB opens a pool on database name with the given extra runtime parameters.
func (e *Env) openDB(ctx context.Context, name string, maxConns int, rt map[string]string) (*pgxpool.Pool, string, error) {
	dsn, err := dsnFor(e.BaseDSN, name)
	if err != nil {
		return nil, "", err
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, "", errors.New("cannot parse --dsn")
	}
	cfg.MaxConns = int32(maxConns) //nolint:gosec // G115: a small positive connection count chosen by this tool.
	for k, v := range rt {
		cfg.ConnConfig.RuntimeParams[k] = v
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	return pool, dsn, err
}

// newMigratedDB creates a database at schema head.
func (e *Env) newMigratedDB(ctx context.Context, name string, maxConns int, rt map[string]string) (*pgxpool.Pool, error) {
	if err := e.createDB(ctx, name, ""); err != nil {
		return nil, err
	}
	pool, dsn, err := e.openDB(ctx, name, maxConns, rt)
	if err != nil {
		return nil, err
	}
	if err := store.Migrate(ctx, dsn); err != nil {
		pool.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if _, err := pool.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS pgstattuple`); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pgstattuple: %w", err)
	}
	return pool, nil
}

// loadInfos reads per-run counts from the loaded corpus.
func loadInfos(ctx context.Context, pool *pgxpool.Pool) ([]*RunInfo, error) {
	rows, err := pool.Query(ctx, `SELECT r.id, r.kind = 'chat', r.status IN ('completed','failed','cancelled'), count(m.id), coalesce(max(m.seq), 0)
		FROM runs r LEFT JOIN run_messages m ON m.run_id = r.id GROUP BY r.id, r.created_at ORDER BY r.created_at, r.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*RunInfo
	for rows.Next() {
		in := &RunInfo{Idx: len(out)}
		if err := rows.Scan(&in.ID, &in.Chat, &in.Terminal, &in.Count, &in.MaxSeq); err != nil {
			return nil, err
		}
		if in.Count > 0 {
			out = append(out, in)
		}
	}
	return out, rows.Err()
}

// compactBaseline records the VACUUM FULL size of the whole corpus on a clone (see Env.CompactTotal).
func (e *Env) compactBaseline(ctx context.Context) (int64, error) {
	const name = "m0_compact"
	if err := e.createDB(ctx, name, "m0_corpus"); err != nil {
		return 0, err
	}
	defer func() { _ = e.dropDB(context.WithoutCancel(ctx), name) }()
	pool, _, err := e.openDB(ctx, name, 2, nil)
	if err != nil {
		return 0, err
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `VACUUM FULL run_messages`); err != nil {
		return 0, err
	}
	return relTotal(ctx, pool, "run_messages")
}
