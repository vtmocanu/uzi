package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/notifysvc"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #333 M8 — the two end-to-end scenarios, wiring the REAL handlers/services together
// against a live DB with a STUBBED forge (an offline worker has no forge egress). Neither
// scenario re-tests one endpoint: each drives a report ⇒ … chain across the milestone seams
// (M2 capture → M3 coalesced notification → M5 filing/dismiss → M4 backlog read) so a
// regression in the SEAM between two milestones — not just inside one — reddens here.
//
// The forge is the same httptest GitLab the M5 suite uses (newFindingForgeStub), the fake
// the real GitLab driver talks to, so "an issue was filed" means the fake recorded a
// CreateIssue with the marker label and the disposition stamped `filed` — never a network
// call. Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres
// (./e2e/run-store-it.sh provides one and sweeps this package for the LiveDB suffix).

// findingsE2E stands up a Handler wired for the WHOLE flow: the real workersvc (capture +
// backlog read), the real forgesvc pointed at the httptest forge (filing), and a real
// notifysvc backed by the live notifications table so the coalescing/suppression path (M3)
// is exercised end-to-end, not mocked. It reuses fileFindingLiveDB's harness (pool, q, box,
// forge stub, settings with the DEFAULT agent-found marker) and adds the notifier the M5
// file-only handler does not need.
func findingsE2E(t *testing.T) (*Handler, *pgxpool.Pool, *store.Queries, *secretbox.Box, *findingForgeStub) {
	t.Helper()
	h, pool, q, box, fs := fileFindingLiveDB(t)
	// The live notifications table is the M3 coalescing/suppression seam. Nil Slacker →
	// inbox-only (an offline worker has no Slack egress either); the inbox row is the
	// durable half this scenario counts.
	h.SetNotifier(notifysvc.New(q, nil, 0, nil))
	return h, pool, q, box, fs
}

// findingsE2EFixture is one owner with a connected repo and a RUNNING (non-terminal) run the
// worker can report against. Fresh uuids per call — the LiveDB runner shares one database.
type findingsE2EFixture struct {
	owner  store.User
	repoID uuid.UUID
	connID uuid.UUID
}

