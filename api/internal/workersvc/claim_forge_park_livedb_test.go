package workersvc

import (
	"context"
	"crypto/rand"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/privcheck"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// claim_forge_park_livedb_test.go is the candidate-side coverage of issue #4689: the claim-time forge
// park's store writer, its retry boundaries, every fence that must keep it from writing, the
// rollback and visibility rules of its transaction, and the terminal controls that must keep failing.
// The regression file next to it pins the headline behaviour on both sides of the fix. Skipped unless
// UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via ./e2e/run-store-it.sh.

// cfpFix is a started run requeued as a parked forge run would be, owned by a fresh custody fixture
// whose service carries the transient guard block.
type cfpFix struct {
	t    *testing.T
	env  codexTestEnv
	f    *custodyContinuationFixture
	run  uuid.UUID
	rec  *readmitRecorder
	life *cfprLifecycle
	mid  string // the run snapshot taken right after the afterAssembly mutation
}

func newCFPFix(t *testing.T) *cfpFix {
	t.Helper()
	env := setupCodexLiveDB(t)
	f := newCustodyContinuationFixture(t, env)
	x := &cfpFix{t: t, env: env, f: f, rec: newReadmitRecorder(), life: &cfprLifecycle{}}
	x.run = cfprStartedQueuedRun(t, f)
	f.svc.SetRepoGuard(&fakeGuard{res: transientGuardBlock()})
	f.svc.SetBroadcaster(x.rec)
	f.svc.SetLifecycle(x.life)
	return x
}

// hooks arms afterAssembly with mutate (run after ClaimRun committed, before the finisher) and
// records the run snapshot the finisher starts from.
func (x *cfpFix) hooks(mutate func(ctx context.Context, run store.Run), h *claimTestHooks) {
	x.t.Helper()
	if h == nil {
		h = &claimTestHooks{}
	}
	h.afterAssembly = func(ctx context.Context, run store.Run, _ *ClaimPayload, _ error) {
		if mutate != nil {
			mutate(ctx, run)
		}
		x.mid = cfpSnap(x.t, x.env, run.ID)
	}
	x.f.svc.claimHooks = h
}

func (x *cfpFix) claim() (*ClaimPayload, error) {
	x.t.Helper()
	return x.f.svc.Claim(x.env.ctx, x.f.wkr, nil)
}

func (x *cfpFix) published() (broadcast, lifecycle []string) {
	_, broadcast = x.rec.published(x.run)
	return broadcast, x.life.statuses()
}

func (x *cfpFix) wantNoPublish() {
	x.t.Helper()
	if b, l := x.published(); len(b) != 0 || len(l) != 0 {
		x.t.Fatalf("published broadcast=%v lifecycle=%v, want nothing", b, l)
	}
}

func (x *cfpFix) wantPublished(status string) {
	x.t.Helper()
	b, l := x.published()
	if len(b) != 1 || b[0] != status || len(l) != 1 || l[0] != status {
		x.t.Fatalf("published broadcast=%v lifecycle=%v, want exactly [%s] each", b, l, status)
	}
}

// cfpSnap renders the run columns every claim-finish outcome could move, as one comparable string.
func cfpSnap(t *testing.T, env codexTestEnv, id uuid.UUID) string {
	t.Helper()
	var s string
	if err := env.pool.QueryRow(env.ctx, `SELECT concat_ws('|', status, worker_id, claim_generation, codex_claim_epoch,
	    COALESCE(encode(codex_cap_hash,'hex'),''), recovery_wait_count, forge_park_count, COALESCE(recovery_wait_cause,''),
	    COALESCE(recovery_wait_phase,''), COALESCE(recovery_retry_not_before::text,''), COALESCE(hold_reason,''), health,
	    policy_pending_fresh, COALESCE(policy_fresh_consumed_generation::text,''), COALESCE(fail_origin,''),
	    COALESCE(claim_released_at::text,''), COALESCE(started_at::text,''), COALESCE(session_id,''),
	    COALESCE(stop_kind,''), status_since)
	    FROM runs WHERE id=$1`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// cfpSide renders the run's active publication intent row; empty when it has none.
func cfpSide(t *testing.T, env codexTestEnv, id uuid.UUID) string {
	t.Helper()
	var s string
	err := env.pool.QueryRow(env.ctx, `SELECT concat_ws('|', state, worker_id, enrollment_seq, episode, park_total,
	    COALESCE(wait_started_at::text,''), COALESCE(first_wait_at::text,''), COALESCE(hold_reason,''),
	    COALESCE(handoff::text,''), candidate_seq, claim_generation)
	    FROM run_publication_resume WHERE run_id=$1 AND state='active'`, id).Scan(&s)
	if errors.Is(err, pgx.ErrNoRows) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// cfpHolds maps each custody hold generation of the run to its state.
func cfpHolds(t *testing.T, env codexTestEnv, id uuid.UUID) map[int64]string {
	t.Helper()
	rows, err := env.pool.Query(env.ctx, `SELECT generation, state FROM recovery_custody_holds WHERE run_id=$1`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var g int64
		var st string
		if err := rows.Scan(&g, &st); err != nil {
			t.Fatal(err)
		}
		out[g] = st
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// wantHolds asserts the predecessor (generation 1) hold and this claim's (generation 2) hold states.
func wantHolds(t *testing.T, env codexTestEnv, id uuid.UUID, gen1, gen2 string) {
	t.Helper()
	got := cfpHolds(t, env, id)
	if got[1] != gen1 || got[2] != gen2 {
		t.Fatalf("custody holds = %v, want generation 1 %q and generation 2 %q", got, gen1, gen2)
	}
}

// wantReleasedNoAdopted asserts this claim's hold was released as no_adopted_source.
func wantReleasedNoAdopted(t *testing.T, env codexTestEnv, id uuid.UUID) {
	t.Helper()
	var st string
	var ev *string
	if err := env.pool.QueryRow(env.ctx, `SELECT state, release_evidence FROM recovery_custody_holds WHERE run_id=$1 AND generation=2`, id).Scan(&st, &ev); err != nil {
		t.Fatal(err)
	}
	if st != "released" || ev == nil || *ev != "no_adopted_source" {
		t.Fatalf("generation 2 hold = %s/%v, want released/no_adopted_source", st, ev)
	}
}

func (x *cfpFix) mustPark() store.Run {
	x.t.Helper()
	payload, err := x.claim()
	if err != nil || payload != nil {
		x.t.Fatalf("Claim = (%v, %v), want an idle park", payload != nil, err)
	}
	r := mustRun(x.t, x.env, x.run)
	if r.Status != "recovery_wait" || r.RecoveryWaitCause.String != "forge_unreachable" {
		x.t.Fatalf("status=%s cause=%v fail_origin=%v, want recovery_wait/forge_unreachable", r.Status, r.RecoveryWaitCause, r.FailOrigin)
	}
	return r
}

// wantUnwritten asserts a claim that returned (nil, nil) or an error left the run exactly as the
// afterAssembly seam did, with no publication.
func (x *cfpFix) wantUnwritten(what string) {
	x.t.Helper()
	if got := cfpSnap(x.t, x.env, x.run); got != x.mid {
		x.t.Fatalf("%s wrote the run:\nbefore %s\nafter  %s", what, x.mid, got)
	}
	x.wantNoPublish()
}

// ---- store-level writer ----

func TestParkClaimForgeUnreachableStoreLiveDB(t *testing.T) {
	type tc struct {
		name   string
		mutate string
		pub    bool
		retry  bool
		parks  bool
		want   int64
	}
	for _, c := range []tc{
		{name: "matching claim", want: 1, retry: true, parks: true},
		{name: "owner hold without a count", want: 1, parks: false},
		{name: "never started", mutate: "first_started_at = NULL", want: 0, retry: true, parks: true},
		{name: "stamped stop", mutate: "stop_kind = 'cancelled'", want: 0, retry: true, parks: true},
		{name: "released claim", mutate: "claim_released_at = now()", want: 0, retry: true, parks: true},
		{name: "wrong generation", mutate: "claim_generation = 3", want: 0, retry: true, parks: true},
		{name: "publication flag without an intent", pub: true, want: 0, retry: true},
		{name: "not claimed", mutate: "status = 'queued'", want: 0, retry: true, parks: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			f := newCustodyContinuationFixture(t, env)
			id := f.seedRun("claimed", 2)
			hash := make([]byte, 32)
			if _, err := rand.Read(hash); err != nil {
				t.Fatal(err)
			}
			env.exec(`UPDATE runs SET worker_id=$2, claimed_at=now(), started_at=now(), first_started_at=now(), codex_cap_hash=$3,
				recovery_wait_count=4, forge_park_count=2 WHERE id=$1`, id, f.workerID, hash)
			if c.mutate != "" {
				env.exec(`UPDATE runs SET `+c.mutate+` WHERE id=$1`, id)
			}
			before := mustRun(t, env, id)
			params := store.ParkClaimForgeUnreachableParams{
				ID: id, WorkerID: pgconv.UUID(f.workerID), ClaimGeneration: 2, Publication: c.pub, CountPark: c.parks,
			}
			if c.retry {
				params.RetryNotBefore = pgconv.Time(time.Now().Add(time.Minute))
			}
			n, err := env.q.ParkClaimForgeUnreachable(env.ctx, params)
			if err != nil || n != c.want {
				t.Fatalf("ParkClaimForgeUnreachable = %d, %v, want %d rows", n, err, c.want)
			}
			after := mustRun(t, env, id)
			if c.want == 0 {
				if after.Status != before.Status || after.CodexClaimEpoch != before.CodexClaimEpoch || after.ForgeParkCount != before.ForgeParkCount {
					t.Fatalf("a 0-row park moved the run: %s/%d/%d -> %s/%d/%d", before.Status, before.CodexClaimEpoch, before.ForgeParkCount,
						after.Status, after.CodexClaimEpoch, after.ForgeParkCount)
				}
				return
			}
			if after.Status != "recovery_wait" || after.RecoveryWaitCause.String != "forge_unreachable" || after.RecoveryWaitPhase.Valid {
				t.Fatalf("status=%s cause=%v phase=%v", after.Status, after.RecoveryWaitCause, after.RecoveryWaitPhase)
			}
			if len(after.CodexCapHash) != 0 || after.CodexClaimEpoch != before.CodexClaimEpoch+1 {
				t.Fatalf("capability not revoked: hash=%x epoch=%d (was %d)", after.CodexCapHash, after.CodexClaimEpoch, before.CodexClaimEpoch)
			}
			wantParks := before.ForgeParkCount
			if c.parks {
				wantParks++
			}
			if after.ForgeParkCount != wantParks || after.RecoveryWaitCount != before.RecoveryWaitCount+1 || after.RecoveryRetryNotBefore.Valid != c.retry {
				t.Fatalf("forge_park_count=%d recovery_wait_count=%d retry=%v", after.ForgeParkCount, after.RecoveryWaitCount, after.RecoveryRetryNotBefore)
			}
			if after.ClaimReleasedAt.Valid || after.WorkerID != before.WorkerID || !after.StartedAt.Time.Equal(before.StartedAt.Time) {
				t.Fatalf("claim_released_at=%v worker=%v started_at=%v, want the affinity kept and the claim unreleased", after.ClaimReleasedAt, after.WorkerID, after.StartedAt)
			}
		})
	}
}

// ---- parks ----

// cfpPromote returns a parked enrolled run to queued: the promoter's transition, with the retry
// stamp in the past and the failing origin's cooldown expired.
func cfpPromote(t *testing.T, f *publicationParkFixture) {
	t.Helper()
	f.expireCooldown()
	f.env.exec(`UPDATE runs SET recovery_retry_not_before=now()-interval '1 hour' WHERE id=$1`, f.runID)
	if _, err := f.env.q.PromoteRecoveryWaitRuns(f.env.ctx, pgconv.Time(time.Now())); err != nil {
		t.Fatal(err)
	}
	if got := f.run(t).Status; got != "queued" {
		t.Fatalf("promoted status = %s, want queued", got)
	}
}

// cfpEnrolledQueued is a started, publication-enrolled run parked once at publish (so its wait clock
// is set at t0) and promoted back to queued, on a service that fails the guard transiently.
func cfpEnrolledQueued(t *testing.T, bound time.Duration) (*publicationParkFixture, time.Time) {
	t.Helper()
	f := newPublicationParkFixture(t, bound)
	f.env.exec(`UPDATE runs SET first_started_at=now()-interval '1 hour' WHERE id=$1`, f.runID)
	t0 := f.clock
	if _, applied, err := f.park(1, "git"); err != nil || !applied {
		t.Fatalf("park at publish: %v/%v", applied, err)
	}
	cfpPromote(t, f)
	f.svc.SetRepoGuard(&fakeGuard{res: transientGuardBlock()})
	return f, t0
}

func TestClaimForgeTransientParksHandoffPeerLiveDB(t *testing.T) {
	f := newPublicationParkFixture(t, time.Hour)
	f.env.exec(`UPDATE runs SET first_started_at=now()-interval '1 hour', checkpoint_tip=$2 WHERE id=$1`, f.runID, pubHeadC)
	req := pubParkReport(1, "git", "push")
	req.HandoffCheckpointTip = strPtr(pubHeadC)
	if _, applied, err := f.svc.SetState(f.env.ctx, f.wkr, f.runID, req); err != nil || !applied {
		t.Fatalf("park with handoff: %v/%v", applied, err)
	}
	cfpPromote(t, f)
	f.env.exec(`UPDATE runs SET updated_at=now()-interval '30 days' WHERE id=$1`, f.runID)
	peer := f.peerWorker(t)
	f.svc.SetRepoGuard(&fakeGuard{res: transientGuardBlock()})
	sideBefore := cfpSide(t, f.env, f.runID)
	handoffBefore := string(f.side(t).Handoff)
	if handoffBefore == "" || !strings.Contains(handoffBefore, pubHeadC) {
		t.Fatalf("fixture handoff = %q, want one carrying the checkpoint tip %s", handoffBefore, pubHeadC)
	}

	if p, err := f.svc.Claim(f.env.ctx, peer, nil); err != nil || p != nil {
		t.Fatalf("peer Claim = (%v, %v), want an idle park", p != nil, err)
	}
	r := f.run(t)
	if r.Status != "recovery_wait" || r.RecoveryWaitPhase.String != "publication" || r.WorkerID != pgconv.UUID(peer.ID) {
		t.Fatalf("status=%s phase=%v worker=%v, want recovery_wait/publication parked on the peer %s", r.Status, r.RecoveryWaitPhase, r.WorkerID, peer.ID)
	}
	side := f.side(t)
	if side.WorkerID.Bytes != f.workerID || string(side.Handoff) != handoffBefore || side.ParkTotal != 2 {
		t.Fatalf("side row custodian/handoff/park_total = %v/%s/%d, want the custodian and handoff kept and one more park", side.WorkerID, side.Handoff, side.ParkTotal)
	}
	if cfpSide(t, f.env, f.runID) == sideBefore {
		t.Fatal("the park did not count on the side row")
	}
}

func TestClaimForgeTransientParksUnresolvedPolicyOutcomeLiveDB(t *testing.T) {
	x := newCFPFix(t)
	x.env.exec(`UPDATE runs SET policy_pending_fresh=false, policy_resolved_at=NULL WHERE id=$1`, x.run)
	before := mustRun(t, x.env, x.run)
	r := x.mustPark()
	if !r.PolicyOutcomeID.Valid || r.PolicyOutcomeID != before.PolicyOutcomeID || r.PolicyResolvedAt.Valid || !r.PolicyPreserveRequired {
		t.Fatalf("policy outcome moved: %s", freshStamps(r))
	}
	if n := cfprRestoreAudits(t, x.env, x.run); n != 0 {
		t.Fatalf("restore audit rows = %d, want 0 (no Fresh grant was consumed)", n)
	}
	x.wantPublished("recovery_wait")
}

// ---- terminal controls ----

func TestClaimForgeGuardTerminalControlsLiveDB(t *testing.T) {
	unreadable := func(msg string) privcheck.Finding {
		return privcheck.Finding{Code: privcheck.CodeProtectionUnreadable, Severity: privcheck.SeverityBlock, Message: msg}
	}
	unprotected := privcheck.Finding{Code: privcheck.CodeDefaultBranchUnprotected, Severity: privcheck.SeverityBlock, Message: "default branch is not protected"}
	for _, c := range []struct {
		name   string
		res    privcheck.GuardResult
		mutate string
	}{
		{"auth class", privcheck.GuardResult{Blocked: true, Findings: []privcheck.Finding{unreadable("could not verify the bot token against the forge")}, ForgeRetryClass: forge.ErrorClassAuth}, ""},
		{"unprotected branch", privcheck.GuardResult{Blocked: true, Findings: []privcheck.Finding{unprotected}}, ""},
		{"client build refusal has no class", privcheck.GuardResult{Blocked: true, Findings: []privcheck.Finding{unreadable("could not build a forge client for this connection")}}, ""},
		{"never started with the network class", transientGuardBlock(), "first_started_at = NULL, started_at = NULL"},
		{"mixed network class and unprotected branch", privcheck.GuardResult{Blocked: true,
			Findings: []privcheck.Finding{unreadable("could not verify the bot token against the forge"), unprotected}, ForgeRetryClass: forge.ErrorClassNetwork}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			x := newCFPFix(t)
			x.f.svc.SetRepoGuard(&fakeGuard{res: c.res})
			if c.mutate != "" {
				x.env.exec(`UPDATE runs SET `+c.mutate+` WHERE id=$1`, x.run)
			}
			before := mustRun(t, x.env, x.run)
			payload, err := x.claim()
			if err != nil || payload != nil {
				t.Fatalf("Claim = (%v, %v), want an idle terminal failure", payload != nil, err)
			}
			r := mustRun(t, x.env, x.run)
			if r.Status != "failed" || r.FailOrigin.String != "guardrail_blocked" || !strings.Contains(r.FailureReason.String, errGuardrailBlockedClaim.Error()) {
				t.Fatalf("status=%s fail_origin=%v reason=%q, want failed/guardrail_blocked", r.Status, r.FailOrigin, r.FailureReason.String)
			}
			if r.ForgeParkCount != before.ForgeParkCount || r.RecoveryWaitCause.Valid {
				t.Fatalf("a terminal control counted a forge park or set a cause: %d %v", r.ForgeParkCount, r.RecoveryWaitCause)
			}
			wantHolds(t, x.env, x.run, "open", "released")
			wantReleasedNoAdopted(t, x.env, x.run)
			if b, l := x.published(); len(b) != 0 || len(l) != 1 || l[0] != "failed" {
				t.Fatalf("published broadcast=%v lifecycle=%v, want only the failed notification", b, l)
			}
		})
	}
}

// ---- retry boundaries ----

func TestClaimForgeTransientOrdinaryRetryBoundariesLiveDB(t *testing.T) {
	for _, c := range []struct {
		name         string
		max          int
		count        int
		wantTimed    bool
		wantCountInc int32
	}{
		{"below the cap", 2, 1, true, 1},
		{"at the cap holds without counting", 2, 2, false, 0},
		{"above the cap holds without counting", 2, 9, false, 0},
		{"cap 0 is unlimited and still counts", 0, 100, true, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			x := newCFPFix(t)
			x.f.svc.p.RunForgeUnreachableMaxParks = c.max
			x.env.exec(`UPDATE runs SET forge_park_count=$2 WHERE id=$1`, x.run, c.count)
			before := mustRun(t, x.env, x.run)
			r := x.mustPark()
			if r.RecoveryRetryNotBefore.Valid != c.wantTimed || r.ForgeParkCount != before.ForgeParkCount+c.wantCountInc ||
				r.RecoveryWaitCount != before.RecoveryWaitCount+1 {
				t.Fatalf("retry=%v forge_park_count=%d (was %d) recovery_wait_count=%d, want timed=%v and +%d",
					r.RecoveryRetryNotBefore, r.ForgeParkCount, before.ForgeParkCount, r.RecoveryWaitCount, c.wantTimed, c.wantCountInc)
			}
			x.wantPublished("recovery_wait")
		})
	}
}

func TestClaimForgeTransientPublicationRetryBoundariesLiveDB(t *testing.T) {
	const bound = time.Hour
	for _, c := range []struct {
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
		t.Run(c.name, func(t *testing.T) {
			f, t0 := cfpEnrolledQueued(t, c.bound)
			f.clock = t0.Add(c.advance)
			before := f.run(t)
			if p, err := f.svc.Claim(f.env.ctx, f.wkr, nil); err != nil || p != nil {
				t.Fatalf("Claim = (%v, %v), want an idle park", p != nil, err)
			}
			r, s := f.run(t), f.side(t)
			if r.Status != "recovery_wait" || r.RecoveryWaitPhase.String != "publication" || r.ForgeParkCount != before.ForgeParkCount {
				t.Fatalf("status=%s phase=%v forge_park_count=%d", r.Status, r.RecoveryWaitPhase, r.ForgeParkCount)
			}
			if c.hold {
				if r.RecoveryRetryNotBefore.Valid || s.HoldReason.String != "publication_retry_exhausted" {
					t.Fatalf("want an owner hold: retry=%v side hold=%v", r.RecoveryRetryNotBefore, s.HoldReason)
				}
			} else if !r.RecoveryRetryNotBefore.Valid || s.HoldReason.Valid {
				t.Fatalf("want a timed retry: retry=%v side hold=%v", r.RecoveryRetryNotBefore, s.HoldReason)
			}
			if !s.WaitStartedAt.Time.Equal(t0) || s.ParkTotal != 2 || s.Episode != 0 {
				t.Fatalf("clock or counters: wait_started_at=%v park_total=%d episode=%d, want the first park's clock kept and park_total 2", s.WaitStartedAt, s.ParkTotal, s.Episode)
			}
		})
	}
}

func TestClaimForgeTransientFirstParkAfterOwnerResumeLiveDB(t *testing.T) {
	const bound = time.Hour
	f := newPublicationParkFixture(t, bound)
	f.env.exec(`UPDATE runs SET first_started_at=now()-interval '1 hour' WHERE id=$1`, f.runID)
	t0 := f.clock
	if _, applied, err := f.park(1, "git"); err != nil || !applied {
		t.Fatal(err)
	}
	f.resumeFlight(t)
	f.clock = t0.Add(bound)
	if run, applied, err := f.park(1, "git"); err != nil || !applied || run.RecoveryRetryNotBefore.Valid {
		t.Fatalf("owner hold park: %v/%v stamp=%v", applied, err, run.RecoveryRetryNotBefore)
	}
	if ok, err := f.svc.ResumePublicationForgeHold(f.env.ctx, f.userID, f.run(t)); err != nil || !ok {
		t.Fatalf("owner Resume = %v, %v", ok, err)
	}
	if s := f.side(t); s.WaitStartedAt.Valid || s.Episode != 1 {
		t.Fatalf("after Resume wait_started_at=%v episode=%d, want a restarted clock and episode 1", s.WaitStartedAt, s.Episode)
	}
	f.expireCooldown()
	f.svc.SetRepoGuard(&fakeGuard{res: transientGuardBlock()})
	f.clock = t0.Add(bound + 10*time.Minute)

	if p, err := f.svc.Claim(f.env.ctx, f.wkr, nil); err != nil || p != nil {
		t.Fatalf("Claim = (%v, %v), want an idle park", p != nil, err)
	}
	r, s := f.run(t), f.side(t)
	if r.Status != "recovery_wait" || !r.RecoveryRetryNotBefore.Valid || s.HoldReason.Valid {
		t.Fatalf("status=%s retry=%v side hold=%v, want a backoff park with no side hold", r.Status, r.RecoveryRetryNotBefore, s.HoldReason)
	}
	if !s.WaitStartedAt.Time.Equal(f.clock) || s.ParkTotal != 3 || s.Episode != 1 {
		t.Fatalf("wait_started_at=%v park_total=%d episode=%d, want the claim's clock, 3 parks, episode 1", s.WaitStartedAt, s.ParkTotal, s.Episode)
	}
}

// ---- fences ----

func TestClaimForgeTransientIdleFencesLiveDB(t *testing.T) {
	for _, c := range []struct {
		name   string
		mutate func(t *testing.T, x *cfpFix, ctx context.Context, run store.Run)
	}{
		{"released claim", func(t *testing.T, x *cfpFix, _ context.Context, run store.Run) {
			x.env.exec(`UPDATE runs SET claim_released_at=now() WHERE id=$1`, run.ID)
		}},
		{"stale generation", func(t *testing.T, x *cfpFix, _ context.Context, run store.Run) {
			x.env.exec(`UPDATE runs SET claim_generation=claim_generation+1 WHERE id=$1`, run.ID)
		}},
		{"stale epoch", func(t *testing.T, x *cfpFix, _ context.Context, run store.Run) {
			x.env.exec(`UPDATE runs SET codex_claim_epoch=codex_claim_epoch+1 WHERE id=$1`, run.ID)
		}},
		{"hash only at the same epoch", func(t *testing.T, x *cfpFix, _ context.Context, run store.Run) {
			hash := make([]byte, 32)
			if _, err := rand.Read(hash); err != nil {
				t.Fatal(err)
			}
			x.env.exec(`UPDATE runs SET codex_cap_hash=$2 WHERE id=$1`, run.ID, hash)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			x := newCFPFix(t)
			x.hooks(func(ctx context.Context, run store.Run) { c.mutate(t, x, ctx, run) }, nil)
			payload, err := x.claim()
			if err != nil || payload != nil {
				t.Fatalf("Claim = (%v, %v), want idle", payload != nil, err)
			}
			x.wantUnwritten(c.name)
			if got := cfpHolds(t, x.env, x.run); got[2] != "open" {
				t.Fatalf("an idle claim touched this claim's custody hold: %v", got)
			}
		})
	}
}

// A stop verdict stamped before the finisher is not a park: the existing terminal arm decides, and
// the publication-intent read hook is never reached.
func TestClaimForgeTransientStampedStopStaysTerminalLiveDB(t *testing.T) {
	x := newCFPFix(t)
	reached := false
	x.hooks(func(_ context.Context, run store.Run) {
		x.env.exec(`UPDATE runs SET stop_kind='cancelled' WHERE id=$1`, run.ID)
	}, &claimTestHooks{intentRead: func() error { reached = true; return errors.New("intent read must not run") }})
	payload, err := x.claim()
	if err != nil || payload != nil {
		t.Fatalf("Claim = (%v, %v), want an idle terminal failure", payload != nil, err)
	}
	if reached {
		t.Fatal("the intent read ran for a run with a stamped stop")
	}
	r := mustRun(t, x.env, x.run)
	if r.Status != "failed" || r.FailOrigin.String != "guardrail_blocked" {
		t.Fatalf("status=%s fail_origin=%v, want failed/guardrail_blocked", r.Status, r.FailOrigin)
	}
	wantHolds(t, x.env, x.run, "open", "released")
	wantReleasedNoAdopted(t, x.env, x.run)
}

// A fresh claim refuses its transfer; it never parks the run and never reads the publication intent.
func TestClaimForgeTransientFreshClaimRefusesTransferLiveDB(t *testing.T) {
	f := newFCFix(t, true)
	frid := f.pending()
	tupleBefore := f.fcTuple()
	f.svc.SetRepoGuard(&fakeGuard{res: transientGuardBlock()})
	reached := false
	f.svc.claimHooks = &claimTestHooks{intentRead: func() error { reached = true; return errors.New("intent read must not run") }}
	if p, err := f.claimAs(f.w2); err != nil || p != nil {
		t.Fatalf("Claim = (%v, %v), want an idle poll", p != nil, err)
	}
	if reached {
		t.Fatal("the intent read ran on a fresh claim")
	}
	if row := f.transfer(frid); row.State != "refused" || row.RefusalReason.String != freshRefusalAssemblyFailed {
		t.Fatalf("transfer = %q/%q, want refused/%s", row.State, row.RefusalReason.String, freshRefusalAssemblyFailed)
	}
	if after := f.fcTuple(); after != tupleBefore {
		t.Fatalf("the run did not return to its held state:\nbefore %s\nafter  %s", tupleBefore, after)
	}
	f.wantOnlyRecoveryWaitPublished()
	var forgeParks int32
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT forge_park_count FROM runs WHERE id=$1`, f.run).Scan(&forgeParks); err != nil || forgeParks != 0 {
		t.Fatalf("forge_park_count = %d (%v), want 0", forgeParks, err)
	}
}

// A request context that ends before the finisher opens its transaction leaves the run claimed for
// the sweeper: nothing is written and nothing is published.
func TestClaimForgeTransientExpiredContextStaysClaimedLiveDB(t *testing.T) {
	x := newCFPFix(t)
	ctx, cancel := context.WithCancel(x.env.ctx)
	defer cancel()
	x.hooks(func(context.Context, store.Run) { cancel() }, nil)
	payload, err := x.f.svc.Claim(ctx, x.f.wkr, nil)
	if err == nil || payload != nil {
		t.Fatalf("Claim = (%v, %v), want an error", payload != nil, err)
	}
	if r := mustRun(t, x.env, x.run); r.Status != "claimed" {
		t.Fatalf("status = %s, want claimed", r.Status)
	}
	x.wantUnwritten("an expired context")
	wantHolds(t, x.env, x.run, "open", "open")
}

// cfpDeadlineCtx is a request context whose deadline passes on demand: expire closes Done and from then
// on Err is context.DeadlineExceeded and Deadline is in the past. It stands in for a WithTimeout whose
// timer fires at a chosen instant, without waiting on a real one.
type cfpDeadlineCtx struct {
	context.Context
	mu   sync.Mutex
	done chan struct{}
	at   time.Time
}

func newCFPDeadlineCtx(parent context.Context) *cfpDeadlineCtx {
	return &cfpDeadlineCtx{Context: parent, done: make(chan struct{})}
}

func (c *cfpDeadlineCtx) expire() {
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-c.done:
	default:
		c.at = time.Now().Add(-time.Second)
		close(c.done)
	}
}

func (c *cfpDeadlineCtx) Done() <-chan struct{} { return c.done }

func (c *cfpDeadlineCtx) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return c.Context.Err()
	}
}

func (c *cfpDeadlineCtx) Deadline() (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.at.IsZero() {
		return c.Context.Deadline()
	}
	return c.at, true
}

// The same fence for a request deadline that expires (context.DeadlineExceeded, not Canceled) between
// the assembly and the finisher: the run stays claimed, nothing is written, nothing is published.
func TestClaimForgeTransientExpiredDeadlineStaysClaimedLiveDB(t *testing.T) {
	x := newCFPFix(t)
	ctx := newCFPDeadlineCtx(x.env.ctx)
	// The deadline "expires" deterministically inside the hook: no real timer is waited on.
	x.hooks(func(context.Context, store.Run) { ctx.expire() }, nil)
	payload, err := x.f.svc.Claim(ctx, x.f.wkr, nil)
	if err == nil || payload != nil {
		t.Fatalf("Claim = (%v, %v), want an error", payload != nil, err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Claim error = %v, want it to wrap DeadlineExceeded", err)
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("request context error = %v, want DeadlineExceeded", ctx.Err())
	}
	if r := mustRun(t, x.env, x.run); r.Status != "claimed" {
		t.Fatalf("status = %s, want claimed", r.Status)
	}
	x.wantUnwritten("an expired deadline")
	wantHolds(t, x.env, x.run, "open", "open")
}

func TestClaimForgeTransientRollbackFencesLiveDB(t *testing.T) {
	for _, c := range []struct {
		name    string
		mutate  func(t *testing.T, x *cfpFix, run store.Run)
		wantErr error
	}{
		{"an active fresh transfer row on a non-fresh claim", func(t *testing.T, x *cfpFix, run store.Run) {
			x.env.exec(`INSERT INTO run_fresh_resumes (run_id, user_id, request_id, evidence_generation, evidence_kind, checkpoint_ref, sha, candidate_token, state)
			    VALUES ($1, $2, $3, 1, 'checkpoint_ref', 'refs/uzi-checkpoints/x', $4, 'tok', 'pending')`,
				run.ID, x.f.userID, uuid.New(), freshSha('c'))
		}, errClaimRecoveryStale},
		{"a capture-bearing hold", func(t *testing.T, x *cfpFix, run store.Run) {
			x.env.exec(`INSERT INTO recovery_captures (id, hold_id, run_id, user_id, original_worker_identity, source_sha, idempotency_key, state)
			    SELECT gen_random_uuid(), h.id, h.run_id, h.user_id, 'fixture', 'abc', gen_random_uuid()::text, 'needs_action'
			    FROM recovery_custody_holds h WHERE h.run_id=$1 AND h.generation=2`, run.ID)
		}, errClaimRecoveryCustody},
		{"a custody hold that is not there", func(t *testing.T, x *cfpFix, run store.Run) {
			x.env.exec(`DELETE FROM recovery_custody_holds WHERE run_id=$1 AND generation=2`, run.ID)
		}, errClaimRecoveryCustody},
	} {
		t.Run(c.name, func(t *testing.T) {
			x := newCFPFix(t)
			// The fixture's cleanup drops the owner's holds; captures reference them, so they go first.
			t.Cleanup(func() { x.env.exec(`DELETE FROM recovery_captures WHERE user_id=$1`, x.f.userID) })
			x.hooks(func(_ context.Context, run store.Run) { c.mutate(t, x, run) }, nil)
			payload, err := x.claim()
			if !errors.Is(err, c.wantErr) || payload != nil {
				t.Fatalf("Claim = (%v, %v), want %v", payload != nil, err, c.wantErr)
			}
			if r := mustRun(t, x.env, x.run); r.Status != "claimed" || r.ClaimGeneration != 2 {
				t.Fatalf("status=%s generation=%d, want it left claimed at 2", r.Status, r.ClaimGeneration)
			}
			x.wantUnwritten(c.name)
			if got := cfpHolds(t, x.env, x.run); got[1] != "open" {
				t.Fatalf("predecessor hold = %v", got)
			}
		})
	}
}

// cfpSeedReservation stamps the claim's origin reservation (this claim's) and a newer, foreign one
// on a second origin, returning the foreign origin's observation fields to compare later.
func cfpSeedReservation(t *testing.T, env codexTestEnv, userID, workerID, runID uuid.UUID) (ownOrigin, foreignOrigin string) {
	t.Helper()
	var gen int64
	var origin *string
	if err := env.pool.QueryRow(env.ctx, `SELECT generation, origin FROM run_forge_origins WHERE run_id=$1`, runID).Scan(&gen, &origin); err != nil || origin == nil {
		t.Fatalf("claim stamped no origin: %v (origin=%v)", err, origin)
	}
	own, foreign := uuid.New(), uuid.New()
	env.exec(`INSERT INTO forge_origin_cooldowns (user_id, worker_id, origin, incarnation, cooled_until, last_reason, observed_at,
	    failure_step, observation_version, reservation_run_id, reservation_generation, reservation_incarnation, reservation_version, reservation_observation_version)
	    VALUES ($1,$2,$3,$4,now()+interval '1 hour','transient_clone_fetch_exhausted',now(),3,5,$5,$6,$4,7,5)`, userID, workerID, *origin, own, runID, gen)
	env.exec(`UPDATE run_forge_origins SET reservation_incarnation=$2, reservation_version=7 WHERE run_id=$1`, runID, own)
	foreignOrigin = "https://newer.forge.e2e"
	env.exec(`INSERT INTO forge_origin_cooldowns (user_id, worker_id, origin, incarnation, cooled_until, last_reason, observed_at,
	    failure_step, observation_version, reservation_run_id, reservation_generation, reservation_incarnation, reservation_version, reservation_observation_version)
	    VALUES ($1,$2,$3,$4,now()+interval '1 hour','transient_clone_fetch_exhausted',now(),2,4,$5,9,$4,3,4)`, userID, workerID, foreignOrigin, foreign, uuid.New())
	return *origin, foreignOrigin
}

type cfpCooldown struct {
	ReservationRunID                   pgtype.UUID
	Step                               int
	ObsVersion                         int64
	Reason                             string
	CooledUntil, ObservedAt            time.Time
	ReservationVersion, ResvGeneration *int64
}

func cfpReadCooldown(t *testing.T, env codexTestEnv, workerID uuid.UUID, origin string) cfpCooldown {
	t.Helper()
	var c cfpCooldown
	if err := env.pool.QueryRow(env.ctx, `SELECT reservation_run_id, failure_step, observation_version, last_reason, cooled_until, observed_at,
	    reservation_version, reservation_generation
	    FROM forge_origin_cooldowns WHERE worker_id=$1 AND origin=$2`, workerID, origin).
		Scan(&c.ReservationRunID, &c.Step, &c.ObsVersion, &c.Reason, &c.CooledUntil, &c.ObservedAt, &c.ReservationVersion, &c.ResvGeneration); err != nil {
		t.Fatal(err)
	}
	return c
}

// cfpFence says how a reservation of this claim is made to differ in one fence column; the zero value
// is a matching reservation.
type cfpFence struct {
	gitVersionDelta int64 // added to the Git row's reservation_version (the run's own stamp stays 7)
	apiVersionDelta int64 // added to the API row's reservation_version (the run's own stamp stays 11)
	gitGenDelta     int64 // added to the Git row's reservation_generation
	apiGenDelta     int64 // added to the API row's reservation_generation
	gitOtherRun     bool  // the Git row is reserved by a different run at this claim's generation
	apiOtherRun     bool  // the API row is reserved by a different run at this claim's generation
	gitOtherInc     bool  // the run stamps a Git reservation incarnation other than its row's
	apiOtherInc     bool  // the run stamps an API reservation incarnation other than its row's
}

// cfpSeedBothReservations reserves BOTH of this claim's origins (the Git origin and the distinct GitHub
// API origin), stamps the run's own reservation identity for each, and adds a reservation of another
// run on a third origin. forge_origin_cooldowns has one row per (user, worker, origin), so a "newer"
// reservation of the same origin cannot sit beside this claim's; a row at the same key whose fence
// column differs (fence) stands in for it.
func cfpSeedBothReservations(t *testing.T, env codexTestEnv, userID, workerID, runID uuid.UUID, fence cfpFence) (gitOrigin, apiOrigin, foreignOrigin string) {
	t.Helper()
	var gen int64
	var origin, api *string
	if err := env.pool.QueryRow(env.ctx, `SELECT generation, origin, api_origin FROM run_forge_origins WHERE run_id=$1`, runID).Scan(&gen, &origin, &api); err != nil ||
		origin == nil || api == nil || *api == *origin {
		t.Fatalf("claim stamped no distinct git/api origin pair: %v (origin=%v api=%v)", err, origin, api)
	}
	gitInc, apiInc, foreignInc := uuid.New(), uuid.New(), uuid.New()
	gitRun, apiRun := runID, runID
	if fence.gitOtherRun {
		gitRun = uuid.New()
	}
	if fence.apiOtherRun {
		apiRun = uuid.New()
	}
	// forge_origin_cooldowns_check ties a row's reservation_incarnation to its own incarnation, so the
	// incarnation fence is varied on the run's side: the run stamps a different incarnation than the
	// one its row carries.
	gitStamp, apiStamp := gitInc, apiInc
	if fence.gitOtherInc {
		gitStamp = uuid.New()
	}
	if fence.apiOtherInc {
		apiStamp = uuid.New()
	}
	env.exec(`INSERT INTO forge_origin_cooldowns (user_id, worker_id, origin, incarnation, cooled_until, last_reason, observed_at,
	    failure_step, observation_version, reservation_run_id, reservation_generation, reservation_incarnation, reservation_version, reservation_observation_version)
	    VALUES ($1,$2,$3,$4,now()+interval '1 hour','transient_clone_fetch_exhausted',now(),3,5,$5,$6,$4,$7,5)`,
		userID, workerID, *origin, gitInc, gitRun, gen+fence.gitGenDelta, 7+fence.gitVersionDelta)
	env.exec(`INSERT INTO forge_origin_cooldowns (user_id, worker_id, origin, incarnation, cooled_until, last_reason, observed_at,
	    failure_step, observation_version, reservation_run_id, reservation_generation, reservation_incarnation, reservation_version, reservation_observation_version)
	    VALUES ($1,$2,$3,$4,now()+interval '2 hour','transient_clone_fetch_exhausted',now(),4,6,$5,$6,$4,$7,6)`,
		userID, workerID, *api, apiInc, apiRun, gen+fence.apiGenDelta, 11+fence.apiVersionDelta)
	env.exec(`UPDATE run_forge_origins SET reservation_incarnation=$2, reservation_version=7, api_reservation_incarnation=$3, api_reservation_version=11 WHERE run_id=$1`,
		runID, gitStamp, apiStamp)
	foreignOrigin = "https://newer.forge.e2e"
	env.exec(`INSERT INTO forge_origin_cooldowns (user_id, worker_id, origin, incarnation, cooled_until, last_reason, observed_at,
	    failure_step, observation_version, reservation_run_id, reservation_generation, reservation_incarnation, reservation_version, reservation_observation_version)
	    VALUES ($1,$2,$3,$4,now()+interval '1 hour','transient_clone_fetch_exhausted',now(),2,4,$5,$6,$4,3,4)`,
		userID, workerID, foreignOrigin, foreignInc, uuid.New(), gen+9)
	return *origin, *api, foreignOrigin
}

// cfpCooldownText renders the whole cooldown row, reservation columns included.
func cfpCooldownText(t *testing.T, env codexTestEnv, workerID uuid.UUID, origin string) string {
	t.Helper()
	var s string
	if err := env.pool.QueryRow(env.ctx, `SELECT c::text FROM forge_origin_cooldowns c WHERE worker_id=$1 AND origin=$2`, workerID, origin).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// cfpObservationText renders every observation column (everything but the reservation binding).
func cfpObservationText(t *testing.T, env codexTestEnv, workerID uuid.UUID, origin string) string {
	t.Helper()
	var s string
	if err := env.pool.QueryRow(env.ctx, `SELECT concat_ws('|', user_id, worker_id, origin, incarnation, observation_version, failure_step,
	    cooled_until, last_reason, observed_at, git_unverified, api_unverified) FROM forge_origin_cooldowns WHERE worker_id=$1 AND origin=$2`, workerID, origin).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// The park clears this claim's reservations through the existing trigger (the Git row and the API row,
// each only while its own fence matches), leaves every other reservation alone, and writes no
// observation of its own.
func TestClaimForgeTransientClearsOnlyThisClaimsReservationLiveDB(t *testing.T) {
	for _, c := range []struct {
		name                 string
		fence                cfpFence
		gitCleared, apiClear bool
	}{
		{"both reservations match", cfpFence{}, true, true},
		{"git row at a newer reservation version", cfpFence{gitVersionDelta: 1}, false, true},
		{"api row at a newer generation", cfpFence{apiGenDelta: 1}, true, false},
		{"api row reserved by another run", cfpFence{apiOtherRun: true}, true, false},
		{"api row at a newer reservation version", cfpFence{apiVersionDelta: 1}, true, false},
		{"git row at another generation", cfpFence{gitGenDelta: 1}, false, true},
		{"git row reserved by another run", cfpFence{gitOtherRun: true}, false, true},
		{"git row at another reservation incarnation", cfpFence{gitOtherInc: true}, false, true},
		{"api row at another reservation incarnation", cfpFence{apiOtherInc: true}, true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			x := newCFPFix(t)
			x.env.exec(`UPDATE repos SET web_url='https://github.com/group/repo' WHERE id=$1`, x.f.repoID)
			x.env.exec(`UPDATE forge_connections SET forge_type='github' WHERE user_id=$1`, x.f.userID)
			var gitO, apiO, foreign string
			var gitRow, apiRow, foreignRow, gitObs, apiObs, foreignObs string
			x.hooks(func(_ context.Context, run store.Run) {
				gitO, apiO, foreign = cfpSeedBothReservations(t, x.env, x.f.userID, x.f.workerID, run.ID, c.fence)
				gitRow, apiRow, foreignRow = cfpCooldownText(t, x.env, x.f.workerID, gitO), cfpCooldownText(t, x.env, x.f.workerID, apiO), cfpCooldownText(t, x.env, x.f.workerID, foreign)
				gitObs, apiObs, foreignObs = cfpObservationText(t, x.env, x.f.workerID, gitO), cfpObservationText(t, x.env, x.f.workerID, apiO), cfpObservationText(t, x.env, x.f.workerID, foreign)
			}, nil)
			x.mustPark()

			for _, o := range []struct {
				what        string
				origin      string
				row, obs    string
				wantCleared bool
			}{
				{"git", gitO, gitRow, gitObs, c.gitCleared},
				{"api", apiO, apiRow, apiObs, c.apiClear},
				{"foreign", foreign, foreignRow, foreignObs, false},
			} {
				after := cfpReadCooldown(t, x.env, x.f.workerID, o.origin)
				if got := cfpObservationText(t, x.env, x.f.workerID, o.origin); got != o.obs {
					t.Fatalf("%s: the park changed the observation fields:\nbefore %s\nafter  %s", o.what, o.obs, got)
				}
				if o.wantCleared {
					if after.ReservationRunID.Valid || after.ReservationVersion != nil || after.ResvGeneration != nil {
						t.Fatalf("%s: this claim's reservation not cleared: %s", o.what, cfpCooldownText(t, x.env, x.f.workerID, o.origin))
					}
					continue
				}
				if got := cfpCooldownText(t, x.env, x.f.workerID, o.origin); got != o.row || !after.ReservationRunID.Valid {
					t.Fatalf("%s: a reservation that is not this claim's moved:\nbefore %s\nafter  %s", o.what, o.row, got)
				}
			}
		})
	}
}

func TestClaimForgeTransientClearsCompletionBlockedLiveDB(t *testing.T) {
	x := newCFPFix(t)
	x.hooks(func(_ context.Context, run store.Run) {
		x.env.exec(`UPDATE runs SET hold_reason='completion_blocked' WHERE id=$1`, run.ID)
	}, nil)
	if r := x.mustPark(); r.HoldReason.Valid {
		t.Fatalf("hold_reason = %v, want the completion_blocked annotation cleared", r.HoldReason)
	}
}

// An intent read that fails leaves the run claimed, the intent active and custody open.
func TestClaimForgeTransientIntentReadErrorRollsBackLiveDB(t *testing.T) {
	f, _ := cfpEnrolledQueued(t, time.Hour)
	x := &cfpFix{t: t, env: f.env, f: f.custodyContinuationFixture, run: f.runID, rec: newReadmitRecorder(), life: &cfprLifecycle{}}
	f.svc.SetBroadcaster(x.rec)
	f.svc.SetLifecycle(x.life)
	x.hooks(nil, &claimTestHooks{intentRead: func() error { return errors.New("injected intent read failure") }})
	side := cfpSide(t, f.env, f.runID)
	payload, err := f.svc.Claim(f.env.ctx, f.wkr, nil)
	if err == nil || payload != nil {
		t.Fatalf("Claim = (%v, %v), want the injected error", payload != nil, err)
	}
	if r := f.run(t); r.Status != "claimed" {
		t.Fatalf("status = %s, want claimed", r.Status)
	}
	x.wantUnwritten("an intent read error")
	if got := cfpSide(t, f.env, f.runID); got != side {
		t.Fatalf("intent moved: %q -> %q", side, got)
	}
	wantHolds(t, f.env, f.runID, "open", "open")
}

// ---- rollback rule for the enrolled transaction ----

// cfpEnrolledFresh is cfpEnrolledQueued with a one-claim Fresh grant the next claim consumes, so the
// rollback rule can prove the restore is rolled back with the rest.
func cfpEnrolledFresh(t *testing.T) (*publicationParkFixture, *cfpFix) {
	t.Helper()
	f, _ := cfpEnrolledQueued(t, time.Hour)
	f.wkr.ProtocolCapabilities = append(f.wkr.ProtocolCapabilities, capability.ProviderPolicyHoldV1)
	f.env.exec(`UPDATE workers SET protocol_capabilities=$2 WHERE id=$1`, f.workerID, f.wkr.ProtocolCapabilities)
	f.env.exec(`UPDATE runs SET policy_preserve_required=true, policy_pending_fresh=true, policy_outcome_id=gen_random_uuid(),
		policy_continuation_mode='same_conversation', policy_applying_worker_id=$2,
		policy_source_generation=1, policy_applying_generation=1, policy_resolved_at=now(),
		health='slow', health_reason='fixture', health_since=now() WHERE id=$1`, f.runID, f.workerID)
	f.env.exec(`UPDATE repos SET web_url='https://github.com/group/repo' WHERE id=$1`, f.repoID)
	f.env.exec(`UPDATE forge_connections SET forge_type='github' WHERE user_id=$1`, f.userID)
	x := &cfpFix{t: t, env: f.env, f: f.custodyContinuationFixture, run: f.runID, rec: newReadmitRecorder(), life: &cfprLifecycle{}}
	f.svc.SetBroadcaster(x.rec)
	f.svc.SetLifecycle(x.life)
	return f, x
}

func TestClaimForgeTransientEnrolledSideRowFailureRollsBackLiveDB(t *testing.T) {
	for _, c := range []struct {
		name    string
		zero    bool
		wantErr error
	}{
		{"hook error", false, nil},
		{"zero side-row rows", true, errClaimRecoveryStale},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, x := cfpEnrolledFresh(t)
			calls := 0
			var ownOrigin string
			var ownBefore cfpCooldown
			x.hooks(func(_ context.Context, run store.Run) {
				ownOrigin, _ = cfpSeedReservation(t, f.env, f.userID, f.workerID, run.ID)
				ownBefore = cfpReadCooldown(t, f.env, f.workerID, ownOrigin)
			}, &claimTestHooks{sideRowWrite: func(ctx context.Context, q *store.Queries) error {
				calls++
				if !c.zero {
					return errors.New("injected")
				}
				// Retire the intent inside the transaction so the side-row update matches nothing.
				if err := q.OptInPublicationResume(ctx); err != nil {
					return err
				}
				_, err := q.RetirePublicationResume(ctx, store.RetirePublicationResumeParams{RunID: f.runID, Reason: "owner_completion_continue"})
				return err
			}})
			side := cfpSide(t, f.env, f.runID)
			payload, err := f.svc.Claim(f.env.ctx, f.wkr, nil)
			if err == nil || payload != nil || (c.wantErr != nil && !errors.Is(err, c.wantErr)) {
				t.Fatalf("Claim = (%v, %v), want an error (%v)", payload != nil, err, c.wantErr)
			}
			if calls != 1 {
				t.Fatalf("sideRowWrite ran %d times, want exactly once", calls)
			}
			r := f.run(t)
			if r.Status != "claimed" || r.ClaimGeneration != 2 {
				t.Fatalf("status=%s generation=%d, want it left claimed at 2", r.Status, r.ClaimGeneration)
			}
			x.wantUnwritten(c.name)
			wantHolds(t, f.env, f.runID, "open", "open")
			if r.PolicyPendingFresh || !r.PolicyFreshConsumedGeneration.Valid || r.PolicyFreshConsumedGeneration.Int64 != 2 || cfprRestoreAudits(t, f.env, f.runID) != 0 {
				t.Fatalf("the Fresh restore was not rolled back: %s audits=%d", freshStamps(r), cfprRestoreAudits(t, f.env, f.runID))
			}
			if got := cfpSide(t, f.env, f.runID); got != side {
				t.Fatalf("side row moved: %q -> %q", side, got)
			}
			if after := cfpReadCooldown(t, f.env, f.workerID, ownOrigin); !after.ReservationRunID.Valid || after.ReservationRunID != ownBefore.ReservationRunID {
				t.Fatalf("this claim's reservation was released by a rolled-back park: %+v", after)
			}
		})
	}
}

// ---- visibility ----

func TestClaimForgeTransientParkIsAtomicToOtherReadersLiveDB(t *testing.T) {
	f, x := cfpEnrolledFresh(t)
	reached, release := make(chan struct{}), make(chan struct{})
	var claimEpoch int64
	x.hooks(func(ctx context.Context, run store.Run) {
		if err := f.env.pool.QueryRow(ctx, `SELECT codex_claim_epoch FROM runs WHERE id=$1`, run.ID).Scan(&claimEpoch); err != nil {
			t.Error(err)
		}
	}, &claimTestHooks{beforeClaimParkCommit: func(context.Context) error {
		close(reached)
		<-release
		return nil
	}})
	side := cfpSide(t, f.env, f.runID)
	runBefore, sideBefore := f.run(t), f.side(t)
	type result struct {
		p   *ClaimPayload
		err error
	}
	done := make(chan result, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		p, err := f.svc.Claim(f.env.ctx, f.wkr, nil)
		done <- result{p, err}
	}()
	select {
	case <-reached:
	case <-time.After(30 * time.Second):
		close(release)
		wg.Wait()
		t.Fatal("the claim never reached the park commit")
	}
	reader, err := f.env.pool.BeginTx(f.env.ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		close(release)
		wg.Wait()
		t.Fatal(err)
	}
	defer func() { _ = reader.Rollback(f.env.ctx) }()
	readState := func(q interface {
		QueryRow(context.Context, string, ...any) pgx.Row
	}) (status string, openHolds int, sideRow string) {
		if err := q.QueryRow(f.env.ctx, `SELECT status FROM runs WHERE id=$1`, f.runID).Scan(&status); err != nil {
			t.Error(err)
		}
		if err := q.QueryRow(f.env.ctx, `SELECT count(*) FROM recovery_custody_holds WHERE run_id=$1 AND state='open'`, f.runID).Scan(&openHolds); err != nil {
			t.Error(err)
		}
		if err := q.QueryRow(f.env.ctx, `SELECT concat_ws('|', park_total, COALESCE(hold_reason,''), wait_started_at) FROM run_publication_resume WHERE run_id=$1 AND state='active'`, f.runID).Scan(&sideRow); err != nil {
			t.Error(err)
		}
		return
	}
	status, holds, sideRow := readState(reader)
	preSide := sideRow
	if status != "claimed" || holds != 2 {
		t.Errorf("a concurrent reader saw status=%s open holds=%d, want the pre-park claimed/2", status, holds)
	}
	close(release)
	res := <-done
	wg.Wait()
	if res.err != nil || res.p != nil {
		t.Fatalf("Claim = (%v, %v), want an idle park", res.p != nil, res.err)
	}
	// The reader's snapshot still shows the pre-park state, and a fresh read shows every write at once.
	if status, holds, sideRow = readState(reader); status != "claimed" || holds != 2 || sideRow != preSide {
		t.Errorf("the repeatable-read snapshot moved: %s/%d/%s", status, holds, sideRow)
	}
	r := f.run(t)
	if r.Status != "recovery_wait" || cfpHolds(t, f.env, f.runID)[2] != "released" || cfpSide(t, f.env, f.runID) == side {
		t.Fatalf("after the commit: status=%s holds=%v", r.Status, cfpHolds(t, f.env, f.runID))
	}
	x.wantPublished("recovery_wait")

	// A fresh repeatable-read transaction opened after the commit sees every write of the park in one
	// snapshot: the run, the custody holds and the publication side row together.
	post, err := f.env.pool.BeginTx(f.env.ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = post.Rollback(f.env.ctx) }()
	var (
		pStatus, pCause, pPhase, pHealth string
		pWait, pPark, pEpoch             int64
		pRetry                           bool
	)
	if err := post.QueryRow(f.env.ctx, `SELECT status, COALESCE(recovery_wait_cause,''), COALESCE(recovery_wait_phase,''), recovery_wait_count,
	    forge_park_count, codex_claim_epoch, health, recovery_retry_not_before IS NOT NULL FROM runs WHERE id=$1`, f.runID).
		Scan(&pStatus, &pCause, &pPhase, &pWait, &pPark, &pEpoch, &pHealth, &pRetry); err != nil {
		t.Fatal(err)
	}
	if pStatus != "recovery_wait" || pCause != "forge_unreachable" || pPhase != "publication" || pWait != int64(runBefore.RecoveryWaitCount)+1 ||
		pPark != int64(runBefore.ForgeParkCount) || pEpoch != claimEpoch+1 || pHealth != "ok" || !pRetry {
		t.Fatalf("post-commit run = %s/%s/%s wait_count=%d forge_park_count=%d epoch=%d health=%s retry=%v; want recovery_wait/forge_unreachable/publication %d/%d/%d/ok/true",
			pStatus, pCause, pPhase, pWait, pPark, pEpoch, pHealth, pRetry, runBefore.RecoveryWaitCount+1, runBefore.ForgeParkCount, claimEpoch+1)
	}
	pHolds := map[int64]string{}
	rows, err := post.Query(f.env.ctx, `SELECT generation, state FROM recovery_custody_holds WHERE run_id=$1`, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var g int64
		var st string
		if err := rows.Scan(&g, &st); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		pHolds[g] = st
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(pHolds) != 2 || pHolds[1] != "open" || pHolds[2] != "released" {
		t.Fatalf("post-commit custody pHolds = %v, want generation 1 open and generation 2 released", pHolds)
	}
	ps, err := f.env.q.WithTx(post).GetActivePublicationResume(f.env.ctx, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	if ps.State != "active" || ps.WorkerID != sideBefore.WorkerID || string(ps.Handoff) != string(sideBefore.Handoff) || ps.Episode != sideBefore.Episode ||
		string(ps.Intent) != string(sideBefore.Intent) || ps.CandidateSeq != sideBefore.CandidateSeq || ps.EnrollmentSeq != sideBefore.EnrollmentSeq ||
		!ps.WaitStartedAt.Time.Equal(sideBefore.WaitStartedAt.Time) || ps.WaitStartedAt.Valid != sideBefore.WaitStartedAt.Valid ||
		!ps.FirstWaitAt.Time.Equal(sideBefore.FirstWaitAt.Time) || ps.FirstWaitAt.Valid != sideBefore.FirstWaitAt.Valid ||
		ps.ParkTotal != sideBefore.ParkTotal+1 || ps.HoldReason.Valid {
		t.Fatalf("post-commit side row:\nbefore %+v\nafter  %+v\nwant only park_total+1 and no hold", sideBefore, ps)
	}
}

// ---- lost acknowledgement and unapplied rediscovery ----

func TestClaimForgeTransientLostAckLiveDB(t *testing.T) {
	t.Run("ordinary park returns to queued", func(t *testing.T) {
		x := newCFPFix(t)
		before := mustRun(t, x.env, x.run)
		x.hooks(nil, &claimTestHooks{commitAckLost: true})
		payload, err := x.claim()
		if err == nil || payload != nil {
			t.Fatalf("Claim = (%v, %v), want the lost-ack error", payload != nil, err)
		}
		x.wantNoPublish()
		r := mustRun(t, x.env, x.run)
		if r.Status != "recovery_wait" || r.ForgeParkCount != before.ForgeParkCount+1 || r.RecoveryWaitCount != before.RecoveryWaitCount+1 {
			t.Fatalf("status=%s forge_park_count=%d recovery_wait_count=%d, want the park applied exactly once", r.Status, r.ForgeParkCount, r.RecoveryWaitCount)
		}
		x.env.exec(`UPDATE runs SET recovery_retry_not_before=now()-interval '1 hour' WHERE id=$1`, x.run)
		if _, err := x.env.q.PromoteRecoveryWaitRuns(x.env.ctx, pgconv.Time(time.Now())); err != nil {
			t.Fatal(err)
		}
		if got := mustRun(t, x.env, x.run).Status; got != "queued" {
			t.Fatalf("promoted status = %s, want queued", got)
		}
	})
	t.Run("cap hold resumes through the owner Resume", func(t *testing.T) {
		x := newCFPFix(t)
		x.f.svc.p.RunForgeUnreachableMaxParks = 1
		before := mustRun(t, x.env, x.run)
		x.hooks(nil, &claimTestHooks{commitAckLost: true})
		if _, err := x.claim(); err == nil {
			t.Fatal("want the lost-ack error")
		}
		r := mustRun(t, x.env, x.run)
		if r.Status != "recovery_wait" || r.RecoveryRetryNotBefore.Valid || r.ForgeParkCount != before.ForgeParkCount {
			t.Fatalf("cap hold: status=%s retry=%v forge_park_count=%d", r.Status, r.RecoveryRetryNotBefore, r.ForgeParkCount)
		}
		resumed, err := x.env.q.ResumeForgeUnreachableHold(x.env.ctx, store.ResumeForgeUnreachableHoldParams{ID: x.run, UserID: x.f.userID, GlobalTimeoutSeconds: 86400})
		if err != nil || resumed.Status != "queued" || resumed.ForgeParkCount != before.ForgeParkCount {
			t.Fatalf("Resume = %s/%v", resumed.Status, err)
		}
	})
	t.Run("publication hold resumes through ResumePublicationForgeHold", func(t *testing.T) {
		f, t0 := cfpEnrolledQueued(t, time.Hour)
		f.clock = t0.Add(time.Hour)
		x := &cfpFix{t: t, env: f.env, f: f.custodyContinuationFixture, run: f.runID, rec: newReadmitRecorder(), life: &cfprLifecycle{}}
		f.svc.SetBroadcaster(x.rec)
		f.svc.SetLifecycle(x.life)
		x.hooks(nil, &claimTestHooks{commitAckLost: true})
		if _, err := f.svc.Claim(f.env.ctx, f.wkr, nil); err == nil {
			t.Fatal("want the lost-ack error")
		}
		x.wantNoPublish()
		r, s := f.run(t), f.side(t)
		if r.Status != "recovery_wait" || r.RecoveryRetryNotBefore.Valid || s.HoldReason.String != "publication_retry_exhausted" || s.ParkTotal != 2 {
			t.Fatalf("publication hold: status=%s retry=%v side=%+v", r.Status, r.RecoveryRetryNotBefore, s)
		}
		if ok, err := f.svc.ResumePublicationForgeHold(f.env.ctx, f.userID, r); err != nil || !ok {
			t.Fatalf("Resume = %v, %v", ok, err)
		}
		if f.run(t).Status != "queued" || f.side(t).Episode != 1 || f.side(t).HoldReason.Valid {
			t.Fatalf("after Resume: status=%s side=%+v, want queued and a new episode with no hold", f.run(t).Status, f.side(t))
		}
	})
}

// A park that did not commit leaves the run claimed. The existing sweeper picks it up and moves it
// out of claimed without failing it.
func TestClaimForgeTransientUnappliedParkIsRediscoveredBySweepLiveDB(t *testing.T) {
	x := newCFPFix(t)
	x.hooks(nil, &claimTestHooks{beforeClaimParkCommit: func(context.Context) error { return errors.New("injected before commit") }})
	payload, err := x.claim()
	if err == nil || payload != nil {
		t.Fatalf("Claim = (%v, %v), want the injected error", payload != nil, err)
	}
	x.wantUnwritten("a park that failed before its commit")
	if mustRun(t, x.env, x.run).Status != "claimed" {
		t.Fatal("the run is not left claimed")
	}
	x.env.exec(`UPDATE runs SET claimed_at = now() - interval '1 hour' WHERE id=$1`, x.run)
	res, err := x.env.q.SweepClaimedNeverStartedCohort(x.env.ctx, store.SweepClaimedNeverStartedCohortParams{
		Cutoff: pgconv.Time(time.Now()), WitnessCutoff: pgconv.Time(time.Now().Add(-10 * time.Minute)), WitnessWindowSeconds: 600, MaxRequeues: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	var disposition string
	for _, r := range res.Charged {
		if r.ID == x.run {
			disposition = "charged:" + r.Status
		}
	}
	for _, r := range res.Unwitnessed {
		if r.ID == x.run {
			disposition = "unwitnessed:" + r.Status
		}
	}
	for _, r := range res.Reset {
		if r.ID == x.run {
			disposition = "reset:" + r.Status
		}
	}
	got := mustRun(t, x.env, x.run)
	if disposition != "reset:queued" || got.Status != "queued" || got.FailOrigin.Valid {
		t.Fatalf("sweep disposition=%q status=%s fail_origin=%v, want the uncharged reset to queued with no failure", disposition, got.Status, got.FailOrigin)
	}
}

// ---- late reports ----

func TestClaimForgeTransientLateReportsAreRefusedLiveDB(t *testing.T) {
	x := newCFPFix(t)
	x.mustPark()
	check := func(stage string) {
		t.Helper()
		before := cfpSnap(t, x.env, x.run)
		for _, g := range []int64{1, 2} {
			for _, state := range []string{"failed", "running"} {
				gen := g
				_, applied, err := x.f.svc.SetState(x.env.ctx, x.f.wkr, x.run, StateRequest{State: state, ClaimGeneration: &gen})
				if applied {
					t.Errorf("%s: a %s report at generation %d was applied (err %v)", stage, state, gen, err)
				}
			}
		}
		if after := cfpSnap(t, x.env, x.run); after != before {
			t.Fatalf("%s: late reports moved the run:\n%s\n%s", stage, before, after)
		}
	}
	check("parked")
	x.env.exec(`UPDATE runs SET recovery_retry_not_before=now()-interval '1 hour' WHERE id=$1`, x.run)
	if _, err := x.env.q.PromoteRecoveryWaitRuns(x.env.ctx, pgconv.Time(time.Now())); err != nil {
		t.Fatal(err)
	}
	check("promoted")
}

// The test above runs on a run holding a pending Fresh policy episode, which SetState refuses before it
// reaches the claim-generation fence. This one parks an ordinary started run (no policy episode), so a
// stale-generation report can only be stopped by the fence itself.
func TestClaimForgeTransientLateReportsHitGenerationFenceLiveDB(t *testing.T) {
	x := newCFPFix(t)
	x.env.exec(`UPDATE runs SET policy_preserve_required=false, policy_pending_fresh=false, policy_outcome_id=NULL,
		policy_continuation_mode=NULL, policy_applying_worker_id=NULL, policy_applying_generation=NULL,
		policy_source_generation=NULL, policy_resolved_at=NULL WHERE id=$1`, x.run)
	r := x.mustPark()
	if r.ClaimGeneration != 2 || r.PolicyPendingFresh || unresolvedPolicyEpisode(r) {
		t.Fatalf("parked run generation=%d pending_fresh=%v unresolved=%v, want generation 2 and no policy episode", r.ClaimGeneration, r.PolicyPendingFresh, unresolvedPolicyEpisode(r))
	}
	check := func(stage string) {
		t.Helper()
		before := cfpSnap(t, x.env, x.run)
		gen1 := int64(1)
		for _, state := range []string{"failed", "running"} {
			_, applied, err := x.f.svc.SetState(x.env.ctx, x.f.wkr, x.run, StateRequest{State: state, ClaimGeneration: &gen1})
			if applied || !errors.Is(err, ErrStaleClaim) {
				t.Errorf("%s: a %s report at generation 1 = applied %v, err %v, want ErrStaleClaim and not applied", stage, state, applied, err)
			}
		}
		if after := cfpSnap(t, x.env, x.run); after != before {
			t.Fatalf("%s: stale reports moved the run:\n%s\n%s", stage, before, after)
		}
	}
	check("parked")
	gen2 := int64(2)
	before := cfpSnap(t, x.env, x.run)
	if _, applied, _ := x.f.svc.SetState(x.env.ctx, x.f.wkr, x.run, StateRequest{State: "running", ClaimGeneration: &gen2}); applied {
		t.Error("parked: a running report at generation 2 was applied")
	}
	if after := cfpSnap(t, x.env, x.run); after != before {
		t.Fatalf("parked: the generation 2 running report moved the run:\n%s\n%s", before, after)
	}
	x.env.exec(`UPDATE runs SET recovery_retry_not_before=now()-interval '1 hour' WHERE id=$1`, x.run)
	if _, err := x.env.q.PromoteRecoveryWaitRuns(x.env.ctx, pgconv.Time(time.Now())); err != nil {
		t.Fatal(err)
	}
	check("promoted")
}
