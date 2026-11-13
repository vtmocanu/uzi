package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4007 slice A, M1: migrations 00432-00435. Every test stands its own database, so the
// CHECKs, the guard triggers and the widened reason CHECK are executed by real statements. A
// guard-refused change is a silent zero-row write (RETURN NULL), a failed CHECK or FK is an error
// naming its constraint, and the tests tell the two apart.

const (
	termTipA   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	termTipB   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	termTipC   = "cccccccccccccccccccccccccccccccccccccccc"
	termDigest = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
)

var termStates = []string{"bound", "publishing", "live", "retiring", "deleted", "abandoned", "not_published"}

// termAllowed is the preservation transition matrix the trigger must enforce. publishing ->
// not_published is conditional: it holds only while no tip, no first_published_at and no
// invoked, publish_unknown or published contribution exists (the matrix fixture for publishing has
// none of them; TestTerminalPreservationNotPublishedLiveDB covers the refusals).
var termAllowed = map[string][]string{
	"bound":         {"publishing", "not_published"},
	"not_published": {"publishing"},
	"publishing":    {"live", "not_published"},
	"live":          {"publishing", "retiring"},
	"retiring":      {"deleted", "abandoned"},
}

type termFixture struct {
	heldFixture
	ctx context.Context
	q   *store.Queries
	t   *testing.T
}

func newTermFixture(t *testing.T, prefix string) *termFixture {
	t.Helper()
	ctx, _, pool := heldMigratedPool(t, prefix)
	h := heldNewHoldFixture(ctx, t, pool, false, "failed", nil)
	return &termFixture{heldFixture: h, ctx: ctx, q: store.New(pool), t: t}
}

// newRun inserts another run of the fixture's owner and repo.
func (f *termFixture) newRun() uuid.UUID {
	f.t.Helper()
	run := uuid.New()
	mustExec(f.ctx, f.t, f.pool, `INSERT INTO runs
		(id,user_id,repo_id,worker_id,kind,issue_iid,issue_title,issue_description,status,claim_generation)
		VALUES($1,$2,$3,$4,'issue',1,'term','body','failed',1)`, run, f.user, f.repo, f.worker)
	return run
}

func (f *termFixture) insertPreservation(run uuid.UUID) store.RunTerminalPreservation {
	f.t.Helper()
	row, err := f.q.InsertTerminalPreservation(f.ctx, store.InsertTerminalPreservationParams{
		RunID: run, UserID: f.user, RepoID: f.repo, Ref: "refs/uzi-terminal/" + run.String(), LifetimeAtBind: "no_pr",
	})
	if err != nil {
		f.t.Fatalf("insert preservation: %v", err)
	}
	return row
}

func (f *termFixture) contributionParams(p store.RunTerminalPreservation, generation int64, tip string, prior *string) store.InsertTerminalContributionParams {
	params := store.InsertTerminalContributionParams{
		PreservationID: p.ID, RunID: p.RunID, Generation: generation, WorkerID: f.worker, UserID: f.user,
		Authority: "current_flight", Tip: tip, CoverageDigest: termDigest, Manifest: []byte(`{"roots":[]}`),
		Disposition: "preserved",
	}
	if prior != nil {
		params.PriorTip = pgtype.Text{String: *prior, Valid: true}
	}
	return params
}

func (f *termFixture) insertContribution(p store.RunTerminalPreservation, generation int64, tip string, prior *string) store.RunTerminalContribution {
	f.t.Helper()
	row, err := f.q.InsertTerminalContribution(f.ctx, f.contributionParams(p, generation, tip, prior))
	if err != nil {
		f.t.Fatalf("insert contribution: %v", err)
	}
	return row
}

// rows runs a write and reports the rows it affected; a CHECK or FK failure is returned.
func (f *termFixture) rows(sql string, args ...any) (int64, error) {
	f.t.Helper()
	tag, err := f.pool.Exec(f.ctx, sql, args...)
	return tag.RowsAffected(), err
}

func (f *termFixture) mustRows(want int64, sql string, args ...any) {
	f.t.Helper()
	n, err := f.rows(sql, args...)
	if err != nil || n != want {
		f.t.Fatalf("%s\n  rows=%d err=%v, want rows=%d", sql, n, err, want)
	}
}

// publish walks the contribution prepared -> invoked -> published.
func (f *termFixture) publish(c store.RunTerminalContribution) {
	f.t.Helper()
	f.mustRows(1, `UPDATE run_terminal_contributions SET state='invoked', create_invoked_at=now() WHERE id=$1`, c.ID)
	f.mustRows(1, `UPDATE run_terminal_contributions SET state='published', published_at=now() WHERE id=$1`, c.ID)
}

// toLive drives a fresh preservation to live at termTipA through a published contribution and
// returns the preservation and that contribution.
func (f *termFixture) toLive(run uuid.UUID) (store.RunTerminalPreservation, store.RunTerminalContribution) {
	f.t.Helper()
	p := f.insertPreservation(run)
	c := f.insertContribution(p, 1, termTipA, nil)
	f.mustRows(1, `UPDATE run_terminal_preservations SET state='publishing', live_run_id=run_id WHERE id=$1`, p.ID)
	f.publish(c)
	f.mustRows(1, `UPDATE run_terminal_preservations SET state='live', tip=$2, first_published_at=now() WHERE id=$1`, p.ID, termTipA)
	return p, c
}

const termRetireStamp = `retire_tip=$2, retire_reason='issue_closed', retire_observed_at=now(), retire_decided_at=now()`

// toState drives a fresh preservation to the given state by legal moves.
func (f *termFixture) toState(state string) store.RunTerminalPreservation {
	f.t.Helper()
	run := f.newRun()
	var p store.RunTerminalPreservation
	switch state {
	case "bound":
		p = f.insertPreservation(run)
	case "not_published":
		p = f.insertPreservation(run)
		f.mustRows(1, `UPDATE run_terminal_preservations SET state='not_published' WHERE id=$1`, p.ID)
	case "publishing":
		p = f.insertPreservation(run)
		f.mustRows(1, `UPDATE run_terminal_preservations SET state='publishing', live_run_id=run_id WHERE id=$1`, p.ID)
	default:
		p, _ = f.toLive(run)
		if state == "live" {
			break
		}
		f.mustRows(1, `UPDATE run_terminal_preservations SET state='retiring', `+termRetireStamp+` WHERE id=$1`, p.ID, termTipA)
		switch state {
		case "deleted":
			f.mustRows(1, `UPDATE run_terminal_preservations SET state='deleted', live_run_id=NULL, deleted_at=now() WHERE id=$1`, p.ID)
		case "abandoned":
			f.mustRows(1, `UPDATE run_terminal_preservations SET state='abandoned', live_run_id=NULL WHERE id=$1`, p.ID)
		}
	}
	return p
}

