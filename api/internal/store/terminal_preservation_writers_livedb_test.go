package store_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4007 slice A, M3: the writer queries. Each is a CAS whose WHERE names the expected state
// (and the old tip); a guard-refused change and a lost race are both zero rows, which pgx reports as
// pgx.ErrNoRows for a :one query.

func termNoRows(t *testing.T, what string, err error) {
	t.Helper()
	if err != pgx.ErrNoRows {
		t.Fatalf("%s: want pgx.ErrNoRows (a refused CAS), got %v", what, err)
	}
}

func termText(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }

func termIn(d time.Duration) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: time.Now().Add(d), Valid: true}
}

func TestTerminalPreservationWritersLiveDB(t *testing.T) {
	f := newTermFixture(t, "term_writers_")

	t.Run("create path: publishing, invoked, published, live", func(t *testing.T) {
		ft := *f
		ft.t = t
		run := ft.newRun()
		p := ft.insertPreservation(run)
		c := ft.insertContribution(p, 1, termTipA, nil)

		// Wrong expected state or tip: zero rows, nothing changes.
		_, err := ft.q.MarkTerminalPreservationPublishing(ft.ctx, store.MarkTerminalPreservationPublishingParams{ID: p.ID, ExpectedState: "live"})
		termNoRows(t, "publishing from the wrong state", err)
		_, err = ft.q.MarkTerminalPreservationPublishing(ft.ctx, store.MarkTerminalPreservationPublishingParams{ID: p.ID, ExpectedState: "bound", ExpectedTip: termText(termTipA)})
		termNoRows(t, "publishing against the wrong tip", err)

		// The contribution cannot be invoked before its preservation is publishing (N1).
		_, err = ft.q.MarkTerminalContributionInvoked(ft.ctx, c.ID)
		termNoRows(t, "invoke while the preservation is bound", err)

		pub, err := ft.q.MarkTerminalPreservationPublishing(ft.ctx, store.MarkTerminalPreservationPublishingParams{ID: p.ID, ExpectedState: "bound"})
		if err != nil || pub.State != "publishing" || pub.LiveRunID.Bytes != run {
			t.Fatalf("publishing = %+v err=%v", pub, err)
		}
		inv, err := ft.q.MarkTerminalContributionInvoked(ft.ctx, c.ID)
		if err != nil || inv.State != "invoked" || !inv.CreateInvokedAt.Valid {
			t.Fatalf("invoked = %+v err=%v", inv, err)
		}
		_, err = ft.q.MarkTerminalContributionInvoked(ft.ctx, c.ID)
		termNoRows(t, "a second invoke", err)

		// published needs the exact expected state.
		_, err = ft.q.RecordTerminalContributionPublished(ft.ctx, store.RecordTerminalContributionPublishedParams{ID: c.ID, ExpectedState: "prepared"})
		termNoRows(t, "published from the wrong state", err)
		// The preservation tip cannot advance before the contribution is published (tip guard).
		_, err = ft.q.SettleTerminalPreservationLive(ft.ctx, store.SettleTerminalPreservationLiveParams{ID: p.ID, Tip: termText(termTipA), NextCheckAt: termIn(time.Hour)})
		termNoRows(t, "live before the contribution is published", err)

		done, err := ft.q.RecordTerminalContributionPublished(ft.ctx, store.RecordTerminalContributionPublishedParams{ID: c.ID, ExpectedState: "invoked"})
		if err != nil || done.State != "published" || !done.PublishedAt.Valid {
			t.Fatalf("published = %+v err=%v", done, err)
		}
		_, err = ft.q.SettleTerminalPreservationLive(ft.ctx, store.SettleTerminalPreservationLiveParams{ID: p.ID, Tip: termText(termTipA), ExpectedTip: termText(termTipB), NextCheckAt: termIn(time.Hour)})
		termNoRows(t, "live against the wrong old tip", err)
		live, err := ft.q.SettleTerminalPreservationLive(ft.ctx, store.SettleTerminalPreservationLiveParams{ID: p.ID, Tip: termText(termTipA), NextCheckAt: termIn(time.Hour)})
		if err != nil || live.State != "live" || live.Tip.String != termTipA || !live.FirstPublishedAt.Valid || live.LastError.Valid {
			t.Fatalf("live = %+v err=%v", live, err)
		}
		// Settling twice is a lost race.
		_, err = ft.q.SettleTerminalPreservationLive(ft.ctx, store.SettleTerminalPreservationLiveParams{ID: p.ID, Tip: termText(termTipA), ExpectedTip: termText(termTipA), NextCheckAt: termIn(time.Hour)})
		termNoRows(t, "live from live", err)
	})

	t.Run("update path advances the tip once and keeps first_published_at", func(t *testing.T) {
		ft := *f
		ft.t = t
		p, _ := ft.toLive(ft.newRun())
		first := ft.mustPres(p.RunID).FirstPublishedAt
		prior := termTipA
		c2 := ft.insertContribution(p, 2, termTipB, &prior)
		pub, err := ft.q.MarkTerminalPreservationPublishing(ft.ctx, store.MarkTerminalPreservationPublishingParams{ID: p.ID, ExpectedState: "live", ExpectedTip: termText(termTipA)})
		if err != nil || pub.State != "publishing" {
			t.Fatalf("live -> publishing = %+v err=%v", pub, err)
		}
		if _, err = ft.q.MarkTerminalContributionInvoked(ft.ctx, c2.ID); err != nil {
			t.Fatal(err)
		}
		if _, err = ft.q.RecordTerminalContributionPublished(ft.ctx, store.RecordTerminalContributionPublishedParams{ID: c2.ID, ExpectedState: "invoked"}); err != nil {
			t.Fatal(err)
		}
		live, err := ft.q.SettleTerminalPreservationLive(ft.ctx, store.SettleTerminalPreservationLiveParams{ID: p.ID, Tip: termText(termTipB), ExpectedTip: termText(termTipA), NextCheckAt: termIn(time.Hour)})
		if err != nil || live.Tip.String != termTipB || live.FirstPublishedAt.Time != first.Time {
			t.Fatalf("live = %+v err=%v (first_published_at %v)", live, err, first)
		}
	})

	t.Run("refused: back to live with a tip, to not_published without one", func(t *testing.T) {
		ft := *f
		ft.t = t
		// Without a tip.
		p := ft.insertPreservation(ft.newRun())
		c := ft.insertContribution(p, 1, termTipA, nil)
		_, err := ft.q.MarkTerminalPreservationPublishing(ft.ctx, store.MarkTerminalPreservationPublishingParams{ID: p.ID, ExpectedState: "bound"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = ft.q.MarkTerminalContributionInvoked(ft.ctx, c.ID); err != nil {
			t.Fatal(err)
		}
		// The preservation cannot be released while the contribution may have created the ref.
		_, err = ft.q.SettleTerminalPreservationRefused(ft.ctx, store.SettleTerminalPreservationRefusedParams{ID: p.ID, NextCheckAt: termIn(time.Hour)})
		termNoRows(t, "release while the contribution is invoked", err)
		_, err = ft.q.RecordTerminalContributionRefused(ft.ctx, store.RecordTerminalContributionRefusedParams{ID: c.ID, ExpectedState: "invoked"})
		if err == nil {
			t.Fatal("a refusal without a reason must fail the CHECK")
		}
		ref, err := ft.q.RecordTerminalContributionRefused(ft.ctx, store.RecordTerminalContributionRefusedParams{ID: c.ID, ExpectedState: "invoked", RefusalReason: termText("create_refused")})
		if err != nil || ref.State != "refused" || ref.RefusalReason.String != "create_refused" {
			t.Fatalf("refused = %+v err=%v", ref, err)
		}
		_, err = ft.q.SettleTerminalPreservationRefused(ft.ctx, store.SettleTerminalPreservationRefusedParams{ID: p.ID, ExpectedTip: termText(termTipC), NextCheckAt: termIn(time.Hour)})
		termNoRows(t, "release against the wrong tip", err)
		np, err := ft.q.SettleTerminalPreservationRefused(ft.ctx, store.SettleTerminalPreservationRefusedParams{ID: p.ID, NextCheckAt: termIn(time.Hour), LastError: termText("ng")})
		if err != nil || np.State != "not_published" || np.LiveRunID.Valid || np.LastError.String != "ng" {
			t.Fatalf("not_published = %+v err=%v", np, err)
		}

		// With a tip.
		lp, _ := ft.toLive(ft.newRun())
		prior := termTipA
		c2 := ft.insertContribution(lp, 2, termTipB, &prior)
		if _, err = ft.q.MarkTerminalPreservationPublishing(ft.ctx, store.MarkTerminalPreservationPublishingParams{ID: lp.ID, ExpectedState: "live", ExpectedTip: termText(termTipA)}); err != nil {
			t.Fatal(err)
		}
		if _, err = ft.q.MarkTerminalContributionInvoked(ft.ctx, c2.ID); err != nil {
			t.Fatal(err)
		}
		if _, err = ft.q.RecordTerminalContributionRefused(ft.ctx, store.RecordTerminalContributionRefusedParams{ID: c2.ID, ExpectedState: "invoked", RefusalReason: termText("update_refused")}); err != nil {
			t.Fatal(err)
		}
		back, err := ft.q.SettleTerminalPreservationRefused(ft.ctx, store.SettleTerminalPreservationRefusedParams{ID: lp.ID, ExpectedTip: termText(termTipA), NextCheckAt: termIn(time.Hour)})
		if err != nil || back.State != "live" || back.Tip.String != termTipA || !back.LiveRunID.Valid {
			t.Fatalf("back to live = %+v err=%v", back, err)
		}
	})

	t.Run("unknown then reconciled", func(t *testing.T) {
		ft := *f
		ft.t = t
		p := ft.insertPreservation(ft.newRun())
		c := ft.insertContribution(p, 1, termTipA, nil)
		if _, err := ft.q.MarkTerminalPreservationPublishing(ft.ctx, store.MarkTerminalPreservationPublishingParams{ID: p.ID, ExpectedState: "bound"}); err != nil {
			t.Fatal(err)
		}
		if _, err := ft.q.MarkTerminalContributionInvoked(ft.ctx, c.ID); err != nil {
			t.Fatal(err)
		}
		u, err := ft.q.RecordTerminalContributionUnknown(ft.ctx, c.ID)
		if err != nil || u.State != "publish_unknown" {
			t.Fatalf("unknown = %+v err=%v", u, err)
		}
		_, err = ft.q.RecordTerminalContributionUnknown(ft.ctx, c.ID)
		termNoRows(t, "unknown from publish_unknown", err)
		n, err := ft.q.ScheduleTerminalPreservationCheck(ft.ctx, store.ScheduleTerminalPreservationCheckParams{ID: p.ID, NextCheckAt: termIn(time.Hour), LastError: termText("timeout")})
		if err != nil || n != 1 {
			t.Fatalf("schedule rows=%d err=%v", n, err)
		}
		if _, err := ft.q.RecordTerminalContributionPublished(ft.ctx, store.RecordTerminalContributionPublishedParams{ID: c.ID, ExpectedState: "publish_unknown"}); err != nil {
			t.Fatalf("publish_unknown -> published: %v", err)
		}
	})

	t.Run("bound -> not_published, and only while bound with no tip", func(t *testing.T) {
		ft := *f
		ft.t = t
		p := ft.insertPreservation(ft.newRun())
		got, err := ft.q.MarkTerminalPreservationNotPublished(ft.ctx, p.ID)
		if err != nil || got.State != "not_published" {
			t.Fatalf("not_published = %+v err=%v", got, err)
		}
		_, err = ft.q.MarkTerminalPreservationNotPublished(ft.ctx, p.ID)
		termNoRows(t, "not_published twice", err)
		locked, err := ft.q.GetTerminalPreservationForUpdate(ft.ctx, p.RunID)
		if err != nil || locked.ID != p.ID {
			t.Fatalf("locked read = %+v err=%v", locked, err)
		}
	})
}

func (f *termFixture) mustPres(run uuid.UUID) store.RunTerminalPreservation {
	f.t.Helper()
	p, err := f.q.GetTerminalPreservationByRun(f.ctx, run)
	if err != nil {
		f.t.Fatal(err)
	}
	return p
}

// A refused contribution always carries a reason, and a reason belongs to a refused one (I-1).
func TestTerminalContributionRefusalBiconditionalLiveDB(t *testing.T) {
	f := newTermFixture(t, "term_refbic_")
	p := f.insertPreservation(f.newRun())
	c := f.insertContribution(p, 1, termTipA, nil)
	f.mustRows(1, `UPDATE run_terminal_preservations SET state='publishing', live_run_id=run_id WHERE id=$1`, p.ID)
	f.mustRows(1, `UPDATE run_terminal_contributions SET state='invoked', create_invoked_at=now() WHERE id=$1`, c.ID)
	_, err := f.rows(`UPDATE run_terminal_contributions SET state='refused' WHERE id=$1`, c.ID)
	heldCheckName(t, err, "run_terminal_contributions_refusal_state_check")
	f.mustRows(1, `UPDATE run_terminal_contributions SET state='refused', refusal_reason='update_refused' WHERE id=$1`, c.ID)
}

// N1: a contribution may be marked invoked only while its preservation is publishing, and the check
// serialises against a concurrent preservation -> not_published in BOTH orders. Two sessions; the
// second statement blocks on the first's uncommitted row lock.
func TestTerminalInvokeVersusNotPublishedLiveDB(t *testing.T) {
	f := newTermFixture(t, "term_n1_")

	// publishingWithPrepared: a publishing preservation whose first contribution was refused, plus a
	// fresh prepared contribution (so the preservation could legally go back to not_published).
	setup := func(ft *termFixture) (store.RunTerminalPreservation, store.RunTerminalContribution) {
		p := ft.insertPreservation(ft.newRun())
		c1 := ft.insertContribution(p, 1, termTipA, nil)
		ft.mustRows(1, `UPDATE run_terminal_preservations SET state='publishing', live_run_id=run_id WHERE id=$1`, p.ID)
		ft.mustRows(1, `UPDATE run_terminal_contributions SET state='invoked', create_invoked_at=now() WHERE id=$1`, c1.ID)
		ft.mustRows(1, `UPDATE run_terminal_contributions SET state='refused', refusal_reason='create_refused' WHERE id=$1`, c1.ID)
		return p, ft.insertContribution(p, 2, termTipB, nil)
	}
	// waitBlocked polls until a session is waiting on a lock, so the interleaving is real.
	waitBlocked := func(ft *termFixture) {
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			var n int
			if err := ft.pool.QueryRow(ft.ctx, `SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND datname = current_database()`).Scan(&n); err != nil {
				ft.t.Fatal(err)
			}
			if n > 0 {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		ft.t.Fatal("the second session never blocked on the first's lock")
	}

	t.Run("invoke commits first: not_published is refused", func(t *testing.T) {
		ft := *f
		ft.t = t
		p, c2 := setup(&ft)
		a, err := ft.pool.Begin(ft.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = a.Rollback(ft.ctx) }()
		tag, err := a.Exec(ft.ctx, `UPDATE run_terminal_contributions SET state='invoked', create_invoked_at=now() WHERE id=$1`, c2.ID)
		if err != nil || tag.RowsAffected() != 1 {
			t.Fatalf("invoke rows=%d err=%v", tag.RowsAffected(), err)
		}
		type result struct {
			rows int64
			err  error
		}
		out := make(chan result, 1)
		go func() {
			n, err := ft.rows(`UPDATE run_terminal_preservations SET state='not_published', live_run_id=NULL WHERE id=$1`, p.ID)
			out <- result{n, err}
		}()
		waitBlocked(&ft)
		if err := a.Commit(ft.ctx); err != nil {
			t.Fatal(err)
		}
		r := <-out
		if r.err != nil || r.rows != 0 {
			t.Fatalf("not_published after a committed invoke: rows=%d err=%v, want 0 rows", r.rows, r.err)
		}
		if got := ft.mustPres(p.RunID); got.State != "publishing" {
			t.Fatalf("the preservation must still be publishing, got %s", got.State)
		}
	})

	t.Run("not_published commits first: the invoke is refused", func(t *testing.T) {
		ft := *f
		ft.t = t
		p, c2 := setup(&ft)
		b, err := ft.pool.Begin(ft.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = b.Rollback(ft.ctx) }()
		tag, err := b.Exec(ft.ctx, `UPDATE run_terminal_preservations SET state='not_published', live_run_id=NULL WHERE id=$1`, p.ID)
		if err != nil || tag.RowsAffected() != 1 {
			t.Fatalf("not_published rows=%d err=%v", tag.RowsAffected(), err)
		}
		type result struct {
			rows int64
			err  error
		}
		out := make(chan result, 1)
		go func() {
			n, err := ft.rows(`UPDATE run_terminal_contributions SET state='invoked', create_invoked_at=now() WHERE id=$1`, c2.ID)
			out <- result{n, err}
		}()
		waitBlocked(&ft)
		if err := b.Commit(ft.ctx); err != nil {
			t.Fatal(err)
		}
		r := <-out
		if r.err != nil || r.rows != 0 {
			t.Fatalf("invoke after a committed not_published: rows=%d err=%v, want 0 rows", r.rows, r.err)
		}
		var state string
		if err := ft.pool.QueryRow(ft.ctx, `SELECT state FROM run_terminal_contributions WHERE id=$1`, c2.ID).Scan(&state); err != nil || state != "prepared" {
			t.Fatalf("the contribution must still be prepared, got %q err=%v", state, err)
		}
	})

	t.Run("a bound preservation refuses the invoke", func(t *testing.T) {
		ft := *f
		ft.t = t
		p := ft.insertPreservation(ft.newRun())
		c := ft.insertContribution(p, 1, termTipA, nil)
		ft.mustRows(0, `UPDATE run_terminal_contributions SET state='invoked', create_invoked_at=now() WHERE id=$1`, c.ID)
	})
}
