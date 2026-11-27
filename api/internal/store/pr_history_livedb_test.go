package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #4691 M2 against a REAL Postgres: SetRunMRState records PR merge/close history
// (pr_history_heads / pr_history_events) atomically with runs.mr_state through data-modifying
// CTEs. Run via e2e/run-store-it.sh (or any throwaway Postgres in UZI_TEST_DATABASE_URL).
//
// Every assertion reads the exact rows with plain SQL, not through a store query.

// prhT0 is the base instant for every fixture time; offsets are in minutes.
var prhT0 = time.Date(2026, 3, 10, 14, 0, 0, 0, time.UTC)

func prhAt(min int) time.Time { return prhT0.Add(time.Duration(min) * time.Minute) }

func prhTS(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

type prhFx struct {
	t      *testing.T
	ctx    context.Context
	pool   *pgxpool.Pool
	q      *store.Queries
	userID uuid.UUID
	connID uuid.UUID
	repoID uuid.UUID
	nextIx int32
}

func prhDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	return dsn
}

// newPRHFixture creates a user, a forge connection and a repo (enabled=false so the delete
// queries accept it) on a freshly migrated database. Cleanup removes the user (cascades).
func newPRHFixture(t *testing.T) *prhFx {
	t.Helper()
	dsn := prhDSN(t)
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	fx := &prhFx{t: t, ctx: ctx, pool: pool, q: store.New(pool),
		userID: uuid.New(), connID: uuid.New(), repoID: uuid.New()}
	fx.exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, fx.userID, fmt.Sprintf("prh-%s@e2e", fx.userID))
	fx.exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, fx.connID, fx.userID, []byte{0x1})
	fx.exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', false)`, fx.repoID, fx.connID)
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if _, err := pool.Exec(cctx, `DELETE FROM users WHERE id = $1`, fx.userID); err != nil {
			t.Errorf("cleanup user: %v", err)
		}
		pool.Close()
	})
	return fx
}

func (fx *prhFx) exec(sql string, args ...any) {
	fx.t.Helper()
	mustExec(fx.ctx, fx.t, fx.pool, sql, args...)
}

// newRun inserts a completed issue run for the given user and PR with an optional prior mr_state.
func (fx *prhFx) newRunFor(userID uuid.UUID, mrIID int64, priorState *string) uuid.UUID {
	fx.t.Helper()
	fx.nextIx++
	id := uuid.New()
	fx.exec(`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, mr_iid, mr_state)
		VALUES ($1, $2, $3, $4, 't', 'd', 'completed', $5, $6)`, id, userID, fx.repoID, fx.nextIx, mrIID, priorState)
	return id
}

func (fx *prhFx) newRun(mrIID int64, priorState string) uuid.UUID {
	var p *string
	if priorState != "" {
		p = &priorState
	}
	return fx.newRunFor(fx.userID, mrIID, p)
}

// observe calls the generated SetRunMRState on the pool (autocommit).
func (fx *prhFx) observe(runID uuid.UUID, state string, observed time.Time, provider *time.Time) (int64, error) {
	return prhObserve(fx.ctx, fx.q, runID, state, observed, provider)
}

func prhObserve(ctx context.Context, q *store.Queries, runID uuid.UUID, state string, observed time.Time, provider *time.Time) (int64, error) {
	p := pgtype.Timestamptz{}
	if provider != nil {
		p = prhTS(*provider)
	}
	return q.SetRunMRState(ctx, store.SetRunMRStateParams{ID: runID, MrState: state, ObservedAt: prhTS(observed), ProviderAt: p})
}

func (fx *prhFx) mustObserve(runID uuid.UUID, state string, observed time.Time, provider *time.Time) {
	fx.t.Helper()
	n, err := fx.observe(runID, state, observed, provider)
	if err != nil {
		fx.t.Fatalf("SetRunMRState(%s): %v", state, err)
	}
	if n != 1 {
		fx.t.Fatalf("SetRunMRState(%s) = %d rows, want 1", state, n)
	}
}

type prhHead struct {
	State             string
	ObservedAt        time.Time
	LastProviderAt    *time.Time
	FirstMergedAt     *time.Time
	FirstMergedSource *string
}

type prhEvent struct {
	From       *string
	To         string
	ProviderAt *time.Time
	TimeSource string
	ObservedAt time.Time
	Bootstrap  bool
	FirstMerge bool
}