// transition attempts a move into `to` with values consistent with every CHECK, so a refusal can
// only be the guard's.
func (f *termFixture) transition(id uuid.UUID, from, to string) (int64, error) {
	set := "state=$2"
	switch to {
	case "publishing":
		set += ", live_run_id=run_id"
	case "live":
		// The live CHECK requires a tip; publishing -> live has a published contribution at it.
		set += ", live_run_id=run_id, tip='" + termTipA + "'"
	case "retiring":
		set += ", live_run_id=run_id, retire_tip='" + termTipA + "', retire_reason='issue_closed', retire_observed_at=now(), retire_decided_at=now()"
	case "deleted":
		set += ", live_run_id=NULL, deleted_at=now(), retire_tip='" + termTipA + "', retire_reason='issue_closed', retire_observed_at=now(), retire_decided_at=now()"
	default:
		set += ", live_run_id=NULL"
	}
	if from == "retiring" && to == "deleted" {
		// Keep the recorded decision byte-identical; only the new state and stamp change.
		set = "state=$2, live_run_id=NULL, deleted_at=now()"
	}
	if from == "retiring" && to == "abandoned" {
		set = "state=$2, live_run_id=NULL"
	}
	return f.rows(`UPDATE run_terminal_preservations SET `+set+` WHERE id=$1`, id, to)
}

func termAllowedMove(from, to string) bool {
	for _, a := range termAllowed[from] {
		if a == to {
			return true
		}
	}
	return false
}

func TestTerminalPreservationMatrixLiveDB(t *testing.T) {
	f := newTermFixture(t, "term_matrix_")
	for _, from := range termStates {
		for _, to := range termStates {
			if from == to {
				continue
			}
			t.Run(from+"_to_"+to, func(t *testing.T) {
				ft := *f
				ft.t = t
				p := ft.toState(from)
				if from == "publishing" && to == "live" {
					// The tip of a live row is the tip of a published contribution.
					ft.publish(ft.insertContribution(p, 1, termTipA, nil))
				}
				n, err := ft.transition(p.ID, from, to)
				if err != nil {
					t.Fatalf("%s -> %s: %v", from, to, err)
				}
				want := int64(0)
				if termAllowedMove(from, to) {
					want = 1
				}
				if n != want {
					t.Fatalf("%s -> %s changed %d rows, want %d", from, to, n, want)
				}
			})
		}
	}
}

func TestTerminalPreservationChecksLiveDB(t *testing.T) {
	f := newTermFixture(t, "term_checks_")
	run := f.newRun()

	_, err := f.rows(`INSERT INTO run_terminal_preservations (run_id,user_id,repo_id,ref,lifetime_at_bind)
		VALUES($1,$2,$3,'refs/uzi-terminal/other','no_pr')`, run, f.user, f.repo)
	heldCheckName(t, err, "run_terminal_preservations_ref_check")
	_, err = f.rows(`INSERT INTO run_terminal_preservations (run_id,user_id,repo_id,ref,lifetime_at_bind)
		VALUES($1,$2,$3,'refs/uzi-terminal/'||$1::uuid::text,'bogus')`, run, f.user, f.repo)
	heldCheckName(t, err, "run_terminal_preservations_lifetime_check")

	p := f.insertPreservation(run)
	_, err = f.rows(`UPDATE run_terminal_preservations SET state='publishing' WHERE id=$1`, p.ID)
	heldCheckName(t, err, "run_terminal_preservations_live_pointer_check")
	_, err = f.rows(`UPDATE run_terminal_preservations SET state='not_published', live_run_id=run_id WHERE id=$1`, p.ID)
	heldCheckName(t, err, "run_terminal_preservations_live_pointer_check")
	_, err = f.rows(`UPDATE run_terminal_preservations SET state='publishing', live_run_id=$2 WHERE id=$1`, p.ID, f.run)
	heldCheckName(t, err, "run_terminal_preservations_live_run_is_own_check")
	_, err = f.rows(`UPDATE run_terminal_preservations SET last_error=repeat('x',513) WHERE id=$1`, p.ID)
	heldCheckName(t, err, "run_terminal_preservations_last_error_check")

	live, _ := f.toLive(f.newRun())
	_, err = f.rows(`UPDATE run_terminal_preservations SET state='retiring' WHERE id=$1`, live.ID)
	heldCheckName(t, err, "run_terminal_preservations_retire_stamp_check")
	_, err = f.rows(`UPDATE run_terminal_preservations SET state='retiring', retire_tip=$2, retire_reason='junk',
		retire_observed_at=now(), retire_decided_at=now() WHERE id=$1`, live.ID, termTipA)
	heldCheckName(t, err, "run_terminal_preservations_retire_reason_check")
	_, err = f.rows(`UPDATE run_terminal_preservations SET state='retiring', retire_tip='nothex',
		retire_reason='issue_closed', retire_observed_at=now(), retire_decided_at=now() WHERE id=$1`, live.ID)
	heldCheckName(t, err, "run_terminal_preservations_retire_tip_check")
	f.mustRows(1, `UPDATE run_terminal_preservations SET state='retiring', `+termRetireStamp+` WHERE id=$1`, live.ID, termTipA)
	_, err = f.rows(`UPDATE run_terminal_preservations SET state='deleted', live_run_id=NULL WHERE id=$1`, live.ID)
	heldCheckName(t, err, "run_terminal_preservations_deleted_at_check")

	// A live row has read the ref back at a tip.
	pub := f.insertPreservation(f.newRun())
	f.mustRows(1, `UPDATE run_terminal_preservations SET state='publishing', live_run_id=run_id WHERE id=$1`, pub.ID)
	_, err = f.rows(`UPDATE run_terminal_preservations SET state='live' WHERE id=$1`, pub.ID)
	heldCheckName(t, err, "run_terminal_preservations_live_tip_check")
}

// publishing -> not_published would clear the live pointer while a remote ref may exist: refused
// once the row recorded a tip, and while any contribution of it may have created the ref.
func TestTerminalPreservationNotPublishedLiveDB(t *testing.T) {
	f := newTermFixture(t, "term_notpub_")

	t.Run("live then publishing then not_published is refused", func(t *testing.T) {
		ft := *f
		ft.t = t
		p, _ := ft.toLive(ft.newRun())
		ft.mustRows(1, `UPDATE run_terminal_preservations SET state='publishing' WHERE id=$1`, p.ID)
		ft.mustRows(0, `UPDATE run_terminal_preservations SET state='not_published', live_run_id=NULL WHERE id=$1`, p.ID)
		// The row is untouched: the pointer and the tip are still held.
		ft.mustRows(1, `UPDATE run_terminal_preservations SET attempts=attempts WHERE id=$1 AND state='publishing' AND live_run_id IS NOT NULL AND tip IS NOT NULL`, p.ID)
	})

	t.Run("the CHECK backstops the guard", func(t *testing.T) {
		ft := *f
		ft.t = t
		p, _ := ft.toLive(ft.newRun())
		ft.mustRows(1, `UPDATE run_terminal_preservations SET state='publishing' WHERE id=$1`, p.ID)
		mustExec(ft.ctx, t, ft.pool, `ALTER TABLE run_terminal_preservations DISABLE TRIGGER run_terminal_preservation_guard`)
		defer mustExec(ft.ctx, t, ft.pool, `ALTER TABLE run_terminal_preservations ENABLE TRIGGER run_terminal_preservation_guard`)
		_, err := ft.rows(`UPDATE run_terminal_preservations SET state='not_published', live_run_id=NULL WHERE id=$1`, p.ID)
		heldCheckName(t, err, "run_terminal_preservations_not_published_check")
	})

	t.Run("refused while a contribution may have created the ref", func(t *testing.T) {
		ft := *f
		ft.t = t
		for _, state := range []string{"invoked", "publish_unknown", "published"} {
			p := ft.insertPreservation(ft.newRun())
			c := ft.insertContribution(p, 1, termTipA, nil)
			ft.mustRows(1, `UPDATE run_terminal_preservations SET state='publishing', live_run_id=run_id WHERE id=$1`, p.ID)
			ft.mustRows(1, `UPDATE run_terminal_contributions SET state='invoked', create_invoked_at=now() WHERE id=$1`, c.ID)
			switch state {
			case "publish_unknown":
				ft.mustRows(1, `UPDATE run_terminal_contributions SET state='publish_unknown' WHERE id=$1`, c.ID)
			case "published":
				ft.mustRows(1, `UPDATE run_terminal_contributions SET state='published', published_at=now() WHERE id=$1`, c.ID)
			}
			ft.mustRows(0, `UPDATE run_terminal_preservations SET state='not_published', live_run_id=NULL WHERE id=$1`, p.ID)
		}
	})

	t.Run("allowed while every contribution is unsent or refused", func(t *testing.T) {
		ft := *f
		ft.t = t
		p := ft.insertPreservation(ft.newRun())
		c := ft.insertContribution(p, 1, termTipA, nil)
		ft.mustRows(1, `UPDATE run_terminal_preservations SET state='publishing', live_run_id=run_id WHERE id=$1`, p.ID)
		ft.mustRows(1, `UPDATE run_terminal_contributions SET state='invoked', create_invoked_at=now() WHERE id=$1`, c.ID)
		ft.mustRows(1, `UPDATE run_terminal_contributions SET state='refused', refusal_reason='create_refused' WHERE id=$1`, c.ID)
		ft.insertContribution(p, 2, termTipB, nil) // prepared, never sent
		ft.mustRows(1, `UPDATE run_terminal_preservations SET state='not_published', live_run_id=NULL WHERE id=$1`, p.ID)
	})
}

