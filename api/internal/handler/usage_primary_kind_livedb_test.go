package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Issue #4209: run counts and outcomes on GET /api/usage and GET /api/admin/usage measure
// primary work runs; token and cost sums still include helper runs. The wire shape does not
// change. Everything runs inside one rolled-back transaction so the factory totals are
// exact, and every expectation is a literal derived from the seeded rows below.

type pkHandlerRun struct {
	user   uuid.UUID
	kind   string
	status string
	origin string
	age    string
	factor int64 // 0 = no run_usage row
	codex  bool
}

func pkJSONKeySet(t reflect.Type) []string {
	var keys []string
	for i := 0; i < t.NumField(); i++ {
		tag := strings.Split(t.Field(i).Tag.Get("json"), ",")[0]
		if tag != "" && tag != "-" {
			keys = append(keys, tag)
		}
	}
	sort.Strings(keys)
	return keys
}

func pkRawKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestUsagePrimaryKindWireLiveDB(t *testing.T) {
	ctx := context.Background()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec("TRUNCATE runs CASCADE")

	userA, userB, connID, repoID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	emailA, emailB := fmt.Sprintf("pk4209a-%s@e2e", userA), fmt.Sprintf("pk4209b-%s@e2e", userB)
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x'), ($3, $4, 'x')`, userA, emailA, userB, emailB)
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userA, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 4209, 'g/pk4209-'||$3, 'https://forge.e2e/g/pk4209', 'main', true)`, repoID, connID, repoID.String())

	n := 0
	var first uuid.UUID
	seed := func(r pkHandlerRun) uuid.UUID {
		t.Helper()
		n++
		id := uuid.New()
		var repo, iid, target, budget, origin, mr, branch, pipelineID, pipelineRef, jobType any = repoID, int64(420900 + n), nil, nil, nil, nil, nil, nil, nil, nil
		harness, reportOnly := "claude", false
		if r.codex {
			harness = "codex"
		}
		switch r.kind {
		case "chat", "judge":
			repo, iid = nil, nil
		case "task":
			iid, branch = nil, fmt.Sprintf("uzi/task/%d", n)
		case "ci_fix":
			iid, pipelineID, pipelineRef = nil, int64(n), "ci-ref"
		case "mr_rework":
			iid, pipelineRef, mr = nil, "mr-ref", int64(910000+n)
		}
		if r.kind == "judge" || r.kind == "cross_check" || r.kind == "mr_rework" {
			target = first
		}
		if r.kind == "cross_check" {
			iid, harness, reportOnly, budget = nil, "codex", true, 60
		}
		if r.origin != "" {
			origin = r.origin
		}
		exec(`INSERT INTO runs (id,user_id,repo_id,issue_iid,issue_title,issue_description,kind,status,fail_origin,created_at,finished_at,
		                        target_run_id,harness,report_only,budget_wall_seconds,mr_iid,branch,pipeline_id,pipeline_ref,job_type)
		      VALUES ($1,$2,$3,$4,'t','d',$5,$6,$7,now()-$8::interval,now(),$9,$10,$11,$12,$13,$14,$15,$16,$17)`,
			id, r.user, repo, iid, r.kind, r.status, origin, r.age, target, harness, reportOnly, budget, mr, branch, pipelineID, pipelineRef, jobType)
		if r.factor != 0 {
			exec(`INSERT INTO run_usage (run_id,session_id,model,input_tokens,cache_read_tokens,cache_creation_tokens,output_tokens,cost_usd,cost_status)
			      VALUES ($1,'s','m',$2,$3,$4,$5,$6::numeric/1000000,'metered')`, id, r.factor, 2*r.factor, 3*r.factor, 4*r.factor, r.factor*5)
		}
		return id
	}
	first = seed(pkHandlerRun{user: userA, kind: "issue", status: "completed", age: "30 days"}) // primary, no usage
	// User A: primary runs with and without usage plus every helper kind.
	seed(pkHandlerRun{user: userA, kind: "issue", status: "completed", age: "30 days", factor: 10})
	seed(pkHandlerRun{user: userA, kind: "issue", status: "completed", age: "1 day", factor: 10})
	seed(pkHandlerRun{user: userA, kind: "issue", status: "failed", origin: "agent_failure", age: "1 day"})
	seed(pkHandlerRun{user: userA, kind: "judge", status: "completed", age: "1 day", factor: 100})
	seed(pkHandlerRun{user: userA, kind: "cross_check", status: "completed", age: "1 day", factor: 1000})
	seed(pkHandlerRun{user: userA, kind: "chat", status: "completed", age: "1 day", factor: 5})
	seed(pkHandlerRun{user: userA, kind: "ci_fix", status: "failed", origin: "agent_failure", age: "1 day", factor: 7})
	// User B: helper runs only, with usage and a helper failure.
	seed(pkHandlerRun{user: userB, kind: "judge", status: "completed", age: "1 day", factor: 200})
	seed(pkHandlerRun{user: userB, kind: "mr_rework", status: "completed", age: "1 day", factor: 300})
	seed(pkHandlerRun{user: userB, kind: "ci_fix", status: "failed", origin: "agent_failure", age: "1 day"})

	q := store.New(tx)
	h := &Handler{q: q, wsvc: workersvc.New(q, nil, workersvc.Params{CustodyHoldLimit: defaultCustodyLimit()})}
	call := func(fn http.HandlerFunc, u uuid.UUID, admin bool) map[string]any {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req = req.WithContext(mw.ContextWithUser(req.Context(), store.User{ID: u, IsAdmin: admin, IsActive: true}))
		rec := httptest.NewRecorder()
		fn(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		var raw map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
			t.Fatal(err)
		}
		return raw
	}
	num := func(m map[string]any, k string) float64 {
		t.Helper()
		v, ok := m[k].(float64)
		if !ok {
			t.Fatalf("key %q missing or not a number in %v", k, pkRawKeys(m))
		}
		return v
	}
	sub := func(m map[string]any, k string) map[string]any {
		t.Helper()
		v, ok := m[k].(map[string]any)
		if !ok {
			t.Fatalf("key %q missing or not an object in %v", k, pkRawKeys(m))
		}
		return v
	}
	eq := func(label string, got float64, want float64) {
		t.Helper()
		if got != want {
			t.Errorf("%s = %v, want %v", label, got, want)
		}
	}
	cost := func(label string, got, wantMicro float64) {
		t.Helper()
		if math.Abs(got-wantMicro/1e6) > 1e-9 {
			t.Errorf("%s = %v, want %v micro-USD", label, got, wantMicro)
		}
	}
	keys := func(label string, m map[string]any, typ reflect.Type) {
		t.Helper()
		if got, want := pkRawKeys(m), pkJSONKeySet(typ); !reflect.DeepEqual(got, want) {
			t.Errorf("%s keys = %v, want %v", label, got, want)
		}
	}
	// Literal keys the web and CLI read; a rename fails here even if the DTO is edited alongside.
	for _, k := range []string{"run_count", "last7_run_count", "harness_runs", "last7_harness_runs", "lifetime_subscription_run_count"} {
		found := false
		for _, have := range append(pkJSONKeySet(reflect.TypeOf(apitypes.SelfUsageDTO{})), pkJSONKeySet(reflect.TypeOf(apitypes.AdminUserUsageDTO{}))...) {
			found = found || have == k
		}
		if !found {
			t.Errorf("wire key %q no longer exists", k)
		}
	}

	// ---- GET /api/usage for user A: tokens include the helper runs judge (100),
	// cross_check (1000) and ci_fix (7) on top of the two primary runs (10+10); chat (5) is
	// excluded from every usage figure.
	selfA := call(h.SelfUsage, userA, false)
	keys("self usage", selfA, reflect.TypeOf(apitypes.SelfUsageDTO{}))
	eq("self run_count", num(selfA, "run_count"), 2)
	life, l7 := sub(selfA, "lifetime"), sub(selfA, "last_7_days")
	eq("self lifetime input", num(life, "input_tokens"), 1127)
	eq("self last7 input", num(l7, "input_tokens"), 1117)
	eq("self lifetime output", num(life, "output_tokens"), 4*1127)
	cost("self lifetime cost", num(life, "cost_usd"), 5*1127)
	outs := sub(selfA, "outcomes")
	for label, w := range map[string]struct{ fin, comp, fail float64 }{"last_7_days": {2, 1, 1}, "lifetime": {4, 3, 1}} {
		o := sub(outs, label)
		eq("self "+label+" finished", num(o, "finished"), w.fin)
		eq("self "+label+" completed", num(o, "completed"), w.comp)
		eq("self "+label+" failed", num(o, "failed"), w.fail)
		eq("self "+label+" fail_origins", sub(o, "fail_origins")["agent_failure"].(float64), w.fail)
	}

	// ---- GET /api/usage for the helper-only user B.
	selfB := call(h.SelfUsage, userB, false)
	keys("self usage (helper-only)", selfB, reflect.TypeOf(apitypes.SelfUsageDTO{}))
	eq("helper-only self run_count", num(selfB, "run_count"), 0)
	eq("helper-only self input", num(sub(selfB, "lifetime"), "input_tokens"), 500)
	eq("helper-only self failed", num(sub(sub(selfB, "outcomes"), "lifetime"), "failed"), 0)
	eq("helper-only self finished", num(sub(sub(selfB, "outcomes"), "lifetime"), "finished"), 0)

	// ---- GET /api/admin/usage.
	admin := call(h.AdminUsage, userA, true)
	if got, want := pkRawKeys(admin), pkJSONKeySet(reflect.TypeOf(apitypes.AdminUsageDTO{})); !reflect.DeepEqual(got, want) {
		t.Errorf("admin usage keys = %v, want %v", got, want)
	}
	factory := sub(admin, "factory")
	keys("admin factory", factory, reflect.TypeOf(apitypes.SelfUsageDTO{}))
	eq("factory run_count", num(factory, "run_count"), 2)
	fl, fl7 := sub(factory, "lifetime"), sub(factory, "last_7_days")
	eq("factory lifetime input (helper tokens included)", num(fl, "input_tokens"), 1627)
	eq("factory last7 input", num(fl7, "input_tokens"), 1617)
	cost("factory lifetime cost", num(fl, "cost_usd"), 5*1627)
	fo := sub(sub(factory, "outcomes"), "lifetime")
	eq("factory finished", num(fo, "finished"), 4)
	eq("factory failed (helper failures excluded)", num(fo, "failed"), 1)
	eq("factory last7 failed", num(sub(sub(factory, "outcomes"), "last_7_days"), "failed"), 1)

	rows := map[string]map[string]any{}
	list, _ := admin["users"].([]any)
	for _, r := range list {
		row := r.(map[string]any)
		keys("admin user row", row, reflect.TypeOf(apitypes.AdminUserUsageDTO{}))
		rows[row["email"].(string)] = row
	}
	a, b := rows[emailA], rows[emailB]
	if a == nil || b == nil {
		t.Fatalf("admin rows missing a seeded user: %v", rows)
	}
	eq("A run_count", num(a, "run_count"), 2)
	eq("A last7_run_count", num(a, "last7_run_count"), 1)
	eq("A harness claude", num(sub(a, "harness_runs"), "claude"), 2)
	eq("A harness codex (cross_check excluded)", num(sub(a, "harness_runs"), "codex"), 0)
	eq("A last7 harness claude", num(sub(a, "last7_harness_runs"), "claude"), 1)
	eq("A last7 harness codex", num(sub(a, "last7_harness_runs"), "codex"), 0)
	eq("A input", num(sub(a, "usage"), "input_tokens"), 1127)
	eq("A failed", num(sub(a, "outcomes"), "failed"), 1)
	eq("A finished", num(sub(a, "outcomes"), "finished"), 4)

	eq("B run_count", num(b, "run_count"), 0)
	eq("B last7_run_count", num(b, "last7_run_count"), 0)
	eq("B harness claude", num(sub(b, "harness_runs"), "claude"), 0)
	eq("B harness codex", num(sub(b, "harness_runs"), "codex"), 0)
	eq("B last7 harness claude", num(sub(b, "last7_harness_runs"), "claude"), 0)
	eq("B input (helper tokens present)", num(sub(b, "usage"), "input_tokens"), 500)
	cost("B cost", num(sub(b, "usage"), "cost_usd"), 5*500)
	eq("B failed (helper failure excluded)", num(sub(b, "outcomes"), "failed"), 0)
	eq("B finished", num(sub(b, "outcomes"), "finished"), 0)
	eq("B last7 failed", num(sub(b, "last7_outcomes"), "failed"), 0)
}
