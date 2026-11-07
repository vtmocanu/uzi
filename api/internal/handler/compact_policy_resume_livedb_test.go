package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/jointoken"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// compactPolicyFx is the real application router plus a policy-hold capable Codex worker.
type compactPolicyFx struct {
	t           *testing.T
	h           *Handler
	router      http.Handler
	owner       uuid.UUID
	other       uuid.UUID
	jwt         string
	ownerUzc    string
	otherUzc    string
	otherJWT    string
	workerID    uuid.UUID
	workerToken string
	repo        uuid.UUID
	caps        []string
	exec        func(sql string, args ...any)
	query       func(sql string, args ...any) string
}

func claimParamsForPolicy(user, worker uuid.UUID, caps []string) store.ClaimRunParams {
	now := time.Now()
	return store.ClaimRunParams{UserID: user, WorkerID: pgconv.UUID(worker), WorkerProtocolCaps: caps, WorkerCaps: []string{},
		CodexCuratedModels: workersvc.CodexCuratedModels(),
		HeartbeatCutoff:    pgconv.Time(now.Add(-time.Minute)), AffinityCutoff: pgconv.Time(now.Add(-25 * time.Minute)),
		SpreadCutoff: pgconv.Time(now.Add(-9 * time.Second)), BackgroundGraceCutoff: pgconv.Time(now.Add(-15 * time.Minute))}
}

func newCompactPolicyFx(t *testing.T) *compactPolicyFx {
	t.Helper()
	h, router, pool := cliLiveDB(t)
	h.wsvc.SetTxBeginner(pool)
	fx := &compactPolicyFx{t: t, h: h, router: router}
	fx.owner, fx.other = cliSeedUser(t, pool, false), cliSeedUser(t, pool, false)
	fx.jwt, fx.otherJWT = cliMintJWT(t, pool, fx.owner), cliMintJWT(t, pool, fx.other)
	fx.ownerUzc, fx.otherUzc = cliMintToken(t, pool, fx.owner, clitoken.ScopeUser), cliMintToken(t, pool, fx.other, clitoken.ScopeUser)
	fx.repo = cliSeedOwnedRepo(t, pool, fx.owner)
	token, hash, err := jointoken.Generate()
	if err != nil {
		t.Fatal(err)
	}
	worker, err := h.q.CreateWorker(context.Background(), store.CreateWorkerParams{UserID: fx.owner, Name: "compact-policy", TokenHash: hash, AnthropicBindMode: "default"})
	if err != nil {
		t.Fatal(err)
	}
	fx.caps = []string{capability.ProviderPolicyHoldV1, capability.CodexHarnessV1, capability.CodexRuntimeV2}
	cliMustExec(t, pool, "UPDATE workers SET protocol_capabilities=$2,status='online' WHERE id=$1", worker.ID, fx.caps)
	fx.workerID, fx.workerToken = worker.ID, token
	fx.exec = func(sql string, args ...any) { cliMustExec(t, pool, sql, args...) }
	fx.query = func(sql string, args ...any) string {
		var s string
		if err := pool.QueryRow(context.Background(), sql, args...).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	t.Cleanup(func() {
		for _, u := range []uuid.UUID{fx.owner, fx.other} {
			_, _ = pool.Exec(context.Background(), "DELETE FROM users WHERE id=$1", u)
		}
	})
	return fx
}

func (fx *compactPolicyFx) seedRun() uuid.UUID {
	fx.t.Helper()
	id := uuid.New()
	fx.exec(`INSERT INTO runs(id,user_id,repo_id,issue_iid,issue_title,issue_description,kind,status,harness,worker_id,claim_generation,session_id,started_at)
		VALUES($1,$2,$3,$4,'policy fixture','desc','issue','running','codex',$5,1,'original-session',now())`,
		id, fx.owner, fx.repo, 36070+int64(id[0])+int64(id[1])*256, fx.workerID)
	return id
}

// hold posts one policy-hold report over the real worker route and returns the outcome id.
func (fx *compactPolicyFx) hold(run uuid.UUID, gen int64, mode string, rehold bool, head string) string {
	fx.t.Helper()
	tag, phase, corr := "cyberPolicy", "implementation", "pr-00000000000000000000000000000000-1"
	req := workersvc.PolicyHoldRequest{OutcomeID: uuid.NewString(), SourceGeneration: gen, ClaimGeneration: gen, Detail: "refused", Cause: "refused",
		ContinuationMode: mode, PolicyTag: &tag, Phase: &phase, CorrelationID: &corr}
	if rehold {
		req.Detail, req.Cause, req.PolicyTag, req.Phase, req.CorrelationID = "resume_rejected", "session_missing", nil, nil, nil
	}
	if head != "" {
		req.Head, req.Published = &head, true
		v := true
		req.CheckpointContainsLatest = &v
	}
	b, err := json.Marshal(req)
	if err != nil {
		fx.t.Fatal(err)
	}
	rec := bearerReqBody(fx.router, http.MethodPost, "/api/worker/runs/"+run.String()+"/policy-hold", fx.workerToken, string(b))
	var reply workersvc.PolicyHoldReply
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &reply) != nil || reply.Disposition != "applied" {
		fx.t.Fatalf("worker hold = %d %s", rec.Code, rec.Body.String())
	}
	return req.OutcomeID
}

