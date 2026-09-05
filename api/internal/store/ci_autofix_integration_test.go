package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestCIAutofixLiveDB exercises the PRD #71 M4 ci-autofix loop-guard queries
// against a REAL Postgres — the eligibility gates and the attempt-ledger
// transitions the fake-store unit tests cannot cover. It pins:
//
//   - ListCIAutofixCandidateRefs: the three eligibility gates (kind-awareness, the
//     default-branch/mr_iid guard, the owner opt-in + anthropic-token gate) in BOTH
//     directions, plus the DISTINCT-ON-branch newest-run pick;
//   - GetCIAutofixAttempt: absent row → ErrNoRows, present row read back;
//   - UpsertCIAutofixAttempt: first proceed inserts count=1, second increments to 2
//     and overwrites the signature/pipeline target;
//   - RecordCIAutofixPipeline: the silent path moves last_pipeline_id WITHOUT
//     advancing the counter;
//   - SetCIAutofixHaltNotified: the comment-once latch;
//   - DeleteCIAutofixAttempt: reset-on-green rowcount;
//   - DeleteCIAutofixAttemptsNotIn: reconcile eviction keeps only the keep-set.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; the e2e
// runner (e2e/run-store-it.sh) provides one.
func TestCIAutofixLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
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

	// Four owners: u1 opted in WITH a token (eligible), u2 opted OUT (with a token),
	// u3 opted in but with NO token, u4 whose ci_autofix_enabled is NULL (inherit)
	// WITH a token. u1's and u4's branches can be candidates.
	u1, u2, u3, u4 := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	connID, repoID := uuid.New(), uuid.New()
	for _, u := range []struct {
		id      uuid.UUID
		enabled bool
	}{{u1, true}, {u2, false}, {u3, true}} {
		mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash, ci_autofix_enabled) VALUES ($1, $2, 'x', $3)`,
			u.id, fmt.Sprintf("autofix-%s@e2e", u.id), u.enabled)
	}
	// PRD #914: u4 OMITS ci_autofix_enabled entirely, so post-00190 (DROP DEFAULT,
	// nullable) it is NULL = inherit the admin global default (ON). u4 proves the
	// candidate gate is `IS NOT FALSE`: a NULL owner is INCLUDED, unlike u2's explicit
	// false. Do NOT add u4 to the enabled-bool loop above.
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		u4, fmt.Sprintf("autofix-%s@e2e", u4))
	// u1, u2, and u4 have an anthropic token; u3 does not.
	mustExec(ctx, t, pool, `INSERT INTO user_secrets (user_id, kind, ciphertext, label) VALUES ($1, 'anthropic_token', $2, 'default')`, u1, []byte{0x1})
	mustExec(ctx, t, pool, `INSERT INTO user_secrets (user_id, kind, ciphertext, label) VALUES ($1, 'anthropic_token', $2, 'default')`, u2, []byte{0x1})
	mustExec(ctx, t, pool, `INSERT INTO user_secrets (user_id, kind, ciphertext, label) VALUES ($1, 'anthropic_token', $2, 'default')`, u4, []byte{0x1})

	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, u1, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)

	// insertRun inserts an issue-shaped run (issue/self_improve) on a branch.
	insertRun := func(owner uuid.UUID, kind, branch string, issueIID int64, mrIID *int64, createdOffset string) {
		var mr any
		if mrIID != nil {
			mr = *mrIID
		}
		mustExec(ctx, t, pool,
			`INSERT INTO runs (user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, branch, mr_iid, created_at)
			 VALUES ($1, $2, $3, $4, 't', 'd', 'completed', $5, $6, now() - $7::interval)`,
			owner, repoID, kind, issueIID, branch, mr, createdOffset)
	}
	// insertPromptRun inserts a prompt-shaped run (issue_iid = NULL, per the
	// runs_kind_shape CHECK for kind='prompt') on a scheduled-lane branch.
	insertPromptRun := func(owner uuid.UUID, branch string, mrIID *int64, createdOffset string) {
		var mr any
		if mrIID != nil {
			mr = *mrIID
		}
		mustExec(ctx, t, pool,
			`INSERT INTO runs (user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, branch, mr_iid, created_at)
			 VALUES ($1, $2, 'prompt', NULL, 't', 'd', 'completed', $3, $4, now() - $5::interval)`,
			owner, repoID, branch, mr, createdOffset)
	}
	// insertPipeline caches a pipeline_status row for a ref.
	insertPipeline := func(ref string, pipelineID int64, status string) {
		mustExec(ctx, t, pool,
			`INSERT INTO pipeline_statuses (repo_id, ref, pipeline_id, sha, status, web_url, synced_at)
			 VALUES ($1, $2, $3, 'deadbeef', $4, 'https://forge.e2e/p', now())`,
			repoID, ref, pipelineID, status)
	}

	iid := func(n int64) *int64 { return &n }

	// u1: the eligible candidate — an issue run on an agent MR branch with a FAILED
	// pipeline. A SECOND, OLDER run on the same branch (no mr_iid) exercises the
	// DISTINCT-ON newest-first pick: the newer row (with the mr_iid) must win.
	insertRun(u1, "issue", "agent/issue-1", 1, nil, "2 hours")
	insertRun(u1, "issue", "agent/issue-1", 1, iid(101), "1 hour")
	insertPipeline("agent/issue-1", 9001, "failed")

	// u1: default branch — excluded by the default-branch guard even though failed.
	insertRun(u1, "issue", "main", 2, iid(102), "1 hour")
	insertPipeline("main", 9002, "failed")

	// u1: no mr_iid — excluded by the mr_iid guard.
	insertRun(u1, "issue", "agent/issue-3", 3, nil, "1 hour")
	insertPipeline("agent/issue-3", 9003, "failed")

	// u1: a green pipeline — excluded (status <> 'failed').
	insertRun(u1, "issue", "agent/issue-4", 4, iid(104), "1 hour")
	insertPipeline("agent/issue-4", 9004, "success")

	// PRD #908 M4: ListCIAutofixCandidateRefs's kind filter widened to
	// kind IN ('issue','ci_fix','prompt','self_improve'), so the scheduled lanes are now
	// INCLUDED. Two runs owned by the eligible owner u1, each on a scheduled-lane branch
	// with a FAILED pipeline + mr_iid, must BOTH surface as candidates.
	//
	// u1: a self_improve run (issue_iid set, per the runs_kind_shape CHECK for the kind) on
	// a uzi/self-improve/<uuid> branch — previously EXCLUDED by kind-awareness, now included.
	selfImproveBranch := "uzi/self-improve/" + uuid.New().String()
	insertRun(u1, "self_improve", selfImproveBranch, 5, iid(105), "1 hour")
	insertPipeline(selfImproveBranch, 9005, "failed")

	// u1: a prompt run (issue_iid NULL, per the shape CHECK) on a uzi/prompt-<uuid> branch.
	promptBranch := "uzi/prompt-" + uuid.New().String()
	insertPromptRun(u1, promptBranch, iid(108), "1 hour")
	insertPipeline(promptBranch, 9008, "failed")

	// u2: a prompt run whose owner opted OUT — still excluded by the owner opt-in gate,
	// proving that gate is kind-INDEPENDENT (it drops a scheduled-lane run the same as an
	// issue run).
	u2PromptBranch := "uzi/prompt-" + uuid.New().String()
	insertPromptRun(u2, u2PromptBranch, iid(106), "1 hour")
	insertPipeline(u2PromptBranch, 9006, "failed")

	// u3: a self_improve run whose owner has NO token — still excluded by the token gate,
	// again kind-independent.
	u3SelfImproveBranch := "uzi/self-improve/" + uuid.New().String()
	insertRun(u3, "self_improve", u3SelfImproveBranch, 7, iid(107), "1 hour")
	insertPipeline(u3SelfImproveBranch, 9007, "failed")

	// PRD #914: u4 (ci_autofix_enabled NULL = inherit → ON) proves the candidate gate is
	// `IS NOT FALSE`: an inherit-default owner is INCLUDED, exactly as u2's explicit false
	// is EXCLUDED. An eligible issue run on an agent-MR branch with a FAILED pipeline.
	insertRun(u4, "issue", "agent/issue-9", 9, iid(109), "1 hour")
	insertPipeline("agent/issue-9", 9009, "failed")

	// ── ListCIAutofixCandidateRefs: the exact eligible SET ──
	// Eligible now = agent/issue-1 (issue lane) + u1's two scheduled-lane branches +
	// agent/issue-9 (u4, ci_autofix_enabled NULL = inherit → ON under the IS NOT FALSE
	// gate). Every other seed is excluded by a specific gate (default-branch, mr_iid,
	// green pipeline, owner opt-out, missing token). Assert the exact set by branch name —
	// precise, not a bare count — so a stray candidate or a missing one both fail.
	cands, err := q.ListCIAutofixCandidateRefs(ctx, repoID)
	if err != nil {
		t.Fatalf("ListCIAutofixCandidateRefs: %v", err)
	}
	gotRefs := make(map[string]bool, len(cands))
	for _, cand := range cands {
		gotRefs[cand.Ref.String] = true
	}
	// agent/issue-9 (u4, NULL) proves inherit→included under IS NOT FALSE; u2's
	// explicit-false branch proves false→excluded.
	wantRefs := []string{"agent/issue-1", selfImproveBranch, promptBranch, "agent/issue-9"}
	if len(cands) != len(wantRefs) {
		t.Fatalf("want exactly %d candidates %v, got %d: %+v", len(wantRefs), wantRefs, len(cands), cands)
	}
	for _, ref := range wantRefs {
		if !gotRefs[ref] {
			t.Errorf("expected %q to be an eligible ci-autofix candidate, got set %v", ref, gotRefs)
		}
	}

	// The issue-lane candidate still carries the NEWEST run's mr_iid (DISTINCT-ON pick), its
	// owner, and its failed pipeline — unchanged by the scheduled-lane widening.
	var c store.ListCIAutofixCandidateRefsRow
	foundIssue := false
	for _, cand := range cands {
		if cand.Ref.String == "agent/issue-1" {
			c, foundIssue = cand, true
		}
	}
	if !foundIssue {
		t.Fatalf("agent/issue-1 must remain a candidate")
	}
	if !c.MrIid.Valid || c.MrIid.Int64 != 101 {
		t.Errorf("candidate must carry the NEWEST run's mr_iid=101, got %+v", c.MrIid)
	}
	if c.UserID != u1 {
		t.Errorf("candidate user_id = %s, want u1 %s", c.UserID, u1)
	}
	if c.PipelineID != 9001 {
		t.Errorf("candidate pipeline_id = %d, want 9001", c.PipelineID)
	}

	// ── the attempt ledger ──
	ref := "agent/issue-1"
	getAttempt := store.GetCIAutofixAttemptParams{RepoID: repoID, Ref: ref}

	// No row yet.
	if _, err := q.GetCIAutofixAttempt(ctx, getAttempt); err == nil {
		t.Fatal("GetCIAutofixAttempt on a never-attempted ref must return an error (no row)")
	}

	// First proceed → count=1, target recorded.
	if err := q.UpsertCIAutofixAttempt(ctx, store.UpsertCIAutofixAttemptParams{
		RepoID: repoID, Ref: ref,
		LastSignature:  pgtype.Text{String: "sig-A", Valid: true},
		LastPipelineID: pgtype.Int8{Int64: 9001, Valid: true},
	}); err != nil {
		t.Fatalf("UpsertCIAutofixAttempt (first): %v", err)
	}
	row, err := q.GetCIAutofixAttempt(ctx, getAttempt)
	if err != nil {
		t.Fatalf("GetCIAutofixAttempt after first proceed: %v", err)
	}
	if row.AttemptCount != 1 || row.LastSignature.String != "sig-A" || row.LastPipelineID.Int64 != 9001 {
		t.Fatalf("first proceed: got count=%d sig=%q pid=%d, want 1/sig-A/9001", row.AttemptCount, row.LastSignature.String, row.LastPipelineID.Int64)
	}
	if row.HaltNotified {
		t.Fatal("halt_notified must default false")
	}

	// Second proceed → count=2, target overwritten.
	if err := q.UpsertCIAutofixAttempt(ctx, store.UpsertCIAutofixAttemptParams{
		RepoID: repoID, Ref: ref,
		LastSignature:  pgtype.Text{String: "sig-B", Valid: true},
		LastPipelineID: pgtype.Int8{Int64: 9010, Valid: true},
	}); err != nil {
		t.Fatalf("UpsertCIAutofixAttempt (second): %v", err)
	}
	row, _ = q.GetCIAutofixAttempt(ctx, getAttempt)
	if row.AttemptCount != 2 || row.LastSignature.String != "sig-B" || row.LastPipelineID.Int64 != 9010 {
		t.Fatalf("second proceed: got count=%d sig=%q pid=%d, want 2/sig-B/9010", row.AttemptCount, row.LastSignature.String, row.LastPipelineID.Int64)
	}

	// Silent path → last_pipeline_id moves, count stays at 2.
	if err := q.RecordCIAutofixPipeline(ctx, store.RecordCIAutofixPipelineParams{
		RepoID: repoID, Ref: ref, LastPipelineID: pgtype.Int8{Int64: 9020, Valid: true},
	}); err != nil {
		t.Fatalf("RecordCIAutofixPipeline: %v", err)
	}
	row, _ = q.GetCIAutofixAttempt(ctx, getAttempt)
	if row.AttemptCount != 2 {
		t.Fatalf("RecordCIAutofixPipeline must NOT advance the counter, got count=%d", row.AttemptCount)
	}
	if row.LastPipelineID.Int64 != 9020 {
		t.Fatalf("RecordCIAutofixPipeline must move last_pipeline_id to 9020, got %d", row.LastPipelineID.Int64)
	}

	// Halt latch.
	if err := q.SetCIAutofixHaltNotified(ctx, store.SetCIAutofixHaltNotifiedParams{
		RepoID: repoID, Ref: ref, LastPipelineID: pgtype.Int8{Int64: 9030, Valid: true},
	}); err != nil {
		t.Fatalf("SetCIAutofixHaltNotified: %v", err)
	}
	row, _ = q.GetCIAutofixAttempt(ctx, getAttempt)
	if !row.HaltNotified || row.LastPipelineID.Int64 != 9030 {
		t.Fatalf("halt latch: got halt=%v pid=%d, want true/9030", row.HaltNotified, row.LastPipelineID.Int64)
	}

	// RecordCIAutofixPipeline on a NEVER-attempted ref inserts a row at the default
	// count (0) — proving the silent path can create as well as update.
	if err := q.RecordCIAutofixPipeline(ctx, store.RecordCIAutofixPipelineParams{
		RepoID: repoID, Ref: "agent/issue-99", LastPipelineID: pgtype.Int8{Int64: 42, Valid: true},
	}); err != nil {
		t.Fatalf("RecordCIAutofixPipeline (insert): %v", err)
	}
	if fresh, err := q.GetCIAutofixAttempt(ctx, store.GetCIAutofixAttemptParams{RepoID: repoID, Ref: "agent/issue-99"}); err != nil || fresh.AttemptCount != 0 {
		t.Fatalf("silent-path insert must start at count=0, got count=%d err=%v", fresh.AttemptCount, err)
	}

	// Reset-on-green: single-ref delete returns 1 for a present row.
	n, err := q.DeleteCIAutofixAttempt(ctx, store.DeleteCIAutofixAttemptParams{RepoID: repoID, Ref: ref})
	if err != nil || n != 1 {
		t.Fatalf("DeleteCIAutofixAttempt = %d, %v; want 1", n, err)
	}
	if n, _ := q.DeleteCIAutofixAttempt(ctx, store.DeleteCIAutofixAttemptParams{RepoID: repoID, Ref: ref}); n != 0 {
		t.Fatalf("DeleteCIAutofixAttempt on an absent ref must be a no-op, got %d", n)
	}

	// Reconcile eviction: keep only agent/issue-99, drop everything else. Seed two
	// more rows so the eviction has something to remove.
	if err := q.UpsertCIAutofixAttempt(ctx, store.UpsertCIAutofixAttemptParams{RepoID: repoID, Ref: "agent/issue-a"}); err != nil {
		t.Fatalf("seed a: %v", err)
	}
	if err := q.UpsertCIAutofixAttempt(ctx, store.UpsertCIAutofixAttemptParams{RepoID: repoID, Ref: "agent/issue-b"}); err != nil {
		t.Fatalf("seed b: %v", err)
	}
	evicted, err := q.DeleteCIAutofixAttemptsNotIn(ctx, store.DeleteCIAutofixAttemptsNotInParams{
		RepoID: repoID, KeepRefs: []string{"agent/issue-99"},
	})
	if err != nil || evicted != 2 {
		t.Fatalf("DeleteCIAutofixAttemptsNotIn evicted %d, %v; want 2 (a and b, keeping 99)", evicted, err)
	}
	if _, err := q.GetCIAutofixAttempt(ctx, store.GetCIAutofixAttemptParams{RepoID: repoID, Ref: "agent/issue-99"}); err != nil {
		t.Fatalf("the kept ref must survive eviction: %v", err)
	}
}

// TestCIAutofixCandidateFailureSpellingsLiveDB pins the issue #1005 forge-neutrality
// fix: ListCIAutofixCandidateRefs must match EVERY forge's failure spelling, not just
// GitLab's "failed". ps.status is the RAW forge pipeline status (GitHub Actions stores
// "failure"/"timed_out"/"startup_failure"; Forgejo "failure"/"error"), so a GitHub or
// Forgejo failed pipeline was silently never a candidate under the old `= 'failed'`
// predicate — the reported symptom ("no ci_fix run has ever existed" on a GitHub repo
// even with auto-fix enabled). This test seeds three otherwise-eligible agent-MR
// branches whose pipeline statuses are "failure", "timed_out", and "error", and asserts
// all three surface. It FAILS on the old `= 'failed'` predicate (0 candidates) and
// PASSES with the IN-list fix. Its own repo/connection/user keep it independent of
// TestCIAutofixLiveDB's exact-count assertion.
func TestCIAutofixCandidateFailureSpellingsLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
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

	owner := uuid.New()
	connID, repoID := uuid.New(), uuid.New()
	mustExec(ctx, t, pool,
		`INSERT INTO users (id, email, password_hash, ci_autofix_enabled) VALUES ($1, $2, 'x', true)`,
		owner, fmt.Sprintf("autofix-spellings-%s@e2e", owner))
	mustExec(ctx, t, pool,
		`INSERT INTO user_secrets (user_id, kind, ciphertext, label) VALUES ($1, 'anthropic_token', $2, 'default')`,
		owner, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'github', 'https://github.e2e', 'bot', 1, $3)`, connID, owner, []byte{0x1})
	// Distinct forge_project_id from TestCIAutofixLiveDB (UNIQUE(connection_id,
	// forge_project_id)); this test owns its own connection anyway.
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 2, 'gh/r', 'https://github.e2e/gh/r', 'main', true)`, repoID, connID)

	insertRun := func(branch string, issueIID, mrIID int64) {
		mustExec(ctx, t, pool,
			`INSERT INTO runs (user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, branch, mr_iid, created_at)
			 VALUES ($1, $2, 'issue', $3, 't', 'd', 'completed', $4, $5, now())`,
			owner, repoID, issueIID, branch, mrIID)
	}
	insertPipeline := func(ref string, pipelineID int64, status string) {
		mustExec(ctx, t, pool,
			`INSERT INTO pipeline_statuses (repo_id, ref, pipeline_id, sha, status, web_url, synced_at)
			 VALUES ($1, $2, $3, 'deadbeef', $4, 'https://github.e2e/p', now())`,
			repoID, ref, pipelineID, status)
	}

	// Three eligible agent-MR branches, each with a non-GitLab failure spelling. Under
	// the old `= 'failed'` predicate NONE of these match; under the fix all three do.
	want := map[string]int64{
		"agent/issue-101": 7101, // GitHub Actions run conclusion "failure"
		"agent/issue-102": 7102, // GitHub Actions run conclusion "timed_out"
		"agent/issue-103": 7103, // Forgejo CommitStatusState "error"
	}
	insertRun("agent/issue-101", 101, 201)
	insertPipeline("agent/issue-101", 7101, "failure")
	insertRun("agent/issue-102", 102, 202)
	insertPipeline("agent/issue-102", 7102, "timed_out")
	insertRun("agent/issue-103", 103, 203)
	insertPipeline("agent/issue-103", 7103, "error")

	// Negative boundary: two branches eligible in every other way (same owner, token,
	// opt-in, non-default branch, mr_iid) but whose pipeline status is a deliberate
	// NON-failure — 'cancelled' (a human cancel) and 'success' (a pass). Neither is a
	// failure, so neither may surface as a candidate. Guards the ps.status IN (...)
	// list against a future accidental superset.
	notCandidates := []string{"agent/issue-104", "agent/issue-105"}
	insertRun("agent/issue-104", 104, 204)
	insertPipeline("agent/issue-104", 7104, "cancelled")
	insertRun("agent/issue-105", 105, 205)
	insertPipeline("agent/issue-105", 7105, "success")

	cands, err := q.ListCIAutofixCandidateRefs(ctx, repoID)
	if err != nil {
		t.Fatalf("ListCIAutofixCandidateRefs: %v", err)
	}
	got := make(map[string]int64, len(cands))
	for _, c := range cands {
		got[c.Ref.String] = c.PipelineID
	}
	if len(got) != len(want) {
		t.Fatalf("want %d candidates (failure/timed_out/error spellings), got %d: %+v", len(want), len(got), cands)
	}
	for ref, pid := range want {
		if got[ref] != pid {
			t.Errorf("branch %q with a non-GitLab failure spelling must be a candidate (pipeline_id %d), got %d — the ps.status predicate hardcodes GitLab's \"failed\" spelling",
				ref, pid, got[ref])
		}
	}
	for _, ref := range notCandidates {
		if _, ok := got[ref]; ok {
			t.Errorf("branch %q has a NON-failure pipeline status (cancelled/success) and must NOT be an auto-fix candidate — the ps.status IN (...) predicate has become a superset of the failure set",
				ref)
		}
	}
}
