package workersvc

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestForgeOriginNormalizationLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	for _, c := range forgeOriginFixtures(t) {
		t.Run(c.URL, func(t *testing.T) {
			got, err := e.q.CanonicalForgeOrigin(e.ctx, c.URL+".git")
			if err != nil || got != c.Origin {
				t.Fatalf("origin = %q, %v; want %q", got, err, c.Origin)
			}
		})
	}
}

func (e interlockLiveDB) originCooldown(t *testing.T, worker uuid.UUID, origin string) store.ForgeOriginCooldown {
	t.Helper()
	row, err := e.q.GetForgeOriginCooldown(e.ctx, store.GetForgeOriginCooldownParams{
		UserID: e.userID, WorkerID: worker, Origin: origin,
	})
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func (e interlockLiveDB) originCount(t *testing.T, worker uuid.UUID) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx, "SELECT count(*) FROM forge_origin_cooldowns WHERE worker_id = $1", worker).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestForgeOriginParkObservationLiveDB(t *testing.T) {
	for _, mode := range []string{"applied_lost_ack", "stale", "custody_refused", "cancel", "cap", "rollback", "released"} {
		t.Run(mode, func(t *testing.T) {
			e := setupInterlockLiveDB(t)
			w := e.seedWorker(t, nil)
			id := e.fpSeedRunning(t, w, 1)
			if mode != "custody_refused" {
				mhOpenHold(t, e, id, 1, w)
			}
			svc := e.forgeParkService(t, 6)
			gen := int64(1)
			switch mode {
			case "stale":
				gen = 0
			case "cancel":
				e.exec(t, "UPDATE runs SET stop_kind = 'cancelled', forge_park_count=6 WHERE id = $1", id)
			case "released":
				e.exec(t, "UPDATE runs SET claim_released_at = now() WHERE id = $1", id)
			case "cap":
				e.exec(t, "UPDATE runs SET forge_park_count = 6 WHERE id = $1", id)
			case "rollback":
				// Execute the generated query inside a rolled-back park transaction.
				tx, err := e.pool.Begin(e.ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = tx.Rollback(e.ctx) }()
				q := store.New(tx)
				if _, err := q.LockForgeOriginWorker(e.ctx, store.LockForgeOriginWorkerParams{RunID: id, WorkerID: w}); err != nil {
					t.Fatal(err)
				}
				if _, err := q.GetRunOwnedByWorkerForUpdate(e.ctx, store.GetRunOwnedByWorkerForUpdateParams{ID: id, WorkerID: pgconv.UUID(w)}); err != nil {
					t.Fatal(err)
				}
				if _, err := q.ParkRunForgeUnreachable(e.ctx, store.ParkRunForgeUnreachableParams{ID: id, WorkerID: pgconv.UUID(w), RetryNotBefore: pgconv.Time(time.Now().Add(time.Minute))}); err != nil {
					t.Fatal(err)
				}
				n, err := q.ObserveParkedForgeOrigin(e.ctx, store.ObserveParkedForgeOriginParams{RunID: id, WorkerID: w, Generation: 1, BaseMs: 60000, CapMs: 1800000})
				if err != nil || n != 1 {
					t.Fatalf("observe = %d, %v", n, err)
				}
				locked, err := q.LockForgeOriginCooldown(e.ctx, store.LockForgeOriginCooldownParams{UserID: e.userID, WorkerID: w, Origin: "https://forge.e2e"})
				if err != nil || locked.ObservationVersion != 1 {
					t.Fatalf("lock contract: %+v, %v", locked, err)
				}
				if err := tx.Rollback(e.ctx); err != nil {
					t.Fatal(err)
				}
				if e.originCount(t, w) != 0 {
					t.Fatal("rollback retained observation")
				}
				return
			}
			req := StateRequest{State: "recovery_wait", RecoveryCause: strPtr("forge_unreachable"), ClaimGeneration: &gen}
			_, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, id, req)
			switch mode {
			case "stale":
				if !errors.Is(err, ErrForgeParkStaleClaim) || applied {
					t.Fatalf("stale = %v, %v", applied, err)
				}
			case "custody_refused":
				if !errors.Is(err, ErrForgeParkCustodyUnsettled) || applied {
					t.Fatalf("custody = %v, %v", applied, err)
				}
			default:
				if err != nil || !applied {
					t.Fatalf("transition = %v, %v", applied, err)
				}
			}
			if mode != "applied_lost_ack" && mode != "cap" {
				if e.originCount(t, w) != 0 {
					t.Fatal("non-park observed network failure")
				}
				return
			}
			first := e.originCooldown(t, w, "https://forge.e2e")
			if first.ObservationVersion != 1 || first.FailureStep != 0 || first.LastReason != "transient_clone_fetch_exhausted" {
				t.Fatalf("first observation = %+v", first)
			}
			delay := first.CooledUntil.Time.Sub(first.ObservedAt.Time)
			if delay < 59*time.Second || delay > 61*time.Second {
				t.Fatalf("server backoff = %s", delay)
			}
			_, applied, err = svc.SetState(e.ctx, store.Worker{ID: w}, id, req)
			if err != nil || applied {
				t.Fatalf("duplicate = %v, %v", applied, err)
			}
			next := e.originCooldown(t, w, "https://forge.e2e")
			if next.ObservationVersion != 1 || !next.CooledUntil.Time.Equal(first.CooledUntil.Time) {
				t.Fatal("duplicate advanced observation")
			}
		})
	}
}

