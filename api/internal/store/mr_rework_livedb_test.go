package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestMRReworkLiveDB exercises the PRD #700 M3 SQL against a REAL Postgres — the parts
// only a live DB can answer: the candidate query's opted-in + opened-MR + green-vs-red
// gates, the create-time CROSS-KIND branch guard keyed on the pipeline_ref column
// (SC4), the runs_kind_shape CHECK for the new mr_rework kind, and the ledger's
// advance-only GREATEST semantics.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (per the store
// live-DB harness). A package that prints `ok` with PASS=0 is INVALID, not green.
func TestMRReworkLiveDB(t *testing.T) {
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

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}

	// A repo owned by an opted-in user (mr_rework_enabled NULL = default-ON) and a repo
	// owned by an opted-OUT user (mr_rework_enabled=false), each with a distinct branch,
	// so the candidate query's opt-in gate can be asserted both ways in one pass.
	inUser, outUser := uuid.New(), uuid.New()
	connID, repoID := uuid.New(), uuid.New()
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, inUser, fmt.Sprintf("in-%s@e2e", inUser))
	exec(`INSERT INTO users (id, email, password_hash, mr_rework_enabled) VALUES ($1, $2, 'x', false)`, outUser, fmt.Sprintf("out-%s@e2e", outUser))
	// The candidate query gates on the owner having an Anthropic token on file (an
	// mr_rework run executes on the owner's token), mirroring ListCIAutofixCandidateRefs.
	// Give the opted-in owner one so it surfaces as a candidate.
	exec(`INSERT INTO user_secrets (user_id, kind, label, is_default, ciphertext, sealed_with)
	      VALUES ($1, 'anthropic_token', 'default', true, $2, 'master')`, inUser, []byte{0x2})
	// Give the opted-OUT owner a token too. Without it, outUser would be excluded for
	// TWO reasons (no token AND mr_rework_enabled=false), so its absence from cands
	// would not prove the opt-out gate specifically. With a valid token present, the
	// only remaining reason it is excluded is mr_rework_enabled=false — making the
	// opt-out assertion below non-vacuous.
	exec(`INSERT INTO user_secrets (user_id, kind, label, is_default, ciphertext, sealed_with)
	      VALUES ($1, 'anthropic_token', 'default', true, $2, 'master')`, outUser, []byte{0x3})
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 777, $3)`, connID, inUser, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 1, 'g/m3', 'https://forge.e2e/g/m3', 'main', true)`, repoID, connID)

	// seedIssueRun inserts a completed issue run with an open MR on agent/issue-<iid>.
	seedIssueRun := func(owner uuid.UUID, iid int64, mrState string, sha string) uuid.UUID {
		t.Helper()
		id := uuid.New()
		branch := fmt.Sprintf("agent/issue-%d", iid)
		exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, branch, mr_iid, mr_state, status)
		      VALUES ($1, $2, $3, 'issue', $4, 't', 'd', $5, $6, $7, 'completed')`,
			id, owner, repoID, iid, branch, iid*10, mrState)
		exec(`INSERT INTO pipeline_statuses (repo_id, ref, pipeline_id, sha, status, web_url, synced_at)
		      VALUES ($1, $2, $3, $4, 'success', 'https://forge.e2e/p', now())`, repoID, branch, iid*100, sha)
		return id
	}

	// seedScheduledRun inserts a completed scheduled-lane run (kind='prompt' or
	// 'self_improve') with an MR on the given branch (PRD #908 widened the candidate kind
	// filter to include these). It stamps the WATCHER-OWNED runs.mr_state (nil ⇒ SQL NULL,
	// the pre-recorder bootstrap state) and the per-run mr_rework_enabled override (nil ⇒
	// SQL NULL). Per the runs_kind_shape CHECK, prompt carries issue_iid NULL and
	// self_improve a non-null issue_iid, so issueIID is threaded through as *int64. A green
	// head pipeline is seeded so the branch is fully realistic; the candidate query LEFT
	// JOINs it, so it is not what gates admission — runs.mr_state is.
	seedScheduledRun := func(owner uuid.UUID, kind, branch string, mrIID int64, mrState *string, mrRework *bool, issueIID *int64) uuid.UUID {
		t.Helper()
		id := uuid.New()
		exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, branch, mr_iid, mr_state, status, mr_rework_enabled)
		      VALUES ($1, $2, $3, $4, $5, 't', 'd', $6, $7, $8, 'completed', $9)`,
			id, owner, repoID, kind, issueIID, branch, mrIID, mrState, mrRework)
		exec(`INSERT INTO pipeline_statuses (repo_id, ref, pipeline_id, sha, status, web_url, synced_at)
		      VALUES ($1, $2, $3, $4, 'success', 'https://forge.e2e/p', now())`, repoID, branch, mrIID*100, "sha-"+branch)
		return id
	}
	sp := func(s string) *string { return &s }
	bp := func(b bool) *bool { return &b }
	ip := func(n int64) *int64 { return &n }

	// PRD #908 scheduled-lane candidates (owned by the opted-in inUser, opened MR, token
	// present, no opt-out anywhere) — both must surface, proving the kind filter widened
	// beyond issue.
	promptCandBranch := "uzi/prompt-" + uuid.New().String()
	srcPrompt := seedScheduledRun(inUser, "prompt", promptCandBranch, 7010, sp("opened"), nil, nil)
	selfImpCandBranch := "uzi/self-improve/" + uuid.New().String()
	srcSelfImp := seedScheduledRun(inUser, "self_improve", selfImpCandBranch, 7020, sp("opened"), nil, ip(7021))

	// Opt-out excludes, both layers of the COALESCE(per-run, owner) chain:
	//   - per-RUN override false (owner inUser defaults ON) → excluded.
	promptRunOptOutBranch := "uzi/prompt-" + uuid.New().String()
	seedScheduledRun(inUser, "prompt", promptRunOptOutBranch, 7030, sp("opened"), bp(false), nil)
	//   - OWNER default false with a NULL run column (outUser) → excluded.
	selfImpOwnerOptOutBranch := "uzi/self-improve/" + uuid.New().String()
	seedScheduledRun(outUser, "self_improve", selfImpOwnerOptOutBranch, 7040, sp("opened"), nil, ip(7041))

	// mr_state gate: a non-'opened' watcher-owned state must be excluded, proving the M3
	// recorder gate matters (the scheduled lane is eligible only while its MR is opened).
	//   - 'merged' (terminal) → excluded.
	promptMergedBranch := "uzi/prompt-" + uuid.New().String()
	seedScheduledRun(inUser, "prompt", promptMergedBranch, 7050, sp("merged"), nil, nil)
	//   - NULL (pre-recorder bootstrap, mr_state = 'opened' is false for NULL) → excluded.
	promptNullStateBranch := "uzi/prompt-" + uuid.New().String()
	seedScheduledRun(inUser, "prompt", promptNullStateBranch, 7060, nil, nil, nil)

	// Opted-in, opened MR, green pipeline → a candidate.
	src7 := seedIssueRun(inUser, 7, "opened", "sha7")
	// Opted-in, but the MR is CLOSED → excluded (stop-on-close, Decision 10).
	seedIssueRun(inUser, 8, "closed", "sha8")
	// Opted-OUT owner → excluded even with an opened MR.
	exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, branch, mr_iid, mr_state, status)
	      VALUES ($1, $2, $3, 'issue', 9, 't', 'd', 'agent/issue-9', 90, 'opened', 'completed')`,
		uuid.New(), outUser, repoID)

	cands, err := q.ListMRReworkCandidates(ctx, repoID)
	if err != nil {
		t.Fatalf("ListMRReworkCandidates: %v", err)
	}
	// The exact candidate SET, keyed by branch → source_run_id. Precise (not a bare count):
	// the issue-lane candidate PLUS the two scheduled-lane candidates survive; every opt-out
	// and non-opened-state seed is absent. A stray candidate or a missing one both fail.
	bySrc := map[string]uuid.UUID{}
	byRow := map[string]store.ListMRReworkCandidatesRow{}
	for _, cand := range cands {
		bySrc[cand.Ref.String] = cand.SourceRunID
		byRow[cand.Ref.String] = cand
	}
	wantCands := map[string]uuid.UUID{
		"agent/issue-7":   src7,
		promptCandBranch:  srcPrompt,
		selfImpCandBranch: srcSelfImp,
	}
	if len(cands) != len(wantCands) {
		t.Fatalf("expected exactly %d candidates (issue-7 + prompt + self_improve), got %d: %+v", len(wantCands), len(cands), cands)
	}
	for ref, wantSrc := range wantCands {
		if bySrc[ref] != wantSrc {
			t.Errorf("candidate %q must surface via source_run_id %s, got %s (set %+v)", ref, wantSrc, bySrc[ref], bySrc)
		}
	}
	for _, excluded := range []string{promptRunOptOutBranch, selfImpOwnerOptOutBranch, promptMergedBranch, promptNullStateBranch} {
		if _, ok := bySrc[excluded]; ok {
			t.Errorf("branch %q must be excluded (opt-out or non-opened mr_state), but it surfaced", excluded)
		}
	}

	// The issue-lane candidate still carries its mr_iid, bot_forge_user_id, and green head
	// pipeline — the scheduled-lane widening must not disturb it.
	c := byRow["agent/issue-7"]
	if c.MrIid.Int64 != 70 || c.SourceRunID != src7 {
		t.Fatalf("issue-7 candidate row wrong: %+v", c)
	}
	if c.BotForgeUserID != 777 {
		t.Fatalf("bot_forge_user_id not projected: %d", c.BotForgeUserID)
	}
	if !c.PipelineStatus.Valid || c.PipelineStatus.String != "success" || c.PipelineSha.String != "sha7" {
		t.Fatalf("green head pipeline not joined: %+v", c)
	}

	// SC4 — the create-time CROSS-KIND branch guard is the create query's own atomic
	// INSERT … WHERE NOT EXISTS, keyed on the pipeline_ref COLUMN. An active ci_fix run
	// created with pipeline_ref=agent/issue-7 (populated AT INSERT, NOT back-filled onto
	// runs.branch) occupies the branch, so a CreateAutoMRReworkRun for that SAME
	// occupied ref matches zero rows (WHERE NOT EXISTS) and the generated :one returns
	// pgx.ErrNoRows — the caller maps that to ErrBranchInUse. mr_iid=700 is distinct from
	// every active mr_rework here, so the ONLY thing blocking the insert is the occupied
	// branch, not the same-MR unique index.
	exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_title, issue_description, pipeline_id, pipeline_ref, status)
	      VALUES ($1, $2, $3, 'ci_fix', 't', 'd', 4242, 'agent/issue-7', 'running')`,
		uuid.New(), inUser, repoID)
	if _, err := q.CreateAutoMRReworkRun(ctx, store.CreateAutoMRReworkRunParams{
		UserID:           inUser,
		RepoID:           repoID,
		IssueTitle:       "Rework MR review (occupied)",
		IssueDescription: "d",
		PipelineRef:      pgtype.Text{String: "agent/issue-7", Valid: true},
		MrIid:            pgtype.Int8{Int64: 700, Valid: true},
		TargetRunID:      pgtype.UUID{Bytes: src7, Valid: true},
		ReviewComments:   []byte(`{"comments":[{"id":120}],"truncated":false}`),
		WaitOnLimit:      false,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("CreateAutoMRReworkRun on an occupied branch: err = %v, want pgx.ErrNoRows (WHERE NOT EXISTS → 0 rows)", err)
	}

	// The runs_kind_shape CHECK accepts a valid mr_rework row (repo_id, pipeline_ref,
	// mr_iid, target_run_id all present). Insert on a DIFFERENT ref so the cross-kind
	// unique index does not collide with the ci_fix above.
	run, err := q.CreateAutoMRReworkRun(ctx, store.CreateAutoMRReworkRunParams{
		UserID:           inUser,
		RepoID:           repoID,
		IssueTitle:       "Rework MR review",
		IssueDescription: "d",
		PipelineRef:      pgtype.Text{String: "agent/issue-7b", Valid: true},
		MrIid:            pgtype.Int8{Int64: 71, Valid: true},
		TargetRunID:      pgtype.UUID{Bytes: src7, Valid: true},
		ReviewComments:   []byte(`{"comments":[{"id":120}],"truncated":false}`),
		WaitOnLimit:      false,
	})
	if err != nil {
		t.Fatalf("CreateAutoMRReworkRun (valid shape): %v", err)
	}
	if run.Kind != "mr_rework" || !run.AutoApprove {
		t.Fatalf("mr_rework run shape wrong: kind=%q auto_approve=%v", run.Kind, run.AutoApprove)
	}

	// The shape CHECK REJECTS an mr_rework missing target_run_id.
	if _, err := pool.Exec(ctx,
		`INSERT INTO runs (id, user_id, repo_id, kind, issue_title, issue_description, pipeline_ref, mr_iid, status)
		 VALUES ($1, $2, $3, 'mr_rework', 't', 'd', 'agent/issue-99', 99, 'queued')`,
		uuid.New(), inUser, repoID); err == nil {
		t.Fatal("runs_kind_shape must reject an mr_rework with no target_run_id")
	}

	// Ledger: first upsert INSERTs attempt_count=1 / high_water=120; a second upsert
	// with a SMALLER high_water leaves the mark unchanged (advance-only GREATEST) while
	// still incrementing the counter.
	up := func(hw int64) {
		t.Helper()
		if err := q.UpsertMRReworkLedger(ctx, store.UpsertMRReworkLedgerParams{RepoID: repoID, Ref: "agent/issue-7", HighWater: hw}); err != nil {
			t.Fatalf("UpsertMRReworkLedger(%d): %v", hw, err)
		}
	}
	up(120)
	up(90) // smaller — must NOT lower the mark
	led, err := q.GetMRReworkLedger(ctx, store.GetMRReworkLedgerParams{RepoID: repoID, Ref: "agent/issue-7"})
	if err != nil {
		t.Fatalf("GetMRReworkLedger: %v", err)
	}
	if led.HighWater != 120 {
		t.Fatalf("high_water must be advance-only, got %d want 120", led.HighWater)
	}
	if led.AttemptCount != 2 {
		t.Fatalf("attempt_count = %d, want 2", led.AttemptCount)
	}

	// Reconcile eviction with an empty keep-set clears the ledger (stop-on-merge cleanup).
	if _, err := q.DeleteMRReworkLedgerNotIn(ctx, store.DeleteMRReworkLedgerNotInParams{RepoID: repoID, KeepRefs: []string{}}); err != nil {
		t.Fatalf("DeleteMRReworkLedgerNotIn: %v", err)
	}
	if _, err := q.GetMRReworkLedger(ctx, store.GetMRReworkLedgerParams{RepoID: repoID, Ref: "agent/issue-7"}); err == nil {
		t.Fatal("expected the ledger row to be evicted")
	}
}

