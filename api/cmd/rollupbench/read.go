package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Proj selects which columns a reader shape returns, matching the live query it stands in for.
type Proj int

const (
	projFull  Proj = iota // the ten-column run_messages row (store.RunMessage)
	projTrace             // seq, kind, payload (ListToolTraceForRun)
	projJobs              // seq, created_at, kind, payload->>'text' (ListJobMessagesForCaller, message part)
)

// ReadSpec is what a reader shape asks of the log: seq > After and (when Before > 0)
// seq < Before, restricted to Kinds, in ascending or Desc order, at most Limit rows
// (Limit 0 = unbounded). Kind filtering happens BEFORE the limit.
type ReadSpec struct {
	After  int
	Before int
	Limit  int
	Kinds  []string
	Desc   bool
	Proj   Proj
}

// ReadStats is what the archived path measures per request.
type ReadStats struct {
	ChunksFetched int
	ChunksMatched int
	DecodedBytes  int64
	LiveRows      int
	RowsOut       int
	PayloadBytes  int64
}

// Shape is one named reader shape. LiveSQL is the exact text of the production query, copied
// from api/internal/store/queries so the baseline documents the shape and does not move when a
// generated store method does.
type Shape struct {
	Name     string
	LiveSQL  string
	Args     func(run uuid.UUID, sp ReadSpec) []any
	Spec     func(last int) ReadSpec
	Heavy    int // iteration divisor: heavier shapes run fewer iterations (recorded in results)
	MinIters int // floor on the iterations at the reader count the bars are evaluated at
	RunsOnly string
}

// The SQL below is copied verbatim (whitespace aside) from the named store queries.
const (
	// runtime.sql ListRunMessagesAfter
	sqlAfter = `SELECT id, run_id, seq, kind, agent, payload, created_at, agent_instance, agent_label, claim_generation
FROM run_messages WHERE run_id = $1 AND seq > $2 ORDER BY seq ASC`
	// runtime.sql ListRunMessagesAfterPage and ListRunMessagesForWorkerPage (identical text)
	sqlAfterPage = `SELECT id, run_id, seq, kind, agent, payload, created_at, agent_instance, agent_label, claim_generation
FROM run_messages WHERE run_id = $1 AND seq > $2 ORDER BY seq ASC LIMIT $3`
	// runtime.sql ListRunMessagesBeforePage
	sqlBeforePage = `SELECT id, run_id, seq, kind, agent, payload, created_at, agent_instance, agent_label, claim_generation
FROM run_messages WHERE run_id = $1 AND seq < $2 ORDER BY seq DESC LIMIT $3`
	// judge.sql ListToolTraceForRun
	sqlToolTrace = `SELECT seq, kind, payload FROM run_messages
WHERE run_id = $1 AND kind IN ('tool_use', 'tool_result') ORDER BY seq ASC LIMIT $2`
	// runtime.sql ListRunUsageFrames
	sqlUsageFrames = `SELECT id, run_id, seq, kind, agent, payload, created_at, agent_instance, agent_label, claim_generation
FROM run_messages WHERE run_id = $1 AND kind IN ('status', 'error', 'progress_note', 'summary_usage') ORDER BY seq ASC`
	// jobs.sql ListJobMessagesForCaller: ONLY the run_messages part. The production query also
	// joins runs (kind = 'job', user) and job_origins; the harness corpus has no job runs, so
	// the join predicates are omitted on both sides of the comparison and the kinds filter plus
	// COALESCE(payload->>'text','') projection, which is what touches run_messages, is kept.
	sqlJobsText = `SELECT m.seq, m.created_at, m.kind, COALESCE(m.payload->>'text', '')::text AS text
FROM run_messages m WHERE m.run_id = $1 AND m.seq > $2 AND m.kind IN ('text', 'status', 'error') ORDER BY m.seq ASC LIMIT $3`
	// chat.sql ListChatRunsForUser
	sqlChatLive = `SELECT * FROM (
    SELECT r.id, r.title, r.status, r.resume_of_run_id, r.created_at, r.updated_at,
           (SELECT count(*) FROM run_user_inputs i WHERE i.run_id = r.id AND i.kind = 'follow_up') AS turn_count,
           (SELECT max(m.created_at) FROM run_messages m WHERE m.run_id = r.id)::timestamptz AS last_message_at
    FROM runs r
    WHERE r.user_id = $1 AND r.kind = 'chat'
) chat
ORDER BY COALESCE(chat.last_message_at, chat.created_at) DESC
LIMIT 200`
	// The archived variant: last_message_at is the later of the newest chunk's last_created_at
	// and the newest live row. GREATEST ignores NULLs, so a run with only one side still reads.
	sqlChatArchived = `SELECT * FROM (
    SELECT r.id, r.title, r.status, r.resume_of_run_id, r.created_at, r.updated_at,
           (SELECT count(*) FROM run_user_inputs i WHERE i.run_id = r.id AND i.kind = 'follow_up') AS turn_count,
           GREATEST((SELECT max(c.last_created_at) FROM ` + chunkTable + ` c WHERE c.run_id = r.id),
                    (SELECT max(m.created_at) FROM run_messages m WHERE m.run_id = r.id))::timestamptz AS last_message_at
    FROM runs r
    WHERE r.user_id = $1 AND r.kind = 'chat'
) chat
ORDER BY COALESCE(chat.last_message_at, chat.created_at) DESC
LIMIT 200`
)

