package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/runenv"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

// run_env_claim_livedb_test.go is the live-DB half of PRD #4416 M2's claim contract: the
// snapshot check under the owner's users-row share lock, the receipts, the park for history that
// cannot be masked, and the races between assembly and finish. Skipped unless
// UZI_TEST_DATABASE_URL is set; run via ./e2e/run-store-it.sh.

// reFix is a cdFix whose worker advertises run_env_v1 (or not) and no recovery archive, so a
// claim opens no custody hold and a run can be re-queued and claimed again.
type reFix struct{ *cdFix }

func newREFix(t *testing.T, capable bool) *reFix {
	t.Helper()
	fx := newCDFix(t)
	caps := []string{capability.CredentialSwitchV1}
	if capable {
		caps = append(caps, capability.RunEnvV1)
	}
	fx.env.exec(`UPDATE workers SET protocol_capabilities = $2 WHERE id = $1`, fx.workerID, caps)
	return &reFix{fx}
}

// Fixture values are assembled at runtime; none is a credential.
func envSecretValue(tag string) string { return "sec" + "ret-value-" + tag }

func (fx *reFix) seal(t *testing.T, varID uuid.UUID, rev int32, name string, secret bool, value string) (store.InsertRunEnvVarParams, vault.SealedMaterial) {
	t.Helper()
	repo := fx.repoID
	kind, err := runenv.AADFor(runenv.AADVersion2, fx.userID, runenv.ScopeRepo, &repo, secret, name, varID, rev)
	if err != nil {
		t.Fatal(err)
	}
	m, err := fx.env.vlt.Seal(fx.env.ctx, fx.userID, kind, []byte(value), vault.SealForUse)
	if err != nil {
		t.Fatal(err)
	}
	//nolint:gosec // G115: fixture values are a few bytes.
	valueBytes := int32(len(value))
	return store.InsertRunEnvVarParams{
		VarID: varID, UserID: fx.userID, RepoID: pgtype.UUID{Bytes: repo, Valid: true}, Name: name, Secret: secret,
		Ciphertext: m.Ciphertext, SealedWith: m.SealedWith, ValueBytes: valueBytes, Rev: rev, AadVersion: runenv.AADVersion2,
	}, m
}

// put creates a new variable (rev 1) and returns its var id.
func (fx *reFix) put(t *testing.T, name string, secret bool, value string) uuid.UUID {
	t.Helper()
	varID := uuid.New()
	p, _ := fx.seal(t, varID, 1, name, secret, value)
	if _, err := fx.env.q.InsertRunEnvVar(fx.env.ctx, p); err != nil {
		t.Fatalf("insert %s: %v", name, err)
	}
	return varID
}

// rotate writes the next version of a live variable: the previous secret version is retired
// (it may be receipted), a previous plain version is dropped, like the handler.
func (fx *reFix) rotate(t *testing.T, name, value string) (varID uuid.UUID, rev int32) {
	t.Helper()
	var secret bool
	if err := fx.env.pool.QueryRow(fx.env.ctx, `SELECT var_id, rev, secret FROM run_env_vars
	    WHERE user_id=$1 AND repo_id=$2 AND name=$3 AND retired_at IS NULL AND pending_delete_at IS NULL`,
		fx.userID, fx.repoID, name).Scan(&varID, &rev, &secret); err != nil {
		t.Fatal(err)
	}
	if secret {
		fx.env.exec(`UPDATE run_env_vars SET retired_at = now() WHERE var_id=$1 AND rev=$2`, varID, rev)
	} else {
		fx.env.exec(`DELETE FROM run_env_vars WHERE var_id=$1 AND rev=$2`, varID, rev)
	}
	p, _ := fx.seal(t, varID, rev+1, name, secret, value)
	if _, err := fx.env.q.InsertRunEnvVar(fx.env.ctx, p); err != nil {
		t.Fatal(err)
	}
	return varID, rev + 1
}

func (fx *reFix) requeue(runID uuid.UUID) {
	fx.env.exec(`UPDATE runs SET status='queued', status_since=now(), claim_released_at=NULL WHERE id=$1`, runID)
}