func (fx *compactPolicyFx) replay(run uuid.UUID, gen int64, outcome string) *httptest.ResponseRecorder {
	fx.t.Helper()
	tag, phase, corr := "cyberPolicy", "implementation", "pr-00000000000000000000000000000000-1"
	b, _ := json.Marshal(workersvc.PolicyHoldRequest{OutcomeID: outcome, SourceGeneration: gen, ClaimGeneration: gen, Detail: "refused", Cause: "refused",
		ContinuationMode: "same_conversation", PolicyTag: &tag, Phase: &phase, CorrelationID: &corr})
	return bearerReqBody(fx.router, http.MethodPost, "/api/worker/runs/"+run.String()+"/policy-hold", fx.workerToken, string(b))
}

func (fx *compactPolicyFx) claim(run uuid.UUID) store.Run {
	fx.t.Helper()
	claimed, err := fx.h.q.ClaimRun(context.Background(), claimParamsForPolicy(fx.owner, fx.workerID, fx.caps))
	if err != nil || claimed.ID != run {
		fx.t.Fatalf("claim %s: %v err=%v", run, claimed.ID, err)
	}
	return claimed
}

// resumeAs sends the owner resume over the cookie+CSRF path ("cookie") or a uzc_ Bearer ("bearer").
func (fx *compactPolicyFx) resumeAs(client string, run uuid.UUID, body string) *httptest.ResponseRecorder {
	fx.t.Helper()
	path := "/api/runs/" + run.String() + "/resume-now"
	switch client {
	case "cookie":
		return cookieReq(fx.t, fx.router, http.MethodPost, path, fx.jwt, body)
	case "bearer":
		return bearerReqBody(fx.router, http.MethodPost, path, fx.ownerUzc, body)
	case "foreign cookie":
		return cookieReq(fx.t, fx.router, http.MethodPost, path, fx.otherJWT, body)
	default:
		return bearerReqBody(fx.router, http.MethodPost, path, fx.otherUzc, body)
	}
}

func (fx *compactPolicyFx) state(run uuid.UUID) string {
	return fx.query(`SELECT jsonb_build_object('run',to_jsonb(r),'inputs',(SELECT COALESCE(jsonb_agg(to_jsonb(i) ORDER BY i.id),'[]'::jsonb) FROM run_user_inputs i WHERE i.run_id=r.id))::text FROM runs r WHERE r.id=$1`, run)
}

func guidanceSHA(g string) string {
	sum := sha256.Sum256([]byte(g))
	return hex.EncodeToString(sum[:])
}

func resumeBody(mode, episode, guidance string) string {
	b, _ := json.Marshal(map[string]string{"mode": mode, "expected_episode_id": episode, "guidance": guidance})
	return string(b)
}

