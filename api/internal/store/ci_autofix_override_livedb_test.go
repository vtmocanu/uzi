package store_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// autofixOverrideEnv is one throwaway repo + connection owned by a single test, so the
// tests below never see each other's rows (the repo id scopes every query under test).
type autofixOverrideEnv struct {
	ctx    context.Context
	pool   *pgxpool.Pool
	q      *store.Queries
	repoID uuid.UUID
	ownerA uuid.UUID // connection owner; has a token
}

func newAutofixOverrideEnv(t *testing.T, forgeProjectID int) *autofixOverrideEnv {
	t.Helper()
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
	t.Cleanup(pool.Close)

	e := &autofixOverrideEnv{ctx: ctx, pool: pool, q: store.New(pool), repoID: uuid.New()}
	e.ownerA = e.newUser(t, nil, true)
	connID := uuid.New()
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.override.e2e', 'bot', 1, $3)`, connID, e.ownerA, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, $3, 'g/override', 'https://forge.override.e2e/g/override', 'main', true)`,
		e.repoID, connID, forgeProjectID)
	return e
}

// newUser inserts a user whose owner-level ci_autofix_enabled is the given tri-state
// (nil = NULL = inherit) and, when withToken, an anthropic token.
func (e *autofixOverrideEnv) newUser(t *testing.T, ownerEnabled *bool, withToken bool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	var v any
	if ownerEnabled != nil {
		v = *ownerEnabled
	}
	mustExec(e.ctx, t, e.pool, `INSERT INTO users (id, email, password_hash, ci_autofix_enabled) VALUES ($1, $2, 'x', $3)`,
		id, fmt.Sprintf("autofix-override-%s@e2e", id), v)
	if withToken {
		mustExec(e.ctx, t, e.pool,
			`INSERT INTO user_secrets (user_id, kind, ciphertext, label) VALUES ($1, 'anthropic_token', $2, 'default')`, id, []byte{0x1})
	}
	return id
}

// insertRun inserts a run of the given kind on a branch with an optional per-run
// override. issue-shaped kinds get an issue_iid; prompt/task get NULL (runs_kind_shape).
func (e *autofixOverrideEnv) insertRun(t *testing.T, owner uuid.UUID, kind, status, branch string, mrIID *int64, override *bool, createdOffset string) uuid.UUID {
	t.Helper()
	var mr, ov, issue any
	if mrIID != nil {
		mr = *mrIID
	}
	if override != nil {
		ov = *override
	}
	if kind == "issue" || kind == "self_improve" {
		issue = int64(uuid.New().ID()%1_000_000) + 1
	}
	id := uuid.New()
	mustExec(e.ctx, t, e.pool,
		`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, branch, mr_iid, ci_autofix_enabled, created_at)
		 VALUES ($1, $2, $3, $4, $5, 't', 'd', $6, $7, $8, $9, now() - $10::interval)`,
		id, owner, e.repoID, kind, issue, status, branch, mr, ov, createdOffset)
	return id
}

func (e *autofixOverrideEnv) insertFailedPipeline(t *testing.T, ref string, pipelineID int64) {
	t.Helper()
	mustExec(e.ctx, t, e.pool,
		`INSERT INTO pipeline_statuses (repo_id, ref, pipeline_id, sha, status, web_url, synced_at)
		 VALUES ($1, $2, $3, 'deadbeef', 'failed', 'https://forge.override.e2e/p', now())`,
		e.repoID, ref, pipelineID)
}

func (e *autofixOverrideEnv) candidateRefs(t *testing.T) map[string]store.ListCIAutofixCandidateRefsRow {
	t.Helper()
	rows, err := e.q.ListCIAutofixCandidateRefs(e.ctx, e.repoID)
	if err != nil {
		t.Fatalf("ListCIAutofixCandidateRefs: %v", err)
	}
	got := make(map[string]store.ListCIAutofixCandidateRefsRow, len(rows))
	for _, r := range rows {
		got[r.Ref.String] = r
	}
	return got
}

func boolPtr(b bool) *bool { return &b }

