package handler

// Issue #4618 live-DB proofs for GET /api/admin/run-writers: the lookup finds every active
// ci_fix / mr_rework writer of one forge project's branch across ALL owners, however old
// the run is and however many newer rows sit in front of it, and answers complete=false
// rather than a silent truncation. They drive the REAL h.Routes() router with really
// minted tokens.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; ./e2e/run-store-it.sh
// provides one and sweeps this package for the LiveDB suffix. No t.Parallel: the audit
// capture swaps the package-level adminRunDebugLogger.

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// rwlIssueIID hands out issue numbers that never collide across the tests of one DB.
var rwlIssueIID atomic.Int64

func init() { rwlIssueIID.Store(700000) }

// rwlProject returns a forge project id no other test of the shared DB uses.
func rwlProject() int64 {
	id := uuid.New()
	return int64(binary.BigEndian.Uint64(id[:8]) >> 1)
}

type rwlOwner struct{ user, conn, repo uuid.UUID }

// rwlPurgeUsers registers a cleanup that deletes the users returned by idsSQL (resolved when
// the cleanup runs, before anything is deleted) together with their runs, repos and forge
// connections, so the shared test database keeps no rows of this test. A failed delete fails
// the test, so leftover rows in the shared database never pass silently.
func rwlPurgeUsers(t *testing.T, pool *pgxpool.Pool, idsSQL string, arg any) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		rows, err := pool.Query(ctx, idsSQL, arg)
		if err != nil {
			t.Errorf("purge: resolve users: %v", err)
			return
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		if err != nil {
			t.Errorf("purge: collect users: %v", err)
			return
		}
		for _, stmt := range []string{
			`DELETE FROM runs WHERE user_id = ANY($1)`,
			`DELETE FROM repos WHERE connection_id IN (SELECT id FROM forge_connections WHERE user_id = ANY($1))`,
			`DELETE FROM forge_connections WHERE user_id = ANY($1)`,
			`DELETE FROM users WHERE id = ANY($1)`,
		} {
			if _, err := pool.Exec(ctx, stmt, ids); err != nil {
				t.Errorf("purge %q: %v", stmt, err)
			}
		}
	})
}

