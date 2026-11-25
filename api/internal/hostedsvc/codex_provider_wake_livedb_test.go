package hostedsvc

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// These are placement/composition proofs only. They neither claim through the
// worker service nor deliver credentials. Advertisement awaits completed M1-M4 delivery.
// Native Codex provenance and Claude controls remain in
// TestHibernationWakeRechecksCodexProvenanceLiveDB and
// TestHibernationWakeSuppressesEphemeralProvisioningLiveDB.
func codexProviderWakeRun(e *wakeEnv) uuid.UUID {
	e.t.Helper()
	run := e.queuedRun()
	p, err := e.q.InsertModelProvider(e.ctx, store.InsertModelProviderParams{
		OwnerUserID: e.user, Name: uuid.NewString(), Engine: "codex",
	})
	if err != nil {
		e.t.Fatal(err)
	}
	if err := e.q.InsertModelProviderRevision(e.ctx, store.InsertModelProviderRevisionParams{
		ProviderID: p.ID, Revision: 1, Protocol: "openai_responses",
		BaseUrl: "https://example.test/v1", AuthStyle: "bearer",
		ModelMap: []byte(`{"default":{"model":"gateway"}}`),
	}); err != nil {
		e.t.Fatal(err)
	}
	key, err := e.q.InsertProviderKeySecret(e.ctx, store.InsertProviderKeySecretParams{
		UserID: e.user, Label: p.ID.String(), Ciphertext: []byte("ct"),
		SealedWith: "master", ProviderID: pgconv.UUID(p.ID),
	})
	if err != nil {
		e.t.Fatal(err)
	}
	e.exec(`UPDATE runs SET harness='codex',
		anthropic_secret_id=NULL, codex_secret_id=NULL, codex_auth_mode=NULL, credential_override_secret_id=NULL,
		model_provider_id=$2, model_provider_revision=1, model_provider_key_mode='user_key',
		model_provider_key_secret_id=$3, model_provider_key_rev=(SELECT enablement_rev FROM user_secrets WHERE id=$3)
		WHERE id=$1`, run, p.ID, key.ID)
	return run
}

func codexProviderWakeCaps() []string {
	return []string{"codex_harness_v1", "codex_runtime_v2", "codex_runtime_v3", "codex_provider_v1"}
}

func assertCodexProviderEphemeral(e *wakeEnv, run uuid.UUID) uuid.UUID {
	e.t.Helper()
	var worker uuid.UUID
	if err := e.pool.QueryRow(e.ctx, `SELECT id FROM workers
		WHERE user_id=$1 AND ephemeral AND ephemeral_run_id=$2`, e.user, run).Scan(&worker); err != nil {
		e.t.Fatalf("ephemeral bound to provider run %s: %v", run, err)
	}
	if e.ephemeralWorkers() != 1 {
		e.t.Fatalf("ephemeral count=%d, want exactly one", e.ephemeralWorkers())
	}
	return worker
}

func TestCodexProviderWakeProvisionMatrixLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name     string
		caps     []string
		eligible bool
	}{
		{"legacy", nil, false},
		{"claude-provider-A1-only", []string{"claude_provider_v1"}, false},
		{"native-codex-runtime", []string{"codex_harness_v1", "codex_runtime_v2"}, false},
		{"full-codex-provider", codexProviderWakeCaps(), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newWakeEnv(t, true)
			w := e.sleeper(nil, tc.caps, 2*time.Hour)
			run := codexProviderWakeRun(e)
			oldOp := e.op(w)
			wantWake := int64(0)
			if tc.eligible {
				wantWake = 1
			}
			if got := e.wake(); got != wantWake {
				t.Fatalf("WakePass=%d, want %d", got, wantWake)
			}
			if !tc.eligible {
				if e.state(w) != "hibernated" || e.op(w) != oldOp || e.wakeRowsFor(run) != 0 {
					t.Fatal("ineligible sleeper changed or recorded demand")
				}
				if got := e.provision(); got != 1 {
					t.Fatalf("ineligible sleeper suppressed provision: %d", got)
				}
				assertCodexProviderEphemeral(e, run)
				return
			}
			if e.state(w) != "waking" || e.wakeRowsFor(run) != 1 ||
				e.count(`SELECT count(*) FROM hibernation_wakes WHERE run_id=$1 AND worker_id=$2`, run, w) != 1 {
				t.Fatal("eligible sleeper did not receive the run's wake")
			}
			if e.wakeOrigin(w) != "demand" {
				t.Fatalf("wake origin=%q, want demand", e.wakeOrigin(w))
			}
			op := e.op(w)
			// Two fixed repeat passes: no polling or wall-clock waits.
			for i := 0; i < 2; i++ {
				if got := e.wake(); got != 0 {
					t.Fatalf("repeat wake=%d", got)
				}
				if got := e.provision(); got != 0 || e.ephemeralWorkers() != 0 {
					t.Fatalf("covered demand provisioned %d workers", got)
				}
			}
			if e.op(w) != op || e.wakeRowsFor(run) != 1 {
				t.Fatal("repeat passes reminted operation or duplicated demand")
			}
		})
	}
}