func TestTerminalPreservationGuardLiveDB(t *testing.T) {
	f := newTermFixture(t, "term_guard_")

	// Born bound: no INSERT may carry a later state or any lifecycle marker.
	t.Run("insert markers refused", func(t *testing.T) {
		ft := *f
		ft.t = t
		for name, c := range map[string]struct{ col, val, extra string }{
			"state live":      {"state", "'live'", ", live_run_id"},
			"state retiring":  {"state", "'retiring'", ", live_run_id"},
			"state deleted":   {"state", "'deleted'", ""},
			"tip":             {"tip", "'" + termTipA + "'", ""},
			"first published": {"first_published_at", "now()", ""},
			"deleted_at":      {"deleted_at", "now()", ""},
			"retire_tip":      {"retire_tip", "'" + termTipA + "'", ""},
			"retire_reason":   {"retire_reason", "'issue_closed'", ""},
			"retire_observed": {"retire_observed_at", "now()", ""},
			"retire_decided":  {"retire_decided_at", "now()", ""},
		} {
			run := ft.newRun()
			extraVal := ""
			if c.extra != "" {
				extraVal = ", $1"
			}
			ft.mustRows(0, fmt.Sprintf(`INSERT INTO run_terminal_preservations (run_id,user_id,repo_id,ref,lifetime_at_bind,%s%s)
				VALUES($1,$2,$3,'refs/uzi-terminal/'||$1::uuid::text,'no_pr',%s%s)`, c.col, c.extra, c.val, extraVal), run, ft.user, ft.repo)
			_ = name
		}
	})

	t.Run("identity and lifetime", func(t *testing.T) {
		ft := *f
		ft.t = t
		p := ft.insertPreservation(ft.newRun())
		for _, set := range []string{
			`user_id=gen_random_uuid()`, `repo_id=gen_random_uuid()`, `id=gen_random_uuid()`,
			`run_id=gen_random_uuid()`, `created_at=created_at - interval '1 day'`,
		} {
			ft.mustRows(0, `UPDATE run_terminal_preservations SET `+set+` WHERE id=$1`, p.ID)
		}
		ft.mustRows(0, `UPDATE run_terminal_preservations SET lifetime_at_bind='issue' WHERE id=$1`, p.ID)
		ft.mustRows(1, `UPDATE run_terminal_preservations SET lifetime_at_bind='pr' WHERE id=$1`, p.ID)
		for _, to := range []string{"no_pr", "issue"} {
			ft.mustRows(0, `UPDATE run_terminal_preservations SET lifetime_at_bind=$2 WHERE id=$1`, p.ID, to)
		}
		// Scheduling fields stay writable on a live row.
		ft.mustRows(1, `UPDATE run_terminal_preservations SET attempts=attempts+1, last_error='x', next_check_at=now() WHERE id=$1`, p.ID)
	})

	t.Run("first_published_at is set once", func(t *testing.T) {
		ft := *f
		ft.t = t
		p, _ := ft.toLive(ft.newRun())
		ft.mustRows(0, `UPDATE run_terminal_preservations SET first_published_at=now() + interval '1 second' WHERE id=$1`, p.ID)
		ft.mustRows(0, `UPDATE run_terminal_preservations SET first_published_at=NULL WHERE id=$1`, p.ID)
		// A same-value write is not a renewal.
		ft.mustRows(1, `UPDATE run_terminal_preservations SET first_published_at=first_published_at WHERE id=$1`, p.ID)
		// A later generation re-publishing does not renew it either.
		ft.mustRows(1, `UPDATE run_terminal_preservations SET state='publishing' WHERE id=$1`, p.ID)
		ft.mustRows(0, `UPDATE run_terminal_preservations SET state='live', first_published_at=now() + interval '1 second' WHERE id=$1`, p.ID)
		ft.mustRows(1, `UPDATE run_terminal_preservations SET state='live' WHERE id=$1`, p.ID)
	})

	t.Run("tip advance", func(t *testing.T) {
		ft := *f
		ft.t = t
		run := ft.newRun()
		p := ft.insertPreservation(run)
		prior := termTipA
		c1 := ft.insertContribution(p, 1, termTipA, nil)
		c2 := ft.insertContribution(p, 2, termTipB, &prior)
		unrelatedPrior := termTipC
		c3 := ft.insertContribution(p, 3, termTipC, &unrelatedPrior)
		ft.mustRows(1, `UPDATE run_terminal_preservations SET state='publishing', live_run_id=run_id WHERE id=$1`, p.ID)

		// From NULL, only to the tip of a PUBLISHED contribution.
		ft.mustRows(0, `UPDATE run_terminal_preservations SET tip=$2 WHERE id=$1`, p.ID, termTipA)
		ft.mustRows(1, `UPDATE run_terminal_contributions SET state='invoked', create_invoked_at=now() WHERE id=$1`, c1.ID)
		ft.mustRows(0, `UPDATE run_terminal_preservations SET tip=$2 WHERE id=$1`, p.ID, termTipA)
		ft.mustRows(1, `UPDATE run_terminal_contributions SET state='published', published_at=now() WHERE id=$1`, c1.ID)
		ft.mustRows(0, `UPDATE run_terminal_preservations SET tip=$2 WHERE id=$1`, p.ID, termTipB)
		ft.mustRows(1, `UPDATE run_terminal_preservations SET state='live', tip=$2, first_published_at=now() WHERE id=$1`, p.ID, termTipA)

		// Never back to NULL, never to an unpublished or foreign tip.
		ft.mustRows(0, `UPDATE run_terminal_preservations SET tip=NULL WHERE id=$1`, p.ID)
		ft.mustRows(0, `UPDATE run_terminal_preservations SET tip=$2 WHERE id=$1`, p.ID, termTipB)
		// A marker needs a publishing preservation, so the live row goes back to publishing first.
		ft.mustRows(1, `UPDATE run_terminal_preservations SET state='publishing' WHERE id=$1`, p.ID)
		ft.mustRows(1, `UPDATE run_terminal_contributions SET state='invoked', create_invoked_at=now() WHERE id=$1`, c2.ID)
		ft.mustRows(1, `UPDATE run_terminal_contributions SET state='published', published_at=now() WHERE id=$1`, c2.ID)
		ft.mustRows(1, `UPDATE run_terminal_contributions SET state='invoked', create_invoked_at=now() WHERE id=$1`, c3.ID)
		ft.mustRows(1, `UPDATE run_terminal_contributions SET state='published', published_at=now() WHERE id=$1`, c3.ID)
		// c3 is published but was prepared against a tip that is not the current one (A).
		ft.mustRows(0, `UPDATE run_terminal_preservations SET tip=$2 WHERE id=$1`, p.ID, termTipC)
		ft.mustRows(1, `UPDATE run_terminal_preservations SET tip=$2 WHERE id=$1`, p.ID, termTipB)
		// Another preservation's published tip does not count.
		other, _ := ft.toLive(ft.newRun())
		ft.mustRows(0, `UPDATE run_terminal_preservations SET tip=$2 WHERE id=$1`, other.ID, termTipB)
	})

	t.Run("retiring is irrevocable", func(t *testing.T) {
		ft := *f
		ft.t = t
		p := ft.toState("retiring")
		for _, to := range []string{"live", "publishing", "bound", "not_published"} {
			if n, err := ft.transition(p.ID, "retiring", to); err != nil || n != 0 {
				t.Fatalf("retiring -> %s: rows=%d err=%v, want a refusal", to, n, err)
			}
		}
		// The recorded decision and the tip are frozen; scheduling is not.
		ft.mustRows(0, `UPDATE run_terminal_preservations SET retire_reason='pr_merged' WHERE id=$1`, p.ID)
		ft.mustRows(0, `UPDATE run_terminal_preservations SET retire_tip=$2 WHERE id=$1`, p.ID, termTipB)
		ft.mustRows(0, `UPDATE run_terminal_preservations SET retire_decided_at=now() + interval '1 minute' WHERE id=$1`, p.ID)
		ft.mustRows(0, `UPDATE run_terminal_preservations SET tip=$2 WHERE id=$1`, p.ID, termTipB)
		ft.mustRows(1, `UPDATE run_terminal_preservations SET attempts=attempts+1, next_check_at=now() WHERE id=$1`, p.ID)
		ft.mustRows(1, `UPDATE run_terminal_preservations SET state='deleted', live_run_id=NULL, deleted_at=now() WHERE id=$1`, p.ID)
	})

	t.Run("finished rows are frozen", func(t *testing.T) {
		ft := *f
		ft.t = t
		for _, state := range []string{"deleted", "abandoned"} {
			p := ft.toState(state)
			ft.mustRows(0, `UPDATE run_terminal_preservations SET attempts=attempts+1 WHERE id=$1`, p.ID)
			ft.mustRows(0, `UPDATE run_terminal_preservations SET last_error='late' WHERE id=$1`, p.ID)
		}
	})

	t.Run("delete refuses pointer-bearing rows", func(t *testing.T) {
		ft := *f
		ft.t = t
		// Rows without history: the guard alone answers.
		for state, want := range map[string]int64{"bound": 1, "not_published": 1, "publishing": 0} {
			p := ft.toState(state)
			ft.mustRows(want, `DELETE FROM run_terminal_preservations WHERE id=$1`, p.ID)
		}
		// Rows with a published contribution: the pointer guard answers before the FK does.
		for _, state := range []string{"live", "retiring"} {
			p := ft.toState(state)
			ft.mustRows(0, `DELETE FROM run_terminal_preservations WHERE id=$1`, p.ID)
		}
		// Once the pointer is gone only the contribution FK holds the row.
		deleted := ft.toState("deleted")
		if _, err := ft.rows(`DELETE FROM run_terminal_preservations WHERE id=$1`, deleted.ID); err == nil || !strings.Contains(err.Error(), "run_terminal_contributions_preservation_fkey") {
			t.Fatalf("a preservation with contributions must be restricted by the composite FK, got %v", err)
		}
	})

	t.Run("pointer restricts the run delete", func(t *testing.T) {
		ft := *f
		ft.t = t
		run := ft.newRun()
		p := ft.insertPreservation(run)
		ft.mustRows(1, `UPDATE run_terminal_preservations SET state='publishing', live_run_id=run_id WHERE id=$1`, p.ID)
		_, err := ft.rows(`DELETE FROM runs WHERE id=$1`, run)
		heldCheckName(t, err, "run_terminal_preservations_live_run_id_fkey")
	})
}

