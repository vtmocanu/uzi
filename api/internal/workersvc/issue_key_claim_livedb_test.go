package workersvc

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// issue_key_claim_livedb_test.go pins the issue_key_v1 claim gate (PRD #4173) on the real claim
// path: a run on a repo linked to an external issue tracker is claimable only by a worker
// self-reporting the protocol capability, so a worker image that ignores the claim's issue_key
// (and would write Closes #<iid>) never receives it. Skipped unless UZI_TEST_DATABASE_URL points
// at a throwaway Postgres.

// linkFixtureRepo links the fixture repo to an issue tracker connection.
func (f *crFix) linkFixtureRepo(t *testing.T) {
	t.Helper()
	conn := uuid.New()
	f.env.exec(`INSERT INTO issue_tracker_connections (id, user_id, kind, base_url, account_id, account_login, token_ciphertext)
	            VALUES ($1, $2, 'youtrack', 'https://issues.example.com', 'a', 'b', 'c')`, conn, f.user)
	f.env.exec(`UPDATE repos SET issue_tracker_connection_id=$2, issue_tracker_project_id='0-1', issue_tracker_project_short='ABC',
	            eligibility_tag_id='6-1', eligibility_tag_name='uzi', issue_tracker_linked_at=now() WHERE id=$1`, f.repo, conn)
}

// startQueuedIssueRun inserts a queued issue run on the fixture repo and returns its id.
func (f *crFix) startQueuedIssueRun(t *testing.T, iid int) uuid.UUID {
	t.Helper()
	id := uuid.New()
	f.env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status)
	            VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'queued')`, id, f.user, f.repo, iid)
	return id
}

func TestIssueKeyClaimGateLiveDB(t *testing.T) {
	t.Run("linked repo", func(t *testing.T) {
		f := newCRFix(t)
		f.linkFixtureRepo(t)
		run := f.startQueuedIssueRun(t, 12)
		old := f.protocolWorker(t, 0)
		f.refuseClaim(t, old, run, "worker without issue_key_v1")

		// The queued-reason resolver names the missing capability.
		rows, err := f.env.q.ListActiveRunsForHealth(f.env.ctx, codexCuratedModelsSlice())
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, r := range rows {
			if r.ID != run {
				continue
			}
			found = true
			f.env.exec(`UPDATE runs SET status_since = now() - interval '1 hour' WHERE id = $1`, run)
			got := f.svc.queuedReason(f.env.ctx, time.Now(), r, healthThresholds{custodyLimit: 100, custodyLimitOK: true})
			if !strings.Contains(got, "issue_key_v1") {
				t.Fatalf("queued reason = %q, want it to name issue_key_v1", got)
			}
		}
		if !found {
			t.Fatal("run missing from the health listing")
		}

		capable := f.protocolWorker(t, 0, capability.IssueKeyV1)
		p := f.acceptClaim(t, capable, run, "worker with issue_key_v1")
		if p.IssueKey != "ABC-12" {
			t.Fatalf("issue_key = %q, want ABC-12", p.IssueKey)
		}
	})
	t.Run("unlinked repo is claimable without the capability", func(t *testing.T) {
		f := newCRFix(t)
		run := f.startQueuedIssueRun(t, 12)
		old := f.protocolWorker(t, 0)
		p := f.acceptClaim(t, old, run, "old worker, unlinked repo")
		if p.IssueKey != "" {
			t.Fatalf("issue_key = %q on an unlinked repo", p.IssueKey)
		}
	})
}

// TestIssueKeyPeerDeferralLiveDB: the fleet-spread peer arm mirrors the gate. A capable but busy
// claimant must not defer a linked run to an idle peer that can never claim it.
func TestIssueKeyPeerDeferralLiveDB(t *testing.T) {
	f := newCRFix(t)
	f.linkFixtureRepo(t)
	capable := f.protocolWorker(t, 4, capability.IssueKeyV1)
	f.protocolWorker(t, 4) // an idle old peer: it could never claim the linked run
	f.env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id)
	            VALUES ($1, $2, $3, 'issue', 771, 't', 'd', 'running', $4)`, uuid.New(), f.user, f.repo, capable.ID)
	run := f.startQueuedIssueRun(t, 12)
	f.acceptClaim(t, capable, run, "busy capable worker with an idle incapable peer")
}