func TestCompactPolicyResumeAuthMountLiveDB(t *testing.T) {
	for _, lane := range []struct {
		name, hold, mode, wrongMode, client, otherClient string
	}{
		{"fresh_from_saved_work over cookie+CSRF", "fresh_from_saved_work", "fresh_from_saved_work", "preserve", "cookie", "bearer"},
		{"preserve over uzc_ Bearer", "same_conversation", "preserve", "fresh_from_saved_work", "bearer", "cookie"},
	} {
		t.Run(lane.name, func(t *testing.T) {
			fx := newCompactPolicyFx(t)
			run := fx.seedRun()
			head := strings.Repeat("a", 40)
			episode := fx.hold(run, 1, lane.hold, false, head)

			// The owner DTO names the one mode this exact episode accepts.
			rec := bearerReq(fx.router, http.MethodGet, "/api/runs/"+run.String(), fx.ownerUzc)
			var wrapped struct {
				Run struct {
					PolicyHold struct {
						HoldCause        *string  `json:"hold_cause"`
						ResumeModes      []string `json:"resume_modes"`
						RecoveryRequired bool     `json:"recovery_required"`
						VerifiedThis     bool     `json:"recovery_verified_this_claim"`
					} `json:"policy_hold"`
				} `json:"run"`
			}
			if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &wrapped) != nil {
				t.Fatalf("GET run = %d %s", rec.Code, rec.Body.String())
			}
			dto := wrapped.Run
			wantModes := []string{lane.mode}
			if lane.hold == "same_conversation" {
				wantModes = []string{"preserve", "fresh_one_claim"}
			}
			if dto.PolicyHold.HoldCause == nil || *dto.PolicyHold.HoldCause != "refused" || !dto.PolicyHold.RecoveryRequired || dto.PolicyHold.VerifiedThis ||
				!slices.Equal(dto.PolicyHold.ResumeModes, wantModes) {
				t.Fatalf("owner DTO policy_hold = %+v (%s), want modes %v", dto.PolicyHold, rec.Body.String(), wantModes)
			}

			refused := func(what string, client, body string, want int) {
				t.Helper()
				before := fx.state(run)
				got := fx.resumeAs(client, run, body)
				if got.Code != want {
					t.Fatalf("%s via %s = %d, want %d: %s", what, client, got.Code, want, got.Body.String())
				}
				if after := fx.state(run); after != before {
					t.Fatalf("%s via %s mutated the run, session, barrier, inputs or queue", what, client)
				}
			}
			for _, client := range []string{"cookie", "bearer"} {
				refused("legacy empty body", client, "", http.StatusConflict)
				refused("unknown mode fresh", client, resumeBody("fresh", episode, "g"), http.StatusBadRequest)
				refused("missing episode id", client, `{"mode":"`+lane.mode+`"}`, http.StatusBadRequest)
				refused("unknown field", client, `{"mode":"`+lane.mode+`","expected_episode_id":"`+episode+`","extra":1}`, http.StatusBadRequest)
				if lane.hold == "fresh_from_saved_work" {
					// The one-claim Fresh belongs to a same_conversation episode only.
					refused("fresh_one_claim on an attempt-path episode", client, resumeBody("fresh_one_claim", episode, "g"), http.StatusConflict)
				}
				refused("cross-mode with correct id and guidance", client, resumeBody(lane.wrongMode, episode, "narrow"), http.StatusConflict)
				refused("stale episode id", client, resumeBody(lane.mode, uuid.NewString(), "narrow"), http.StatusConflict)
				refused("NUL guidance", client, resumeBody(lane.mode, episode, "a\x00b"), http.StatusBadRequest)
				refused("oversized guidance", client, resumeBody(lane.mode, episode, strings.Repeat("x", 16001)), http.StatusBadRequest)
			}
			// Foreign owners are 404 on both credential kinds, and mutate nothing.
			for _, client := range []string{"foreign cookie", "foreign bearer"} {
				refused("foreign owner", client, resumeBody(lane.mode, episode, "narrow"), http.StatusNotFound)
			}

			ok := fx.resumeAs(lane.client, run, resumeBody(lane.mode, episode, "narrow the scope"))
			var resumed struct {
				Run struct {
					Status string `json:"status"`
				} `json:"run"`
			}
			if ok.Code != http.StatusOK || json.Unmarshal(ok.Body.Bytes(), &resumed) != nil || resumed.Run.Status != "queued" {
				t.Fatalf("resume = %d %s", ok.Code, ok.Body.String())
			}
			row := fx.query(`SELECT jsonb_build_object('status',status,'hold',hold_reason,'session',session_id,'preserve',policy_preserve_required,
				'resolved',policy_resolved_at IS NOT NULL,'required',policy_required_head,'outcome',policy_outcome_id)::text FROM runs WHERE id=$1`, run)
			var got map[string]any
			_ = json.Unmarshal([]byte(row), &got)
			wantSession, wantPreserve, wantRequired := any("original-session"), any(true), any(head)
			if lane.mode == "fresh_from_saved_work" {
				wantSession, wantPreserve = nil, false // R is kept until the saved work is verified
			}
			if got["status"] != "queued" || got["hold"] != nil || got["session"] != wantSession || got["preserve"] != wantPreserve ||
				got["resolved"] != true || got["required"] != wantRequired || got["outcome"] != episode {
				t.Fatalf("resumed row = %s", row)
			}
			inputs := fx.query("SELECT string_agg(kind||'='||COALESCE(body,''),'|' ORDER BY id) FROM run_user_inputs WHERE run_id=$1", run)
			wantInputs := `follow_up=narrow the scope|resume={"policy_mode":"` + lane.mode + `","episode_id":"` + episode + `","guidance_sha256":"` + guidanceSHA("narrow the scope") + `","prior_session_id":"original-session"}`
			if inputs != wantInputs {
				t.Fatalf("inputs = %q, want %q", inputs, wantInputs)
			}

			// A lost owner response retried on either credential kind is idempotent: 200, no new rows.
			settled := fx.state(run)
			for _, client := range []string{lane.client, lane.otherClient} {
				retry := fx.resumeAs(client, run, resumeBody(lane.mode, episode, "narrow the scope"))
				var replayed struct {
					Replay bool `json:"replay"`
					Run    struct {
						Status string `json:"status"`
					} `json:"run"`
				}
				if retry.Code != http.StatusOK || json.Unmarshal(retry.Body.Bytes(), &replayed) != nil || !replayed.Replay || replayed.Run.Status != "queued" {
					t.Fatalf("lost-response retry via %s = %d %s, want 200 with replay=true", client, retry.Code, retry.Body.String())
				}
			}
			if fx.state(run) != settled {
				t.Fatal("retry mutated the resumed run or duplicated guidance or audit")
			}
			// Different guidance, or none, for the same resolved mode+episode is a 409 that records nothing.
			for _, g := range []string{"something else", ""} {
				diff := fx.resumeAs(lane.client, run, resumeBody(lane.mode, episode, g))
				if diff.Code != http.StatusConflict || !strings.Contains(diff.Body.String(), "already resumed with different guidance; the new guidance was not recorded") {
					t.Fatalf("different guidance %q = %d %s", g, diff.Code, diff.Body.String())
				}
				if fx.state(run) != settled {
					t.Fatalf("different guidance %q mutated the run", g)
				}
			}
			if n := fx.query("SELECT count(*) FROM run_user_inputs WHERE run_id=$1 AND kind='follow_up'", run); n != "1" {
				t.Fatalf("follow_up rows = %s, want exactly the original", n)
			}
			if wrong := fx.resumeAs(lane.client, run, resumeBody(lane.wrongMode, episode, "")); wrong.Code != http.StatusConflict || fx.state(run) != settled {
				t.Fatalf("unrecorded mode after resolution = %d", wrong.Code)
			}

			// A lost-ACK worker replay after Resume reopens and duplicates nothing.
			rep := fx.replay(run, 1, episode)
			var reply workersvc.PolicyHoldReply
			if rep.Code != http.StatusOK || json.Unmarshal(rep.Body.Bytes(), &reply) != nil || reply.Disposition != "already_applied" || fx.state(run) != settled {
				t.Fatalf("worker replay after resume = %d %s", rep.Code, rep.Body.String())
			}

			// The real claim: an attempt-path episode is ordinary afterwards; a preserving one is not.
			claimed := fx.claim(run)
			if claimed.ClaimGeneration != 2 || claimed.PolicyPreserveRequired != (lane.mode == "preserve") {
				t.Fatalf("claim after resume: generation=%d preserve=%v", claimed.ClaimGeneration, claimed.PolicyPreserveRequired)
			}
			if lane.mode == "preserve" {
				if !claimed.PolicyRequiredHead.Valid || claimed.PolicyRequiredHead.String != head {
					t.Fatalf("preserving claim lost the required head: %+v", claimed.PolicyRequiredHead)
				}
			} else if claimed.SessionID.Valid || !claimed.PolicyRequiredHead.Valid || claimed.PolicyRequiredHead.String != head {
				t.Fatalf("attempt-path claim must keep the saved-work head and no session: head=%v session=%v", claimed.PolicyRequiredHead, claimed.SessionID)
			}

			// A new actual refusal re-holds; the old episode can no longer be replayed or resumed.
			second := fx.hold(run, 2, lane.hold, false, strings.Repeat("b", 40))
			if second == episode {
				t.Fatal("fixture reused the episode")
			}
			refused("old episode against a new hold", lane.client, resumeBody(lane.mode, episode, "old consent"), http.StatusConflict)
			if again := fx.resumeAs(lane.client, run, resumeBody(lane.mode, second, "")); again.Code != http.StatusOK {
				t.Fatalf("new episode resume = %d %s", again.Code, again.Body.String())
			}
		})
	}
}

