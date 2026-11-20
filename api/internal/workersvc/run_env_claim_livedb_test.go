package workersvc

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/runenv"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

// PRD #4416 M2: the repo-variable claim, against a real PostgreSQL (skipped without
// UZI_TEST_DATABASE_URL). The fixture is newCDFix (a queued issue run on a worker) plus the run_env_v1
// capability.

func newRunEnvFix(t *testing.T) *cdFix {
	t.Helper()
	fx := newCDFix(t)
	fx.setRunEnvCap(true)
	return fx
}

func (fx *cdFix) setRunEnvCap(on bool) {
	if on {
		fx.env.exec(`UPDATE workers SET protocol_capabilities = ARRAY(SELECT DISTINCT unnest(protocol_capabilities || ARRAY[$2]::text[])) WHERE id = $1`, fx.workerID, capability.RunEnvV1)
		return
	}
	fx.env.exec(`UPDATE workers SET protocol_capabilities = array_remove(protocol_capabilities, $2) WHERE id = $1`, fx.workerID, capability.RunEnvV1)
}

// insertEnv seals value under the version 2 context and inserts one run_env_vars row.
func (fx *cdFix) insertEnv(t *testing.T, varID uuid.UUID, rev int32, name, value string, secret bool) {
	t.Helper()
	repo := fx.repoID
	material, err := fx.env.vlt.Seal(fx.env.ctx, fx.userID, runenv.AADKindV2(runenv.ScopeRepo, &repo, secret, name, varID, int(rev)), []byte(value), vault.SealForUse)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, err := fx.env.q.InsertRunEnvVar(fx.env.ctx, store.InsertRunEnvVarParams{
		VarID: varID, UserID: fx.userID, RepoID: pgtype.UUID{Bytes: repo, Valid: true}, Name: name, Secret: secret,
		Description: "about " + name, Ciphertext: material.Ciphertext, SealedWith: material.SealedWith,
		AadVersion: runenv.AADVersion2, ValueBytes: int32(len(value)), Rev: rev,
	}); err != nil {
		t.Fatalf("insert run env var: %v", err)
	}
}

// putEnv creates a new variable at revision 1.
func (fx *cdFix) putEnv(t *testing.T, name, value string, secret bool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	fx.insertEnv(t, id, 1, name, value, secret)
	return id
}

// rotateEnv retires the live version of a variable and inserts the next revision.
func (fx *cdFix) rotateEnv(t *testing.T, varID uuid.UUID, name, value string, secret bool) int32 {
	t.Helper()
	var rev int32
	if err := fx.env.pool.QueryRow(fx.env.ctx, `UPDATE run_env_vars SET retired_at = now() WHERE var_id = $1 AND retired_at IS NULL RETURNING rev`, varID).Scan(&rev); err != nil {
		t.Fatalf("retire: %v", err)
	}
	fx.insertEnv(t, varID, rev+1, name, value, secret)
	return rev + 1
}

type envReceipt struct {
	secret      bool
	first, last int64
}

func (fx *cdFix) receipts(t *testing.T, runID uuid.UUID) map[string]envReceipt {
	t.Helper()
	rows, err := fx.env.pool.Query(fx.env.ctx, `SELECT name, rev, secret, first_claim_generation, last_claim_generation FROM run_env_receipts WHERE run_id = $1`, runID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]envReceipt{}
	for rows.Next() {
		var name string
		var rev int32
		var r envReceipt
		if err := rows.Scan(&name, &rev, &r.secret, &r.first, &r.last); err != nil {
			t.Fatal(err)
		}
		out[name+"/"+string(rune('0'+rev))] = r
	}
	return out
}

// requeue returns a claimed run to queued with its worker affinity, as a later claim sees it.
func (fx *cdFix) requeue(t *testing.T, runID uuid.UUID) {
	t.Helper()
	fx.env.exec(`UPDATE runs SET status = 'queued', status_since = now(), claim_released_at = now() WHERE id = $1`, runID)
}

func (fx *cdFix) claim(t *testing.T) *ClaimPayload {
	t.Helper()
	p, err := fx.svc.Claim(fx.env.ctx, fx.worker(t), nil)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	return p
}