func TestForgeOriginClaimMappingBarrierLiveDB(t *testing.T) {
	for _, mode := range []string{"origin", "connection", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			e := setupInterlockLiveDB(t)
			w := e.seedWorker(t, nil)
			id := e.fpSeedRunning(t, w, 1)
			e.exec(t, "UPDATE runs SET status = 'claimed' WHERE id = $1", id)
			if _, err := e.q.GetRunClaimContext(e.ctx, id); err != nil {
				t.Fatalf("initial context: %v", err)
			}
			switch mode {
			case "origin":
				e.exec(t, "UPDATE repos SET web_url = 'https://other.example/repo' WHERE id = $1", e.repoID)
			case "malformed":
				e.exec(t, "UPDATE repos SET web_url = 'not a URL' WHERE id = $1", e.repoID)
			case "connection":
				conn := uuid.New()
				e.exec(t, `INSERT INTO forge_connections (id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext)
                    VALUES ($1,$2,'gitlab','https://api-other.example','bot',1,$3)`, conn, e.userID, []byte("x"))
				e.exec(t, "UPDATE repos SET connection_id = $2 WHERE id = $1", e.repoID, conn)
			}
			if _, err := e.q.GetRunClaimContext(e.ctx, id); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("edited context: %v", err)
			}
			run, err := e.q.GetRunByID(e.ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			mhOpenHold(t, e, id, 1, w)
			svc := e.forgeParkService(t, 6)
			payload, err := svc.assembleClaim(e.ctx, store.Worker{ID: w}, run)
			if payload != nil || !errors.Is(err, errForgeOriginClaimRefused) || errors.Is(err, errRunRepoOwnerMismatch) {
				t.Fatalf("assembly delivered mismatched mapping: payload=%v err=%v", payload != nil, err)
			}
			payload, err = svc.finishRunClaim(e.ctx, run, payload, err, claimRecoveryIdentity{workerID: w, recoveryCapable: true})
			if payload != nil || err != nil {
				t.Fatalf("mapping refusal cleanup = %v, %v", payload != nil, err)
			}
			final, err := e.q.GetRunByID(e.ctx, id)
			if err != nil || final.Status != "failed" || final.ClaimGeneration != 1 ||
				!strings.Contains(final.FailureReason.String, errForgeOriginClaimRefused.Error()) {
				t.Fatalf("mapping refusal state = %+v, %v", final, err)
			}
		})
	}
}