var (
	toolKinds  = []string{"tool_use", "tool_result"}
	usageKinds = []string{"status", "error", "progress_note", "summary_usage"}
	jobKinds   = []string{"text", "status", "error"}
)

func argsAfter(run uuid.UUID, sp ReadSpec) []any { return []any{run, sp.After, sp.Limit} }

// readerShapes is the shape inventory of the plan, in reporting order.
func readerShapes() []Shape {
	return []Shape{
		{Name: "first_page_200", LiveSQL: sqlAfterPage, Args: argsAfter, Heavy: 1,
			Spec: func(int) ReadSpec { return ReadSpec{After: 0, Limit: 200} }},
		{Name: "first_page_1000", LiveSQL: sqlAfterPage, Args: argsAfter, Heavy: 2,
			Spec: func(int) ReadSpec { return ReadSpec{After: 0, Limit: 1000} }},
		{Name: "tail_200", LiveSQL: sqlBeforePage, Heavy: 1,
			Args: func(run uuid.UUID, sp ReadSpec) []any { return []any{run, sp.Before, sp.Limit} },
			Spec: func(last int) ReadSpec { return ReadSpec{Before: last + 1, Limit: 200, Desc: true} }},
		{Name: "tail_1000", LiveSQL: sqlBeforePage, Heavy: 2,
			Args: func(run uuid.UUID, sp ReadSpec) []any { return []any{run, sp.Before, sp.Limit} },
			Spec: func(last int) ReadSpec { return ReadSpec{Before: last + 1, Limit: 1000, Desc: true} }},
		{Name: "mid_backward_200", LiveSQL: sqlBeforePage, Heavy: 1,
			Args: func(run uuid.UUID, sp ReadSpec) []any { return []any{run, sp.Before, sp.Limit} },
			Spec: func(last int) ReadSpec { return ReadSpec{Before: max(2, last/2), Limit: 200, Desc: true} }},
		{Name: "judge_trace_page_200", LiveSQL: sqlAfterPage, Args: argsAfter, Heavy: 1,
			Spec: func(last int) ReadSpec { return ReadSpec{After: last / 2, Limit: 200} }},
		{Name: "judge_signal_scan_4000", LiveSQL: sqlToolTrace, Heavy: 10, Spec: func(int) ReadSpec {
			return ReadSpec{Kinds: toolKinds, Limit: 4000, Proj: projTrace}
		}, Args: func(run uuid.UUID, sp ReadSpec) []any { return []any{run, sp.Limit} }},
		{Name: "usage_frames_scan", LiveSQL: sqlUsageFrames, Heavy: 2, Spec: func(int) ReadSpec {
			return ReadSpec{Kinds: usageKinds}
		}, Args: func(run uuid.UUID, _ ReadSpec) []any { return []any{run} }},
		{Name: "jobs_text_200", LiveSQL: sqlJobsText, Heavy: 1, Spec: func(int) ReadSpec {
			return ReadSpec{Kinds: jobKinds, Limit: 200, Proj: projJobs}
		}, Args: argsAfter},
		{Name: "full_replay", LiveSQL: sqlAfter, Heavy: 100, MinIters: 50, RunsOnly: "largest", Spec: func(int) ReadSpec {
			return ReadSpec{After: 0}
		}, Args: func(run uuid.UUID, sp ReadSpec) []any { return []any{run, sp.After} }},
	}
}

