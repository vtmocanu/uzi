package workersvc

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Reassemble a new worker claim from persisted plan, session, frame and frozen-binding
// state. Worker accounting state is deliberately absent from this API fixture.
func TestCodexPlanningCrashClaimRediscoveryLiveDB(t *testing.T) {
	f, cf := codexPlanningFixture(t)
	f.env.exec(`UPDATE users SET default_codex_model='gpt-6-sol', default_codex_effort='medium',
		planning_codex_model='gpt-6-astra', planning_codex_effort='high' WHERE id=$1`, f.userID)
	w := store.Worker{ID: f.workerID, UserID: f.userID, ProtocolCapabilities: codexPlanningCaps(capability.CodexPhaseModelsV1)}
	for _, tc := range []struct {
		name, phase      string
		approved, replan bool
		seq              int64
	}{
		{"approved", "implementing", true, false, 0},
		{"unapproved", "awaiting_approval", false, false, 7},
		// An empty resume_phase means enter planning under the existing claim contract.
		{"replan", "", false, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := insertCodexPlanningRun(t, f, cf, "issue")
			const session = "codex-crash-saved-thread"
			const plan = "# Persisted Codex plan"
			n, err := f.env.q.SetRunAwaitingApproval(f.env.ctx, store.SetRunAwaitingApprovalParams{
				ID: id, WorkerID: pgconv.UUID(f.workerID),
				PlanMd: pgconv.TextOrNull(plan), SessionID: pgconv.TextOrNull(session),
			})
			if err != nil || n != 1 {
				t.Fatalf("persist plan/session: rows=%d err=%v", n, err)
			}
			f.env.exec(`INSERT INTO run_messages(run_id,seq,kind,payload) VALUES
				($1,3,'plan','{"plan_md":"old"}'), ($1,7,'plan_revising','{}'), ($1,9,'text','{}')`, id)
			f.env.exec(`UPDATE runs SET status='claimed', auto_approve=$2, last_seq=9 WHERE id=$1`, id, tc.approved)
			if tc.replan {
				// A genuine replan clears the old plan, retaining the saved Codex thread.
				f.env.exec(`UPDATE runs SET plan_md=NULL WHERE id=$1`, id)
			}
			run := mustRun(t, f.env, id)
			if run.Harness != "codex" || !run.SessionID.Valid || run.SessionID.String != session ||
				(!tc.replan && (!run.PlanMd.Valid || run.PlanMd.String != plan)) || (tc.replan && run.PlanMd.Valid) {
				t.Fatal("persisted Codex restart fixture lost its plan/session state")
			}
			restarted := New(f.env.q, f.env.box, testParams())
			restarted.SetVault(f.env.vlt)
			restarted.SetTxBeginner(f.env.pool)
			p, err := restarted.assembleClaim(f.env.ctx, w, run)
			if err != nil {
				t.Fatalf("rediscover Codex claim: %v", err)
			}
			if p.ResumePhase != tc.phase || p.PlanApproved != tc.approved || p.ResumePlanSeq != tc.seq {
				t.Fatalf("rediscovered phase=%q approved=%v seq=%d, want %q/%v/%d",
					p.ResumePhase, p.PlanApproved, p.ResumePlanSeq, tc.phase, tc.approved, tc.seq)
			}
			if p.Secrets.Codex == nil || p.Secrets.Codex.AuthMode != codexAuthModeSubscription || p.Secrets.AnthropicOAuthToken != "" {
				t.Fatal("rediscovered claim did not use the frozen Codex credential")
			}
			if derefOr(p.SessionID, "") != session || (!tc.replan && derefOr(p.PlanMd, "") != plan) || (tc.replan && p.PlanMd != nil) {
				t.Fatal("claim did not restore the saved session and plan")
			}
			if derefOr(p.Config.DefaultModel, "") != "gpt-6-sol" || derefOr(p.Config.DefaultEffort, "") != "medium" {
				t.Fatal("implementation pair changed during restart")
			}
			want := map[string]any{"model": "gpt-6-astra", "effort": "high",
				"model_source": "user_planning_default", "effort_source": "user_planning_default"}
			if !reflect.DeepEqual(planPhaseOnWire(t, p), want) {
				t.Fatal("restart did not recover the current planning pair and provenance")
			}
		})
	}
}

