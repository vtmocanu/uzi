package workersvc

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

func TestManualCordonHealthEntryMetadataAndPrerequisites(t *testing.T) {
	actor, worker := uuid.New(), uuid.New()
	label, reason := strings.Repeat("😀", 200), strings.Repeat("🚀", 500)
	evidence, err := json.Marshal(map[string]any{
		"worker_id": worker, "actor_id": actor, "at": t0,
		"actor_label": "\x1b]0;hidden\a" + label + "overflow",
		"reason":      "\x1b[31m" + reason + "overflow\x1b[0m\x1b]8;;https://example.com\x1b\\\x1b]8;;\x1b\\",
	})
	if err != nil {
		t.Fatal(err)
	}
	manual := fmt.Sprintf("waiting for workers cordoned (manual): %s (%s) at %s: %s", label, actor, t0.Format(time.RFC3339), reason)
	for _, age := range []time.Duration{time.Minute, time.Hour} {
		for _, mode := range []string{"manual", "hard fence", "claimable continuation", "disabled", "custody", "vault", "account", "generic"} {
			t.Run(age.String()+"/"+mode, func(t *testing.T) {
				r := ownerHeldRun()
				r.StatusSince = ago(age)
				counts := store.CountOnlineWorkersClaimableForRunRow{ManualSuitable: 1, ManualCordoned: 1, ManualContext: evidence}
				fs := &healthFakeStore{eligibility: counts}
				svc, _ := pinSvc(fs, defaultHealthSettings())
				svc.p.WorkerAffinityCeiling = 2 * time.Hour
				th := healthThresholds{queued: 10 * time.Minute, custodyLimit: testCustodyLimit, custodyLimitOK: true}
				want := manual
				switch mode {
				case "hard fence":
					fs.eligibility.AutomaticOwnerHold = true
					want += "; " + ownerHoldReason(r.UpdatedAt.Time.Add(svc.p.WorkerAffinityCeiling), r.StaleRequeueWorkerName)
				case "claimable continuation":
					fs.eligibility.Claimable = 1
					// Generic lack-of-capacity text is outside this assertion; it must
					// never become an owner hold or manual block.
					want = ""
				case "disabled":
					th.queued = 0
					want = ""
				case "vault":
					svc.SetVault(vault.New(newBox(t), newMemVaultStore()))
					want = reasonVaultLocked
				case "custody":
					fs.custodyHolds = int64(testCustodyLimit)
					want = reasonCustodyLimit
				case "account":
					r.CodexAccountGated = true
					want = reasonCodexAccountUnavailable
				case "generic":
					r.OwnerHoldPinnable = false
					fs.eligibility = store.CountOnlineWorkersClaimableForRunRow{}
					want = reasonNoWorker
				}
				if age < th.queued && (mode == "vault" || mode == "custody" || mode == "account" || mode == "generic" || mode == "claimable continuation") {
					want = ""
				}
				flag, got := svc.healthTargetFor(context.Background(), t0, r, th)
				if mode == "claimable continuation" && age >= th.queued {
					if strings.Contains(got, "cordoned (manual)") || strings.Contains(got, "draining or maintenance") {
						t.Fatalf("claimable continuation: %q", got)
					}
					return
				}
				wantFlag := healthWaitingWorker
				if want == "" {
					wantFlag = healthOK
				}
				if got != want || flag != wantFlag {
					t.Fatalf("health=(%q,%q), want=(%q,%q)", flag, got, wantFlag, want)
				}
			})
		}
	}
}

