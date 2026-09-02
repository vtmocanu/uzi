package workersvc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestOpenMRGuardLiveDB is the acceptance test for the create-time open-MR refusal
// (issue #856) against a REAL Postgres. createRun must refuse a fresh issue run when the
// SAME issue already has a COMPLETED (terminal) run owning an OPEN merge request — the
// active-run gate cannot see a terminal run, so this guard is the only thing that stops a
// fresh run re-planning and re-reviewing onto an already-open MR. Five arms, each on its
// own repo+issue so they cannot cross-contaminate:
//
//  1. blocked   — a completed issue run with mr_iid + mr_state='opened' ⇒ a second
//     CreateRun returns ErrOpenMRExists and the message names the MR number.
//  2. merged    — the same prior run with mr_state='merged' (no open MR) ⇒ allowed.
//  3. forced    — an open MR present but force=true ⇒ allowed (the guard is bypassed).
//  4. kind scope — a completed SELF_IMPROVE run (kind != 'issue') owning an open MR for the
//     same issue ⇒ allowed, because the guard is scoped to kind='issue'.
//  5. watcher lag — a completed issue run with mr_iid set but mr_state STILL NULL (the
//     watcher has not ticked yet) ⇒ a second CreateRun is still REFUSED with ErrOpenMRExists
//     naming the MR. This proves the guard keys on the authoritative mr_iid and no longer
//     depends on the watcher having recorded 'opened'; a NULL mr_state blocks conservatively.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (per the store
// live-DB harness; migrations run in-test).
func TestOpenMRGuardLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()
	q := store.New(pool)
	// nil box + nil forge: the guard fires (or does not) BEFORE any forge round-trip, and
	// guardDefaultBranch is a no-op with a nil guard — the same minimal wiring the sibling
	// mr_rework branch-guard live-DB test uses.
	svc := New(q, nil, Params{})

	// The cached issue always carries the default uzi label, so the run-eligibility gate
	// (PRD #764) passes and createRun reaches the open-MR guard under test.
	const issueIID int64 = 856

	// seedRepoAndIssue provisions a fresh owner + connection + repo (each with distinct
	// uuids so the four arms are isolated) plus one open, uzi-labelled cached issue, and
	// returns the owner and repo ids for a CreateRun call.
	seedRepoAndIssue := func(t *testing.T, tag string) (uuid.UUID, uuid.UUID) {
		t.Helper()
		userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
		exec := func(sql string, args ...any) {
			t.Helper()
			if _, err := pool.Exec(ctx, sql, args...); err != nil {
				t.Fatalf("exec %q: %v", sql, err)
			}
		}
		exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
			userID, fmt.Sprintf("omr-%s-%s@e2e", tag, userID))
		exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		      VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
		exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		      VALUES ($1, $2, 1, $3, 'https://forge.e2e/g/omr', 'main', true)`, repoID, connID, fmt.Sprintf("g/omr-%s", repoID))
		// Cached, open, uzi-labelled issue: the same jsonb the board renders from, which the
		// run-eligibility gate reads (PRD #764/#767). No PRD link is required.
		exec(`INSERT INTO issues (repo_id, forge_issue_iid, title, state, labels, web_url, has_prd_link, forge_updated_at, synced_at)
		      VALUES ($1, $2, 'Open-MR guard fixture', 'opened', '["uzi"]'::jsonb, 'https://x', false, now(), now())`,
			repoID, issueIID)
		return userID, repoID
	}

	// seedPriorRun inserts a COMPLETED run of the given kind owning an MR in the given
	// mr_state for this issue — the prior run whose open MR the guard keys on.
	seedPriorRun := func(t *testing.T, userID, repoID uuid.UUID, kind, mrState string, mrIID int64) {
		t.Helper()
		if _, err := pool.Exec(ctx,
			`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, mr_iid, mr_state, status)
			 VALUES ($1, $2, $3, $4, $5, 'prior', 'd', $6, $7, 'completed')`,
			uuid.New(), userID, repoID, kind, issueIID, mrIID, mrState); err != nil {
			t.Fatalf("insert prior %s run: %v", kind, err)
		}
	}

	t.Run("blocks a fresh issue run when a prior issue run owns an open MR", func(t *testing.T) {
		userID, repoID := seedRepoAndIssue(t, "block")
		const mrIID int64 = 4210
		seedPriorRun(t, userID, repoID, "issue", "opened", mrIID)

		_, err := svc.CreateRun(ctx, userID, repoID, issueIID, "the description", nil, nil, false /*force*/, nil)
		if !errors.Is(err, ErrOpenMRExists) {
			t.Fatalf("CreateRun err = %v, want ErrOpenMRExists", err)
		}
		// The refusal must name the MR so the caller can act on it.
		if want := fmt.Sprintf("!%d", mrIID); !strings.Contains(err.Error(), want) {
			t.Fatalf("CreateRun err %q does not mention the MR number %q", err.Error(), want)
		}
	})

	t.Run("allows a fresh issue run once the prior run's MR is merged", func(t *testing.T) {
		userID, repoID := seedRepoAndIssue(t, "merged")
		seedPriorRun(t, userID, repoID, "issue", "merged", 4220)

		run, err := svc.CreateRun(ctx, userID, repoID, issueIID, "the description", nil, nil, false /*force*/, nil)
		if err != nil {
			t.Fatalf("CreateRun with a merged prior MR err = %v, want success", err)
		}
		if run.Kind != RunKindIssue {
			t.Fatalf("run.Kind = %q, want %q", run.Kind, RunKindIssue)
		}
	})

	t.Run("force bypasses the open-MR guard", func(t *testing.T) {
		userID, repoID := seedRepoAndIssue(t, "force")
		seedPriorRun(t, userID, repoID, "issue", "opened", 4230)

		run, err := svc.CreateRun(ctx, userID, repoID, issueIID, "the description", nil, nil, true /*force*/, nil)
		if err != nil {
			t.Fatalf("CreateRun with force=true err = %v, want success even with an open MR", err)
		}
		if run.Kind != RunKindIssue {
			t.Fatalf("run.Kind = %q, want %q", run.Kind, RunKindIssue)
		}
	})

	t.Run("guard is scoped to issue kind: a non-issue prior run's open MR does not block", func(t *testing.T) {
		userID, repoID := seedRepoAndIssue(t, "scope")
		// A completed self_improve run (kind != 'issue') owning an open MR for the SAME
		// issue. GetOpenMRRunForIssue filters kind='issue', so it must not match.
		seedPriorRun(t, userID, repoID, "self_improve", "opened", 4240)

		run, err := svc.CreateRun(ctx, userID, repoID, issueIID, "the description", nil, nil, false /*force*/, nil)
		if err != nil {
			t.Fatalf("CreateRun with only a non-issue prior open-MR run err = %v, want success", err)
		}
		if run.Kind != RunKindIssue {
			t.Fatalf("run.Kind = %q, want %q", run.Kind, RunKindIssue)
		}
	})

	t.Run("blocks during the watcher-lag window: mr_iid set but mr_state still NULL", func(t *testing.T) {
		userID, repoID := seedRepoAndIssue(t, "lag")
		const mrIID int64 = 4250
		// A completed issue run whose mr_iid was written atomically at completion but whose
		// mr_state is STILL NULL — the watcher has not yet ticked. This is the window a
		// mr_state='opened'-only predicate missed; keying on mr_iid (releasing only on a
		// terminal mr_state) must block here.
		if _, err := pool.Exec(ctx,
			`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, mr_iid, status)
			 VALUES ($1, $2, $3, 'issue', $4, 'prior', 'd', $5, 'completed')`,
			uuid.New(), userID, repoID, issueIID, mrIID); err != nil {
			t.Fatalf("insert prior issue run with NULL mr_state: %v", err)
		}

		_, err := svc.CreateRun(ctx, userID, repoID, issueIID, "the description", nil, nil, false /*force*/, nil)
		if !errors.Is(err, ErrOpenMRExists) {
			t.Fatalf("CreateRun err = %v, want ErrOpenMRExists (NULL mr_state must block)", err)
		}
		// The refusal must still name the MR even before the watcher recorded a state.
		if want := fmt.Sprintf("!%d", mrIID); !strings.Contains(err.Error(), want) {
			t.Fatalf("CreateRun err %q does not mention the MR number %q", err.Error(), want)
		}
	})
}