func TestCodexPlanningCrashDeliveredFramesLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, workerID, repoID := env.seedCodexInfra(t)
	const session = "codex-crash-usage-thread"
	const modelA, modelB = "gpt-6-astra", "gpt-6-sol"
	id := env.seedFoldRun(t, userID, workerID, repoID, 1, session)
	// seedFoldRun intentionally leaves the schema's Claude default in place.
	env.exec(`UPDATE runs SET harness='codex' WHERE id=$1`, id)
	w := store.Worker{ID: workerID, UserID: userID, Status: "online"}
	newService := func() *Service {
		s := New(env.q, env.box, testParams())
		s.SetTxBeginner(env.pool)
		return s
	}
	result := func(seq int32, model string, input, read, write, output int64) IncomingMessage {
		m := resultFrame(seq, model, input)
		raw, err := json.Marshal(map[string]any{
			"event": "result", "modelUsage": map[string]any{model: map[string]any{
				"inputTokens": input, "cacheReadInputTokens": read, "cacheCreationInputTokens": write,
				"outputTokens": output, "costUSD": 0, "costStatus": "subscription",
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		m.Payload = raw
		return m
	}
	first := []IncomingMessage{initFrame(1), result(2, modelA, 70, 20, 10, 40)}
	second := []IncomingMessage{initFrame(3), result(4, modelB, 8, 1, 1, 5)}
	third := []IncomingMessage{initFrame(5), result(6, modelB, 4, 2, 1, 3)}
	// These positive, distinct tails existed only in the dead process's buffer.
	// Neither tail is ever passed to AppendMessagesForClaim, including on retry.
	unsent := []IncomingMessage{result(5, modelA, 7000, 2000, 1000, 4000), result(6, modelB, 8000, 1000, 1000, 5000)}
	if len(unsent) != 2 || string(unsent[0].Payload) == string(first[1].Payload) || string(unsent[1].Payload) == string(second[1].Payload) {
		t.Fatal("unsent-tail fixture must differ from delivered work")
	}
	appendBatch := func(s *Service, gen int64, frames []IncomingMessage) {
		t.Helper()
		if err := s.AppendMessagesForClaim(env.ctx, w, id, frames, &gen); err != nil {
			t.Fatalf("append generation %d: %v", gen, err)
		}
	}
	// Read complete usage rows, omitting only the mutable update timestamp.
	readRows := func() []string {
		t.Helper()
		rows, err := env.pool.Query(env.ctx, `SELECT (to_jsonb(u)-'updated_at')::text
			FROM run_usage u WHERE run_id=$1 ORDER BY lineage_epoch,model`, id)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var got []string
		for rows.Next() {
			var row string
			if err := rows.Scan(&row); err != nil {
				t.Fatal(err)
			}
			got = append(got, row)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return got
	}
	equalRows := func(label string, before []string) {
		t.Helper()
		after := readRows()
		if len(after) != len(before) {
			t.Fatalf("%s row count=%d want %d", label, len(after), len(before))
		}
		for i := range before {
			if after[i] != before[i] {
				t.Fatalf("%s changed complete usage row %d", label, i)
			}
		}
	}
	assertStored := func(legs int) {
		t.Helper()
		run := mustRun(t, env, id)
		if run.Harness != "codex" || !run.SessionID.Valid || run.SessionID.String != session || int64(run.LastSeq) != int64(legs)*2 {
			t.Fatal("persisted run lost Codex session or delivered-prefix high water")
		}
		rows, err := env.pool.Query(env.ctx, `SELECT run_id,session_id,model,lineage_epoch,lineage_index,
			harness,usage_basis,claim_generation,input_tokens,cache_read_tokens,cache_creation_tokens,output_tokens
			FROM run_usage WHERE run_id=$1 ORDER BY lineage_epoch,model`, id)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		want := []struct {
			model  string
			tokens [4]int64
		}{{modelA, [4]int64{70, 20, 10, 40}}, {modelB, [4]int64{8, 1, 1, 5}}, {modelB, [4]int64{4, 2, 1, 3}}}
		i := 0
		for rows.Next() {
			var rid uuid.UUID
			var sid, model, harness, basis string
			var epoch, lineage int32
			var gen int64
			var tokens [4]int64
			if err := rows.Scan(&rid, &sid, &model, &epoch, &lineage, &harness, &basis, &gen, &tokens[0], &tokens[1], &tokens[2], &tokens[3]); err != nil {
				t.Fatal(err)
			}
			if i >= legs {
				t.Fatal("unexpected usage leg (possibly reconstructed unsent tail)")
			}
			if rid != id || sid != session || model != want[i].model || epoch != int32(i+1) || lineage != 0 ||
				harness != "codex" || basis != "per_leg" || gen != int64(i+1) || tokens != want[i].tokens {
				t.Fatalf("usage leg %d key, provenance, basis or token vector differs", i)
			}
			i++
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if i != legs {
			t.Fatalf("usage legs=%d want %d", i, legs)
		}
		total, err := env.q.GetRunUsageTotal(env.ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		cost, err := total.CostUsd.Float64Value()
		if err != nil || !cost.Valid || cost.Float64 != 0 || total.CostStatus != "subscription" {
			t.Fatal("delivered Codex subscription cost/status changed")
		}
		wantTotal := [4]int64{70, 20, 10, 40}
		switch legs {
		case 2:
			wantTotal = [4]int64{78, 21, 11, 45}
		case 3:
			wantTotal = [4]int64{82, 23, 12, 48}
		}
		if [4]int64{total.InputTokens, total.CacheReadTokens, total.CacheCreationTokens, total.OutputTokens} != wantTotal {
			t.Fatalf("delivered-only additive token total differs for %d legs", legs)
		}
		var count int
		if err := env.pool.QueryRow(env.ctx, `SELECT count(*) FROM run_messages WHERE run_id=$1`, id).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != legs*2 {
			t.Fatalf("stored frames=%d want %d (unsent tail must stay absent)", count, legs*2)
		}
		wantFrames := []IncomingMessage{first[0], first[1], second[0], second[1], third[0], third[1]}
		for seq := 1; seq <= legs*2; seq++ {
			var gen int64
			var kind string
			var payload []byte
			if err := env.pool.QueryRow(env.ctx, `SELECT claim_generation,kind,payload
				FROM run_messages WHERE run_id=$1 AND seq=$2`, id, seq).Scan(&gen, &kind, &payload); err != nil {
				t.Fatal(err)
			}
			if gen != int64((seq+1)/2) {
				t.Fatalf("frame %d generation=%d", seq, gen)
			}
			var gotPayload, wantPayload any
			if err := json.Unmarshal(payload, &gotPayload); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(wantFrames[seq-1].Payload, &wantPayload); err != nil {
				t.Fatal(err)
			}
			if kind != wantFrames[seq-1].Kind || !reflect.DeepEqual(gotPayload, wantPayload) {
				t.Fatalf("persisted frame %d differs from delivered frame", seq)
			}
		}
	}
	svc := newService()
	appendBatch(svc, 1, first)
	assertStored(1)
	before := readRows()
	appendBatch(svc, 1, first)
	equalRows("generation 1 redelivery", before)
	assertStored(1)
	// Lose the process and its unflushed tail, then resume the same saved thread.
	env.exec(`UPDATE runs SET claim_generation=2 WHERE id=$1`, id)
	svc = newService()
	appendBatch(svc, 2, second)
	assertStored(2)
	before = readRows()
	appendBatch(svc, 2, second)
	equalRows("generation 2 redelivery", before)
	assertStored(2)
	// Another reclaimed implementation leg on the same model must ADD, rather than
	// taking the maximum of same-session copies across distinct lineage epochs.
	env.exec(`UPDATE runs SET claim_generation=3 WHERE id=$1`, id)
	svc = newService()
	appendBatch(svc, 3, third)
	assertStored(3)
	before = readRows()
	appendBatch(svc, 3, third)
	equalRows("generation 3 redelivery", before)
	assertStored(3)
	if err := RefoldRunUsage(env.ctx, env.pool, env.q, mustRun(t, env, id)); err != nil {
		t.Fatal(err)
	}
	equalRows("full persisted-frame refold", before)
	assertStored(3)
}

func TestCodexPlanningCrashCommittedHoldOwnerActionsLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	o := seedReevalOwner(t, env, BindModeAuto, false)
	w := store.Worker{ID: o.workerID, UserID: o.userID}
	newService := func() *Service {
		s := New(env.q, env.box, testParams())
		s.SetTxBeginner(env.pool)
		return s
	}
	const session = "codex-held-thread"
	head := "0123456789abcdef0123456789abcdef01234567"
	seed := func(iid int64) uuid.UUID {
		id := seedPhaseSwitchRun(t, env, o, iid, "awaiting_approval")
		env.exec(`UPDATE runs SET harness='codex',plan_md='# Approved plan',session_id=$2,auto_approve=true,
			status_since=now()-interval '120 seconds',budget_paused_seconds=11 WHERE id=$1`, id, session)
		return id
	}
	probe := func(s *Service, id uuid.UUID, want string) {
		t.Helper()
		status, _, gen, hold, released, _, _, err := s.RunOwnership(env.ctx, w, id)
		if err != nil || status != want || gen != 3 || released || hold == nil || *hold != HoldReasonPhaseSwitchUnavailable {
			t.Fatalf("ownership status=%q generation=%d released=%v hold=%v err=%v, want %s/3/marker", status, gen, released, hold, err, want)
		}
	}
	id := seed(325301)
	before := mustRun(t, env, id)
	svc := newService()
	// The commit succeeds; pretend its response was lost before the worker observed it.
	_, applied, err := svc.SetRunPhaseSwitchHold(env.ctx, w, id, &head, 3, PhaseSwitchImplement)
	if err != nil || !applied {
		t.Fatalf("initial hold applied=%v err=%v", applied, err)
	}
	first := mustRun(t, env, id)
	// Bound the interval from the persisted transition timestamp, not a sleep or
	// a timing tolerance: PostgreSQL rounds the elapsed seconds when casting to int.
	waitSeconds := int64(first.StatusSince.Time.Sub(before.StatusSince.Time).Round(time.Second) / time.Second)
	if first.Harness != "codex" || first.Status != "paused" || first.SessionID.String != session ||
		waitSeconds < 120 || int64(first.BudgetPausedSeconds) != int64(before.BudgetPausedSeconds)+waitSeconds {
		t.Fatal("hold did not persist Codex session and bank the exact approval wait")
	}
	svc = newService()
	probe(svc, id, "paused")
	again, applied, err := svc.SetRunPhaseSwitchHold(env.ctx, w, id, &head, 3, PhaseSwitchImplement)
	if err != nil || !applied {
		t.Fatalf("lost-ACK retry applied=%v err=%v", applied, err)
	}
	if !reflect.DeepEqual(first, again) {
		t.Fatal("lost-ACK retry changed the committed held row, timestamps or budgets")
	}
	if _, err := env.q.ResumePausedRun(env.ctx, store.ResumePausedRunParams{ID: id, UserID: o.userID, GlobalTimeoutSeconds: 3600}); err != nil {
		t.Fatal(err)
	}
	queued := mustRun(t, env, id)
	if queued.Status != "queued" || queued.SessionID != first.SessionID || queued.WorkerID != first.WorkerID ||
		queued.ClaimGeneration != 3 || queued.ClaimReleasedAt.Valid || queued.HoldReason != first.HoldReason || queued.HoldCapturedHead != first.HoldCapturedHead {
		t.Fatal("owner resume lost session, live claim or hold marker")
	}
	stale, applied, err := svc.SetRunPhaseSwitchHold(env.ctx, w, id, &head, 3, PhaseSwitchImplement)
	// applied=false with the real queued row is the service equivalent of the handler's 409.
	if err != nil || applied || stale.Status != "queued" {
		t.Fatalf("stale hold retry status=%q applied=%v err=%v", stale.Status, applied, err)
	}
	if !reflect.DeepEqual(queued, mustRun(t, env, id)) {
		t.Fatal("stale retry reparked or changed the owner's queued row")
	}
	probe(svc, id, "queued")
	cancelID := seed(325302)
	if _, applied, err := svc.SetRunPhaseSwitchHold(env.ctx, w, cancelID, &head, 3, PhaseSwitchImplement); err != nil || !applied {
		t.Fatalf("cancel fixture hold applied=%v err=%v", applied, err)
	}
	res, err := svc.SubmitInputWithOptions(env.ctx, o.userID, cancelID, "cancel", "owner cancel", nil, SubmitInputOptions{})
	if err != nil || !res.ServerSide {
		t.Fatalf("owner cancel serverSide=%v err=%v", res.ServerSide, err)
	}
	status, _, _, _, _, _, _, err := newService().RunOwnership(env.ctx, w, cancelID)
	if err != nil || status != "cancelled" {
		t.Fatalf("ownership after cancel status=%q err=%v", status, err)
	}
}