func TestManualCordonHealthEntryAssignedFenceLiveDB(t *testing.T) {
	for _, fenced := range []bool{false, true} {
		t.Run(fmt.Sprint(fenced), func(t *testing.T) {
			e := setupInterlockLiveDB(t)
			svc := e.permitService(t)
			svc.p.WorkerAffinityCeiling = time.Hour
			worker := e.seedHeartbeatWorker(t, nil)
			run := e.seedQueuedRun(t, nil, nil)
			e.exec(t, "UPDATE runs SET worker_id=$2,status_since=now(),updated_at=now() WHERE id=$1", run, worker)
			e.exec(t, "UPDATE workers SET maintenance_fenced=$2 WHERE id=$1", worker, fenced)
			cordoned, err := store.ChangeWorkerManualCordon(e.ctx, e.pool, e.userID, worker, "😀 operator", pgtype.Text{String: "🚀 investigate", Valid: true}, true)
			if err != nil {
				t.Fatal(err)
			}
			th := healthThresholds{queued: 10 * time.Minute, custodyLimit: testCustodyLimit, custodyLimitOK: true}
			row := e.healthRowFor(t, run)
			flag, reason := svc.healthTargetFor(e.ctx, time.Now(), row, th)
			if !fenced {
				if flag != healthOK || reason != "" {
					t.Fatalf("claimable continuation health=(%q,%q)", flag, reason)
				}
				return
			}
			manual := fmt.Sprintf("waiting for workers cordoned (manual): 😀 operator (%s) at %s: 🚀 investigate", e.userID, cordoned.ManualCordonedAt.Time.UTC().Format(time.RFC3339))
			want := manual + "; " + ownerHoldReason(row.UpdatedAt.Time.Add(svc.p.WorkerAffinityCeiling), row.StaleRequeueWorkerName)
			if flag != healthWaitingWorker || reason != want {
				t.Fatalf("fenced health=(%q,%q), want %q", flag, reason, want)
			}
		})
	}
}

func TestManualCordonHealthLeaseAvailabilityLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	svc.ephemeralLease = 2 * time.Hour
	run := e.seedQueuedRun(t, nil, nil)
	bound := e.seedQueuedRun(t, nil, nil)
	worker := e.seedHeartbeatWorker(t, nil)
	e.exec(t, "UPDATE runs SET worker_id=$2,branch=NULL WHERE id=$1", run, worker)
	e.exec(t, `UPDATE workers SET ephemeral=true,ephemeral_run_id=$2,
        lease_since=now(),lease_repo_id=(SELECT repo_id FROM runs WHERE id=$3),
        lease_branch=(SELECT 'agent/issue-' || issue_iid::text FROM runs WHERE id=$3),draining_since=now() WHERE id=$1`, worker, bound, run)
	for _, draining := range []bool{true, false} {
		e.exec(t, "UPDATE workers SET draining_since=CASE WHEN $2 THEN now() ELSE NULL END WHERE id=$1", worker, draining)
		counts, err := svc.workerEligibilityForHealth(e.ctx, time.Now(), run, true)
		if err != nil {
			t.Fatal(err)
		}
		want := int64(1)
		if draining {
			want = 0
		}
		if counts.Claimable != want || counts.ManualSuitable != 1 {
			t.Fatalf("draining=%v counts=%+v want claimable=%d diagnostic suitable=1", draining, counts, want)
		}
	}
}

func TestManualCordonHealthYoungUnicodeLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	run := e.seedQueuedRun(t, nil, nil)
	worker := e.seedHeartbeatWorker(t, nil)
	label, reason := strings.Repeat("😀", 200), strings.Repeat("🚀", 500)
	cordoned, err := store.ChangeWorkerManualCordon(e.ctx, e.pool, e.userID, worker, label, pgtype.Text{String: reason, Valid: true}, true)
	if err != nil {
		t.Fatal(err)
	}
	row := e.healthRowFor(t, run)
	flag, got := svc.healthTargetFor(e.ctx, time.Now(), row, healthThresholds{queued: time.Hour, custodyLimit: testCustodyLimit, custodyLimitOK: true})
	want := fmt.Sprintf("waiting for workers cordoned (manual): %s (%s) at %s: %s", label, e.userID, cordoned.ManualCordonedAt.Time.UTC().Format(time.RFC3339), reason)
	if flag != healthWaitingWorker || got != want {
		t.Fatalf("young health=(%q,%q), want %q", flag, got, want)
	}
}
