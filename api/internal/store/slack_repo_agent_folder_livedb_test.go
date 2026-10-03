package store_test

import (
	"slices"
	"testing"
)

// TestGetSlackRunContextRepoAgentFolderLiveDB executes the Slack names/folder
// projection against Postgres. newWPFixture skips without UZI_TEST_DATABASE_URL.
func TestGetSlackRunContextRepoAgentFolderLiveDB(t *testing.T) {
	fx := newWPFixture(t)
	for _, tc := range []struct {
		name, roster, folder string
		names                []string
	}{
		{"Codex", `[{"name":"tester","folder":".codex/agents","description":"private"},{"name":"coder","folder":".claude/agents"}]`, ".codex/agents", []string{"tester", "coder"}},
		{"Claude", `[{"name":"tester","folder":".claude/agents"},{"name":"coder"}]`, ".claude/agents", []string{"tester", "coder"}},
		{"missing folder", `[{"name":"tester"},{"name":"coder","folder":".codex/agents"}]`, ".claude/agents", []string{"tester", "coder"}},
		{"empty folder", `[{"name":"tester","folder":""},{"name":"coder"}]`, ".claude/agents", []string{"tester", "coder"}},
		{"null folder", `[{"name":"tester","folder":null},{"name":"coder"}]`, ".claude/agents", []string{"tester", "coder"}},
		{"NULL roster", "", ".claude/agents", []string{}},
		{"empty roster", "[]", ".claude/agents", []string{}},
		{"unexpected folder", `[{"name":"tester","folder":".other/agents"},{"name":"coder"}]`, ".claude/agents", []string{"tester", "coder"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := fx.run(wpRun{status: "awaiting_approval"})
			var roster any
			if tc.roster != "" {
				roster = tc.roster
			}
			mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET repo_agents = $2::jsonb WHERE id = $1`, id, roster)
			row, err := fx.q.GetSlackRunContext(fx.ctx, id)
			if err != nil {
				t.Fatalf("GetSlackRunContext: %v", err)
			}
			if row.RepoAgentFolder != tc.folder {
				t.Errorf("folder = %q, want %q", row.RepoAgentFolder, tc.folder)
			}
			if !slices.Equal(row.RepoAgentNames, tc.names) {
				t.Errorf("names = %v, want ordered %v", row.RepoAgentNames, tc.names)
			}
		})
	}
}