func TestTerminalContributionGuardLiveDB(t *testing.T) {
	f := newTermFixture(t, "term_contrib_")
	p := f.insertPreservation(f.newRun())
	// The prepared -> invoked marker needs its preservation publishing (migration 00433), so every
	// contribution below that is built past prepared shares this publishing preservation.
	f.mustRows(1, `UPDATE run_terminal_preservations SET state='publishing', live_run_id=run_id WHERE id=$1`, p.ID)
	gen := int64(0)
	next := func() int64 { gen++; return gen }

	// contributionIn builds a contribution in the given state by legal moves, on a preservation of
	// its own: a preservation may have only one unresolved (invoked or publish_unknown) push.
	contributionIn := func(state string) store.RunTerminalContribution {
		pp := f.insertPreservation(f.newRun())
		f.mustRows(1, `UPDATE run_terminal_preservations SET state='publishing', live_run_id=run_id WHERE id=$1`, pp.ID)
		if state == "recorded" {
			params := f.contributionParams(pp, next(), termTipA, nil)
			params.Disposition = "closed_issue_abandoned"
			row, err := f.q.InsertTerminalContribution(f.ctx, params)
			if err != nil {
				t.Fatal(err)
			}
			return row
		}
		c := f.insertContribution(pp, next(), termTipA, nil)
		switch state {
		case "invoked", "published", "publish_unknown", "refused":
			f.mustRows(1, `UPDATE run_terminal_contributions SET state='invoked', create_invoked_at=now() WHERE id=$1`, c.ID)
		}
		switch state {
		case "published":
			f.mustRows(1, `UPDATE run_terminal_contributions SET state='published', published_at=now() WHERE id=$1`, c.ID)
		case "publish_unknown":
			f.mustRows(1, `UPDATE run_terminal_contributions SET state='publish_unknown' WHERE id=$1`, c.ID)
		case "refused":
			f.mustRows(1, `UPDATE run_terminal_contributions SET state='refused', refusal_reason='foreign_tip' WHERE id=$1`, c.ID)
		}
		return c
	}

	states := []string{"prepared", "invoked", "published", "publish_unknown", "refused", "recorded"}
	allowed := map[string][]string{
		"prepared":        {"invoked"},
		"invoked":         {"published", "refused", "publish_unknown"},
		"publish_unknown": {"published", "refused"},
	}
	for _, from := range states {
		for _, to := range states {
			if from == to {
				continue
			}
			t.Run(from+"_to_"+to, func(t *testing.T) {
				ft := *f
				ft.t = t
				c := contributionIn(from)
				set := "state=$2"
				switch to {
				case "invoked":
					set += ", create_invoked_at=COALESCE(create_invoked_at, now())"
				case "published":
					set += ", create_invoked_at=COALESCE(create_invoked_at, now()), published_at=now()"
				case "refused":
					set += ", create_invoked_at=COALESCE(create_invoked_at, now()), refusal_reason='foreign_tip'"
				case "publish_unknown":
					set += ", create_invoked_at=COALESCE(create_invoked_at, now())"
				case "prepared":
					set += ", create_invoked_at=NULL"
				}
				n, err := ft.rows(`UPDATE run_terminal_contributions SET `+set+` WHERE id=$1`, c.ID, to)
				want := int64(0)
				for _, a := range allowed[from] {
					if a == to {
						want = 1
					}
				}
				// to=recorded breaks the disposition tie, but only after the guard has had its say.
				if to == "recorded" && want == 0 {
					if err != nil || n != 0 {
						t.Fatalf("%s -> recorded: rows=%d err=%v, want a silent refusal", from, n, err)
					}
					return
				}
				if err != nil || n != want {
					t.Fatalf("%s -> %s: rows=%d err=%v, want rows=%d", from, to, n, err, want)
				}
			})
		}
	}

	t.Run("born prepared or recorded", func(t *testing.T) {
		ft := *f
		ft.t = t
		insert := func(extraCols, extraVals, disposition, state string) (int64, error) {
			return ft.rows(`INSERT INTO run_terminal_contributions
				(preservation_id,run_id,generation,worker_id,user_id,authority,tip,coverage_digest,manifest,disposition,state`+extraCols+`)
				VALUES($1,$2,$3,$4,$5,'current_flight',$6,$7,'{}',$8,$9`+extraVals+`)`,
				p.ID, p.RunID, next(), ft.worker, ft.user, termTipA, termDigest, disposition, state)
		}
		for _, bad := range []struct{ cols, vals, disposition, state string }{
			{"", "", "preserved", "invoked"},
			{", create_invoked_at", ", now()", "preserved", "invoked"},
			{"", "", "preserved", "published"},
			{", create_invoked_at, published_at", ", now(), now()", "preserved", "published"},
			{"", "", "preserved", "refused"},
			{", refusal_reason", ", 'foreign_tip'", "preserved", "prepared"},
			{"", "", "closed_pr_abandoned", "prepared"},
		} {
			if n, err := insert(bad.cols, bad.vals, bad.disposition, bad.state); err != nil || n != 0 {
				t.Fatalf("insert %+v: rows=%d err=%v, want a silent refusal", bad, n, err)
			}
		}
		for _, good := range []struct{ disposition, state string }{
			{"preserved", "prepared"}, {"closed_issue_abandoned", "recorded"}, {"closed_pr_abandoned", "recorded"},
		} {
			if n, err := insert("", "", good.disposition, good.state); err != nil || n != 1 {
				t.Fatalf("insert %+v: rows=%d err=%v", good, n, err)
			}
		}
	})

	t.Run("immutable evidence", func(t *testing.T) {
		ft := *f
		ft.t = t
		prior := termTipB
		c := ft.insertContribution(p, next(), termTipA, &prior)
		for _, set := range []string{
			`tip='` + termTipC + `'`, `prior_tip=NULL`, `prior_tip='` + termTipC + `'`,
			`coverage_digest='` + strings.Repeat("e", 64) + `'`, `manifest='{"roots":[1]}'`,
			`generation=generation+100`, `worker_id=gen_random_uuid()`, `user_id=gen_random_uuid()`,
			`authority='open_guarded_hold'`, `disposition='closed_pr_abandoned'`, `run_id=gen_random_uuid()`,
			`preservation_id=gen_random_uuid()`, `id=gen_random_uuid()`,
		} {
			ft.mustRows(0, `UPDATE run_terminal_contributions SET `+set+` WHERE id=$1`, c.ID)
		}
		// A state-preserving scheduling write is fine while the row is open.
		ft.mustRows(1, `UPDATE run_terminal_contributions SET updated_at=now() WHERE id=$1`, c.ID)
	})

	t.Run("create_invoked_at is set once", func(t *testing.T) {
		ft := *f
		ft.t = t
		c := contributionIn("invoked")
		ft.mustRows(0, `UPDATE run_terminal_contributions SET create_invoked_at=now() + interval '1 second' WHERE id=$1`, c.ID)
		ft.mustRows(0, `UPDATE run_terminal_contributions SET create_invoked_at=NULL WHERE id=$1`, c.ID)
		ft.mustRows(0, `UPDATE run_terminal_contributions SET state='published', published_at=now(), create_invoked_at=now() + interval '1 second' WHERE id=$1`, c.ID)
		ft.mustRows(1, `UPDATE run_terminal_contributions SET state='published', published_at=now() WHERE id=$1`, c.ID)
		// published_at is set once too, and a published row is frozen.
		ft.mustRows(0, `UPDATE run_terminal_contributions SET published_at=now() + interval '1 second' WHERE id=$1`, c.ID)
		ft.mustRows(0, `UPDATE run_terminal_contributions SET updated_at=now() WHERE id=$1`, c.ID)
	})

	t.Run("checks", func(t *testing.T) {
		ft := *f
		ft.t = t
		bad := func(col, val string) error {
			cols := map[string]string{
				"generation": fmt.Sprint(next()), "manifest": "'{}'", "authority": "'current_flight'", "hold_id": "NULL",
				"tip": "'" + termTipA + "'", "coverage_digest": "'" + termDigest + "'",
			}
			cols[col] = val
			_, err := ft.rows(fmt.Sprintf(`INSERT INTO run_terminal_contributions
				(preservation_id,run_id,generation,worker_id,user_id,authority,tip,coverage_digest,manifest,disposition,state,hold_id)
				VALUES($1,$2,%s,$3,$4,%s,%s,%s,%s,'preserved','prepared',%s)`,
				cols["generation"], cols["authority"], cols["tip"], cols["coverage_digest"], cols["manifest"], cols["hold_id"]),
				p.ID, p.RunID, ft.worker, ft.user)
			return err
		}
		heldCheckName(t, bad("generation", "0"), "run_terminal_contributions_generation_check")
		heldCheckName(t, bad("manifest", "'[]'"), "run_terminal_contributions_manifest_check")
		heldCheckName(t, bad("manifest", `jsonb_build_object('pad', repeat('x', 262144))`), "run_terminal_contributions_manifest_check")
		heldCheckName(t, bad("authority", "'open_guarded_hold'"), "run_terminal_contributions_hold_authority_check")
		heldCheckName(t, bad("authority", "'bogus'"), "run_terminal_contributions_authority_check")
		heldCheckName(t, bad("hold_id", "'"+ft.hold.String()+"'"), "run_terminal_contributions_hold_authority_check")
		heldCheckName(t, bad("tip", "'nothex'"), "run_terminal_contributions_tip_check")
		heldCheckName(t, bad("coverage_digest", "'abc'"), "run_terminal_contributions_coverage_digest_check")

		// A guarded-hold contribution names its hold, and the hold cannot then be deleted.
		if _, err := ft.rows(`INSERT INTO run_terminal_contributions
			(preservation_id,run_id,generation,worker_id,user_id,hold_id,authority,tip,coverage_digest,manifest,disposition,state)
			VALUES($1,$2,$3,$4,$5,$6,'open_guarded_hold',$7,$8,'{}','preserved','prepared')`,
			p.ID, p.RunID, next(), ft.worker, ft.user, ft.hold, termTipA, termDigest); err != nil {
			t.Fatalf("hold-authority insert: %v", err)
		}
		_, err := ft.rows(`DELETE FROM recovery_custody_holds WHERE id=$1`, ft.hold)
		heldCheckName(t, err, "run_terminal_contributions_hold_id_fkey")

		// Marker ties.
		c := ft.insertContribution(p, next(), termTipA, nil)
		_, err = ft.rows(`UPDATE run_terminal_contributions SET state='invoked' WHERE id=$1`, c.ID)
		heldCheckName(t, err, "run_terminal_contributions_invoked_marker_check")
		ft.mustRows(1, `UPDATE run_terminal_contributions SET state='invoked', create_invoked_at=now() WHERE id=$1`, c.ID)
		_, err = ft.rows(`UPDATE run_terminal_contributions SET state='published' WHERE id=$1`, c.ID)
		heldCheckName(t, err, "run_terminal_contributions_published_stamp_check")
		_, err = ft.rows(`UPDATE run_terminal_contributions SET state='refused', refusal_reason='junk' WHERE id=$1`, c.ID)
		heldCheckName(t, err, "run_terminal_contributions_refusal_reason_check")
		// A refusal reason belongs to a refused contribution only.
		_, err = ft.rows(`UPDATE run_terminal_contributions SET refusal_reason='foreign_tip' WHERE id=$1`, c.ID)
		heldCheckName(t, err, "run_terminal_contributions_refusal_state_check")

		// One contribution per (run, generation); the composite FK binds run to its preservation.
		dup := ft.contributionParams(p, c.Generation, termTipA, nil)
		again, err := ft.q.InsertTerminalContribution(ft.ctx, dup)
		if err != nil || again.ID != c.ID || again.State != "invoked" {
			t.Fatalf("a repeated insert must load the existing row unchanged, got %+v err=%v", again, err)
		}
		_, err = ft.rows(`INSERT INTO run_terminal_contributions
			(preservation_id,run_id,generation,worker_id,user_id,authority,tip,coverage_digest,manifest,disposition,state)
			VALUES($1,gen_random_uuid(),$2,$3,$4,'current_flight',$5,$6,'{}','preserved','prepared')`,
			p.ID, next(), ft.worker, ft.user, termTipA, termDigest)
		heldCheckName(t, err, "run_terminal_contributions_preservation_fkey")
	})

	t.Run("delete refuses a pushed tip's record", func(t *testing.T) {
		ft := *f
		ft.t = t
		for _, state := range states {
			c := contributionIn(state)
			want := int64(1)
			if state == "invoked" || state == "published" || state == "publish_unknown" {
				want = 0
			}
			ft.mustRows(want, `DELETE FROM run_terminal_contributions WHERE id=$1`, c.ID)
		}
	})
}

