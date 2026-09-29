package store_test

import (
	"context"
	"regexp"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Live-DB coverage for PRD #1908 M1 (migration 00271): the repo-less `job` run kind, its
// job_type discriminator, the four job tables and products.allowed_job_types. Skipped unless
// UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via e2e/run-store-it.sh.

// insertJobRun inserts a valid repo-less job run for the user and returns its id.
func insertJobRun(ctx context.Context, t *testing.T, pool *pgxpool.Pool, userID uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := pool.QueryRow(ctx,
		`INSERT INTO runs (user_id, issue_title, issue_description, kind, job_type)
		 VALUES ($1, 'job', 'do the research', 'job', 'research') RETURNING id`, userID).Scan(&id)
	if err != nil {
		t.Fatalf("insert a repo-less job run: %v", err)
	}
	return id
}

func TestJobRunShapeLiveDB(t *testing.T) {
	ctx, pool, _ := productLiveDB(t)
	userID, _ := newProductUser(ctx, t, pool)

	// A repo-less job with a job_type inserts, and reads back repo-less and typed.
	id := insertJobRun(ctx, t, pool, userID)
	var kind, jobType string
	var repoID, issueIid, branch *string
	if err := pool.QueryRow(ctx,
		`SELECT kind, job_type, repo_id::text, issue_iid::text, branch FROM runs WHERE id = $1`, id).
		Scan(&kind, &jobType, &repoID, &issueIid, &branch); err != nil {
		t.Fatalf("read back job run: %v", err)
	}
	if kind != "job" || jobType != "research" || repoID != nil || issueIid != nil || branch != nil {
		t.Fatalf("job run = kind %q type %q repo %v issue %v branch %v; want a repo-less research job", kind, jobType, repoID, issueIid, branch)
	}

	rejected := []struct {
		name, cols, vals string
	}{
		{"job with an issue_iid", "kind, job_type, issue_iid", "'job', 'research', 7"},
		{"job with a branch", "kind, job_type, branch", "'job', 'research', 'agent/x'"},
		{"job without a job_type", "kind", "'job'"},
		{"job with an unknown job_type", "kind, job_type", "'job', 'nope'"},
		{"job_type on a chat run", "kind, job_type", "'chat', 'research'"},
	}
	for _, c := range rejected {
		t.Run(c.name, func(t *testing.T) {
			if err := insertRunShape(ctx, pool, userID, c.cols, c.vals); !isCheckViolation(err) {
				t.Fatalf("%s must violate a CHECK (23514), got %v", c.name, err)
			}
		})
	}
	// job_type on a repo-bound kind is rejected too: the (kind='job') = (job_type IS NOT NULL)
	// CHECK fires before the shape does, so the repo need not exist for the assertion to hold.
	if err := insertRunShape(ctx, pool, userID, "kind, job_type, issue_iid", "'issue', 'research', 7"); err == nil {
		t.Fatal("job_type on a non-job kind must be rejected")
	}
}

func TestJobInputsLiveDB(t *testing.T) {
	ctx, pool, _ := productLiveDB(t)
	userID, _ := newProductUser(ctx, t, pool)
	id := insertJobRun(ctx, t, pool, userID)

	ins := func(ordinal int, name string) error {
		_, err := pool.Exec(ctx,
			`INSERT INTO job_inputs (run_id, ordinal, name, content_md) VALUES ($1, $2, $3, 'body')`, id, ordinal, name)
		return err
	}
	for i, name := range []string{"notes.md", "a", "A1._-b", "0lead"} {
		if err := ins(i, name); err != nil {
			t.Fatalf("valid input name %q rejected: %v", name, err)
		}
	}
	for i, name := range []string{"../x", "a/b", ".x", "a..b", "", "-x", "a b", "x\n"} {
		if err := ins(100+i, name); !isCheckViolation(err) {
			t.Errorf("input name %q must violate the name CHECK (23514), got %v", name, err)
		}
	}
	// A 101-character name is over the pattern's length bound.
	long := "a"
	for len(long) < 101 {
		long += "a"
	}
	if err := ins(200, long); !isCheckViolation(err) {
		t.Errorf("a 101-character input name must violate the name CHECK, got %v", err)
	}
	// (run_id, name) is unique.
	if err := ins(300, "notes.md"); !isUniqueViolation(err) {
		t.Errorf("a duplicate (run_id, name) must be a unique violation, got %v", err)
	}

	// Deleting the run cascades to its inputs.
	mustExec(ctx, t, pool, `DELETE FROM runs WHERE id = $1`, id)
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM job_inputs WHERE run_id = $1`, id).Scan(&n); err != nil || n != 0 {
		t.Fatalf("job_inputs must cascade with the run; count = %d, %v", n, err)
	}
}

func TestJobResultTablesLiveDB(t *testing.T) {
	ctx, pool, q := productLiveDB(t)
	userID, _ := newProductUser(ctx, t, pool)
	product := newProduct(ctx, t, q, userID)
	id := insertJobRun(ctx, t, pool, userID)

	// job_origins: a uzc_ caller has no product, a uzp_ caller has one; the label is capped.
	mustExec(ctx, t, pool, `INSERT INTO job_origins (run_id, product_id, requested_by_label) VALUES ($1, $2, 'alice')`, id, product.ID)
	other := insertJobRun(ctx, t, pool, userID)
	mustExec(ctx, t, pool, `INSERT INTO job_origins (run_id) VALUES ($1)`, other)
	third := insertJobRun(ctx, t, pool, userID)
	label := make([]byte, 201)
	for i := range label {
		label[i] = 'x'
	}
	if _, err := pool.Exec(ctx, `INSERT INTO job_origins (run_id, requested_by_label) VALUES ($1, $2)`, third, string(label)); !isCheckViolation(err) {
		t.Errorf("a 201-byte requested_by_label must violate its CHECK, got %v", err)
	}
	// A product with jobs cannot be hard-deleted (RESTRICT).
	if _, err := pool.Exec(ctx, `DELETE FROM products WHERE id = $1`, product.ID); err == nil {
		t.Error("deleting a product that has a job origin must be refused")
	}

	// job_results: one row per run.
	mustExec(ctx, t, pool, `INSERT INTO job_results (run_id, status, report_md) VALUES ($1, 'completed', '# done')`, id)
	if _, err := pool.Exec(ctx, `INSERT INTO job_results (run_id, status) VALUES ($1, 'completed')`, id); !isUniqueViolation(err) {
		t.Errorf("a second job_results row for one run must be a unique violation, got %v", err)
	}

	// job_findings.
	ins := func(ordinal int, severity string, url, file any, line any) error {
		_, err := pool.Exec(ctx,
			`INSERT INTO job_findings (run_id, ordinal, severity, message_md, url, file, line)
			 VALUES ($1, $2, $3, 'msg', $4, $5, $6)`, id, ordinal, severity, url, file, line)
		return err
	}
	if err := ins(0, "info", nil, nil, nil); err != nil {
		t.Fatalf("a bare finding must insert: %v", err)
	}
	if err := ins(1, "warning", "https://example.test/x", nil, nil); err != nil {
		t.Fatalf("a url finding must insert: %v", err)
	}
	if err := ins(2, "error", nil, "src/a.go", 12); err != nil {
		t.Fatalf("a file+line finding must insert: %v", err)
	}
	for name, err := range map[string]error{
		"an unknown severity":   ins(3, "fatal", nil, nil, nil),
		"both url and file":     ins(4, "info", "https://example.test/x", "a.go", nil),
		"a line without a file": ins(5, "info", nil, nil, 3),
		"a negative line":       ins(6, "info", nil, "a.go", -1),
		"a negative ordinal":    ins(-1, "info", nil, nil, nil),
	} {
		if !isCheckViolation(err) {
			t.Errorf("%s must violate a CHECK (23514), got %v", name, err)
		}
	}
	if err := ins(2, "info", nil, nil, nil); !isUniqueViolation(err) {
		t.Errorf("a duplicate (run_id, ordinal) finding must be a unique violation, got %v", err)
	}
}

func TestJobFailOriginsAcceptedLiveDB(t *testing.T) {
	ctx, pool, _ := productLiveDB(t)
	userID, _ := newProductUser(ctx, t, pool)
	for _, origin := range []string{"no_job_capable_worker", "ephemeral_worker_never_registered", "job_no_result"} {
		id := insertJobRun(ctx, t, pool, userID)
		if _, err := pool.Exec(ctx,
			`UPDATE runs SET status = 'failed', fail_origin = $2, finished_at = now() WHERE id = $1`, id, origin); err != nil {
			t.Errorf("fail_origin %q must be accepted by runs_fail_origin_check: %v", origin, err)
		}
	}
	id := insertJobRun(ctx, t, pool, userID)
	if _, err := pool.Exec(ctx, `UPDATE runs SET fail_origin = 'not_a_real_origin' WHERE id = $1`, id); !isCheckViolation(err) {
		t.Errorf("an unknown fail_origin must still violate the CHECK, got %v", err)
	}
}

func TestProductAllowedJobTypesLiveDB(t *testing.T) {
	ctx, pool, q := productLiveDB(t)
	userID, _ := newProductUser(ctx, t, pool)
	product := newProduct(ctx, t, q, userID)

	var def []string
	if err := pool.QueryRow(ctx, `SELECT allowed_job_types FROM products WHERE id = $1`, product.ID).Scan(&def); err != nil || len(def) != 0 {
		t.Fatalf("a new product's allowed_job_types must default to empty, got %v, %v", def, err)
	}
	set := func(v []string) error {
		_, err := pool.Exec(ctx, `UPDATE products SET allowed_job_types = $2 WHERE id = $1`, product.ID, v)
		return err
	}
	if err := set([]string{"research"}); err != nil {
		t.Fatalf("the known job type must be accepted: %v", err)
	}
	if err := set([]string{}); err != nil {
		t.Fatalf("an empty allow-list must be accepted: %v", err)
	}
	for name, v := range map[string][]string{
		"an unknown value":           {"bogus"},
		"a known plus unknown value": {"research", "bogus"},
		"a case variant":             {"Research"},
	} {
		if err := set(v); !isCheckViolation(err) {
			t.Errorf("%s must violate products_allowed_job_types_check, got %v", name, err)
		}
	}
	// A NULL element is not a known value, so the array is rejected. The array literal goes
	// through SQL because a Go []string cannot carry a NULL element.
	if _, err := pool.Exec(ctx, `UPDATE products SET allowed_job_types = ARRAY['research', NULL]::text[] WHERE id = $1`, product.ID); !isCheckViolation(err) {
		t.Errorf("a NULL element must violate products_allowed_job_types_check, got %v", err)
	}
	// NULL for the column itself is refused by NOT NULL.
	if _, err := pool.Exec(ctx, `UPDATE products SET allowed_job_types = NULL WHERE id = $1`, product.ID); err == nil {
		t.Error("a NULL allowed_job_types must be refused")
	}
}

// TestJobTypesMatchDBChecksLiveDB pins runkind.JobTypes() to BOTH CHECK constraints that
// enumerate the job types, read from the live catalog rather than restated.
func TestJobTypesMatchDBChecksLiveDB(t *testing.T) {
	ctx, pool, _ := productLiveDB(t)
	lit := regexp.MustCompile(`'([^']+)'`)
	for _, c := range []struct{ table, constraint string }{
		{"runs", "runs_job_type_check"},
		{"products", "products_allowed_job_types_check"},
	} {
		var def string
		if err := pool.QueryRow(ctx,
			`SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = $1 AND conrelid = $2::regclass`,
			c.constraint, c.table).Scan(&def); err != nil {
			t.Fatalf("read %s: %v", c.constraint, err)
		}
		var got []string
		for _, m := range lit.FindAllStringSubmatch(def, -1) {
			got = append(got, m[1])
		}
		slices.Sort(got)
		want := slices.Clone(runkind.JobTypes())
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("%s allows %v; runkind.JobTypes() = %v (def: %s)", c.constraint, got, want, def)
		}
	}
}

