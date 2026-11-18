package workersvc

import (
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Pins the zero-row fences in applyCodexSwitchLocked: a write that matches no row
// and raises no error must still abort the whole apply and roll back every earlier
// write. A BEFORE ROW trigger returning NULL skips the row, so the statement
// succeeds with zero rows affected (or Inserted=false for the message insert).
// For the two RETURNING writes (mint, journal) the skip surfaces client-side as
// pgx.ErrNoRows, which does NOT abort the Postgres transaction, so only the Go
// error check stops a commit without a minted capability or journal row.
// This does not cover the RAISE paths; TestCodexApplyWriteRollbackLiveDB does.
// Each trigger is scoped to this fixture's run so sibling LiveDB tests are unaffected.
func TestCodexApplyZeroRowWriteRollbackLiveDB(t *testing.T) {
	for _, step := range []string{"binding", "mint", "journal", "stamp", "message", "last_seq"} {
		t.Run(step, func(t *testing.T) {
			f, run, _ := codexApplyFixture(t, false)
			name := "apply_skip_" + fmt.Sprintf("%x", uuid.New().ID())
			table, event, condition := "runs", "UPDATE", "NEW.id = '"+run.String()+"'::uuid"
			switch step {
			case "binding":
				condition += " AND NEW.codex_secret_id IS DISTINCT FROM OLD.codex_secret_id"
			case "mint":
				condition += " AND NEW.codex_claim_epoch IS DISTINCT FROM OLD.codex_claim_epoch"
			case "journal":
				table, event, condition = "run_credential_switch_epochs", "INSERT", "NEW.run_id = '"+run.String()+"'::uuid"
			case "stamp":
				condition += " AND NEW.credential_switch_requested_at IS NULL AND OLD.credential_switch_requested_at IS NOT NULL"
			case "message":
				table, event, condition = "run_messages", "INSERT", "NEW.run_id = '"+run.String()+"'::uuid"
			case "last_seq":
				condition += " AND NEW.last_seq > OLD.last_seq"
			}
			f.env.exec("CREATE FUNCTION " + name + "() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF " + condition + " THEN RETURN NULL; END IF; RETURN NEW; END $$")
			f.env.exec("CREATE TRIGGER " + name + " BEFORE " + event + " ON " + table + " FOR EACH ROW EXECUTE FUNCTION " + name + "()")
			defer f.env.exec("DROP FUNCTION " + name + "() CASCADE")
			before := f.snap(t, run)
			result, err := f.svc.ApplyCredentialSwitch(f.env.ctx, f.worker(t), run, f.req(f.opOf(t, run)))
			if err == nil || result.Outcome == ApplyOutcomeApplied || result.Codex != nil {
				t.Fatalf("%s: zero-row write returned auth: %v err=%v", step, result, err)
			}
			switch step {
			case "message":
				if err.Error() != "codex switch message sequence occupied" {
					t.Fatalf("%s: err = %v, want message sequence occupied", step, err)
				}
			case "mint", "journal":
				if !errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("%s: err = %v, want pgx.ErrNoRows", step, err)
				}
			default:
				if !errors.Is(err, ErrStaleClaim) {
					t.Fatalf("%s: err = %v, want ErrStaleClaim", step, err)
				}
			}
			if after := f.snap(t, run); after != before {
				t.Fatalf("%s: zero-row write left partial state", step)
			}
		})
	}
}