// The insert-or-load queries return the existing row on a conflict and expose both lookups.
func TestTerminalPreservationQueriesLiveDB(t *testing.T) {
	f := newTermFixture(t, "term_queries_")
	run := f.newRun()
	p := f.insertPreservation(run)
	if again := f.insertPreservation(run); again.ID != p.ID || again.State != "bound" {
		t.Fatalf("a repeated insert must load the existing row, got %+v", again)
	}
	got, err := f.q.GetTerminalPreservationByRun(f.ctx, run)
	if err != nil || got.ID != p.ID || got.Ref != "refs/uzi-terminal/"+run.String() || got.LifetimeAtBind != "no_pr" {
		t.Fatalf("get by run = %+v err=%v", got, err)
	}
	manifest, _ := json.Marshal(map[string]any{"roots": []string{}})
	params := f.contributionParams(p, 1, termTipA, nil)
	params.Manifest = manifest
	c1, err := f.q.InsertTerminalContribution(f.ctx, params)
	if err != nil || c1.State != "prepared" || c1.HoldID.Valid || c1.PriorTip.Valid {
		t.Fatalf("insert contribution = %+v err=%v", c1, err)
	}
	prior := termTipA
	params2 := f.contributionParams(p, 2, termTipB, &prior)
	params2.Authority = "open_guarded_hold"
	params2.HoldID = pgtype.UUID{Bytes: f.hold, Valid: true}
	c2, err := f.q.InsertTerminalContribution(f.ctx, params2)
	if err != nil || c2.PriorTip.String != termTipA || c2.HoldID.Bytes != f.hold {
		t.Fatalf("insert contribution 2 = %+v err=%v", c2, err)
	}
	params3 := f.contributionParams(p, 3, termTipC, nil)
	params3.Disposition = "closed_pr_abandoned"
	c3, err := f.q.InsertTerminalContribution(f.ctx, params3)
	if err != nil || c3.State != "recorded" {
		t.Fatalf("an abandonment must be born recorded, got %+v err=%v", c3, err)
	}
	byGen, err := f.q.GetTerminalContributionByRunGeneration(f.ctx, store.GetTerminalContributionByRunGenerationParams{RunID: run, Generation: 2})
	if err != nil || byGen.ID != c2.ID {
		t.Fatalf("get by run+generation = %+v err=%v", byGen, err)
	}
	all, err := f.q.ListTerminalContributionsForRun(f.ctx, run)
	if err != nil || len(all) != 3 || all[0].ID != c3.ID || all[1].ID != c2.ID || all[2].ID != c1.ID {
		t.Fatalf("list for run must be newest generation first, got %d rows err=%v", len(all), err)
	}
}

