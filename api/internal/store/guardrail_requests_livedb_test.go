package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1432 M1 against a REAL Postgres. sqlc/go build/go vet pass regardless of
// whether the ON CONFLICT arbiter, the single-shot decide guard, the DISTINCT ON
// latest-per-repo pick, and the tenant-boundary join actually behave — only
// executing the SQL against Postgres proves them. Covers:
//
//	(a) UpsertGuardrailOverrideRequest: first call inserts a pending row; a second
//	    call for the SAME repo UPDATEs it in place (one row, new reason/findings) —
//	    the partial-unique-index arbiter (repo_id WHERE status = 'pending').
//	(b) DecideGuardrailOverrideRequest: the first decide returns the settled row; a
//	    second decide on the same id returns pgx.ErrNoRows (already decided) — the
//	    `AND status = 'pending'` single-shot guard.
//	(c) ListGuardrailOverrideRequestsForUser: the LATEST request per repo, scoped to
//	    the caller — a second user's repo+request is NOT returned (the connection
//	    join is the tenant boundary).
//	(d) ListPendingGuardrailOverrideRequests: only pending rows, with the joined
//	    path / owner_email / forge_type; a decided row is excluded.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.
func TestGuardrailOverrideRequestLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run against a throwaway Postgres for live-DB coverage")
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

	// Two tenants. Emails are prefixed a-/z- so the admin pending queue's
	// `ORDER BY u.email ASC` is deterministic (user1 before user2), and the admin
	// decider (m-) owns no connection so it never appears as an owner.
	user1, user2, adminID := uuid.New(), uuid.New(), uuid.New()
	conn1, conn2 := uuid.New(), uuid.New()
	repo1, repo2, repo3 := uuid.New(), uuid.New(), uuid.New()

	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		user1, fmt.Sprintf("a-cdr1432-%s@e2e", user1))
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		user2, fmt.Sprintf("z-cdr1432-%s@e2e", user2))
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		adminID, fmt.Sprintf("m-cdr1432-%s@e2e", adminID))

	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'github', 'https://forge.e2e', 'bot', 1, $3)`, conn1, user1, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 2, $3)`, conn2, user2, []byte{0x2})

	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch)
		 VALUES ($1, $2, 1, 'u1/repo-alpha', 'https://forge.e2e/u1/repo-alpha', 'main')`, repo1, conn1)
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch)
		 VALUES ($1, $2, 2, 'u1/repo-beta', 'https://forge.e2e/u1/repo-beta', 'main')`, repo2, conn1)
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch)
		 VALUES ($1, $2, 3, 'u2/repo-gamma', 'https://forge.e2e/u2/repo-gamma', 'main')`, repo3, conn2)

	// --- (a) upsert: insert then conflict-update the pending row ------------
	findingsA := []byte(`[{"code":"default_branch_unprotected","severity":"block","message":"unprotected"}]`)
	req, err := q.UpsertGuardrailOverrideRequest(ctx, store.UpsertGuardrailOverrideRequestParams{
		RepoID:      repo1,
		RequestedBy: user1,
		Reason:      "please allow alpha",
		Findings:    findingsA,
	})
	if err != nil {
		t.Fatalf("UpsertGuardrailOverrideRequest insert: %v", err)
	}
	if req.Status != "pending" {
		t.Errorf("status after insert = %q, want pending", req.Status)
	}
	if req.Reason != "please allow alpha" {
		t.Errorf("reason after insert = %q, want %q", req.Reason, "please allow alpha")
	}
	if got := decodeFindingCodes(t, req.Findings); len(got) != 1 || got[0] != "default_branch_unprotected" {
		t.Errorf("findings after insert = %v, want [default_branch_unprotected]", got)
	}
	if req.DecidedBy.Valid || req.DecidedAt.Valid || req.DecisionNote.Valid {
		t.Errorf("a fresh request must have NULL decided_by/decided_at/decision_note, got %+v", req)
	}

	findingsB := []byte(`[{"code":"bot_can_merge","severity":"block","message":"bot can merge"},{"code":"write_role_can_push","severity":"block","message":"write can push"}]`)
	req2, err := q.UpsertGuardrailOverrideRequest(ctx, store.UpsertGuardrailOverrideRequestParams{
		RepoID:      repo1,
		RequestedBy: user1,
		Reason:      "second try, more context",
		Findings:    findingsB,
	})
	if err != nil {
		t.Fatalf("UpsertGuardrailOverrideRequest conflict-update: %v", err)
	}
	if req2.ID != req.ID {
		t.Errorf("conflict-update created a NEW row (id %s != %s); ON CONFLICT (repo_id) WHERE status='pending' did not fire", req2.ID, req.ID)
	}
	if req2.Reason != "second try, more context" {
		t.Errorf("reason after update = %q, want %q", req2.Reason, "second try, more context")
	}
	if got := decodeFindingCodes(t, req2.Findings); len(got) != 2 {
		t.Errorf("findings after update = %v, want 2 codes (EXCLUDED.findings applied)", got)
	}
	var repo1Rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM guardrail_override_requests WHERE repo_id = $1`, repo1).Scan(&repo1Rows); err != nil {
		t.Fatalf("count repo1 requests: %v", err)
	}
	if repo1Rows != 1 {
		t.Fatalf("repo1 has %d request rows after two upserts, want exactly 1 (the arbiter updated in place)", repo1Rows)
	}

	// --- (b) decide is single-shot -----------------------------------------
	decided, err := q.DecideGuardrailOverrideRequest(ctx, store.DecideGuardrailOverrideRequestParams{
		ID:           req.ID,
		Status:       "approved",
		DecidedBy:    adminID,
		DecisionNote: pgtype.Text{String: "approved by admin", Valid: true},
	})
	if err != nil {
		t.Fatalf("DecideGuardrailOverrideRequest first decide: %v", err)
	}
	if decided.Status != "approved" {
		t.Errorf("status after decide = %q, want approved", decided.Status)
	}
	if !decided.DecidedBy.Valid || uuid.UUID(decided.DecidedBy.Bytes) != adminID {
		t.Errorf("decided_by = %+v, want %s", decided.DecidedBy, adminID)
	}
	if !decided.DecidedAt.Valid {
		t.Error("decided_at must be stamped by the decide")
	}
	if decided.DecisionNote.String != "approved by admin" || !decided.DecisionNote.Valid {
		t.Errorf("decision_note = %+v, want \"approved by admin\"", decided.DecisionNote)
	}
	// A second decide on the same id must match no PENDING row and return ErrNoRows.
	if _, err := q.DecideGuardrailOverrideRequest(ctx, store.DecideGuardrailOverrideRequestParams{
		ID:        req.ID,
		Status:    "rejected",
		DecidedBy: adminID,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("second decide on an already-decided id err = %v, want pgx.ErrNoRows", err)
	}
	// The row itself is untouched by the refused second decide.
	after, err := q.GetGuardrailOverrideRequest(ctx, req.ID)
	if err != nil {
		t.Fatalf("GetGuardrailOverrideRequest: %v", err)
	}
	if after.Status != "approved" {
		t.Errorf("status after a refused second decide = %q, want approved (unchanged)", after.Status)
	}

	// --- (c) ListForUser: latest per repo, tenant-scoped -------------------
	// repo1's only request is now approved. Push its created_at into the past, then
	// open a FRESH pending request on repo1 (no conflict — the approved row left the
	// partial index) so the newest-per-repo pick is unambiguous.
	mustExec(ctx, t, pool,
		`UPDATE guardrail_override_requests SET created_at = now() - interval '1 hour' WHERE id = $1`, req.ID)
	newReq, err := q.UpsertGuardrailOverrideRequest(ctx, store.UpsertGuardrailOverrideRequestParams{
		RepoID:      repo1,
		RequestedBy: user1,
		Reason:      "re-requesting after the first was approved",
		Findings:    findingsA,
	})
	if err != nil {
		t.Fatalf("UpsertGuardrailOverrideRequest fresh pending after decide: %v", err)
	}
	if newReq.ID == req.ID || newReq.Status != "pending" {
		t.Fatalf("expected a NEW pending row on repo1, got id=%s status=%s (req.ID=%s)", newReq.ID, newReq.Status, req.ID)
	}
	// repo2 (user1) gets a pending request; repo3 (user2) gets one that must never
	// surface for user1.
	if _, err := q.UpsertGuardrailOverrideRequest(ctx, store.UpsertGuardrailOverrideRequestParams{
		RepoID: repo2, RequestedBy: user1, Reason: "allow beta", Findings: findingsA,
	}); err != nil {
		t.Fatalf("UpsertGuardrailOverrideRequest repo2: %v", err)
	}
	if _, err := q.UpsertGuardrailOverrideRequest(ctx, store.UpsertGuardrailOverrideRequestParams{
		RepoID: repo3, RequestedBy: user2, Reason: "allow gamma", Findings: findingsA,
	}); err != nil {
		t.Fatalf("UpsertGuardrailOverrideRequest repo3: %v", err)
	}

	forUser, err := q.ListGuardrailOverrideRequestsForUser(ctx, user1)
	if err != nil {
		t.Fatalf("ListGuardrailOverrideRequestsForUser: %v", err)
	}
	byRepo := map[uuid.UUID]store.GuardrailOverrideRequest{}
	for _, r := range forUser {
		if _, dup := byRepo[r.RepoID]; dup {
			t.Errorf("ListGuardrailOverrideRequestsForUser returned >1 row for repo %s (DISTINCT ON failed)", r.RepoID)
		}
		byRepo[r.RepoID] = r
	}
	if len(byRepo) != 2 {
		t.Fatalf("ListForUser returned %d repos for user1, want 2 (repo1, repo2)", len(byRepo))
	}
	if _, ok := byRepo[repo3]; ok {
		t.Error("ListForUser leaked user2's repo3 request — the connection join is not scoping the tenant")
	}
	if got := byRepo[repo1]; got.ID != newReq.ID {
		t.Errorf("ListForUser repo1 row id = %s, want the NEWEST %s (DISTINCT ON newest-per-repo)", got.ID, newReq.ID)
	}
	if got := byRepo[repo2]; got.Status != "pending" {
		t.Errorf("ListForUser repo2 status = %q, want pending", got.Status)
	}

	// --- (d) ListPending: only pending, with the joined display fields ------
	pending, err := q.ListPendingGuardrailOverrideRequests(ctx)
	if err != nil {
		t.Fatalf("ListPendingGuardrailOverrideRequests: %v", err)
	}
	// This query is UNSCOPED (all users), so the DB may hold rows from a prior run on
	// the same throwaway Postgres. Assert the always-true invariants globally, then
	// pin THIS run's three seeded rows as an ordered subsequence — the query's
	// `ORDER BY u.email ASC, r.path_with_namespace ASC` preserves their relative order
	// regardless of what else is interleaved.
	mine := map[uuid.UUID]store.ListPendingGuardrailOverrideRequestsRow{}
	var seededSeq []uuid.UUID
	for _, p := range pending {
		if p.Status != "pending" {
			t.Errorf("ListPending returned a non-pending row (id %s, status %q)", p.ID, p.Status)
		}
		if p.ID == req.ID {
			t.Errorf("ListPending included the approved request %s", req.ID)
		}
		switch p.RepoID {
		case repo1, repo2, repo3:
			mine[p.RepoID] = p
			seededSeq = append(seededSeq, p.RepoID)
		}
	}
	// Ordered by owner_email ASC then repo path ASC: user1 (a-) rows first by path
	// (repo-alpha, repo-beta), then user2 (z-) repo-gamma.
	wantSeq := []uuid.UUID{repo1, repo2, repo3}
	if len(seededSeq) != len(wantSeq) {
		t.Fatalf("ListPending held %d of this run's seeded repos, want 3 (repo1 newReq, repo2, repo3)", len(seededSeq))
	}
	for i, want := range wantSeq {
		if seededSeq[i] != want {
			t.Errorf("ListPending seeded-row order[%d] = %s, want %s (owner_email ASC, path ASC)", i, seededSeq[i], want)
		}
	}
	// The joined display fields on each seeded row.
	wantFields := []struct {
		repoID     uuid.UUID
		path       string
		ownerID    uuid.UUID
		ownerEmail string
		forgeType  string
	}{
		{repo1, "u1/repo-alpha", user1, fmt.Sprintf("a-cdr1432-%s@e2e", user1), "github"},
		{repo2, "u1/repo-beta", user1, fmt.Sprintf("a-cdr1432-%s@e2e", user1), "github"},
		{repo3, "u2/repo-gamma", user2, fmt.Sprintf("z-cdr1432-%s@e2e", user2), "gitlab"},
	}
	for _, w := range wantFields {
		p := mine[w.repoID]
		if p.PathWithNamespace != w.path {
			t.Errorf("ListPending repo %s path_with_namespace = %q, want %q", w.repoID, p.PathWithNamespace, w.path)
		}
		if p.OwnerID != w.ownerID {
			t.Errorf("ListPending repo %s owner_id = %s, want %s", w.repoID, p.OwnerID, w.ownerID)
		}
		if p.OwnerEmail != w.ownerEmail {
			t.Errorf("ListPending repo %s owner_email = %q, want %q", w.repoID, p.OwnerEmail, w.ownerEmail)
		}
		if p.ForgeType != w.forgeType {
			t.Errorf("ListPending repo %s forge_type = %q, want %q", w.repoID, p.ForgeType, w.forgeType)
		}
	}
}

// decodeFindingCodes unmarshals a jsonb findings blob into its list of codes. The
// blob round-trips through Postgres jsonb, which may reorder/reformat, so the test
// asserts on the decoded codes rather than the raw bytes.
func decodeFindingCodes(t *testing.T, raw []byte) []string {
	t.Helper()
	var fs []struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(raw, &fs); err != nil {
		t.Fatalf("unmarshal findings %q: %v", raw, err)
	}
	codes := make([]string, len(fs))
	for i, f := range fs {
		codes[i] = f.Code
	}
	return codes
}