// TestIssueKeyPlacementMirrorsLiveDB: the claimable count and the capability-gap list treat a
// worker without the capability as unable to serve a linked run.
func TestIssueKeyPlacementMirrorsLiveDB(t *testing.T) {
	f := newCRFix(t)
	ctx := f.env.ctx
	f.linkFixtureRepo(t)
	f.env.exec(`UPDATE users SET ephemeral_workers_enabled = true WHERE id = $1`, f.user)
	run := f.startQueuedIssueRun(t, 12)
	f.env.exec(`UPDATE runs SET status_since = now() - interval '1 hour' WHERE id = $1`, run)

	now := time.Now()
	hb := pgtype.Timestamptz{Time: now.Add(-45 * time.Second), Valid: true}
	cutoff := pgtype.Timestamptz{Time: now.Add(time.Hour), Valid: true}
	claimable := func() int64 {
		t.Helper()
		n, err := f.env.q.CountOnlineWorkersClaimableForRun(ctx, store.CountOnlineWorkersClaimableForRunParams{
			RunID: run, HeartbeatCutoff: hb, AffinityCutoff: cutoff, CrossCheckEvaluatedAt: cutoff, CrossCheckAffinityCutoff: cutoff,
			CapabilityAware: true, CodexCuratedModels: codexCuratedModelsSlice(), DockerRepoAllowlist: []uuid.UUID{},
			EphemeralLease: pgtype.Interval{Valid: true},
		})
		if err != nil {
			t.Fatal(err)
		}
		return n.Claimable
	}
	gap := func() bool {
		t.Helper()
		rows, err := f.env.q.ListUnplaceableQueuedRunsForEphemeral(ctx, store.ListUnplaceableQueuedRunsForEphemeralParams{
			CrossCheckEvaluatedAt: cutoff, CrossCheckAffinityCutoff: cutoff, BackgroundGraceCutoff: cutoff,
			CodexCuratedModels: codexCuratedModelsSlice(), DockerRepoAllowlist: []uuid.UUID{}, EphemeralLease: pgtype.Interval{Valid: true},
			MaxPerUser: 1000, MaxRows: 10000,
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			if r.ID == run {
				return true
			}
		}
		return false
	}

	f.protocolWorker(t, 0)
	if n := claimable(); n != 0 {
		t.Fatalf("claimable with only an old worker = %d, want 0", n)
	}
	if !gap() {
		t.Fatal("a linked run with only old workers online must be a capability-gap candidate")
	}
	f.protocolWorker(t, 1, capability.IssueKeyV1)
	if n := claimable(); n != 1 {
		t.Fatalf("claimable with one capable worker = %d, want 1", n)
	}
	if gap() {
		t.Fatal("a capable worker is online: not a capability gap")
	}
}

// TestIssueKeySaturationMirrorsLiveDB: ListSaturationQueuedRunsForEphemeral carries the issue_key
// clause in both of its worker sets. A linked run is saturation demand only when a capable worker
// exists (first set) and none of the capable workers has a free slot (second set), so a worker
// without the capability counts as neither capable nor free.
func TestIssueKeySaturationMirrorsLiveDB(t *testing.T) {
	f := newCRFix(t)
	ctx := f.env.ctx
	f.linkFixtureRepo(t)
	f.env.exec(`UPDATE users SET ephemeral_workers_enabled = true WHERE id = $1`, f.user)
	run := f.startQueuedIssueRun(t, 12)
	f.env.exec(`UPDATE runs SET status_since = now() - interval '1 hour' WHERE id = $1`, run)
	cutoff := pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true}
	saturated := func() bool {
		t.Helper()
		rows, err := f.env.q.ListSaturationQueuedRunsForEphemeral(ctx, store.ListSaturationQueuedRunsForEphemeralParams{
			CrossCheckEvaluatedAt: cutoff, CrossCheckAffinityCutoff: cutoff, BackgroundGraceCutoff: cutoff,
			CodexCuratedModels: codexCuratedModelsSlice(), DockerRepoAllowlist: []uuid.UUID{}, EphemeralLease: pgtype.Interval{Valid: true},
			SaturationDelay: pgtype.Interval{Valid: true}, MaxPerUser: 1000, MaxRows: 10000,
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			if r.ID == run {
				return true
			}
		}
		return false
	}
	fill := func(w store.Worker) {
		f.env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id)
		            VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'running', $5)`, uuid.New(), f.user, f.repo, 900+int(w.ID[0]), w.ID)
	}

	// Only workers without the capability, one free and one full: nobody could ever claim the
	// run, so it is a capability gap and not saturation demand (first worker set).
	oldFree := f.protocolWorker(t, 2)
	oldFull := f.protocolWorker(t, 1)
	fill(oldFull)
	if saturated() {
		t.Fatal("a linked run with only incapable workers online is a gap, not saturation")
	}

	// A capable worker with a free slot will claim it: not saturated.
	capable := f.protocolWorker(t, 1, capability.IssueKeyV1)
	if saturated() {
		t.Fatal("a capable worker has a free slot: not saturated")
	}

	// The capable worker is full: saturated, although an incapable worker still has a free slot
	// (second worker set: that worker cannot claim the run, so it does not count as free room).
	fill(capable)
	if !saturated() {
		t.Fatalf("every capable worker is full while incapable worker %s has room: want saturated", oldFree.ID)
	}
}
