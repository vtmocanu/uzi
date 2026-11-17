package poller

import (
	"fmt"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/issuetracker/youtracktest"
)

// PRD #4173 M4 with issue #4399: a linked repo that owes its first import (state
// 'waiting') is claimed by one tick, imported through the issue tracker (never the
// forge's ListIssues), and settled on the linked issue sync's result alone.

// owe marks the linked repo as owing its first import and wires the engine's import
// state store, as production does.
func (e *linkedEnv) owe(t *testing.T) {
	t.Helper()
	if _, err := e.pool.Exec(e.ctx, `UPDATE repos SET initial_import_state='waiting', initial_import_updated_at=now() WHERE id=$1`, e.repo); err != nil {
		t.Fatal(err)
	}
	e.engine.SetImportStateStore(e.q)
}

// importRow returns the repo's import state and error class ("" is NULL).
func (e *linkedEnv) importRow(t *testing.T) (state, class string) {
	t.Helper()
	var c *string
	if err := e.pool.QueryRow(e.ctx, `SELECT initial_import_state, initial_import_error_class FROM repos WHERE id=$1`, e.repo).Scan(&state, &c); err != nil {
		t.Fatal(err)
	}
	if c != nil {
		class = *c
	}
	return state, class
}

func TestLinkedRepoOwedFirstImportSucceedsThroughTheIssueTrackerLiveDB(t *testing.T) {
	srv := youtracktest.NewServer(t)
	trustFake(t, srv)
	env := newLinkedEnv(t, srv, nil)
	n := srv.SeedIssue("tagged", "body", youtracktest.EligibilityTagID)
	env.owe(t)

	env.poll(t)

	if state, class := env.importRow(t); state != "succeeded" || class != "" {
		t.Fatalf("initial_import_state = %q class %q, want succeeded with no class", state, class)
	}
	var key *string
	if err := env.pool.QueryRow(env.ctx, `SELECT issue_key FROM issues WHERE repo_id = $1 AND forge_issue_iid = $2`, env.repo, n).Scan(&key); err != nil {
		t.Fatalf("the import did not cache the tagged issue through the issue tracker: %v", err)
	}
	if want := fmt.Sprintf("%s-%d", youtracktest.ProjectShortName, n); key == nil || *key != want {
		t.Fatalf("issue_key = %v, want %s", key, want)
	}
	for _, c := range env.rec.Calls() {
		if strings.HasPrefix(c, "ListIssues(") {
			t.Errorf("the forge listed issues for a linked repo's import: %q", c)
		}
	}
}

func TestLinkedRepoOwedFirstImportFailsWithTheIssueTrackerErrorClassLiveDB(t *testing.T) {
	srv := youtracktest.NewServer(t)
	trustFake(t, srv)
	env := newLinkedEnv(t, srv, nil)
	env.owe(t)
	// The stored token no longer matches the one the fake accepts: YouTrack answers 401.
	srv.Token += "-rotated"

	env.poll(t)

	state, class := env.importRow(t)
	if state != "failed" || class != string(forge.ErrorClassAuth) {
		t.Fatalf("initial_import_state = %q class %q, want failed with class %q", state, class, forge.ErrorClassAuth)
	}
	for _, c := range env.rec.Calls() {
		if strings.HasPrefix(c, "ListIssues(") {
			t.Errorf("the forge listed issues for a linked repo's import: %q", c)
		}
	}
}
