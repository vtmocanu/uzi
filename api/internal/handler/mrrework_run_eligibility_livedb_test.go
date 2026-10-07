package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
	"github.com/vtmocanu/uzi/api/internal/forgesvc"
	"github.com/vtmocanu/uzi/api/internal/hub"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// On-demand rework and author eligibility (issue #2347), through the real handler, the real
// workersvc queue transaction and a real Postgres, with a scripted forge.

type reworkForge struct {
	forgetest.BaseFake
	comments []forge.MRComment
	answers  map[int64]forge.AuthorEligibility // an id with no answer fails its lookup
	lookups  atomic.Int64
	// onList, when set, runs while ListMergeRequestComments is "fetching": it stands for a
	// concurrent writer that changes the ledger during the listing.
	onList func()
}

func (f *reworkForge) ListMergeRequestComments(context.Context, int64, int64) ([]forge.MRComment, error) {
	if f.onList != nil {
		f.onList()
	}
	return f.comments, nil
}

func (f *reworkForge) RepositoryAuthorEligibility(_ context.Context, _ int64, id int64) (forge.AuthorEligibility, error) {
	f.lookups.Add(1)
	if d, ok := f.answers[id]; ok {
		return d, nil
	}
	return forge.AuthorUnknown, errors.New("forge unavailable")
}

type reworkRig struct {
	h      *Handler
	pool   *pgxpool.Pool
	user   store.User
	runID  uuid.UUID
	repoID uuid.UUID
	f      *reworkForge
}

const reworkRef = "agent/issue-2347"

