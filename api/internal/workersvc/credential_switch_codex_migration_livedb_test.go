package workersvc

import (
	"testing"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// The integrated default branch is at 495 (follow-up finalize admission). Only this
// fixture-owned database is downgraded; it must retain the default-branch
// columns while Codex snapshot/target migrations 496 and 497 apply afterward.
func TestCodexSwitchMigrationUpgradeLiveDB(t *testing.T) {
	env := isolatedCoordinateDatabase(t)
	dsn := env.pool.Config().ConnString()
	columns := func(table string, names []string, want int) {
		t.Helper()
		var count int
		if err := env.pool.QueryRow(env.ctx, `SELECT count(*) FROM information_schema.columns
WHERE table_schema = 'public' AND table_name = $1 AND column_name = ANY($2::text[])`, table, names).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != want {
			t.Fatalf("%s: found %d migration columns, want %d", table, count, want)
		}
	}
	marker := []string{"roll_marker", "roll_marker_observed_at"}
	importCols := []string{"initial_import_state", "initial_import_generation", "initial_import_error_class", "initial_import_updated_at"}
	epoch := []string{"codex_auth_mode", "codex_material_revision", "codex_account_key", "codex_account_id", "codex_claim_epoch"}
	pending := []string{"codex_switch_target_secret_id", "codex_switch_target_generation", "codex_switch_target_op_id", "codex_switch_target_auth_mode", "codex_switch_target_material_revision", "codex_switch_target_account_key", "codex_switch_target_account_revision"}
	provenance := []string{"claim_provenance_worker_id", "claim_provenance_generation"}
	assertHead := func() {
		t.Helper()
		columns("worker_upgrade_reports", marker, len(marker))
		columns("repos", importCols, len(importCols))
		columns("runs", provenance, len(provenance))
		columns("run_credential_epochs", epoch, len(epoch))
		columns("run_credential_switch_epochs", epoch, len(epoch))
		columns("runs", pending, len(pending))
	}
	assertHead() // Fresh installation.
	if err := store.MigrateDownTo(env.ctx, dsn, 495); err != nil {
		t.Fatal(err)
	}
	columns("worker_upgrade_reports", marker, len(marker))
	columns("repos", importCols, len(importCols))
	columns("runs", provenance, len(provenance))
	columns("run_credential_epochs", epoch, 0)
	columns("run_credential_switch_epochs", epoch, 0)
	columns("runs", pending, 0)
	if err := store.Migrate(env.ctx, dsn); err != nil {
		t.Fatal(err)
	}
	assertHead() // Upgrade from the unchanged supplied default-branch schema.
}
