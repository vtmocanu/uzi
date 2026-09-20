package handler

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// TestAdminListWorkersRollHealthLiveDB proves AdminListWorkers now folds the LEFT-JOINed roll
// signal (PRD #1484 M2): a stuck hosted worker owned by ANOTHER user surfaces on the cross-user
// admin fleet list as upgrade_status=upgrade_failed carrying its blocking container and reason,
// exactly as GET /api/workers does for the owner. Driven through the REAL chi router so the
// mounted RequireUser + RequireAdminRO chain and the whole ListAllWorkers → DTO path run, which
// a fake-client test cannot exercise.
//
// It FAILS on the pre-fix handler: that built the admin DTOs via workerDTOFromWorker (Signal:
// nil), so a hosted worker classified by a bare version compare (here `unknown`, cliLiveDB
// leaving the control-plane version unstamped) and the upgrade_blocking_* fields were always
// null. The assertions below therefore change red→green with the population fix.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.
func TestAdminListWorkersRollHealthLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)

	admin := cliSeedUser(t, pool, true)
	owner := cliSeedUser(t, pool, false) // a DIFFERENT user owns the stuck worker
	adminUza := cliMintToken(t, pool, admin, clitoken.ScopeAdminRO)

	// A hosted worker owned by `owner`. hosted_size + template_declared are required by the
	// ck_workers_hosted_metadata CHECK; token_hash is UNIQUE across the shared LiveDB, so derive
	// it from a fresh uuid rather than a literal.
	workerID := uuid.New()
	tokenHash := uuid.New()
	cliMustExec(t, pool,
		`INSERT INTO workers (id, user_id, name, token_hash, status, kind, hosted_size, template_declared, version, last_heartbeat_at)
		 VALUES ($1, $2, 'stuck-hosted', $3, 'online', 'hosted', 'm', 'base', '0.83.0', now())`,
		workerID, owner, tokenHash[:])
	// A FRESH (observed_at = now()) stuck roll report ⇒ R1 (hosted + fresh + phase=stuck) ⇒
	// upgrade_failed, with the blocking container/reason surfaced on the DTO.
	cliMustExec(t, pool,
		`INSERT INTO worker_upgrade_reports
		   (worker_id, phase, controller_reported_at, observed_at, blocking_container, blocking_reason, last_exit_code, restart_count)
		 VALUES ($1, 'stuck', now(), now(), 'seed-nix', 'ImagePullBackOff', 2, 3)`,
		workerID)

	rec := bearerReq(router, http.MethodGet, "/api/admin/workers", adminUza)
	if rec.Code != http.StatusOK {
		t.Fatalf("uza_ GET /api/admin/workers = %d, want 200\nbody: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Workers []struct {
			ID                       string  `json:"id"`
			Kind                     string  `json:"kind"`
			UpgradeStatus            string  `json:"upgrade_status"`
			UpgradeBlockingContainer *string `json:"upgrade_blocking_container"`
			UpgradeBlockingReason    *string `json:"upgrade_blocking_reason"`
			UpgradeLastExitCode      *int32  `json:"upgrade_last_exit_code"`
		} `json:"workers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode admin workers: %v (body %s)", err, rec.Body.String())
	}

	var found bool
	for _, w := range body.Workers {
		if w.ID != workerID.String() {
			continue
		}
		found = true
		if w.UpgradeStatus != workersvc.UpgradeStatusUpgradeFailed {
			t.Fatalf("admin worker upgrade_status = %q, want %q — the roll signal must be folded; the pre-fix Signal:nil path read a bare version compare",
				w.UpgradeStatus, workersvc.UpgradeStatusUpgradeFailed)
		}
		if w.UpgradeBlockingContainer == nil || *w.UpgradeBlockingContainer != "seed-nix" {
			t.Errorf("upgrade_blocking_container = %v, want \"seed-nix\" (populated only when the roll signal is folded)", w.UpgradeBlockingContainer)
		}
		if w.UpgradeBlockingReason == nil || *w.UpgradeBlockingReason != "ImagePullBackOff" {
			t.Errorf("upgrade_blocking_reason = %v, want \"ImagePullBackOff\"", w.UpgradeBlockingReason)
		}
		if w.UpgradeLastExitCode == nil || *w.UpgradeLastExitCode != 2 {
			t.Errorf("upgrade_last_exit_code = %v, want 2", w.UpgradeLastExitCode)
		}
	}
	if !found {
		t.Fatalf("stuck hosted worker %s absent from the admin fleet list", workerID)
	}
}
