package store

import (
	"os"
	"regexp"
	"testing"
)

// TestReplayPageLimitMatchesWorker pins ListReplayRunInputs' LIMIT to the worker's
// REPLAY_PAGE_LIMIT (issue #1604). The worker treats only a page shorter than that constant as
// the end of the replayed backlog; if the server LIMIT drops below it, a full server page reads
// as short, and the worker can show a plan gate with replayed verdicts still unread.
func TestReplayPageLimitMatchesWorker(t *testing.T) {
	serverLimit := regexp.MustCompile(`(?m)^LIMIT (\d+)\s*$`).FindStringSubmatch(listReplayRunInputs)
	if serverLimit == nil {
		t.Fatalf("ListReplayRunInputs has no trailing LIMIT:\n%s", listReplayRunInputs)
	}
	src, err := os.ReadFile("../../../agent/src/steering.ts")
	if err != nil {
		t.Fatalf("read the worker's steering.ts: %v", err)
	}
	workerLimit := regexp.MustCompile(`(?m)^const REPLAY_PAGE_LIMIT = (\d+);$`).FindSubmatch(src)
	if workerLimit == nil {
		t.Fatal("agent/src/steering.ts no longer declares `const REPLAY_PAGE_LIMIT = <n>;`")
	}
	if serverLimit[1] != string(workerLimit[1]) {
		t.Fatalf("ListReplayRunInputs LIMIT %s != worker REPLAY_PAGE_LIMIT %s", serverLimit[1], workerLimit[1])
	}
}