func TestTerminalPreservationDueListLiveDB(t *testing.T) {
	f := newTermFixture(t, "term_due_")
	mk := func(state string, due string) uuid.UUID {
		p := f.toState(state)
		f.mustRows(1, `UPDATE run_terminal_preservations SET next_check_at=now() + $2::interval WHERE id=$1`, p.ID, due)
		return p.ID
	}
	// Frozen rows cannot be rescheduled, so make their due time before they finish.
	early := mk("bound", "-3 hours")
	laterSameBatch := mk("live", "-2 hours")
	publishing := mk("publishing", "-1 hour")
	retiring := mk("retiring", "-30 minutes")
	future := mk("live", "1 hour")
	notPublished := mk("not_published", "-4 hours")
	doneP := f.toState("deleted")
	_ = doneP
	// Two rows due at the same instant are ordered by id.
	tie1, tie2 := mk("bound", "-10 minutes"), mk("bound", "-10 minutes")
	f.mustRows(2, `UPDATE run_terminal_preservations SET next_check_at='2020-01-01T00:00:00Z' WHERE id = ANY($1::uuid[])`, []string{tie1.String(), tie2.String()})
	tiesFirst := []uuid.UUID{tie1, tie2}
	if strings.Compare(tie1.String(), tie2.String()) > 0 {
		tiesFirst = []uuid.UUID{tie2, tie1}
	}

	rows, err := f.q.ListTerminalPreservationsDue(f.ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	var ids []uuid.UUID
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	want := append(append([]uuid.UUID{}, tiesFirst...), early, laterSameBatch, publishing, retiring)
	if len(ids) != len(want) {
		t.Fatalf("due = %v, want %v (not_published, deleted and future rows excluded: %s %s)", ids, want, notPublished, future)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("due[%d] = %s, want %s\nall: %v", i, ids[i], want[i], ids)
		}
	}
	limited, err := f.q.ListTerminalPreservationsDue(f.ctx, 2)
	if err != nil || len(limited) != 2 || limited[0].ID != want[0] || limited[1].ID != want[1] {
		t.Fatalf("limit 2 = %d rows err=%v", len(limited), err)
	}
}

