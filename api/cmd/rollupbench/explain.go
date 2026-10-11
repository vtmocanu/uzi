package main

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// explainChunkFetch records EXPLAIN (ANALYZE, BUFFERS) of the chunk metadata query and of the
// chunk body fetch for three representative requests. The queries bind their parameters, and
// the plan text is scrubbed of anything uuid-shaped the planner echoes; it carries no payload,
// because the queries filter on keys and return the bodies to the client, not into the plan.
func (e *Env) explainChunkFetch(ctx context.Context, pool *pgxpool.Pool) map[string]string {
	type request struct {
		name string
		run  *RunInfo
		spec ReadSpec
	}
	var reqs []request
	if l := e.Strata.Largest; l != nil {
		reqs = append(reqs,
			request{"first_page_200_largest_run", l, ReadSpec{Limit: 200}},
			request{"tail_200_largest_run", l, ReadSpec{Before: l.MaxSeq + 1, Limit: 200, Desc: true}})
	}
	if lp := e.Strata.Pools["large"]; len(lp) > 0 {
		reqs = append(reqs, request{"judge_signal_scan_large_run", lp[0], ReadSpec{Kinds: toolKinds, Limit: 4000, Proj: projTrace}})
	}
	out := map[string]string{}
	for _, r := range reqs {
		txt, err := explainRequest(ctx, pool, r.run.ID, r.spec)
		if err != nil {
			txt += "\nnot recorded: " + scrub(err.Error())
		}
		out[r.name] = txt
	}
	return out
}

func explainRequest(ctx context.Context, pool *pgxpool.Pool, run uuid.UUID, sp ReadSpec) (string, error) {
	tx, err := pool.BeginTx(ctx, roTx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var b strings.Builder
	b.WriteString("-- chunk metadata query\n")
	sql, args := metasQuery(run, sp, 0, false)
	plan, err := explainText(ctx, tx, "", sql, args)
	b.WriteString(plan)
	if err != nil {
		return b.String(), err
	}
	metas, err := (&pgArchStore{q: tx, run: run}).metas(ctx, sp, 0, false)
	if err != nil {
		return b.String(), err
	}
	if len(metas) == 0 {
		b.WriteString("\n-- chunk body fetch: not recorded, the metadata query found no chunks for this request\n")
		return b.String(), nil
	}
	var firsts []int
	for i := 0; i < len(metas) && i < 2; i++ {
		firsts = append(firsts, metas[i].first)
	}
	b.WriteString("\n-- chunk body fetch (first two candidate chunks)\n")
	// SERIALIZE (PG17) makes the plan include the cost of detoasting and converting the bytea
	// result, which is where the chunk bodies' I/O sits.
	plan, err = explainText(ctx, tx, ", SERIALIZE TEXT", sqlChunkBodies, []any{run, firsts})
	b.WriteString(plan)
	return b.String(), err
}

// explainText runs EXPLAIN (ANALYZE, BUFFERS, FORMAT TEXT<extra>) over sql.
func explainText(ctx context.Context, q queryer, extra, sql string, args []any) (string, error) {
	rows, err := q.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT TEXT"+extra+") "+sql, args...)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			return scrub(strings.Join(lines, "\n")), err
		}
		lines = append(lines, l)
	}
	return scrub(strings.Join(lines, "\n")), rows.Err()
}