func seedFindingsE2E(ctx context.Context, t *testing.T, pool *pgxpool.Pool, box *secretbox.Box, forgeURL string) findingsE2EFixture {
	t.Helper()
	f := findingsE2EFixture{
		owner:  store.User{ID: uuid.New(), Email: fmt.Sprintf("m8-owner-%s@e2e", uuid.NewString()[:8])},
		repoID: uuid.New(),
		connID: uuid.New(),
	}
	sealed, err := box.Seal([]byte("glpat-dummy-token"))
	if err != nil {
		t.Fatalf("seal token: %v", err)
	}
	mustExecT(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, f.owner.ID, f.owner.Email)
	mustExecT(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', $3, 'bot-o', 1, $4)`, f.connID, f.owner.ID, forgeURL, sealed)
	mustExecT(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/ra', 'https://forge.example/g/ra', 'main', true)`, f.repoID, f.connID)
	return f
}

// seedRunningRun inserts a non-terminal ('running') issue run the worker can report a finding
// against (WorkerCreateFinding rejects a terminal run). Each e2e "run" that reports a finding
// is a distinct row, so scenario (b) can re-report the same coordinate from a LATER run.
func seedRunningRun(ctx context.Context, t *testing.T, pool *pgxpool.Pool, ownerID, repoID uuid.UUID, iid int64) uuid.UUID {
	t.Helper()
	runID := uuid.New()
	mustExecT(ctx, t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind)
		 VALUES ($1, $2, $3, $4, 'Do X', 'd', 'running', 'issue')`, runID, ownerID, repoID, iid)
	return runID
}

// reportFinding drives WorkerCreateFinding as a worker on ownerID's run, returning the created
// finding id (the real endpoint's {"id":...} body). It is the M2 capture seam invoked through
// the HTTP handler, not the service directly, so the RequireWorker/derive-from-run/notify wiring
// is on the path.
func reportFinding(t *testing.T, h *Handler, ownerID, runID uuid.UUID, title, description, location string) uuid.UUID {
	t.Helper()
	wkr := store.Worker{ID: uuid.New(), UserID: ownerID}
	body, _ := json.Marshal(map[string]any{"title": title, "description": description, "location": location, "confidence": "high"})
	rec := httptest.NewRecorder()
	h.WorkerCreateFinding(rec, workerChatReq(http.MethodPost, "/api/worker/runs/x/findings", wkr, runID, string(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("WorkerCreateFinding = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode create-finding response: %v", err)
	}
	id, err := uuid.Parse(resp.ID)
	if err != nil {
		t.Fatalf("create-finding id is not a uuid: %q", resp.ID)
	}
	return id
}

// findingRowCount counts the per-run EVIDENCE rows at a coordinate (one per report; two runs
// finding the same bug write two rows — the "seen in N runs" source, D1).
func findingRowCount(ctx context.Context, t *testing.T, pool *pgxpool.Pool, userID, repoID uuid.UUID, location string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM findings WHERE user_id = $1 AND repo_id = $2 AND location = $3`,
		userID, repoID, location).Scan(&n); err != nil {
		t.Fatalf("count findings: %v", err)
	}
	return n
}

// incidentalNotificationCount is the number of inbox notification ROWS of kind
// incidental_finding for a user. Coalescing keeps one row per run, so this is the anti-nag
// assertion's instrument: it must NOT climb when a dismissed coordinate is re-reported.
func incidentalNotificationCount(ctx context.Context, t *testing.T, pool *pgxpool.Pool, userID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM notifications WHERE user_id = $1 AND kind = $2`,
		userID, notifysvc.KindIncidentalFinding).Scan(&n); err != nil {
		t.Fatalf("count notifications: %v", err)
	}
	return n
}

// listBacklog drives the real ListFindings handler for a bucket and returns the decoded DTO.
func listBacklog(t *testing.T, h *Handler, user store.User, bucket string) apitypes.IncidentalFindingBacklogDTO {
	t.Helper()
	target := "/api/findings"
	if bucket != "" {
		target += "?bucket=" + bucket
	}
	r := httptest.NewRequest(http.MethodGet, target, nil)
	r = r.WithContext(mw.ContextWithUser(r.Context(), user))
	rec := httptest.NewRecorder()
	h.ListFindings(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("ListFindings(%s) = %d, want 200; body=%s", bucket, rec.Code, rec.Body.String())
	}
	var dto apitypes.IncidentalFindingBacklogDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("decode backlog: %v", err)
	}
	return dto
}

// backlogHas reports whether the backlog carries a coordinate at `location`, returning it.
func backlogHas(dto apitypes.IncidentalFindingBacklogDTO, location string) (apitypes.IncidentalFindingDTO, bool) {
	for _, f := range dto.Findings {
		if f.Location == location {
			return f, true
		}
	}
	return apitypes.IncidentalFindingDTO{}, false
}

// ── Scenario (a): the happy path — report → notify → file → the fake Forge recorded the
// CreateIssue → the backlog shows the coordinate `filed` with the issue link ──────────────
//
// Assertions:
//  1. after the worker POST, a `findings` evidence row + an `open` disposition exist at the
//     canonicalised coordinate;
//  2. exactly one inbox notification (kind incidental_finding) was created, payload count=1
//     (M3 coalescing — the run's first finding);
//  3. filing via POST /api/findings/{id}/issue records exactly one fake-Forge CreateIssue
//     whose labels include the server marker, and stamps the disposition `filed` with
//     filed_issue_iid + filed_issue_url;
//  4. GET /api/findings?bucket=filed shows the coordinate as `filed` carrying the issue link.
func TestFindingsE2EHappyPathLiveDB(t *testing.T) {
	h, pool, _, box, fs := findingsE2E(t)
	ctx := context.Background()
	f := seedFindingsE2E(ctx, t, pool, box, fs.server.URL)
	runID := seedRunningRun(ctx, t, pool, f.owner.ID, f.repoID, 101)

	// The agent-supplied location carries cosmetic drift the server canonicalises.
	const canonical = "internal/sweep.go#sweeploop"
	findingID := reportFinding(t, h, f.owner.ID, runID,
		"leaked ticker in sweepLoop", "the sweeper starts a ticker it never stops", "./internal/Sweep.go#sweepLoop")

	// (1) evidence row + open disposition at the canonical coordinate.
	if got := findingRowCount(ctx, t, pool, f.owner.ID, f.repoID, canonical); got != 1 {
		t.Fatalf("evidence rows = %d, want 1", got)
	}
	if status, iid := dispositionStatus(ctx, t, pool, f.owner.ID, f.repoID, canonical); status != "open" || iid != nil {
		t.Fatalf("after capture: disposition = (%s, iid %v), want (open, nil)", status, iid)
	}

	// (2) exactly one incidental_finding notification, payload count=1.
	if got := incidentalNotificationCount(ctx, t, pool, f.owner.ID); got != 1 {
		t.Fatalf("incidental_finding notifications = %d, want exactly 1 (M3 coalescing)", got)
	}
	var payloadCount int
	if err := pool.QueryRow(ctx,
		`SELECT (payload->>'count')::int FROM notifications WHERE user_id = $1 AND kind = $2`,
		f.owner.ID, notifysvc.KindIncidentalFinding).Scan(&payloadCount); err != nil {
		t.Fatalf("read notification payload count: %v", err)
	}
	if payloadCount != 1 {
		t.Fatalf("notification payload count = %d, want 1", payloadCount)
	}

	// (3) file it — the fake Forge records exactly one CreateIssue with the marker label, and
	// the disposition settles to filed with the iid + url stamped.
	rr := httptest.NewRecorder()
	h.FileFinding(rr, fileFindingReq(f.owner, findingID, map[string]any{"labels": []string{"needs-triage"}}))
	if rr.Code != http.StatusCreated {
		t.Fatalf("FileFinding = %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
	var fileResp apitypes.IncidentalFindingFileResultDTO
	if err := json.Unmarshal(rr.Body.Bytes(), &fileResp); err != nil {
		t.Fatalf("decode file result: %v", err)
	}
	if fileResp.Issue.IID == 0 || fileResp.Warning != "" {
		t.Fatalf("want a created issue with no warning, got %+v", fileResp)
	}
	if fs.count() != 1 {
		t.Fatalf("fake-Forge CreateIssue calls = %d, want 1", fs.count())
	}
	if labels := fs.creates[0].Labels; !containsStr(labels, findingTestMarker) {
		t.Fatalf("filed labels %v must include the server marker %q", labels, findingTestMarker)
	}
	status, iid, url := dispositionRow(ctx, t, pool, f.owner.ID, f.repoID, canonical)
	if status != "filed" || iid == nil || *iid != fileResp.Issue.IID {
		t.Fatalf("disposition not settled: status=%s iid=%v (issue %d)", status, iid, fileResp.Issue.IID)
	}
	if url == "" || url != fileResp.Issue.WebURL {
		t.Fatalf("settle must stamp filed_issue_url; got %q, want %q", url, fileResp.Issue.WebURL)
	}

	// (4) the filed bucket shows the coordinate as filed with the issue link.
	dto := listBacklog(t, h, f.owner, "filed")
	row, ok := backlogHas(dto, canonical)
	if !ok {
		t.Fatalf("filed bucket does not carry the coordinate %q; backlog=%+v", canonical, dto.Findings)
	}
	if row.Status != "filed" {
		t.Fatalf("filed-bucket coordinate status = %q, want filed", row.Status)
	}
	if row.FiledIssueIID == nil || *row.FiledIssueIID != fileResp.Issue.IID {
		t.Fatalf("filed coordinate iid = %v, want %d", row.FiledIssueIID, fileResp.Issue.IID)
	}
	if row.FiledIssueURL == "" || row.FiledIssueURL != fileResp.Issue.WebURL {
		t.Fatalf("filed coordinate must link the issue; got %q, want %q", row.FiledIssueURL, fileResp.Issue.WebURL)
	}
	// The now-filed coordinate has left the to_file bucket.
	if _, still := backlogHas(listBacklog(t, h, f.owner, "to_file"), canonical); still {
		t.Fatalf("a filed coordinate must not remain in the to_file bucket")
	}
}

// ── Scenario (b): the anti-nag guarantee (R2) — report → dismiss → a LATER run re-reports the
// SAME coordinate → the report is recorded but does NOT re-nag and does NOT re-enter to_file;
// then a materially-DIFFERENT report on that coordinate DOES re-open and notify ───────────────
//
// Assertions across the two re-reports:
//
//	dismiss:        the coordinate is `dismissed`, one notification exists (run1's first finding).
//	identical re-report from a later run:
//	  (1) a NEW evidence row exists (2 total at the coordinate — the report IS recorded);
//	  (2) NO new notification fired (count stays 1 — the coalescing/suppression path, R2);
//	  (3) the coordinate does NOT reappear in to_file and stays `dismissed`.
//	materially-different re-report on the same coordinate:
//	  (4) it re-opens (reappears in to_file, status `open`);
//	  (5) a notification fires (count climbs to 2).
func TestFindingsE2EAntiNagLiveDB(t *testing.T) {
	h, pool, _, box, fs := findingsE2E(t)
	ctx := context.Background()
	f := seedFindingsE2E(ctx, t, pool, box, fs.server.URL)

	const canonical = "internal/retry.go#retryloop"
	const location = "internal/retry.go#retryLoop"
	const sameTitle = "retry can never succeed"
	const sameDesc = "the backoff resets so the retry loop spins forever"

	// (0) run1 reports the finding → one notification, one open coordinate.
	run1 := seedRunningRun(ctx, t, pool, f.owner.ID, f.repoID, 201)
	findingID := reportFinding(t, h, f.owner.ID, run1, sameTitle, sameDesc, location)
	if got := incidentalNotificationCount(ctx, t, pool, f.owner.ID); got != 1 {
		t.Fatalf("after run1's finding: notifications = %d, want 1", got)
	}

	// The user dismisses it (wont_do).
	rr := httptest.NewRecorder()
	h.DismissFinding(rr, dismissFindingReq(f.owner, findingID, map[string]any{"reason": "wont_do"}))
	if rr.Code != http.StatusOK {
		t.Fatalf("DismissFinding = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if status, _ := dispositionStatus(ctx, t, pool, f.owner.ID, f.repoID, canonical); status != "dismissed" {
		t.Fatalf("after dismiss: status = %s, want dismissed", status)
	}
	notifsBeforeRereport := incidentalNotificationCount(ctx, t, pool, f.owner.ID)
	if notifsBeforeRereport != 1 {
		t.Fatalf("dismiss must not create a notification; count = %d, want 1", notifsBeforeRereport)
	}

	// (b.1-3) a LATER run re-reports the SAME coordinate with the SAME content (identical
	// content_hash) → the evidence is recorded, but the anti-nag path suppresses it.
	run2 := seedRunningRun(ctx, t, pool, f.owner.ID, f.repoID, 202)
	reportFinding(t, h, f.owner.ID, run2, sameTitle, sameDesc, location)

	if got := findingRowCount(ctx, t, pool, f.owner.ID, f.repoID, canonical); got != 2 {
		t.Fatalf("(1) the re-report must record a new evidence row: rows = %d, want 2", got)
	}
	if got := incidentalNotificationCount(ctx, t, pool, f.owner.ID); got != notifsBeforeRereport {
		t.Fatalf("(2) a suppressed matching-hash re-report must NOT notify (R2): count = %d, want %d", got, notifsBeforeRereport)
	}
	if status, _ := dispositionStatus(ctx, t, pool, f.owner.ID, f.repoID, canonical); status != "dismissed" {
		t.Fatalf("(3) the coordinate must stay dismissed, got %s", status)
	}
	if _, ok := backlogHas(listBacklog(t, h, f.owner, "to_file"), canonical); ok {
		t.Fatalf("(3) a dismissed coordinate must NOT reappear in the to_file bucket")
	}

	// (b.4-5) a materially-DIFFERENT report on the SAME coordinate (different title/description →
	// different content_hash) → it re-opens and notifies.
	run3 := seedRunningRun(ctx, t, pool, f.owner.ID, f.repoID, 203)
	reportFinding(t, h, f.owner.ID, run3,
		"unbounded goroutine leak in retryLoop", "each retry spawns a goroutine that never exits, leaking one per attempt", location)

	dto := listBacklog(t, h, f.owner, "to_file")
	row, ok := backlogHas(dto, canonical)
	if !ok {
		t.Fatalf("(4) a materially-different report must re-open the coordinate into to_file; backlog=%+v", dto.Findings)
	}
	if row.Status != "open" {
		t.Fatalf("(4) re-opened coordinate status = %q, want open", row.Status)
	}
	if got := incidentalNotificationCount(ctx, t, pool, f.owner.ID); got != notifsBeforeRereport+1 {
		t.Fatalf("(5) a materially-different re-open must notify: count = %d, want %d", got, notifsBeforeRereport+1)
	}
}
