package workersvc

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4007 slice A, M4: the terminal-preservation sweep against a REAL Postgres, with the broker
// (listing, delete) replaced by the counting fake of terminal_preservation_livedb_test.go and the
// forge reads by its recording fake. Skipped unless UZI_TEST_DATABASE_URL points at a throwaway
// Postgres (./e2e/run-store-it.sh).

// sweepFix is a termFix whose run can be made terminal, with a counting delete seam and a counting
// SSRF gate (heldRemote calls it first, so its count is the count of credential resolutions).
type sweepFix struct {
	*termFix
	mu      sync.Mutex
	allowed int
	deletes []pushbroker.DeleteTerminalOptions
	delOut  func(o pushbroker.DeleteTerminalOptions) (pushbroker.TerminalDeleteOutcome, error)
}

func newSweepFix(t *testing.T) *sweepFix {
	t.Helper()
	f := &sweepFix{termFix: newTermFix(t, "flight")}
	f.svc.SetForgeBaseURLAllowed(func(u string) bool {
		f.mu.Lock()
		f.allowed++
		f.mu.Unlock()
		return u == "https://forge.e2e"
	})
	f.svc.terminal.del = func(_ context.Context, o pushbroker.DeleteTerminalOptions) (pushbroker.TerminalDeleteOutcome, error) {
		f.mu.Lock()
		f.deletes = append(f.deletes, o)
		fn := f.delOut
		f.mu.Unlock()
		if fn != nil {
			return fn(o)
		}
		// The default remote: CAS-delete at the expected tip.
		f.fake.mu.Lock()
		defer f.fake.mu.Unlock()
		cur, ok := f.fake.refs[o.RunID]
		switch {
		case !ok:
			return pushbroker.TerminalDeleteGone, nil
		case cur != o.ExpectedTip:
			return pushbroker.TerminalDeleteMoved, pushbroker.ErrTerminalRefMoved
		}
		delete(f.fake.refs, o.RunID)
		return pushbroker.TerminalDeleteGone, nil
	}
	// Rows of other fixtures in the shared database are none of this test's business: push them out
	// of the due window so the scan limit and the empty-pass assertions see only this fixture.
	f.isolate()
	return f
}

// isolate pushes every other owner's preservation rows out of the due window.
func (f *sweepFix) isolate() {
	f.t.Helper()
	f.e.exec(f.t, `UPDATE run_terminal_preservations SET next_check_at = now() + interval '30 days'
		WHERE user_id <> $1 AND next_check_at <= now()`, f.e.userID)
}

// publishGen2 publishes an update from generation 1's tip as the run's second flight.
func (f *sweepFix) publishGen2() (TerminalPublishResult, error) {
	f.claimGeneration(2)
	return f.publish(f.req(2, termTip2, termTip1))
}

func (f *sweepFix) allowCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.allowed
}

func (f *sweepFix) deleteCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.deletes)
}

func (f *sweepFix) forgeCalls() int {
	f.forges.mu.Lock()
	defer f.forges.mu.Unlock()
	return f.forges.issueCalls + f.forges.hostCalls + f.forges.fallback + f.forges.getIssues + f.forges.getMRs
}

func (f *sweepFix) sweep() int64 {
	f.t.Helper()
	n, err := f.svc.SweepTerminalPreservations(f.e.ctx)
	if err != nil {
		f.t.Fatalf("SweepTerminalPreservations: %v", err)
	}
	return n
}

// sweepErr runs a pass and returns its error (a crash hook makes it fail on purpose).
func (f *sweepFix) sweepErr() error {
	_, err := f.svc.SweepTerminalPreservations(f.e.ctx)
	return err
}

// shiftClock moves the service clock forward, for the grace windows that compare against it.
func (f *sweepFix) shiftClock(d time.Duration) {
	f.svc.now = func() time.Time { return time.Now().Add(d) }
}

// resetCounts forgets the credential resolutions and forge reads of the setup, so an assertion
// counts only the sweep's.
func (f *sweepFix) resetCounts() {
	f.mu.Lock()
	f.allowed = 0
	f.mu.Unlock()
	f.forges.mu.Lock()
	f.forges.issueCalls, f.forges.hostCalls, f.forges.fallback, f.forges.getIssues, f.forges.getMRs = 0, 0, 0, 0, 0
	f.forges.mu.Unlock()
}

// claimGeneration moves the run's claim to generation g, as a later flight of the run would.
func (f *sweepFix) claimGeneration(g int64) {
	f.t.Helper()
	f.e.exec(f.t, `UPDATE runs SET claim_generation=$2 WHERE id=$1`, f.run, g)
}

// due makes the preservation's next check come now.
func (f *sweepFix) due() {
	f.t.Helper()
	f.e.exec(f.t, `UPDATE run_terminal_preservations SET next_check_at = now() - interval '1 minute' WHERE run_id=$1`, f.run)
}

// terminal turns the run into a finished one with its claim released, the shape a retire needs.
func (f *sweepFix) terminal() {
	f.t.Helper()
	f.e.exec(f.t, `UPDATE runs SET status='completed', claim_released_at=now() WHERE id=$1`, f.run)
}

// live publishes generation 1 at termTip1 while the run is still its worker's flight, then finishes
// the run and makes the row due.
func (f *sweepFix) live() {
	f.t.Helper()
	res, err := f.publish(f.req(1, termTip1, ""))
	if err != nil || res.PreservationState != "live" {
		f.t.Fatalf("publish = %+v, %v", res, err)
	}
	f.terminal()
	f.due()
	f.resetCounts()
}