func envNames(p *ClaimPayload) []string {
	var out []string
	for _, v := range p.Secrets.RunEnv {
		out = append(out, v.Name+"="+v.Value)
	}
	sort.Strings(out)
	return out
}

func redactNames(p *ClaimPayload) []string {
	var out []string
	for _, v := range p.Secrets.RunEnvRedact {
		out = append(out, v.Name+"="+v.Value)
	}
	sort.Strings(out)
	return out
}

func eq(a, b []string) bool { return strings.Join(a, "\x1f") == strings.Join(b, "\x1f") }

// TestRunEnvClaimDeliversAndReceiptsLiveDB: a claim delivers the live variables (plain and secret,
// with description), writes one receipt per version, and a later rotation reaches only the next claim,
// which also redacts the retired secret version. Receipts accumulate; a repeat claim adds none.
func TestRunEnvClaimDeliversAndReceiptsLiveDB(t *testing.T) {
	fx := newRunEnvFix(t)
	id := fx.queuedRun(t, "issue")
	plain := fx.putEnv(t, "PLAIN_ONE", "alpha", false)
	secret := fx.putEnv(t, "SECRET_ONE", "sekret-1", true)
	_ = plain

	first := fx.claim(t)
	if first == nil {
		t.Fatal("first claim delivered nothing")
	}
	if got, want := envNames(first), []string{"PLAIN_ONE=alpha", "SECRET_ONE=sekret-1"}; !eq(got, want) {
		t.Fatalf("run_env = %v, want %v", got, want)
	}
	if len(first.Secrets.RunEnvRedact) != 0 {
		t.Fatalf("run_env_redact = %v on a first claim", redactNames(first))
	}
	for _, v := range first.Secrets.RunEnv {
		if v.Description != "about "+v.Name || v.Scope != "repo" || v.Rev != 1 || v.Secret != (v.Name == "SECRET_ONE") {
			t.Fatalf("run_env entry = %+v", v)
		}
	}
	raw, err := json.Marshal(first)
	if err != nil || strings.Contains(string(raw), "runEnv") || strings.Contains(string(raw), "varID") {
		t.Fatalf("the assembly snapshot leaked into the wire form: %v", err)
	}
	rec := fx.receipts(t, id)
	if len(rec) != 2 || rec["PLAIN_ONE/1"].secret || !rec["SECRET_ONE/1"].secret || rec["SECRET_ONE/1"].first != 1 || rec["SECRET_ONE/1"].last != 1 {
		t.Fatalf("receipts after claim 1 = %+v", rec)
	}

	// A change after the finish reaches only the next claim.
	fx.rotateEnv(t, secret, "SECRET_ONE", "sekret-2", true)
	if got := envNames(first); !eq(got, []string{"PLAIN_ONE=alpha", "SECRET_ONE=sekret-1"}) {
		t.Fatalf("the delivered claim changed: %v", got)
	}
	fx.requeue(t, id)
	second := fx.claim(t)
	if second == nil {
		t.Fatal("second claim delivered nothing")
	}
	if got, want := envNames(second), []string{"PLAIN_ONE=alpha", "SECRET_ONE=sekret-2"}; !eq(got, want) {
		t.Fatalf("run_env 2 = %v, want %v", got, want)
	}
	if got, want := redactNames(second), []string{"SECRET_ONE=sekret-1"}; !eq(got, want) {
		t.Fatalf("run_env_redact 2 = %v, want %v", got, want)
	}
	rec = fx.receipts(t, id)
	if len(rec) != 3 || rec["PLAIN_ONE/1"].first != 1 || rec["PLAIN_ONE/1"].last != 2 ||
		rec["SECRET_ONE/1"].first != 1 || rec["SECRET_ONE/1"].last != 2 ||
		rec["SECRET_ONE/2"].first != 2 || rec["SECRET_ONE/2"].last != 2 {
		t.Fatalf("receipts after claim 2 = %+v", rec)
	}

	// Dedup: a repeat claim with no change adds no receipt and repeats the same lists.
	fx.requeue(t, id)
	third := fx.claim(t)
	if third == nil || !eq(envNames(third), envNames(second)) || !eq(redactNames(third), redactNames(second)) {
		t.Fatalf("third claim = %+v", third)
	}
	if rec = fx.receipts(t, id); len(rec) != 3 || rec["SECRET_ONE/1"].last != 3 {
		t.Fatalf("receipts after claim 3 = %+v", rec)
	}
	// The owner's run page names what the latest claim delivered, never a value.
	rows, err := fx.env.q.ListRunInjectedEnv(fx.env.ctx, store.ListRunInjectedEnvParams{RunID: id, ClaimGeneration: 3})
	if err != nil || len(rows) != 3 {
		t.Fatalf("injected env rows = %v, %v", rows, err)
	}
}