// The verification route is mounted beside policy-hold behind the same worker authentication and
// strict, 1 KiB bounded decoding.
func TestPolicyRecoveryVerifiedRouteLiveDB(t *testing.T) {
	fx := newCompactPolicyFx(t)
	run := fx.seedRun()
	head := strings.Repeat("a", 40)
	episode := fx.hold(run, 1, "same_conversation", false, head)
	if resp := fx.resumeAs("bearer", run, resumeBody("preserve", episode, "")); resp.Code != http.StatusOK {
		t.Fatalf("resume = %d %s", resp.Code, resp.Body.String())
	}
	fx.claim(run) // generation 2
	path := "/api/worker/runs/" + run.String() + "/policy-recovery-verified"
	post := func(token, body string) *httptest.ResponseRecorder {
		return bearerReqBody(fx.router, http.MethodPost, path, token, body)
	}
	valid := `{"claim_generation":2,"required_head":"` + head + `"}`
	if r := post("", valid); r.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated = %d", r.Code)
	}
	if r := post(fx.ownerUzc, valid); r.Code != http.StatusUnauthorized {
		t.Fatalf("owner CLI token on a worker route = %d", r.Code)
	}
	before := fx.state(run)
	for name, bad := range map[string]string{
		"unknown field":      `{"claim_generation":2,"required_head":"` + head + `","x":1}`,
		"trailing value":     valid + ` {}`,
		"missing generation": `{"required_head":"` + head + `"}`,
		"uppercase head":     `{"claim_generation":2,"required_head":"` + strings.ToUpper(head) + `"}`,
		"zero generation":    `{"claim_generation":0,"required_head":"` + head + `"}`,
		"over 1 KiB":         valid[:len(valid)-1] + `,"claim_generation":2}` + strings.Repeat(" ", 1100),
	} {
		if r := post(fx.workerToken, bad); r.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d %s", name, r.Code, r.Body.String())
		}
	}
	if fx.state(run) != before {
		t.Fatal("rejected verification bodies mutated the run")
	}
	// A stale generation is a typed pending 409; a wrong head is an authoritative refusal.
	stale := post(fx.workerToken, `{"claim_generation":1,"required_head":"`+head+`"}`)
	var reply workersvc.PolicyRecoveryVerifiedReply
	if stale.Code != http.StatusConflict || json.Unmarshal(stale.Body.Bytes(), &reply) != nil || reply.Disposition != "pending" || reply.Reason != "stale_claim" {
		t.Fatalf("stale = %d %s", stale.Code, stale.Body.String())
	}
	mismatch := post(fx.workerToken, `{"claim_generation":2,"required_head":"`+strings.Repeat("c", 40)+`"}`)
	if mismatch.Code != http.StatusOK || json.Unmarshal(mismatch.Body.Bytes(), &reply) != nil || reply.Disposition != "refused" || reply.Reason != "head_mismatch" {
		t.Fatalf("mismatch = %d %s", mismatch.Code, mismatch.Body.String())
	}
	if fx.state(run) != before {
		t.Fatal("refused verification mutated the run")
	}
	okResp := post(fx.workerToken, valid)
	if okResp.Code != http.StatusOK || json.Unmarshal(okResp.Body.Bytes(), &reply) != nil || reply.Disposition != "recorded" {
		t.Fatalf("recorded = %d %s", okResp.Code, okResp.Body.String())
	}
	// A published-latest re-hold in the verified generation supersedes R; the next claim carries it.
	newHead := strings.Repeat("d", 40)
	second := fx.hold(run, 2, "same_conversation", true, newHead)
	if got := fx.query("SELECT policy_required_head||'/'||policy_recovery_verified_head FROM runs WHERE id=$1", run); got != newHead+"/"+newHead {
		t.Fatalf("R/VH after verified supersede = %s", got)
	}
	if resp := fx.resumeAs("cookie", run, resumeBody("preserve", second, "")); resp.Code != http.StatusOK {
		t.Fatalf("second resume = %d %s", resp.Code, resp.Body.String())
	}
	if claimed := fx.claim(run); !claimed.PolicyRequiredHead.Valid || claimed.PolicyRequiredHead.String != newHead {
		t.Fatalf("next claim head = %+v, want %s", claimed.PolicyRequiredHead, newHead)
	}
}

