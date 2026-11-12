package workersvc

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type sourceGapHealthStore struct {
	*repoAgentSourceHealthStore
	worker      store.Worker
	workerErr   error
	workerCalls []store.GetWorkerByIDForUserParams
}

func (f *sourceGapHealthStore) GetWorkerByIDForUser(_ context.Context, arg store.GetWorkerByIDForUserParams) (store.Worker, error) {
	f.workerCalls = append(f.workerCalls, arg)
	return f.worker, f.workerErr
}
func sourceGapFixture() (*sourceGapHealthStore, *Service, store.ListActiveRunsForHealthRow) {
	base, _, r := repoAgentSourceHealth(false, 1)
	r.RepoAgentSourceRequired = true
	r.ClaimGeneration = 1

	r.WorkerID = pgtype.UUID{Bytes: uuid.New(), Valid: true}
	r.UpdatedAt = ago(3 * time.Hour)
	base.eligibility.Claimable = 1
	f := &sourceGapHealthStore{repoAgentSourceHealthStore: base, worker: store.Worker{ID: uuid.UUID(r.WorkerID.Bytes), UserID: r.UserID, DrainingSince: ago(time.Hour)}}
	return f, healthSvc(f, defaultHealthSettings()), r
}

func TestHealthSourceGapAssignedContinuation(t *testing.T) {
	for _, commit := range []bool{false, true} {
		for _, free := range []int64{0, 1, 2} {
			for _, peers := range []int64{0, 1, 2} {
				f, s, r := sourceGapFixture()
				r.RepoAgentSourceCommit = pgtype.Text{String: "retained", Valid: commit}
				f.capable = peers
				f.freeSlotWorkers = free
				f.active = []store.ListActiveRunsForHealthRow{r}
				s.detectRunHealth(context.Background(), t0)
				got := lastWrite(t, f.healthFakeStore, r.ID).HealthReason.String
				if !strings.Contains(got, reasonAssignedRepoAgentSourceGap) {
					t.Fatalf("commit=%v free=%d peers=%d reason=%q", commit, free, peers, got)
				}
				if got == reasonAllWorkersBusy || got == reasonWaitingWorker {
					t.Fatal(got)
				}
				if len(f.workerCalls) != 1 || f.workerCalls[0].ID != f.worker.ID || f.workerCalls[0].UserID != r.UserID {
					t.Fatalf("lookup=%v", f.workerCalls)
				}
			}
		}
	}
}

func TestHealthSourceGapControls(t *testing.T) {
	for _, name := range []string{"commit only", "no requirement", "foreign", "missing", "error", "capable", "unassigned", "crosscheck", "noncontinuation", "no pin within ceiling", "eligibility error", "claimable peer", "unavailable peer", "manual preserved", "forge", "roll", "account"} {
		t.Run(name, func(t *testing.T) {
			f, s, r := sourceGapFixture()
			want := reasonWaitingWorker
			switch name {
			case "commit only":
				r.RepoAgentSourceRequired = false
				r.RepoAgentSourceCommit = pgtype.Text{String: "retained", Valid: true}
				want = reasonAssignedRepoAgentSourceGap
			case "no requirement":
				r.RepoAgentSourceRequired = false
			case "foreign":
				f.worker.UserID = uuid.New()
			case "missing":
				f.worker = store.Worker{}
			case "error":
				f.workerErr = errors.New("read failed")
			case "capable":
				f.worker.ProtocolCapabilities = []string{capability.RepoAgentSourceV1}
			case "unassigned":
				r.WorkerID = pgtype.UUID{}
			case "crosscheck":
				r.Kind = "cross_check"
				want = reasonNoCrossCheckWorker
			case "noncontinuation":
				r.ClaimGeneration = 0
			case "no pin within ceiling":
				r.UpdatedAt = ago(time.Minute)
				want = reasonAssignedRepoAgentSourceGap
			case "claimable peer":
				f.eligibility.Claimable = 1
				want = reasonAssignedRepoAgentSourceGap
			case "unavailable peer":
				f.eligibility.Claimable = 0
				f.eligibility.StrictEligible = 1
				want = reasonAssignedRepoAgentSourceGap + sourcePeerUnavailableSuffix
			case "eligibility error":
				f.eligibilityErr = errors.New("projection failed")
				want = reasonAssignedRepoAgentSourceGap
			case "manual preserved":
				f.eligibilityErr = errors.New("projection failed")
				r.HealthReason = pgtype.Text{String: manualCordonWaitPrefix + " owner", Valid: true}
				want = r.HealthReason.String
			case "forge":
				f.eligibility.Claimable = 0
				f.eligibility.ForgeBlocked = 1
				want = reasonForgeOriginWait
			case "roll":
				f.eligibility.DrainingEligible = 1
				want = ReasonWorkersUpgrading
			case "account":
				r.CodexAccountGated = true
				want = reasonCodexAccountUnavailable
			}
			reason := s.queuedReasonWithEligibility(context.Background(), t0, r, healthThresholds{custodyLimit: testCustodyLimit, custodyLimitOK: true}, nil)
			if reason != want {
				t.Fatalf("reason=%q want %q", reason, want)
			}
		})
	}
}