// publishing leaves a publishing preservation whose generation-1 contribution is in want (invoked
// or publish_unknown), with the remote as left by the send.
func (f *sweepFix) publishing(want string) {
	f.t.Helper()
	switch want {
	case "invoked":
		// tx2 fails, so the marker stays invoked: the shape of a request that died after its send.
		f.svc.terminal.outcome = func() error { return errors.New("tx2 injected failure") }
		res, err := f.publish(f.req(1, termTip1, ""))
		f.svc.terminal.outcome = nil
		if err != nil || res.State != "invoked" {
			f.t.Fatalf("publish = %+v, %v", res, err)
		}
	case "publish_unknown":
		f.fake.createFn = func(context.Context, pushbroker.TerminalPackOptions) (pushbroker.TerminalOutcome, error) {
			return pushbroker.TerminalUnknown, errors.New("push timed out")
		}
		res, err := f.publish(f.req(1, termTip1, ""))
		if err != nil || res.State != "publish_unknown" {
			f.t.Fatalf("publish = %+v, %v", res, err)
		}
	default:
		f.t.Fatalf("publishing(%q)", want)
	}
	f.due()
	f.resetCounts()
}

// snapshot is every preservation and contribution row of the fixture's owner, plus every held
// publication row in the database, as text, so a pass that must change nothing can be compared.
func (f *sweepFix) snapshot() string {
	f.t.Helper()
	var s string
	err := f.e.pool.QueryRow(f.e.ctx, `SELECT
		coalesce((SELECT string_agg(row_to_json(p)::text, '|' ORDER BY p.id) FROM run_terminal_preservations p WHERE p.user_id = $1), '') || '#' ||
		coalesce((SELECT string_agg(row_to_json(c)::text, '|' ORDER BY c.id) FROM run_terminal_contributions c WHERE c.user_id = $1), '') || '#' ||
		coalesce((SELECT string_agg(row_to_json(h)::text, '|' ORDER BY h.id) FROM run_held_publications h), '')`, f.e.userID).Scan(&s)
	if err != nil {
		f.t.Fatal(err)
	}
	return s
}

func (f *sweepFix) mustPres(state string) store.RunTerminalPreservation {
	f.t.Helper()
	p, ok := f.pres()
	if !ok || p.State != state {
		f.t.Fatalf("preservation = %+v (found %v), want state %q", p, ok, state)
	}
	return p
}

func (f *sweepFix) mustCon(gen int64, state string) store.RunTerminalContribution {
	f.t.Helper()
	c, ok := f.con(gen)
	if !ok || c.State != state {
		f.t.Fatalf("contribution %d = %+v (found %v), want state %q", gen, c, ok, state)
	}
	return c
}

