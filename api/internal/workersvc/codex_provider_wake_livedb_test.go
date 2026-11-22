package workersvc

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Bounded M3 proves wake admission only. The actual claim remains held by
// TestCodexProviderActualClaimHeldBeforeReads. Positive claim proof remains
// required at final activation; neither this fixture nor readiness removes that hold.
func codexProviderWakeFixture(t *testing.T) (*provFix, *hibFx) {
	t.Helper()
	p := newProvFix(t)
	seedCodexSource(t, p)
	// The provider fixture's ordinary online worker must not suppress demand.
	p.env.exec(`UPDATE workers SET status='offline' WHERE id=$1`, p.workerID)
	f := &hibFx{t: t, env: p.env, svc: p.svc, set: &fakeHibSettings{on: true},
		user: p.userID, repo: p.repoID, actor: HibernationActor{ID: p.userID, Label: "admin"}, deploy: uuid.NewString()}
	f.svc.SetHibernationSettings(f.set)
	f.advertise(1, 5, 0)
	return p, f
}

func frozenCodexWakeRun(t *testing.T, p *provFix) store.Run {
	t.Helper()
	r, _, err := p.svc.createRunAtomic(p.env.ctx, p.userID, nil, p.insertAt(nil))
	if err != nil {
		t.Fatal(err)
	}
	wantCodexBinding(t, p, mustRun(t, p.env, r.ID), 1, p.keyRev())
	p.env.exec(`UPDATE runs SET updated_at=now()-interval '3 hours' WHERE id=$1`, r.ID)
	return mustRun(t, p.env, r.ID)
}

func codexWakeCaps() []string {
	return []string{capability.CodexHarnessV1, capability.CodexRuntimeV2, capability.CodexProviderV1}
}

func setWakeCaps(f *hibFx, id uuid.UUID, caps []string) {
	f.t.Helper()
	if caps == nil {
		caps = []string{}
	}
	f.env.exec(`UPDATE workers SET protocol_capabilities=$2 WHERE id=$1`, id, caps)
}

// Use the real engine-aware placement consumer, including its online suppression
// arm, rather than reproducing the capability predicate in the test.
func wakeCandidate(t *testing.T, f *hibFx, run uuid.UUID) uuid.UUID {
	t.Helper()
	now := time.Now()
	stamp := func(at time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: at, Valid: true} }
	rows, err := f.env.q.ListHibernationWakeDemand(f.env.ctx, store.ListHibernationWakeDemandParams{
		CrossCheckEvaluatedAt: stamp(now), CrossCheckAffinityCutoff: stamp(now.Add(-time.Hour)),
		EphemeralLease:     pgtype.Interval{Microseconds: int64(time.Hour / time.Microsecond), Valid: true},
		CodexCuratedModels: []string{"gpt-6-astra", "gpt-5.6-sol", "gpt-6-luna", "gpt-6-sol", "gpt-6.1-sol"},
		AffinityCutoff:     stamp(now.Add(-time.Hour)), HeartbeatCutoff: stamp(now.Add(-time.Minute)),
		StaleRequeueCutoff: stamp(now.Add(-time.Hour)), BackgroundGraceCutoff: stamp(now.Add(-time.Hour)),
		DockerRepoAllowlist: []uuid.UUID{}, MaxRows: 10000,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.RunID == run {
			return r.WorkerID
		}
	}
	return uuid.Nil
}

func wantCodexWake(t *testing.T, f *hibFx, run, worker uuid.UUID) {
	t.Helper()
	before := f.op(worker)
	res, err := f.svc.DemandWake(f.env.ctx, run, worker)
	if err != nil || res != DemandWakeWoke {
		t.Fatalf("DemandWake=%v, %v; want Woke", res, err)
	}
	f.want(worker, "waking")
	if f.op(worker) == before || f.wakeRows(run) != 1 || f.wakeWorker(run) != worker {
		t.Fatal("wake did not mint a fresh op and exactly one row for the selected worker")
	}
	f.wantOrigins(worker, "", WakeOriginDemand)
}

func TestCodexProviderWakePlacementLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name string
		caps []string
	}{
		{"legacy", nil},
		{"claude_A1_only", []string{capability.ClaudeProviderV1}},
		{"codex_runtime_only", []string{capability.CodexHarnessV1, capability.CodexRuntimeV2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, f := codexProviderWakeFixture(t)
			bad := f.sleepingWorker()
			setWakeCaps(f, bad, tc.caps)
			r := frozenCodexWakeRun(t, p)
			if got := wakeCandidate(t, f, r.ID); got != uuid.Nil {
				t.Fatalf("ineligible sleeper selected: %s", got)
			}
			// Even an awake incompatible worker must not suppress the compatible sleeper.
			f.env.exec(`UPDATE workers SET hibernation_state='none', status='online' WHERE id=$1`, p.workerID)
			setWakeCaps(f, p.workerID, tc.caps)
			good := f.sleepingWorker()
			setWakeCaps(f, good, codexWakeCaps())
			if got := wakeCandidate(t, f, r.ID); got != good {
				t.Fatalf("compatible wake suppressed: got %s want %s", got, good)
			}
			lease := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Microsecond)
			f.env.exec(`UPDATE workers SET last_manual_wake_settled_at=$2 WHERE id=$1`, good, lease)
			wantCodexWake(t, f, r.ID, good)
			f.want(bad, "hibernated")
			if f.wakeRows(r.ID) != 1 {
				t.Fatal("ineligible worker captured wake row")
			}
			// Exercise actual ready + registration settlement without assembling a claim.
			f.settleOnReplacement(good, false)
			if term := f.row(good).LastManualWakeSettledAt; !term.Valid || !term.Time.Equal(lease) {
				t.Fatalf("demand settlement renewed manual origin lease: %v want %v", term, lease)
			}
			if _, err := f.svc.HibernateWorker(f.env.ctx, f.actor, good, ""); refusalCode(err) != RefusalPendingWakeDemand {
				t.Fatalf("queued wake demand lost its hibernation fence: %v", err)
			}
			if got := mustRun(t, f.env, r.ID); got.Status != "queued" || got.WorkerID.Valid {
				t.Fatal("wake readiness unexpectedly claimed the run")
			}
			wantCodexBinding(t, p, mustRun(t, f.env, r.ID), 1, p.keyRev())
		})
	}
}