func TestHealthSourceGapHeldLifecycle(t *testing.T) {
	for _, owner := range []bool{false, true} {
		f, s, r := sourceGapFixture()
		r.StatusSince = ago(time.Minute)
		r.UpdatedAt = ago(time.Minute)
		r.StaleRequeuePinnable = !owner
		r.OwnerHoldPinnable = owner
		s.p.WorkerStaleRequeueGrace = 20 * time.Minute
		s.p.WorkerAffinityCeiling = 5 * time.Minute
		b := &fakeBroadcaster{}
		s.SetBroadcaster(b)
		f.active = []store.ListActiveRunsForHealthRow{r}
		s.detectRunHealth(context.Background(), t0)
		w := lastWrite(t, f.healthFakeStore, r.ID)
		if !strings.HasPrefix(w.HealthReason.String, "waiting until 2026-07-12T12:04:00Z for its previous worker") || !strings.HasSuffix(w.HealthReason.String, heldRepoAgentSourceGapSuffix) || !isStaleRequeuePinReason(w.HealthReason.String) {
			t.Fatal(w.HealthReason.String)
		}
		if w.HealthNotifiedAt.Valid || len(b.healthNudges) != 1 || b.healthNudges[0] {
			t.Fatal("held nudge")
		}
		r.Health, r.HealthReason, r.HealthSince = w.Health, w.HealthReason, w.HealthSince
		f.active = []store.ListActiveRunsForHealthRow{r}
		if n := s.detectRunHealth(context.Background(), t0.Add(time.Minute)); n != 0 {
			t.Fatal("unstable hold")
		}
		// Ceiling is inclusive. After it ends an aged continuation starts a new episode.
		r.StatusSince = ago(15 * time.Minute)
		f.active = []store.ListActiveRunsForHealthRow{r}
		s.detectRunHealth(context.Background(), t0.Add(4*time.Minute))
		if !isStaleRequeuePinReason(lastWrite(t, f.healthFakeStore, r.ID).HealthReason.String) {
			t.Fatal("lost boundary hold")
		}
		s.detectRunHealth(context.Background(), t0.Add(4*time.Minute+time.Nanosecond))
		exit := lastWrite(t, f.healthFakeStore, r.ID)
		if exit.HealthReason.String != reasonAssignedRepoAgentSourceGap || !exit.HealthSince.Time.Equal(t0.Add(4*time.Minute+time.Nanosecond)) {
			t.Fatalf("exit=%+v", exit)
		}
	}
}

func TestHealthSourceGapNoComposedPeerOmitsUnavailable(t *testing.T) {
	f, s, r := sourceGapFixture()
	f.capable = 1
	f.eligibility.Claimable = 0
	f.eligibility.StrictEligible = 0
	got := s.queuedReasonWithEligibility(context.Background(), t0, r, healthThresholds{custodyLimit: testCustodyLimit, custodyLimitOK: true}, nil)
	if got != reasonAssignedRepoAgentSourceGap {
		t.Fatalf("reason=%q", got)
	}
}

// A live, non-draining assigned worker without the capability still pins peers until the
// affinity ceiling (ClaimRun's affinity arm pins on a fresh heartbeat), so health must show
// the held form then, and the post-hold form only after the pin ended.
func TestHealthSourceGapLiveAssignedPin(t *testing.T) {
	for _, name := range []string{"held", "after ceiling", "ceiling inclusive", "forge released", "stale heartbeat"} {
		t.Run(name, func(t *testing.T) {
			f, s, r := sourceGapFixture()
			s.p.WorkerAffinityCeiling = 5 * time.Minute
			s.p.WorkerHeartbeatStale = time.Minute
			f.worker.DrainingSince = pgtype.Timestamptz{}
			f.worker.LastHeartbeatAt = ago(10 * time.Second)
			r.StaleRequeueWorkerName = "live-w"
			r.UpdatedAt = ago(time.Minute)
			held := true
			switch name {
			case "after ceiling":
				r.UpdatedAt = ago(6 * time.Minute)
				held = false
			case "ceiling inclusive":
				r.UpdatedAt = ago(5 * time.Minute)
			case "forge released":
				r.StaleRequeueWorkerName = ""
				held = false
			case "stale heartbeat":
				f.worker.LastHeartbeatAt = ago(10 * time.Minute)
				held = false
			}
			b := &fakeBroadcaster{}
			s.SetBroadcaster(b)
			f.active = []store.ListActiveRunsForHealthRow{r}
			s.detectRunHealth(context.Background(), t0)
			w := lastWrite(t, f.healthFakeStore, r.ID)
			got := w.HealthReason.String
			if held {
				want := "waiting until " + r.UpdatedAt.Time.Add(5*time.Minute).UTC().Format(time.RFC3339) + " for its previous worker live-w to return" + heldRepoAgentSourceGapSuffix
				if got != want || !isStaleRequeuePinReason(got) {
					t.Fatalf("reason=%q want %q", got, want)
				}
				if w.HealthNotifiedAt.Valid || len(b.healthNudges) != 1 || b.healthNudges[0] {
					t.Fatal("held must not nudge")
				}
				return
			}
			if got != reasonAssignedRepoAgentSourceGap || isStaleRequeuePinReason(got) {
				t.Fatalf("reason=%q", got)
			}
		})
	}
}