// rwlSeedOwner inserts a non-admin user with one forge connection and one repo of the
// given forge identity.
func rwlSeedOwner(t *testing.T, pool *pgxpool.Pool, forgeType, baseURL string, project int64) rwlOwner {
	t.Helper()
	o := rwlOwner{user: cliSeedUser(t, pool, false), conn: uuid.New(), repo: uuid.New()}
	rwlPurgeUsers(t, pool, `SELECT $1::uuid`, o.user)
	cliMustExec(t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, $3, $4, 'bot', 1, $5)`, o.conn, o.user, forgeType, baseURL, []byte{0x1})
	cliMustExec(t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, $3, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, o.repo, o.conn, project)
	return o
}

func rwlSeedCIFix(t *testing.T, pool *pgxpool.Pool, o rwlOwner, branch, status string, createdAt time.Time) uuid.UUID {
	t.Helper()
	id := uuid.New()
	cliMustExec(t, pool,
		`INSERT INTO runs (id, user_id, repo_id, kind, issue_title, issue_description, status, pipeline_id, pipeline_ref, created_at)
		 VALUES ($1, $2, $3, 'ci_fix', 't', 'd', $4, 1, $5, $6)`, id, o.user, o.repo, status, branch, createdAt)
	return id
}

// rwlSeedMRRework inserts a mr_rework run; its target is a completed issue run of the same
// owner (the source run whose MR is watched).
func rwlSeedMRRework(t *testing.T, pool *pgxpool.Pool, o rwlOwner, branch string, mr int64, status string, createdAt time.Time) uuid.UUID {
	t.Helper()
	target := uuid.New()
	cliMustExec(t, pool,
		`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status)
		 VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'completed')`, target, o.user, o.repo, rwlIssueIID.Add(1))
	id := uuid.New()
	cliMustExec(t, pool,
		`INSERT INTO runs (id, user_id, repo_id, kind, issue_title, issue_description, status, pipeline_ref, mr_iid, target_run_id, created_at)
		 VALUES ($1, $2, $3, 'mr_rework', 't', 'd', $4, $5, $6, $7, $8)`, id, o.user, o.repo, status, branch, mr, target, createdAt)
	return id
}

// rwlSeedFiller inserts n completed issue runs of the owner, set-based, newer than from.
func rwlSeedFiller(t *testing.T, pool *pgxpool.Pool, o rwlOwner, n int, from time.Time) {
	t.Helper()
	cliMustExec(t, pool,
		`INSERT INTO runs (user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, created_at)
		 SELECT $1, $2, 'issue', $3 + g, 't', 'd', 'completed', $4::timestamptz + g * interval '1 second'
		 FROM generate_series(1, $5::int) g`, o.user, o.repo, rwlIssueIID.Add(int64(n)), from, n)
}

type rwlFixture struct {
	h      *Handler
	router http.Handler
	pool   *pgxpool.Pool
	admin  uuid.UUID
	uza    string
	uzaID  uuid.UUID
	uzc    string
	jwt    string
	audit  *adrdAudit
}

func rwlNewFixture(t *testing.T) *rwlFixture {
	t.Helper()
	h, router, pool := cliLiveDB(t)
	h.cfg.AdminRunDebug = true
	f := &rwlFixture{h: h, router: router, pool: pool}
	f.admin = cliSeedUser(t, pool, true)
	rwlPurgeUsers(t, pool, `SELECT $1::uuid`, f.admin)
	f.uza, f.uzaID = cliInsertToken(t, pool, f.admin, clitoken.ScopeAdminRO, nil, false)
	f.uzc = cliMintToken(t, pool, f.admin, clitoken.ScopeUser)
	f.jwt = cliMintJWT(t, pool, f.admin)
	f.audit = adrdCaptureAudit(t)
	return f
}

func rwlPath(repo uuid.UUID, branch string, mr *int64) string {
	q := url.Values{"repo": {repo.String()}, "branch": {branch}}
	if mr != nil {
		q.Set("mr_iid", fmt.Sprint(*mr))
	}
	return "/api/admin/run-writers?" + q.Encode()
}

// get requests the route as the admin_ro token and returns the decoded 200 body.
func (f *rwlFixture) get(t *testing.T, repo uuid.UUID, branch string, mr *int64) apitypes.RunWritersDTO {
	t.Helper()
	rec := bearerReq(f.router, http.MethodGet, rwlPath(repo, branch, mr), f.uza)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET run-writers(%s, %q, %v) = %d, want 200\nbody: %s", repo, branch, mr, rec.Code, rec.Body.String())
	}
	var dto apitypes.RunWritersDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("decode: %v (body %.300s)", err, rec.Body.String())
	}
	return dto
}

func rwlIDs(dto apitypes.RunWritersDTO) map[string]apitypes.RunWriterDTO {
	out := map[string]apitypes.RunWriterDTO{}
	for _, w := range dto.Writers {
		out[w.RunID] = w
	}
	return out
}

func rwlWantExactly(t *testing.T, label string, dto apitypes.RunWritersDTO, want ...uuid.UUID) {
	t.Helper()
	got := rwlIDs(dto)
	if len(got) != len(want) {
		t.Errorf("%s: served %d writers %v, want %d", label, len(got), dto.Writers, len(want))
	}
	for _, id := range want {
		if _, ok := got[id.String()]; !ok {
			t.Errorf("%s: writer %s missing from %v", label, id, dto.Writers)
		}
	}
	if !dto.Complete {
		t.Errorf("%s: complete = false", label)
	}
}

// An active ci_fix older than 200 newer runs of the same owner is found; the owner-scoped
// run list (LIMIT 200) is the baseline that cannot see it.
func TestRunWritersOlderThan200LiveDB(t *testing.T) {
	f := rwlNewFixture(t)
	o := rwlSeedOwner(t, f.pool, "gitlab", "https://forge.e2e", rwlProject())
	base := time.Now().UTC().Add(-30 * 24 * time.Hour)
	writer := rwlSeedCIFix(t, f.pool, o, "agent/issue-1", "running", base)
	rwlSeedFiller(t, f.pool, o, 250, base)

	rows, err := store.New(f.pool).ListRunsForUser(context.Background(), store.ListRunsForUserParams{
		BackgroundGraceCutoff: pgtype.Timestamptz{Time: time.Now(), Valid: true}, UserID: o.user,
	})
	if err != nil {
		t.Fatalf("ListRunsForUser: %v", err)
	}
	if len(rows) != 200 {
		t.Fatalf("baseline vacuous: the owner run list returned %d rows, want a full 200", len(rows))
	}
	for _, r := range rows {
		if r.Run.ID == writer {
			t.Fatalf("baseline broken: the owner run list already contains the writer (%d rows)", len(rows))
		}
	}
	rwlWantExactly(t, "older than 200", f.get(t, o.repo, "agent/issue-1", nil), writer)
}

// A foreign owner's writer behind more than 500 newer irrelevant rows is found; the admin
// debug listing's first 500 rows is the baseline that cannot see it.
func TestRunWritersBehind500LiveDB(t *testing.T) {
	f := rwlNewFixture(t)
	project := rwlProject()
	target := rwlSeedOwner(t, f.pool, "gitlab", "https://forge.e2e", project)
	foreign := rwlSeedOwner(t, f.pool, "gitlab", "https://forge.e2e", project)
	noise := rwlSeedOwner(t, f.pool, "gitlab", "https://forge.e2e", rwlProject())
	base := time.Now().UTC().Add(-30 * 24 * time.Hour)
	writer := rwlSeedCIFix(t, f.pool, foreign, "agent/issue-2", "queued", base)
	rwlSeedFiller(t, f.pool, noise, 600, base)

	rows, err := store.New(f.pool).ListRunsAdminDebug(context.Background(), store.ListRunsAdminDebugParams{
		BackgroundGraceCutoff: pgtype.Timestamptz{Time: time.Now(), Valid: true}, MaxRows: 501,
	})
	if err != nil {
		t.Fatalf("ListRunsAdminDebug: %v", err)
	}
	if len(rows) != 501 {
		t.Fatalf("baseline vacuous: admin debug listing returned %d rows, want 501 before truncating", len(rows))
	}
	if len(rows) > 500 {
		rows = rows[:500]
	}
	for _, r := range rows {
		if r.Run.ID == writer {
			t.Fatalf("baseline broken: the admin debug listing's first 500 rows contain the writer")
		}
	}
	dto := f.get(t, target.repo, "agent/issue-2", nil)
	rwlWantExactly(t, "behind 500", dto, writer)
	if w := rwlIDs(dto)[writer.String()]; w.OwnerID != foreign.user.String() || w.RepoID != foreign.repo.String() ||
		w.Kind != "ci_fix" || w.Status != "queued" || w.PipelineRef == nil || *w.PipelineRef != "agent/issue-2" || w.MRIID != nil {
		t.Errorf("writer metadata = %+v", w)
	}
}

// Two owners' connections to the same project are the same project; a different project id
// (a fork) and the same project id on a different forge type are not.
func TestRunWritersSameProjectLiveDB(t *testing.T) {
	f := rwlNewFixture(t)
	project := rwlProject()
	now := time.Now().UTC()
	a := rwlSeedOwner(t, f.pool, "gitlab", "https://forge.e2e", project)
	b := rwlSeedOwner(t, f.pool, "gitlab", "https://forge.e2e", project)
	fork := rwlSeedOwner(t, f.pool, "gitlab", "https://forge.e2e", rwlProject())
	otherForge := rwlSeedOwner(t, f.pool, "forgejo", "https://forge.e2e", project)
	wa := rwlSeedCIFix(t, f.pool, a, "feat/x", "running", now)
	wb := rwlSeedCIFix(t, f.pool, b, "feat/x", "queued", now.Add(time.Second))
	rwlSeedCIFix(t, f.pool, fork, "feat/x", "running", now)
	rwlSeedCIFix(t, f.pool, otherForge, "feat/x", "running", now)

	for _, from := range []rwlOwner{a, b} {
		dto := f.get(t, from.repo, "feat/x", nil)
		rwlWantExactly(t, "two owners", dto, wa, wb)
		if dto.Writers[0].RunID != wa.String() || dto.Writers[1].RunID != wb.String() {
			t.Errorf("order = %v, want oldest first", dto.Writers)
		}
	}
	rwlWantExactly(t, "fork", f.get(t, fork.repo, "feat/x", nil), mustFirstWriter(t, f, fork))
	rwlWantExactly(t, "other forge type", f.get(t, otherForge.repo, "feat/x", nil), mustFirstWriter(t, f, otherForge))
}

func mustFirstWriter(t *testing.T, f *rwlFixture, o rwlOwner) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := f.pool.QueryRow(context.Background(), `SELECT id FROM runs WHERE repo_id = $1 AND kind = 'ci_fix'`, o.repo).Scan(&id); err != nil {
		t.Fatalf("lookup writer: %v", err)
	}
	return id
}

// The instance base URL is compared canonically: a trailing :443 is the same instance, and
// for github only api.github.com is github.com. Other ports and other hosts are not.
func TestRunWritersInstanceSpellingsLiveDB(t *testing.T) {
	f := rwlNewFixture(t)
	cases := []struct {
		forge, target, other string
		want                 bool
	}{
		{"github", "https://github.com", "https://github.com:443", true},
		{"github", "https://github.com:443", "https://github.com", true},
		{"github", "https://github.com", "https://api.github.com", true},
		{"github", "https://api.github.com", "https://github.com:443", true},
		{"github", "https://api.github.com:443", "https://github.com", true},
		{"github", "https://github.com", "https://github.com:8443", false},
		{"github", "https://github.com", "https://ghe.example.com", false},
		{"gitlab", "https://gitlab.example.com", "https://gitlab.example.com:443", true},
		{"gitlab", "https://gitlab.example.com:443", "https://gitlab.example.com", true},
		{"gitlab", "https://gitlab.example.com", "https://gitlab.example.com:8443", false},
		{"gitlab", "https://gitlab.example.com", "https://other.example.com", false},
		// api.github.com is github.com for a github forge only.
		{"gitlab", "https://github.com", "https://api.github.com", false},
		{"forgejo", "https://github.com", "https://api.github.com", false},
	}
	for _, tc := range cases {
		t.Run(tc.forge+" "+tc.target+" vs "+tc.other, func(t *testing.T) {
			project := rwlProject()
			target := rwlSeedOwner(t, f.pool, tc.forge, tc.target, project)
			other := rwlSeedOwner(t, f.pool, tc.forge, tc.other, project)
			writer := rwlSeedCIFix(t, f.pool, other, "br", "running", time.Now().UTC())
			dto := f.get(t, target.repo, "br", nil)
			_, found := rwlIDs(dto)[writer.String()]
			if found != tc.want {
				t.Errorf("writer on %q found from a repo on %q = %v, want %v", tc.other, tc.target, found, tc.want)
			}
			// Parity with the Go rule: github rows must agree with settings.CanonicalInstance.
			if tc.forge == "github" {
				if goMatch := settings.CanonicalInstance(tc.target) == settings.CanonicalInstance(tc.other); goMatch != found {
					t.Errorf("SQL match = %v but Go CanonicalInstance match = %v", found, goMatch)
				}
			}
		})
	}
}

// Branch matching is exact; an mr_rework is matched only by the supplied MR, never by branch.
func TestRunWritersBranchAndMRMatchingLiveDB(t *testing.T) {
	f := rwlNewFixture(t)
	o := rwlSeedOwner(t, f.pool, "gitlab", "https://forge.e2e", rwlProject())
	now := time.Now().UTC()
	ci := rwlSeedCIFix(t, f.pool, o, "agent/issue-7", "running", now)
	mrw := rwlSeedMRRework(t, f.pool, o, "agent/issue-9", 77, "queued", now.Add(time.Second))
	mr77, mr78 := int64(77), int64(78)
	// An active mr_rework with the same MR number on another project (a fork) is a different
	// MR: every case below, counted exactly, must exclude it.
	fork := rwlSeedOwner(t, f.pool, "gitlab", "https://forge.e2e", rwlProject())
	forkMRW := rwlSeedMRRework(t, f.pool, fork, "agent/issue-9", 77, "running", now.Add(2*time.Second))
	rwlWantExactly(t, "fork sees only its own mr", f.get(t, fork.repo, "agent/issue-9", &mr77), forkMRW)

	cases := []struct {
		name   string
		branch string
		mr     *int64
		want   []uuid.UUID
	}{
		{"exact branch", "agent/issue-7", nil, []uuid.UUID{ci}},
		{"prefix", "agent/issue", nil, nil},
		{"case variant", "Agent/issue-7", nil, nil},
		{"longer", "agent/issue-77", nil, nil},
		{"mr_rework branch without mr", "agent/issue-9", nil, nil},
		{"ci_fix branch with the mr", "agent/issue-7", &mr77, []uuid.UUID{ci, mrw}},
		{"mr alone matches regardless of branch", "unrelated", &mr77, []uuid.UUID{mrw}},
		{"other mr", "agent/issue-7", &mr78, []uuid.UUID{ci}},
		{"other mr, mr_rework branch", "agent/issue-9", &mr78, nil},
	}
	for _, tc := range cases {
		dto := f.get(t, o.repo, tc.branch, tc.mr)
		rwlWantExactly(t, tc.name, dto, tc.want...)
	}
}

// Each nonterminal status counts; the three terminal ones do not.
func TestRunWritersStatusesLiveDB(t *testing.T) {
	f := rwlNewFixture(t)
	o := rwlSeedOwner(t, f.pool, "gitlab", "https://forge.e2e", rwlProject())
	now := time.Now().UTC()
	active := []string{"queued", "claimed", "running", "awaiting_approval", "awaiting_input", "limit_wait",
		"awaiting_followup", "pool_wait", "paused", "recovery_wait"}
	terminal := []string{"completed", "failed", "cancelled"}
	ids := map[string]uuid.UUID{}
	for _, s := range append(append([]string{}, active...), terminal...) {
		ids[s] = rwlSeedCIFix(t, f.pool, o, "st-"+s, s, now)
	}
	// An mr_rework in every status, each with its own MR, follows the same rule.
	mrIDs := map[string]uuid.UUID{}
	mrOf := map[string]int64{}
	for i, s := range append(append([]string{}, active...), terminal...) {
		mrOf[s] = int64(100 + i)
		mrIDs[s] = rwlSeedMRRework(t, f.pool, o, "mrw-"+s, mrOf[s], s, now)
	}
	for _, s := range active {
		mr := mrOf[s]
		rwlWantExactly(t, "status "+s, f.get(t, o.repo, "st-"+s, nil), ids[s])
		rwlWantExactly(t, "mr_rework status "+s, f.get(t, o.repo, "no-such-branch", &mr), mrIDs[s])
	}
	for _, s := range terminal {
		mr := mrOf[s]
		rwlWantExactly(t, "status "+s, f.get(t, o.repo, "st-"+s, nil))
		rwlWantExactly(t, "mr_rework status "+s, f.get(t, o.repo, "no-such-branch", &mr))
	}
}

// 501 matching writers serve 500 with complete=false; an empty result is complete.
func TestRunWritersOverflowLiveDB(t *testing.T) {
	f := rwlNewFixture(t)
	project := rwlProject()
	target := rwlSeedOwner(t, f.pool, "gitlab", "https://forge.e2e", project)
	const branch = "overflow-branch"
	rwlPurgeUsers(t, f.pool, `SELECT c.user_id FROM repos r JOIN forge_connections c ON c.id = r.connection_id WHERE r.forge_project_id = $1`, project)
	cliMustExec(t, f.pool, `
		WITH n AS (SELECT g, gen_random_uuid() AS uid, gen_random_uuid() AS cid, gen_random_uuid() AS rid FROM generate_series(1, 501) g),
		u AS (INSERT INTO users (id, email, password_hash, is_admin, vault_mode)
		      SELECT uid, 'rwo-' || uid || '@e2e', 'x', false, 'unprotected' FROM n),
		c AS (INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		      SELECT cid, uid, 'gitlab', 'https://forge.e2e', 'bot', 1, '\x01'::bytea FROM n),
		r AS (INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		      SELECT rid, cid, $1, 'g/r', 'https://forge.e2e/g/r', 'main', true FROM n)
		INSERT INTO runs (user_id, repo_id, kind, issue_title, issue_description, status, pipeline_id, pipeline_ref, created_at)
		SELECT uid, rid, 'ci_fix', 't', 'd', 'queued', 1, $2, now() - interval '30 days' + g * interval '1 second' FROM n`, project, branch)

	dto := f.get(t, target.repo, branch, nil)
	if len(dto.Writers) != 500 || dto.Complete {
		t.Errorf("served %d writers complete=%v, want 500 and false", len(dto.Writers), dto.Complete)
	}
	recs := f.audit.take(t)
	if len(recs) != 1 || recs[0]["complete"] != false || recs[0]["returned"] != float64(500) {
		t.Errorf("audit = %v, want one record with returned 500 complete false", recs)
	}

	// The store boundary: asking for exactly the number of rows that exist, and one fewer.
	rows, err := store.ListRunWriters(context.Background(), f.pool, target.repo, branch, nil, 501)
	if err != nil || len(rows) != 501 {
		t.Fatalf("store read with 501 = %d rows, %v", len(rows), err)
	}
	rows, err = store.ListRunWriters(context.Background(), f.pool, target.repo, branch, nil, 3)
	if err != nil || len(rows) != 3 {
		t.Fatalf("store read with 3 = %d rows, %v", len(rows), err)
	}

	empty := f.get(t, target.repo, "no-such-branch", nil)
	if len(empty.Writers) != 0 || !empty.Complete {
		t.Errorf("empty result = %+v, want no writers and complete", empty)
	}
}

// The route end to end with really minted credentials: the 200 body and its single audit
// record, the refusals and an unknown repo.
func TestRunWritersRouteLiveDB(t *testing.T) {
	f := rwlNewFixture(t)
	o := rwlSeedOwner(t, f.pool, "gitlab", "https://forge.e2e", rwlProject())
	writer := rwlSeedCIFix(t, f.pool, o, "secret-branch-xyz", "running", time.Now().UTC())

	rec := bearerReq(f.router, http.MethodGet, rwlPath(o.repo, "secret-branch-xyz", nil), f.uza)
	if rec.Code != http.StatusOK {
		t.Fatalf("uza_ = %d: %s", rec.Code, rec.Body.String())
	}
	var env map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil || len(env) != 2 {
		t.Fatalf("envelope = %s (%v)", rec.Body.String(), err)
	}
	recs := f.audit.take(t)
	if len(recs) != 1 {
		t.Fatalf("audit records = %d, want 1: %v", len(recs), recs)
	}
	got := recs[0]
	if got["route"] != "admin.run_writers" || got["actor_user_id"] != f.admin.String() || got["token_id"] != f.uzaID.String() ||
		got["repo_id"] != o.repo.String() || got["mr_iid"] != "none" || got["branch_bytes"] != float64(len("secret-branch-xyz")) ||
		got["returned"] != float64(1) || got["complete"] != true {
		t.Errorf("audit record = %v", got)
	}
	if strings.Contains(fmt.Sprint(got), "secret-branch-xyz") {
		t.Errorf("audit record carries the branch text: %v", got)
	}
	if !strings.Contains(rec.Body.String(), writer.String()) {
		t.Errorf("body lacks the writer: %s", rec.Body.String())
	}

	// A valid uzc_ or cookie credential authenticates (RequireUser) and is then refused as
	// non-admin_ro, so exactly 403. Only a request with no credential at all is 401: it is
	// refused by RequireUser before RequireAdminRO (middleware/cli_auth.go) is reached.
	refused := []struct {
		name string
		want int
		rec  func() int
	}{
		{"uzc_", http.StatusForbidden, func() int { return bearerReq(f.router, http.MethodGet, rwlPath(o.repo, "b", nil), f.uzc).Code }},
		{"cookie", http.StatusForbidden, func() int {
			return cookieReq(t, f.router, http.MethodGet, rwlPath(o.repo, "b", nil), f.jwt, "").Code
		}},
		{"no credential", http.StatusUnauthorized, func() int { return bearerReq(f.router, http.MethodGet, rwlPath(o.repo, "b", nil), "").Code }},
	}
	for _, tc := range refused {
		if code := tc.rec(); code != tc.want {
			t.Errorf("%s = %d, want %d", tc.name, code, tc.want)
		}
	}
	if rec := bearerReq(f.router, http.MethodGet, rwlPath(uuid.New(), "b", nil), f.uza); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("unknown repo = %d, want 422: %s", rec.Code, rec.Body.String())
	}
	if rec := bearerReq(f.router, http.MethodGet, "/api/admin/run-writers?repo=nope&branch=b", f.uza); rec.Code != http.StatusBadRequest {
		t.Errorf("bad repo = %d, want 400", rec.Code)
	}
	if recs := f.audit.take(t); len(recs) != 0 {
		t.Errorf("refusals and errors wrote %d audit records: %v", len(recs), recs)
	}

	// With the deployment flag off the same uza_ is refused by the handler.
	f.h.cfg.AdminRunDebug = false
	if rec := bearerReq(f.router, http.MethodGet, rwlPath(o.repo, "secret-branch-xyz", nil), f.uza); rec.Code != http.StatusForbidden {
		t.Errorf("flag off = %d, want 403", rec.Code)
	}
}
