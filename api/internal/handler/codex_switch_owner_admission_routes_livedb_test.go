package handler

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/clitoken"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
)

// TestWorkerCodexOwnerRequestAdmissionRefusalsLiveDB drives every refusal of
// the live Codex credential switch request through the real authenticated
// owner route (POST /api/runs/{id}/credential). Each subtest starts from a
// fresh fixture with X withdrawn, sets exactly one refusing condition, and
// asserts the exact status, a message substring, the precondition that was
// set, and that nothing the snapshot covers changed (pending fields stay NULL).
func TestWorkerCodexOwnerRequestAdmissionRefusalsLiveDB(t *testing.T) {
	const (
		msgIneligible  = "codex credentials can be switched only on an active, worker-held run"
		msgAssembling  = "the claim is being assembled; retry in a moment"
		msgRaced       = "the run changed state while switching its token; retry"
		msgTargetGone  = "credential override token not found"
		msgRunNotFound = "run not found"
	)

	// setup builds a fresh fixture, withdraws X and sets the run status.
	// It returns the env and the fixture's Codex target B.
	setup := func(t *testing.T, status string) (*applyRouteEnv, uuid.UUID) {
		t.Helper()
		e, w := setupStoppedGuardsRouteWithSeed(t, true)
		var target uuid.UUID
		if err := e.pool.QueryRow(context.Background(),
			`SELECT codex_switch_target_secret_id FROM runs WHERE id=$1`, e.run).Scan(&target); err != nil {
			t.Fatal(err)
		}
		if target == uuid.Nil || target == e.secret {
			t.Fatal("fixture requires a Codex target B distinct from the Anthropic override")
		}
		withdrawStoppedRoute(t, e, w)
		stoppedRouteExec(t, e, "UPDATE runs SET status=$2 WHERE id=$1", e.run, status)
		return e, target
	}

	// insertAlias creates a Codex alias secret and its state row for owner.
	insertAlias := func(t *testing.T, e *applyRouteEnv, owner uuid.UUID, kind, stateStatus string) uuid.UUID {
		t.Helper()
		id := uuid.New()
		sealed, err := e.handler.box.Seal([]byte("harmless-admission-fixture-" + id.String()))
		if err != nil {
			t.Fatal(err)
		}
		stoppedRouteExec(t, e, `INSERT INTO user_secrets(id,user_id,kind,label,ciphertext,sealed_with)
 VALUES($1,$2,$3,$4,$5,'master')`, id, owner, kind, "admission-"+id.String(), sealed)
		stoppedRouteExec(t, e, `INSERT INTO codex_credential_state(user_secret_id,user_id,status)
 VALUES($1,$2,$3)`, id, owner, stateStatus)
		return id
	}

	// refuse checks the run status precondition, sends the owner request as
	// requester, and asserts the refusal plus an unchanged snapshot.
	refuse := func(t *testing.T, e *applyRouteEnv, wantRunStatus string, requester, secret uuid.UUID, wantCode int, wantMsg string) {
		t.Helper()
		var gotStatus string
		var pendingSet bool
		if err := e.pool.QueryRow(context.Background(), `SELECT status,
 credential_switch_requested_at IS NOT NULL OR credential_switch_generation IS NOT NULL
 OR codex_switch_target_secret_id IS NOT NULL OR codex_switch_target_op_id IS NOT NULL
 FROM runs WHERE id=$1`, e.run).Scan(&gotStatus, &pendingSet); err != nil {
			t.Fatal(err)
		}
		if gotStatus != wantRunStatus {
			t.Fatalf("precondition: run status=%q, want %q", gotStatus, wantRunStatus)
		}
		if pendingSet {
			t.Fatal("precondition: pending switch fields must start NULL")
		}
		lim := mw.NewLimiter(100000, time.Minute, nil)
		router := e.handler.Routes(lim, lim, lim, lim, lim, lim, lim, lim, lim, lim, lim)
		token := cliMintToken(t, e.pool, requester, clitoken.ScopeUser)
		before := stoppedRouteSnapshot(t, e)
		rec := bearerReqBody(router, http.MethodPost, "/api/runs/"+e.run.String()+"/credential", token,
			fmt.Sprintf(`{"mode":"pinned","secret_id":%q}`, secret))
		if rec.Code != wantCode || !strings.Contains(rec.Body.String(), wantMsg) {
			t.Fatalf("status=%d body=%q, want %d containing %q", rec.Code, rec.Body.String(), wantCode, wantMsg)
		}
		after := stoppedRouteSnapshot(t, e)
		for key, want := range before {
			if !reflect.DeepEqual(after[key], want) {
				t.Fatalf("refused request changed snapshot section %s", key)
			}
		}
		if len(before) != len(after) {
			t.Fatal("snapshot section count changed")
		}
		if err := e.pool.QueryRow(context.Background(), `SELECT
 credential_switch_requested_at IS NOT NULL OR credential_switch_generation IS NOT NULL
 OR codex_switch_target_secret_id IS NOT NULL OR codex_switch_target_op_id IS NOT NULL
 FROM runs WHERE id=$1`, e.run).Scan(&pendingSet); err != nil {
			t.Fatal(err)
		}
		if pendingSet {
			t.Fatal("refused request left pending switch fields set")
		}
	}

	// Guard isolated: requestCodexCredentialSwitch's applyLiveStatus check
	// (ErrCodexSwitchIneligible, 422). Target B is valid in every case.
	for _, status := range []string{"queued", "paused", "limit_wait", "recovery_wait"} {
		t.Run("ineligible_"+status, func(t *testing.T) {
			e, target := setup(t, status)
			refuse(t, e, status, e.user, target, http.StatusUnprocessableEntity, msgIneligible)
		})
	}

	// Guard isolated: the observed.Status == "claimed" check
	// (ErrCredentialSwitchClaimAssembling, 409).
	t.Run("claim_assembling_claimed", func(t *testing.T) {
		e, target := setup(t, "claimed")
		refuse(t, e, "claimed", e.user, target, http.StatusConflict, msgAssembling)
	})

	// Redundant fence (combined): e.secret is an anthropic_token with no
	// codex_credential_state row, so LockCodexAliasForShareNowait fails, and
	// codexSwitchTargetUsable would refuse the anthropic kind as well; either
	// alone maps to ErrCredentialSwitchRaced (409).
	t.Run("anthropic_target_running", func(t *testing.T) {
		e, _ := setup(t, "running")
		refuse(t, e, "running", e.user, e.secret, http.StatusConflict, msgRaced)
	})

	// Guard isolated: codexSwitchTargetUsable alone. Enablement, secret meta and
	// the alias lock all succeed because the rows exist, are owned by the
	// requester and are enabled; only the kind/status/link combination is
	// unusable, so removing that predicate would let each request through.
	unusable := []struct {
		name, kind, status string
		useFixtureTarget   bool
	}{
		// openai_api_key requires status static; failed breaks the first disjunct.
		{"openai_api_key_failed", "openai_api_key", "failed", true},
		// codex_auth requires status linked (and a linked account); each
		// non-linked status breaks the second disjunct. Neither has a
		// provider account, so the link term is false as well.
		{"codex_auth_staging", "codex_auth", "staging", false},
		{"codex_auth_failed", "codex_auth", "failed", false},
		// A codex_auth alias with the static status is also unusable.
		{"codex_auth_static", "codex_auth", "static", false},
		// status linked but no provider account: the linked term alone refuses.
		{"codex_auth_linked_without_account", "codex_auth", "linked", false},
	}
	for _, c := range unusable {
		t.Run("unusable_target_"+c.name, func(t *testing.T) {
			e, target := setup(t, "running")
			if c.useFixtureTarget {
				stoppedRouteExec(t, e, `UPDATE codex_credential_state SET status=$2 WHERE user_secret_id=$1`, target, c.status)
			} else {
				target = insertAlias(t, e, e.user, c.kind, c.status)
			}
			refuse(t, e, "running", e.user, target, http.StatusConflict, msgRaced)
		})
	}

	// Guard isolated: the request-side disabled term. Target B stays a usable,
	// owned static API key; only its user_secrets.disabled_at is set, so only
	// `disabled` in the target condition refuses it (ErrCredentialSwitchRaced).
	t.Run("disabled_target_running", func(t *testing.T) {
		e, target := setup(t, "running")
		stoppedRouteExec(t, e, `UPDATE user_secrets SET disabled_at=now() WHERE id=$1`, target)
		refuse(t, e, "running", e.user, target, http.StatusConflict, msgRaced)
	})

	// Redundant fence (combined): LockSecretEnablementForShareNowait,
	// GetUserSecretMetaByID and LockCodexAliasForShareNowait are each
	// owner-scoped, so another user's otherwise usable API-key secret is refused
	// unless all three are removed (404 from the first two reads, 409 from the
	// alias lock alone).
	t.Run("other_users_secret_running", func(t *testing.T) {
		e, _ := setup(t, "running")
		other := uuid.New()
		stoppedRouteExec(t, e, `INSERT INTO users (id, email, password_hash, vault_mode) VALUES ($1, $2, 'x', 'unprotected')`,
			other, fmt.Sprintf("admission-other-%s@e2e", other))
		foreign := insertAlias(t, e, other, "openai_api_key", "static")
		refuse(t, e, "running", e.user, foreign, http.StatusNotFound, msgTargetGone)
	})

	// Guard isolated: the owner-scoped run read in SetRunCredential
	// (ErrRunNotFound, 404) for a different user's token on this run, with a
	// target that would be valid for the owner.
	t.Run("other_users_token_running", func(t *testing.T) {
		e, target := setup(t, "running")
		other := uuid.New()
		stoppedRouteExec(t, e, `INSERT INTO users (id, email, password_hash, vault_mode) VALUES ($1, $2, 'x', 'unprotected')`,
			other, fmt.Sprintf("admission-other-%s@e2e", other))
		refuse(t, e, "running", other, target, http.StatusNotFound, msgRunNotFound)
	})
}
