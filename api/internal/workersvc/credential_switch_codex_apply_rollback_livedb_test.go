package workersvc

import (
	"fmt"
	"testing"

	"github.com/google/uuid"
)

// Inject at every write boundary, including a deferred commit-time failure.
// Each trigger is scoped to this fixture's run so sibling LiveDB tests are unaffected.
func TestCodexApplyWriteRollbackLiveDB(t *testing.T) {
	for _, step := range []string{"binding", "mint", "journal", "stamp", "message", "last_seq", "commit"} {
		t.Run(step, func(t *testing.T) {
			f, run, _ := codexApplyFixture(t, false)
			name := "apply_fail_" + fmt.Sprintf("%x", uuid.New().ID())
			table, event, condition := "runs", "UPDATE", "NEW.id = '"+run.String()+"'::uuid"
			deferred := ""
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
			case "commit":
				condition += " AND NEW.last_seq > OLD.last_seq"
				deferred = "DEFERRABLE INITIALLY DEFERRED"
			}
			f.env.exec("CREATE FUNCTION " + name + "() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF " + condition + " THEN RAISE EXCEPTION 'injected apply write failure'; END IF; RETURN NEW; END $$")
			prefix, timing := "CREATE TRIGGER ", "BEFORE"
			if step == "commit" {
				prefix, timing = "CREATE CONSTRAINT TRIGGER ", "AFTER"
			}
			f.env.exec(prefix + name + " " + timing + " " + event + " ON " + table + " " + deferred + " FOR EACH ROW EXECUTE FUNCTION " + name + "()")
			defer f.env.exec("DROP FUNCTION " + name + "() CASCADE")
			before := f.snap(t, run)
			result, err := f.svc.ApplyCredentialSwitch(f.env.ctx, f.worker(t), run, f.req(f.opOf(t, run)))
			if err == nil || result.Outcome == ApplyOutcomeApplied || result.Codex != nil {
				t.Fatalf("write failure returned auth: %v err=%v", result, err)
			}
			if after := f.snap(t, run); after != before {
				t.Fatal("failed transaction changed run, capability, stamp, journal or messages")
			}
		})
	}
}
