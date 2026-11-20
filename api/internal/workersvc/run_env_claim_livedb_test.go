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
	// A secret version a run may hold is retired; a plain version is simply replaced (the table's
	// retained_secret check refuses to retire a plain row).
	stmt := `UPDATE run_env_vars SET retired_at = now() WHERE var_id = $1 AND retired_at IS NULL RETURNING rev`
	if !secret {
		stmt = `DELETE FROM run_env_vars WHERE var_id = $1 AND retired_at IS NULL RETURNING rev`
	}
	if err := fx.env.pool.QueryRow(fx.env.ctx, stmt, varID).Scan(&rev); err != nil {
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
				fx.env.exec(`DELETE FROM run_env_vars WHERE var_id = $1`, v)
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

// TestRunEnvClaimDoesNotWaitForUserWritersLiveDB: the finish takes the owner row FOR SHARE NOWAIT after
// the run lock. A writer holding that row FOR UPDATE (a variable write or vault change) makes the finish
// roll back with 55P03 and retry; releasing the writer inside the retry budget lets the claim deliver, and
// a writer that outlasts the budget gets the claim back as 55P03 with nothing mutated, long before the
// writer finishes (a waiting lock would have blocked for the writer's whole hold).
func TestRunEnvClaimDoesNotWaitForUserWritersLiveDB(t *testing.T) {
	hold := func(t *testing.T, fx *cdFix, d time.Duration) chan struct{} {
		t.Helper()
		tx, err := fx.env.pool.Begin(fx.env.ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = tx.Rollback(fx.env.ctx) })
		if _, err := tx.Exec(fx.env.ctx, `SELECT 1 FROM users WHERE id = $1 FOR UPDATE`, fx.userID); err != nil {
			t.Fatal(err)
		}
		released := make(chan struct{})
		time.AfterFunc(d, func() { _ = tx.Commit(fx.env.ctx); close(released) })
		return released
	}
	setDelay := func(t *testing.T) {
		old := finishRunClaimRetryDelay
		finishRunClaimRetryDelay = 100 * time.Millisecond
		t.Cleanup(func() { finishRunClaimRetryDelay = old })
	}
	// The writer starts after assembly: ClaimRun itself reads the owner row and would wait on it, which is
	// not what is under test.
	holdAfterAssembly := func(fx *cdFix, t *testing.T, d time.Duration) chan chan struct{} {
		out := make(chan chan struct{}, 1)
		var once sync.Once
		fx.svc.claimHooks = &claimTestHooks{afterAssembly: func(context.Context, store.Run, *ClaimPayload, error) {
			once.Do(func() { out <- hold(t, fx, d) })
		}}
		return out
	}
	t.Run("released inside the retry budget", func(t *testing.T) {
		fx := newRunEnvFix(t)
		fx.queuedRun(t, "issue")
		fx.putEnv(t, "FIRST", "one", false)
		setDelay(t)
		started := holdAfterAssembly(fx, t, 150*time.Millisecond)
		p := fx.claim(t)
		released := <-started
		<-released
		if p == nil || !eq(envNames(p), []string{"FIRST=one"}) {
			t.Fatalf("claim = %+v, want the variable delivered after the writer released", p)
		}
	})
	t.Run("outlasting the retry budget", func(t *testing.T) {
		fx := newRunEnvFix(t)
		id := fx.queuedRun(t, "issue")
		fx.putEnv(t, "FIRST", "one", false)
		setDelay(t)
		const writerHold = 3 * time.Second
		started := holdAfterAssembly(fx, t, writerHold)
		start := time.Now()
		p, err := fx.svc.Claim(fx.env.ctx, fx.worker(t), nil)
		elapsed := time.Since(start)
		released := <-started
		if p != nil || !isLockNotAvailable(err) {
			t.Fatalf("Claim = (%v, %v), want (nil, 55P03)", p != nil, err)
		}
		if elapsed >= writerHold-500*time.Millisecond {
			t.Fatalf("the claim took %s against a %s writer: the finish waited for the owner row instead of failing fast", elapsed, writerHold)
		}
		if r := mustRun(t, fx.env, id); r.Status != "claimed" || len(fx.receipts(t, id)) != 0 {
			t.Fatalf("run = %s, receipts %v, want the claim left as it was with no receipt", r.Status, fx.receipts(t, id))
		}
		<-released
	})
}

// releaseCustody settles every custody hold the run's claim opened (it ends the hold without the
// release evidence the production path writes: the fixture only needs the live pointers gone).
func (fx *cdFix) releaseCustody(t *testing.T, runID uuid.UUID) {
	t.Helper()
	fx.bypass(t, `UPDATE recovery_custody_holds SET state = 'released', live_worker_id = NULL, live_run_id = NULL WHERE run_id = $1`, runID)
}

