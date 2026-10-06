package workersvc

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vtmocanu/uzi/api/internal/store"
)

const policyFailureReason = "Codex provider safety-policy refusal (cyberPolicy)"

// policyObservation uses worker-shaped metadata, plus an unknown field to pin opaque API storage.
func policyObservation(seq int32, child bool) IncomingMessage {
	root := "pr-" + strings.ReplaceAll(uuid.NewString(), "-", "") + "-1"
	payload := map[string]any{
		"event": "provider_policy_refusal", "provider": "codex", "category": "policy_refusal",
		"policy_tag": "cyberPolicy", "origin": "root", "phase": "planning",
		"correlation_id": root, "opaque_extension": map[string]any{"retained": true},
	}
	if child {
		payload["origin"] = "child"
		payload["phase"] = "implementation"
		payload["role"] = "reviewer"
		payload["parent_correlation_id"] = root
		payload["correlation_id"] = "pr-" + strings.ReplaceAll(uuid.NewString(), "-", "") + "-1"
	}
	raw, _ := json.Marshal(payload)
	return IncomingMessage{Seq: seq, Kind: "status", Payload: raw}
}

func TestProviderPolicyRefusalStateAndCheckLiveDB(t *testing.T) {
	e := setupUsageTail(t)
	gen := int64(1)
	origin, reason := "provider_policy_refusal", policyFailureReason
	before := mustRun(t, e.codexTestEnv, e.runID)
	request := StateRequest{
		State: "failed", ClaimGeneration: &gen, FailOrigin: &origin, FailureReason: &reason,
		MessagesThroughSeq: throughPtr(3),
	}
	if err := e.svc.AppendMessagesForClaim(e.ctx, e.wkr, e.runID,
		[]IncomingMessage{policyObservation(1, false), policyObservation(3, true)}, &gen); err != nil {
		t.Fatal(err)
	}
	if _, applied, err := e.svc.SetState(e.ctx, e.wkr, e.runID, request); !errors.Is(err, ErrMessagesPending) || applied {
		t.Fatalf("policy report with log hole: applied=%v err=%v, want ErrMessagesPending", applied, err)
	}
	if pending := mustRun(t, e.codexTestEnv, e.runID); pending.Status != "running" || pending.FailOrigin.Valid {
		t.Fatal("policy report bypassed the terminal message fence")
	}
	if err := e.svc.AppendMessagesForClaim(e.ctx, e.wkr, e.runID,
		[]IncomingMessage{policyObservation(2, true)}, &gen); err != nil {
		t.Fatal(err)
	}
	run, applied, err := e.svc.SetState(e.ctx, e.wkr, e.runID, request)
	if err != nil || !applied || run.Status != "failed" {
		t.Fatalf("SetState: applied=%v status=%s err=%v", applied, run.Status, err)
	}
	got := mustRun(t, e.codexTestEnv, e.runID)
	if got.Status != "failed" || !got.FailOrigin.Valid || got.FailOrigin.String != origin ||
		!got.FailureReason.Valid || got.FailureReason.String != reason {
		t.Fatalf("stored failure = %+v", got)
	}
	if got.RequeueCount != before.RequeueCount || got.RecoveryWaitCount != before.RecoveryWaitCount ||
		got.RecoveryWaitCause != before.RecoveryWaitCause || got.RecoveryRetryNotBefore != before.RecoveryRetryNotBefore {
		t.Fatal("policy failure changed execution requeue or recovery state")
	}
	for _, state := range []string{"running", "recovery_wait"} {
		_, applied, err := e.svc.SetState(e.ctx, e.wkr, e.runID, StateRequest{State: state, ClaimGeneration: &gen})
		if err != nil || applied {
			t.Fatalf("terminal report %s: applied=%v err=%v", state, applied, err)
		}
	}
	if got := mustRun(t, e.codexTestEnv, e.runID); got.Status != "failed" || got.FailOrigin.String != origin ||
		got.RequeueCount != before.RequeueCount || got.RecoveryWaitCount != before.RecoveryWaitCount ||
		got.RecoveryWaitCause != before.RecoveryWaitCause || got.RecoveryRetryNotBefore != before.RecoveryRetryNotBefore {
		t.Fatal("late report changed terminal policy failure")
	}
	_, err = e.pool.Exec(e.ctx, "UPDATE runs SET fail_origin = 'invalid_policy_origin' WHERE id = $1", e.runID)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "runs_fail_origin_check" {
		t.Fatalf("invalid origin: %v, want runs_fail_origin_check SQLSTATE 23514", err)
	}
}