// The widened reason CHECK accepts the two new values, keeps the ten old ones and still refuses junk.
func TestTerminalPreservationReasonCheckLiveDB(t *testing.T) {
	ctx, _, pool := heldMigratedPool(t, "term_reason_")
	f := heldNewHoldFixture(ctx, t, pool, false, "failed", nil)
	// The reason is stamped by a nested guard in production; this test targets the CHECK alone.
	mustExec(ctx, t, pool, `ALTER TABLE recovery_custody_holds DISABLE TRIGGER completed_publication_hold_guard`)

	var validated bool
	if err := pool.QueryRow(ctx, `SELECT convalidated FROM pg_constraint
		WHERE conrelid='recovery_custody_holds'::regclass AND conname='recovery_custody_holds_completed_publication_reason_check'`).Scan(&validated); err != nil || !validated {
		t.Fatalf("the widened CHECK must exist under its original name and be validated: %v %v", validated, err)
	}
	for _, reason := range []string{
		"completion_identity_missing", "not_completed", "identity_changed", "mr_missing", "branch_missing",
		"branch_mismatch", "head_mismatch", "not_ancestor", "ancestry_unknown", "forge_timeout",
		"terminal_preservation_pending", "terminal_preservation_refused",
	} {
		if _, err := pool.Exec(ctx, `UPDATE recovery_custody_holds SET completed_publication_reason=$2 WHERE id=$1`, f.hold, reason); err != nil {
			t.Fatalf("reason %q refused: %v", reason, err)
		}
	}
	for _, junk := range []string{"terminal_preservation", "terminal_preservation_pending ", "", "nope"} {
		_, err := pool.Exec(ctx, `UPDATE recovery_custody_holds SET completed_publication_reason=$2 WHERE id=$1`, f.hold, junk)
		heldCheckName(t, err, "recovery_custody_holds_completed_publication_reason_check")
	}
}