// TestRunEnvRepoDeleteLiveDB (O8): deleting a repo after a claim either removes the repo's runs, their
// receipts and the variable rows together, or is refused with every row intact. The claim opened a
// custody hold, whose live pointer to the run restricts the delete; once the hold is released the
// delete cascades. The maintenance read for a deleted run is then "not owned" (the handler's 404).
func TestRunEnvRepoDeleteLiveDB(t *testing.T) {
	counts := func(fx *cdFix, runID uuid.UUID) (runs, receipts, vars int) {
		t.Helper()
		for q, dst := range map[string]*int{
			`SELECT count(*) FROM runs WHERE id = $1`:                 &runs,
			`SELECT count(*) FROM run_env_receipts WHERE run_id = $1`: &receipts,
			`SELECT count(*) FROM run_env_vars WHERE repo_id = $1`:    &vars,
		} {
			arg := any(runID)
			if strings.Contains(q, "run_env_vars") {
				arg = fx.repoID
			}
			if err := fx.env.pool.QueryRow(fx.env.ctx, q, arg).Scan(dst); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	fx := newRunEnvFix(t)
	id := fx.queuedRun(t, "issue")
	fx.putEnv(t, "SECRET_ONE", "sekret-1", true)
	if p := fx.claim(t); p == nil {
		t.Fatal("setup claim delivered nothing")
	}
	if _, err := fx.svc.RunEnvRedact(fx.env.ctx, fx.worker(t), id); err != nil {
		t.Fatalf("RunEnvRedact before delete: %v", err)
	}

	// Refused: the open custody hold restricts the run, and nothing was removed.
	if _, err := fx.env.pool.Exec(fx.env.ctx, `DELETE FROM repos WHERE id = $1`, fx.repoID); err == nil {
		t.Fatal("deleting a repo whose run holds custody succeeded")
	}
	if r, rc, v := counts(fx, id); r != 1 || rc != 1 || v != 1 {
		t.Fatalf("after the refused delete: runs=%d receipts=%d variable rows=%d, want 1 1 1", r, rc, v)
	}
	if _, err := fx.svc.RunEnvRedact(fx.env.ctx, fx.worker(t), id); err != nil {
		t.Fatalf("RunEnvRedact after the refused delete: %v", err)
	}

	// Cascades: with the custody released, the runs, receipts and rows go together.
	fx.releaseCustody(t, id)
	fx.env.exec(`DELETE FROM repos WHERE id = $1`, fx.repoID)
	if r, rc, v := counts(fx, id); r != 0 || rc != 0 || v != 0 {
		t.Fatalf("after the repo delete: runs=%d receipts=%d variable rows=%d, want 0 0 0", r, rc, v)
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

// TestRunEnvClaimInterleavedWithCodexRefreshCommitLiveDB: a Codex-bound run with a variable is claimed
// while a refresh commit holds the account row and a user-row writer (a variable write or vault change,
// FOR UPDATE on users) queues behind the finish's owner share lock. The finish takes the account row
// FOR SHARE NOWAIT after the owner row, so it rolls back with 55P03 instead of waiting, which frees the
// writer, which lets the commit finish; the finish's retry then delivers. There is no deadlock and
// no stuck claim.
func TestRunEnvClaimInterleavedWithCodexRefreshCommitLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fx := newCodexClaimFix(t, env, false)
	retryDelay := finishRunClaimRetryDelay
	finishRunClaimRetryDelay = 200 * time.Millisecond
	t.Cleanup(func() { finishRunClaimRetryDelay = retryDelay })

	varID := uuid.New()
	material, err := env.vlt.Seal(env.ctx, fx.userID, runenv.AADKindV2(runenv.ScopeRepo, &fx.repoID, false, "CODEX_PLAIN", varID, 1), []byte("alpha"), vault.SealForUse)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.q.InsertRunEnvVar(env.ctx, store.InsertRunEnvVarParams{
		VarID: varID, UserID: fx.userID, RepoID: pgtype.UUID{Bytes: fx.repoID, Valid: true}, Name: "CODEX_PLAIN", Secret: false,
		Description: "d", Ciphertext: material.Ciphertext, SealedWith: material.SealedWith, AadVersion: runenv.AADVersion2, ValueBytes: 5, Rev: 1,
	}); err != nil {
		t.Fatal(err)
	}
	env.exec(`UPDATE workers SET protocol_capabilities = protocol_capabilities || ARRAY[$2]::text[] WHERE id = $1`, fx.workerA, capability.RunEnvV1)
	claimant := fx.claimant(t, true)
	claimant.ProtocolCapabilities = append(claimant.ProtocolCapabilities, capability.RunEnvV1)

	// An in-progress refresh lease the commit below completes.
	op := uuid.New()
	var generation int64
	if err := env.pool.QueryRow(env.ctx, `UPDATE codex_provider_account SET coord_state = 'in_progress', coord_operation_id = $2,
		lease_deadline = now() + interval '1 hour' WHERE id = $1 RETURNING generation`, fx.accountID, op).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	sealed, err := env.box.Seal([]byte(`{"access_token":"a-rotated","refresh_token":"r-rotated"}`))
	if err != nil {
		t.Fatal(err)
	}

	writerDone := make(chan error, 1)
	var once sync.Once
	fx.svc.claimHooks = &claimTestHooks{afterAssembly: func(_ context.Context, _ store.Run, p *ClaimPayload, aerr error) {
		once.Do(func() {
			if aerr != nil || p == nil || len(p.Secrets.RunEnv) != 1 {
				t.Errorf("assembly = (%v, %v), want one variable", p != nil, aerr)
				return
			}
			refresh, err := env.pool.Begin(env.ctx)
			if err != nil {
				t.Error(err)
				return
			}
			if _, err := store.New(refresh).CommitCodexRefresh(env.ctx, store.CommitCodexRefreshParams{
				Sealed: sealed, SealedWith: store.SealedWithMaster, Op: op, ID: fx.accountID, UserID: fx.userID, FromGeneration: generation,
			}); err != nil {
				t.Errorf("refresh commit: %v", err)
				_ = refresh.Rollback(env.ctx)
				return
			}
			// The refresh transaction goes on to take the owner row FOR UPDATE, as a variable write
			// would, and commits only then. It must wait for the finish's owner share lock to clear.
			go func() {
				_, err := refresh.Exec(env.ctx, `SELECT 1 FROM users WHERE id = $1 FOR UPDATE`, fx.userID)
				if err == nil {
					err = refresh.Commit(env.ctx)
				} else {
					_ = refresh.Rollback(env.ctx)
				}
				writerDone <- err
			}()
		})
	}}
	type result struct {
		p   *ClaimPayload
		err error
	}
	done := make(chan result, 1)
	go func() {
		p, err := fx.svc.Claim(env.ctx, claimant, nil)
		done <- result{p, err}
	}()
	select {
	case r := <-done:
		if r.err != nil || r.p == nil || len(r.p.Secrets.RunEnv) != 1 {
			t.Fatalf("Claim = (%v, %v), want the claim delivered after the refresh commit", r.p != nil, r.err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the claim did not finish: the finish and the refresh commit are stuck")
	}
	select {
	case err := <-writerDone:
		if err != nil {
			t.Fatalf("the refresh transaction failed (a deadlock victim?): %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the refresh transaction never finished")
	}
	var committed int64
	if err := env.pool.QueryRow(env.ctx, `SELECT generation FROM codex_provider_account WHERE id = $1`, fx.accountID).Scan(&committed); err != nil || committed != generation+1 {
		t.Fatalf("account generation = %d, %v, want %d", committed, err, generation+1)
	}
}

// TestRunEnvWorkerMissingForRunLiveDB: the health query behind "no online worker supports repo
// variables" is true only for a lane run that has an enabled variable (or a secret receipt) while no
// online worker of the owner reports run_env_v1.
func TestRunEnvWorkerMissingForRunLiveDB(t *testing.T) {
	fx := newRunEnvFix(t)
	fx.env.exec(`UPDATE workers SET status = 'online', last_heartbeat_at = now() WHERE id = $1`, fx.workerID)
	missing := func(id uuid.UUID) bool {
		t.Helper()
		got, err := fx.env.q.RunEnvWorkerMissingForRun(fx.env.ctx, store.RunEnvWorkerMissingForRunParams{RunID: id, UserID: fx.userID})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	run := fx.queuedRun(t, "issue")
	if missing(run) {
		t.Fatal("a run with no variables reports a missing worker")
	}
	v := fx.putEnv(t, "FIRST", "one", false)
	if missing(run) {
		t.Fatal("a capable online worker exists but the query reports missing")
	}
	fx.setRunEnvCap(false)
	if !missing(run) {
		t.Fatal("variables and no capable online worker, but the query reports none missing")
	}
	review := fx.queuedRun(t, "issue")
	fx.env.exec(`UPDATE runs SET review_target_run_id = $2 WHERE id = $1`, review, run)
	if missing(review) {
		t.Fatal("a review run is outside the lane but reports a missing worker")
	}
	fx.env.exec(`UPDATE run_env_vars SET disabled_at = now() WHERE var_id = $1`, v)
	if missing(run) {
		t.Fatal("a disabled variable alone reports a missing worker")
	}
	// A secret receipt keeps the run gated even with every variable disabled or gone.
	fx.env.exec(`DELETE FROM run_env_vars WHERE var_id = $1`, v)
	fx.env.exec(`INSERT INTO run_env_receipts (run_id, user_id, var_id, rev, scope, name, secret, first_claim_generation, last_claim_generation) VALUES ($1,$2,$3,1,'repo','FIRST',true,1,1)`, run, fx.userID, uuid.New())
	if !missing(run) {
		t.Fatal("a run holding a secret receipt reports no missing worker")
	}
	fx.setRunEnvCap(true)
	if missing(run) {
		t.Fatal("a capable online worker exists but the query reports missing")
	}
}