// TestCIAutofixRunOverrideCandidateTriStateLiveDB pins issue #4617: the selected run's
// ci_autofix_enabled override wins over the owner default in the candidate gate, resolved
// COALESCE(run, owner) IS NOT FALSE.
func TestCIAutofixRunOverrideCandidateTriStateLiveDB(t *testing.T) {
	e := newAutofixOverrideEnv(t, 11)
	ownerNull := e.newUser(t, nil, true)
	ownerFalse := e.newUser(t, boolPtr(false), true)
	ownerOn := e.newUser(t, boolPtr(true), true)

	cases := []struct {
		branch   string
		owner    uuid.UUID
		override *bool
		want     bool
	}{
		{"agent/issue-1", ownerNull, boolPtr(false), false}, // run false beats owner default ON (NULL)
		{"agent/issue-2", ownerOn, boolPtr(false), false},   // run false beats owner true
		{"agent/issue-3", ownerFalse, boolPtr(true), true},  // run true beats owner false
		{"agent/issue-4", ownerNull, nil, true},             // inherit, owner NULL
		{"agent/issue-5", ownerFalse, nil, false},           // inherit, owner false
		{"agent/issue-6", ownerOn, nil, true},               // inherit, owner true
		{"agent/issue-7", ownerNull, boolPtr(true), true},   // explicit true, owner NULL
	}
	for i, c := range cases {
		e.insertRun(t, c.owner, "issue", "completed", c.branch, iid64(int64(100+i)), c.override, "1 hour")
		e.insertFailedPipeline(t, c.branch, int64(9000+i))
	}
	got := e.candidateRefs(t)
	for _, c := range cases {
		_, ok := got[c.branch]
		if ok != c.want {
			t.Errorf("branch %s (override=%v): candidate=%v, want %v", c.branch, c.override, ok, c.want)
		}
	}
}

func iid64(n int64) *int64 { return &n }

// TestCIAutofixSelectedRunMirrorsCandidateLiveDB is the drift guard between the
// per_branch CTE of ListCIAutofixCandidateRefs and GetCIAutofixSelectedRun: on one
// fixture both must pick the same run, identified by a distinct mr_iid per run.
func TestCIAutofixSelectedRunMirrorsCandidateLiveDB(t *testing.T) {
	e := newAutofixOverrideEnv(t, 12)

	type fixture struct {
		name   string
		branch string
		wantMR int64
		seed   func(branch string)
		pipe   int64
	}
	owner := e.ownerA
	// Each fixture seeds runs on its own branch; the run carrying wantMR must be selected.
	fixtures := []fixture{
		{name: "newest run", branch: "agent/issue-21", wantMR: 302, pipe: 8001, seed: func(b string) {
			e.insertRun(t, owner, "issue", "completed", b, iid64(301), nil, "3 hours")
			e.insertRun(t, owner, "issue", "completed", b, iid64(302), nil, "1 hour")
		}},
		{name: "cancelled newest falls through", branch: "agent/issue-22", wantMR: 311, pipe: 8002, seed: func(b string) {
			e.insertRun(t, owner, "issue", "completed", b, iid64(311), nil, "3 hours")
			e.insertRun(t, owner, "issue", "cancelled", b, iid64(312), nil, "1 hour")
		}},
		{name: "non-candidate kind newer is ignored", branch: "agent/issue-23", wantMR: 321, pipe: 8003, seed: func(b string) {
			e.insertRun(t, owner, "issue", "completed", b, iid64(321), nil, "3 hours")
			e.insertRun(t, owner, "task", "completed", b, iid64(322), nil, "1 hour")
		}},
		{name: "newer run without mr_iid ignored", branch: "agent/issue-24", wantMR: 331, pipe: 8004, seed: func(b string) {
			e.insertRun(t, owner, "issue", "completed", b, iid64(331), nil, "3 hours")
			e.insertRun(t, owner, "issue", "completed", b, nil, nil, "1 hour")
		}},
	}
	for _, f := range fixtures {
		f.seed(f.branch)
		e.insertFailedPipeline(t, f.branch, f.pipe)
	}

	got := e.candidateRefs(t)
	for _, f := range fixtures {
		cand, ok := got[f.branch]
		if !ok {
			t.Errorf("%s: %s is not a candidate", f.name, f.branch)
			continue
		}
		if !cand.MrIid.Valid || cand.MrIid.Int64 != f.wantMR {
			t.Errorf("%s: candidate mr_iid = %+v, want %d", f.name, cand.MrIid, f.wantMR)
		}
		sel, err := e.q.GetCIAutofixSelectedRun(e.ctx, store.GetCIAutofixSelectedRunParams{
			RepoID: e.repoID, Ref: pgtype.Text{String: f.branch, Valid: true},
		})
		if err != nil {
			t.Errorf("%s: GetCIAutofixSelectedRun: %v", f.name, err)
			continue
		}
		var selMR int64
		if err := e.pool.QueryRow(e.ctx, `SELECT mr_iid FROM runs WHERE id = $1`, sel.ID).Scan(&selMR); err != nil {
			t.Fatalf("%s: read selected run: %v", f.name, err)
		}
		if selMR != cand.MrIid.Int64 || sel.UserID != cand.UserID {
			t.Errorf("%s: selected run (mr_iid=%d user=%s) differs from candidate (mr_iid=%d user=%s)",
				f.name, selMR, sel.UserID, cand.MrIid.Int64, cand.UserID)
		}
	}

	// Nothing qualifies: only a cancelled run and a branch with no runs at all.
	e.insertRun(t, owner, "issue", "cancelled", "agent/issue-25", iid64(341), nil, "1 hour")
	for _, ref := range []string{"agent/issue-25", "agent/issue-none"} {
		_, err := e.q.GetCIAutofixSelectedRun(e.ctx, store.GetCIAutofixSelectedRunParams{
			RepoID: e.repoID, Ref: pgtype.Text{String: ref, Valid: true},
		})
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("ref %s: err = %v, want pgx.ErrNoRows", ref, err)
		}
	}

	// The row carries the raw run override and owner default.
	ownerOff := e.newUser(t, boolPtr(false), true)
	e.insertRun(t, ownerOff, "issue", "completed", "agent/issue-26", iid64(351), boolPtr(true), "1 hour")
	sel, err := e.q.GetCIAutofixSelectedRun(e.ctx, store.GetCIAutofixSelectedRunParams{
		RepoID: e.repoID, Ref: pgtype.Text{String: "agent/issue-26", Valid: true},
	})
	if err != nil {
		t.Fatalf("GetCIAutofixSelectedRun: %v", err)
	}
	if !sel.RunCiAutofixEnabled.Valid || !sel.RunCiAutofixEnabled.Bool {
		t.Errorf("run override = %+v, want true", sel.RunCiAutofixEnabled)
	}
	if !sel.OwnerCiAutofixEnabled.Valid || sel.OwnerCiAutofixEnabled.Bool {
		t.Errorf("owner default = %+v, want false", sel.OwnerCiAutofixEnabled)
	}
}