// Set-token keeps its Codex refusal. On a run in an unresolved policy episode the same 422 only
// gains the pointer to the continuation that applies; nothing is written.
func TestSetTokenOnPolicyHeldCodexRunLiveDB(t *testing.T) {
	fx := newCompactPolicyFx(t)
	run := fx.seedRun()
	fx.hold(run, 1, "same_conversation", false, strings.Repeat("a", 40))
	hint := ". Resume this policy hold with POST /api/runs/" + run.String() + "/resume-now, an explicit mode and expected_episode_id."
	const base = "a credential override is not supported on the codex harness"
	overrides := `SELECT concat_ws('|',credential_override_mode,credential_override_secret_id::text) FROM runs WHERE id=$1`
	before, overridesBefore := fx.state(run), fx.query(overrides, run)
	for _, client := range []string{"cookie", "bearer"} {
		for _, body := range []string{`{"mode":"auto"}`, `{"mode":"inherit"}`, `{"mode":"default"}`} {
			path := "/api/runs/" + run.String() + "/credential"
			var rec *httptest.ResponseRecorder
			if client == "cookie" {
				rec = cookieReq(t, fx.router, http.MethodPost, path, fx.jwt, body)
			} else {
				rec = bearerReqBody(fx.router, http.MethodPost, path, fx.ownerUzc, body)
			}
			var msg struct {
				Error string `json:"error"`
			}
			if rec.Code != http.StatusUnprocessableEntity || json.Unmarshal(rec.Body.Bytes(), &msg) != nil || msg.Error != base+hint {
				t.Fatalf("set-token %s %s = %d %s", client, body, rec.Code, rec.Body.String())
			}
		}
	}
	if fx.state(run) != before || fx.query(overrides, run) != overridesBefore {
		t.Fatal("refused set-token changed the run row, an override column or the inputs")
	}
	// Once the episode is resolved the refusal is the unchanged Codex message with no pointer.
	episode := fx.query("SELECT policy_outcome_id::text FROM runs WHERE id=$1", run)
	if resp := fx.resumeAs("bearer", run, resumeBody("preserve", episode, "")); resp.Code != http.StatusOK {
		t.Fatalf("resume = %d %s", resp.Code, resp.Body.String())
	}
	rec := bearerReqBody(fx.router, http.MethodPost, "/api/runs/"+run.String()+"/credential", fx.ownerUzc, `{"mode":"auto"}`)
	var msg struct {
		Error string `json:"error"`
	}
	if rec.Code != http.StatusUnprocessableEntity || json.Unmarshal(rec.Body.Bytes(), &msg) != nil || msg.Error != base {
		t.Fatalf("resolved-episode set-token = %d %s", rec.Code, rec.Body.String())
	}
}