func (fx *reFix) claim(t *testing.T) *ClaimPayload {
	t.Helper()
	p, err := fx.svc.Claim(fx.env.ctx, fx.worker(t), nil)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	return p
}

type receiptRow struct {
	varID       uuid.UUID
	rev         int32
	scope, name string
	secret      bool
	first, last int64
}

func (fx *reFix) receipts(t *testing.T, runID uuid.UUID) []receiptRow {
	t.Helper()
	rows, err := fx.env.pool.Query(fx.env.ctx, `SELECT var_id, rev, scope, name, secret, first_claim_generation, last_claim_generation
	    FROM run_env_receipts WHERE run_id=$1 ORDER BY name, rev`, runID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []receiptRow
	for rows.Next() {
		var r receiptRow
		if err := rows.Scan(&r.varID, &r.rev, &r.scope, &r.name, &r.secret, &r.first, &r.last); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// envShape, redactShape, payloadShape and redactRespShape describe run-env claim data for failure
// messages by name, revision, scope, secret flag and value length only, never the value.
func envShape(vs []ClaimRunEnvVar) string {
	parts := make([]string, 0, len(vs))
	for _, v := range vs {
		parts = append(parts, fmt.Sprintf("%s#%d(scope=%s secret=%v valueLen=%d)", v.Name, v.Rev, v.Scope, v.Secret, len(v.Value)))
	}
	return fmt.Sprintf("%d vars [%s]", len(vs), strings.Join(parts, " "))
}

func redactShape(vs []ClaimRunEnvRedact) string {
	parts := make([]string, 0, len(vs))
	for _, v := range vs {
		parts = append(parts, fmt.Sprintf("%s#%d(scope=%s valueLen=%d)", v.Name, v.Rev, v.Scope, len(v.Value)))
	}
	return fmt.Sprintf("%d redact [%s]", len(vs), strings.Join(parts, " "))
}

func payloadShape(p *ClaimPayload) string {
	if p == nil {
		return "no payload"
	}
	return envShape(p.Secrets.RunEnv) + ", " + redactShape(p.Secrets.RunEnvRedact)
}

func redactRespShape(r RunEnvRedactResponse) string {
	return fmt.Sprintf("protected=%v %s", r.Protected, redactShape(r.Versions))
}

func runEnvByName(p *ClaimPayload) map[string]ClaimRunEnvVar {
	out := map[string]ClaimRunEnvVar{}
	for _, v := range p.Secrets.RunEnv {
		out[v.Name] = v
	}
	return out
}

func TestRunEnvClaimDeliversAndReceiptsLiveDB(t *testing.T) {
	fx := newREFix(t, true)
	runID := fx.queuedRun(t, "issue")
	plainID := fx.put(t, "API_BASE_URL", false, "https://sandbox.example.test")
	secretID := fx.put(t, "API_KEY", true, envSecretValue("one"))

	p := fx.claim(t)
	if p == nil {
		t.Fatal("claim idle, want a payload")
	}
	got := runEnvByName(p)
	if len(p.Secrets.RunEnv) != 2 || got["API_BASE_URL"].Value != "https://sandbox.example.test" || got["API_BASE_URL"].Secret ||
		got["API_KEY"].Value != envSecretValue("one") || !got["API_KEY"].Secret {
		t.Fatalf("run_env = %s", envShape(p.Secrets.RunEnv))
	}
	if got["API_KEY"].VarID != secretID.String() || got["API_KEY"].Rev != 1 || got["API_KEY"].Scope != runenv.ScopeRepo {
		t.Fatalf("identity on the wire = %s", envShape([]ClaimRunEnvVar{got["API_KEY"]}))
	}
	if len(p.Secrets.RunEnvRedact) != 0 {
		t.Fatalf("redact = %s, want none", redactShape(p.Secrets.RunEnvRedact))
	}
	// The snapshot is unexported metadata: it never reaches the wire.
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "effective") || strings.Contains(string(raw), "capable") {
		t.Fatalf("snapshot leaked into the claim JSON")
	}
	rs := fx.receipts(t, runID)
	if len(rs) != 2 {
		t.Fatalf("receipts = %+v, want one per delivered version (plain and secret)", rs)
	}
	for _, r := range rs {
		if r.first != 1 || r.last != 1 || r.rev != 1 {
			t.Fatalf("receipt = %+v, want generation 1 rev 1", r)
		}
		if r.name == "API_BASE_URL" && (r.secret || r.varID != plainID) || r.name == "API_KEY" && (!r.secret || r.varID != secretID) {
			t.Fatalf("receipt = %+v", r)
		}
	}
}

// A rotation after the claim reaches only the next claim; every earlier secret version rides
// in run_env_redact exactly once however many claims repeat; receipts accumulate.
func TestRunEnvClaimRotationAfterFinishReachesNextClaimLiveDB(t *testing.T) {
	fx := newREFix(t, true)
	runID := fx.queuedRun(t, "issue")
	fx.put(t, "API_KEY", true, envSecretValue("one"))
	if p := fx.claim(t); p == nil || runEnvByName(p)["API_KEY"].Value != envSecretValue("one") {
		t.Fatalf("first claim = %s", payloadShape(p))
	}
	fx.rotate(t, "API_KEY", envSecretValue("two"))
	fx.requeue(runID)
	p2 := fx.claim(t)
	if p2 == nil {
		t.Fatal("second claim idle")
	}
	if v := runEnvByName(p2)["API_KEY"]; v.Value != envSecretValue("two") || v.Rev != 2 {
		t.Fatalf("second claim run_env = %s, want rev 2", envShape(p2.Secrets.RunEnv))
	}
	if len(p2.Secrets.RunEnvRedact) != 1 || p2.Secrets.RunEnvRedact[0].Value != envSecretValue("one") || p2.Secrets.RunEnvRedact[0].Rev != 1 ||
		p2.Secrets.RunEnvRedact[0].Name != "API_KEY" || p2.Secrets.RunEnvRedact[0].Scope != runenv.ScopeRepo {
		t.Fatalf("second claim redact = %s, want the rev 1 value once", redactShape(p2.Secrets.RunEnvRedact))
	}
	fx.requeue(runID)
	p3 := fx.claim(t)
	if p3 == nil || len(p3.Secrets.RunEnvRedact) != 1 || len(p3.Secrets.RunEnv) != 1 {
		t.Fatalf("third claim = %s, want deduplicated redact", payloadShape(p3))
	}
	rs := fx.receipts(t, runID)
	if len(rs) != 2 || rs[0].rev != 1 || rs[0].first != 1 || rs[0].last != 1 || rs[1].rev != 2 || rs[1].first != 2 || rs[1].last != 3 {
		t.Fatalf("receipts = %+v, want rev1 (1,1) and rev2 (2,3)", rs)
	}
}

// A change between assembly and finish returns the run to queued with nothing delivered and no
// receipts: create, rotate, delete, disable and enable.
func TestRunEnvClaimRaceBetweenAssemblyAndFinishRequeuesLiveDB(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(t *testing.T, fx *reFix)
		mutate func(t *testing.T, fx *reFix)
	}{
		{"create", func(*testing.T, *reFix) {}, func(t *testing.T, fx *reFix) { fx.put(t, "LATE_VAR", false, "late") }},
		{"rotate", func(t *testing.T, fx *reFix) { fx.put(t, "ROT_KEY", true, envSecretValue("a")) },
			func(t *testing.T, fx *reFix) { fx.rotate(t, "ROT_KEY", envSecretValue("b")) }},
		{"delete", func(t *testing.T, fx *reFix) { fx.put(t, "DEL_VAR", false, "x") },
			func(_ *testing.T, fx *reFix) {
				fx.env.exec(`DELETE FROM run_env_vars WHERE user_id=$1 AND name='DEL_VAR'`, fx.userID)
			}},
		{"disable", func(t *testing.T, fx *reFix) { fx.put(t, "DIS_VAR", false, "x") },
			func(_ *testing.T, fx *reFix) {
				fx.env.exec(`UPDATE run_env_vars SET disabled_at=now() WHERE user_id=$1 AND name='DIS_VAR'`, fx.userID)
			}},
		{"enable", func(t *testing.T, fx *reFix) {
			fx.put(t, "EN_VAR", false, "x")
			fx.env.exec(`UPDATE run_env_vars SET disabled_at=now() WHERE user_id=$1 AND name='EN_VAR'`, fx.userID)
		}, func(_ *testing.T, fx *reFix) {
			fx.env.exec(`UPDATE run_env_vars SET disabled_at=NULL WHERE user_id=$1 AND name='EN_VAR'`, fx.userID)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fx := newREFix(t, true)
			runID := fx.queuedRun(t, "issue")
			c.setup(t, fx)
			fx.svc.claimHooks = &claimTestHooks{afterAssembly: func(_ context.Context, _ store.Run, p *ClaimPayload, err error) {
				if err != nil || p == nil {
					t.Fatalf("assembly = (%v, %v), want a payload", p != nil, err)
				}
				c.mutate(t, fx)
			}}
			if p := fx.claim(t); p != nil {
				t.Fatalf("claim delivered %s, want an idle (requeued) claim", envShape(p.Secrets.RunEnv))
			}
			if r := mustRun(t, fx.env, runID); r.Status != "queued" || r.FailOrigin.Valid {
				t.Fatalf("status=%s fail=%v, want queued", r.Status, r.FailOrigin)
			}
			if rs := fx.receipts(t, runID); len(rs) != 0 {
				t.Fatalf("receipts = %+v, want none", rs)
			}
			// The next claim, with the change in place, delivers it.
			fx.svc.claimHooks = nil
			fx.requeue(runID)
			if p := fx.claim(t); p == nil {
				t.Fatal("next claim idle")
			}
		})
	}
}

// A repo's first variable, added after admission for a worker without run_env_v1, never
// reaches that worker: caught at assembly when it lands before, at finish when it lands after.
func TestRunEnvClaimFirstVariableForIncapableWorkerRequeuesLiveDB(t *testing.T) {
	t.Run("before assembly", func(t *testing.T) {
		fx := newREFix(t, false)
		runID := fx.queuedRun(t, "issue")
		var assemblyErr error
		fx.svc.claimHooks = &claimTestHooks{
			beforeAssembly: func(context.Context, store.Run) { fx.put(t, "FIRST_VAR", false, "x") },
			afterAssembly:  func(_ context.Context, _ store.Run, _ *ClaimPayload, err error) { assemblyErr = err },
		}
		if p := fx.claim(t); p != nil {
			t.Fatalf("delivered %s", envShape(p.Secrets.RunEnv))
		}
		if !errors.Is(assemblyErr, errRunEnvCapabilityMissing) {
			t.Fatalf("assembly error = %v, want errRunEnvCapabilityMissing", assemblyErr)
		}
		if r := mustRun(t, fx.env, runID); r.Status != "queued" || r.FailOrigin.Valid {
			t.Fatalf("status=%s fail=%v, want queued", r.Status, r.FailOrigin)
		}
		if rs := fx.receipts(t, runID); len(rs) != 0 {
			t.Fatalf("receipts = %+v", rs)
		}
	})
	t.Run("after assembly", func(t *testing.T) {
		fx := newREFix(t, false)
		runID := fx.queuedRun(t, "issue")
		fx.svc.claimHooks = &claimTestHooks{afterAssembly: func(_ context.Context, _ store.Run, p *ClaimPayload, err error) {
			if err != nil || p == nil {
				t.Fatalf("assembly = (%v, %v)", p != nil, err)
			}
			fx.put(t, "FIRST_VAR", false, "x")
		}}
		if p := fx.claim(t); p != nil {
			t.Fatalf("delivered %s", envShape(p.Secrets.RunEnv))
		}
		if r := mustRun(t, fx.env, runID); r.Status != "queued" {
			t.Fatalf("status=%s, want queued", r.Status)
		}
		if rs := fx.receipts(t, runID); len(rs) != 0 {
			t.Fatalf("receipts = %+v", rs)
		}
	})
}

// A locked vault delays the run and never fails it.
func TestRunEnvClaimLockedVaultRequeuesLiveDB(t *testing.T) {
	fx := newREFix(t, true)
	runID := fx.queuedRun(t, "issue")
	varID := uuid.New()
	// A DEK-sealed row: no DEK is cached for the owner, so opening reports a locked vault.
	if _, err := fx.env.q.InsertRunEnvVar(fx.env.ctx, store.InsertRunEnvVarParams{
		VarID: varID, UserID: fx.userID, RepoID: pgtype.UUID{Bytes: fx.repoID, Valid: true}, Name: "LOCKED_VAR",
		Ciphertext: []byte("not-openable"), SealedWith: "dek", ValueBytes: 3, Rev: 1, AadVersion: runenv.AADVersion2,
	}); err != nil {
		t.Fatal(err)
	}
	// A dek row means a protected owner: the owner turns protected after the claim gate (which
	// idles a locked owner before any assembly) and before the values are opened.
	fx.svc.claimHooks = &claimTestHooks{beforeAssembly: func(context.Context, store.Run) {
		fx.env.exec(`UPDATE users SET vault_mode='protected' WHERE id=$1`, fx.userID)
	}}
	if p := fx.claim(t); p != nil {
		t.Fatalf("delivered %s", envShape(p.Secrets.RunEnv))
	}
	if r := mustRun(t, fx.env, runID); r.Status != "queued" || r.FailOrigin.Valid || r.HoldReason.Valid {
		t.Fatalf("status=%s fail=%v hold=%v, want a plain queued requeue", r.Status, r.FailOrigin, r.HoldReason)
	}
	if rs := fx.receipts(t, runID); len(rs) != 0 {
		t.Fatalf("receipts = %+v", rs)
	}
}

func (fx *reFix) assertParkedHistoryUnavailable(t *testing.T, runID uuid.UUID, p *ClaimPayload) {
	t.Helper()
	if p != nil {
		t.Fatalf("delivered %s, want a parked claim", envShape(p.Secrets.RunEnv))
	}
	r := mustRun(t, fx.env, runID)
	if r.Status != "paused" || !r.HoldReason.Valid || r.HoldReason.String != "run_env_history_unavailable" || !r.ClaimReleasedAt.Valid || r.FailOrigin.Valid {
		t.Fatalf("status=%s hold=%v released=%v fail=%v, want paused/run_env_history_unavailable", r.Status, r.HoldReason, r.ClaimReleasedAt, r.FailOrigin)
	}
}

// A receipted secret version that is no longer stored parks the run with a visible reason.
func TestRunEnvClaimMissingReceiptedVersionParksLiveDB(t *testing.T) {
	fx := newREFix(t, true)
	runID := fx.queuedRun(t, "issue")
	fx.put(t, "API_KEY", true, envSecretValue("one"))
	if p := fx.claim(t); p == nil {
		t.Fatal("first claim idle")
	}
	varID, _ := fx.rotate(t, "API_KEY", envSecretValue("two"))
	fx.env.exec(`DELETE FROM run_env_vars WHERE var_id=$1 AND rev=1`, varID)
	fx.requeue(runID)
	fx.assertParkedHistoryUnavailable(t, runID, fx.claim(t))
}

// A ciphertext copied from one version of a variable into another does not open under the
// other's context (the version-2 context binds var_id and rev): the claim parks.
func TestRunEnvClaimSwappedCiphertextParksLiveDB(t *testing.T) {
	for _, direction := range []string{"rev1 into rev2 (the injected row)", "rev2 into rev1 (the redact row)"} {
		t.Run(direction, func(t *testing.T) {
			fx := newREFix(t, true)
			runID := fx.queuedRun(t, "issue")
			fx.put(t, "API_KEY", true, envSecretValue("one"))
			if p := fx.claim(t); p == nil {
				t.Fatal("first claim idle")
			}
			varID, _ := fx.rotate(t, "API_KEY", envSecretValue("two"))
			src, dst := 1, 2
			if strings.HasPrefix(direction, "rev2") {
				src, dst = 2, 1
			}
			fx.env.exec(`UPDATE run_env_vars d SET ciphertext = s.ciphertext, sealed_with = s.sealed_with
			    FROM run_env_vars s WHERE d.var_id=$1 AND d.rev=$2 AND s.var_id=$1 AND s.rev=$3`, varID, dst, src)
			fx.requeue(runID)
			fx.assertParkedHistoryUnavailable(t, runID, fx.claim(t))
		})
	}
}

// Runs that never receive variables get none even when the repo has them: a task-review run,
// an isolated-lane run and the kinds outside the lead lane.
func TestRunEnvAttachSkipsNonDeliveredShapesLiveDB(t *testing.T) {
	fx := newREFix(t, true)
	fx.put(t, "API_BASE_URL", false, "https://sandbox.example.test")
	fx.put(t, "API_KEY", true, envSecretValue("one"))
	wkr := fx.worker(t)
	// The shape fields are varied on the row value; the reads key on the stored run's id.
	base := mustRun(t, fx.env, fx.queuedRun(t, "issue"))
	for _, c := range []struct {
		name string
		mod  func(*store.Run)
		want bool
	}{
		{"issue", func(*store.Run) {}, true},
		{"task review", func(r *store.Run) { r.Kind = "task"; r.ReviewTargetRunID = pgtype.UUID{Bytes: uuid.New(), Valid: true} }, false},
		{"task", func(r *store.Run) { r.Kind = "task" }, true},
		{"isolated", func(r *store.Run) { r.EgressProfileID = pgtype.UUID{Bytes: uuid.New(), Valid: true} }, false},
		{"chat", func(r *store.Run) { r.Kind = "chat" }, false},
		{"judge", func(r *store.Run) { r.Kind = "judge" }, false},
		{"job", func(r *store.Run) { r.Kind = "job" }, false},
		{"cross_check", func(r *store.Run) { r.Kind = "cross_check" }, false},
	} {
		run := base
		c.mod(&run)
		p := &ClaimPayload{}
		if err := fx.svc.attachRunEnv(fx.env.ctx, wkr, run, p); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if delivered := len(p.Secrets.RunEnv) == 2; delivered != c.want || (p.runEnv != nil) != c.want {
			t.Errorf("%s: delivered=%v snapshot=%v, want %v", c.name, len(p.Secrets.RunEnv), p.runEnv != nil, c.want)
		}
	}
}

// While the finish holds the owner's users row FOR KEY SHARE, a variable writer's FOR UPDATE fence
// waits; it proceeds once the claim commits, and the next claim sees its rotation.
func TestRunEnvFinishHoldsShareLockWriterProceedsAfterLiveDB(t *testing.T) {
	fx := newREFix(t, true)
	runID := fx.queuedRun(t, "issue")
	varID := fx.put(t, "API_KEY", true, envSecretValue("one"))
	next, _ := fx.seal(t, varID, 2, "API_KEY", true, envSecretValue("two"))

	writerDone := make(chan error, 1)
	var started sync.WaitGroup
	var blocked bool
	fx.svc.claimHooks = &claimTestHooks{afterRunEnvLock: func(ctx context.Context, _ store.Run) {
		started.Add(1)
		go func() {
			started.Done()
			tx, err := fx.env.pool.Begin(ctx)
			if err != nil {
				writerDone <- err
				return
			}
			defer func() { _ = tx.Rollback(ctx) }()
			// The variable writers' fence (FenceVaultWrite): the users row FOR UPDATE.
			if _, err := tx.Exec(ctx, `SELECT 1 FROM users WHERE id=$1 FOR UPDATE`, fx.userID); err != nil {
				writerDone <- err
				return
			}
			if _, err := tx.Exec(ctx, `UPDATE run_env_vars SET retired_at=now() WHERE var_id=$1 AND rev=1`, varID); err != nil {
				writerDone <- err
				return
			}
			if _, err := store.New(tx).InsertRunEnvVar(ctx, next); err != nil {
				writerDone <- err
				return
			}
			writerDone <- tx.Commit(ctx)
		}()
		started.Wait()
		select {
		case err := <-writerDone:
			t.Errorf("writer finished while the finish held the share lock (err=%v)", err)
			writerDone <- err
		case <-time.After(400 * time.Millisecond):
			blocked = true
		}
	}}
	p := fx.claim(t)
	if p == nil || runEnvByName(p)["API_KEY"].Value != envSecretValue("one") {
		t.Fatalf("claim = %s, want rev 1 delivered", payloadShape(p))
	}
	if !blocked {
		t.Fatal("the writer was not observed waiting")
	}
	select {
	case err := <-writerDone:
		if err != nil {
			t.Fatalf("writer: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("writer still blocked after the claim committed")
	}
	fx.svc.claimHooks = nil
	fx.requeue(runID)
	p2 := fx.claim(t)
	if p2 == nil || runEnvByName(p2)["API_KEY"].Value != envSecretValue("two") || len(p2.Secrets.RunEnvRedact) != 1 {
		t.Fatalf("next claim = %s, want the rotation and the old version for masking", payloadShape(p2))
	}
}

// A users-row writer that holds the row when the finish needs it is a lock-not-available retry:
// the finish never waits, never parks and never fails the run because of it.
func TestRunEnvFinishLockNotAvailableRetriesLiveDB(t *testing.T) {
	fx := newREFix(t, true)
	runID := fx.queuedRun(t, "issue")
	fx.put(t, "API_KEY", true, envSecretValue("one"))
	old := finishRunClaimRetryDelay
	finishRunClaimRetryDelay = 10 * time.Millisecond
	defer func() { finishRunClaimRetryDelay = old }()

	// ClaimRun joins the users row, so the writer takes it after the claim committed: between
	// assembly and finish, held through every bounded retry.
	var tx pgx.Tx
	fx.svc.claimHooks = &claimTestHooks{afterAssembly: func(ctx context.Context, _ store.Run, _ *ClaimPayload, _ error) {
		var err error
		if tx, err = fx.env.pool.Begin(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `SELECT 1 FROM users WHERE id=$1 FOR UPDATE`, fx.userID); err != nil {
			t.Fatal(err)
		}
	}}
	p, claimErr := fx.svc.Claim(fx.env.ctx, fx.worker(t), nil)
	if tx != nil {
		defer func() { _ = tx.Rollback(fx.env.ctx) }()
	}
	if p != nil || !isLockNotAvailable(claimErr) {
		t.Fatalf("claim = (%v, %v), want no payload and a 55P03 after the bounded retries", p != nil, claimErr)
	}
	if r := mustRun(t, fx.env, runID); r.Status != "claimed" || r.FailOrigin.Valid || r.HoldReason.Valid {
		t.Fatalf("status=%s fail=%v hold=%v, want the run untouched", r.Status, r.FailOrigin, r.HoldReason)
	}
}

// Deleting the repo after a claim cascades the run and its receipts and the variable rows, and
// leaves the identity rows behind: the variable ids stay bound to their name, so no later row can
// reuse one (there is no unprotected publish to worry about: the run that needed masking is gone).
func TestRunEnvRepoDeleteAfterClaimCascadesReceiptsKeepsIdentityLiveDB(t *testing.T) {
	fx := newREFix(t, true)
	runID := fx.queuedRun(t, "issue")
	secretID := fx.put(t, "API_KEY", true, envSecretValue("one"))
	if p := fx.claim(t); p == nil {
		t.Fatal("claim idle")
	}
	if rs := fx.receipts(t, runID); len(rs) != 1 {
		t.Fatalf("receipts = %+v", rs)
	}
	fx.env.exec(`DELETE FROM repos WHERE id = $1`, fx.repoID)
	count := func(sql string, args ...any) int {
		var n int
		if err := fx.env.pool.QueryRow(fx.env.ctx, sql, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(`SELECT count(*) FROM runs WHERE id=$1`, runID); n != 0 {
		t.Errorf("run survived the repo delete (%d)", n)
	}
	if n := count(`SELECT count(*) FROM run_env_receipts WHERE run_id=$1`, runID); n != 0 {
		t.Errorf("receipts survived the run (%d)", n)
	}
	if n := count(`SELECT count(*) FROM run_env_vars WHERE var_id=$1`, secretID); n != 0 {
		t.Errorf("variable rows survived the repo (%d)", n)
	}
	if n := count(`SELECT count(*) FROM run_env_var_ids WHERE var_id=$1`, secretID); n != 1 {
		t.Errorf("identity rows = %d, want the identity to survive", n)
	}
}
