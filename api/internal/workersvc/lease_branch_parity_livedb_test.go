package workersvc

import (
	"context"
	"fmt"
	"math"
	"os"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// goLeaseBranch is the effective branch identity the Go side derives for a run, written from the
// SAME sources the claim path and the CI-fix create path use, never from fn_run_lease_branch:
//   - issue: agentIssueBranch(issue_iid) for a positive iid, and only when the run's recorded branch
//     is NULL, empty or already that exact value. runs.branch of an issue run is written from the
//     worker's own terminal report, so any other value (a worker-chosen branch, another issue's
//     branch) yields no identity rather than being trusted;
//   - mr_rework: run.PipelineRef (claim_assembly.go sources the claim's Branch from it);
//   - ci_fix: claimPipelineFromSnapshot(snapshot).Ref;
//   - every other kind: none.
//
// "" means no identity (SQL NULL).
func goLeaseBranch(kind string, branch, pipelineRef *string, iid *int64, snapshot []byte) string {
	str := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	switch kind {
	case runkind.Issue:
		if iid == nil || *iid <= 0 {
			return ""
		}
		canonical := agentIssueBranch(*iid)
		if b := str(branch); b != "" && b != canonical {
			return ""
		}
		return canonical
	case runkind.MRRework:
		return str(pipelineRef)
	case runkind.CIFix:
		if p := claimPipelineFromSnapshot(snapshot); p != nil {
			return p.Ref
		}
		return ""
	default:
		return ""
	}
}

// TestLeaseBranchParityLiveDB pins fn_run_lease_branch (migration 00282, the single SQL identity
// every lease claim and placement predicate shares) to the Go sources it mirrors, over valid,
// malformed and empty inputs, including a worker-reported issue branch that must never become the
// identity. Mutating either side (the SQL function's 'agent/issue-' prefix, a kind
// arm, the ci_fix ref rule, or agentIssueBranch / claimPipelineFromSnapshot) reddens it.
func TestLeaseBranchParityLiveDB(t *testing.T) {
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

	sp := func(s string) *string { return &s }
	ip := func(n int64) *int64 { return &n }
	type row struct {
		name        string
		kind        string
		branch      *string
		pipelineRef *string
		iid         *int64
		snapshot    *string // JSON text, nil = SQL NULL
	}
	rows := []row{
		// issue: canonical branch from a valid iid.
		{name: "issue iid 1", kind: "issue", iid: ip(1)},
		{name: "issue iid 7", kind: "issue", iid: ip(7)},
		{name: "issue iid 2006", kind: "issue", iid: ip(2006)},
		{name: "issue iid max int64", kind: "issue", iid: ip(math.MaxInt64)},
		// issue: a recorded branch is accepted only when it is exactly the canonical one.
		{name: "issue canonical explicit branch", kind: "issue", branch: sp("agent/issue-7"), iid: ip(7)},
		{name: "issue canonical explicit branch, large iid", kind: "issue", branch: sp("agent/issue-2006"), iid: ip(2006)},
		// issue: a worker-reported branch never becomes the identity (trust boundary), whatever the iid.
		{name: "issue reported branch of another issue", kind: "issue", branch: sp("agent/issue-21"), iid: ip(20)},
		{name: "issue reported branch of another issue, iid 8", kind: "issue", branch: sp("agent/issue-7"), iid: ip(8)},
		{name: "issue reported custom branch", kind: "issue", branch: sp("feature/x"), iid: ip(5)},
		{name: "issue reported custom branch, no iid", kind: "issue", branch: sp("feature/x")},
		{name: "issue reported non-canonical spelling", kind: "issue", branch: sp("agent/issue-007"), iid: ip(7)},
		{name: "issue reported branch with a trailing suffix", kind: "issue", branch: sp("agent/issue-7-x"), iid: ip(7)},
		{name: "issue reported branch with unicode and spaces", kind: "issue", branch: sp("feat/ünï cödé x"), iid: ip(0)},
		{name: "issue canonical-looking branch, iid 0", kind: "issue", branch: sp("agent/issue-0"), iid: ip(0)},
		{name: "issue canonical-looking branch, NULL iid", kind: "issue", branch: sp("agent/issue-7")},
		// issue: no identity.
		{name: "issue NULL iid, NULL branch", kind: "issue"},
		{name: "issue iid 0", kind: "issue", iid: ip(0)},
		{name: "issue negative iid", kind: "issue", iid: ip(-5)},
		{name: "issue min int64 iid", kind: "issue", iid: ip(math.MinInt64)},
		{name: "issue empty branch, NULL iid", kind: "issue", branch: sp("")},
		{name: "issue empty branch falls back to iid", kind: "issue", branch: sp(""), iid: ip(9)},
		// mr_rework: pipeline_ref.
		{name: "mr_rework pipeline_ref", kind: "mr_rework", pipelineRef: sp("agent/issue-12")},
		{name: "mr_rework ignores branch", kind: "mr_rework", branch: sp("other"), pipelineRef: sp("agent/issue-12"), iid: ip(3)},
		{name: "mr_rework empty pipeline_ref", kind: "mr_rework", pipelineRef: sp("")},
		{name: "mr_rework NULL pipeline_ref", kind: "mr_rework", branch: sp("other"), iid: ip(3)},
		// ci_fix: the failure snapshot's ref.
		{name: "ci_fix snapshot ref", kind: "ci_fix", pipelineRef: sp("ignored"), snapshot: sp(`{"pipeline_id":5,"ref":"agent/issue-13","sha":"abc"}`)},
		{name: "ci_fix snapshot with failed jobs", kind: "ci_fix", snapshot: sp(`{"pipeline_id":5,"ref":"main","failed_jobs":[{"name":"t","stage":"test"}]}`)},
		{name: "ci_fix empty ref", kind: "ci_fix", pipelineRef: sp("agent/issue-1"), snapshot: sp(`{"pipeline_id":5,"ref":""}`)},
		{name: "ci_fix missing ref key", kind: "ci_fix", pipelineRef: sp("agent/issue-1"), snapshot: sp(`{"pipeline_id":5}`)},
		{name: "ci_fix null ref", kind: "ci_fix", snapshot: sp(`{"ref":null}`)},
		{name: "ci_fix non-string ref", kind: "ci_fix", snapshot: sp(`{"ref":5}`)},
		{name: "ci_fix object ref", kind: "ci_fix", snapshot: sp(`{"ref":{"a":"b"}}`)},
		{name: "ci_fix empty object snapshot", kind: "ci_fix", snapshot: sp(`{}`)},
		{name: "ci_fix array snapshot", kind: "ci_fix", snapshot: sp(`[]`)},
		{name: "ci_fix array of objects snapshot", kind: "ci_fix", snapshot: sp(`[{"ref":"x"}]`)},
		{name: "ci_fix scalar string snapshot", kind: "ci_fix", snapshot: sp(`"agent/issue-1"`)},
		{name: "ci_fix scalar number snapshot", kind: "ci_fix", snapshot: sp(`5`)},
		{name: "ci_fix json null snapshot", kind: "ci_fix", snapshot: sp(`null`)},
		{name: "ci_fix NULL snapshot", kind: "ci_fix", pipelineRef: sp("agent/issue-1")},
		// every other kind has no identity, whatever columns are set.
		{name: "self_improve", kind: "self_improve", branch: sp("agent/issue-1"), iid: ip(1)},
		{name: "prompt", kind: "prompt", branch: sp("agent/issue-1")},
		{name: "task", kind: "task", branch: sp("uzi/task/abc")},
		{name: "job", kind: "job", branch: sp("agent/issue-1")},
		{name: "judge", kind: "judge"},
		{name: "chat", kind: "chat"},
		{name: "unknown kind", kind: "frobnicate", branch: sp("agent/issue-1"), pipelineRef: sp("agent/issue-1"), iid: ip(1), snapshot: sp(`{"ref":"agent/issue-1"}`)},
		{name: "empty kind", kind: "", branch: sp("agent/issue-1"), iid: ip(1)},
		{name: "kind is case-sensitive", kind: "Issue", iid: ip(1)},
	}

	for _, c := range rows {
		t.Run(c.name, func(t *testing.T) {
			var snap []byte
			var snapArg any
			if c.snapshot != nil {
				snap = []byte(*c.snapshot)
				snapArg = *c.snapshot
			}
			arg := func(p *string) any {
				if p == nil {
					return nil
				}
				return *p
			}
			var iidArg any
			if c.iid != nil {
				iidArg = *c.iid
			}
			var got *string
			if err := pool.QueryRow(ctx, `SELECT fn_run_lease_branch($1::text, $2::text, $3::text, $4::bigint, $5::jsonb)`,
				c.kind, arg(c.branch), arg(c.pipelineRef), iidArg, snapArg).Scan(&got); err != nil {
				t.Fatalf("fn_run_lease_branch: %v", err)
			}
			want := goLeaseBranch(c.kind, c.branch, c.pipelineRef, c.iid, snap)
			gotStr := ""
			if got != nil {
				gotStr = *got
			}
			if gotStr != want {
				t.Fatalf("SQL fn_run_lease_branch = %q, Go effective branch = %q", gotStr, want)
			}
			// SQL NULL and Go "" must be the same fact: an empty string is never returned.
			if got != nil && *got == "" {
				t.Fatalf("fn_run_lease_branch returned an empty string; no identity must be NULL")
			}
			// An issue run's identity is always the canonical branch of its own iid, whatever was recorded.
			if c.kind == runkind.Issue && got != nil {
				n, ok := issueIIDFromAgentBranch(*got)
				if !ok || c.iid == nil || n != *c.iid {
					t.Fatalf("issueIIDFromAgentBranch(%q) = (%d, %v), want (%v, true)", *got, n, ok, c.iid)
				}
			}
		})
	}

	// agentIssueBranch is the convention the SQL function spells out; check it directly for a spread of iids.
	for _, n := range []int64{1, 9, 10, 99, 100, 2006, 1 << 40, math.MaxInt64} {
		var got *string
		if err := pool.QueryRow(ctx, `SELECT fn_run_lease_branch('issue', NULL, NULL, $1::bigint, NULL)`, n).Scan(&got); err != nil {
			t.Fatalf("iid %d: %v", n, err)
		}
		if got == nil || *got != agentIssueBranch(n) || *got != fmt.Sprintf("agent/issue-%d", n) {
			t.Fatalf("iid %d: SQL = %v, agentIssueBranch = %q", n, got, agentIssueBranch(n))
		}
	}
}