func TestCodexProviderWakeCapabilityLossLiveDB(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		name := "provision"
		if replacement {
			name = "replacement-sleeper"
		}
		t.Run(name, func(t *testing.T) {
			e := newWakeEnv(t, true)
			// Older incompatible sleepers must not outrank the compatible sleeper.
			for _, caps := range [][]string{nil, {"claude_provider_v1"}, {"codex_harness_v1", "codex_runtime_v2"}} {
				e.sleeper(nil, caps, 3*time.Hour)
			}
			w := e.sleeper(nil, codexProviderWakeCaps(), 2*time.Hour)
			run := codexProviderWakeRun(e)
			if got := e.wake(); got != 1 || e.state(w) != "waking" {
				t.Fatalf("compatible wake=%d state=%s", got, e.state(w))
			}
			if got := e.provision(); got != 0 || e.ephemeralWorkers() != 0 {
				t.Fatalf("initial covered provision=%d", got)
			}
			e.exec(`UPDATE workers SET protocol_capabilities=$2 WHERE id=$1`, w,
				[]string{"codex_harness_v1", "codex_runtime_v2"})
			next := uuid.Nil
			if replacement {
				next = e.sleeper(nil, codexProviderWakeCaps(), time.Hour)
			}
			want := int64(0)
			if replacement {
				want = 1
			}
			if got := e.wake(); got != want {
				t.Fatalf("after capability loss wake=%d, want %d", got, want)
			}
			if e.count(`SELECT count(*) FROM hibernation_wakes WHERE run_id=$1 AND worker_id=$2`, run, w) != 0 {
				t.Fatal("stale demand still names incapable worker")
			}
			if replacement {
				if e.state(next) != "waking" || e.count(`SELECT count(*) FROM hibernation_wakes WHERE run_id=$1 AND worker_id=$2`, run, next) != 1 {
					t.Fatal("replacement did not receive demand")
				}
				if got := e.provision(); got != 0 || e.ephemeralWorkers() != 0 {
					t.Fatalf("replacement wake failed to suppress provision: %d", got)
				}
			} else {
				if e.wakeRowsFor(run) != 0 {
					t.Fatal("stale demand was not released")
				}
				if got := e.provision(); got != 1 {
					t.Fatalf("capability loss did not allow provision: %d", got)
				}
				assertCodexProviderEphemeral(e, run)
			}
		})
	}
}

func TestCodexProviderWakeLeaseLiveDB(t *testing.T) {
	for _, compatible := range []bool{false, true} {
		name := "incompatible-lease"
		if compatible {
			name = "compatible-lease"
		}
		t.Run(name, func(t *testing.T) {
			e := newWakeEnv(t, true)
			e.settings.lease = time.Hour
			run := codexProviderWakeRun(e)
			w := e.sleeper(nil, codexProviderWakeCaps(), time.Hour)
			leased := e.leasedEphemeral(run)
			caps := []string{"codex_harness_v1", "codex_runtime_v2"}
			if compatible {
				caps = codexProviderWakeCaps()
			}
			e.exec(`UPDATE workers SET protocol_capabilities=$2 WHERE id=$1`, leased, caps)
			want := int64(1)
			if compatible {
				want = 0
			}
			if got := e.wake(); got != want {
				t.Fatalf("WakePass=%d, want %d", got, want)
			}
			if compatible {
				if e.state(w) != "hibernated" || e.wakeRowsFor(run) != 0 {
					t.Fatal("compatible live lease did not cover demand")
				}
			} else if e.state(w) != "waking" || e.wakeRowsFor(run) != 1 {
				t.Fatal("incompatible lease suppressed eligible wake")
			}
			if got := e.provision(); got != 0 {
				t.Fatalf("lease or wake covered demand but provision=%d", got)
			}
			if e.count(`SELECT count(*) FROM workers WHERE user_id=$1 AND ephemeral AND ephemeral_run_id=$2`, e.user, run) != 0 {
				t.Fatal("created a bound duplicate despite lease or wake coverage")
			}
		})
	}
}