// TestCIAutofixSelectedRunTieBreakLiveDB pins the id DESC tie-break: two qualifying runs on
// one branch with an IDENTICAL created_at must resolve to the larger id in both the
// candidate listing and the single-branch selection. The runs differ in owner and mr_iid so
// the pick is observable.
func TestCIAutofixSelectedRunTieBreakLiveDB(t *testing.T) {
	e := newAutofixOverrideEnv(t, 15)
	const branch = "agent/issue-51"
	lowOwner := e.newUser(t, nil, true)
	highOwner := e.newUser(t, nil, true)
	lowID, highID := uuid.New(), uuid.New()
	if bytes.Compare(lowID[:], highID[:]) > 0 {
		lowID, highID = highID, lowID
	}
	insert := func(id, owner uuid.UUID, mr int64, iss int64) {
		mustExec(e.ctx, t, e.pool,
			`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, branch, mr_iid, created_at)
			 VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'completed', $5, $6, '2026-01-02 03:04:05+00')`,
			id, owner, e.repoID, iss, branch, mr)
	}
	// Insert the larger id first so insertion order cannot explain the pick.
	insert(highID, highOwner, 502, 5102)
	insert(lowID, lowOwner, 501, 5101)
	e.insertFailedPipeline(t, branch, 8501)

	cand, ok := e.candidateRefs(t)[branch]
	if !ok {
		t.Fatalf("%s is not a candidate", branch)
	}
	if cand.MrIid.Int64 != 502 || cand.UserID != highOwner {
		t.Errorf("candidate picked mr_iid=%d user=%s, want the larger-id run (mr_iid=502 user=%s)", cand.MrIid.Int64, cand.UserID, highOwner)
	}
	sel, err := e.q.GetCIAutofixSelectedRun(e.ctx, store.GetCIAutofixSelectedRunParams{
		RepoID: e.repoID, Ref: pgtype.Text{String: branch, Valid: true},
	})
	if err != nil {
		t.Fatalf("GetCIAutofixSelectedRun: %v", err)
	}
	if sel.ID != highID || sel.UserID != highOwner {
		t.Errorf("selected run = %s user=%s, want the larger id %s user=%s", sel.ID, sel.UserID, highID, highOwner)
	}
}

