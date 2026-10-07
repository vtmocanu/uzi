package workersvc

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Exercise recovery through Publish, the attempts arm, and real Claim assembly.
func TestLiveUnknownPublishRecoveryAndReclaimLiveDB(t *testing.T) {
	e := setupCodexLiveDB(t)
	user, worker, repo := e.seedCodexInfra(t)
	seal := func(value string) []byte {
		t.Helper()
		b, err := e.box.Seal([]byte(value))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	e.exec("UPDATE forge_connections SET token_ciphertext=$1 WHERE user_id=$2", seal(codexToken("bot-pat")), user)
	e.exec("INSERT INTO user_secrets(id,user_id,kind,label,is_default,ciphertext,sealed_with) VALUES($1,$2,'anthropic_token',$3,true,$4,'master')", uuid.New(), user, "model-"+uuid.NewString(), seal(codexToken("anthropic")))
	run := uuid.New()
	e.exec("INSERT INTO runs(id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,status) VALUES($1,$2,$3,'issue',101,'t','d','queued')", run, user, repo)
	svc := New(e.q, e.box, testParams())
	svc.SetTxBeginner(e.pool)
	svc.SetRetentionLockPool(e.pool)
	svc.SetForgeBaseURLAllowed(func(u string) bool { return u == "https://forge.e2e" })
	svc.SetBackground(func(fn func()) { fn() })
	origin := newMemForge()
	svc.SetCreateRefFn(origin.createRef)
	svc.SetDeleteCheckpointFn(origin.deleteRef)
	svc.SetListRefTipsFn(origin.listRefTips)
	w := store.Worker{ID: worker, UserID: user, Name: "recovery-worker", Status: "online", ProtocolCapabilities: []string{capability.RecoveryArchiveV1}}
	claim := func(gen int64, tip *string) {
		t.Helper()
		p, err := svc.Claim(e.ctx, w, nil)
		if err != nil || p == nil {
			t.Fatalf("Claim: payload=%+v err=%v", p, err)
		}
		if p.RunID != run.String() || p.ClaimGeneration != gen || !reflect.DeepEqual(p.CheckpointTip, tip) {
			t.Fatalf("claim identity/anchor: got run=%s generation=%d tip=%v; want run=%s generation=%d tip=%v", p.RunID, p.ClaimGeneration, p.CheckpointTip, run, gen, tip)
		}
	}
	claim(1, nil)
	ref := checkpointRefPrefix + agentIssueBranch(101)
	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	svc.SetPublishFn(func(_ context.Context, o pushbroker.Options) (pushbroker.Result, error) {
		if !origin.land(ref, "", o.DeclaredTip) {
			t.Fatal("fixture push did not land")
		}
		cancel()
		return pushbroker.Result{Ref: ref, Disposition: pushbroker.PublishOutcomeUnknown}, context.Canceled
	})
	res, err := svc.Publish(ctx, w, run, lateTip, []byte("pack"))
	if err == nil || err.Error() != "publish: context canceled" || res.Published || ctx.Err() == nil {
		t.Fatalf("unknown cancelled publish: %+v %v", res, err)
	}
	attempts := func() []store.CheckpointPublishAttempt {
		t.Helper()
		rows, err := e.pool.Query(e.ctx, "SELECT id FROM checkpoint_publish_attempts WHERE run_id=$1 ORDER BY id", run)
		if err != nil {
			t.Fatal(err)
		}
		var ids []uuid.UUID
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		var out []store.CheckpointPublishAttempt
		for _, id := range ids {
			a, err := e.q.GetCheckpointPublishAttempt(e.ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, a)
		}
		return out
	}
	a := attempts()
	if len(a) != 1 || !a[0].ReconcileReadyAt.Valid {
		t.Fatalf("cancelled context lost readiness: %+v", a)
	}
	original := a[0]
	svc.SetPublishFn(origin.publish)
	res, err = svc.Publish(e.ctx, w, run, lateTip, []byte("pack"))
	if err != nil || res.Published || res.Skipped != "not_descendant" {
		t.Fatalf("immediate unchanged retry: %+v %v", res, err)
	}
	if a := attempts(); len(a) != 1 || !reflect.DeepEqual(a[0], original) {
		t.Fatalf("retry consumed original evidence: %+v", a)
	}
	before, berr := e.q.GetCheckpointRetention(e.ctx, run)
	origin.mu.Lock()
	events := append([]string(nil), origin.events...)
	publishes, creates := origin.publishCalls, origin.createCalls
	origin.mu.Unlock()
	svc.retentionHooks = &retentionTestHooks{beforeForgeWrite: func(_ uuid.UUID, op string) {
		if op != "attempt-confirm" {
			t.Fatalf("unexpected settlement/write: %s", op)
		}
	}}
	n, err := svc.reconcilePublishAttempts(e.ctx, pgconv.UUID(run), nil)
	if err != nil || n != 1 {
		t.Fatalf("live reconcile: count=%d err=%v", n, err)
	}
	if a := attempts(); len(a) != 0 {
		t.Fatalf("confirmed original not consumed: %+v", a)
	}
	after, aerr := e.q.GetCheckpointRetention(e.ctx, run)
	if !errors.Is(berr, pgx.ErrNoRows) || !errors.Is(aerr, pgx.ErrNoRows) || !reflect.DeepEqual(before, after) {
		t.Fatalf("live reconciliation created retention: before=%+v/%v after=%+v/%v", before, berr, after, aerr)
	}
	origin.mu.Lock()
	if !reflect.DeepEqual(events, origin.events) || publishes != origin.publishCalls || creates != origin.createCalls || len(origin.deletes) != 0 {
		t.Fatalf("reconcile wrote forge: events=%v", origin.events)
	}
	origin.mu.Unlock()
	tip, err := e.q.GetRunCheckpointTipForRetention(e.ctx, run)
	if err != nil || !tip.Valid || tip.String != lateTip {
		t.Fatalf("confirmed tip: %+v %v", tip, err)
	}
	res, err = svc.Publish(e.ctx, w, run, lateTip, []byte("pack"))
	if err != nil || !res.Published || res.Skipped != "" {
		t.Fatalf("unchanged retry after confirmation: %+v %v", res, err)
	}
	if a := attempts(); len(a) != 0 {
		t.Fatalf("successful unchanged retry left attempts: %+v", a)
	}
	e.exec("UPDATE runs SET status='queued',worker_id=NULL,updated_at=now() WHERE id=$1", run)
	anchor := lateTip
	claim(2, &anchor)
	e.exec("UPDATE runs SET status='queued',worker_id=NULL,updated_at=now() WHERE id=$1", run)
	claim(3, &anchor)
}
