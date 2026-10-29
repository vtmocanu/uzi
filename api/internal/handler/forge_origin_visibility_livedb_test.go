package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestForgeOriginVisibilityRoutesLiveDB(t *testing.T) {
	h, router, pool := cliLiveDB(t)
	h.wsvc.SetTxBeginner(pool)
	owner := cliSeedUser(t, pool, false)
	foreign := cliSeedUser(t, pool, true)
	conn := rmSeedConn(t, pool, owner)
	repo := rmSeedRepo(t, pool, conn, 358501, true)
	// Only the canonical authority may leave the API, even if the stored repo URL
	// contains userinfo, a path and a query.
	cliMustExec(t, pool, "UPDATE repos SET web_url=$2 WHERE id=$1", repo, "https://user:fixture-password@Forge.Example:443/private/repo?secret=fixture")
	run := rmSeedRun(t, pool, owner, repo, "running")
	worker, incarnation := uuid.New(), uuid.New()
	cliMustExec(t, pool, "INSERT INTO workers(id,user_id,name,token_hash,status,protocol_capabilities) VALUES($1,$2,'visibility',$3,'online',$4)",
		worker, owner, []byte(uuid.NewString()), []string{capability.ForgeCloneSuccessV1})
	cliMustExec(t, pool, "UPDATE runs SET worker_id=$2,claim_generation=1 WHERE id=$1", run, worker)
	until := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
	cliMustExec(t, pool, "INSERT INTO forge_origin_cooldowns(user_id,worker_id,origin,cooled_until,last_reason,observed_at,incarnation) VALUES($1,$2,'https://forge.example',$3,'transient_clone_fetch_exhausted',now(),$4)", owner, worker, until, incarnation)
	jwt := cliMintJWT(t, pool, owner)
	foreignJWT := cliMintJWT(t, pool, foreign)
	read := func(path, credential string) []byte {
		t.Helper()
		rec := cookieReq(t, router, http.MethodGet, path, credential, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: status %d: %s", path, rec.Code, rec.Body.String())
		}
		return rec.Body.Bytes()
	}
	check := func(state string, halfOpen bool) {
		t.Helper()
		for _, path := range []string{"/api/workers", "/api/runs/" + run.String(), "/api/runs", "/api/runs?view=summary"} {
			raw := read(path, jwt)
			var body struct {
				Workers []apitypes.WorkerDTO      `json:"workers"`
				Run     apitypes.RunDTO           `json:"run"`
				Runs    []apitypes.RunListItemDTO `json:"runs"`
			}
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatal(err)
			}
			var got *apitypes.ForgeOriginCooldownDTO
			switch {
			case path == "/api/workers":
				for _, w := range body.Workers {
					if w.ID == worker.String() && len(w.ForgeOriginCooldowns) == 1 {
						got = &w.ForgeOriginCooldowns[0]
					}
				}
			case strings.HasPrefix(path, "/api/runs/"):
				got = body.Run.ForgeOriginCooldown
			default:
				for _, r := range body.Runs {
					if r.ID == run.String() {
						got = r.ForgeOriginCooldown
					}
				}
			}
			if got == nil || got.Origin != "https://forge.example" || got.State != state || !got.CooledUntil.Equal(until) {
				t.Fatalf("%s projection: %+v want state=%s until=%v", path, got, state, until)
			}
			if halfOpen {
				if got.HalfOpenRunID == nil || *got.HalfOpenRunID != run.String() {
					t.Fatalf("%s missing exact probe: %+v", path, got)
				}
			} else if got.HalfOpenRunID != nil {
				t.Fatalf("%s stale probe: %+v", path, got)
			}
			for _, forbidden := range []string{"fixture-password", "/private/repo", "secret=fixture", "transient_clone_fetch_exhausted"} {
				// Other existing DTO fields may contain repo URLs. Check only the new projection.
				b, err := json.Marshal(got)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(b), forbidden) {
					t.Fatalf("%s leaked %s", path, forbidden)
				}
			}
		}
	}
	check("cooling", false)
	until = until.Add(-2 * time.Hour)
	cliMustExec(t, pool, "UPDATE forge_origin_cooldowns SET cooled_until=$2 WHERE worker_id=$1", worker, until)
	check("retry_eligible_unverified", false)
	cliMustExec(t, pool, "UPDATE forge_origin_cooldowns SET reservation_run_id=$2,reservation_generation=1,reservation_incarnation=incarnation,reservation_version=7,reservation_observation_version=observation_version WHERE worker_id=$1", worker, run)
	cliMustExec(t, pool, "UPDATE run_forge_origins SET reservation_incarnation=$2,reservation_version=7 WHERE run_id=$1", run, incarnation)
	check("half_open", true)
	// Cordon responses use the single-worker overlay rather than the list loop.
	token := cliMintToken(t, pool, owner, clitoken.ScopeUser)
	cordonResponse := func(rec *httptest.ResponseRecorder) apitypes.WorkerDTO {
		t.Helper()
		var body struct {
			Worker apitypes.WorkerDTO `json:"worker"`
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("cordon status=%d body=%s", rec.Code, rec.Body.String())
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body.Worker
	}
	cordoned := cordonResponse(bearerReqBody(router, http.MethodPost, "/api/workers/"+worker.String()+"/cordon", token, `{"reason":"visibility test"}`))
	if len(cordoned.ForgeOriginCooldowns) != 1 || cordoned.ForgeOriginCooldowns[0].State != "half_open" {
		t.Fatal("single-worker response omitted cooldown")
	}
	cordonResponse(bearerReq(router, http.MethodDelete, "/api/workers/"+worker.String()+"/cordon", token))
	cliMustExec(t, pool, "UPDATE workers SET protocol_capabilities=ARRAY[]::text[] WHERE id=$1", worker)
	check("legacy_serialized_until_upgrade", true)
	// Even an administrator cannot read another owner's cooldown through an overlay.
	ctx := mw.ContextWithUser(t.Context(), store.User{ID: foreign, IsAdmin: true})
	d := apitypes.RunDTO{ID: run.String()}
	h.overlayRunForgeOriginCooldowns(ctx, &d)
	if d.ForgeOriginCooldown != nil || len(h.workerForgeOriginCooldowns(ctx, []uuid.UUID{worker})) != 0 {
		t.Fatal("foreign administrator overlay leaked cooldown")
	}
	if strings.Contains(string(read("/api/workers", foreignJWT)), "https://forge.example") ||
		strings.Contains(string(read("/api/runs?view=summary", foreignJWT)), "https://forge.example") {
		t.Fatal("foreign list leaked cooldown")
	}
	// A stale mapping must not expose a probe ID.
	cliMustExec(t, pool, "UPDATE run_forge_origins SET generation=2 WHERE run_id=$1", run)
	check("reserved_unverified", false)
	// Per-worker output stays bounded and ordered, without dropping this earliest row.
	for i := range 33 {
		extra := rmSeedRepo(t, pool, conn, int64(358510+i), true)
		origin := fmt.Sprintf("https://extra-%02d.example", i)
		cliMustExec(t, pool, "UPDATE repos SET web_url=$2 WHERE id=$1", extra, origin+"/repo")
		cliMustExec(t, pool, "INSERT INTO forge_origin_cooldowns(user_id,worker_id,origin,cooled_until,last_reason,observed_at) VALUES($1,$2,$3,$4,'transient_clone_fetch_exhausted',now())", owner, worker, origin, until.Add(time.Hour))
	}
	ownerCtx := mw.ContextWithUser(t.Context(), store.User{ID: owner})
	bounded := h.workerForgeOriginCooldowns(ownerCtx, []uuid.UUID{worker})[worker]
	if len(bounded) != 32 || bounded[0].Origin != "https://forge.example" || bounded[31].Origin != "https://extra-30.example" {
		t.Fatal("worker origins are not bounded in deterministic deadline/origin order")
	}
	cliMustExec(t, pool, "DELETE FROM forge_origin_cooldowns WHERE worker_id=$1 AND origin <> 'https://forge.example'", worker)
	// Canonical-origin changes prune the old key; no stale origin survives.
	cliMustExec(t, pool, "UPDATE repos SET web_url='https://replacement.example/repo' WHERE id=$1", repo)
	ctx = mw.ContextWithUser(t.Context(), store.User{ID: owner})
	h.overlayRunForgeOriginCooldowns(ctx, &d)
	if d.ForgeOriginCooldown != nil || len(h.workerForgeOriginCooldowns(ctx, []uuid.UUID{worker})) != 0 {
		t.Fatal("obsolete origin survived")
	}
}

func TestForgeOriginVisibilityMissingStoreFallback(t *testing.T) {
	h := &Handler{}
	ctx := mw.ContextWithUser(t.Context(), store.User{ID: uuid.New()})
	d := apitypes.RunDTO{ID: uuid.NewString()}
	h.overlayRunForgeOriginCooldowns(ctx, &d)
	if d.ForgeOriginCooldown != nil || h.workerForgeOriginCooldowns(ctx, []uuid.UUID{uuid.New()}) != nil {
		t.Fatal("missing store fabricated recovery")
	}
}