func (fx *prhFx) heads(mrIID int64) []prhHead {
	fx.t.Helper()
	rows, err := fx.pool.Query(fx.ctx, `SELECT state, observed_at, last_provider_at, first_merged_at, first_merged_source
		FROM pr_history_heads WHERE repo_id=$1 AND mr_iid=$2`, fx.repoID, mrIID)
	if err != nil {
		fx.t.Fatalf("heads: %v", err)
	}
	defer rows.Close()
	var out []prhHead
	for rows.Next() {
		var h prhHead
		if err := rows.Scan(&h.State, &h.ObservedAt, &h.LastProviderAt, &h.FirstMergedAt, &h.FirstMergedSource); err != nil {
			fx.t.Fatalf("scan head: %v", err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		fx.t.Fatalf("heads rows: %v", err)
	}
	return out
}

func (fx *prhFx) head(mrIID int64) prhHead {
	fx.t.Helper()
	hs := fx.heads(mrIID)
	if len(hs) != 1 {
		fx.t.Fatalf("heads for PR %d = %d rows, want exactly 1", mrIID, len(hs))
	}
	return hs[0]
}

func (fx *prhFx) events(mrIID int64) []prhEvent {
	fx.t.Helper()
	rows, err := fx.pool.Query(fx.ctx, `SELECT from_state, to_state, provider_at, time_source, observed_at, bootstrap, first_merge
		FROM pr_history_events WHERE repo_id=$1 AND mr_iid=$2 ORDER BY id`, fx.repoID, mrIID)
	if err != nil {
		fx.t.Fatalf("events: %v", err)
	}
	defer rows.Close()
	var out []prhEvent
	for rows.Next() {
		var e prhEvent
		if err := rows.Scan(&e.From, &e.To, &e.ProviderAt, &e.TimeSource, &e.ObservedAt, &e.Bootstrap, &e.FirstMerge); err != nil {
			fx.t.Fatalf("scan event: %v", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		fx.t.Fatalf("events rows: %v", err)
	}
	return out
}

func (fx *prhFx) mrState(runID uuid.UUID) *string {
	fx.t.Helper()
	var s *string
	if err := fx.pool.QueryRow(fx.ctx, `SELECT mr_state FROM runs WHERE id=$1`, runID).Scan(&s); err != nil {
		fx.t.Fatalf("mr_state: %v", err)
	}
	return s
}

func prhStrEq(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func prhTimeEq(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

func prhS(s string) *string { return &s }

func prhStr(p *string) string {
	if p == nil {
		return "<NULL>"
	}
	return *p
}

func prhTm(p *time.Time) string {
	if p == nil {
		return "<NULL>"
	}
	return p.UTC().Format(time.RFC3339)
}

// wantEvent is an exact expected event row.
type wantEvent struct {
	From       *string
	To         string
	ProviderAt *time.Time
	TimeSource string
	ObservedAt time.Time
	Bootstrap  bool
	FirstMerge bool
}

func (fx *prhFx) assertEvents(mrIID int64, want ...wantEvent) {
	fx.t.Helper()
	got := fx.events(mrIID)
	if len(got) != len(want) {
		fx.t.Fatalf("PR %d events: got %d rows, want %d: %+v", mrIID, len(got), len(want), got)
	}
	for i, w := range want {
		g := got[i]
		if !prhStrEq(g.From, w.From) || g.To != w.To || !prhTimeEq(g.ProviderAt, w.ProviderAt) || g.TimeSource != w.TimeSource ||
			!g.ObservedAt.Equal(w.ObservedAt) || g.Bootstrap != w.Bootstrap || g.FirstMerge != w.FirstMerge {
			fx.t.Fatalf("PR %d event[%d]: got {from=%s to=%s provider=%s src=%s obs=%s boot=%v first=%v}, want {from=%s to=%s provider=%s src=%s obs=%s boot=%v first=%v}",
				mrIID, i, prhStr(g.From), g.To, prhTm(g.ProviderAt), g.TimeSource, prhTm(&g.ObservedAt), g.Bootstrap, g.FirstMerge,
				prhStr(w.From), w.To, prhTm(w.ProviderAt), w.TimeSource, prhTm(&w.ObservedAt), w.Bootstrap, w.FirstMerge)
		}
	}
}

func (fx *prhFx) assertHead(mrIID int64, state string, observed time.Time, lastProvider, firstMergedAt *time.Time, firstSrc *string) {
	fx.t.Helper()
	h := fx.head(mrIID)
	if h.State != state || !h.ObservedAt.Equal(observed) || !prhTimeEq(h.LastProviderAt, lastProvider) ||
		!prhTimeEq(h.FirstMergedAt, firstMergedAt) || !prhStrEq(h.FirstMergedSource, firstSrc) {
		fx.t.Fatalf("PR %d head: got {state=%s obs=%s lastProv=%s firstMerged=%s src=%s}, want {state=%s obs=%s lastProv=%s firstMerged=%s src=%s}",
			mrIID, h.State, prhTm(&h.ObservedAt), prhTm(h.LastProviderAt), prhTm(h.FirstMergedAt), prhStr(h.FirstMergedSource),
			state, prhTm(&observed), prhTm(lastProvider), prhTm(firstMergedAt), prhStr(firstSrc))
	}
}

func (fx *prhFx) assertMRState(runID uuid.UUID, want string) {
	fx.t.Helper()
	if got := fx.mrState(runID); !prhStrEq(got, &want) {
		fx.t.Fatalf("runs.mr_state = %s, want %s", prhStr(got), want)
	}
}

func (fx *prhFx) assertNoHistory(mrIID int64) {
	fx.t.Helper()
	if hs := fx.heads(mrIID); len(hs) != 0 {
		fx.t.Fatalf("PR %d has %d heads, want none: %+v", mrIID, len(hs), hs)
	}
	if es := fx.events(mrIID); len(es) != 0 {
		fx.t.Fatalf("PR %d has %d events, want none: %+v", mrIID, len(es), es)
	}
}

func TestPRHistoryBasicsLiveDB(t *testing.T) {
	t.Run("cte pre-state then conflict path", func(t *testing.T) {
		fx := newPRHFixture(t)
		run := fx.newRun(1, "opened")
		fx.mustObserve(run, "closed", prhAt(1), nil)
		fx.assertEvents(1, wantEvent{From: prhS("opened"), To: "closed", TimeSource: "observation_fallback", ObservedAt: prhAt(1), Bootstrap: true})
		fx.assertHead(1, "closed", prhAt(1), nil, nil, nil)
		fx.mustObserve(run, "opened", prhAt(2), nil)
		fx.assertEvents(1,
			wantEvent{From: prhS("opened"), To: "closed", TimeSource: "observation_fallback", ObservedAt: prhAt(1), Bootstrap: true},
			wantEvent{From: prhS("closed"), To: "opened", TimeSource: "observation_fallback", ObservedAt: prhAt(2)})
		fx.assertHead(1, "opened", prhAt(2), nil, nil, nil)
		fx.assertMRState(run, "opened")
	})

	t.Run("concurrent duplicates", func(t *testing.T) {
		fx := newPRHFixture(t)
		for round := int64(1); round <= 20; round++ {
			a := fx.newRun(round, "")
			b := fx.newRun(round, "")
			obsA, obsB := prhAt(5), prhAt(5)
			if round%2 == 0 {
				obsB = prhAt(6) // alternate tied and strictly newer second observer
			}
			start := make(chan struct{})
			var wg sync.WaitGroup
			errs := make([]error, 2)
			rows := make([]int64, 2)
			for i, c := range []struct {
				id  uuid.UUID
				obs time.Time
			}{{a, obsA}, {b, obsB}} {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					rows[i], errs[i] = fx.observe(c.id, "merged", c.obs, nil)
				}()
			}
			close(start)
			wg.Wait()
			for i := range errs {
				if errs[i] != nil || rows[i] != 1 {
					t.Fatalf("round %d observer %d: rows=%d err=%v", round, i, rows[i], errs[i])
				}
			}
			hs := fx.heads(round)
			if len(hs) != 1 {
				t.Fatalf("round %d: %d heads, want 1", round, len(hs))
			}
			// The head keeps the newest observation whichever writer won the insert.
			wantObs := obsA
			if obsB.After(wantObs) {
				wantObs = obsB
			}
			if hs[0].State != "merged" || !hs[0].ObservedAt.Equal(wantObs) {
				t.Fatalf("round %d: head = %+v, want merged observed at %s", round, hs[0], wantObs)
			}
			var firstMerge int
			if err := fx.pool.QueryRow(fx.ctx, `SELECT count(*) FROM pr_history_events WHERE repo_id=$1 AND mr_iid=$2 AND first_merge`,
				fx.repoID, round).Scan(&firstMerge); err != nil {
				t.Fatal(err)
			}
			if firstMerge != 1 {
				t.Fatalf("round %d: %d first_merge events, want 1", round, firstMerge)
			}
			es := fx.events(round)
			if len(es) != 1 {
				t.Fatalf("round %d: %d events, want 1: %+v", round, len(es), es)
			}
			if e := es[0]; e.From != nil || e.To != "merged" || e.TimeSource != "observation_fallback" || !e.Bootstrap || !e.FirstMerge {
				t.Fatalf("round %d: event = %+v, want nil -> merged, observation_fallback, bootstrap, first_merge", round, e)
			}
			// Either writer may insert first, so the event carries one of the two submitted observations.
			if e := es[0]; !e.ObservedAt.Equal(obsA) && !e.ObservedAt.Equal(obsB) {
				t.Fatalf("round %d: event observed_at = %s, want %s or %s", round, prhTm(&e.ObservedAt), prhTm(&obsA), prhTm(&obsB))
			}
			if e := es[0]; e.ProviderAt != nil {
				t.Fatalf("round %d: event provider_at = %s, want nil (no provider time submitted)", round, prhTm(e.ProviderAt))
			}
			fx.assertMRState(a, "merged")
			fx.assertMRState(b, "merged")
		}
	})

	t.Run("prd example 1: two runs one first-merge event", func(t *testing.T) {
		fx := newPRHFixture(t)
		a, b := fx.newRun(1, ""), fx.newRun(1, "")
		prov := prhAt(2)
		fx.mustObserve(a, "merged", prhAt(5), &prov)
		fx.mustObserve(b, "merged", prhAt(5), &prov)
		fx.assertEvents(1, wantEvent{To: "merged", ProviderAt: &prov, TimeSource: "provider", ObservedAt: prhAt(5), Bootstrap: true, FirstMerge: true})
		fx.assertHead(1, "merged", prhAt(5), &prov, &prov, prhS("provider"))
	})

	t.Run("prd example 2: no provider time falls back", func(t *testing.T) {
		fx := newPRHFixture(t)
		run := fx.newRun(1, "")
		fx.mustObserve(run, "merged", prhAt(5), nil)
		fx.assertEvents(1, wantEvent{To: "merged", TimeSource: "observation_fallback", ObservedAt: prhAt(5), Bootstrap: true, FirstMerge: true})
		obs := prhAt(5)
		fx.assertHead(1, "merged", prhAt(5), nil, &obs, prhS("observation_fallback"))
	})

	t.Run("prd example 3: first observation already merged", func(t *testing.T) {
		fx := newPRHFixture(t)
		run := fx.newRun(1, "")
		prov := prhAt(-30)
		fx.mustObserve(run, "merged", prhAt(5), &prov)
		fx.assertEvents(1, wantEvent{To: "merged", ProviderAt: &prov, TimeSource: "provider", ObservedAt: prhAt(5), Bootstrap: true, FirstMerge: true})
	})

	t.Run("prd example 4: unchanged observations advance watermark only", func(t *testing.T) {
		fx := newPRHFixture(t)
		run := fx.newRun(1, "")
		fx.mustObserve(run, "opened", prhAt(1), nil)
		fx.mustObserve(run, "opened", prhAt(2), nil)
		fx.mustObserve(run, "opened", prhAt(3), nil)
		fx.assertEvents(1, wantEvent{To: "opened", TimeSource: "observation_fallback", ObservedAt: prhAt(1), Bootstrap: true})
		fx.assertHead(1, "opened", prhAt(3), nil, nil, nil)
	})
}

func TestPRHistoryOrderingLiveDB(t *testing.T) {
	t.Run("delayed older opened after merged", func(t *testing.T) {
		fx := newPRHFixture(t)
		a, b := fx.newRun(1, ""), fx.newRun(1, "")
		fx.mustObserve(a, "merged", prhAt(2), nil)
		fx.mustObserve(b, "opened", prhAt(1), nil)
		fx.assertHead(1, "merged", prhAt(2), nil, ptrOf(prhAt(2)), prhS("observation_fallback"))
		fx.assertEvents(1, wantEvent{To: "merged", TimeSource: "observation_fallback", ObservedAt: prhAt(2), Bootstrap: true, FirstMerge: true})
		fx.assertMRState(b, "opened")
	})

	t.Run("closed opened closed", func(t *testing.T) {
		fx := newPRHFixture(t)
		run := fx.newRun(1, "")
		fx.mustObserve(run, "closed", prhAt(1), nil)
		fx.mustObserve(run, "opened", prhAt(2), nil)
		fx.mustObserve(run, "closed", prhAt(3), nil)
		fx.assertEvents(1,
			wantEvent{To: "closed", TimeSource: "observation_fallback", ObservedAt: prhAt(1), Bootstrap: true},
			wantEvent{From: prhS("closed"), To: "opened", TimeSource: "observation_fallback", ObservedAt: prhAt(2)},
			wantEvent{From: prhS("opened"), To: "closed", TimeSource: "observation_fallback", ObservedAt: prhAt(3)})
		fx.assertHead(1, "closed", prhAt(3), nil, nil, nil)
	})

	t.Run("watermark survives delayed closed", func(t *testing.T) {
		fx := newPRHFixture(t)
		a, b := fx.newRun(1, ""), fx.newRun(1, "")
		fx.mustObserve(a, "opened", prhAt(1), nil)
		fx.mustObserve(b, "opened", prhAt(3), nil)
		fx.mustObserve(a, "closed", prhAt(2), nil)
		fx.assertHead(1, "opened", prhAt(3), nil, nil, nil)
		fx.assertEvents(1, wantEvent{To: "opened", TimeSource: "observation_fallback", ObservedAt: prhAt(1), Bootstrap: true})
		fx.assertMRState(a, "closed")
	})

	t.Run("tied observed_at dropped", func(t *testing.T) {
		fx := newPRHFixture(t)
		a, b := fx.newRun(1, ""), fx.newRun(1, "")
		fx.mustObserve(a, "opened", prhAt(1), nil)
		fx.mustObserve(b, "closed", prhAt(1), nil)
		fx.assertHead(1, "opened", prhAt(1), nil, nil, nil)
		fx.assertEvents(1, wantEvent{To: "opened", TimeSource: "observation_fallback", ObservedAt: prhAt(1), Bootstrap: true})
		fx.assertMRState(b, "closed")
	})
}

func ptrOf(t time.Time) *time.Time { return &t }

func TestPRHistoryProviderTimeLiveDB(t *testing.T) {
	t.Run("future provider time falls back", func(t *testing.T) {
		fx := newPRHFixture(t)
		run := fx.newRun(1, "")
		future := prhAt(5).Add(time.Hour)
		fx.mustObserve(run, "merged", prhAt(5), &future)
		fx.assertEvents(1, wantEvent{To: "merged", TimeSource: "observation_fallback", ObservedAt: prhAt(5), Bootstrap: true, FirstMerge: true})
		fx.assertHead(1, "merged", prhAt(5), nil, ptrOf(prhAt(5)), prhS("observation_fallback"))
	})

	t.Run("range boundaries through the generated query", func(t *testing.T) {
		// The forgesvc conversion passes values in [1970-01-01, 10000-01-01) UTC. The epoch
		// is stored as a provider time; the last fractional second of year 9999 is accepted
		// by the statement and, being after the observation, falls back in SQL.
		fx := newPRHFixture(t)
		epoch := time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)
		last := time.Date(9999, 12, 31, 23, 59, 59, 500_000_000, time.UTC)
		runA := fx.newRun(1, "")
		fx.mustObserve(runA, "closed", prhAt(1), &epoch)
		fx.assertEvents(1, wantEvent{To: "closed", ProviderAt: &epoch, TimeSource: "provider", ObservedAt: prhAt(1), Bootstrap: true})
		fx.assertHead(1, "closed", prhAt(1), &epoch, nil, nil)
		runB := fx.newRun(2, "")
		fx.mustObserve(runB, "merged", prhAt(2), &last)
		fx.assertEvents(2, wantEvent{To: "merged", TimeSource: "observation_fallback", ObservedAt: prhAt(2), Bootstrap: true, FirstMerge: true})
		fx.assertHead(2, "merged", prhAt(2), nil, ptrOf(prhAt(2)), prhS("observation_fallback"))
	})

	t.Run("inverted provider time falls back", func(t *testing.T) {
		fx := newPRHFixture(t)
		run := fx.newRun(1, "")
		p2, p1 := prhAt(2), prhAt(1)
		fx.mustObserve(run, "closed", prhAt(3), &p2)
		fx.mustObserve(run, "merged", prhAt(4), &p1)
		fx.assertEvents(1,
			wantEvent{To: "closed", ProviderAt: &p2, TimeSource: "provider", ObservedAt: prhAt(3), Bootstrap: true},
			wantEvent{From: prhS("closed"), To: "merged", TimeSource: "observation_fallback", ObservedAt: prhAt(4), FirstMerge: true})
		// last_provider_at never decreases; first merge takes the observation fallback.
		fx.assertHead(1, "merged", prhAt(4), &p2, ptrOf(prhAt(4)), prhS("observation_fallback"))
	})

	t.Run("null head provider then real one is kept", func(t *testing.T) {
		fx := newPRHFixture(t)
		run := fx.newRun(1, "")
		p := prhAt(2)
		fx.mustObserve(run, "opened", prhAt(1), nil)
		fx.mustObserve(run, "merged", prhAt(3), &p)
		fx.assertEvents(1,
			wantEvent{To: "opened", TimeSource: "observation_fallback", ObservedAt: prhAt(1), Bootstrap: true},
			wantEvent{From: prhS("opened"), To: "merged", ProviderAt: &p, TimeSource: "provider", ObservedAt: prhAt(3), FirstMerge: true})
		fx.assertHead(1, "merged", prhAt(3), &p, &p, prhS("provider"))
		// A later NULL provider time must not wipe the kept one.
		fx.mustObserve(run, "closed", prhAt(4), nil)
		fx.assertHead(1, "closed", prhAt(4), &p, &p, prhS("provider"))
	})
}

func TestPRHistoryBootstrapLiveDB(t *testing.T) {
	t.Run("same-state fresh insert", func(t *testing.T) {
		fx := newPRHFixture(t)
		run := fx.newRun(1, "merged")
		prov := prhAt(1)
		fx.mustObserve(run, "merged", prhAt(2), &prov)
		fx.assertEvents(1, wantEvent{From: prhS("merged"), To: "merged", ProviderAt: &prov, TimeSource: "provider", ObservedAt: prhAt(2), Bootstrap: true, FirstMerge: true})
		fx.assertHead(1, "merged", prhAt(2), &prov, &prov, prhS("provider"))
	})

	t.Run("null prior state gives null from_state", func(t *testing.T) {
		fx := newPRHFixture(t)
		run := fx.newRun(1, "")
		fx.mustObserve(run, "closed", prhAt(1), nil)
		ev := fx.events(1)
		if len(ev) != 1 || ev[0].From != nil || !ev[0].Bootstrap {
			t.Fatalf("events = %+v, want one bootstrap event with NULL from_state", ev)
		}
	})

	t.Run("pre-tracking opened observed merged", func(t *testing.T) {
		fx := newPRHFixture(t)
		run := fx.newRun(1, "opened")
		fx.mustObserve(run, "merged", prhAt(2), nil)
		fx.assertEvents(1, wantEvent{From: prhS("opened"), To: "merged", TimeSource: "observation_fallback", ObservedAt: prhAt(2), Bootstrap: true, FirstMerge: true})
	})
}

func TestPRHistoryFirstMergeLiveDB(t *testing.T) {
	t.Run("opened confirmed then first merge", func(t *testing.T) {
		fx := newPRHFixture(t)
		run := fx.newRun(1, "")
		p := prhAt(2)
		fx.mustObserve(run, "opened", prhAt(1), nil)
		fx.mustObserve(run, "opened", prhAt(2), nil)
		fx.mustObserve(run, "merged", prhAt(3), &p)
		fx.assertEvents(1,
			wantEvent{To: "opened", TimeSource: "observation_fallback", ObservedAt: prhAt(1), Bootstrap: true},
			wantEvent{From: prhS("opened"), To: "merged", ProviderAt: &p, TimeSource: "provider", ObservedAt: prhAt(3), FirstMerge: true})
		fx.assertHead(1, "merged", prhAt(3), &p, &p, prhS("provider"))
	})

	t.Run("re-merge after reopen is not first merge", func(t *testing.T) {
		fx := newPRHFixture(t)
		run := fx.newRun(1, "")
		p1, p2 := prhAt(1), prhAt(4)
		fx.mustObserve(run, "merged", prhAt(2), &p1)
		fx.mustObserve(run, "opened", prhAt(3), nil)
		fx.mustObserve(run, "merged", prhAt(5), &p2)
		fx.assertEvents(1,
			wantEvent{To: "merged", ProviderAt: &p1, TimeSource: "provider", ObservedAt: prhAt(2), Bootstrap: true, FirstMerge: true},
			wantEvent{From: prhS("merged"), To: "opened", TimeSource: "observation_fallback", ObservedAt: prhAt(3)},
			wantEvent{From: prhS("opened"), To: "merged", ProviderAt: &p2, TimeSource: "provider", ObservedAt: prhAt(5)})
		fx.assertHead(1, "merged", prhAt(5), &p2, &p1, prhS("provider"))
	})

	t.Run("unexpected unique conflict fails atomically", func(t *testing.T) {
		fx := newPRHFixture(t)
		run := fx.newRun(1, "")
		fx.mustObserve(run, "opened", prhAt(1), nil)
		// Simulate an inconsistency: a stray first_merge event for a PR whose head never merged.
		fx.exec(`INSERT INTO pr_history_events (repo_id, mr_iid, from_state, to_state, provider_at, time_source, observed_at, bootstrap, first_merge)
			VALUES ($1, 1, 'opened', 'merged', NULL, 'observation_fallback', $2, false, true)`, fx.repoID, prhAt(0))
		n, err := fx.observe(run, "merged", prhAt(2), nil)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
			t.Fatalf("SetRunMRState = (%d, %v), want SQLSTATE 23505", n, err)
		}
		fx.assertMRState(run, "opened")
		fx.assertHead(1, "opened", prhAt(1), nil, nil, nil)
		fx.assertEvents(1,
			wantEvent{To: "opened", TimeSource: "observation_fallback", ObservedAt: prhAt(1), Bootstrap: true},
			wantEvent{From: prhS("opened"), To: "merged", TimeSource: "observation_fallback", ObservedAt: prhAt(0), FirstMerge: true})
	})

	t.Run("every merged head has exactly one first_merge event", func(t *testing.T) {
		fx := newPRHFixture(t)
		// PR 1: fresh merge. PR 2: opened then merged then reopened then re-merged.
		// PR 3: pre-tracking merged. PR 4: merged then a delayed older opened.
		r1 := fx.newRun(1, "")
		fx.mustObserve(r1, "merged", prhAt(1), nil)
		r2 := fx.newRun(2, "")
		fx.mustObserve(r2, "opened", prhAt(1), nil)
		fx.mustObserve(r2, "merged", prhAt(2), nil)
		fx.mustObserve(r2, "opened", prhAt(3), nil)
		fx.mustObserve(r2, "merged", prhAt(4), nil)
		r3 := fx.newRun(3, "merged")
		fx.mustObserve(r3, "merged", prhAt(1), nil)
		r4, r4b := fx.newRun(4, ""), fx.newRun(4, "")
		fx.mustObserve(r4, "merged", prhAt(2), nil)
		fx.mustObserve(r4b, "opened", prhAt(1), nil)
		var merged, bad int
		if err := fx.pool.QueryRow(fx.ctx, `SELECT count(*) FILTER (WHERE h.state = 'merged'),
				count(*) FILTER (WHERE (SELECT count(*) FROM pr_history_events e WHERE e.repo_id = h.repo_id AND e.mr_iid = h.mr_iid AND e.first_merge) <> 1)
			FROM pr_history_heads h WHERE h.repo_id = $1`, fx.repoID).Scan(&merged, &bad); err != nil {
			t.Fatal(err)
		}
		if merged != 4 || bad != 0 {
			t.Fatalf("merged heads=%d (want 4), heads without exactly one first_merge event=%d (want 0)", merged, bad)
		}
	})
}

// prhBeginObserve runs SetRunMRState inside tx via store.New(tx).
func prhBeginObserve(ctx context.Context, tx pgx.Tx, runID uuid.UUID, state string, observed time.Time) (int64, error) {
	return prhObserve(ctx, store.New(tx), runID, state, observed, nil)
}

func TestPRHistoryAtomicityLiveDB(t *testing.T) {
	t.Run("uncommitted change invisible to another session", func(t *testing.T) {
		fx := newPRHFixture(t)
		run := fx.newRun(1, "opened")
		tx, err := fx.pool.Begin(fx.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(fx.ctx) }()
		n, err := prhBeginObserve(fx.ctx, tx, run, "closed", prhAt(1))
		if err != nil || n != 1 {
			t.Fatalf("observe in tx = (%d, %v)", n, err)
		}
		// A second session (the pool) still sees the prior state.
		fx.assertMRState(run, "opened")
		fx.assertNoHistory(1)
		if err := tx.Commit(fx.ctx); err != nil {
			t.Fatal(err)
		}
		fx.assertMRState(run, "closed")
		fx.assertHead(1, "closed", prhAt(1), nil, nil, nil)
		fx.assertEvents(1, wantEvent{From: prhS("opened"), To: "closed", TimeSource: "observation_fallback", ObservedAt: prhAt(1), Bootstrap: true})
	})

	t.Run("forced event-insert failure rolls back everything", func(t *testing.T) {
		fx := newPRHFixture(t)
		run := fx.newRun(1, "")
		fx.mustObserve(run, "opened", prhAt(1), nil)
		const fn, trg = "pr_history_test_force_fail", "pr_history_test_force_fail_trg"
		fx.exec(fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN
				IF NEW.repo_id = '%s'::uuid THEN
					RAISE EXCEPTION 'pr_history_test_forced_failure' USING ERRCODE = 'P0001';
				END IF;
				RETURN NEW;
			END $$`, fn, fx.repoID))
		fx.exec(fmt.Sprintf(`CREATE TRIGGER %s BEFORE INSERT ON pr_history_events FOR EACH ROW EXECUTE FUNCTION %s()`, trg, fn))
		t.Cleanup(func() {
			cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			// The fixture's own cleanup (registered earlier) closes the pool last; drop first.
			p2, err := store.OpenPool(cctx, os.Getenv("UZI_TEST_DATABASE_URL"))
			if err != nil {
				t.Errorf("cleanup pool: %v", err)
				return
			}
			defer p2.Close()
			for _, s := range []string{"DROP TRIGGER IF EXISTS " + trg + " ON pr_history_events", "DROP FUNCTION IF EXISTS " + fn + "()"} {
				if _, err := p2.Exec(cctx, s); err != nil {
					t.Errorf("cleanup %q: %v", s, err)
				}
			}
		})

		// The writer's connection stays checked out while the checks run, so the pool
		// answers them from a different backend: a guaranteed second session.
		connW, err := fx.pool.Acquire(fx.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer connW.Release() // also on every early t.Fatal; a second Release is a no-op
		writerPID := prhPID(fx.ctx, t, connW)
		n, err := prhObserve(fx.ctx, store.New(connW.Conn()), run, "closed", prhAt(2), nil)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "P0001" || pgErr.Message != "pr_history_test_forced_failure" {
			t.Fatalf("SetRunMRState = (%d, %v), want P0001 pr_history_test_forced_failure", n, err)
		}
		if checkerPID := prhPID(fx.ctx, t, fx.pool); checkerPID == writerPID {
			t.Fatalf("checker session pid %d equals writer pid", checkerPID)
		}
		func() {
			defer connW.Release()
			fx.assertMRState(run, "opened")
			fx.assertHead(1, "opened", prhAt(1), nil, nil, nil)
			fx.assertEvents(1, wantEvent{To: "opened", TimeSource: "observation_fallback", ObservedAt: prhAt(1), Bootstrap: true})
		}()

		// Control: with the trigger disabled the same call succeeds and writes the event.
		fx.exec(fmt.Sprintf(`ALTER TABLE pr_history_events DISABLE TRIGGER %s`, trg))
		fx.mustObserve(run, "closed", prhAt(2), nil)
		fx.assertMRState(run, "closed")
		fx.assertHead(1, "closed", prhAt(2), nil, nil, nil)
		fx.assertEvents(1,
			wantEvent{To: "opened", TimeSource: "observation_fallback", ObservedAt: prhAt(1), Bootstrap: true},
			wantEvent{From: prhS("opened"), To: "closed", TimeSource: "observation_fallback", ObservedAt: prhAt(2)})
	})
}

func TestPRHistoryOtherLiveDB(t *testing.T) {
	t.Run("non-owner run updates mr_state only", func(t *testing.T) {
		fx := newPRHFixture(t)
		other := uuid.New()
		fx.exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, other, fmt.Sprintf("prh-other-%s@e2e", other))
		t.Cleanup(func() { _, _ = fx.pool.Exec(context.Background(), `DELETE FROM users WHERE id=$1`, other) })
		run := fx.newRunFor(other, 1, prhS("opened"))
		fx.mustObserve(run, "merged", prhAt(1), nil)
		fx.assertMRState(run, "merged")
		fx.assertNoHistory(1)
	})

	t.Run("unknown state writes nothing", func(t *testing.T) {
		fx := newPRHFixture(t)
		run := fx.newRun(1, "opened")
		n, err := fx.observe(run, "weird", prhAt(1), nil)
		if err != nil || n != 0 {
			t.Fatalf("SetRunMRState(weird) = (%d, %v), want (0, nil)", n, err)
		}
		fx.assertMRState(run, "opened")
		fx.assertNoHistory(1)
	})

	t.Run("repo delete cascades heads and events", func(t *testing.T) {
		fx := newPRHFixture(t)
		run := fx.newRun(1, "")
		fx.mustObserve(run, "opened", prhAt(1), nil)
		fx.mustObserve(run, "merged", prhAt(2), nil)
		fx.assertHead(1, "merged", prhAt(2), nil, ptrOf(prhAt(2)), prhS("observation_fallback"))
		fx.exec(`DELETE FROM repos WHERE id=$1`, fx.repoID)
		var heads, events int
		if err := fx.pool.QueryRow(fx.ctx, `SELECT (SELECT count(*) FROM pr_history_heads WHERE repo_id=$1),
			(SELECT count(*) FROM pr_history_events WHERE repo_id=$1)`, fx.repoID).Scan(&heads, &events); err != nil {
			t.Fatal(err)
		}
		if heads != 0 || events != 0 {
			t.Fatalf("after repo delete: heads=%d events=%d, want 0/0", heads, events)
		}
	})
}

// ---- delete-order / deadlock tests ----

func prhPID(ctx context.Context, t *testing.T, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) int32 {
	t.Helper()
	var pid int32
	if err := q.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatalf("pg_backend_pid: %v", err)
	}
	return pid
}

// prhWaitBlocked polls (bounded at 10s) until blocked is waiting on blocker.
func prhWaitBlocked(ctx context.Context, t *testing.T, pool *pgxpool.Pool, blocked, blocker int32) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var ok bool
		if err := pool.QueryRow(ctx, `SELECT pg_blocking_pids($1::int) @> ARRAY[$2::int]`, blocked, blocker).Scan(&ok); err != nil {
			t.Fatalf("pg_blocking_pids: %v", err)
		}
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pid %d never blocked on pid %d within 10s", blocked, blocker)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// prhRes is a goroutine's statement result.
type prhRes struct {
	n   int64
	err error
}

// prhRollback rolls tx back on its own bounded context, so a cancelled test context
// never leaves a connection checked out (pool.Close in cleanup would then block).
func prhRollback(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}

// prhJoin returns a deferred func that cancels the session context and waits up to 15s
// for the goroutine to finish, so a test that fails early does not release or roll back
// a connection the goroutine is still using. After that bound it gives up and the test
// may race its own teardown; the 90s session contexts make that practically unreachable.
func prhJoin(cancel context.CancelFunc, finished <-chan struct{}) func() {
	return func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(15 * time.Second):
		}
	}
}

// prhRecv waits (bounded) for a goroutine result.
func prhRecv(t *testing.T, ch <-chan prhRes, what string) prhRes {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(30 * time.Second):
		t.Fatalf("%s did not finish within 30s", what)
		return prhRes{}
	}
}

func prhSQLState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func (fx *prhFx) count(sql string, args ...any) int {
	fx.t.Helper()
	var n int
	if err := fx.pool.QueryRow(fx.ctx, sql, args...).Scan(&n); err != nil {
		fx.t.Fatal(err)
	}
	return n
}

func TestPRHistoryPrelockedDeleteOrderLiveDB(t *testing.T) {
	type variant struct {
		name   string
		del    func(ctx context.Context, q *store.Queries, fx *prhFx) (int64, error)
		gone   func(fx *prhFx) int // rows of the deleted root object remaining
		extraC func(ctx context.Context, fx *prhFx, tx pgx.Tx) error
	}
	variants := []variant{
		{
			name: "DeleteRepoForUser",
			del: func(ctx context.Context, q *store.Queries, fx *prhFx) (int64, error) {
				return q.DeleteRepoForUser(ctx, store.DeleteRepoForUserParams{ID: fx.repoID, UserID: fx.userID})
			},
			gone: func(fx *prhFx) int { return fx.count(`SELECT count(*) FROM repos WHERE id=$1`, fx.repoID) },
		},
		{
			name: "DeleteForgeConnectionForUser",
			del: func(ctx context.Context, q *store.Queries, fx *prhFx) (int64, error) {
				return q.DeleteForgeConnectionForUser(ctx, store.DeleteForgeConnectionForUserParams{ID: fx.connID, UserID: fx.userID})
			},
			gone: func(fx *prhFx) int { return fx.count(`SELECT count(*) FROM forge_connections WHERE id=$1`, fx.connID) },
			extraC: func(ctx context.Context, fx *prhFx, tx pgx.Tx) error {
				var one int
				return tx.QueryRow(ctx, `SELECT 1 FROM forge_connections WHERE id=$1 FOR KEY SHARE NOWAIT`, fx.connID).Scan(&one)
			},
		},
	}
	for _, v := range variants {
		t.Run(v.name+"/observation holds run lock first", func(t *testing.T) {
			fx := newPRHFixture(t)
			ctx, cancel := context.WithTimeout(fx.ctx, 90*time.Second)
			defer cancel()
			run := fx.newRun(1, "opened")
			txA, err := fx.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer prhRollback(txA)
			pidA := prhPID(ctx, t, txA)
			var one int
			if err := txA.QueryRow(ctx, `SELECT 1 FROM runs WHERE id=$1 FOR NO KEY UPDATE`, run).Scan(&one); err != nil {
				t.Fatal(err)
			}

			connB, err := fx.pool.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer connB.Release()
			pidB := prhPID(ctx, t, connB)
			done := make(chan prhRes, 1)
			finished := make(chan struct{})
			go func() {
				defer close(finished)
				n, err := v.del(ctx, store.New(connB.Conn()), fx)
				done <- prhRes{n, err}
			}()
			defer prhJoin(cancel, finished)()
			prhWaitBlocked(ctx, t, fx.pool, pidB, pidA)

			// Session C: B waiting on the run lock must not already hold the repo (or connection) row.
			txC, err := fx.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer prhRollback(txC)
			if err := txC.QueryRow(ctx, `SELECT 1 FROM repos WHERE id=$1 FOR KEY SHARE NOWAIT`, fx.repoID).Scan(&one); err != nil {
				t.Fatalf("repos FOR KEY SHARE NOWAIT while B waits: %v", err)
			}
			if v.extraC != nil {
				if err := v.extraC(ctx, fx, txC); err != nil {
					t.Fatalf("forge_connections FOR KEY SHARE NOWAIT while B waits: %v", err)
				}
			}
			if err := txC.Rollback(ctx); err != nil {
				t.Fatal(err)
			}

			n, err := prhBeginObserve(ctx, txA, run, "merged", prhAt(1))
			if err != nil || n != 1 {
				t.Fatalf("A observe = (%d, %v), want (1, nil); sqlstate %q", n, err, prhSQLState(err))
			}
			if err := txA.Commit(ctx); err != nil {
				t.Fatalf("A commit: %v", err)
			}
			select {
			case r := <-done:
				if r.err != nil || r.n != 1 {
					t.Fatalf("B delete = (%d, %v), want (1, nil); sqlstate %q", r.n, r.err, prhSQLState(r.err))
				}
			case <-time.After(15 * time.Second):
				t.Fatal("B delete did not finish after A committed")
			}
			if v.gone(fx) != 0 {
				t.Fatal("deleted root object still present")
			}
			if h := fx.count(`SELECT count(*) FROM pr_history_heads WHERE repo_id=$1`, fx.repoID); h != 0 {
				t.Fatalf("heads after cascade = %d, want 0", h)
			}
			if e := fx.count(`SELECT count(*) FROM pr_history_events WHERE repo_id=$1`, fx.repoID); e != 0 {
				t.Fatalf("events after cascade = %d, want 0", e)
			}
		})

		t.Run(v.name+"/delete commits first then observation returns 0", func(t *testing.T) {
			fx := newPRHFixture(t)
			ctx, cancel := context.WithTimeout(fx.ctx, 90*time.Second)
			defer cancel()
			run := fx.newRun(1, "opened")
			txB, err := fx.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer prhRollback(txB)
			n, err := v.del(ctx, store.New(txB), fx)
			if err != nil || n != 1 {
				t.Fatalf("B delete in tx = (%d, %v)", n, err)
			}
			connA, err := fx.pool.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer connA.Release()
			pidA := prhPID(ctx, t, connA)
			pidB := prhPID(ctx, t, txB)
			done := make(chan prhRes, 1)
			finished := make(chan struct{})
			go func() {
				defer close(finished)
				n, err := prhObserve(ctx, store.New(connA.Conn()), run, "merged", prhAt(1), nil)
				done <- prhRes{n, err}
			}()
			defer prhJoin(cancel, finished)()
			prhWaitBlocked(ctx, t, fx.pool, pidA, pidB)
			if err := txB.Commit(ctx); err != nil {
				t.Fatalf("B commit: %v", err)
			}
			select {
			case r := <-done:
				if r.err != nil || r.n != 0 {
					t.Fatalf("observation after delete = (%d, %v), want (0, nil); sqlstate %q", r.n, r.err, prhSQLState(r.err))
				}
			case <-time.After(15 * time.Second):
				t.Fatal("observation did not finish after B committed")
			}
			if c := fx.count(`SELECT count(*) FROM runs WHERE id=$1`, run); c != 0 {
				t.Fatalf("run still present: %d", c)
			}
			if h := fx.count(`SELECT count(*) FROM pr_history_heads WHERE repo_id=$1`, fx.repoID); h != 0 {
				t.Fatalf("heads = %d, want 0", h)
			}
			if e := fx.count(`SELECT count(*) FROM pr_history_events WHERE repo_id=$1`, fx.repoID); e != 0 {
				t.Fatalf("events = %d, want 0", e)
			}
		})
	}
}

// TestPRHistoryCrossCheckChildDeleteDeadlockLiveDB races a watcher observation against a repo
// delete and checks both victim choices. Each case sets explicit per-transaction
// deadlock_timeout values with SET LOCAL, so the victim-selection delay is short and independent
// of the server's default deadlock_timeout. (Fixture setup and several post-race assertions run
// outside the race's 90s context and are not covered by that bound.)
//
// Privilege: deadlock_timeout is a superuser-only (PGC_SUSET) parameter, so SET LOCAL needs a
// superuser connection or the SET privilege on it (PostgreSQL 15+, including 17:
// GRANT SET ON PARAMETER deadlock_timeout TO <role>). Without it the test fails at the SET, not
// at the lock race. Developers without that privilege can run ./e2e/run-store-it.sh, whose
// throwaway database connects as the image's superuser role.
//
// The test also assumes the server does not set lock_timeout, statement_timeout,
// idle_in_transaction_session_timeout or transaction_timeout below a few seconds.
func TestPRHistoryCrossCheckChildDeleteDeadlockLiveDB(t *testing.T) {
	// setup builds: lead issue run (non-cross_check), a completed cross_check child with mr_iid set and
	// the connection owner as user, a cross_checks row linking them, and a repo with enabled=false.
	setup := func(t *testing.T) (*planCrossCheckDeletionFixture, *prhFx) {
		t.Helper()
		ctx := context.Background()
		fx := setupPlanCrossCheckDeletion(ctx, t)
		f := fx.f
		mustExec(ctx, t, f.pool, `UPDATE repos SET enabled=false WHERE id=$1`, f.repoID)
		mustExec(ctx, t, f.pool, `UPDATE runs SET status='completed', mr_iid=77 WHERE id=$1`, fx.checkerID)
		p := &prhFx{t: t, ctx: ctx, pool: f.pool, q: f.q, userID: f.userID, repoID: f.repoID}
		// Sanity: the fixture really is the shape the scenario needs.
		var kind string
		var issue *int32
		var uid uuid.UUID
		if err := f.pool.QueryRow(ctx, `SELECT kind, issue_iid, user_id FROM runs WHERE id=$1`, fx.checkerID).Scan(&kind, &issue, &uid); err != nil {
			t.Fatal(err)
		}
		if kind != "cross_check" || issue != nil || uid != f.userID {
			t.Fatalf("child shape kind=%s issue=%v user=%v", kind, issue, uid)
		}
		return fx, p
	}

	observeChild := func(ctx context.Context, tx pgx.Tx, fx *planCrossCheckDeletionFixture) (int64, error) {
		return prhObserve(ctx, store.New(tx), fx.checkerID, "merged", prhAt(1), nil)
	}

	// begin starts a tx with the given deadlock_timeout.
	begin := func(ctx context.Context, t *testing.T, pool *pgxpool.Pool, timeout string) pgx.Tx {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if timeout != "" {
			if _, err := tx.Exec(ctx, "SET LOCAL deadlock_timeout = '"+timeout+"'"); err != nil {
				_ = tx.Rollback(ctx)
				t.Fatalf("SET LOCAL deadlock_timeout=%s refused: %v", timeout, err)
			}
		}
		return tx
	}

	t.Run("watcher loses", func(t *testing.T) {
		fx, p := setup(t)
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		f := fx.f
		// Both actors use the same short transaction-local deadlock_timeout, so the wait below is bounded
		// whatever the server's deadlock_timeout is.
		const dt = time.Second
		const dtSetting = "1s"

		txA := begin(ctx, t, f.pool, dtSetting)
		defer prhRollback(txA)
		pidA := prhPID(ctx, t, txA)
		var one int
		if err := txA.QueryRow(ctx, `SELECT 1 FROM runs WHERE id=$1 FOR NO KEY UPDATE`, fx.checkerID).Scan(&one); err != nil {
			t.Fatal(err)
		}

		txB := begin(ctx, t, f.pool, dtSetting)
		defer prhRollback(txB)
		pidB := prhPID(ctx, t, txB)
		doneB := make(chan prhRes, 1)
		finished := make(chan struct{})
		go func() {
			defer close(finished)
			n, err := store.New(txB).DeleteRepoForUser(ctx, store.DeleteRepoForUserParams{ID: f.repoID, UserID: f.userID})
			if err == nil {
				err = txB.Commit(ctx)
			}
			doneB <- prhRes{n, err}
		}()
		defer prhJoin(cancel, finished)()
		prhWaitBlocked(ctx, t, f.pool, pidB, pidA)
		// The sleep must exceed B's deadlock_timeout. PostgreSQL runs a waiter's deadlock check once,
		// when that waiter's own deadlock_timeout expires, so B's single check has passed (no cycle)
		// before A starts waiting. A's check then finds the cycle, and A is the victim.
		time.Sleep(2*dt + 500*time.Millisecond)

		nA, errA := observeChild(ctx, txA, fx)
		if errA == nil {
			errA = txA.Commit(ctx)
		}
		rB := prhRecv(t, doneB, "B (delete)")
		if got := prhSQLState(errA); got != "40P01" {
			t.Fatalf("A (watcher) = (%d, %v) sqlstate %q, want 40P01", nA, errA, got)
		}
		if rB.err != nil || rB.n != 1 {
			t.Fatalf("B (delete) = (%d, %v) sqlstate %q, want committed with 1 row", rB.n, rB.err, prhSQLState(rB.err))
		}
		prhRollback(txA)
		if c := p.count(`SELECT count(*) FROM repos WHERE id=$1`, f.repoID); c != 0 {
			t.Fatalf("repo remains: %d", c)
		}
		if c := p.count(`SELECT count(*) FROM runs WHERE repo_id=$1`, f.repoID); c != 0 {
			t.Fatalf("runs remain: %d", c)
		}
		if c := p.count(`SELECT count(*) FROM pr_history_heads WHERE repo_id=$1`, f.repoID); c != 0 {
			t.Fatalf("heads remain: %d", c)
		}
		if c := p.count(`SELECT count(*) FROM pr_history_events WHERE repo_id=$1`, f.repoID); c != 0 {
			t.Fatalf("events remain: %d", c)
		}
	})

	t.Run("delete loses", func(t *testing.T) {
		fx, p := setup(t)
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		f := fx.f

		txA := begin(ctx, t, f.pool, "60s")
		defer prhRollback(txA)
		pidA := prhPID(ctx, t, txA)
		var one int
		if err := txA.QueryRow(ctx, `SELECT 1 FROM runs WHERE id=$1 FOR NO KEY UPDATE`, fx.checkerID).Scan(&one); err != nil {
			t.Fatal(err)
		}
		txB := begin(ctx, t, f.pool, "3s")
		defer prhRollback(txB)
		pidB := prhPID(ctx, t, txB)
		doneB := make(chan prhRes, 1)
		finished := make(chan struct{})
		go func() {
			defer close(finished)
			n, err := store.New(txB).DeleteRepoForUser(ctx, store.DeleteRepoForUserParams{ID: f.repoID, UserID: f.userID})
			if err == nil {
				err = txB.Commit(ctx)
			}
			doneB <- prhRes{n, err}
		}()
		defer prhJoin(cancel, finished)()
		prhWaitBlocked(ctx, t, f.pool, pidB, pidA)

		nA, errA := observeChild(ctx, txA, fx)
		if errA != nil || nA != 1 {
			t.Fatalf("A (watcher) = (%d, %v) sqlstate %q, want success", nA, errA, prhSQLState(errA))
		}
		if err := txA.Commit(ctx); err != nil {
			t.Fatalf("A commit: %v", err)
		}
		rB := prhRecv(t, doneB, "B (delete)")
		if got := prhSQLState(rB.err); got != "40P01" {
			t.Fatalf("B (delete) = (%d, %v) sqlstate %q, want 40P01", rB.n, rB.err, got)
		}
		prhRollback(txB)
		if c := p.count(`SELECT count(*) FROM repos WHERE id=$1`, f.repoID); c != 1 {
			t.Fatalf("repo count = %d, want 1 (delete lost)", c)
		}
		var st string
		if err := f.pool.QueryRow(ctx, `SELECT mr_state FROM runs WHERE id=$1`, fx.checkerID).Scan(&st); err != nil || st != "merged" {
			t.Fatalf("child mr_state = %q, %v; want merged", st, err)
		}
		if c := p.count(`SELECT count(*) FROM pr_history_heads WHERE repo_id=$1 AND mr_iid=77 AND state='merged'`, f.repoID); c != 1 {
			t.Fatalf("heads = %d, want 1", c)
		}
		if c := p.count(`SELECT count(*) FROM pr_history_events WHERE repo_id=$1 AND mr_iid=77 AND to_state='merged' AND first_merge AND bootstrap`, f.repoID); c != 1 {
			t.Fatalf("events = %d, want 1", c)
		}
	})
}