// Another repository retaining the old origin must not authorize this run after
// its own mapping changes between claim and the service's park transaction.
func TestForgeOriginParkMappingEditLiveDB(t *testing.T) {
	for _, mode := range []string{"origin", "connection", "cap_origin", "cap_connection"} {
		t.Run(mode, func(t *testing.T) {
			e := setupInterlockLiveDB(t)
			w := e.seedWorker(t, nil)
			id := e.fpSeedRunning(t, w, 1)
			mhOpenHold(t, e, id, 1, w)
			e.exec(t, `INSERT INTO repos (id,connection_id,forge_project_id,path_with_namespace,web_url)
                SELECT $2,connection_id,999,'group/retained','https://forge.e2e/group/retained'
                FROM repos WHERE id=$1`, e.repoID, uuid.New())
			if strings.HasPrefix(mode, "cap_") {
				e.exec(t, "UPDATE runs SET forge_park_count=6 WHERE id=$1", id)
			}
			if strings.HasSuffix(mode, "origin") {
				e.exec(t, "UPDATE repos SET web_url='https://edited.example/repo' WHERE id=$1", e.repoID)
			} else {
				conn := uuid.New()
				e.exec(t, `INSERT INTO forge_connections (id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext)
                    VALUES ($1,$2,'gitlab','https://edited.example','bot',1,$3)`, conn, e.userID, []byte("x"))
				e.exec(t, "UPDATE repos SET connection_id=$2 WHERE id=$1", e.repoID, conn)
			}
			_, applied, err := e.forgeParkService(t, 6).SetState(e.ctx, store.Worker{ID: w}, id,
				StateRequest{State: "recovery_wait", RecoveryCause: strPtr("forge_unreachable"), ClaimGeneration: i64Ptr(1)})
			if err != nil || !applied {
				t.Fatalf("park = %v, %v", applied, err)
			}
			if e.originCount(t, w) != 0 {
				t.Fatal("edited mapping recorded an observation using another repository's authorization")
			}
		})
	}
}

func TestForgeOriginFreshUnknownClaimLiveDB(t *testing.T) {
	for _, raw := range []string{"not a URL", "https://[bad]/repo", "http://forge.e2e/repo"} {
		t.Run(raw, func(t *testing.T) {
			e := setupInterlockLiveDB(t)
			w := e.seedWorker(t, nil)
			e.exec(t, "UPDATE repos SET web_url=$2 WHERE id=$1", e.repoID, raw)
			id := e.fpSeedRunning(t, w, 1)
			e.exec(t, "UPDATE runs SET status='claimed' WHERE id=$1", id)
			var unknown bool
			if err := e.pool.QueryRow(e.ctx, "SELECT origin IS NULL FROM run_forge_origins WHERE run_id=$1 AND generation=1", id).Scan(&unknown); err != nil || !unknown {
				t.Fatalf("fresh unknown snapshot = %v, %v", unknown, err)
			}
			if _, err := e.q.GetRunClaimContext(e.ctx, id); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("fresh malformed context = %v", err)
			}
			// Fixing the URL cannot reauthorize an already-stamped unknown generation.
			e.exec(t, "UPDATE repos SET web_url='https://forge.e2e/repo' WHERE id=$1", e.repoID)
			run, err := e.q.GetRunByID(e.ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			mhOpenHold(t, e, id, 1, w)
			svc := e.forgeParkService(t, 6)
			payload, err := svc.assembleClaim(e.ctx, store.Worker{ID: w}, run)
			if payload != nil || !errors.Is(err, errForgeOriginClaimRefused) || errors.Is(err, errRunRepoOwnerMismatch) {
				t.Fatalf("fresh malformed assembly = %v, %v", payload != nil, err)
			}
			payload, err = svc.finishRunClaim(e.ctx, run, payload, err, claimRecoveryIdentity{workerID: w, recoveryCapable: true})
			if payload != nil || err != nil {
				t.Fatalf("fenced refusal = %v, %v", payload != nil, err)
			}
			final, err := e.q.GetRunByID(e.ctx, id)
			if err != nil || final.Status != "failed" || final.ClaimGeneration != 1 {
				t.Fatalf("fenced refusal state = %+v, %v", final, err)
			}
		})
	}
}