// Direct-call admission is a separate contract from the placement listing. A red
// here identifies a missing DemandWake recheck, not permission to activate claims.
func TestCodexProviderDemandWakeRejectsIneligibleLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name string
		caps []string
	}{
		{"legacy", nil},
		{"claude_A1_only", []string{capability.ClaudeProviderV1}},
		{"codex_runtime_only", []string{capability.CodexHarnessV1, capability.CodexRuntimeV2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, f := codexProviderWakeFixture(t)
			bad, good := f.sleepingWorker(), f.sleepingWorker()
			setWakeCaps(f, bad, tc.caps)
			setWakeCaps(f, good, codexWakeCaps())
			r := frozenCodexWakeRun(t, p)
			before := f.wakeColumns(bad)
			res, err := f.svc.DemandWake(f.env.ctx, r.ID, bad)
			// Keep probing the compatible worker even if the first assertion fails: an
			// erroneously inserted row must not silently hide the suppression regression.
			if err != nil || res != DemandWakeNone {
				t.Errorf("ineligible DemandWake=%v, %v; want None", res, err)
			}
			if f.wakeRows(r.ID) != 0 {
				t.Error("ineligible DemandWake inserted a wake row")
			}
			if f.state(bad) != "hibernated" || f.wakeColumns(bad) != before {
				t.Error("ineligible DemandWake changed sleeping worker")
			}
			if got := wakeCandidate(t, f, r.ID); got != good {
				t.Errorf("ineligible demand suppressed compatible wake: got %s want %s", got, good)
			}
			wantCodexWake(t, f, r.ID, good)
		})
	}
}

func TestCodexProviderWakeKeepsBusyCustodyFencesLiveDB(t *testing.T) {
	for _, kind := range []string{"busy", "custody"} {
		t.Run(kind, func(t *testing.T) {
			p, f := codexProviderWakeFixture(t)
			w, _ := f.worker()
			setWakeCaps(f, w, codexWakeCaps())
			r := frozenCodexWakeRun(t, p)
			want := RefusalBusy
			if kind == "busy" {
				f.env.exec(`UPDATE runs SET worker_id=$2 WHERE id=$1`, r.ID, w)
			} else {
				want = RefusalCustodyHold
				f.env.exec(`UPDATE runs SET status='completed' WHERE id=$1`, r.ID)
				f.env.exec(`INSERT INTO recovery_custody_holds(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id,inventory_guarded)
 VALUES($1,$2,$3,$4,1,'open',$5,'ident',$5,$4,true)`, uuid.New(), f.user, f.repo, r.ID, w)
			}
			if _, err := f.svc.HibernateWorker(f.env.ctx, f.actor, w, ""); refusalCode(err) != want {
				t.Fatalf("%s fence=%v want %s", kind, err, want)
			}
			f.want(w, "none")
			if f.wakeRows(r.ID) != 0 {
				t.Fatal("refused hibernation created wake row")
			}
		})
	}
}

func TestCodexProviderSourceKeepsClaudeChatWakeLiveDB(t *testing.T) {
	p, f := codexProviderWakeFixture(t)
	w := f.sleepingWorker()
	// No Claude provider source or vendor credential: selecting Codex alone must
	// not turn a normal Claude chat into a Codex/provider-bound chat.
	r := f.queuedRun()
	f.env.exec(`UPDATE runs SET kind='chat', harness='claude', repo_id=NULL, issue_iid=NULL WHERE id=$1`, r)
	if got := wakeCandidate(t, f, r); got != w {
		t.Fatalf("Claude chat wake=%s want %s", got, w)
	}
	wantCodexWake(t, f, r, w)
	f.settleOnReplacement(w, false)
	// Explicitly Codex-indicating chat remains outside wake admission, even on a
	// fully capable worker. No claim activation is manufactured.
	other := f.sleepingWorker()
	setWakeCaps(f, other, codexWakeCaps())
	codexChat := frozenCodexWakeRun(t, p)
	f.env.exec(`UPDATE runs SET kind='chat', repo_id=NULL, issue_iid=NULL WHERE id=$1`, codexChat.ID)
	if got := wakeCandidate(t, f, codexChat.ID); got != uuid.Nil {
		t.Fatalf("Codex chat enabled wake: %s", got)
	}
	if f.wakeRows(codexChat.ID) != 0 {
		t.Fatal("Codex chat acquired wake row")
	}
	f.want(other, "hibernated")
}
