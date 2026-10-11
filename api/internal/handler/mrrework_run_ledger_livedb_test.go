package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Namespaced MR-rework ledger through the on-demand rework endpoint (ADR-1199): per-namespace
// marks, the legacy-row bootstrap and its refusals, against a real Postgres.

func nscomment(ns string, id, author int64, body string) forge.MRComment {
	c := rcomment(id, author, body)
	c.IDNamespace = ns
	return c
}

// ledgerJSON is the whole ledger row as one comparable string.
func (r *reworkRig) ledgerJSON(t *testing.T) string {
	t.Helper()
	var s string
	if err := r.pool.QueryRow(context.Background(),
		`SELECT row_to_json(l)::text FROM mr_rework_ledger l WHERE repo_id = $1 AND ref = $2`, r.repoID, reworkRef).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// Regression: a consumed issue_comment mark of 5.58e9 must not hide an eligible review_comment
// whose id (3.96e9) is lower in absolute value but unseen in its own namespace.
func TestOnDemandReworkNamespacedMarksDoNotHideALowerIDLiveDB(t *testing.T) {
	rig := newReworkRig(t, &settingsStore{}, time.Minute)
	rig.f.answers[11] = forge.AuthorEligible
	q := store.New(rig.pool)
	if _, err := q.UpsertMRReworkLedger(context.Background(), store.UpsertMRReworkLedgerParams{
		RepoID: rig.repoID, Ref: reworkRef, HighWater: 5_580_000_000, Marks: []byte(`{"issue_comment":5580000000}`),
	}); err != nil {
		t.Fatal(err)
	}
	rig.f.comments = []forge.MRComment{
		nscomment(forge.CommentNSIssueComment, 5_580_000_000, 11, "already handled"),
		nscomment(forge.CommentNSReviewComment, 3_960_000_001, 11, "please rename the helper"),
	}
	rec := rig.post(t, `{}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d body %s, want 201", rec.Code, rec.Body.String())
	}
	if n := rig.reworkRuns(t); n != 1 {
		t.Fatalf("mr_rework runs = %d, want 1", n)
	}
}

// A legacy row (written by the pre-ADR-1199 binary) is bootstrapped by the request, which then
// answers from the bootstrapped state.
func TestOnDemandReworkBootstrapsALegacyRowLiveDB(t *testing.T) {
	rig := newReworkRig(t, &settingsStore{}, time.Minute)
	rig.f.answers[11] = forge.AuthorEligible
	cliMustExec(t, rig.pool,
		`INSERT INTO mr_rework_ledger (repo_id, ref, attempt_count, high_water) VALUES ($1, $2, 1, 5580000000)`, rig.repoID, reworkRef)
	rig.f.comments = []forge.MRComment{
		nscomment(forge.CommentNSIssueComment, 5_580_000_000, 11, "already handled"),
		nscomment(forge.CommentNSReviewComment, 3_960_000_001, 11, "also handled"),
	}
	var legacy bool
	check := func(want bool) {
		t.Helper()
		if err := rig.pool.QueryRow(context.Background(),
			`SELECT comment_marks IS NULL FROM mr_rework_ledger WHERE repo_id = $1 AND ref = $2`, rig.repoID, reworkRef).Scan(&legacy); err != nil {
			t.Fatal(err)
		}
		if legacy != want {
			t.Fatalf("comment_marks IS NULL = %v, want %v", legacy, want)
		}
	}
	check(true)

	// Everything listed is at or below the legacy mark: bootstrapped as consumed, nothing new.
	rec := rig.post(t, `{}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "nothing to rework") {
		t.Fatalf("status %d body %s, want the nothing-to-rework 409", rec.Code, rec.Body.String())
	}
	check(false)
	var marks string
	if err := rig.pool.QueryRow(context.Background(),
		`SELECT comment_marks::text FROM mr_rework_ledger WHERE repo_id = $1 AND ref = $2`, rig.repoID, reworkRef).Scan(&marks); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(marks, `"issue_comment": 5580000000`) || !strings.Contains(marks, `"review_comment": 3960000001`) {
		t.Fatalf("bootstrapped comment_marks = %s", marks)
	}
	if rig.reworkRuns(t) != 0 {
		t.Fatal("a run was created from already-consumed comments")
	}

	// A genuinely new comment in the bootstrapped namespace now reworks.
	rig.f.comments = append(rig.f.comments, nscomment(forge.CommentNSIssueComment, 5_580_000_002, 11, "new request"))
	rec = rig.post(t, `{}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d body %s, want 201", rec.Code, rec.Body.String())
	}
}

// 5001 legacy pending ids, each present in two namespaces, expand past the pending cap: the
// request is refused and the row is left byte-for-byte as it was.
func TestOnDemandReworkBootstrapOverflowRefusesAndLeavesTheRowLiveDB(t *testing.T) {
	rig := newReworkRig(t, &settingsStore{}, time.Minute)
	rig.f.answers[11] = forge.AuthorEligible
	cliMustExec(t, rig.pool,
		`INSERT INTO mr_rework_ledger (repo_id, ref, attempt_count, high_water, pending_unknown_ids)
		 VALUES ($1, $2, 1, 100000, (SELECT array_agg(g::bigint) FROM generate_series(1, 5001) g))`, rig.repoID, reworkRef)
	comments := make([]forge.MRComment, 0, 2*5001)
	for id := int64(1); id <= 5001; id++ {
		comments = append(comments,
			nscomment(forge.CommentNSIssueComment, id, 11, "x"),
			nscomment(forge.CommentNSReviewComment, id, 11, "y"))
	}
	rig.f.comments = comments
	before := rig.ledgerJSON(t)

	rec := rig.post(t, `{}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), workersvc.ErrReviewLedgerBootstrapOverflow.Error()) {
		t.Fatalf("status %d body %s, want 409 with the overflow message", rec.Code, rec.Body.String())
	}
	if rig.reworkRuns(t) != 0 {
		t.Fatal("a run was created on an overflowing legacy ledger")
	}
	if after := rig.ledgerJSON(t); after != before {
		t.Fatalf("ledger row changed on refusal:\nbefore %s\nafter  %s", before, after)
	}
}

