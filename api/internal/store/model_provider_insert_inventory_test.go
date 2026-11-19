package store_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestEveryRunsInsertNamesTheProviderBinding is the PRD #4320 M4 insert inventory. A runs INSERT
// that does not name model_provider_key_mode silently writes a vendor run (every binding column
// NULL) for any caller, including a provider-sourced owner's: the failure is invisible because
// the columns default and sqlc.narg params compile when unstamped. Every statement in
// queries/*.sql that inserts into runs must therefore NAME the binding (an explicit NULL is fine
// for a lane that refuses providers until PRD #4322, but it must be written down). The scan strips
// SQL comments, splits statements on ';' and matches "INSERT INTO runs" as a whole word, so
// run_usage, run_messages and the rest do not count.
//
// Out of scope, stated: INSERTs into runs outside queries/*.sql (the hand-written seed statements
// in cmd/codexm3btestserver and cmd/rollupbench, and test fixtures) are not production writers of
// provider-sourced runs and are not scanned here.
func TestEveryRunsInsertNamesTheProviderBinding(t *testing.T) {
	files, err := filepath.Glob("queries/*.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("no query files: %v", err)
	}
	comment := regexp.MustCompile(`--[^\n]*`)
	insert := regexp.MustCompile(`(?i)\bINSERT\s+INTO\s+runs\b`)
	named := regexp.MustCompile(`(?i)\bmodel_provider_key_mode\b`)
	sites := 0
	for _, f := range files {
		raw, err := os.ReadFile(f) //nolint:gosec // G304: f comes from a Glob over this package's own queries dir.
		if err != nil {
			t.Fatal(err)
		}
		for _, stmt := range strings.Split(comment.ReplaceAllString(string(raw), ""), ";") {
			if !insert.MatchString(stmt) {
				continue
			}
			sites++
			if !named.MatchString(stmt) {
				head := strings.Join(strings.Fields(stmt), " ")
				if len(head) > 120 {
					head = head[:120]
				}
				t.Errorf("%s: an INSERT INTO runs does not name model_provider_key_mode (a silent vendor run for a provider-sourced owner): %s", f, head)
			}
		}
	}
	// 15 statements today (CreateRun, CreatePromptRun, CreateSelfImproveRun, CreateTaskRun,
	// CreateThenFixRun, CreateTaskReviewRun, CreateCIFixRun, the two mr_rework inserts, the two
	// chat inserts, CreateJudgeRun, CreateJobRun and the two cross-check children). A drop means
	// the instrument lost its targets.
	if sites < 15 {
		t.Fatalf("found %d INSERT INTO runs statements, want at least 15 (the instrument lost its targets)", sites)
	}
}
