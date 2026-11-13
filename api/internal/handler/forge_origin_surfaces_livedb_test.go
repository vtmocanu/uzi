package handler

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #1324 D9: the owner overlays show a row for the API origin as well as the Git origin, and
// say which evidence surfaces are still unverified. A run shows the row with the later deadline.
func TestForgeOriginSurfacesOverlayLiveDB(t *testing.T) {
	h, _, pool := cliLiveDB(t)
	for _, forgeType := range []string{"github", "gitlab"} {
		t.Run(forgeType, func(t *testing.T) {
			owner := cliSeedUser(t, pool, false)
			conn := rmSeedConn(t, pool, owner)
			cliMustExec(t, pool, "UPDATE forge_connections SET forge_type=$2 WHERE id=$1", conn, forgeType)
			repo := rmSeedRepo(t, pool, conn, 132401, true)
			cliMustExec(t, pool, "UPDATE repos SET web_url='https://forge.example/group/repo' WHERE id=$1", repo)
			run := rmSeedRun(t, pool, owner, repo, "running")
			worker := uuid.New()
			cliMustExec(t, pool, "INSERT INTO workers(id,user_id,name,token_hash,status) VALUES($1,$2,'surfaces',$3,'online')",
				worker, owner, []byte(uuid.NewString()))
			cliMustExec(t, pool, "UPDATE runs SET worker_id=$2,claim_generation=1 WHERE id=$1", run, worker)
			insert := func(origin string, hours int, git, api bool) {
				t.Helper()
				cliMustExec(t, pool, `INSERT INTO forge_origin_cooldowns(user_id,worker_id,origin,cooled_until,last_reason,observed_at,git_unverified,api_unverified)
					VALUES($1,$2,$3,now()+make_interval(hours => $4),'transient_publication_exhausted',now(),$5,$6)`, owner, worker, origin, hours, git, api)
			}
			ctx := mw.ContextWithUser(t.Context(), store.User{ID: owner})
			d := apitypes.RunDTO{ID: run.String()}
			if forgeType == "gitlab" {
				// One host serves both surfaces: one row carrying both flags.
				insert("https://forge.example", 1, true, true)
				h.overlayRunForgeOriginCooldowns(ctx, &d)
				rows := h.workerForgeOriginCooldowns(ctx, []uuid.UUID{worker})[worker]
				if d.ForgeOriginCooldown == nil || len(rows) != 1 || strings.Join(rows[0].Surfaces, ",") != "git,api" ||
					strings.Join(d.ForgeOriginCooldown.Surfaces, ",") != "git,api" {
					t.Fatalf("shared row: run=%+v worker rows=%+v", d.ForgeOriginCooldown, rows)
				}
				return
			}
			// Use github.com for the distinct Git/API-origin case; GHES shares one origin (it
			// serves its API under /api/v3 on the web host), so it has one row like GitLab.
			cliMustExec(t, pool, "UPDATE repos SET web_url='https://github.com/group/repo' WHERE id=$1", repo)
			insert("https://github.com", 1, true, false)
			insert("https://api.github.com", 2, false, true)
			h.overlayRunForgeOriginCooldowns(ctx, &d)
			rows := h.workerForgeOriginCooldowns(ctx, []uuid.UUID{worker})[worker]
			got := map[string]string{}
			for _, r := range rows {
				got[r.Origin] = strings.Join(r.Surfaces, ",")
			}
			if len(rows) != 2 || got["https://github.com"] != "git" || got["https://api.github.com"] != "api" {
				t.Fatalf("worker rows: %+v", rows)
			}
			if d.ForgeOriginCooldown == nil || d.ForgeOriginCooldown.Origin != "https://api.github.com" ||
				strings.Join(d.ForgeOriginCooldown.Surfaces, ",") != "api" {
				t.Fatalf("run projection should be the later-deadline API row: %+v", d.ForgeOriginCooldown)
			}
			// A row for an origin the repo no longer authorizes is not shown.
			cliMustExec(t, pool, "DELETE FROM forge_origin_cooldowns WHERE worker_id=$1 AND origin='https://github.com'", worker)
			cliMustExec(t, pool, "UPDATE repos SET web_url='https://elsewhere.example/group/repo' WHERE id=$1", repo)
			cliMustExec(t, pool, "INSERT INTO forge_origin_cooldowns(user_id,worker_id,origin,cooled_until,last_reason,observed_at,git_unverified,api_unverified) VALUES($1,$2,'https://api.stale.example',$3,'transient_publication_exhausted',now(),false,true)",
				owner, worker, time.Now().Add(time.Hour))
			d = apitypes.RunDTO{ID: run.String()}
			h.overlayRunForgeOriginCooldowns(ctx, &d)
			if d.ForgeOriginCooldown != nil {
				t.Fatalf("unauthorized origin shown: %+v", d.ForgeOriginCooldown)
			}
		})
	}
}
