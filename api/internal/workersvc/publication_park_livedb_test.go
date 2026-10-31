package workersvc

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #1324 M3a: the server's publication park policy against a real Postgres. Time is driven
// through the service clock, never by sleeping. Skipped unless UZI_TEST_DATABASE_URL points at a
// throwaway Postgres (./e2e/run-store-it.sh); a package printing ok with PASS=0 is invalid.

func pubParkReport(gen int64, surface, step string) StateRequest {
	return StateRequest{
		State: "recovery_wait", RecoveryCause: strPtr("forge_unreachable"), ClaimGeneration: i64Ptr(gen),
		RecoveryPhase: strPtr("publication"), PublicationSurface: strPtr(surface), PublicationStep: strPtr(step),
	}
}

// publicationParkFixture is an enrolled running run with an open (unguarded) custody hold at
// generation 1 and a service clock the test moves.
type publicationParkFixture struct {
	*publicationResumeFixture
	clock time.Time
}

func newPublicationParkFixture(t *testing.T, bound time.Duration) *publicationParkFixture {
	t.Helper()
	f := newPublicationResumeFixture(t)
	f.svc.p.RunProviderParkMaxDuration = bound
	pf := &publicationParkFixture{publicationResumeFixture: f, clock: time.Now().UTC().Truncate(time.Microsecond)}
	f.svc.SetClock(func() time.Time { return pf.clock })
	pf.openHold(t, f.runID, 1)
	f.enroll(t)
	return pf
}

// openHold opens an unguarded custody hold for run at gen, owned by the fixture worker. Unguarded,
// so the legacy forge park WOULD release it as no_adopted_source.
func (f *publicationParkFixture) openHold(t *testing.T, run uuid.UUID, gen int64) {
	t.Helper()
	f.env.exec(`INSERT INTO recovery_custody_holds
 (user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id,inventory_guarded)
 VALUES ($1,$2,$3,$4,'open',$5,'fixture',$5,$3,false)`, f.userID, f.repoID, run, gen, f.workerID)
}

func (f *publicationParkFixture) park(gen int64, surface string) (store.Run, bool, error) {
	return f.svc.SetState(f.env.ctx, f.wkr, f.runID, pubParkReport(gen, surface, "push"))
}