func TestProviderPolicyRefusalOpaqueMessagesAndFencesLiveDB(t *testing.T) {
	e := setupUsageTail(t)
	gen := int64(1)
	msgs := []IncomingMessage{policyObservation(1, false), policyObservation(2, true), policyObservation(3, false)}
	for attempt := 0; attempt < 2; attempt++ {
		if err := e.svc.AppendMessagesForClaim(e.ctx, e.wkr, e.runID, msgs, &gen); err != nil {
			t.Fatalf("message delivery %d: %v", attempt, err)
		}
	}
	read := func() []store.RunMessage {
		t.Helper()
		rows, err := e.q.ListRunMessagesAfter(e.ctx, store.ListRunMessagesAfterParams{RunID: e.runID})
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	rows := read()
	if len(rows) != 3 {
		t.Fatalf("deduplicated messages = %d, want 3", len(rows))
	}
	for i, row := range rows {
		var got, want any
		if err := json.Unmarshal(row.Payload, &got); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(msgs[i].Payload, &want); err != nil {
			t.Fatal(err)
		}
		if row.Seq != msgs[i].Seq || !row.ClaimGeneration.Valid || row.ClaimGeneration.Int64 != gen || !reflect.DeepEqual(got, want) {
			t.Fatalf("opaque roundtrip or generation differs: %+v", row)
		}
	}
	before := mustRun(t, e.codexTestEnv, e.runID)
	if before.Status != "running" || before.FailOrigin.Valid || before.LastSeq != 3 ||
		before.RequeueCount != 0 || before.RecoveryWaitCount != 0 || before.RecoveryWaitCause.Valid {
		t.Fatal("observations changed execution state or failed to advance last_seq")
	}
	e.exec("UPDATE runs SET claim_generation = 2 WHERE id = $1", e.runID)
	if err := e.svc.AppendMessagesForClaim(e.ctx, e.wkr, e.runID, []IncomingMessage{policyObservation(4, true)}, &gen); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("stale generation: %v", err)
	}
	gen = 2
	if err := e.svc.AppendMessagesForClaim(e.ctx, e.wkr, e.runID, msgs, &gen); err != nil {
		t.Fatalf("redelivery in new generation: %v", err)
	}
	// The seq identity remains stable across generations.
	if got := read(); !reflect.DeepEqual(got, rows) {
		t.Fatal("new generation replaced existing seq payload or provenance")
	}
	e.exec("UPDATE runs SET status = 'failed', fail_origin = 'provider_policy_refusal', claim_released_at = now() WHERE id = $1", e.runID)
	if err := e.svc.AppendMessagesForClaim(e.ctx, e.wkr, e.runID,
		[]IncomingMessage{policyObservation(4, true)}, &gen); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("released terminal claim: %v, want ErrStaleClaim", err)
	}
	// Legacy posts are silently discarded by the released-claim SQL fence.
	// Preserve the existing response contract and verify storage remains unchanged.
	if err := e.svc.AppendMessagesForClaim(e.ctx, e.wkr, e.runID,
		[]IncomingMessage{policyObservation(4, true)}, nil); err != nil {
		t.Fatalf("released legacy post: %v, want nil", err)
	}
	if got := read(); !reflect.DeepEqual(got, rows) {
		t.Fatal("fenced messages modified storage")
	}
	if got := mustRun(t, e.codexTestEnv, e.runID); got.LastSeq != 3 || got.Status != "failed" || got.RequeueCount != 0 || got.RecoveryWaitCount != 0 {
		t.Fatal("fenced messages changed execution state")
	}
}