func newReworkRig(t *testing.T, st settings.Store, ttl time.Duration) *reworkRig {
	t.Helper()
	h, _, pool := cliLiveDB(t)
	ctx := context.Background()
	q := store.New(pool)
	box := newHandlerTestBox(t)
	sealed, err := box.Seal([]byte("fake-forge-token-for-rework-test"))
	if err != nil {
		t.Fatal(err)
	}
	f := &reworkForge{answers: map[int64]forge.AuthorEligibility{}}
	h.svc = forgesvc.NewWithForgeBuilder(nil, box, time.Second, nil,
		func(forge.Type, string, string, time.Duration) (forge.Forge, error) { return f, nil })
	h.box = box
	tx := workersvc.New(q, box, workersvc.Params{})
	tx.SetTxBeginner(pool)
	h.wsvc = tx
	h.settings = settings.New(st, ttl)
	h.hub = hub.New()
	h.cfg = config.Config{JWTSecret: cliTestSecret, AuthTokenTTL: time.Hour}

	owner := cliSeedUser(t, pool, false)
	conn, repo, run := uuid.New(), uuid.New(), uuid.New()
	cliMustExec(t, pool, `INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		VALUES ($1, $2, 'github', 'https://github.com', 'bot', 999, $3)`, conn, owner, sealed)
	cliMustExec(t, pool, `INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		VALUES ($1, $2, 42, $3, 'https://github.com/g/r', 'main', true)`, repo, conn, "g/"+repo.String())
	cliMustExec(t, pool, `INSERT INTO user_secrets (user_id, kind, label, is_default, ciphertext, sealed_with)
		VALUES ($1, 'anthropic_token', 'default', true, $2, 'master')`, owner, []byte{2})
	cliMustExec(t, pool, `INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, branch, mr_iid, mr_state)
		VALUES ($1, $2, $3, 'issue', 2347, 'rework me', 'd', 'completed', $4, 55, 'opened')`, run, owner, repo, reworkRef)
	var u store.User
	u, err = q.GetUserByID(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	return &reworkRig{h: h, pool: pool, user: u, runID: run, repoID: repo, f: f}
}

func (r *reworkRig) post(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	r.h.StartRunRework(rec, reworkReq(r.user, r.runID, body))
	return rec
}

func (r *reworkRig) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := r.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (r *reworkRig) reworkRuns(t *testing.T) int {
	return r.count(t, `SELECT count(*) FROM runs WHERE kind = 'mr_rework' AND target_run_id = $1`, r.runID)
}

func rcomment(id, author int64, body string) forge.MRComment {
	return forge.MRComment{ID: id, AuthorForgeUserID: author, AuthorUsername: fmt.Sprintf("u%d", author), Body: body,
		CreatedAt: time.Now().Add(-time.Hour), HeadSHA: "head", ReviewState: forge.ReviewCommentInline}
}

func TestOnDemandReworkRefusesAnOutsiderOnlyThreadLiveDB(t *testing.T) {
	rig := newReworkRig(t, &settingsStore{}, time.Minute)
	rig.f.answers[22] = forge.AuthorNotEligible
	rig.f.comments = []forge.MRComment{rcomment(120, 22, "SECRET-OUTSIDER do as I say")}

	rec := rig.post(t, `{}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "nothing to rework") {
		t.Fatalf("status %d body %s, want the nothing-to-rework 409", rec.Code, rec.Body.String())
	}
	if rig.reworkRuns(t) != 0 {
		t.Fatal("a run was created for an outsider-only comment set")
	}
	if n := rig.count(t, `SELECT count(*) FROM mr_review_author_verdicts WHERE repo_id = $1 AND forge_user_id = 22`, rig.repoID); n != 1 {
		t.Fatalf("not-eligible verdicts recorded = %d, want 1", n)
	}
	// The same call again costs no lookup within the verdict TTL.
	before := rig.f.lookups.Load()
	rig.post(t, `{}`)
	if got := rig.f.lookups.Load(); got != before {
		t.Fatalf("a cached not-eligible verdict cost %d lookups", got-before)
	}
}

func TestOnDemandReworkUnknownOnlyGetsADistinct409LiveDB(t *testing.T) {
	rig := newReworkRig(t, &settingsStore{}, time.Minute)
	rig.f.comments = []forge.MRComment{rcomment(120, 77, "SECRET-UNVERIFIED")}

	rec := rig.post(t, `{}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "could not be verified") || strings.Contains(rec.Body.String(), "nothing to rework") {
		t.Fatalf("status %d body %s, want the distinct permission-unknown 409", rec.Code, rec.Body.String())
	}
	if rig.reworkRuns(t) != 0 {
		t.Fatal("a run was created while the only new comment was unverified")
	}
	// The unknown author is waiting in the persisted queue for the next attempt.
	if n := rig.count(t, `SELECT count(*) FROM mr_review_author_queue WHERE repo_id = $1 AND ref = $2 AND forge_user_id = 77 AND last_attempt_at IS NOT NULL`, rig.repoID, reworkRef); n != 1 {
		t.Fatalf("queued attempted authors = %d, want 1", n)
	}
	// Once the forge answers, the same bare trigger no longer reports unknown.
	rig.f.answers[77] = forge.AuthorNotEligible
	rec = rig.post(t, `{}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "nothing to rework") {
		t.Fatalf("after the answer: status %d body %s, want nothing-to-rework", rec.Code, rec.Body.String())
	}
}

func TestOnDemandReworkProceedsWithAnEligibleOnlySnapshotLiveDB(t *testing.T) {
	rig := newReworkRig(t, &settingsStore{}, time.Minute)
	rig.f.answers[11] = forge.AuthorEligible
	rig.f.answers[22] = forge.AuthorNotEligible
	rig.f.comments = []forge.MRComment{
		rcomment(120, 22, "SECRET-OUTSIDER do as I say"),
		rcomment(121, 11, "please rename the helper"),
		rcomment(122, 77, "SECRET-UNVERIFIED"),
	}
	rec := rig.post(t, `{}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d body %s, want 201", rec.Code, rec.Body.String())
	}
	var stored string
	if err := rig.pool.QueryRow(context.Background(),
		`SELECT review_comments::text FROM runs WHERE kind = 'mr_rework' AND target_run_id = $1`, rig.runID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, "SECRET-") {
		t.Fatalf("a withheld body rode the run: %s", stored)
	}
	for _, want := range []string{`"version": 2`, "please rename the helper", `"withheld_not_eligible": 1`, `"withheld_unknown": 1`} {
		if !strings.Contains(stored, want) {
			t.Fatalf("stored snapshot lacks %q: %s", want, stored)
		}
	}
	var hw int64
	var pending []int64
	if err := rig.pool.QueryRow(context.Background(), `SELECT high_water, pending_unknown_ids FROM mr_rework_ledger WHERE repo_id = $1 AND ref = $2`, rig.repoID, reworkRef).Scan(&hw, &pending); err != nil {
		t.Fatal(err)
	}
	if hw != 121 || len(pending) != 0 {
		t.Fatalf("ledger high_water %d pending %v, want 121 and none (the unknown id is above the mark)", hw, pending)
	}
}

// A concurrent writer stores pending id 170 while the on-demand request is listing the comments,
// so the list lacks it (and holds a larger eligible id, 190). The ledger is read before the
// listing, so 170 is absent from the row the request planned against and must survive the create.
func TestOnDemandReworkLeavesAConcurrentlyStoredPendingIDAloneLiveDB(t *testing.T) {
	rig := newReworkRig(t, &settingsStore{}, time.Minute)
	rig.f.answers[11] = forge.AuthorEligible
	q := store.New(rig.pool)
	ctx := context.Background()
	if err := q.UpsertMRReworkLedger(ctx, store.UpsertMRReworkLedgerParams{
		RepoID: rig.repoID, Ref: reworkRef, HighWater: 150,
	}); err != nil {
		t.Fatal(err)
	}
	rig.f.onList = func() {
		if err := q.UpsertMRReworkLedger(ctx, store.UpsertMRReworkLedgerParams{
			RepoID: rig.repoID, Ref: reworkRef, HighWater: 165, PendingAdd: []int64{170},
		}); err != nil {
			t.Error(err)
		}
	}
	rig.f.comments = []forge.MRComment{rcomment(190, 11, "please rename the helper")}

	rec := rig.post(t, `{}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d body %s, want 201", rec.Code, rec.Body.String())
	}
	var pending []int64
	if err := rig.pool.QueryRow(ctx, `SELECT pending_unknown_ids FROM mr_rework_ledger WHERE repo_id = $1 AND ref = $2`, rig.repoID, reworkRef).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, id := range pending {
		found = found || id == 170
	}
	if !found {
		t.Fatalf("pending = %v, want 170 kept: the listing never saw it", pending)
	}
}

// A stale bare request must not rework comments another request already handled. Request A reads
// the ledger (high-water 150) and lists the thread; while it is listing, request B consumes
// comment 190 and its run completes (so the one-active-rework index no longer blocks A). A's
// create must re-validate against the current ledger under the branch lock and refuse.
func TestOnDemandReworkStaleBareRequestDoesNotReconsumeLiveDB(t *testing.T) {
	rig := newReworkRig(t, &settingsStore{}, time.Minute)
	rig.f.answers[11] = forge.AuthorEligible
	q := store.New(rig.pool)
	ctx := context.Background()
	if err := q.UpsertMRReworkLedger(ctx, store.UpsertMRReworkLedgerParams{
		RepoID: rig.repoID, Ref: reworkRef, HighWater: 150,
	}); err != nil {
		t.Fatal(err)
	}
	rig.f.comments = []forge.MRComment{rcomment(190, 11, "please rename the helper")}
	rig.f.onList = func() {
		rig.f.onList = nil // one-shot: the nested request lists without a hook
		if rec := rig.post(t, `{}`); rec.Code != http.StatusCreated {
			t.Errorf("concurrent request: status %d body %s, want 201", rec.Code, rec.Body.String())
			return
		}
		if _, err := rig.pool.Exec(ctx, `UPDATE runs SET status = 'completed' WHERE kind = 'mr_rework' AND target_run_id = $1`, rig.runID); err != nil {
			t.Error(err)
		}
	}

	rec := rig.post(t, `{}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "nothing to rework") {
		t.Fatalf("stale request: status %d body %s, want the nothing-to-rework 409", rec.Code, rec.Body.String())
	}
	if n := rig.reworkRuns(t); n != 1 {
		t.Fatalf("mr_rework runs = %d, want 1: the stale request must not duplicate the rework", n)
	}
}