func (f *publicationParkFixture) run(t *testing.T) store.Run {
	t.Helper()
	r, err := f.env.q.GetRunByID(f.env.ctx, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (f *publicationParkFixture) side(t *testing.T) store.RunPublicationResume {
	t.Helper()
	s, err := f.env.q.GetActivePublicationResume(f.env.ctx, f.runID)
	if err != nil {
		t.Fatalf("active intent: %v", err)
	}
	return s
}

func (f *publicationParkFixture) hold(t *testing.T) (state string, evidence pgtype.Text) {
	t.Helper()
	if err := f.env.pool.QueryRow(f.env.ctx,
		`SELECT state, release_evidence FROM recovery_custody_holds WHERE run_id=$1 ORDER BY generation DESC LIMIT 1`, f.runID).Scan(&state, &evidence); err != nil {
		t.Fatal(err)
	}
	return state, evidence
}

// resumeFlight returns a parked run to running at its current generation, as a retry promotion
// followed by a claim and a running report does, under the run-fence opt-ins.
func (f *publicationParkFixture) resumeFlight(t *testing.T) {
	t.Helper()
	tx, err := f.env.pool.Begin(f.env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(f.env.ctx) }()
	if err = store.New(tx).OptInRunFences(f.env.ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(f.env.ctx, `UPDATE runs SET status='running', status_since=now(), recovery_wait_cause=NULL, recovery_retry_not_before=NULL WHERE id=$1`, f.runID); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(f.env.ctx); err != nil {
		t.Fatal(err)
	}
}

func timed(r store.Run) bool { return r.RecoveryRetryNotBefore.Valid }

// Before, at and after the limit, and with the limit disabled. The first park of an episode never
// holds; the clock starts there and no later park, duplicate or generation change restarts it.
func TestPublicationParkElapsedBoundLiveDB(t *testing.T) {
	const bound = time.Hour
	for _, tc := range []struct {
		name    string
		bound   time.Duration
		advance time.Duration
		hold    bool
	}{
		{"before the limit", bound, bound - time.Second, false},
		{"at the limit", bound, bound, true},
		{"after the limit", bound, bound + 3*time.Hour, true},
		{"disabled limit", 0, 1000 * time.Hour, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPublicationParkFixture(t, tc.bound)
			t0 := f.clock
			run, applied, err := f.park(1, "git")
			if err != nil || !applied || run.Status != "recovery_wait" || !timed(run) || run.RecoveryWaitPhase.String != "publication" ||
				run.RecoveryWaitCause.String != "forge_unreachable" {
				t.Fatalf("first park status=%s phase=%v stamp=%v applied=%v err=%v", run.Status, run.RecoveryWaitPhase, run.RecoveryRetryNotBefore, applied, err)
			}
			s := f.side(t)
			if !s.WaitStartedAt.Time.Equal(t0) || !s.FirstWaitAt.Time.Equal(t0) || s.ParkTotal != 1 || s.HoldReason.Valid {
				t.Fatalf("side after first park: %+v", s)
			}
			f.resumeFlight(t)
			f.clock = t0.Add(tc.advance)
			run, applied, err = f.park(1, "git")
			if err != nil || !applied || run.Status != "recovery_wait" {
				t.Fatalf("second park status=%s applied=%v err=%v", run.Status, applied, err)
			}
			s = f.side(t)
			if tc.hold {
				if timed(run) || !s.HoldReason.Valid || s.HoldReason.String != "publication_retry_exhausted" {
					t.Fatalf("expected owner hold, stamp=%v side=%+v", run.RecoveryRetryNotBefore, s)
				}
			} else if !timed(run) || !run.RecoveryRetryNotBefore.Time.After(f.clock) || s.HoldReason.Valid {
				t.Fatalf("expected timed retry, stamp=%v side=%+v", run.RecoveryRetryNotBefore, s)
			}
			if !s.WaitStartedAt.Time.Equal(t0) || !s.FirstWaitAt.Time.Equal(t0) || s.ParkTotal != 2 {
				t.Fatalf("clock or counters moved: %+v", s)
			}
			if run.RecoveryWaitCount != 2 || run.ForgeParkCount != 0 {
				t.Fatalf("recovery_wait_count=%d forge_park_count=%d", run.RecoveryWaitCount, run.ForgeParkCount)
			}
			if st, _ := f.hold(t); st != "open" {
				t.Fatalf("custody hold %s", st)
			}
		})
	}
}

// The owner hold is inert to every timer promoter and to the pre-clone Resume; only the
// publication Resume releases it, as a new episode that keeps the intent and custody.
func TestPublicationHoldResumeEpisodeLiveDB(t *testing.T) {
	f := newPublicationParkFixture(t, time.Hour)
	ctx := f.env.ctx
	t0 := f.clock
	if _, _, err := f.park(1, "git"); err != nil {
		t.Fatal(err)
	}
	f.resumeFlight(t)
	f.clock = t0.Add(time.Hour)
	held, applied, err := f.park(1, "git")
	if err != nil || !applied || timed(held) {
		t.Fatalf("hold=%v/%v/%v", held.Status, applied, err)
	}
	if _, err := f.env.q.PromoteRecoveryWaitRuns(ctx, pgtype.Timestamptz{Time: t0.Add(1000 * time.Hour), Valid: true}); err != nil {
		t.Fatal(err)
	}
	if n, err := f.env.q.PromoteRecoveryWaitRunNow(ctx, store.PromoteRecoveryWaitRunNowParams{ID: f.runID, UserID: f.userID}); err != nil || n != 0 {
		t.Fatalf("early promote=%d/%v", n, err)
	}
	// The pre-clone Resume must not match a phase hold.
	if _, err := f.env.q.ResumeForgeUnreachableHold(ctx, store.ResumeForgeUnreachableHoldParams{ID: f.runID, UserID: f.userID, GlobalTimeoutSeconds: 86400}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("pre-clone Resume matched a publication hold: %v", err)
	}
	if r := f.run(t); r.Status != "recovery_wait" {
		t.Fatalf("status=%s", r.Status)
	}
	// A foreign owner cannot resume it.
	if ok, err := f.svc.ResumePublicationForgeHold(ctx, uuid.New(), held); err != nil || ok {
		t.Fatalf("foreign resume=%v/%v", ok, err)
	}
	before := f.side(t)
	ok, err := f.svc.ResumePublicationForgeHold(ctx, f.userID, held)
	if err != nil || !ok {
		t.Fatalf("resume=%v/%v", ok, err)
	}
	r := f.run(t)
	if r.Status != "queued" || r.RecoveryWaitPhase.Valid || r.RecoveryWaitCause.Valid || r.ForgeParkCount != 0 ||
		r.ClaimGeneration != 1 || r.WorkerID != held.WorkerID || r.SessionID != held.SessionID {
		t.Fatalf("resumed run: %+v", r)
	}
	s := f.side(t)
	if s.Episode != before.Episode+1 || s.WaitStartedAt.Valid || s.HoldReason.Valid ||
		!s.FirstWaitAt.Time.Equal(t0) || s.ParkTotal != before.ParkTotal || string(s.Intent) != string(before.Intent) ||
		s.EnrollmentSeq != before.EnrollmentSeq || s.State != "active" {
		t.Fatalf("side after resume: %+v (before %+v)", s, before)
	}
	if st, ev := f.hold(t); st != "open" || ev.Valid {
		t.Fatalf("resume touched custody: %s/%v", st, ev)
	}
	// It is not a hold any more, so a second resume is a no-op.
	if ok, err := f.svc.ResumePublicationForgeHold(ctx, f.userID, held); err != nil || ok {
		t.Fatalf("second resume=%v/%v", ok, err)
	}
	// The custodian claims it again and a new episode starts a fresh clock: the next park, even
	// an hour after the first episode began, is a timed retry.
	got, gen := f.claim(t)
	if got != f.runID || gen != 2 {
		t.Fatalf("custodian claim=%s/%d", got, gen)
	}
	f.clock = f.clock.Add(time.Minute)
	if _, applied, err := f.svc.SetState(ctx, f.wkr, f.runID, StateRequest{State: "running", ClaimGeneration: i64Ptr(gen)}); err != nil || !applied {
		t.Fatalf("running=%v/%v", applied, err)
	}
	run, applied, err := f.svc.SetState(ctx, f.wkr, f.runID, pubParkReport(gen, "git", "push"))
	if err != nil || !applied || !timed(run) {
		t.Fatalf("next episode park status=%s stamp=%v applied=%v err=%v", run.Status, run.RecoveryRetryNotBefore, applied, err)
	}
	if s = f.side(t); !s.WaitStartedAt.Time.Equal(f.clock) || s.Episode != 1 || s.ParkTotal != 3 {
		t.Fatalf("side after next-episode park: %+v", s)
	}
}

// Duplicates never reset the clock or the counters; a new generation keeps the clock too.
func TestPublicationParkDuplicateAndGenerationKeepClockLiveDB(t *testing.T) {
	f := newPublicationParkFixture(t, time.Hour)
	t0 := f.clock
	if _, applied, err := f.park(1, "git"); err != nil || !applied {
		t.Fatalf("park=%v/%v", applied, err)
	}
	f.clock = t0.Add(30 * time.Minute)
	if _, applied, err := f.park(1, "git"); err != nil || applied {
		t.Fatalf("duplicate=%v/%v, want an unapplied no-op", applied, err)
	}
	if s := f.side(t); s.ParkTotal != 1 || !s.WaitStartedAt.Time.Equal(t0) {
		t.Fatalf("duplicate moved the side row: %+v", s)
	}
	// A later claim of the same run (new generation, new hold) keeps the wait clock, so a park
	// past the bound holds immediately.
	f.resumeFlight(t)
	f.reclaim(t, 2)
	f.openHold(t, f.runID, 2)
	f.clock = t0.Add(time.Hour)
	run, applied, err := f.park(2, "git")
	if err != nil || !applied || timed(run) {
		t.Fatalf("park at generation 2 status=%s stamp=%v applied=%v err=%v", run.Status, run.RecoveryRetryNotBefore, applied, err)
	}
	if s := f.side(t); !s.WaitStartedAt.Time.Equal(t0) || s.HoldReason.String != "publication_retry_exhausted" {
		t.Fatalf("generation change restarted the clock: %+v", s)
	}
	// The same holds for a different worker taking the run over: only the run's side row is read.
	f.resumeFlight(t)
	peer := f.peerWorker(t)
	f.reclaim(t, 3)
	f.env.exec(`DELETE FROM recovery_custody_holds WHERE run_id=$1`, f.runID)
	f.env.exec(`INSERT INTO recovery_custody_holds
 (user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id,inventory_guarded)
 VALUES ($1,$2,$3,3,'open',$4,'fixture',$4,$3,false)`, f.userID, f.repoID, f.runID, peer.ID)
	tx, err := f.env.pool.Begin(f.env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.New(tx).OptInRunFences(f.env.ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(f.env.ctx, `UPDATE runs SET worker_id=$2 WHERE id=$1`, f.runID, peer.ID); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(f.env.ctx); err != nil {
		t.Fatal(err)
	}
	f.clock = t0.Add(2 * time.Hour)
	if _, applied, err = f.svc.SetState(f.env.ctx, peer, f.runID, pubParkReport(3, "git", "push")); err != nil || !applied {
		t.Fatalf("park by the peer=%v/%v", applied, err)
	}
	if s := f.side(t); !s.WaitStartedAt.Time.Equal(t0) || s.ParkTotal != 3 {
		t.Fatalf("worker change restarted the clock: %+v", s)
	}
}

// peerWorker inserts a second online worker of the same owner that holds the capability.
func (f *publicationParkFixture) peerWorker(t *testing.T) store.Worker {
	t.Helper()
	id := uuid.New()
	caps := []string{capability.RecoveryArchiveV1, capability.PublicationResumeV1}
	f.env.exec(`INSERT INTO workers (id,user_id,name,token_hash,status,last_heartbeat_at,protocol_capabilities) VALUES ($1,$2,'peer',$3,'online',now(),$4)`,
		id, f.userID, id[:], caps)
	return store.Worker{ID: id, UserID: f.userID, Name: "peer", Status: "online", ProtocolCapabilities: caps}
}

// The forge-park cap and counter belong to the pre-clone park alone.
func TestPublicationParkIgnoresForgeParkCapLiveDB(t *testing.T) {
	f := newPublicationParkFixture(t, time.Hour)
	f.svc.p.RunForgeUnreachableMaxParks = 1
	f.env.exec(`UPDATE runs SET forge_park_count=5 WHERE id=$1`, f.runID)
	run, applied, err := f.park(1, "git")
	if err != nil || !applied || !timed(run) || run.ForgeParkCount != 5 {
		t.Fatalf("enrolled park stamp=%v count=%d applied=%v err=%v", run.RecoveryRetryNotBefore, run.ForgeParkCount, applied, err)
	}

	// Control: a run without an intent still takes the pre-clone path, unchanged.
	plain := f.seedRun("running", 1)
	f.env.exec(`UPDATE runs SET worker_id=$2, started_at=now() WHERE id=$1`, plain, f.workerID)
	f.openHold(t, plain, 1)
	got, applied, err := f.svc.SetState(f.env.ctx, f.wkr, plain, fpForgeParkReport())
	if err != nil || !applied || got.ForgeParkCount != 1 || !timed(got) || got.RecoveryWaitPhase.Valid {
		t.Fatalf("non-enrolled park count=%d stamp=%v phase=%v applied=%v err=%v", got.ForgeParkCount, got.RecoveryRetryNotBefore, got.RecoveryWaitPhase, applied, err)
	}
	var state string
	var ev pgtype.Text
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT state, release_evidence FROM recovery_custody_holds WHERE run_id=$1`, plain).Scan(&state, &ev); err != nil ||
		state != "released" || ev.String != "no_adopted_source" {
		t.Fatalf("non-enrolled hold %s/%v/%v, want the no_adopted_source release", state, ev, err)
	}
}

// A legacy (untyped) forge park of an enrolled run never takes the pre-clone path.
func TestPublicationLegacyParkRedirectsLiveDB(t *testing.T) {
	f := newPublicationParkFixture(t, time.Hour)
	// A capable worker must send the typed phase: refused, nothing mutated.
	run, applied, err := f.svc.SetState(f.env.ctx, f.wkr, f.runID, fpForgeParkReport())
	if !errors.Is(err, ErrPublicationEnrolled) || applied || run.Status != "running" {
		t.Fatalf("capable legacy park status=%s applied=%v err=%v", run.Status, applied, err)
	}
	if s := f.side(t); s.ParkTotal != 0 {
		t.Fatalf("refused park counted: %+v", s)
	}
	// A worker without the capability is redirected to the publication writer.
	legacy := f.wkr
	legacy.ProtocolCapabilities = []string{capability.RecoveryArchiveV1}
	run, applied, err = f.svc.SetState(f.env.ctx, legacy, f.runID, fpForgeParkReport())
	if err != nil || !applied || run.Status != "recovery_wait" || run.RecoveryWaitPhase.String != "publication" || run.ForgeParkCount != 0 || !timed(run) {
		t.Fatalf("redirected park status=%s phase=%v count=%d applied=%v err=%v", run.Status, run.RecoveryWaitPhase, run.ForgeParkCount, applied, err)
	}
	if st, ev := f.hold(t); st != "open" || ev.Valid {
		t.Fatalf("legacy park of an enrolled run released custody: %s/%v", st, ev)
	}
	if s := f.side(t); s.ParkTotal != 1 {
		t.Fatalf("side=%+v", s)
	}
	// A legacy duplicate is a no-op.
	if _, applied, err = f.svc.SetState(f.env.ctx, legacy, f.runID, fpForgeParkReport()); err != nil || applied {
		t.Fatalf("legacy duplicate=%v/%v", applied, err)
	}
	if s := f.side(t); s.ParkTotal != 1 {
		t.Fatalf("duplicate counted: %+v", s)
	}
}

// A stop verdict stamped during the outage still cancels the run, and the enrolled run's custody
// hold is not released as no_adopted_source on the way.
func TestPublicationParkStampedStopCancelsWithoutReleaseLiveDB(t *testing.T) {
	f := newPublicationParkFixture(t, time.Hour)
	f.env.exec(`UPDATE runs SET stop_kind='cancelled' WHERE id=$1`, f.runID)
	legacy := f.wkr
	legacy.ProtocolCapabilities = []string{capability.RecoveryArchiveV1}
	if _, applied, err := f.svc.SetState(f.env.ctx, legacy, f.runID, fpForgeParkReport()); err != nil || !applied {
		t.Fatalf("stopped park=%v/%v", applied, err)
	}
	if r := f.run(t); r.Status != "cancelled" {
		t.Fatalf("status=%s", r.Status)
	}
	if _, ev := f.hold(t); ev.String == "no_adopted_source" {
		t.Fatal("stamped stop released the enrolled run's custody as no_adopted_source")
	}
}

// The handoff is recorded only when it names the run's published checkpoint tip.
func TestPublicationParkHandoffAndPeerClaimLiveDB(t *testing.T) {
	f := newPublicationParkFixture(t, time.Hour)
	peer := f.peerWorker(t)
	f.env.exec(`UPDATE runs SET checkpoint_tip=$2 WHERE id=$1`, f.runID, pubHeadC)
	req := pubParkReport(1, "git", "push")
	req.HandoffCheckpointTip = strPtr(pubHeadB) // not the published tip
	if _, applied, err := f.svc.SetState(f.env.ctx, f.wkr, f.runID, req); err != nil || !applied {
		t.Fatalf("park=%v/%v", applied, err)
	}
	if s := f.side(t); len(s.Handoff) != 0 {
		t.Fatalf("mismatching handoff recorded: %s", s.Handoff)
	}
	// Without a handoff only the custodian claims the run, even past the affinity ceiling.
	f.env.exec(`UPDATE runs SET status='queued', updated_at=now()-interval '30 days' WHERE id=$1`, f.runID)
	if got, err := f.svc.Claim(f.env.ctx, peer, nil); err != nil || got != nil {
		t.Fatalf("peer claimed a custodian-bound run: %+v/%v", got, err)
	}
	// The matching tip records it, and a peer may then claim.
	f.resumeFlight(t)
	req.HandoffCheckpointTip = strPtr(pubHeadC)
	f.clock = f.clock.Add(time.Minute)
	if _, applied, err := f.svc.SetState(f.env.ctx, f.wkr, f.runID, req); err != nil || !applied {
		t.Fatalf("park with handoff=%v/%v", applied, err)
	}
	s := f.side(t)
	var h map[string]string
	if err := json.Unmarshal(s.Handoff, &h); err != nil || h["checkpoint_tip"] != pubHeadC {
		t.Fatalf("handoff=%s err=%v", s.Handoff, err)
	}
	f.env.exec(`UPDATE runs SET status='queued', updated_at=now()-interval '30 days' WHERE id=$1`, f.runID)
	got, err := f.svc.Claim(f.env.ctx, peer, nil)
	if err != nil || got == nil || got.RunID != f.runID.String() {
		t.Fatalf("peer claim with a handoff: %+v/%v", got, err)
	}
	if got.PublicationResume == nil || string(got.PublicationResume.Handoff) == "" {
		t.Fatalf("handoff missing from the claim: %+v", got.PublicationResume)
	}
}

// The custodian is the only claimant of an enrolled run without a handoff.
func TestPublicationCustodianOnlyClaimLiveDB(t *testing.T) {
	f := newPublicationParkFixture(t, time.Hour)
	peer := f.peerWorker(t)
	f.env.exec(`UPDATE runs SET status='queued', updated_at=now()-interval '30 days' WHERE id=$1`, f.runID)
	if got, err := f.svc.Claim(f.env.ctx, peer, nil); err != nil || got != nil {
		t.Fatalf("peer claimed: %+v/%v", got, err)
	}
	got, g := f.claim(t)
	if got != f.runID || g != 2 {
		t.Fatalf("custodian claim=%s/%d", got, g)
	}
}

// D5: a queued enrolled run without a handoff is held for its owner when the custodian cannot
// serve it, and only then.
func TestPublicationCustodianSweepLiveDB(t *testing.T) {
	type arm struct {
		name   string
		mutate func(f *publicationParkFixture)
		held   bool
	}
	healthy := func(f *publicationParkFixture) {
		f.env.exec(`UPDATE workers SET last_heartbeat_at=$2 WHERE id=$1`, f.workerID, f.clock)
	}
	for _, a := range []arm{
		{"healthy custodian", func(f *publicationParkFixture) { healthy(f) }, false},
		{"no custodian", func(f *publicationParkFixture) {
			healthy(f)
			f.env.exec(`UPDATE runs SET worker_id=NULL WHERE id=$1`, f.runID)
		}, true},
		{"custodian lacks the capability", func(f *publicationParkFixture) {
			healthy(f)
			f.env.exec(`UPDATE workers SET protocol_capabilities=$2 WHERE id=$1`, f.workerID, []string{capability.RecoveryArchiveV1})
		}, true},
		{"stale heartbeat", func(f *publicationParkFixture) {
			f.env.exec(`UPDATE workers SET last_heartbeat_at=$2 WHERE id=$1`, f.workerID, f.clock.Add(-10*time.Minute))
		}, true},
		{"draining inside the ceiling (image roll)", func(f *publicationParkFixture) {
			f.env.exec(`UPDATE workers SET draining_since=$2, last_heartbeat_at=$3 WHERE id=$1`, f.workerID, f.clock, f.clock.Add(-10*time.Minute))
		}, false},
		{"draining beyond the ceiling", func(f *publicationParkFixture) {
			f.env.exec(`UPDATE workers SET draining_since=$2, last_heartbeat_at=$3 WHERE id=$1`, f.workerID, f.clock.Add(-3*time.Hour), f.clock.Add(-10*time.Minute))
			f.env.exec(`UPDATE runs SET updated_at=$2 WHERE id=$1`, f.runID, f.clock.Add(-3*time.Hour))
		}, true},
		{"handoff recorded", func(f *publicationParkFixture) {
			f.env.exec(`UPDATE workers SET last_heartbeat_at=$2 WHERE id=$1`, f.workerID, f.clock.Add(-10*time.Minute))
			f.env.exec(`UPDATE run_publication_resume SET handoff='{"checkpoint_tip":"`+pubHeadC+`"}'::json WHERE run_id=$1`, f.runID)
		}, false},
	} {
		t.Run(a.name, func(t *testing.T) {
			f := newPublicationParkFixture(t, time.Hour)
			f.svc.p.WorkerHeartbeatStale = time.Minute
			f.svc.p.WorkerAffinityCeiling = 2 * time.Hour
			f.env.exec(`UPDATE runs SET status='queued' WHERE id=$1`, f.runID)
			a.mutate(f)
			n, err := f.svc.holdPublicationRunsWithoutCustodian(f.env.ctx, f.clock)
			if err != nil {
				t.Fatal(err)
			}
			r := f.run(t)
			if !a.held {
				if r.Status != "queued" || f.side(t).HoldReason.Valid {
					t.Fatalf("held a servable run: status=%s side=%+v n=%d", r.Status, f.side(t), n)
				}
				return
			}
			s := f.side(t)
			if r.Status != "recovery_wait" || r.RecoveryWaitCause.String != "forge_unreachable" || r.RecoveryWaitPhase.String != "publication" ||
				r.RecoveryRetryNotBefore.Valid || s.HoldReason.String != "publication_custodian_unavailable" || s.State != "active" {
				t.Fatalf("not held: status=%s run=%+v side=%+v", r.Status, r, s)
			}
			if st, ev := f.hold(t); st != "open" || ev.Valid {
				t.Fatalf("hold touched custody: %s/%v", st, ev)
			}
			// It is an owner hold: the timer promoter leaves it alone, and the owner's Resume queues it.
			if _, err := f.env.q.PromoteRecoveryWaitRuns(f.env.ctx, pgtype.Timestamptz{Time: f.clock.Add(1000 * time.Hour), Valid: true}); err != nil {
				t.Fatal(err)
			}
			if f.run(t).Status != "recovery_wait" {
				t.Fatal("timer promoted a custodian hold")
			}
			if ok, err := f.svc.ResumePublicationForgeHold(f.env.ctx, f.userID, r); err != nil || !ok {
				t.Fatalf("resume=%v/%v", ok, err)
			}
		})
	}
}

// The sweep wires the custodian pass in (a no-custodian queued run is held by Sweep itself).
func TestPublicationSweepHoldsUnservableRunLiveDB(t *testing.T) {
	f := newPublicationParkFixture(t, time.Hour)
	f.env.exec(`UPDATE runs SET status='queued', worker_id=NULL WHERE id=$1`, f.runID)
	res, err := f.svc.Sweep(f.env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.PublicationHeld < 1 || f.run(t).Status != "recovery_wait" || f.side(t).HoldReason.String != "publication_custodian_unavailable" {
		t.Fatalf("held=%d status=%s side=%+v", res.PublicationHeld, f.run(t).Status, f.side(t))
	}
}

// A claimed-never-started sweep requeues the run and keeps its intent.
func TestPublicationClaimedNeverStartedKeepsIntentLiveDB(t *testing.T) {
	f := newPublicationParkFixture(t, time.Hour)
	f.env.exec(`UPDATE runs SET status='claimed', claimed_at=now()-interval '1 hour' WHERE id=$1`, f.runID)
	rows, err := f.env.q.SweepClaimedNeverStarted(f.env.ctx, pgtype.Timestamptz{Time: time.Now(), Valid: true})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range rows {
		found = found || r.ID == f.runID
	}
	if !found || f.run(t).Status != "queued" {
		t.Fatalf("not swept: found=%v status=%s", found, f.run(t).Status)
	}
	if s := f.side(t); s.State != "active" || s.EnrollmentSeq != 1 {
		t.Fatalf("intent=%+v", s)
	}
}

// Custody admission is unchanged by Resume: no exemption and no refund, at limits 1 and 3.
func TestPublicationResumeCustodyLimitsLiveDB(t *testing.T) {
	for _, limit := range []int32{1, 3} {
		t.Run(string(rune('0'+limit)), func(t *testing.T) {
			f := newPublicationParkFixture(t, time.Hour)
			f.svc.p.CustodyHoldLimit = &fakeCustodyLimit{n: limit}
			// Fill the owner's custody up to the limit with unrelated runs' holds (the run's own
			// open hold counts toward it).
			f.seedUnrelatedOpenHolds(int(limit) - 1)
			if _, applied, err := f.park(1, "git"); err != nil || !applied {
				t.Fatalf("park=%v/%v", applied, err)
			}
			if f.openHolds(t) != int64(limit) {
				t.Fatalf("open holds=%d want %d", f.openHolds(t), limit)
			}
			r := f.run(t)
			if ok, err := f.svc.ResumePublicationForgeHold(f.env.ctx, f.userID, r); err == nil && ok {
				t.Fatal("a timed park is not an owner hold")
			}
			// Make it an owner hold, resume it, and claim.
			f.resumeFlight(t)
			f.clock = f.clock.Add(time.Hour)
			if _, applied, err := f.park(1, "git"); err != nil || !applied {
				t.Fatalf("hold=%v/%v", applied, err)
			}
			if ok, err := f.svc.ResumePublicationForgeHold(f.env.ctx, f.userID, f.run(t)); err != nil || !ok {
				t.Fatalf("resume=%v/%v", ok, err)
			}
			got, gen := f.claim(t)
			// The run's own open hold keeps it a continuation only while its own count is below the
			// limit: limit 1 leaves it queued, limit 3 lets it through. Resume changed neither rule.
			if limit == 1 {
				if got != uuid.Nil {
					t.Fatalf("resume bypassed the custody limit: %s", got)
				}
				return
			}
			if got != f.runID || gen != 2 {
				t.Fatalf("claim=%s/%d want %s/2", got, gen, f.runID)
			}
		})
	}
}

// The API-origin seam: a git park honours the origin cooldown, an api park does not read it.
func TestPublicationParkHonoursGitOriginCooldownLiveDB(t *testing.T) {
	f := newPublicationParkFixture(t, 0)
	f.env.exec(`UPDATE runs SET status='queued' WHERE id=$1`, f.runID)
	got, gen := f.claim(t)
	if got != f.runID {
		t.Fatalf("claim=%s", got)
	}
	if _, applied, err := f.svc.SetState(f.env.ctx, f.wkr, f.runID, StateRequest{State: "running", ClaimGeneration: i64Ptr(gen)}); err != nil || !applied {
		t.Fatalf("running=%v/%v", applied, err)
	}
	var origin string
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT origin FROM run_forge_origins WHERE run_id=$1`, f.runID).Scan(&origin); err != nil {
		t.Fatalf("claim stamped no origin: %v", err)
	}
	cooled := f.clock.Add(3 * time.Hour)
	f.env.exec(`INSERT INTO forge_origin_cooldowns (user_id, worker_id, origin, failure_step, cooled_until, last_reason, observed_at)
 VALUES ($1,$2,$3,0,$4,'transient_clone_fetch_exhausted',now())`, f.userID, f.workerID, origin, cooled)
	for surface, wantCooled := range map[string]bool{"api": false, "git": true} {
		f.resumeFlight(t)
		f.env.exec(`UPDATE runs SET recovery_wait_count=0 WHERE id=$1`, f.runID)
		run, applied, err := f.svc.SetState(f.env.ctx, f.wkr, f.runID, pubParkReport(gen, surface, "push"))
		if err != nil || !applied || !timed(run) {
			t.Fatalf("%s park stamp=%v applied=%v err=%v", surface, run.RecoveryRetryNotBefore, applied, err)
		}
		if after := !run.RecoveryRetryNotBefore.Time.Before(cooled); after != wantCooled {
			t.Fatalf("%s park stamp %v vs cooldown %v: honoured=%v want %v", surface, run.RecoveryRetryNotBefore.Time, cooled, after, wantCooled)
		}
	}
}

// ---- D7: the requeue-cap failers never fail an enrolled run; D6: register reconciliation ----

// enrollSQL gives a running run an active intent without the API (opted-in SQL), as a prior flight
// would have.
func enrollSQL(t *testing.T, env codexTestEnv, runID, repoID uuid.UUID, branch string) {
	t.Helper()
	tx, err := env.pool.Begin(env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(env.ctx) }()
	if err = store.New(tx).OptInPublicationResume(env.ctx); err != nil {
		t.Fatal(err)
	}
	intent, _ := json.Marshal(map[string]any{
		"run_id": runID, "repo_id": repoID, "branch": branch, "claim_generation": 1,
		"original_head": pubHeadA, "floor": map[string]any{"kind": "absent"}, "candidate": nil,
	})
	if _, err = tx.Exec(env.ctx, `UPDATE runs SET branch=$2 WHERE id=$1`, runID, branch); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(env.ctx, `INSERT INTO run_publication_resume (run_id, enrollment_seq, claim_generation, intent, state) VALUES ($1,1,1,$2,'active')`, runID, intent); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(env.ctx); err != nil {
		t.Fatal(err)
	}
}

func activeIntentSeq(t *testing.T, env codexTestEnv, runID uuid.UUID) (int32, bool) {
	t.Helper()
	s, err := env.q.GetActivePublicationResume(env.ctx, runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return s.EnrollmentSeq, true
}

// With no recovery evidence at all, an over-cap run is failed worker_lost; the same run with an
// active intent is parked as an owner hold instead, and keeps its intent. Every failer shares the
// evidence classification, so the three entry points are exercised.
func TestPublicationOverCapFailersExcludeEnrolledLiveDB(t *testing.T) {
	setup := func(t *testing.T) (codexTestEnv, *Service, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID) {
		env := setupCodexLiveDB(t)
		userID, _, repoID := env.seedCodexInfra(t)
		p := testParams()
		p.RunMaxRequeues = 1
		svc := snapshotSvc(env, p)
		workerID := seedSnapshotWorker(t, env, userID, "old-nonce")
		plain := seedOutageRun(t, env, userID, repoID, workerID, "running", "issue", 1, 1)
		enrolled := seedOutageRun(t, env, userID, repoID, workerID, "running", "issue", 1, 1)
		enrollSQL(t, env, enrolled, repoID, "agent/issue-9")
		return env, svc, userID, workerID, plain, enrolled
	}
	check := func(t *testing.T, env codexTestEnv, plain, enrolled uuid.UUID) {
		t.Helper()
		if got := statusOf(t, env, plain); got != "failed" || failOriginOf(t, env, plain) != "worker_lost" {
			t.Fatalf("control run status=%s origin=%s, want failed/worker_lost", got, failOriginOf(t, env, plain))
		}
		if got := statusOf(t, env, enrolled); got != "recovery_wait" || failOriginOf(t, env, enrolled) != "" {
			t.Fatalf("enrolled run status=%s origin=%q, want an owner hold", got, failOriginOf(t, env, enrolled))
		}
		if seq, ok := activeIntentSeq(t, env, enrolled); !ok || seq != 1 {
			t.Fatalf("intent lost: %d/%v", seq, ok)
		}
		var cause string
		if err := env.pool.QueryRow(env.ctx, `SELECT recovery_wait_cause FROM runs WHERE id=$1`, enrolled).Scan(&cause); err != nil || cause != "worker_requeue_exhausted" {
			t.Fatalf("cause=%q err=%v", cause, err)
		}
	}
	t.Run("register orphan pass", func(t *testing.T) {
		env, svc, userID, workerID, plain, enrolled := setup(t)
		registerWith(t, env, svc, userID, workerID, nil)
		check(t, env, plain, enrolled)
	})
	t.Run("worker over cap", func(t *testing.T) {
		env, _, _, workerID, plain, enrolled := setup(t)
		if _, err := env.q.FailWorkerRunsOverCap(env.ctx, store.FailWorkerRunsOverCapParams{
			FailureReason: pgtype.Text{String: "x", Valid: true}, WorkerID: pgtype.UUID{Bytes: workerID, Valid: true}, MaxRequeues: 1,
		}); err != nil {
			t.Fatal(err)
		}
		check(t, env, plain, enrolled)
	})
	t.Run("stale worker sweep", func(t *testing.T) {
		env, _, _, workerID, plain, enrolled := setup(t)
		env.exec(`UPDATE workers SET last_heartbeat_at = now() - interval '1 day' WHERE id = $1`, workerID)
		if _, err := env.q.FailRunsOfStaleWorkersOverCap(env.ctx, store.FailRunsOfStaleWorkersOverCapParams{
			FailureReason: pgtype.Text{String: "x", Valid: true}, MaxRequeues: 1, FailCutoff: pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true},
		}); err != nil {
			t.Fatal(err)
		}
		check(t, env, plain, enrolled)
	})
}

// A run listed under publication_resume on a register snapshot is never a finalize_resume: it
// does not consume the #1742 allowance, and the ordinary requeue keeps its intent.
func TestPublicationRegisterExcludesFinalizeResumeLiveDB(t *testing.T) {
	pub := func(run uuid.UUID, gen int64) PublicationResumeEntry {
		return PublicationResumeEntry{RunID: run.String(), ClaimGeneration: gen, EnrollmentSeq: 1, CandidateSeq: 0}
	}
	for _, tc := range []struct {
		name       string
		requeues   int32
		offerPub   bool
		wantStatus string
		wantMark   bool
	}{
		{"at the limit without the publication list uses the allowance", 1, false, "queued", true},
		{"at the limit with the publication list does not", 1, true, "recovery_wait", false},
		{"under budget with the publication list requeues and keeps the intent", 0, true, "queued", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			userID, _, repoID := env.seedCodexInfra(t)
			svc := snapshotSvc(env, testParams())
			workerID := seedSnapshotWorker(t, env, userID, "old-nonce")
			runID := seedOutageRun(t, env, userID, repoID, workerID, "running", "issue", 1, tc.requeues)
			enrollSQL(t, env, runID, repoID, "agent/issue-8")
			snap := finalizeSnap(false, fin(runID, 1))
			if tc.offerPub {
				snap.PublicationResume = []PublicationResumeEntry{pub(runID, 1)}
			}
			registerWith(t, env, svc, userID, workerID, snap)
			if got := statusOf(t, env, runID); got != tc.wantStatus {
				t.Fatalf("status=%s want %s", got, tc.wantStatus)
			}
			if _, has := finalizeMarkOf(t, env, runID); has != tc.wantMark {
				t.Fatalf("finalize_resume_generation set=%v want %v", has, tc.wantMark)
			}
			if seq, ok := activeIntentSeq(t, env, runID); !ok || seq != 1 {
				t.Fatalf("intent lost: %d/%v", seq, ok)
			}
		})
	}
}

// ---- D2: the owner's completion-hold continue ends the intent in the same transaction ----

func TestPublicationContinueRetiresIntentAtomicallyLiveDB(t *testing.T) {
	blocked := func(f *publicationParkFixture) {
		f.env.exec(`UPDATE runs SET status='paused', hold_reason='completion_blocked', status_since=now() WHERE id=$1`, f.runID)
	}
	t.Run("retires with the decision", func(t *testing.T) {
		f := newPublicationParkFixture(t, time.Hour)
		blocked(f)
		holdsBefore := f.openHolds(t)
		if _, err := f.svc.ContinueCompletionDecision(f.env.ctx, f.userID, f.runID, "go on"); err != nil {
			t.Fatal(err)
		}
		if _, ok := activeIntentSeq(t, f.env, f.runID); ok {
			t.Fatal("intent still active after the owner's continue")
		}
		var state, reason string
		if err := f.env.pool.QueryRow(f.env.ctx, `SELECT state, retired_reason FROM run_publication_resume WHERE run_id=$1 AND enrollment_seq=1`, f.runID).Scan(&state, &reason); err != nil ||
			state != "retired" || reason != "owner_completion_continue" {
			t.Fatalf("state=%s reason=%s err=%v", state, reason, err)
		}
		if r := f.run(t); r.Status != "queued" {
			t.Fatalf("status=%s", r.Status)
		}
		if f.openHolds(t) != holdsBefore {
			t.Fatal("custody changed")
		}
		// The next finalize enrolls afresh, with the next sequence, at a later generation.
		f.reclaim(t, 2)
		f.resumeFlight(t)
		if got := f.enrollAt(t, 2); got.EnrollmentSeq != 2 || got.State != "active" {
			t.Fatalf("re-enroll=%+v", got)
		}
		// A write against the retired sequence is refused.
		c := pubHeadC
		_, err := f.svc.RegisterPublicationIntent(f.env.ctx, f.wkr, f.runID,
			PublicationIntentRequest{ClaimGeneration: 2, EnrollmentSeq: seq(1), CandidateSeq: seq(1), Intent: f.intentAt(1, "agent/issue-1", pubHeadA, &c)})
		if !errors.Is(err, ErrPublicationEnrollmentRetired) {
			t.Fatalf("stale write err=%v", err)
		}
	})
	t.Run("a decision that cannot apply leaves the intent", func(t *testing.T) {
		// The run is not completion-blocked, so the decision fails AFTER the retirement statement
		// ran: both must roll back.
		f := newPublicationParkFixture(t, time.Hour)
		if _, err := f.svc.ContinueCompletionDecision(f.env.ctx, f.userID, f.runID, ""); !errors.Is(err, ErrCompletionNotBlocked) {
			t.Fatalf("err=%v", err)
		}
		if seq, ok := activeIntentSeq(t, f.env, f.runID); !ok || seq != 1 {
			t.Fatalf("a failed continue retired the intent: %d/%v", seq, ok)
		}
	})
}