// Reader serves one shape for one run.
type Reader interface {
	Read(ctx context.Context, sh *Shape, run uuid.UUID, sp ReadSpec) ([]Record, ReadStats, error)
}

type queryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

var roTx = pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}

// scanProj reads rows of the given projection.
func scanProj(rows pgx.Rows, proj Proj, out []Record) ([]Record, error) {
	defer rows.Close() // also on a scan error, so the connection is never returned mid-result
	for rows.Next() {
		var r Record
		var ts time.Time
		var err error
		switch proj {
		case projFull:
			var rid uuid.UUID
			err = rows.Scan(&r.ID, &rid, &r.Seq, &r.Kind, &r.Agent, &r.Payload, &ts, &r.AgentInstance, &r.AgentLabel, &r.ClaimGeneration)
			r.CreatedAtMicro = ts.UnixMicro()
		case projTrace:
			err = rows.Scan(&r.Seq, &r.Kind, &r.Payload)
		case projJobs:
			var text string
			err = rows.Scan(&r.Seq, &ts, &r.Kind, &text)
			r.CreatedAtMicro = ts.UnixMicro()
			r.Payload = []byte(text)
		}
		if err != nil {
			return out, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LiveReader runs the production SQL against run_messages. With InTx it wraps each request in
// the same REPEATABLE READ READ ONLY transaction the archived reader needs, so the transaction
// overhead is reported separately instead of being charged to the archive.
type LiveReader struct {
	Pool *pgxpool.Pool
	InTx bool
}

func (l *LiveReader) Read(ctx context.Context, sh *Shape, run uuid.UUID, sp ReadSpec) ([]Record, ReadStats, error) {
	var q queryer = l.Pool
	var tx pgx.Tx
	if l.InTx {
		var err error
		if tx, err = l.Pool.BeginTx(ctx, roTx); err != nil {
			return nil, ReadStats{}, err
		}
		q = tx
	}
	rows, err := q.Query(ctx, sh.LiveSQL, sh.Args(run, sp)...)
	if err != nil {
		if tx != nil {
			_ = tx.Rollback(ctx)
		}
		return nil, ReadStats{}, err
	}
	out, err := scanProj(rows, sp.Proj, nil)
	if tx != nil {
		if err == nil {
			err = tx.Commit(ctx)
		} else {
			_ = tx.Rollback(ctx)
		}
	}
	st := ReadStats{RowsOut: len(out), LiveRows: len(out)}
	for i := range out {
		st.PayloadBytes += int64(len(out[i].Payload))
	}
	return out, st, err
}

// archStore is the storage a archived read needs: the watermark, chunk metadata and bodies for
// one run, and the live rows above the watermark. The Postgres implementation is pgArchStore;
// the unit tests use an in-memory one so the paging rules are checked without a database.
type archStore interface {
	watermark(ctx context.Context) (int, error)
	metas(ctx context.Context, sp ReadSpec, cursor int, haveCursor bool) ([]chunkMeta, error)
	chunkData(ctx context.Context, firsts []int) (map[int][]byte, error)
	live(ctx context.Context, sp ReadSpec, lowBound, want int) ([]Record, error)
}

type chunkMeta struct{ first, last, n int }

const metaPage = 512

func kindSet(kinds []string) map[string]bool {
	if len(kinds) == 0 {
		return nil
	}
	m := make(map[string]bool, len(kinds))
	for _, k := range kinds {
		m[k] = true
	}
	return m
}

// ArchReader serves a run from its chunks plus the live tail above the watermark, in one
// REPEATABLE READ READ ONLY transaction, with no cache.
type ArchReader struct {
	Pool *pgxpool.Pool
	Eng  *Engine
}

func (a *ArchReader) Read(ctx context.Context, _ *Shape, run uuid.UUID, sp ReadSpec) ([]Record, ReadStats, error) {
	tx, err := a.Pool.BeginTx(ctx, roTx)
	if err != nil {
		return nil, ReadStats{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	out, st, err := readArchived(ctx, &pgArchStore{q: tx, run: run}, a.Eng, sp)
	if err != nil {
		return nil, st, err
	}
	return out, st, tx.Commit(ctx)
}

// readArchived is the read algorithm: watermark first, then candidate chunks decoded one group
// at a time until the limit (applied after the kind filter) is met, then the live tail.
func readArchived(ctx context.Context, store archStore, eng *Engine, sp ReadSpec) ([]Record, ReadStats, error) {
	var st ReadStats
	wm, err := store.watermark(ctx)
	if err != nil {
		return nil, st, err
	}
	var out []Record
	remaining := func() int {
		if sp.Limit == 0 {
			return math.MaxInt
		}
		return sp.Limit - len(out)
	}
	liveTail := func() error {
		rows, err := store.live(ctx, sp, max(sp.After, wm), remaining())
		out = append(out, rows...)
		st.LiveRows += len(rows)
		return err
	}
	chunks := func() error {
		var err error
		out, err = readChunks(ctx, store, eng, sp, out, &st)
		return err
	}
	if !sp.Desc {
		if wm > sp.After {
			if err := chunks(); err != nil {
				return nil, st, err
			}
		}
		if remaining() > 0 {
			if err := liveTail(); err != nil {
				return nil, st, err
			}
		}
	} else {
		if err := liveTail(); err != nil {
			return nil, st, err
		}
		if remaining() > 0 && wm > sp.After && (sp.Before == 0 || sp.Before > 1) {
			if err := chunks(); err != nil {
				return nil, st, err
			}
		}
	}
	for i := range out {
		st.PayloadBytes += int64(len(out[i].Payload))
	}
	st.RowsOut = len(out)
	return out, st, nil
}

// readChunks decodes only the chunks that can hold rows of the request.
func readChunks(ctx context.Context, store archStore, eng *Engine, sp ReadSpec, out []Record, st *ReadStats) ([]Record, error) {
	want := math.MaxInt
	if sp.Limit > 0 {
		want = sp.Limit - len(out)
	}
	kinds := kindSet(sp.Kinds)
	var metas []chunkMeta
	mi, cursor, haveCursor, exhausted := 0, 0, false, false
	group := 2
	for want > 0 {
		if mi == len(metas) {
			if exhausted {
				break
			}
			var err error
			if metas, err = store.metas(ctx, sp, cursor, haveCursor); err != nil {
				return out, err
			}
			mi = 0
			exhausted = len(metas) < metaPage
			if len(metas) == 0 {
				break
			}
			cursor, haveCursor = metas[len(metas)-1].first, true
		}
		take, rowsEst := 0, 0
		for mi+take < len(metas) {
			m := metas[mi+take]
			n := m.n
			if kinds == nil && sp.Limit > 0 && take == 0 {
				// The first chunk may straddle the cursor; count only the seq span past it.
				if !sp.Desc && sp.After >= m.first {
					n = min(n, m.last-sp.After)
				}
				if sp.Desc && sp.Before > 0 && sp.Before <= m.last {
					n = min(n, sp.Before-m.first)
				}
			}
			rowsEst += n
			take++
			if kinds == nil && sp.Limit > 0 && rowsEst >= want {
				break
			}
			if kinds == nil && sp.Limit == 0 && take >= 16 {
				break
			}
			if kinds != nil && take >= group {
				break
			}
		}
		if kinds != nil {
			group = min(group*2, 32)
		}
		firsts := make([]int, take)
		for i := range firsts {
			firsts[i] = metas[mi+i].first
		}
		mi += take
		data, err := store.chunkData(ctx, firsts)
		if err != nil {
			return out, err
		}
		for _, f := range firsts {
			d, ok := data[f]
			if !ok {
				return out, fmt.Errorf("chunk %d missing from a candidate set", f)
			}
			recs, h, err := eng.Decode(d)
			if err != nil {
				return out, err
			}
			st.ChunksFetched++
			st.DecodedBytes += int64(h.RawLen)
			matched := false
			emit := func(r *Record) bool {
				if r.Seq <= sp.After || (sp.Before > 0 && r.Seq >= sp.Before) || (kinds != nil && !kinds[r.Kind]) {
					return false
				}
				rc := *r
				project(&rc, sp.Proj)
				out = append(out, rc)
				matched = true
				want--
				return want <= 0
			}
			if sp.Desc {
				for i := len(recs) - 1; i >= 0; i-- {
					if emit(&recs[i]) {
						break
					}
				}
			} else {
				for i := range recs {
					if emit(&recs[i]) {
						break
					}
				}
			}
			if matched {
				st.ChunksMatched++
			}
			if want <= 0 {
				return out, nil
			}
		}
	}
	return out, nil
}

// pgArchStore is archStore over Postgres inside one transaction.
type pgArchStore struct {
	q   queryer
	run uuid.UUID
}

func (p *pgArchStore) watermark(ctx context.Context) (int, error) {
	wm := 0
	err := p.q.QueryRow(ctx, `SELECT archived_through FROM `+archiveTable+` WHERE run_id = $1`, p.run).Scan(&wm)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return wm, err
}

func (p *pgArchStore) metas(ctx context.Context, sp ReadSpec, cursor int, haveCursor bool) ([]chunkMeta, error) {
	sql, args := metasQuery(p.run, sp, cursor, haveCursor)
	rows, err := p.q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []chunkMeta
	for rows.Next() {
		var m chunkMeta
		if err := rows.Scan(&m.first, &m.last, &m.n); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// metasQuery is the candidate-chunk metadata query of one request, with its bind arguments.
func metasQuery(run uuid.UUID, sp ReadSpec, cursor int, haveCursor bool) (string, []any) {
	args := []any{run}
	where := `run_id = $1`
	add := func(cond string, v any) {
		args = append(args, v)
		where += " AND " + fmt.Sprintf(cond, len(args))
	}
	if sp.After > 0 {
		add(`last_seq > $%d`, sp.After)
	}
	if sp.Before > 0 {
		add(`first_seq < $%d`, sp.Before)
	}
	if len(sp.Kinds) > 0 {
		add(`kinds && $%d::text[]`, sp.Kinds)
	}
	if haveCursor {
		if sp.Desc {
			add(`first_seq < $%d`, cursor)
		} else {
			add(`first_seq > $%d`, cursor)
		}
	}
	order := `ASC`
	if sp.Desc {
		order = `DESC`
	}
	return `SELECT first_seq, last_seq, n_rows FROM ` + chunkTable + ` WHERE ` + where +
		fmt.Sprintf(` ORDER BY first_seq %s LIMIT %d`, order, metaPage), args
}

// sqlChunkBodies fetches the bodies of the chunks chosen from the metadata.
const sqlChunkBodies = `SELECT first_seq, data FROM ` + chunkTable + ` WHERE run_id = $1 AND first_seq = ANY($2)`

func (p *pgArchStore) chunkData(ctx context.Context, firsts []int) (map[int][]byte, error) {
	rows, err := p.q.Query(ctx, sqlChunkBodies, p.run, firsts)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	data := make(map[int][]byte, len(firsts))
	for rows.Next() {
		var f int
		var d []byte
		if err := rows.Scan(&f, &d); err != nil {
			return nil, err
		}
		data[f] = d
	}
	return data, rows.Err()
}

// live reads the rows above the watermark with the same SQL family as the live readers.
func (p *pgArchStore) live(ctx context.Context, sp ReadSpec, lowBound, want int) ([]Record, error) {
	cols := map[Proj]string{
		projFull:  `id, run_id, seq, kind, agent, payload, created_at, agent_instance, agent_label, claim_generation`,
		projTrace: `seq, kind, payload`,
		projJobs:  `seq, created_at, kind, COALESCE(payload->>'text', '')::text`,
	}[sp.Proj]
	args := []any{p.run, lowBound}
	sql := `SELECT ` + cols + ` FROM run_messages WHERE run_id = $1 AND seq > $2`
	if sp.Before > 0 {
		args = append(args, sp.Before)
		sql += fmt.Sprintf(` AND seq < $%d`, len(args))
	}
	if len(sp.Kinds) > 0 {
		sql += ` AND kind IN ('` + strings.Join(sp.Kinds, `','`) + `')`
	}
	if sp.Desc {
		sql += ` ORDER BY seq DESC`
	} else {
		sql += ` ORDER BY seq ASC`
	}
	if want != math.MaxInt {
		args = append(args, want)
		sql += fmt.Sprintf(` LIMIT $%d`, len(args))
	}
	rows, err := p.q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return scanProj(rows, sp.Proj, nil)
}

// project reduces a decoded record to the columns the shape returns, so archived and live
// results compare field for field.
func project(r *Record, proj Proj) {
	switch proj {
	case projTrace:
		*r = Record{Seq: r.Seq, Kind: r.Kind, Payload: r.Payload}
	case projJobs:
		*r = Record{Seq: r.Seq, Kind: r.Kind, CreatedAtMicro: r.CreatedAtMicro, Payload: []byte(jsonText(r.Payload))}
	}
}

// jsonText mirrors COALESCE(payload->>'text', ”): a string value unquoted, other scalars as
// their JSON text, an absent or null value as the empty string.
func jsonText(payload []byte) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(payload, &m) != nil {
		return ""
	}
	raw, ok := m["text"]
	if !ok || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

// sameResult reports whether two result sets are identical for the projection.
func sameResult(a, b []Record, proj Proj) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		switch proj {
		case projFull:
			if !recordsEqual(&a[i], &b[i]) {
				return false
			}
		case projTrace:
			if a[i].Seq != b[i].Seq || a[i].Kind != b[i].Kind || !jsonEqual(a[i].Payload, b[i].Payload) {
				return false
			}
		case projJobs:
			if a[i].Seq != b[i].Seq || a[i].Kind != b[i].Kind || a[i].CreatedAtMicro != b[i].CreatedAtMicro || string(a[i].Payload) != string(b[i].Payload) {
				return false
			}
		}
	}
	return true
}

func jsonEqual(a, b []byte) bool { return string(a) == string(b) }

// ChatRow is one ListChatRunsForUser result row reduced to what is compared.
type ChatRow struct {
	ID      uuid.UUID
	LastMsg *time.Time
	TurnCnt int64
	Status  string
	Created time.Time
	Updated time.Time
	Resume  *uuid.UUID
	Title   *string
}

// ListChats runs either the live or the archived chat-list query.
func ListChats(ctx context.Context, pool *pgxpool.Pool, user uuid.UUID, archived, inTx bool) ([]ChatRow, error) {
	var q queryer = pool
	var tx pgx.Tx
	if inTx {
		var err error
		if tx, err = pool.BeginTx(ctx, roTx); err != nil {
			return nil, err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		q = tx
	}
	sql := sqlChatLive
	if archived {
		sql = sqlChatArchived
	}
	rows, err := q.Query(ctx, sql, user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChatRow
	for rows.Next() {
		var c ChatRow
		if err := rows.Scan(&c.ID, &c.Title, &c.Status, &c.Resume, &c.Created, &c.Updated, &c.TurnCnt, &c.LastMsg); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if tx != nil {
		return out, tx.Commit(ctx)
	}
	return out, nil
}