// TestFnWorkerCanClaimNeverAdmitsDockerJobLiveDB: fn_worker_can_claim is deliberately unchanged
// by migration 00271. A docker worker admits only allow-listed repos plus repo-less JUDGE runs,
// so a repo-less job is not admitted. The function can return NULL (a NULL run_repo_id makes the
// allow-list arm NULL), so the result is scanned into a nullable bool and asserted IS NOT TRUE.
func TestFnWorkerCanClaimNeverAdmitsDockerJobLiveDB(t *testing.T) {
	ctx, pool, _ := productLiveDB(t)
	can := func(isDocker bool, kind string, capAware bool) *bool {
		t.Helper()
		var got *bool
		err := pool.QueryRow(ctx,
			`SELECT fn_worker_can_claim($1, '{}'::uuid[], NULL::uuid, $2, '{}'::text[], '{}'::text[], $3)`,
			isDocker, kind, capAware).Scan(&got)
		if err != nil {
			t.Fatalf("fn_worker_can_claim(docker=%v, kind=%s): %v", isDocker, kind, err)
		}
		return got
	}
	for _, capAware := range []bool{true, false} {
		if got := can(true, "job", capAware); got != nil && *got {
			t.Errorf("a docker worker must not admit a repo-less job (capability_aware=%v), got TRUE", capAware)
		}
	}
	// Sanity: the same call shape admits a repo-less judge on a docker worker, so the job
	// refusal above is the kind, not a malformed call.
	if got := can(true, "judge", false); got == nil || !*got {
		t.Errorf("a docker worker must still admit a repo-less judge, got %v", got)
	}
}