// A claimed generation that predates the new tables has no observation snapshot.
// This fixture reproduces that persisted state without inventing claim history.
func TestForgeOriginLegacyClaimWithoutSnapshotLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	w := e.seedWorker(t, nil)
	id := e.fpSeedRunning(t, w, 1)
	e.exec(t, "UPDATE runs SET status='claimed' WHERE id=$1", id)
	e.exec(t, "DELETE FROM run_forge_origins WHERE run_id=$1", id)
	e.exec(t, "UPDATE repos SET web_url='https://edited.example/repo' WHERE id=$1", e.repoID)
	if _, err := e.q.GetRunClaimContext(e.ctx, id); err != nil {
		t.Fatalf("historical in-flight claim was reauthorized or refused: %v", err)
	}
	var snapshots int
	if err := e.pool.QueryRow(e.ctx, "SELECT count(*) FROM run_forge_origins WHERE run_id=$1", id).Scan(&snapshots); err != nil || snapshots != 0 {
		t.Fatalf("historical claim invented a snapshot: %d, %v", snapshots, err)
	}
	// The next generation is stamped normally and cannot silently change origin.
	e.exec(t, "UPDATE runs SET claim_generation=2 WHERE id=$1", id)
	e.exec(t, "UPDATE repos SET web_url='https://newer.example/repo' WHERE id=$1", e.repoID)
	if _, err := e.q.GetRunClaimContext(e.ctx, id); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("new generation bypassed its frozen mapping: %v", err)
	}
}

// Isolated research uses the owner-scoped context to mint its fetch credential,
// but never receives a repository or Git credential. Git placement metadata must
// not become an authorization check for this noncloning lane.
func TestForgeOriginIsolatedClaimIgnoresGitMappingLiveDB(t *testing.T) {
	f := newIsoFix(t)
	id := f.queuedRun(t, 3585, true)
	lane := f.laneWorker(t, id)
	f.env.exec("UPDATE runs SET status='claimed', worker_id=$2, claim_generation=1 WHERE id=$1", id, lane.ID)
	f.env.exec("UPDATE repos SET web_url='https://edited.example/repo' WHERE id=$1", f.repo)
	run, err := f.env.q.GetRunByID(f.env.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := f.svc.assembleClaim(f.env.ctx, lane, run)
	if err != nil || payload == nil || payload.IsolatedFetch == nil {
		t.Fatalf("noncloning claim refused by Git placement metadata: payload=%v err=%v", payload != nil, err)
	}
	if payload.Repo != (ClaimRepo{}) || payload.Secrets.ForgePAT != "" || payload.Secrets.ForgeUsername != "" {
		t.Fatal("noncloning exemption exposed Git identity or credentials")
	}
	// The exemption removes only a Git-placement check, never owner scoping.
	other := uuid.New()
	f.env.exec("INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')", other, other.String()+"@example.com")
	t.Cleanup(func() { f.env.exec("DELETE FROM users WHERE id=$1", other) })
	f.env.exec("UPDATE forge_connections SET user_id=$2 WHERE id=(SELECT connection_id FROM repos WHERE id=$1)", f.repo, other)
	if _, err := f.env.q.GetRunClaimContext(f.env.ctx, id); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("noncloning exemption crossed connection ownership: %v", err)
	}
}

func TestForgeOriginConcurrentFailuresLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	w := e.seedWorker(t, nil)
	ids := []uuid.UUID{e.fpSeedRunning(t, w, 1), e.fpSeedRunning(t, w, 1)}
	for _, id := range ids {
		mhOpenHold(t, e, id, 1, w)
	}
	svc := e.forgeParkService(t, 6)
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(id uuid.UUID) {
			defer wg.Done()
			<-start
			_, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, id, StateRequest{State: "recovery_wait", RecoveryCause: strPtr("forge_unreachable"), ClaimGeneration: i64Ptr(1)})
			if err == nil && !applied {
				err = fmt.Errorf("park not applied")
			}
			results <- err
		}(id)
	}
	close(start)
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	c := e.originCooldown(t, w, "https://forge.e2e")
	if c.ObservationVersion != 2 || c.FailureStep != 1 {
		t.Fatalf("lost concurrent observation: %+v", c)
	}
	delay := c.CooledUntil.Time.Sub(c.ObservedAt.Time)
	if delay < 119*time.Second || delay > 121*time.Second {
		t.Fatalf("second backoff = %s", delay)
	}
}