// With no sleeper or wake, only the lease protocol can suppress provisioning.
func TestCodexProviderLeaseProvisionWithoutWakeLiveDB(t *testing.T) {
	for _, compatible := range []bool{false, true} {
		name := "incompatible"
		if compatible {
			name = "compatible"
		}
		t.Run(name, func(t *testing.T) {
			e := newWakeEnv(t, true)
			e.settings.lease = time.Hour
			run := codexProviderWakeRun(e)
			leased := e.leasedEphemeral(run)
			caps := []string{"codex_harness_v1", "codex_runtime_v2"}
			if compatible {
				caps = codexProviderWakeCaps()
			}
			e.exec(`UPDATE workers SET protocol_capabilities=$2 WHERE id=$1`, leased, caps)
			if e.wake() != 0 || e.wakeRowsFor(run) != 0 {
				t.Fatal("fixture unexpectedly supplied wake coverage")
			}
			want := int64(1)
			if compatible {
				want = 0
			}
			if got := e.provision(); got != want {
				t.Fatalf("ProvisionPass=%d, want %d with compatible lease=%t", got, want, compatible)
			}
			if bound := e.count(`SELECT count(*) FROM workers WHERE user_id=$1 AND ephemeral AND ephemeral_run_id=$2`, e.user, run); bound != int(want) {
				t.Fatalf("bound replacements=%d, want %d", bound, want)
			}
			if e.ephemeralWorkers() != 1+int(want) || e.count(`SELECT count(*) FROM workers WHERE id=$1`, leased) != 1 {
				t.Fatal("lease control or replacement worker count changed unexpectedly")
			}
		})
	}
}

func TestCodexProviderReapProvisionLiveDB(t *testing.T) {
	for _, guard := range []string{"none", "busy", "custody", "previously-claimed", "full-v3"} {
		t.Run(guard, func(t *testing.T) {
			e := newWakeEnv(t, true)
			run := codexProviderWakeRun(e)
			if got := e.provision(); got != 1 {
				t.Fatalf("initial provision=%d", got)
			}
			w := assertCodexProviderEphemeral(e, run)
			// Registered recently: incompatibility, not startup timeout, must reap it.
			e.exec(`UPDATE workers SET status='online', online_since=now(), last_heartbeat_at=now(),
				protocol_capabilities=$2 WHERE id=$1`, w, []string{"codex_provider_v1", "codex_harness_v1", "codex_runtime_v2"})
			switch guard {
			case "previously-claimed":
				e.exec("UPDATE runs SET claim_generation=1 WHERE id=$1", run)
			case "full-v3":
				e.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", w, codexProviderWakeCaps())
			case "busy":
				active := e.queuedRun()
				e.exec(`UPDATE runs SET status='running', worker_id=$2 WHERE id=$1`, active, w)
			case "custody":
				e.exec(`INSERT INTO recovery_custody_holds
					(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id)
					VALUES ($1,$2,$3,$4,1,'open',$5,'ident',$5,$4)`, uuid.New(), e.user, e.repo, run, w)
			}
			if got := e.provision(); got != 0 {
				t.Fatalf("occupied uniqueness slot provisioned duplicate: %d", got)
			}
			if _, err := e.prov.ReapPass(e.ctx); err != nil {
				t.Fatalf("ReapPass: %v", err)
			}
			exists := e.count(`SELECT count(*) FROM workers WHERE id=$1`, w)
			if guard != "none" {
				if exists != 1 {
					t.Fatal("reaper deleted guarded worker")
				}
				if got := e.provision(); got != 0 {
					t.Fatalf("guarded slot allowed duplicate provision: %d", got)
				}
				return
			}
			if exists != 0 {
				t.Fatal("incompatible provider-bound ephemeral survived ReapPass")
			}
			if got := e.provision(); got != 1 {
				t.Fatalf("freed slot did not provision replacement: %d", got)
			}
			if next := assertCodexProviderEphemeral(e, run); next == w {
				t.Fatal("replacement reused deleted worker identity")
			}
		})
	}
}
