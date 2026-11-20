package handler

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// PRD #4416 M2: RunDTO.injected_env names what the run's latest claim delivered, for the owner only,
// from receipts (no value is opened). Skipped without UZI_TEST_DATABASE_URL.
func TestRunInjectedEnvOwnerOnlyLiveDB(t *testing.T) {
	h, router, pool, owner, lead, _ := summaryLiveFixture(t)
	receipt := func(name, scope string, secret bool, gen int64, last int64) {
		t.Helper()
		cliMustExec(t, pool, `INSERT INTO run_env_receipts (run_id, user_id, var_id, rev, scope, name, secret, first_claim_generation, last_claim_generation)
VALUES ($1,$2,$3,1,$4,$5,$6,$7,$8)`, lead, owner, uuid.New(), scope, name, secret, gen, last)
	}
	// Generation 2 is current. A secret and a plain variable were delivered by it; a version last
	// delivered by generation 1 only (rotated away since) is not listed.
	cliMustExec(t, pool, `UPDATE runs SET claim_generation = 2 WHERE id = $1`, lead)
	receipt("API_URL", "repo", false, 1, 2)
	receipt("API_KEY", "repo", true, 1, 2)
	receipt("OLD_KEY", "repo", true, 1, 1)

	get := func(userID uuid.UUID) (int, apitypes.RunDTO, string) {
		t.Helper()
		rec := cookieReq(t, router, http.MethodGet, "/api/runs/"+lead.String(), cliMintJWT(t, pool, userID), "")
		var resp struct {
			Run apitypes.RunDTO `json:"run"`
		}
		if rec.Code == http.StatusOK {
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
		}
		return rec.Code, resp.Run, rec.Body.String()
	}

	code, dto, body := get(owner)
	if code != http.StatusOK {
		t.Fatalf("owner GET = %d %s", code, body)
	}
	want := []apitypes.InjectedEnvDTO{{Name: "API_KEY", Scope: "repo", Secret: true}, {Name: "API_URL", Scope: "repo", Secret: false}}
	if len(dto.InjectedEnv) != len(want) {
		t.Fatalf("injected_env = %+v, want %+v", dto.InjectedEnv, want)
	}
	for i := range want {
		if dto.InjectedEnv[i] != want[i] {
			t.Fatalf("injected_env[%d] = %+v, want %+v", i, dto.InjectedEnv[i], want[i])
		}
	}
	if strings.Contains(body, "OLD_KEY") {
		t.Fatalf("a version the latest claim did not deliver is listed: %s", body)
	}

	// An admin may read another user's run but is not its owner: no injected_env at all.
	admin := cliSeedUser(t, pool, true)
	t.Cleanup(func() { cliMustExec(t, pool, `DELETE FROM users WHERE id=$1`, admin) })
	code, _, body = get(admin)
	if code != http.StatusOK {
		t.Fatalf("admin GET = %d %s", code, body)
	}
	if strings.Contains(body, "injected_env") || strings.Contains(body, "API_KEY") {
		t.Fatalf("an admin viewer sees injected names: %s", body)
	}
	// The helper itself refuses a non-owner viewer, independent of the route's viewer gate.
	run, err := h.wsvc.GetRunForViewer(t.Context(), owner, false, lead)
	if err != nil {
		t.Fatal(err)
	}
	var direct apitypes.RunDTO
	h.overlayInjectedEnv(t.Context(), admin, run, &direct)
	if direct.InjectedEnv != nil {
		t.Fatalf("overlayInjectedEnv filled a non-owner's view: %+v", direct.InjectedEnv)
	}

	// A run whose claim delivered nothing carries no field (omitempty), and a never-claimed run too.
	cliMustExec(t, pool, `UPDATE runs SET claim_generation = 3 WHERE id = $1`, lead)
	code, _, body = get(owner)
	if code != http.StatusOK || strings.Contains(body, "injected_env") {
		t.Fatalf("a later claim that delivered nothing: %d %s", code, body)
	}
}