// TestRunEnvClaimRaceRequeuesLiveDB: any change to the effective set between assembly and the finish
// (create, rotate, delete, disable, enable) requeues the exact claim with nothing delivered and no
// receipt; the next claim delivers the new state.
func TestRunEnvClaimRaceRequeuesLiveDB(t *testing.T) {
	type tc struct {
		name   string
		setup  func(fx *cdFix) uuid.UUID
		mutate func(fx *cdFix, v uuid.UUID)
		want   []string
	}
	cases := []tc{
		{"create", func(fx *cdFix) uuid.UUID { return fx.putEnv(t, "FIRST", "one", false) },
			func(fx *cdFix, _ uuid.UUID) { fx.putEnv(t, "SECOND", "two", false) }, []string{"FIRST=one", "SECOND=two"}},
		{"rotate", func(fx *cdFix) uuid.UUID { return fx.putEnv(t, "FIRST", "one", false) },
			func(fx *cdFix, v uuid.UUID) { fx.rotateEnv(t, v, "FIRST", "uno", false) }, []string{"FIRST=uno"}},
		{"delete", func(fx *cdFix) uuid.UUID { return fx.putEnv(t, "FIRST", "one", false) },
			func(fx *cdFix, v uuid.UUID) {
				fx.env.exec(`UPDATE run_env_vars SET pending_delete_at = now() WHERE var_id = $1`, v)
			}, nil},
		{"disable", func(fx *cdFix) uuid.UUID { return fx.putEnv(t, "FIRST", "one", false) },
			func(fx *cdFix, v uuid.UUID) {
				fx.env.exec(`UPDATE run_env_vars SET disabled_at = now() WHERE var_id = $1`, v)
			}, nil},
		{"enable", func(fx *cdFix) uuid.UUID {
			v := fx.putEnv(t, "FIRST", "one", false)
			fx.env.exec(`UPDATE run_env_vars SET disabled_at = now() WHERE var_id = $1`, v)
			return v
		}, func(fx *cdFix, v uuid.UUID) {
			fx.env.exec(`UPDATE run_env_vars SET disabled_at = NULL WHERE var_id = $1`, v)
		}, []string{"FIRST=one"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fx := newRunEnvFix(t)
			id := fx.queuedRun(t, "issue")
			v := c.setup(fx)
			var once sync.Once
			fx.svc.claimHooks = &claimTestHooks{afterAssembly: func(context.Context, store.Run, *ClaimPayload, error) {
				once.Do(func() { c.mutate(fx, v) })
			}}
			if p := fx.claim(t); p != nil {
				t.Fatalf("a claim over a changed variable set was delivered: %v", envNames(p))
			}
			r := mustRun(t, fx.env, id)
			if r.Status != "queued" || len(fx.receipts(t, id)) != 0 {
				t.Fatalf("after the race: status=%s receipts=%v, want queued and none", r.Status, fx.receipts(t, id))
			}
			p := fx.claim(t)
			if p == nil {
				t.Fatal("the requeued claim delivered nothing")
			}
			if got := envNames(p); !eq(got, c.want) {
				t.Fatalf("run_env = %v, want %v", got, c.want)
			}
			if len(fx.receipts(t, id)) != len(c.want) {
				t.Fatalf("receipts = %v for %v", fx.receipts(t, id), c.want)
			}
		})
	}
}