// TestChatAgentRunReadsExcludeJobLiveDB: the chat agent's worker-scoped run reads exclude job
// runs (as they exclude judge) — the list omits the job and the detail read is a 404-equivalent
// no-row, while the user's other runs stay visible.
func TestChatAgentRunReadsExcludeJobLiveDB(t *testing.T) {
	ctx, pool, q := productLiveDB(t)
	userID, _ := newProductUser(ctx, t, pool)
	jobID := insertJobRun(ctx, t, pool, userID)
	var chatID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO runs (user_id, issue_title, issue_description, kind) VALUES ($1, 'c', 'd', 'chat') RETURNING id`,
		userID).Scan(&chatID); err != nil {
		t.Fatalf("insert chat run: %v", err)
	}

	rows, err := q.ListRunsForWorkerUser(ctx, store.ListRunsForWorkerUserParams{UserID: userID, Lim: 50})
	if err != nil {
		t.Fatalf("ListRunsForWorkerUser: %v", err)
	}
	var sawChat bool
	for _, r := range rows {
		if r.ID == jobID {
			t.Fatalf("ListRunsForWorkerUser must not list a job run: %+v", r)
		}
		if r.ID == chatID {
			sawChat = true
		}
	}
	if !sawChat {
		t.Fatalf("ListRunsForWorkerUser must still list the user's chat run; got %d rows", len(rows))
	}
	if _, err := q.GetRunForWorkerUser(ctx, store.GetRunForWorkerUserParams{ID: jobID, UserID: userID}); err != pgx.ErrNoRows {
		t.Fatalf("GetRunForWorkerUser(job) must be no row, got %v", err)
	}
	if _, err := q.GetRunForWorkerUser(ctx, store.GetRunForWorkerUserParams{ID: chatID, UserID: userID}); err != nil {
		t.Fatalf("GetRunForWorkerUser(chat) must still resolve: %v", err)
	}
}
