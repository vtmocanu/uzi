package handler

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestAccountCooldownReadersLiveDB(t *testing.T) {
	db := ephemeralPriorityDatabase(t)
	for _, mode := range []string{"expired", "source changed", "credential lost", "worker offline"} {
		t.Run(mode, func(t *testing.T) {
			fx := priorityFixture(t, db)
			alias := fx.seedEphemeralAdmissionAlias()
			id := fx.queuedRun([]string{})
			fx.bindEphemeralAdmission(id, alias)
			fx.holdEphemeralAdmission(alias)
			worker := fx.onlineWorker("codex", false)
			exec := func(sql string, args ...any) {
				t.Helper()
				if _, err := fx.pool.Exec(fx.ctx, sql, args...); err != nil {
					t.Fatal(err)
				}
			}
			exec(`UPDATE runs r SET status='running',worker_id=$2,claim_generation=1,
    health='waiting_account_cooldown',health_reason=$3,
    codex_account_revision=a.credential_revision,
    codex_account_key=jsonb_build_array(a.provider_user_id,a.workspace_account_id)::text
    FROM codex_credential_state c JOIN codex_provider_account a ON a.id=c.provider_account_id
    WHERE r.id=$1 AND c.user_secret_id=r.codex_secret_id`, id, worker, apitypes.AccountCooldownReason)
			exec(`INSERT INTO worker_active_runs(worker_id,run_id,claim_generation,phase,snapshot_epoch,reported_at,
    cooldown_operation,cooldown_class,cooldown_episode_id,cooldown_eligible_at,cooldown_observed_at,cooldown_until)
    SELECT $2,$1,1,'running',1,now(),'{"kind":"root"}'::jsonb,'rate_limit',episode_id,effective_expiry,now(),now()+interval '45 seconds'
    FROM codex_account_admission_throttle WHERE user_id=$3`, id, worker, fx.userID)
			h := &Handler{q: fx.q}
			ctx := mw.ContextWithUser(fx.ctx, store.User{ID: fx.userID})
			read := func() apitypes.RunDTO {
				t.Helper()
				r, err := fx.q.GetRunByID(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				dto := runToDTO(r, "", 0, 0, 0, time.Now(), 0)
				applyRevising(&dto, false)
				h.overlayQueueReasons(ctx, &dto)
				return dto
			}
			if dto := read(); dto.Health != "waiting_account_cooldown" || dto.Progress == nil || dto.Progress.State != "waiting" {
				t.Fatalf("positive control: %+v", dto)
			}
			switch mode {
			case "expired":
				exec("UPDATE worker_active_runs SET cooldown_observed_at=now()-interval '46 seconds',cooldown_until=now()-interval '1 second' WHERE run_id=$1", id)
			case "source changed":
				exec("UPDATE runs SET gate_revision=gate_revision+1,gate_presentation_id=$2 WHERE id=$1", id, uuid.New())
				exec(`INSERT INTO run_user_inputs(run_id,kind,body,gate_binding,gate_revision,consumed_at,consumed_worker_id,consumed_claim_generation)
     SELECT id,'revise_plan','revise','bound',gate_revision,now(),worker_id,claim_generation FROM runs WHERE id=$1`, id)
			case "credential lost":
				exec("UPDATE user_secrets SET disabled_at=now() WHERE id=$1", alias)
			case "worker offline":
				exec("UPDATE workers SET status='offline' WHERE id=$1", worker)
			}
			dto := read()
			if dto.Health != "ok" || dto.HealthReason != nil || dto.AccountCooldownClass != nil || dto.Progress == nil || dto.Progress.State == "waiting" {
				t.Fatalf("%s stale detail/list seed: %+v", mode, dto)
			}
			summary := apitypes.RunSummaryOf(apitypes.RunListItemDTO{RunDTO: dto})
			if summary.Health != "ok" || summary.HealthReason != nil || summary.Progress.State == "waiting" {
				t.Fatalf("stale summary: %+v", summary)
			}
			rows, err := fx.q.ListLatestRunsForRepo(ctx, fx.repoID)
			if err != nil || len(rows) != 1 {
				t.Fatalf("board rows=%d: %v", len(rows), err)
			}
			r, err := fx.q.GetRunByID(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			single, err := fx.q.GetLatestRunForIssue(ctx, store.GetLatestRunForIssueParams{RepoID: fx.repoID, IssueIid: r.IssueIid})
			if err != nil {
				t.Fatal(err)
			}
			if single.AccountCooldownClass.Valid || single.AccountCooldownEligibleAt.Valid {
				t.Fatal("single board projection survived invalid authority")
			}
			cards := assembleCards([]store.Issue{{ForgeIssueIid: r.IssueIid.Int64}}, rows, nil, nil, nil, fx.userID, "github", nil, 0)
			board := cards[0].LatestRun
			if board.Health != "ok" || board.HealthReason != nil || board.AccountCooldownClass != nil {
				t.Fatalf("stale board: %+v", board)
			}
			foreign := runToDTO(r, "", 0, 0, 0, time.Now(), 0)
			h.overlayQueueReasons(mw.ContextWithUser(ctx, store.User{ID: uuid.New(), IsAdmin: true}), &foreign)
			if foreign.HealthReason != nil || strings.Contains(foreign.Health, "cooldown") {
				t.Fatal("private health survived foreign read")
			}
			// Typed null evidence is the same shape used by both board paths.
			board.overlayAccountCooldown(pgconv.TextOrNull("rate_limit"), pgconv.Time(time.Now().Add(-time.Second)))
			if board.AccountCooldownClass != nil {
				t.Fatal("expired read-time evidence survived")
			}
		})
	}
}