// TestSetRunCiAutofixEnabledLiveDB pins the owner-scoped setter: true/false/NULL and a
// foreign user_id affecting 0 rows.
func TestSetRunCiAutofixEnabledLiveDB(t *testing.T) {
	e := newAutofixOverrideEnv(t, 13)
	other := e.newUser(t, nil, false)
	// A completed run: the setter has no status guard.
	runID := e.insertRun(t, e.ownerA, "issue", "completed", "agent/issue-31", iid64(401), nil, "1 hour")

	read := func() pgtype.Bool {
		var v pgtype.Bool
		if err := e.pool.QueryRow(e.ctx, `SELECT ci_autofix_enabled FROM runs WHERE id = $1`, runID).Scan(&v); err != nil {
			t.Fatalf("read override: %v", err)
		}
		return v
	}
	set := func(user uuid.UUID, v pgtype.Bool) int64 {
		n, err := e.q.SetRunCiAutofixEnabled(e.ctx, store.SetRunCiAutofixEnabledParams{CiAutofixEnabled: v, ID: runID, UserID: user})
		if err != nil {
			t.Fatalf("SetRunCiAutofixEnabled: %v", err)
		}
		return n
	}

	if n := set(e.ownerA, pgtype.Bool{Bool: true, Valid: true}); n != 1 {
		t.Fatalf("set true rows = %d, want 1", n)
	}
	if v := read(); !v.Valid || !v.Bool {
		t.Fatalf("after set true: %+v", v)
	}
	if n := set(e.ownerA, pgtype.Bool{Bool: false, Valid: true}); n != 1 {
		t.Fatalf("set false rows = %d, want 1", n)
	}
	if v := read(); !v.Valid || v.Bool {
		t.Fatalf("after set false: %+v", v)
	}
	if n := set(other, pgtype.Bool{Bool: true, Valid: true}); n != 0 {
		t.Fatalf("foreign user rows = %d, want 0", n)
	}
	if v := read(); !v.Valid || v.Bool {
		t.Fatalf("foreign write must not change the override, got %+v", v)
	}
	if n := set(e.ownerA, pgtype.Bool{}); n != 1 {
		t.Fatalf("clear rows = %d, want 1", n)
	}
	if v := read(); v.Valid {
		t.Fatalf("after clear want NULL, got %+v", v)
	}
}

func TestSetRunCiAutofixEnabledPreservesQueuedUpdatedAtLiveDB(t *testing.T) {
	e := newAutofixOverrideEnv(t, 16)
	runID := e.insertRun(t, e.ownerA, "issue", "queued", "agent/issue-32", nil, nil, "1 hour")
	const affinityTime = "2026-01-02 03:04:05+00"
	mustExec(e.ctx, t, e.pool, `UPDATE runs SET updated_at = $2::timestamptz WHERE id = $1`, runID, affinityTime)

	n, err := e.q.SetRunCiAutofixEnabled(e.ctx, store.SetRunCiAutofixEnabledParams{
		ID: runID, UserID: e.ownerA, CiAutofixEnabled: pgtype.Bool{Bool: false, Valid: true},
	})
	if err != nil || n != 1 {
		t.Fatalf("set queued override: rows = %d, err = %v", n, err)
	}

	var override pgtype.Bool
	var unchanged bool
	if err := e.pool.QueryRow(e.ctx, `SELECT ci_autofix_enabled, updated_at = $2::timestamptz FROM runs WHERE id = $1`,
		runID, affinityTime).Scan(&override, &unchanged); err != nil {
		t.Fatalf("read queued run: %v", err)
	}
	if !override.Valid || override.Bool {
		t.Fatalf("queued override = %+v, want false", override)
	}
	if !unchanged {
		t.Fatal("changing the CI-autofix override reset the queued run's affinity timestamp")
	}
}

// TestListActiveCIFixRunIDsForRefLiveDB pins the owner-scoped active ci_fix listing.
func TestListActiveCIFixRunIDsForRefLiveDB(t *testing.T) {
	e := newAutofixOverrideEnv(t, 14)
	other := e.newUser(t, nil, false)
	const ref = "agent/issue-41"

	insertFix := func(owner uuid.UUID, status, pipelineRef string, pipelineID int64) uuid.UUID {
		id := uuid.New()
		mustExec(e.ctx, t, e.pool,
			`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, pipeline_id, pipeline_ref)
			 VALUES ($1, $2, $3, 'ci_fix', NULL, 't', 'd', $4, $5, $6)`,
			id, owner, e.repoID, status, pipelineID, pipelineRef)
		return id
	}
	done := insertFix(e.ownerA, "completed", ref, 1)
	active := insertFix(e.ownerA, "queued", ref, 2)
	_ = insertFix(other, "queued", "agent/issue-42", 3) // another user's active fix on another ref

	list := func(user uuid.UUID, r string) []uuid.UUID {
		ids, err := e.q.ListActiveCIFixRunIDsForRef(e.ctx, store.ListActiveCIFixRunIDsForRefParams{
			RepoID: e.repoID, UserID: user, Ref: pgtype.Text{String: r, Valid: true},
		})
		if err != nil {
			t.Fatalf("ListActiveCIFixRunIDsForRef: %v", err)
		}
		return ids
	}
	got := list(e.ownerA, ref)
	if len(got) != 1 || got[0] != active {
		t.Fatalf("owner list = %v, want only the active run %s (completed %s excluded)", got, active, done)
	}
	if got := list(other, ref); len(got) != 0 {
		t.Fatalf("another user must see no active fix on the owner's ref, got %v", got)
	}
	if got := list(other, "agent/issue-42"); len(got) != 1 {
		t.Fatalf("other user's own ref: got %v, want 1", got)
	}
}
