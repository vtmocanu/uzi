package workersvc

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

func issueRun() store.Run { return store.Run{ID: uuid.New(), Kind: "issue"} }

func strp(s string) *string { return &s }

func TestClampWirePRDDonePathAcceptsAValidIssueRunPath(t *testing.T) {
	for _, p := range []string{"prds/72-x.md", "prds/done/72-x.md"} {
		got := clampWirePRDDonePath(issueRun(), strp(p))
		if !got.Valid || got.String != p {
			t.Errorf("clamp(%q) = valid=%v %q, want valid=true %q", p, got.Valid, got.String, p)
		}
	}
}

func TestClampWirePRDDonePathDropsNil(t *testing.T) {
	if got := clampWirePRDDonePath(issueRun(), nil); got.Valid {
		t.Errorf("nil must land NULL, got %q", got.String)
	}
}

// Decision 13's authoritative gate. The worker gates the tool schema too, but a
// worker is untrusted input reachable with a Bearer join token, so the server may
// not take its word for the run kind — runs.kind is the fact.
func TestClampWirePRDDonePathDropsEveryNonIssueKind(t *testing.T) {
	// self_improve is the sharpest: it runs against uzi's own repo (which HAS a
	// prds/ directory) and its issue is a reused backlog container whose
	// description an ungated patch would overwrite.
	for _, kind := range []string{"self_improve", "ci_fix", "judge"} {
		run := store.Run{ID: uuid.New(), Kind: kind}
		got := clampWirePRDDonePath(run, strp("prds/done/72-x.md"))
		if got.Valid {
			t.Errorf("kind %q: a perfectly VALID path must still be dropped; got %q", kind, got.String)
		}
	}
}

// Every rejected shape from the design's validator table, asserted here as well as
// in prdpath: this is the call site that decides a bad path costs the run nothing.
func TestClampWirePRDDonePathDropsInvalidPathsWithoutErroring(t *testing.T) {
	for _, p := range []string{
		"prds/../../../etc/passwd",
		"prds/../x.md",
		"/prds/x.md",
		"docs/x.md",
		"prds/x.txt",
		"rm -rf / prds/x.md",
		"https://host/g/p/-/blob/main/prds/x.md",
		"prds/",
		"prds/x.md#L4",
		"prds/x.md?ref=main",
		"prds/a\x00b.md",
		"",
	} {
		if got := clampWirePRDDonePath(issueRun(), strp(p)); got.Valid {
			t.Errorf("clamp(%q) = %q, want a drop", p, got.String)
		}
	}
}

// TestCompletionPRDDonePathWireContract mirrors TestCompletionMrWebURLWireContract:
// prd_done_path is additive + optional, so an OLD worker (which never sends the
// key) decodes with a nil PrdDonePath and stores NULL. A NEW worker that moved no
// PRD omits it too — the runner sends no key rather than "" or null — so the two
// are deliberately the same shape on the wire.
func TestCompletionPRDDonePathWireContract(t *testing.T) {
	const oldWorker = `{"status":"completed","branch":"agent/issue-7","mr_iid":42}`
	var oldReq StateRequest
	if err := json.Unmarshal([]byte(oldWorker), &oldReq); err != nil {
		t.Fatalf("old-worker completion unmarshal: %v", err)
	}
	if oldReq.PrdDonePath != nil {
		t.Errorf("old worker: expected nil PrdDonePath, got %q", *oldReq.PrdDonePath)
	}
	if p := clampWirePRDDonePath(issueRun(), oldReq.PrdDonePath); p.Valid {
		t.Errorf("old worker: expected NULL prd_done_path, got %q", p.String)
	}

	const path = "prds/done/72-prd-lifecycle-in-run.md"
	newWorker := `{"status":"completed","branch":"agent/issue-7","mr_iid":42,"prd_done_path":"` + path + `"}`
	var newReq StateRequest
	if err := json.Unmarshal([]byte(newWorker), &newReq); err != nil {
		t.Fatalf("new-worker completion unmarshal: %v", err)
	}
	if newReq.PrdDonePath == nil || *newReq.PrdDonePath != path {
		t.Errorf("new worker: expected PrdDonePath %q, got %v", path, newReq.PrdDonePath)
	}
	if p := clampWirePRDDonePath(issueRun(), newReq.PrdDonePath); !p.Valid || p.String != path {
		t.Errorf("new worker: expected prd_done_path %q, got valid=%v %q", path, p.Valid, p.String)
	}
}