func (e interlockLiveDB) seedOriginReservation(t *testing.T, id, w uuid.UUID) uuid.UUID {
	t.Helper()
	incarnation := uuid.New()
	e.exec(t, `INSERT INTO forge_origin_cooldowns
        (user_id,worker_id,origin,incarnation,cooled_until,last_reason,observed_at,
         reservation_run_id,reservation_generation,reservation_incarnation,reservation_version,reservation_observation_version)
        VALUES ($1,$2,'https://forge.e2e',$3,now(),'transient_clone_fetch_exhausted',now(),$4,1,$3,7,1)`, e.userID, w, incarnation, id)
	e.exec(t, "UPDATE run_forge_origins SET reservation_incarnation = $2, reservation_version = 7 WHERE run_id = $1", id, incarnation)
	return incarnation
}

func TestForgeOriginReservationLifecycleLiveDB(t *testing.T) {
	cases := []struct {
		name, change string
		kept         bool
	}{
		{"claimed", "status = 'claimed'", true}, {"running", "status = 'running'", true},
		{"approval", "status = 'awaiting_approval'", true}, {"input", "status = 'awaiting_input'", true},
		{"followup", "status = 'awaiting_followup'", true},
		{"park", "status = 'recovery_wait'", false}, {"failed", "status = 'failed'", false},
		{"completed", "status = 'completed'", false}, {"cancelled", "status = 'cancelled'", false},
		{"requeue", "status = 'queued'", false}, {"release", "claim_released_at = now()", false},
		{"generation", "claim_generation = 2", false}, {"worker", "worker_id = NULL", false},
		{"cancel_stamp", "stop_kind = 'cancelled'", false}, {"delete", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := setupInterlockLiveDB(t)
			w := e.seedWorker(t, nil)
			id := e.fpSeedRunning(t, w, 1)
			e.seedOriginReservation(t, id, w)
			if c.name == "delete" {
				e.exec(t, "DELETE FROM runs WHERE id = $1", id)
			} else {
				e.exec(t, "UPDATE runs SET "+c.change+" WHERE id = $1", id)
			}
			row := e.originCooldown(t, w, "https://forge.e2e")
			if row.ReservationRunID.Valid != c.kept {
				t.Fatalf("reservation valid = %v, want %v", row.ReservationRunID.Valid, c.kept)
			}
		})
	}
}

func TestForgeOriginReservationABALiveDB(t *testing.T) {
	for _, mode := range []string{"incarnation", "version", "generation", "run"} {
		t.Run(mode, func(t *testing.T) {
			e := setupInterlockLiveDB(t)
			w := e.seedWorker(t, nil)
			id := e.fpSeedRunning(t, w, 1)
			old := e.seedOriginReservation(t, id, w)
			switch mode {
			case "incarnation":
				e.exec(t, "DELETE FROM forge_origin_cooldowns WHERE worker_id = $1", w)
				fresh := uuid.New()
				e.exec(t, `INSERT INTO forge_origin_cooldowns
                    (user_id,worker_id,origin,incarnation,cooled_until,last_reason,observed_at,reservation_run_id,reservation_generation,reservation_incarnation,reservation_version,reservation_observation_version)
                    VALUES ($1,$2,'https://forge.e2e',$3,now(),'transient_clone_fetch_exhausted',now(),$4,1,$3,7,1)`, e.userID, w, fresh, id)
				if fresh == old {
					t.Fatal("fixture reused incarnation")
				}
			case "version":
				e.exec(t, "UPDATE forge_origin_cooldowns SET reservation_version = 8 WHERE worker_id = $1", w)
			case "generation":
				e.exec(t, "UPDATE forge_origin_cooldowns SET reservation_generation = 2 WHERE worker_id = $1", w)
			case "run":
				e.exec(t, "UPDATE forge_origin_cooldowns SET reservation_run_id = $2 WHERE worker_id = $1", w, uuid.New())
			}
			e.exec(t, "UPDATE runs SET status = 'queued' WHERE id = $1", id)
			if !e.originCooldown(t, w, "https://forge.e2e").ReservationRunID.Valid {
				t.Fatal("old lifecycle released newer reservation")
			}
		})
	}
}