// Down refuses while a pointer-bearing preservation exists, then removes the objects, nulls the
// two new reasons and restores the narrower CHECK; re-applying Up works.
func TestTerminalPreservationRollbackLiveDB(t *testing.T) {
	ctx, dsn, pool := heldMigratedPool(t, "term_rollback_")
	f := heldNewHoldFixture(ctx, t, pool, false, "failed", nil)
	ft := &termFixture{heldFixture: f, ctx: ctx, q: store.New(pool), t: t}
	live, _ := ft.toLive(f.run)
	mustExec(ctx, t, pool, `ALTER TABLE recovery_custody_holds DISABLE TRIGGER completed_publication_hold_guard`)
	mustExec(ctx, t, pool, `UPDATE recovery_custody_holds SET completed_publication_reason='terminal_preservation_refused' WHERE id=$1`, f.hold)
	mustExec(ctx, t, pool, `ALTER TABLE recovery_custody_holds ENABLE TRIGGER completed_publication_hold_guard`)

	if err := store.MigrateDownTo(ctx, dsn, 431); err == nil {
		t.Fatal("Down accepted a live terminal preservation")
	}
	var applied bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('run_terminal_preservations') IS NOT NULL
		AND to_regclass('run_terminal_contributions') IS NOT NULL`).Scan(&applied); err != nil || !applied {
		t.Fatalf("refused Down changed the schema: %v %v", applied, err)
	}
	// Retire the ref's row: a finished preservation no longer blocks the rollback.
	ft.mustRows(1, `UPDATE run_terminal_preservations SET state='retiring', `+termRetireStamp+` WHERE id=$1`, live.ID, termTipA)
	ft.mustRows(1, `UPDATE run_terminal_preservations SET state='deleted', live_run_id=NULL, deleted_at=now() WHERE id=$1`, live.ID)
	if err := store.MigrateDownTo(ctx, dsn, 431); err != nil {
		t.Fatalf("Down: %v", err)
	}
	var objects int
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM pg_class WHERE relname IN ('run_terminal_preservations','run_terminal_contributions'))
		+ (SELECT count(*) FROM pg_proc WHERE proname LIKE 'run_terminal_%')`).Scan(&objects); err != nil || objects != 0 {
		t.Fatalf("terminal objects remain: %d err=%v", objects, err)
	}
	var reason *string
	if err := pool.QueryRow(ctx, `SELECT completed_publication_reason FROM recovery_custody_holds WHERE id=$1`, f.hold).Scan(&reason); err != nil || reason != nil {
		t.Fatalf("the new reason survived Down: %v err=%v", reason, err)
	}
	var def string
	if err := pool.QueryRow(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint
		WHERE conrelid='recovery_custody_holds'::regclass AND conname='recovery_custody_holds_completed_publication_reason_check'`).Scan(&def); err != nil ||
		strings.Contains(def, "terminal_preservation") || !strings.Contains(def, "forge_timeout") {
		t.Fatalf("the narrower CHECK was not restored: %q err=%v", def, err)
	}
	var enabled string
	if err := pool.QueryRow(ctx, `SELECT tgenabled::text FROM pg_trigger
		WHERE tgrelid='recovery_custody_holds'::regclass AND tgname='completed_publication_hold_guard'`).Scan(&enabled); err != nil || enabled != "O" {
		t.Fatalf("completed_publication_hold_guard not re-enabled: %q %v", enabled, err)
	}
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("re-up: %v", err)
	}
}

// termRefusal asserts a Down stopped with the refusal naming `want` and left both tables in place.
func termRefusal(t *testing.T, err error, want string, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Down error = %v, want the refusal %q", err, want)
	}
	var tables int
	if qerr := pool.QueryRow(context.Background(), `SELECT count(*) FROM pg_class
		WHERE relname IN ('run_terminal_preservations','run_terminal_contributions') AND relkind = 'r'`).Scan(&tables); qerr != nil {
		t.Fatalf("count tables: %v", qerr)
	}
	// 432 alone leaves one table, the other cases two.
	if tables == 0 {
		t.Fatal("a refused Down dropped the tables")
	}
}

// Each Down's own refusal fires, reached through the migration that owns it: a Down below it runs
// first, so the rows are inserted only after the later migrations are rolled back.
func TestTerminalPreservationRollbackRefusalsLiveDB(t *testing.T) {
	// 00432: a pointer-bearing preservation with no contribution (00433 would not refuse it).
	t.Run("00432 refuses a live pointer", func(t *testing.T) {
		ctx, dsn, pool := heldMigratedPool(t, "term_rb432_")
		f := heldNewHoldFixture(ctx, t, pool, false, "failed", nil)
		ft := &termFixture{heldFixture: f, ctx: ctx, q: store.New(pool), t: t}
		if err := store.MigrateDownTo(ctx, dsn, 432); err != nil {
			t.Fatalf("Down to 432: %v", err)
		}
		p := ft.insertPreservation(f.run)
		ft.mustRows(1, `UPDATE run_terminal_preservations SET state='publishing', live_run_id=run_id WHERE id=$1`, p.ID)
		termRefusal(t, store.MigrateDownTo(ctx, dsn, 431), "run_terminal_preservations has live rows", pool)
		// Dropping the pointer is what unblocks it: the refusal was the only obstacle.
		// The guard reads run_terminal_contributions, already rolled back: clear the pointer past it.
		mustExec(ctx, t, pool, `ALTER TABLE run_terminal_preservations DISABLE TRIGGER run_terminal_preservation_guard`)
		ft.mustRows(1, `UPDATE run_terminal_preservations SET state='not_published', live_run_id=NULL WHERE id=$1`, p.ID)
		if err := store.MigrateDownTo(ctx, dsn, 431); err != nil {
			t.Fatalf("Down after the pointer was cleared: %v", err)
		}
	})

	// 00433: no pointer anywhere, but a push that may have created the ref.
	for _, state := range []string{"invoked", "publish_unknown"} {
		t.Run("00433 refuses an "+state+" contribution", func(t *testing.T) {
			ctx, dsn, pool := heldMigratedPool(t, "term_rb433_")
			f := heldNewHoldFixture(ctx, t, pool, false, "failed", nil)
			ft := &termFixture{heldFixture: f, ctx: ctx, q: store.New(pool), t: t}
			if err := store.MigrateDownTo(ctx, dsn, 433); err != nil {
				t.Fatalf("Down to 433: %v", err)
			}
			p := ft.insertPreservation(f.run)
			c := ft.insertContribution(p, 1, termTipA, nil)
			ft.mustRows(1, `UPDATE run_terminal_preservations SET state='publishing', live_run_id=run_id WHERE id=$1`, p.ID)
			ft.mustRows(1, `UPDATE run_terminal_contributions SET state='invoked', create_invoked_at=now() WHERE id=$1`, c.ID)
			if state == "publish_unknown" {
				ft.mustRows(1, `UPDATE run_terminal_contributions SET state='publish_unknown' WHERE id=$1`, c.ID)
			}
			termRefusal(t, store.MigrateDownTo(ctx, dsn, 432), "run_terminal_contributions has live rows", pool)
			ft.mustRows(1, `UPDATE run_terminal_contributions SET state='refused', refusal_reason='create_refused' WHERE id=$1`, c.ID)
			// The preservation still bears its live pointer, which the 00433 Down also refuses on.
			ft.mustRows(1, `UPDATE run_terminal_preservations SET state='not_published', live_run_id=NULL WHERE id=$1`, p.ID)
			if err := store.MigrateDownTo(ctx, dsn, 432); err != nil {
				t.Fatalf("Down after the contribution was refused: %v", err)
			}
		})
	}

	// 00434 runs first on a full rollback: its refusal must stop it before the reasons are cleared.
	for name, arrange := range map[string]func(ft *termFixture, run uuid.UUID){
		"a live pointer": func(ft *termFixture, run uuid.UUID) {
			p := ft.insertPreservation(run)
			ft.mustRows(1, `UPDATE run_terminal_preservations SET state='publishing', live_run_id=run_id WHERE id=$1`, p.ID)
		},
		"an invoked contribution": func(ft *termFixture, run uuid.UUID) {
			p := ft.insertPreservation(run)
			c := ft.insertContribution(p, 1, termTipA, nil)
			ft.mustRows(1, `UPDATE run_terminal_preservations SET state='publishing', live_run_id=run_id WHERE id=$1`, p.ID)
			ft.mustRows(1, `UPDATE run_terminal_contributions SET state='invoked', create_invoked_at=now() WHERE id=$1`, c.ID)
		},
	} {
		t.Run("00434 refuses "+name+" before clearing the reasons", func(t *testing.T) {
			ctx, dsn, pool := heldMigratedPool(t, "term_rb434_")
			f := heldNewHoldFixture(ctx, t, pool, false, "failed", nil)
			ft := &termFixture{heldFixture: f, ctx: ctx, q: store.New(pool), t: t}
			arrange(ft, f.run)
			mustExec(ctx, t, pool, `ALTER TABLE recovery_custody_holds DISABLE TRIGGER completed_publication_hold_guard`)
			mustExec(ctx, t, pool, `UPDATE recovery_custody_holds SET completed_publication_reason='terminal_preservation_refused' WHERE id=$1`, f.hold)
			mustExec(ctx, t, pool, `ALTER TABLE recovery_custody_holds ENABLE TRIGGER completed_publication_hold_guard`)

			termRefusal(t, store.MigrateDownTo(ctx, dsn, 431), "has live rows", pool)
			var reason *string
			if err := pool.QueryRow(ctx, `SELECT completed_publication_reason FROM recovery_custody_holds WHERE id=$1`, f.hold).Scan(&reason); err != nil ||
				reason == nil || *reason != "terminal_preservation_refused" {
				t.Fatalf("a refused rollback cleared the reason: %v err=%v", reason, err)
			}
		})
	}
}

// The 00434 Down must clear the new reasons on a hold that carries a merged-predecessor discard
// binding, which merged_predecessor_discard_guard rejects any change to unless it is disabled.
func TestTerminalPreservationRollbackBoundHoldLiveDB(t *testing.T) {
	ctx, dsn, pool := heldMigratedPool(t, "term_rbbind_")
	f := heldNewHoldFixture(ctx, t, pool, false, "failed", nil)
	// The binding is validated by the guard on the way in; the guards are off to seed the end state.
	mustExec(ctx, t, pool, `ALTER TABLE recovery_custody_holds DISABLE TRIGGER merged_predecessor_discard_guard`)
	mustExec(ctx, t, pool, `ALTER TABLE recovery_custody_holds DISABLE TRIGGER completed_publication_hold_guard`)
	mustExec(ctx, t, pool, `UPDATE recovery_custody_holds SET state='discarded', live_worker_id=NULL, live_run_id=NULL,
		release_evidence='merged_predecessor_discard', merged_predecessor_discard_binding='{}'::jsonb,
		completed_publication_reason='terminal_preservation_pending' WHERE id=$1`, f.hold)
	mustExec(ctx, t, pool, `ALTER TABLE recovery_custody_holds ENABLE TRIGGER completed_publication_hold_guard`)
	mustExec(ctx, t, pool, `ALTER TABLE recovery_custody_holds ENABLE TRIGGER merged_predecessor_discard_guard`)

	if err := store.MigrateDownTo(ctx, dsn, 431); err != nil {
		t.Fatalf("Down with a bound hold: %v", err)
	}
	var reason *string
	if err := pool.QueryRow(ctx, `SELECT completed_publication_reason FROM recovery_custody_holds WHERE id=$1`, f.hold).Scan(&reason); err != nil || reason != nil {
		t.Fatalf("the new reason survived Down: %v err=%v", reason, err)
	}
	var enabled string
	if err := pool.QueryRow(ctx, `SELECT tgenabled::text FROM pg_trigger
		WHERE tgrelid='recovery_custody_holds'::regclass AND tgname='merged_predecessor_discard_guard'`).Scan(&enabled); err != nil || enabled != "O" {
		t.Fatalf("merged_predecessor_discard_guard not re-enabled: %q %v", enabled, err)
	}
}