func TestProviderPolicyRefusalOutcomesLiveDB(t *testing.T) {
	e := setupUsageTail(t)
	origins := AllHumanLandableFailOrigins()
	// Factory results may include other fixtures; compare the exact delta.
	before, err := e.q.AdminRunOutcomes(e.ctx, origins)
	if err != nil {
		t.Fatal(err)
	}
	gen := int64(1)
	msgs := []IncomingMessage{policyObservation(1, false), policyObservation(2, true), policyObservation(3, false)}
	if err := e.svc.AppendMessagesForClaim(e.ctx, e.wkr, e.runID, msgs, &gen); err != nil {
		t.Fatal(err)
	}
	origin, reason := "provider_policy_refusal", policyFailureReason
	if _, applied, err := e.svc.SetState(e.ctx, e.wkr, e.runID, StateRequest{State: "failed", ClaimGeneration: &gen, FailOrigin: &origin, FailureReason: &reason}); err != nil || !applied {
		t.Fatalf("fail policy run: applied=%v err=%v", applied, err)
	}
	parent := e.seedFoldRun(t, e.userID, e.wkr.ID, e.repoID, gen, "sess-policy-parent")
	if err := e.svc.AppendMessagesForClaim(e.ctx, e.wkr, parent, []IncomingMessage{policyObservation(1, true)}, &gen); err != nil {
		t.Fatal(err)
	}
	// Complete through the service so the child observation cannot act as a terminal signal.
	if _, applied, err := e.svc.SetState(e.ctx, e.wkr, parent, StateRequest{State: "completed", ClaimGeneration: &gen}); err != nil || !applied {
		t.Fatalf("complete parent: applied=%v err=%v", applied, err)
	}
	assertOrigins := func(scope string, failed int64, raw []byte, wantPolicy int64) {
		t.Helper()
		var buckets map[string]int64
		if err := json.Unmarshal(raw, &buckets); err != nil {
			t.Fatal(err)
		}
		var sum int64
		for _, n := range buckets {
			sum += n
		}
		if sum != failed || buckets[origin] != wantPolicy {
			t.Fatalf("%s: failed=%d origins=%v, want sum==failed and policy=%d", scope, failed, buckets, wantPolicy)
		}
	}
	self, err := e.q.SelfRunOutcomes(e.ctx, store.SelfRunOutcomesParams{UserID: e.userID, LandableOrigins: origins})
	if err != nil {
		t.Fatal(err)
	}
	if self.LifetimeFailed != 1 || self.Last7Failed != 1 || self.LifetimeCompleted != 1 || self.Last7Completed != 1 || self.LifetimeFinished != 2 {
		t.Fatalf("self outcomes: %+v", self)
	}
	assertOrigins("self lifetime", self.LifetimeFailed, self.LifetimeFailOrigins, 1)
	assertOrigins("self week", self.Last7Failed, self.Last7FailOrigins, 1)
	admin, err := e.q.AdminRunOutcomes(e.ctx, origins)
	if err != nil {
		t.Fatal(err)
	}
	var old map[string]int64
	if err := json.Unmarshal(before.LifetimeFailOrigins, &old); err != nil {
		t.Fatal(err)
	}
	assertOrigins("factory lifetime", admin.LifetimeFailed, admin.LifetimeFailOrigins, old[origin]+1)
	var oldWeek map[string]int64
	if err := json.Unmarshal(before.Last7FailOrigins, &oldWeek); err != nil {
		t.Fatal(err)
	}
	assertOrigins("factory week", admin.Last7Failed, admin.Last7FailOrigins, oldWeek[origin]+1)
	if admin.LifetimeFailed != before.LifetimeFailed+1 || admin.Last7Failed != before.Last7Failed+1 ||
		admin.LifetimeCompleted != before.LifetimeCompleted+1 || admin.Last7Completed != before.Last7Completed+1 {
		t.Fatal("factory outcomes counted observations instead of runs")
	}
	users, err := e.q.AdminRunOutcomesPerUser(e.ctx, origins)
	if err != nil {
		t.Fatal(err)
	}
	for _, user := range users {
		if user.UserID == e.userID {
			if user.Failed != 1 || user.Completed != 1 || user.Finished != 2 {
				t.Fatalf("per-user outcomes: %+v", user)
			}
			assertOrigins("per-user", user.Failed, user.FailOrigins, 1)
			return
		}
	}
	t.Fatal("missing per-user outcome")
}

func TestProviderPolicyRefusalJudgeEnqueueLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, repoID := seedCodexOnlyOriginUser(t, env)
	svc := judgeEnqueueSvc(env, userID)
	for _, iter := range []int32{0, 7} {
		targetID := uuid.New()
		env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, harness, fail_origin, iteration_count)
			VALUES ($1, $2, $3, 'issue', $4, 'policy refusal', 'd', 'failed', 'codex', 'provider_policy_refusal', $5)`,
			targetID, userID, repoID, 232100+iter, iter)
		svc.maybeEnqueueJudge(env.ctx, mustRun(t, env, targetID))
		id, harness, ok := judgeForTarget(t, env, targetID)
		if !ok || harness != "codex" {
			t.Fatalf("iteration %d: judge missing or wrong harness: %s", iter, harness)
		}
		if !mustRun(t, env, id).CodexSecretID.Valid {
			t.Fatal("judge credential binding was not frozen")
		}
	}
}