// TestRunEnvClaimOldWorkerLiveDB: a worker without run_env_v1 never claims a run that has variables
// (the SQL gate), and a first variable created after the claim passed the gate requeues the claim at
// the finish with nothing delivered.
func TestRunEnvClaimOldWorkerLiveDB(t *testing.T) {
	t.Run("gate", func(t *testing.T) {
		fx := newRunEnvFix(t)
		fx.setRunEnvCap(false)
		id := fx.queuedRun(t, "issue")
		fx.putEnv(t, "FIRST", "one", false)
		if p := fx.claim(t); p != nil {
			t.Fatal("an old worker claimed a gated run")
		}
		if r := mustRun(t, fx.env, id); r.Status != "queued" || r.ClaimGeneration != 0 {
			t.Fatalf("run = status %s generation %d, want untouched queued", r.Status, r.ClaimGeneration)
		}
		fx.setRunEnvCap(true)
		if p := fx.claim(t); p == nil || !eq(envNames(p), []string{"FIRST=one"}) {
			t.Fatalf("the capable worker's claim = %+v", p)
		}
	})
	t.Run("first variable after the gate", func(t *testing.T) {
		fx := newRunEnvFix(t)
		fx.setRunEnvCap(false)
		id := fx.queuedRun(t, "issue")
		var once sync.Once
		fx.svc.claimHooks = &claimTestHooks{afterAssembly: func(context.Context, store.Run, *ClaimPayload, error) {
			once.Do(func() { fx.putEnv(t, "FIRST", "one", false) })
		}}
		if p := fx.claim(t); p != nil {
			t.Fatal("a claim over a variable created mid-claim was delivered to an old worker")
		}
		if r := mustRun(t, fx.env, id); r.Status != "queued" || len(fx.receipts(t, id)) != 0 {
			t.Fatalf("run = %s receipts %v", r.Status, fx.receipts(t, id))
		}
	})
}