// The legacy scalars move between the row read and the bootstrap write: the compare-and-set
// loses and the request answers ErrReworkLedgerChanged as a 409, writing nothing.
func TestOnDemandReworkBootstrapRaceIs409LiveDB(t *testing.T) {
	rig := newReworkRig(t, &settingsStore{}, time.Minute)
	rig.f.answers[11] = forge.AuthorEligible
	cliMustExec(t, rig.pool,
		`INSERT INTO mr_rework_ledger (repo_id, ref, attempt_count, high_water) VALUES ($1, $2, 1, 100)`, rig.repoID, reworkRef)
	rig.f.comments = []forge.MRComment{nscomment(forge.CommentNSReviewComment, 190, 11, "please rename the helper")}
	rig.f.onList = func() {
		cliMustExec(t, rig.pool, `UPDATE mr_rework_ledger SET high_water = 120 WHERE repo_id = $1 AND ref = $2`, rig.repoID, reworkRef)
	}
	rec := rig.post(t, `{}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), workersvc.ErrReworkLedgerChanged.Error()) {
		t.Fatalf("status %d body %s, want 409 with the ledger-changed message", rec.Code, rec.Body.String())
	}
	if rig.reworkRuns(t) != 0 {
		t.Fatal("a run was created after a lost bootstrap race")
	}
}

// The row turns legacy between the read and StartMRReworkForRun's advance: the create refuses
// with ErrReworkLedgerChanged, which the handler maps to 409, and no run is left behind.
func TestOnDemandReworkRowTurnedLegacyDuringCreateIs409LiveDB(t *testing.T) {
	rig := newReworkRig(t, &settingsStore{}, time.Minute)
	rig.f.answers[11] = forge.AuthorEligible
	q := store.New(rig.pool)
	if _, err := q.UpsertMRReworkLedger(context.Background(), store.UpsertMRReworkLedgerParams{
		RepoID: rig.repoID, Ref: reworkRef, HighWater: 100, Marks: []byte(`{"review_comment":100}`),
	}); err != nil {
		t.Fatal(err)
	}
	rig.f.comments = []forge.MRComment{nscomment(forge.CommentNSReviewComment, 190, 11, "please rename the helper")}
	rig.f.onList = func() {
		cliMustExec(t, rig.pool, `UPDATE mr_rework_ledger SET comment_marks = NULL WHERE repo_id = $1 AND ref = $2`, rig.repoID, reworkRef)
	}
	rec := rig.post(t, `{}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), workersvc.ErrReworkLedgerChanged.Error()) {
		t.Fatalf("status %d body %s, want 409 with the ledger-changed message", rec.Code, rec.Body.String())
	}
	if rig.reworkRuns(t) != 0 {
		t.Fatal("a run survived a refused create")
	}
}