// dbNow is the database clock.
func (f *sweepFix) dbNow() time.Time {
	f.t.Helper()
	var n time.Time
	if err := f.e.pool.QueryRow(f.e.ctx, `SELECT now()`).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

// An empty sweep is effect-inert: no credential resolution, no forge or broker call, no row
// written, whether the table has no rows of the owner or only rows that are not due or are finished.
func TestTerminalSweepEmptyIsInertLiveDB(t *testing.T) {
	f := newSweepFix(t)
	before := f.snapshot()
	if n := f.sweep(); n != 0 {
		t.Fatalf("empty sweep changed %d rows", n)
	}
	if f.snapshot() != before || f.allowCount() != 0 || f.forgeCalls() != 0 || f.deleteCount() != 0 {
		t.Fatalf("empty sweep had an effect: allowed=%d forgeCalls=%d deletes=%d", f.allowCount(), f.forgeCalls(), f.deleteCount())
	}
	if pre, sends, lists := f.fake.counts(); pre+sends+lists != 0 {
		t.Fatalf("broker calls: prepares=%d sends=%d lists=%d", pre, sends, lists)
	}

	// A live row whose next check is in the future, and a finished (not_published) row: not due.
	res, err := f.publish(f.req(1, termTip1, ""))
	if err != nil || res.PreservationState != "live" {
		t.Fatalf("publish = %+v, %v", res, err)
	}
	other := f.e.seedLegacyRunningRun(t, f.worker.ID)
	pre, err := f.e.q.InsertTerminalPreservation(f.e.ctx, store.InsertTerminalPreservationParams{
		RunID: other, UserID: f.e.userID, RepoID: f.e.repoID, Ref: pushbroker.TerminalRef(other), LifetimeAtBind: "issue",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.e.q.MarkTerminalPreservationNotPublished(f.e.ctx, pre.ID); err != nil {
		t.Fatal(err)
	}
	f.fake.mu.Lock()
	f.fake.lists, f.fake.prepares, f.fake.creates = 0, 0, 0
	f.fake.mu.Unlock()
	f.mu.Lock()
	f.allowed = 0
	f.mu.Unlock()
	f.forges.mu.Lock()
	f.forges.issueCalls, f.forges.getIssues = 0, 0
	f.forges.mu.Unlock()

	before = f.snapshot()
	if n := f.sweep(); n != 0 {
		t.Fatalf("sweep with nothing due changed %d rows", n)
	}
	if f.snapshot() != before {
		t.Fatal("sweep with nothing due wrote a row")
	}
	if p, s, l := f.fake.counts(); p+s+l != 0 || f.allowCount() != 0 || f.forgeCalls() != 0 || f.deleteCount() != 0 {
		t.Fatalf("sweep with nothing due had an effect: prepares=%d sends=%d lists=%d allowed=%d forgeCalls=%d deletes=%d",
			p, s, l, f.allowCount(), f.forgeCalls(), f.deleteCount())
	}
}

// The reconcile lane lists the ref and never sends.
func TestTerminalSweepReconcileLiveDB(t *testing.T) {
	t.Run("invoked, ref at the tip, is published and live", func(t *testing.T) {
		f := newSweepFix(t)
		f.publishing("invoked")
		f.shiftClock(terminalReconcileGrace + time.Minute)
		_, sendsBefore, listsBefore := f.fake.counts()
		if n := f.sweep(); n != 1 {
			t.Fatalf("changed %d rows, want 1", n)
		}
		p := f.mustPres("live")
		c := f.mustCon(1, "published")
		if p.Tip.String != termTip1 || !p.FirstPublishedAt.Valid || !p.LiveRunID.Valid || c.Tip != termTip1 {
			t.Fatalf("preservation = %+v contribution = %+v", p, c)
		}
		if _, sends, lists := f.fake.counts(); sends != sendsBefore || lists != listsBefore+1 {
			t.Fatalf("sends %d->%d lists %d->%d: the reconcile must list once and never send", sendsBefore, sends, listsBefore, lists)
		}
		if f.deleteCount() != 0 {
			t.Fatal("reconcile deleted")
		}
	})

	t.Run("invoked inside the grace is left to its own request", func(t *testing.T) {
		f := newSweepFix(t)
		f.publishing("invoked")
		before := f.mustPres("publishing")
		if n := f.sweep(); n != 0 {
			t.Fatalf("changed %d rows", n)
		}
		after := f.mustPres("publishing")
		f.mustCon(1, "invoked")
		if _, _, lists := f.fake.counts(); lists != 0 || f.allowCount() != 0 {
			t.Fatalf("listed %d times inside the grace (allowed=%d)", lists, f.allowCount())
		}
		if !after.NextCheckAt.Time.After(f.dbNow().Add(5*time.Minute)) || after.Attempts != before.Attempts {
			t.Fatalf("next_check_at = %v attempts = %d, want parked to the end of the grace without an attempt", after.NextCheckAt.Time, after.Attempts)
		}
	})

	t.Run("invoked, ref absent, becomes publish_unknown with backoff and no send", func(t *testing.T) {
		f := newSweepFix(t)
		f.publishing("invoked")
		f.fake.mu.Lock()
		delete(f.fake.refs, f.run)
		f.fake.mu.Unlock()
		f.shiftClock(terminalReconcileGrace + time.Minute)
		_, sendsBefore, _ := f.fake.counts()
		if n := f.sweep(); n != 1 {
			t.Fatalf("changed %d rows, want 1 (invoked -> publish_unknown)", n)
		}
		p := f.mustPres("publishing")
		f.mustCon(1, "publish_unknown")
		if p.Attempts != 1 || !p.LastError.Valid || !p.NextCheckAt.Time.After(f.dbNow().Add(30*time.Second)) {
			t.Fatalf("preservation = %+v, want one attempt, a recorded reason and a backoff", p)
		}
		if _, sends, _ := f.fake.counts(); sends != sendsBefore {
			t.Fatalf("sends %d -> %d: the reconcile must never send", sendsBefore, sends)
		}
	})

	t.Run("publish_unknown create, ref absent, stays unknown and backs off exponentially", func(t *testing.T) {
		f := newSweepFix(t)
		f.publishing("publish_unknown")
		_, sendsBefore, _ := f.fake.counts()
		if n := f.sweep(); n != 0 {
			t.Fatalf("changed %d rows, want 0", n)
		}
		p1 := f.mustPres("publishing")
		f.mustCon(1, "publish_unknown")
		f.due()
		f.sweep()
		p2 := f.mustPres("publishing")
		if p1.Attempts != 1 || p2.Attempts != 2 {
			t.Fatalf("attempts = %d then %d, want 1 then 2", p1.Attempts, p2.Attempts)
		}
		if !p2.NextCheckAt.Time.After(p1.NextCheckAt.Time.Add(30 * time.Second)) {
			t.Fatalf("next_check_at %v then %v: the second backoff must be longer", p1.NextCheckAt.Time, p2.NextCheckAt.Time)
		}
		if _, sends, lists := f.fake.counts(); sends != sendsBefore || lists != 2 {
			t.Fatalf("sends %d -> %d, lists = %d: the reconcile must list each time and never send", sendsBefore, sends, lists)
		}
	})

	t.Run("update, ref still at the prior tip, stays publish_unknown", func(t *testing.T) {
		f := newSweepFix(t)
		if res, err := f.publish(f.req(1, termTip1, "")); err != nil || res.PreservationState != "live" {
			t.Fatalf("publish gen 1 = %+v, %v", res, err)
		}
		f.fake.updateFn = func(context.Context, pushbroker.TerminalPackOptions) (pushbroker.TerminalOutcome, error) {
			return pushbroker.TerminalUnknown, errors.New("push timed out")
		}
		if res, err := f.publishGen2(); err != nil || res.State != "publish_unknown" {
			t.Fatalf("publish gen 2 = %+v, %v", res, err)
		}
		f.due()
		if n := f.sweep(); n != 0 {
			t.Fatalf("changed %d rows, want 0", n)
		}
		p := f.mustPres("publishing")
		f.mustCon(2, "publish_unknown")
		if p.Tip.String != termTip1 || p.Attempts != 1 {
			t.Fatalf("preservation = %+v, want the old tip kept and one attempt", p)
		}
		if _, sends, _ := f.fake.counts(); sends != 2 {
			t.Fatalf("sends = %d, want the 2 of the route only", sends)
		}
	})

	t.Run("update, ref at the new tip, is published", func(t *testing.T) {
		f := newSweepFix(t)
		if res, err := f.publish(f.req(1, termTip1, "")); err != nil || res.PreservationState != "live" {
			t.Fatalf("publish gen 1 = %+v, %v", res, err)
		}
		f.fake.updateFn = func(_ context.Context, o pushbroker.TerminalPackOptions) (pushbroker.TerminalOutcome, error) {
			f.fake.setRef(o.RunID, o.Tip) // the push landed but the reply was lost
			return pushbroker.TerminalUnknown, errors.New("reply lost")
		}
		if res, err := f.publishGen2(); err != nil || res.State != "publish_unknown" {
			t.Fatalf("publish gen 2 = %+v, %v", res, err)
		}
		f.due()
		if n := f.sweep(); n != 1 {
			t.Fatalf("changed %d rows, want 1", n)
		}
		if p := f.mustPres("live"); p.Tip.String != termTip2 {
			t.Fatalf("tip = %q, want %q", p.Tip.String, termTip2)
		}
		f.mustCon(2, "published")
	})

	t.Run("a foreign tip refuses the contribution, abandons the preservation and never touches the ref", func(t *testing.T) {
		f := newSweepFix(t)
		f.publishing("publish_unknown")
		f.fake.setRef(f.run, termTip3)
		_, sendsBefore, _ := f.fake.counts()
		if n := f.sweep(); n != 1 {
			t.Fatalf("changed %d rows, want 1", n)
		}
		p := f.mustPres("abandoned")
		c := f.mustCon(1, "refused")
		if c.RefusalReason.String != "foreign_tip" || p.LiveRunID.Valid {
			t.Fatalf("contribution = %+v preservation = %+v", c, p)
		}
		if f.deleteCount() != 0 {
			t.Fatal("a foreign ref was deleted")
		}
		if _, sends, _ := f.fake.counts(); sends != sendsBefore {
			t.Fatalf("sends %d -> %d", sendsBefore, sends)
		}
		f.fake.mu.Lock()
		got := f.fake.refs[f.run]
		f.fake.mu.Unlock()
		if got != termTip3 {
			t.Fatalf("the foreign ref was changed to %q", got)
		}
		// A finished row is not due again.
		if _, _, lists := f.fake.counts(); lists != 1 {
			t.Fatalf("lists = %d", lists)
		}
		f.due()
		before := f.snapshot()
		f.sweep()
		if f.snapshot() != before {
			t.Fatal("an abandoned row was touched by a later pass")
		}
	})

	t.Run("a listing error backs off and changes no state", func(t *testing.T) {
		f := newSweepFix(t)
		f.publishing("publish_unknown")
		f.fake.listErr = errors.New("forge unreachable")
		beforeCon := f.mustCon(1, "publish_unknown")
		if n := f.sweep(); n != 0 {
			t.Fatalf("changed %d rows", n)
		}
		p := f.mustPres("publishing")
		c := f.mustCon(1, "publish_unknown")
		if p.Attempts != 1 || !strings.Contains(p.LastError.String, "forge unreachable") || !p.NextCheckAt.Time.After(f.dbNow()) {
			t.Fatalf("preservation = %+v, want one attempt, the error text and a backoff", p)
		}
		if c.UpdatedAt != beforeCon.UpdatedAt {
			t.Fatal("the contribution was written")
		}
	})
}

// A bound row with no invoked contribution, after the grace, never had a ref.
func TestTerminalSweepStaleBoundLiveDB(t *testing.T) {
	bind := func(f *sweepFix) store.RunTerminalPreservation {
		p, err := f.e.q.InsertTerminalPreservation(f.e.ctx, store.InsertTerminalPreservationParams{
			RunID: f.run, UserID: f.e.userID, RepoID: f.e.repoID, Ref: pushbroker.TerminalRef(f.run), LifetimeAtBind: "issue",
		})
		if err != nil {
			f.t.Fatal(err)
		}
		return p
	}
	t.Run("inside the grace the row waits", func(t *testing.T) {
		f := newSweepFix(t)
		bind(f)
		if n := f.sweep(); n != 0 {
			t.Fatalf("changed %d rows", n)
		}
		p := f.mustPres("bound")
		if !p.NextCheckAt.Time.After(f.dbNow().Add(30 * time.Minute)) {
			t.Fatalf("next_check_at = %v, want parked to the end of the grace", p.NextCheckAt.Time)
		}
	})
	t.Run("past the grace it is not_published", func(t *testing.T) {
		f := newSweepFix(t)
		bind(f)
		f.shiftClock(terminalBoundGrace + time.Minute)
		if n := f.sweep(); n != 1 {
			t.Fatalf("changed %d rows, want 1", n)
		}
		p := f.mustPres("not_published")
		if p.LiveRunID.Valid || p.Tip.Valid {
			t.Fatalf("preservation = %+v", p)
		}
		if f.allowCount() != 0 || f.forgeCalls() != 0 {
			t.Fatal("the stale-bound lane resolved a credential or called the forge")
		}
	})
	t.Run("a prepared contribution does not keep it bound", func(t *testing.T) {
		f := newSweepFix(t)
		p := bind(f)
		if _, err := f.e.q.InsertTerminalContribution(f.e.ctx, store.InsertTerminalContributionParams{
			PreservationID: p.ID, RunID: f.run, Generation: 1, WorkerID: f.worker.ID, UserID: f.e.userID, Authority: "current_flight",
			Tip: termTip1, CoverageDigest: strings.Repeat("d", 64), Manifest: []byte(`{}`), Disposition: "preserved",
		}); err != nil {
			t.Fatal(err)
		}
		f.shiftClock(terminalBoundGrace + time.Minute)
		if n := f.sweep(); n != 1 {
			t.Fatalf("changed %d rows, want 1", n)
		}
		f.mustPres("not_published")
		f.mustCon(1, "prepared")
	})
	t.Run("a busy run is skipped", func(t *testing.T) {
		f := newSweepFix(t)
		bind(f)
		f.shiftClock(terminalBoundGrace + time.Minute)
		tx, err := f.e.pool.Begin(f.e.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(f.e.ctx) }()
		if _, err = tx.Exec(f.e.ctx, `SELECT 1 FROM runs WHERE id=$1 FOR UPDATE`, f.run); err != nil {
			t.Fatal(err)
		}
		if n := f.sweep(); n != 0 {
			t.Fatalf("changed %d rows while the run was locked", n)
		}
		f.mustPres("bound")
		_ = tx.Rollback(f.e.ctx)
		f.due()
		if n := f.sweep(); n != 1 {
			t.Fatalf("changed %d rows once the run was free, want 1", n)
		}
		f.mustPres("not_published")
	})
}

// With retirement disabled (production) a due live row is only pushed out: its lifetime is never
// read, no credential is resolved and DeleteTerminal is never called.
func TestTerminalSweepRetireDisabledByDefaultLiveDB(t *testing.T) {
	f := newSweepFix(t)
	f.live()
	f.forges.issue = forge.Issue{State: "closed"}
	f.forges.mu.Lock()
	f.forges.issueCalls, f.forges.getIssues = 0, 0
	f.forges.mu.Unlock()
	f.mu.Lock()
	f.allowed = 0
	f.mu.Unlock()
	before := f.mustPres("live")
	if terminalRetirementEnabled {
		t.Fatal("terminalRetirementEnabled must be false in slice A")
	}
	if n := f.sweep(); n != 0 {
		t.Fatalf("changed %d rows", n)
	}
	p := f.mustPres("live")
	if p.Tip != before.Tip || p.RetireTip.Valid || f.deleteCount() != 0 || f.forgeCalls() != 0 || f.allowCount() != 0 {
		t.Fatalf("retire ran while disabled: preservation = %+v deletes=%d forgeCalls=%d allowed=%d", p, f.deleteCount(), f.forgeCalls(), f.allowCount())
	}
	if _, _, lists := f.fake.counts(); lists != 0 {
		t.Fatalf("listed %d times while disabled", lists)
	}
	if !p.NextCheckAt.Time.After(f.dbNow().Add(23 * time.Hour)) {
		t.Fatalf("next_check_at = %v, want the row pushed out so it cannot starve the other lanes", p.NextCheckAt.Time)
	}
	// A retiring row cannot exist while disabled; were one present it would not be deleted either.
	f.e.exec(t, `UPDATE run_terminal_preservations SET state='retiring', retire_tip=tip, retire_reason='issue_closed',
		retire_observed_at=now(), retire_decided_at=now(), next_check_at=now() - interval '1 minute' WHERE run_id=$1`, f.run)
	f.sweep()
	f.mustPres("retiring")
	if f.deleteCount() != 0 {
		t.Fatal("DeleteTerminal was called while retirement was disabled")
	}
}

func (f *sweepFix) enableRetire() { f.svc.terminal.retireEnabled = true }

// retireStamp asserts the row carries a complete retire stamp.
func retireStamp(t *testing.T, p store.RunTerminalPreservation) {
	t.Helper()
	if !p.RetireTip.Valid || !p.RetireReason.Valid || !p.RetireObservedAt.Valid || !p.RetireDecidedAt.Valid {
		t.Fatalf("incomplete retire stamp: %+v", p)
	}
	if p.RetireDecidedAt.Time.Before(p.RetireObservedAt.Time) {
		t.Fatalf("decided %v before observed %v", p.RetireDecidedAt.Time, p.RetireObservedAt.Time)
	}
}

// The retire lane, enabled by the test override.
func TestTerminalSweepRetireLiveDB(t *testing.T) {
	t.Run("a proven-closed issue retires the ref and deletes the row's pointer", func(t *testing.T) {
		f := newSweepFix(t)
		f.enableRetire()
		f.live()
		f.forges.issue = forge.Issue{State: "closed"}
		if n := f.sweep(); n != 2 {
			t.Fatalf("changed %d rows, want 2 (retiring, deleted)", n)
		}
		p := f.mustPres("deleted")
		retireStamp(t, p)
		if p.RetireTip.String != termTip1 || p.RetireReason.String != "issue_closed" || p.LiveRunID.Valid || !p.DeletedAt.Valid {
			t.Fatalf("preservation = %+v", p)
		}
		if f.deleteCount() != 1 || f.deletes[0].ExpectedTip != termTip1 || f.deletes[0].RunID != f.run {
			t.Fatalf("deletes = %+v, want one CAS delete at %s", f.deletes, termTip1)
		}
		f.fake.mu.Lock()
		_, present := f.fake.refs[f.run]
		f.fake.mu.Unlock()
		if present {
			t.Fatal("the ref is still on the remote")
		}
	})

	t.Run("a closed merge request retires with pr_closed", func(t *testing.T) {
		f := newSweepFix(t)
		f.enableRetire()
		f.issueless(5)
		f.live()
		f.forges.mr = forge.MergeRequestSummary{State: forge.MRStateMerged}
		f.sweep()
		if p := f.mustPres("deleted"); p.RetireReason.String != "pr_closed" {
			t.Fatalf("retire_reason = %q", p.RetireReason.String)
		}
	})

	t.Run("a reopen visible at the observation keeps the ref", func(t *testing.T) {
		f := newSweepFix(t)
		f.enableRetire()
		f.forges.issue = forge.Issue{State: "opened"}
		f.live()
		if n := f.sweep(); n != 0 {
			t.Fatalf("changed %d rows", n)
		}
		p := f.mustPres("live")
		if f.deleteCount() != 0 || !p.NextCheckAt.Time.After(f.dbNow().Add(23*time.Hour)) {
			t.Fatalf("deletes=%d next_check_at=%v", f.deleteCount(), p.NextCheckAt.Time)
		}
	})

	t.Run("a reopen after the forge answer but before the decision still retires (documented)", func(t *testing.T) {
		f := newSweepFix(t)
		f.enableRetire()
		f.live()
		f.forges.issue = forge.Issue{State: "closed"}
		f.svc.terminal.afterObserve = func(*time.Time) {
			f.forges.mu.Lock()
			f.forges.issue = forge.Issue{State: "opened"}
			f.forges.mu.Unlock()
		}
		f.sweep()
		f.mustPres("deleted")
		if f.deleteCount() != 1 {
			t.Fatalf("deletes = %d, want 1", f.deleteCount())
		}
	})

	t.Run("an observation older than the maximum age is discarded without a write and re-observed", func(t *testing.T) {
		f := newSweepFix(t)
		f.enableRetire()
		f.live()
		f.forges.issue = forge.Issue{State: "closed"}
		f.svc.terminal.afterObserve = func(at *time.Time) { *at = at.Add(-(retireObservationMaxAge + time.Minute)) }
		before := f.snapshot()
		if n := f.sweep(); n != 0 {
			t.Fatalf("changed %d rows", n)
		}
		if f.snapshot() != before || f.deleteCount() != 0 {
			t.Fatalf("a stale observation wrote or deleted: deletes=%d", f.deleteCount())
		}
		observed := f.forges.getIssues
		f.svc.terminal.afterObserve = nil
		f.sweep()
		f.mustPres("deleted")
		if f.forges.getIssues != observed+1 {
			t.Fatalf("issue reads %d -> %d, want exactly one fresh observation", observed, f.forges.getIssues)
		}
	})

	t.Run("a row that left live before the decision is not retired", func(t *testing.T) {
		f := newSweepFix(t)
		f.enableRetire()
		f.live()
		f.forges.issue = forge.Issue{State: "closed"}
		f.svc.terminal.afterObserve = func(*time.Time) {
			// A concurrent update is simulated by taking the row out of live.
			f.e.exec(t, `UPDATE run_terminal_preservations SET state='publishing', live_run_id=run_id WHERE run_id=$1`, f.run)
		}
		f.sweep()
		f.mustPres("publishing")
		if f.deleteCount() != 0 {
			t.Fatal("deleted a row that was no longer live")
		}
	})

	t.Run("a busy run is skipped and decided on a later pass", func(t *testing.T) {
		f := newSweepFix(t)
		f.enableRetire()
		f.live()
		f.forges.issue = forge.Issue{State: "closed"}
		tx, err := f.e.pool.Begin(f.e.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(f.e.ctx) }()
		if _, err = tx.Exec(f.e.ctx, `SELECT 1 FROM runs WHERE id=$1 FOR UPDATE`, f.run); err != nil {
			t.Fatal(err)
		}
		if n := f.sweep(); n != 0 {
			t.Fatalf("changed %d rows while the run was locked", n)
		}
		p := f.mustPres("live")
		if f.deleteCount() != 0 || !p.NextCheckAt.Time.After(f.dbNow()) {
			t.Fatalf("deletes=%d next_check_at=%v", f.deleteCount(), p.NextCheckAt.Time)
		}
		_ = tx.Rollback(f.e.ctx)
		f.due()
		f.sweep()
		f.mustPres("deleted")
	})

	protections := []struct {
		name   string
		reason string
		arm    func(f *sweepFix)
	}{
		{"run not terminal", "run_nonterminal", func(f *sweepFix) {
			f.e.exec(f.t, `UPDATE runs SET status='running', claim_released_at=NULL WHERE id=$1`, f.run)
		}},
		{"claim still live", "claim_live", func(f *sweepFix) {
			f.e.exec(f.t, `INSERT INTO worker_active_runs (worker_id, run_id, claim_generation, phase, snapshot_epoch, reported_at) VALUES ($1, $2, 1, 'running', 1, now())`, f.worker.ID, f.run)
		}},
		{"excluded origin", "excluded_origin", func(f *sweepFix) {
			f.e.exec(f.t, `UPDATE runs SET status='failed', fail_origin='push_secret_blocked' WHERE id=$1`, f.run)
		}},
		{"open custody hold", "custody_hold_open", func(f *sweepFix) { f.openHold() }},
		{"unresolved contribution", "contribution_unresolved", func(f *sweepFix) {
			p := f.mustPres("live")
			if _, err := f.e.q.InsertTerminalContribution(f.e.ctx, store.InsertTerminalContributionParams{
				PreservationID: p.ID, RunID: f.run, Generation: 2, WorkerID: f.worker.ID, UserID: f.e.userID, Authority: "current_flight",
				Tip: termTip2, PriorTip: pgtype.Text{String: termTip1, Valid: true}, CoverageDigest: strings.Repeat("d", 64),
				Manifest: []byte(`{}`), Disposition: "preserved",
			}); err != nil {
				f.t.Fatal(err)
			}
		}},
		{"resumed by an active run", "dependent_run_active", func(f *sweepFix) {
			f.e.exec(f.t, `INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, kind, status, resume_of_run_id)
				VALUES ($1, $2, $3, 9001, 't', 'd', 'issue', 'queued', $4)`, uuid.New(), f.e.userID, f.e.repoID, f.run)
		}},
		{"another run of the same issue", "cross_run_consumer", func(f *sweepFix) {
			var iid int64
			if err := f.e.pool.QueryRow(f.e.ctx, `SELECT issue_iid FROM runs WHERE id=$1`, f.run).Scan(&iid); err != nil {
				f.t.Fatal(err)
			}
			f.e.exec(f.t, `INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, kind, status)
				VALUES ($1, $2, $3, $4, 't', 'd', 'issue', 'queued')`, uuid.New(), f.e.userID, f.e.repoID, iid)
		}},
	}
	for _, tc := range protections {
		t.Run("protection: "+tc.name+" keeps the ref and records the reason", func(t *testing.T) {
			f := newSweepFix(t)
			f.enableRetire()
			f.live()
			f.forges.issue = forge.Issue{State: "closed"}
			tc.arm(f)
			if n := f.sweep(); n != 0 {
				t.Fatalf("changed %d rows", n)
			}
			p := f.mustPres("live")
			if p.LastError.String != "retire kept: "+tc.reason || f.deleteCount() != 0 || !p.NextCheckAt.Time.After(f.dbNow()) {
				t.Fatalf("last_error = %q deletes=%d next_check_at=%v, want %q", p.LastError.String, f.deleteCount(), p.NextCheckAt.Time, "retire kept: "+tc.reason)
			}
		})
	}

	t.Run("a protection that arrives before the decision keeps", func(t *testing.T) {
		f := newSweepFix(t)
		f.enableRetire()
		f.live()
		f.forges.issue = forge.Issue{State: "closed"}
		f.svc.terminal.afterObserve = func(*time.Time) { f.openHold() }
		f.sweep()
		p := f.mustPres("live")
		if p.LastError.String != "retire kept: custody_hold_open" || f.deleteCount() != 0 {
			t.Fatalf("last_error = %q deletes=%d", p.LastError.String, f.deleteCount())
		}
	})

	t.Run("a run with neither issue nor merge request never retires", func(t *testing.T) {
		f := newSweepFix(t)
		f.enableRetire()
		f.issueless(0)
		f.forges.issue = forge.Issue{State: "closed"}
		f.forges.mr = forge.MergeRequestSummary{State: forge.MRStateMerged}
		f.live()
		if p := f.mustPres("live"); p.LifetimeAtBind != "no_pr" {
			t.Fatalf("lifetime_at_bind = %q", p.LifetimeAtBind)
		}
		if n := f.sweep(); n != 0 {
			t.Fatalf("changed %d rows", n)
		}
		f.mustPres("live")
		if f.deleteCount() != 0 || f.forges.getIssues != 0 || f.forges.getMRs != 0 {
			t.Fatalf("deletes=%d issueReads=%d mrReads=%d", f.deleteCount(), f.forges.getIssues, f.forges.getMRs)
		}
	})

	t.Run("a no_pr run that gained a merge request is classified by it", func(t *testing.T) {
		f := newSweepFix(t)
		f.enableRetire()
		f.issueless(0)
		f.live()
		f.mustPres("live")
		f.e.exec(t, `UPDATE runs SET mr_iid=7 WHERE id=$1`, f.run)
		f.forges.mr = forge.MergeRequestSummary{State: forge.MRStateOpened}
		f.sweep()
		f.mustPres("live")
		if f.deleteCount() != 0 || f.forges.getMRs != 1 {
			t.Fatalf("deletes=%d mrReads=%d, want the open MR read and no delete", f.deleteCount(), f.forges.getMRs)
		}
		f.forges.mr = forge.MergeRequestSummary{State: forge.MRStateClosed}
		f.due()
		f.sweep()
		if p := f.mustPres("deleted"); p.RetireReason.String != "pr_closed" || p.LifetimeAtBind != "no_pr" {
			t.Fatalf("preservation = %+v", p)
		}
	})
}

// openHold inserts an open inventory-guarded custody hold of the fixture's run, the way a worker
// that failed with unsettled work would.
func (f *sweepFix) openHold() {
	f.t.Helper()
	f.e.exec(f.t, `INSERT INTO recovery_custody_holds
		(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id,inventory_guarded)
		VALUES ($1,$2,$3,$4,1,'open',$5,'term-ident',$5,$4,true)`, uuid.New(), f.e.userID, f.e.repoID, f.run, f.worker.ID)
}

// Once retiring is committed it is final: the decision is never re-observed, a crash after the remote
// effect and a lagging remote both converge to deleted, and nothing can make the row live again.
func TestTerminalSweepRetireIrrevocableLiveDB(t *testing.T) {
	t.Run("a crash after the delete, then a reopen, still ends deleted and never live", func(t *testing.T) {
		f := newSweepFix(t)
		f.enableRetire()
		f.live()
		f.forges.issue = forge.Issue{State: "closed"}
		f.svc.terminal.afterDelete = func() error { return errors.New("injected crash after the delete") }
		if err := f.sweepErr(); err == nil {
			t.Fatal("the crash hook did not fail the pass")
		}
		p := f.mustPres("retiring")
		retireStamp(t, p)
		if f.deleteCount() != 1 {
			t.Fatalf("deletes = %d", f.deleteCount())
		}
		// The issue is reopened. The decision stands: the next pass repeats only the remote effect.
		f.forges.mu.Lock()
		f.forges.issue = forge.Issue{State: "opened"}
		reads := f.forges.getIssues
		f.forges.mu.Unlock()
		f.svc.terminal.afterDelete = nil
		if n := f.sweep(); n != 1 {
			t.Fatalf("changed %d rows, want 1 (retiring -> deleted)", n)
		}
		f.mustPres("deleted")
		if f.forges.getIssues != reads {
			t.Fatalf("the lifetime was re-observed after the irrevocable decision (%d -> %d reads)", reads, f.forges.getIssues)
		}
		if f.deleteCount() != 1 {
			t.Fatalf("deletes = %d, want the single delete: the ref was already gone", f.deleteCount())
		}
	})

	t.Run("a remote that lags converges to deleted on a later pass", func(t *testing.T) {
		f := newSweepFix(t)
		f.enableRetire()
		f.live()
		f.forges.issue = forge.Issue{State: "closed"}
		// The delete is accepted but the confirming list still shows the ref.
		f.delOut = func(pushbroker.DeleteTerminalOptions) (pushbroker.TerminalDeleteOutcome, error) {
			return pushbroker.TerminalDeleteUnknown, errors.New("ref still present after the delete")
		}
		if n := f.sweep(); n != 1 {
			t.Fatalf("changed %d rows, want 1 (retiring)", n)
		}
		p := f.mustPres("retiring")
		if p.Attempts != 1 || !p.LastError.Valid || !p.NextCheckAt.Time.After(f.dbNow()) {
			t.Fatalf("preservation = %+v, want backoff with the attempt counted", p)
		}
		// The remote catches up; the issue reopens too.
		f.fake.mu.Lock()
		delete(f.fake.refs, f.run)
		f.fake.mu.Unlock()
		f.forges.mu.Lock()
		f.forges.issue = forge.Issue{State: "opened"}
		f.forges.mu.Unlock()
		f.due()
		if n := f.sweep(); n != 1 {
			t.Fatalf("changed %d rows, want 1", n)
		}
		f.mustPres("deleted")
		if f.deleteCount() != 1 {
			t.Fatalf("deletes = %d, want 1: the second pass saw the ref absent", f.deleteCount())
		}
	})

	t.Run("a ref at a foreign tip is abandoned, never deleted", func(t *testing.T) {
		f := newSweepFix(t)
		f.enableRetire()
		f.live()
		f.forges.issue = forge.Issue{State: "closed"}
		f.fake.setRef(f.run, termTip3)
		f.sweep()
		p := f.mustPres("abandoned")
		if p.LiveRunID.Valid || f.deleteCount() != 0 {
			t.Fatalf("preservation = %+v deletes=%d", p, f.deleteCount())
		}
	})

	t.Run("a ref that moved during the delete is abandoned", func(t *testing.T) {
		f := newSweepFix(t)
		f.enableRetire()
		f.live()
		f.forges.issue = forge.Issue{State: "closed"}
		f.delOut = func(pushbroker.DeleteTerminalOptions) (pushbroker.TerminalDeleteOutcome, error) {
			return pushbroker.TerminalDeleteMoved, pushbroker.ErrTerminalRefMoved
		}
		f.sweep()
		f.mustPres("abandoned")
	})

	t.Run("the trigger refuses to make a retiring row live", func(t *testing.T) {
		f := newSweepFix(t)
		f.enableRetire()
		f.live()
		f.forges.issue = forge.Issue{State: "closed"}
		f.svc.terminal.afterDelete = func() error { return errors.New("injected crash") }
		_ = f.sweepErr()
		f.mustPres("retiring")
		for _, set := range []string{"state='live'", "state='publishing'", "state='bound'", "state='not_published'"} {
			tag, err := f.e.pool.Exec(f.e.ctx, `UPDATE run_terminal_preservations SET `+set+` WHERE run_id=$1 AND state='retiring'`, f.run)
			if err != nil || tag.RowsAffected() != 0 {
				t.Fatalf("%s on a retiring row: rows=%d err=%v, want the trigger to refuse", set, tag.RowsAffected(), err)
			}
		}
		f.mustPres("retiring")
	})
}

// The route and the retirement, in both orders.
func TestTerminalSweepRetireVersusRouteLiveDB(t *testing.T) {
	t.Run("the route refuses once the row is retiring", func(t *testing.T) {
		f := newSweepFix(t)
		if res, err := f.publish(f.req(1, termTip1, "")); err != nil || res.PreservationState != "live" {
			t.Fatalf("publish gen 1 = %+v, %v", res, err)
		}
		f.e.exec(t, `UPDATE run_terminal_preservations SET state='retiring', retire_tip=tip, retire_reason='issue_closed',
			retire_observed_at=now(), retire_decided_at=now() WHERE run_id=$1`, f.run)
		_, sendsBefore, _ := f.fake.counts()
		_, err := f.publishGen2()
		if got := termReason(t, err); got != apitypes.TerminalReasonPreservationRetired {
			t.Fatalf("reason = %q, want %q", got, apitypes.TerminalReasonPreservationRetired)
		}
		if _, sends, _ := f.fake.counts(); sends != sendsBefore {
			t.Fatal("the route sent to a retiring ref")
		}
		if _, ok := f.con(2); ok {
			t.Fatal("a contribution was recorded against a retiring preservation")
		}
	})

	t.Run("a retirement with a contribution in flight keeps", func(t *testing.T) {
		f := newSweepFix(t)
		f.enableRetire()
		// Generation 2 is unresolved (publish_unknown) behind a settled live generation: the row is
		// publishing, so the retire lane never sees it live.
		if res, err := f.publish(f.req(1, termTip1, "")); err != nil || res.PreservationState != "live" {
			t.Fatalf("publish gen 1 = %+v, %v", res, err)
		}
		f.fake.updateFn = func(context.Context, pushbroker.TerminalPackOptions) (pushbroker.TerminalOutcome, error) {
			return pushbroker.TerminalUnknown, errors.New("push timed out")
		}
		if res, err := f.publishGen2(); err != nil || res.State != "publish_unknown" {
			t.Fatalf("publish gen 2 = %+v, %v", res, err)
		}
		f.terminal()
		f.due()
		f.resetCounts()
		f.sweep()
		f.mustPres("publishing")
		if f.deleteCount() != 0 || f.forges.getIssues != 0 {
			t.Fatalf("deletes=%d issueReads=%d: a publishing row must not be observed for retirement", f.deleteCount(), f.forges.getIssues)
		}
	})
}

// The two sweeps share the tick but not their rows.
func TestTerminalSweepIsolationFromHeldSweepLiveDB(t *testing.T) {
	c := newCleanupFix(t)
	c.created()
	f := newSweepFix(t)
	f.enableRetire()
	f.live()
	f.forges.issue = forge.Issue{State: "closed"}
	// Make the held row as due as it can be.
	c.e.exec(t, `UPDATE run_held_publications SET next_attempt_at = now() - interval '1 hour' WHERE run_id=$1`, c.run)

	heldRows := func() string {
		var s string
		if err := f.e.pool.QueryRow(f.e.ctx, `SELECT coalesce(string_agg(row_to_json(h)::text, '|' ORDER BY h.id), '') FROM run_held_publications h`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	termRows := func() string {
		var s string
		if err := f.e.pool.QueryRow(f.e.ctx, `SELECT coalesce(string_agg(row_to_json(p)::text, '|' ORDER BY p.id), '') ||
			coalesce((SELECT string_agg(row_to_json(k)::text, '|' ORDER BY k.id) FROM run_terminal_contributions k), '')
			FROM run_terminal_preservations p`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}

	beforeTerm := termRows()
	if _, err := c.svc.SweepHeldPublications(c.e.ctx); err != nil {
		t.Fatal(err)
	}
	if termRows() != beforeTerm {
		t.Fatal("SweepHeldPublications touched a terminal-preservation row")
	}

	beforeHeld := heldRows()
	f.sweep()
	f.mustPres("deleted")
	if heldRows() != beforeHeld {
		t.Fatal("SweepTerminalPreservations touched a run_held_publications row")
	}
}