// TestMRReworkCoalesceLiveDB proves the PRD #841 M1 live-inherit resolution: the candidate
// query's eligibility filter is COALESCE(per_branch.mr_rework_enabled, u.mr_rework_enabled)
// IS NOT FALSE, so the per-RUN override (runs.mr_rework_enabled, nullable) coalesces OVER
// the owner default (users.mr_rework_enabled, nullable, default-ON per 00165), read LIVE.
//
// This can only be answered by a real Postgres — sqlc's type deduction is not Postgres's,
// and a fold applied to the .sql only (not the generated const) is inert (.claude/rules/go.md).
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; it runs in CI's
// store-it sweep. A package printing `ok` with PASS=0 is INVALID, not green.
//
// The four truth-table rows are chosen so a REGRESSION reverting the filter to the pre-841
// `u.mr_rework_enabled IS NOT FALSE` (owner-only) would FAIL this test: rows 3 and 4 are the
// per-run-override-wins cases in BOTH directions, and under the owner-only filter row 3
// (owner true) would wrongly surface and row 4 (owner false) would wrongly vanish.
func TestMRReworkCoalesceLiveDB(t *testing.T) {
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

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	bp := func(b bool) *bool { return &b }

	// Three owners spanning the owner-default tri-state (NULL = default-ON, false = opted
	// out, true = opted in), each with an Anthropic token so the token gate never becomes
	// the reason a row is excluded (that would make the COALESCE assertions vacuous). Each
	// owns runs in ONE shared repo so ListMRReworkCandidates returns them together.
	ownerNull, ownerFalse, ownerTrue := uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, ownerNull, fmt.Sprintf("n-%s@e2e", ownerNull))
	exec(`INSERT INTO users (id, email, password_hash, mr_rework_enabled) VALUES ($1, $2, 'x', false)`, ownerFalse, fmt.Sprintf("f-%s@e2e", ownerFalse))
	exec(`INSERT INTO users (id, email, password_hash, mr_rework_enabled) VALUES ($1, $2, 'x', true)`, ownerTrue, fmt.Sprintf("t-%s@e2e", ownerTrue))
	for _, u := range []uuid.UUID{ownerNull, ownerFalse, ownerTrue} {
		exec(`INSERT INTO user_secrets (user_id, kind, label, is_default, ciphertext, sealed_with)
		      VALUES ($1, 'anthropic_token', 'default', true, $2, 'master')`, u, []byte{0x9})
	}
	connID, repoID := uuid.New(), uuid.New()
	// The repo's connection is owned by ownerNull; ownership does not gate the candidate
	// query (it keys on the RUN's user_id via per_branch.user_id → users u), so runs owned
	// by any of the three surface together.
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 888, $3)`, connID, ownerNull, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 42, 'g/m841', 'https://forge.e2e/g/m841', 'main', true)`, repoID, connID)

	// seedRun inserts a completed issue run with an OPEN MR on the given branch, stamping
	// mr_rework_enabled (nil ⇒ SQL NULL) and an explicit created_at offset so the newest
	// run per branch is deterministic. Returns the run id.
	seedRun := func(owner uuid.UUID, iid int64, branch string, mrRework *bool, ageSeconds int) uuid.UUID {
		t.Helper()
		id := uuid.New()
		exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, branch, mr_iid, mr_state, status, mr_rework_enabled, created_at)
		      VALUES ($1, $2, $3, 'issue', $4, 't', 'd', $5, $6, 'opened', 'completed', $7, now() - make_interval(secs => $8))`,
			id, owner, repoID, iid, branch, iid*10, mrRework, ageSeconds)
		return id
	}

	// The truth table. Each row is on its OWN branch so DISTINCT ON isolates them.
	// Row 1: run NULL + owner NULL     → COALESCE(NULL,NULL)=NULL  IS NOT FALSE → candidate.
	row1 := seedRun(ownerNull, 101, "agent/issue-101", nil, 100)
	// Row 2: run NULL + owner false    → COALESCE(NULL,false)=false           → excluded.
	seedRun(ownerFalse, 102, "agent/issue-102", nil, 100)
	// Row 3: run false + owner true    → COALESCE(false,true)=false           → excluded (per-run wins).
	seedRun(ownerTrue, 103, "agent/issue-103", bp(false), 100)
	// Row 4: run true + owner false    → COALESCE(true,false)=true            → candidate (per-run wins).
	row4 := seedRun(ownerFalse, 104, "agent/issue-104", bp(true), 100)
	// Row 5 (reused branch): two completed issue runs on the SAME branch. The OLDER carries
	// mr_rework_enabled=true (would be a candidate if read), the NEWER carries false. Owner
	// default is NULL. per_branch's DISTINCT ON (r.branch) ORDER BY r.branch, created_at DESC
	// binds eligibility to the NEWEST run, so COALESCE(false,NULL)=false → excluded. Proves an
	// older run's toggle does not move eligibility (the web checkbox/CLI target the newest run).
	seedRun(ownerNull, 105, "agent/issue-105", bp(true), 200)  // older
	seedRun(ownerNull, 106, "agent/issue-105", bp(false), 100) // newer, same branch

	cands, err := q.ListMRReworkCandidates(ctx, repoID)
	if err != nil {
		t.Fatalf("ListMRReworkCandidates: %v", err)
	}
	got := map[string]uuid.UUID{}
	for _, c := range cands {
		got[c.Ref.String] = c.SourceRunID
	}

	// Exactly the two candidate rows (1 and 4) survive; the three excluded refs (rows 2, 3,
	// and the reused-branch row 5) are absent. Asserting the exact set makes the negatives
	// non-vacuous — a stray candidate or a missing one both fail.
	if len(cands) != 2 {
		t.Fatalf("expected exactly 2 candidates (rows 1 and 4), got %d: %+v", len(cands), cands)
	}
	if got["agent/issue-101"] != row1 {
		t.Fatalf("row 1 (run NULL + owner NULL) must be a candidate via its own run, got %+v", got)
	}
	if got["agent/issue-104"] != row4 {
		t.Fatalf("row 4 (run true + owner false) must be a candidate — per-run override wins over the owner default; got %+v", got)
	}
	if _, ok := got["agent/issue-102"]; ok {
		t.Fatal("row 2 (run NULL + owner false) must be excluded — inherits the owner opt-out")
	}
	if _, ok := got["agent/issue-103"]; ok {
		t.Fatal("row 3 (run false + owner true) must be excluded — per-run override wins over the owner opt-in")
	}
	if _, ok := got["agent/issue-105"]; ok {
		t.Fatal("reused branch: the NEWEST run's mr_rework_enabled=false must exclude the branch, even though an older run on it is true")
	}
}