// TestRunEnvClaimLockedVaultRequeuesLiveDB: a variable that needs the owner's unlocked vault requeues
// the claim (transient), never parks or fails it, and writes no receipt.
func TestRunEnvClaimLockedVaultRequeuesLiveDB(t *testing.T) {
	fx := newRunEnvFix(t)
	id := fx.queuedRun(t, "issue")
	if _, err := fx.env.q.InsertRunEnvVar(fx.env.ctx, store.InsertRunEnvVarParams{
		VarID: uuid.New(), UserID: fx.userID, RepoID: pgtype.UUID{Bytes: fx.repoID, Valid: true}, Name: "LOCKED_ONE",
		Description: "d", Ciphertext: []byte("not-a-real-ciphertext"), SealedWith: "dek", AadVersion: runenv.AADVersion2, ValueBytes: 5, Rev: 1,
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if p := fx.claim(t); p != nil {
		t.Fatal("a claim over a locked-vault variable was delivered")
	}
	if r := mustRun(t, fx.env, id); r.Status != "queued" || len(fx.receipts(t, id)) != 0 {
		t.Fatalf("run = %s receipts %v, want queued and none", r.Status, fx.receipts(t, id))
	}
}

// TestRunEnvClaimMissingReceiptedVersionParksLiveDB: a receipted secret version whose row is gone, or a
// live row whose ciphertext was swapped for another version's (the revision is bound into the sealing
// context), parks the run on run_env_history_unavailable. No promoter or sweeper moves it, and the
// owner's resume re-attempts the claim, which parks again while the version is missing.
func TestRunEnvClaimMissingReceiptedVersionParksLiveDB(t *testing.T) {
	for _, mode := range []string{"deleted row", "ciphertext swap"} {
		t.Run(mode, func(t *testing.T) {
			fx := newRunEnvFix(t)
			id := fx.queuedRun(t, "issue")
			v := fx.putEnv(t, "SECRET_ONE", "sekret-1", true)
			if p := fx.claim(t); p == nil {
				t.Fatal("setup claim delivered nothing")
			}
			fx.rotateEnv(t, v, "SECRET_ONE", "sekret-2", true)
			if mode == "deleted row" {
				fx.env.exec(`DELETE FROM run_env_vars WHERE var_id = $1 AND rev = 1`, v)
			} else {
				fx.env.exec(`UPDATE run_env_vars SET ciphertext = (SELECT ciphertext FROM run_env_vars WHERE var_id = $1 AND rev = 1), sealed_with = (SELECT sealed_with FROM run_env_vars WHERE var_id = $1 AND rev = 1) WHERE var_id = $1 AND rev = 2`, v)
			}
			fx.requeue(t, id)
			if p := fx.claim(t); p != nil {
				t.Fatalf("a claim with unavailable history was delivered: %v", envNames(p))
			}
			held := func() {
				t.Helper()
				r := mustRun(t, fx.env, id)
				if r.Status != "paused" || r.HoldReason.String != holdRunEnvHistoryUnavailable || r.ClaimReleasedAt.Valid == false {
					t.Fatalf("run = status %s hold %q released=%v, want paused/%s", r.Status, r.HoldReason.String, r.ClaimReleasedAt.Valid, holdRunEnvHistoryUnavailable)
				}
			}
			held()
			fx.svc.RequestCredentialDisabledPromotion(fx.userID)
			if _, err := fx.svc.Sweep(fx.env.ctx); err != nil {
				t.Fatalf("Sweep: %v", err)
			}
			held()
			if _, err := fx.env.q.ResumePausedRun(fx.env.ctx, store.ResumePausedRunParams{
				ID: id, UserID: fx.userID, GlobalTimeoutSeconds: int32(testParams().RunTimeout.Seconds()),
			}); err != nil {
				t.Fatalf("ResumePausedRun: %v", err)
			}
			if p := fx.claim(t); p != nil {
				t.Fatal("the resumed claim was delivered while the version is still unavailable")
			}
			held()
		})
	}
}

// TestRunEnvClaimDoesNotDeadlockWithUserWritersLiveDB: the finish takes the owner row FOR SHARE NOWAIT
// after the run lock. A writer holding that row FOR UPDATE (a variable write or vault change) makes the
// finish roll back with 55P03 and retry; releasing it inside the retry budget lets the claim deliver.
func TestRunEnvClaimDoesNotDeadlockWithUserWritersLiveDB(t *testing.T) {
	fx := newRunEnvFix(t)
	fx.queuedRun(t, "issue")
	fx.putEnv(t, "FIRST", "one", false)
	finishRunClaimRetryDelayOld := finishRunClaimRetryDelay
	finishRunClaimRetryDelay = 100 * time.Millisecond
	t.Cleanup(func() { finishRunClaimRetryDelay = finishRunClaimRetryDelayOld })

	tx, err := fx.env.pool.Begin(fx.env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(fx.env.ctx) })
	if _, err := tx.Exec(fx.env.ctx, `SELECT 1 FROM users WHERE id = $1 FOR UPDATE`, fx.userID); err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	time.AfterFunc(150*time.Millisecond, func() { _ = tx.Commit(fx.env.ctx); close(released) })
	p := fx.claim(t)
	<-released
	if p == nil || !eq(envNames(p), []string{"FIRST=one"}) {
		t.Fatalf("claim = %+v, want the variable delivered after the writer released", p)
	}
}

// TestRunEnvRepoDeleteCascadesLiveDB: deleting the repo removes its runs, their receipts and the
// variable rows; the maintenance read for the deleted run is then "not owned" (the handler's 404).
func TestRunEnvRepoDeleteCascadesLiveDB(t *testing.T) {
	fx := newRunEnvFix(t)
	id := fx.queuedRun(t, "issue")
	fx.putEnv(t, "SECRET_ONE", "sekret-1", true)
	if p := fx.claim(t); p == nil {
		t.Fatal("setup claim delivered nothing")
	}
	if _, err := fx.svc.RunEnvRedact(fx.env.ctx, fx.worker(t), id); err != nil {
		t.Fatalf("RunEnvRedact before delete: %v", err)
	}
	fx.env.exec(`DELETE FROM repos WHERE id = $1`, fx.repoID)
	for _, q := range []string{
		`SELECT count(*) FROM runs WHERE id = $1`,
		`SELECT count(*) FROM run_env_receipts WHERE run_id = $1`,
	} {
		var n int
		if err := fx.env.pool.QueryRow(fx.env.ctx, q, id).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s = %d, %v after the repo delete, want 0", q, n, err)
		}
	}
	var n int
	if err := fx.env.pool.QueryRow(fx.env.ctx, `SELECT count(*) FROM run_env_vars WHERE repo_id = $1`, fx.repoID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("variable rows after the repo delete = %d, %v", n, err)
	}
	if _, err := fx.svc.RunEnvRedact(fx.env.ctx, fx.worker(t), id); err != ErrRunNotOwned {
		t.Fatalf("RunEnvRedact after delete = %v, want ErrRunNotOwned", err)
	}
}

// TestRunEnvRedactMaintenanceLiveDB: ownership, shape, and the two 409 causes of the maintenance read.
func TestRunEnvRedactMaintenanceLiveDB(t *testing.T) {
	fx := newRunEnvFix(t)
	id := fx.queuedRun(t, "issue")
	w := fx.worker(t)

	got, err := fx.svc.RunEnvRedact(fx.env.ctx, w, id)
	if err != nil || got.Protected || got.Versions != nil {
		t.Fatalf("no receipts: %+v, %v, want protected=false", got, err)
	}
	plain := fx.putEnv(t, "PLAIN_ONE", "alpha", false)
	secret := fx.putEnv(t, "SECRET_ONE", "sekret-1", true)
	if p := fx.claim(t); p == nil {
		t.Fatal("setup claim delivered nothing")
	}
	got, err = fx.svc.RunEnvRedact(fx.env.ctx, w, id)
	if err != nil || !got.Protected || len(got.Versions) != 1 || got.Versions[0].Name != "SECRET_ONE" || got.Versions[0].Value != "sekret-1" || got.Versions[0].VarID != secret {
		t.Fatalf("live secret: %+v, %v (a plain receipt must not appear)", got, err)
	}
	_ = plain
	// A retired version is still answered.
	fx.rotateEnv(t, secret, "SECRET_ONE", "sekret-2", true)
	got, err = fx.svc.RunEnvRedact(fx.env.ctx, w, id)
	if err != nil || len(got.Versions) != 1 || got.Versions[0].Value != "sekret-1" {
		t.Fatalf("retired secret: %+v, %v", got, err)
	}
	// Another worker, a run that does not exist, and another user's worker are all "not owned".
	other := fx.env.seedUser(t)
	if _, err := fx.svc.RunEnvRedact(fx.env.ctx, store.Worker{ID: uuid.New(), UserID: fx.userID}, id); err != ErrRunNotOwned {
		t.Fatalf("another worker = %v", err)
	}
	if _, err := fx.svc.RunEnvRedact(fx.env.ctx, w, uuid.New()); err != ErrRunNotOwned {
		t.Fatalf("unknown run = %v", err)
	}
	if _, err := fx.svc.RunEnvRedact(fx.env.ctx, store.Worker{ID: w.ID, UserID: other}, id); err != ErrRunNotOwned {
		t.Fatalf("another user's worker = %v", err)
	}
	// A missing receipted version.
	fx.env.exec(`DELETE FROM run_env_vars WHERE var_id = $1 AND rev = 1`, secret)
	if _, err := fx.svc.RunEnvRedact(fx.env.ctx, w, id); err != ErrRunEnvHistoryUnavailable {
		t.Fatalf("missing version = %v", err)
	}
	// A locked vault.
	locked := fx.putEnv(t, "LOCKED_ONE", "x", true)
	fx.env.exec(`INSERT INTO run_env_receipts (run_id, user_id, var_id, rev, scope, name, secret, first_claim_generation, last_claim_generation) VALUES ($1,$2,$3,1,'repo','LOCKED_ONE',true,1,1)`, id, fx.userID, locked)
	fx.env.exec(`DELETE FROM run_env_receipts WHERE run_id = $1 AND name = 'SECRET_ONE'`, id)
	fx.env.exec(`UPDATE run_env_vars SET sealed_with = 'dek' WHERE var_id = $1`, locked)
	if _, err := fx.svc.RunEnvRedact(fx.env.ctx, w, id); err != ErrRunEnvVaultLocked {
		t.Fatalf("locked vault = %v", err)
	}
}
