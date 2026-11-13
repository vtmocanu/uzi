package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Issue #4209: Overview usage run counts and run outcomes measure PRIMARY work runs
// (issue, task, prompt, job, self_improve), defined once by fn_usage_primary_kind. Token,
// cost and disclosure sums keep including helper runs. These tests seed every registered
// kind and compute every expectation in Go from the literal primary set below, never from
// the SQL function under test.

var wantPrimaryKinds = map[string]bool{"issue": true, "task": true, "prompt": true, "job": true, "self_improve": true}

type primaryKindFixture struct {
	ctx   context.Context
	tx    pgx.Tx
	t     *testing.T
	repo  uuid.UUID
	first uuid.UUID // a primary run later helpers can target
	n     int
}

func (f *primaryKindFixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.tx.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatal(err)
	}
}

type pkRun struct {
	user     uuid.UUID
	kind     string
	status   string
	origin   string
	age      string // postgres interval before now()
	harness  string // "" = claude
	factor   int64  // 0 = no run_usage row
	cost     string // cost_status for the usage row; "" = metered
	patch    bool
	mrIID    int64 // 0 = none
	autoAppr bool
}

// seed inserts one run with the schema-valid shape of its kind and an optional usage row.
func (f *primaryKindFixture) seed(r pkRun) uuid.UUID {
	f.t.Helper()
	f.n++
	id := uuid.New()
	var repo, iid, target, budget, origin, patch, mr, branch, pipelineID, pipelineRef, jobType any = f.repo, int64(420900 + f.n), nil, nil, nil, nil, nil, nil, nil, nil, nil
	harness, reportOnly := "claude", false
	if r.harness != "" {
		harness = r.harness
	}
	switch r.kind {
	case "chat", "judge":
		repo, iid = nil, nil
	case "job":
		repo, iid, jobType = nil, nil, "research"
	case "task":
		iid, branch = nil, fmt.Sprintf("uzi/task/%d", f.n)
	case "prompt", "ci_fix", "mr_rework":
		iid = nil
	}
	if r.kind == "ci_fix" {
		pipelineID, pipelineRef = int64(f.n), "ci-ref"
	}
	if r.kind == "mr_rework" {
		pipelineRef = "mr-ref"
		if r.mrIID == 0 {
			r.mrIID = int64(910000 + f.n) // required by the kind shape; unique so each owns its own PR
		}
	}
	if r.kind == "judge" || r.kind == "cross_check" || r.kind == "mr_rework" {
		target = f.first
	}
	if r.kind == "cross_check" {
		iid, harness, reportOnly, budget = nil, "codex", true, 60
	}
	if r.origin != "" {
		origin = r.origin
	}
	if r.patch {
		patch = "saved patch"
	}
	if r.mrIID != 0 {
		mr = r.mrIID
	}
	f.exec(`INSERT INTO runs (id,user_id,repo_id,issue_iid,issue_title,issue_description,kind,status,fail_origin,preserved_patch,created_at,finished_at,
	                          target_run_id,harness,report_only,budget_wall_seconds,mr_iid,auto_approve,branch,pipeline_id,pipeline_ref,job_type)
	        VALUES ($1,$2,$3,$4,'t','d',$5,$6,$7,$8,now()-$9::interval,now(),$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)`,
		id, r.user, repo, iid, r.kind, r.status, origin, patch, r.age, target, harness, reportOnly, budget, mr, r.autoAppr, branch, pipelineID, pipelineRef, jobType)
	if r.factor != 0 {
		status, cost := r.cost, int64(0)
		if status == "" {
			status = "metered"
		}
		if status == "metered" {
			cost = r.factor * 5
		}
		f.exec(`INSERT INTO run_usage (run_id,session_id,model,input_tokens,cache_read_tokens,cache_creation_tokens,output_tokens,cost_usd,cost_status)
		        VALUES ($1,'s','m',$2,$3,$4,$5,$6::numeric/1000000,$7)`, id, r.factor, 2*r.factor, 3*r.factor, 4*r.factor, cost, status)
	}
	return id
}

func newPrimaryKindFixture(t *testing.T) (*primaryKindFixture, *store.Queries, []uuid.UUID, func()) {
	t.Helper()
	e := setupHarnessEnv(t)
	tx, err := e.pool.BeginTx(e.ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cleanup := func() {
		if err := tx.Rollback(context.Background()); err != nil && err != pgx.ErrTxClosed {
			t.Error(err)
		}
	}
	f := &primaryKindFixture{ctx: e.ctx, tx: tx, t: t, repo: e.repoID}
	// Rolled back with the transaction; the harness runs serially.
	f.exec("TRUNCATE runs CASCADE")
	users := []uuid.UUID{e.userID, uuid.New(), uuid.New()}
	for _, id := range users[1:] {
		f.exec("INSERT INTO users (id,email,password_hash) VALUES ($1,$2,'x')", id, fmt.Sprintf("pk4209-%s@example.com", id))
	}
	f.first = f.seed(pkRun{user: users[0], kind: "issue", status: "completed", age: "30 days"})
	return f, store.New(tx), users, cleanup
}

func usagePrimaryRegistryKinds(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile("../../../fixtures/run-kinds/registry.json")
	if err != nil {
		t.Fatal(err)
	}
	var reg struct {
		Kinds []string `json:"kinds"`
	}
	if err := json.Unmarshal(raw, &reg); err != nil {
		t.Fatal(err)
	}
	if len(reg.Kinds) != 10 {
		t.Fatalf("registry kinds = %v", reg.Kinds)
	}
	return reg.Kinds
}

func TestUsagePrimaryKindFunctionLiveDB(t *testing.T) {
	e := setupHarnessEnv(t)
	for _, kind := range usagePrimaryRegistryKinds(t) {
		var got bool
		if err := e.pool.QueryRow(e.ctx, `SELECT fn_usage_primary_kind($1)`, kind).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != wantPrimaryKinds[kind] {
			t.Errorf("fn_usage_primary_kind(%q) = %v, want %v", kind, got, wantPrimaryKinds[kind])
		}
	}
	// A kind outside the registry is not primary either.
	var got bool
	if err := e.pool.QueryRow(e.ctx, `SELECT fn_usage_primary_kind('not_a_kind')`).Scan(&got); err != nil || got {
		t.Errorf("unknown kind: got %v err %v", got, err)
	}
	// Inlining: STRICT, SECURITY DEFINER or a SET clause would stop PostgreSQL from inlining the
	// function, leaving an opaque per-row call. The VERBOSE plan must show the expanded qual.
	// The plans are those of the real sqlc statements, read from their generated constants.
	landable := workersvc.AllHumanLandableFailOrigins()
	for _, c := range []struct {
		name string
		args []any
	}{
		{"selfUsage", []any{e.userID}},
		{"selfRunOutcomes", []any{e.userID, landable}},
		{"adminUsageTotals", nil},
		{"adminUsagePerUser", nil},
		{"adminRunOutcomes", []any{landable}},
		{"adminRunOutcomesPerUser", []any{landable}},
	} {
		name := c.name
		rows, err := e.pool.Query(e.ctx, "EXPLAIN (VERBOSE) "+sqlcConst(t, c.name), c.args...)
		if err != nil {
			t.Fatal(err)
		}
		var plan strings.Builder
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			plan.WriteString(line + "\n")
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(plan.String(), "fn_usage_primary_kind") {
			t.Errorf("%s: fn_usage_primary_kind was not inlined:\n%s", name, plan.String())
		}
		if !strings.Contains(plan.String(), "kind = ANY ('{issue,task,prompt,job,self_improve}'") {
			t.Errorf("%s: expected the inlined primary-kind qual:\n%s", name, plan.String())
		}
	}
}

// sqlcConst returns the value of an unexported string constant of the generated
// runtime.sql.go (a concatenation of string literals), so the plan check runs the real SQL.
func sqlcConst(t *testing.T, name string) string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "runtime.sql.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var eval func(ast.Expr) string
	eval = func(x ast.Expr) string {
		switch v := x.(type) {
		case *ast.BasicLit:
			s, err := strconv.Unquote(v.Value)
			if err != nil {
				t.Fatal(err)
			}
			return s
		case *ast.BinaryExpr:
			return eval(v.X) + eval(v.Y)
		case *ast.ParenExpr:
			return eval(v.X)
		}
		t.Fatalf("const %s: unsupported expression %T", name, x)
		return ""
	}
	for _, d := range file.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, s := range gd.Specs {
			vs := s.(*ast.ValueSpec)
			if len(vs.Names) == 1 && vs.Names[0].Name == name {
				return eval(vs.Values[0])
			}
		}
	}
	t.Fatalf("const %s not found in runtime.sql.go", name)
	return ""
}

func TestUsagePrimaryRunCountsLiveDB(t *testing.T) {
	f, q, users, cleanup := newPrimaryKindFixture(t)
	defer cleanup()
	ctx := f.ctx

	type acc struct {
		run, last7, claude, codex, l7claude, l7codex       int64
		in, cr, cc, out, cost, sub, unrep                  int64
		l7in, l7cr, l7cc, l7out, last7cost, l7sub, l7unrep int64
	}
	// figures lists every usage sum in the order the row figures below use.
	figures := func(a *acc) [14]int64 {
		return [14]int64{a.in, a.cr, a.cc, a.out, a.cost, a.sub, a.unrep, a.l7in, a.l7cr, a.l7cc, a.l7out, a.last7cost, a.l7sub, a.l7unrep}
	}
	want := map[int]*acc{0: {}, 1: {}, 2: {}}
	var factor int64 = 10
	add := func(u int, r pkRun) uuid.UUID {
		id := f.seed(r)
		a := want[u]
		recent := r.age == "1 day" || r.age == "2 days"
		primary := wantPrimaryKinds[r.kind]
		harness := r.harness
		if harness == "" {
			harness = "claude"
		}
		if r.kind == "cross_check" {
			harness = "codex"
		}
		if r.factor == 0 || r.kind == "chat" { // chat usage is excluded from every usage figure
			return id
		}
		if primary {
			a.run++
			if harness == "claude" {
				a.claude++
			} else {
				a.codex++
			}
			if recent {
				a.last7++
				if harness == "claude" {
					a.l7claude++
				} else {
					a.l7codex++
				}
			}
		}
		a.in += r.factor
		a.cr += 2 * r.factor
		a.cc += 3 * r.factor
		a.out += 4 * r.factor
		cost := int64(0)
		switch r.cost {
		case "", "metered":
			cost = r.factor * 5
		case "subscription":
			a.sub++
			if recent {
				a.l7sub++
			}
		case "unreported":
			a.unrep++
			if recent {
				a.l7unrep++
			}
		}
		a.cost += cost
		if recent {
			a.l7in += r.factor
			a.l7cr += 2 * r.factor
			a.l7cc += 3 * r.factor
			a.l7out += 4 * r.factor
			a.last7cost += cost
		}
		return id
	}
	// The fixture's own 30-day issue run (user 0) carries no usage.
	for _, kind := range usagePrimaryRegistryKinds(t) {
		for u, ages := range map[int][]string{0: {"1 day", "10 days"}, 1: {"2 days"}} {
			for i, age := range ages {
				factor++
				harness := "claude"
				if i == 1 {
					harness = "codex"
				}
				cost := ""
				if kind == "judge" && u == 1 {
					cost = "subscription"
				}
				if kind == "ci_fix" && u == 0 && i == 1 {
					cost = "unreported"
				}
				add(u, pkRun{user: users[u], kind: kind, status: "completed", age: age, harness: harness, factor: factor, cost: cost})
			}
		}
	}
	// A primary run without usage contributes nothing to usage counts.
	add(0, pkRun{user: users[0], kind: "issue", status: "completed", age: "1 day"})
	// Manual ci_fix, automatic ci_fix, and an automatic ci_fix whose auto_approve was later
	// cleared (the SetRunAwaitingApproval path clears it; a direct UPDATE stands in because that
	// query needs a full plan-gate fixture). All are excluded alike: the rule has no auto_approve inference.
	for _, auto := range []bool{false, true} {
		factor++
		add(0, pkRun{user: users[0], kind: "ci_fix", status: "completed", age: "1 day", factor: factor, autoAppr: auto})
	}
	factor++
	cleared := add(0, pkRun{user: users[0], kind: "ci_fix", status: "completed", age: "1 day", factor: factor, autoAppr: true})
	f.exec(`UPDATE runs SET auto_approve = false WHERE id = $1`, cleared)
	// Both shapes must exist when counts are read: exactly one still-automatic ci_fix (the
	// earlier auto=true seed) and the cleared one, each with usage.
	var stillAuto, clearedAuto, withUsage int
	if err := f.tx.QueryRow(ctx, `SELECT count(*) FROM runs r WHERE r.kind = 'ci_fix' AND r.user_id = $1 AND r.auto_approve`, users[0]).Scan(&stillAuto); err != nil {
		t.Fatal(err)
	}
	if err := f.tx.QueryRow(ctx, `SELECT count(*) FROM runs r WHERE r.id = $1 AND NOT r.auto_approve`, cleared).Scan(&clearedAuto); err != nil {
		t.Fatal(err)
	}
	if err := f.tx.QueryRow(ctx, `SELECT count(*) FROM run_usage u JOIN runs r ON r.id = u.run_id WHERE r.kind = 'ci_fix' AND r.user_id = $1 AND (r.auto_approve OR r.id = $2)`, users[0], cleared).Scan(&withUsage); err != nil {
		t.Fatal(err)
	}
	if stillAuto != 1 || clearedAuto != 1 || withUsage != 2 {
		t.Fatalf("fixture bug: automatic ci_fix %d (want 1), cleared %d (want 1), with usage %d (want 2)", stillAuto, clearedAuto, withUsage)
	}
	// A chat run with usage is excluded from everything.
	f.seed(pkRun{user: users[0], kind: "chat", status: "completed", age: "1 day", factor: 999})
	// User 2 has helper usage only.
	for _, kind := range []string{"judge", "cross_check", "mr_rework", "ci_fix"} {
		factor++
		add(2, pkRun{user: users[2], kind: kind, status: "completed", age: "1 day", factor: factor})
	}

	// Self.
	for i, u := range users {
		a := want[i]
		s, err := q.SelfUsage(ctx, u)
		if err != nil {
			t.Fatal(err)
		}
		got := [14]int64{s.LifetimeInputTokens, s.LifetimeCacheReadTokens, s.LifetimeCacheCreationTokens, s.LifetimeOutputTokens, microsOf(t, s.LifetimeCostUsd), s.LifetimeSubscriptionRunCount, s.LifetimeUnreportedRunCount,
			s.Last7InputTokens, s.Last7CacheReadTokens, s.Last7CacheCreationTokens, s.Last7OutputTokens, microsOf(t, s.Last7CostUsd), s.Last7SubscriptionRunCount, s.Last7UnreportedRunCount}
		if got != figures(a) {
			t.Errorf("SelfUsage user %d sums: got %v want %v", i, got, figures(a))
		}
		if s.RunCount != a.run {
			t.Errorf("SelfUsage user %d run_count = %d, want %d", i, s.RunCount, a.run)
		}
	}

	// Per user and factory.
	per, err := q.AdminUsagePerUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(per) != 3 {
		t.Fatalf("per-user rows = %d, want 3 (helper-only user keeps a row)", len(per))
	}
	var sum acc
	var sumFig [14]int64
	for _, r := range per {
		i := -1
		for j, u := range users {
			if u == r.UserID {
				i = j
			}
		}
		if i < 0 {
			t.Fatalf("unexpected user %s", r.UserID)
		}
		a := want[i]
		got := [6]int64{r.RunCount, r.Last7RunCount, r.ClaudeRunCount, r.CodexRunCount, r.Last7ClaudeRunCount, r.Last7CodexRunCount}
		exp := [6]int64{a.run, a.last7, a.claude, a.codex, a.l7claude, a.l7codex}
		if got != exp {
			t.Errorf("AdminUsagePerUser user %d counts: got %v want %v", i, got, exp)
		}
		fig := [14]int64{r.InputTokens, r.CacheReadTokens, r.CacheCreationTokens, r.OutputTokens, microsOf(t, r.CostUsd), r.SubscriptionRunCount, r.UnreportedRunCount,
			r.Last7InputTokens, r.Last7CacheReadTokens, r.Last7CacheCreationTokens, r.Last7OutputTokens, microsOf(t, r.Last7CostUsd), r.Last7SubscriptionRunCount, r.Last7UnreportedRunCount}
		if fig != figures(a) {
			t.Errorf("AdminUsagePerUser user %d sums: got %v want %v", i, fig, figures(a))
		}
		for k := range sumFig {
			sumFig[k] += fig[k]
		}
		if r.ClaudeRunCount+r.CodexRunCount != r.RunCount || r.Last7ClaudeRunCount+r.Last7CodexRunCount != r.Last7RunCount {
			t.Errorf("user %d: claude+codex != run_count", i)
		}
		sum.run += r.RunCount
		sum.last7 += r.Last7RunCount
	}
	if i2 := want[2]; i2.run != 0 || i2.in == 0 {
		t.Fatalf("fixture bug: helper-only user %+v", i2)
	}
	tot, err := q.AdminUsageTotals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var wantRun int64
	var wantFig [14]int64
	for _, a := range want {
		wantRun += a.run
		for k, v := range figures(a) {
			wantFig[k] += v
		}
	}
	if tot.RunCount != wantRun || tot.RunCount != sum.run {
		t.Errorf("AdminUsageTotals.RunCount = %d, want %d (per-user sum %d)", tot.RunCount, wantRun, sum.run)
	}
	totFig := [14]int64{tot.LifetimeInputTokens, tot.LifetimeCacheReadTokens, tot.LifetimeCacheCreationTokens, tot.LifetimeOutputTokens, microsOf(t, tot.LifetimeCostUsd), tot.LifetimeSubscriptionRunCount, tot.LifetimeUnreportedRunCount,
		tot.Last7InputTokens, tot.Last7CacheReadTokens, tot.Last7CacheCreationTokens, tot.Last7OutputTokens, microsOf(t, tot.Last7CostUsd), tot.Last7SubscriptionRunCount, tot.Last7UnreportedRunCount}
	if totFig != wantFig {
		t.Errorf("AdminUsageTotals sums: got %v want %v", totFig, wantFig)
	}
	if totFig != sumFig {
		t.Errorf("AdminUsageTotals sums %v != per-user sum %v", totFig, sumFig)
	}
	if sum.last7 != want[0].last7+want[1].last7+want[2].last7 {
		t.Errorf("last7 per-user sum %d", sum.last7)
	}
}

func TestUsagePrimaryRunOutcomesLiveDB(t *testing.T) {
	f, q, users, cleanup := newPrimaryKindFixture(t)
	defer cleanup()
	ctx := f.ctx

	// Primary terminal runs for users 0 and 1; user 2 has only helper failures.
	type exp struct{ fin, comp, canc, rej, div, fail, nl, l7fin, l7fail int64 }
	// The fixture's own 30-day completed issue run is a primary terminal run of user 0.
	want := map[int]*exp{0: {fin: 1, comp: 1}, 1: {}, 2: {}}
	origins := map[int]map[string]int64{0: {}, 1: {}, 2: {}}
	l7origins := map[int]map[string]int64{0: {}, 1: {}, 2: {}}
	landable := map[string]bool{}
	for _, o := range workersvc.AllHumanLandableFailOrigins() {
		landable[o] = true
	}
	// The expected last (non-rejected, non-diverged) failure per user: all finished_at values
	// equal the transaction's now(), so the newest failure is the one with the greatest id.
	type lastFail struct {
		id     uuid.UUID
		origin string
	}
	lastFails := map[int]lastFail{}
	primary := func(u int, kind, status, origin, age string, usage bool, patch bool) uuid.UUID {
		var factor int64
		if usage {
			factor = 1
		}
		id := f.seed(pkRun{user: users[u], kind: kind, status: status, origin: origin, age: age, factor: factor, patch: patch})
		recent := age == "1 day" || age == "2 days"
		e := want[u]
		e.fin++
		if recent {
			e.l7fin++
		}
		switch {
		case status == "completed":
			e.comp++
		case status == "cancelled":
			e.canc++
		case origin == "plan_rejected":
			e.rej++
		case origin == "base_diverged":
			e.div++
		default:
			e.fail++
			o := origin
			if o == "" {
				o = "unknown"
			}
			origins[u][o]++
			if lf, ok := lastFails[u]; !ok || id.String() > lf.id.String() {
				lastFails[u] = lastFail{id: id, origin: o}
			}
			if recent {
				l7origins[u][o]++
				e.l7fail++
			}
			if patch && landable[origin] {
				e.nl++
			}
		}
		return id
	}
	for _, kind := range []string{"issue", "task", "prompt", "job", "self_improve"} {
		primary(0, kind, "completed", "", "1 day", true, false)
		primary(0, kind, "failed", "agent_failure", "1 day", kind == "issue", false)
	}
	primary(0, "issue", "cancelled", "", "10 days", false, false)
	primary(0, "issue", "failed", "plan_rejected", "1 day", false, false)
	primary(0, "issue", "failed", "base_diverged", "1 day", false, false)
	primary(0, "issue", "failed", "workflow_scope_missing", "1 day", false, true)
	primary(0, "issue", "failed", "", "10 days", false, false) // failure without usage still counts
	user1Done := primary(1, "issue", "completed", "", "1 day", true, false)
	primary(1, "issue", "failed", "workflow_scope_missing", "10 days", false, true)
	// A primary completion strictly after user 1's last failure, so completed_since_last_failure is 1
	// before any helper exists; later helper completions (finished even later) must not add to it.
	f.exec(`UPDATE runs SET finished_at = now() + interval '30 minutes', status_since = now() + interval '30 minutes' WHERE id = $1`, user1Done)
	wantSince := map[int]int64{0: 0, 1: 1}

	// Snapshot the primary-only answer, then add LATER failed helpers for every helper kind
	// (manual and automatic ci_fix alike, recoverable ones carrying a preserved patch) and a
	// chat. None may move any outcome field.
	selfBefore := make([]store.SelfRunOutcomesRow, 3)
	for i, u := range users {
		r, err := q.SelfRunOutcomes(ctx, store.SelfRunOutcomesParams{UserID: u, LandableOrigins: workersvc.AllHumanLandableFailOrigins()})
		if err != nil {
			t.Fatal(err)
		}
		selfBefore[i] = r
	}
	factoryBefore, err := q.AdminRunOutcomes(ctx, workersvc.AllHumanLandableFailOrigins())
	if err != nil {
		t.Fatal(err)
	}
	perBefore, err := q.AdminRunOutcomesPerUser(ctx, workersvc.AllHumanLandableFailOrigins())
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []int{0, 1, 2} {
		for _, kind := range []string{"judge", "cross_check", "mr_rework", "ci_fix", "ci_fix", "chat"} {
			for i, st := range []string{"failed", "completed"} {
				origin := ""
				if st == "failed" {
					origin = "workflow_scope_missing"
				}
				f.seed(pkRun{user: users[u], kind: kind, status: st, origin: origin, age: "1 day", patch: st == "failed", autoAppr: i == 0 && kind == "ci_fix"})
			}
		}
	}
	f.exec(`UPDATE runs SET created_at = now() + interval '1 hour', finished_at = now() + interval '1 hour', status_since = now() + interval '1 hour' WHERE kind <> ALL($1)`,
		[]string{"issue", "task", "prompt", "job", "self_improve"})

	landableList := workersvc.AllHumanLandableFailOrigins()
	for i, u := range users {
		r, err := q.SelfRunOutcomes(ctx, store.SelfRunOutcomesParams{UserID: u, LandableOrigins: landableList})
		if err != nil {
			t.Fatal(err)
		}
		if !sameOutcomeRow(r, selfBefore[i]) {
			t.Errorf("SelfRunOutcomes user %d moved after helper failures:\nbefore %+v\nafter  %+v", i, selfBefore[i], r)
		}
		e := want[i]
		got := [9]int64{r.LifetimeFinished, r.LifetimeCompleted, r.LifetimeCancelled, r.LifetimePlanRejected, r.LifetimeBaseDiverged, r.LifetimeFailed, r.LifetimeNeedsLanding, r.Last7Finished, r.Last7Failed}
		exps := [9]int64{e.fin, e.comp, e.canc, e.rej, e.div, e.fail, e.nl, e.l7fin, e.l7fail}
		if got != exps {
			t.Errorf("SelfRunOutcomes user %d: got %v want %v", i, got, exps)
		}
		if r.LifetimeFinished != r.LifetimeCompleted+r.LifetimeCancelled+r.LifetimePlanRejected+r.LifetimeBaseDiverged+r.LifetimeFailed {
			t.Errorf("user %d: finished != segments", i)
		}
		assertOrigins(t, fmt.Sprintf("user %d lifetime", i), r.LifetimeFailOrigins, origins[i], r.LifetimeFailed)
		assertOrigins(t, fmt.Sprintf("user %d last7", i), r.Last7FailOrigins, l7origins[i], r.Last7Failed)
	}
	if want[2].fin != 0 {
		t.Fatal("fixture bug: user 2 should have no primary outcomes")
	}

	fr, err := q.AdminRunOutcomes(ctx, landableList)
	if err != nil {
		t.Fatal(err)
	}
	if fr.LifetimeFinished != factoryBefore.LifetimeFinished || fr.LifetimeFailed != factoryBefore.LifetimeFailed ||
		fr.LastFailedRunID != factoryBefore.LastFailedRunID || fr.LastFailedAt != factoryBefore.LastFailedAt ||
		fr.CompletedSinceLastFailure != factoryBefore.CompletedSinceLastFailure ||
		fr.LifetimeNeedsLanding != factoryBefore.LifetimeNeedsLanding || fr.Last7NeedsLanding != factoryBefore.Last7NeedsLanding {
		t.Errorf("AdminRunOutcomes moved after helper failures: before %+v after %+v", factoryBefore, fr)
	}
	var sumFin, sumFail, sumNL, sumL7Fin, sumL7Fail int64
	for _, e := range want {
		sumFin += e.fin
		sumFail += e.fail
		sumNL += e.nl
		sumL7Fin += e.l7fin
		sumL7Fail += e.l7fail
	}
	if fr.LifetimeFinished != sumFin || fr.LifetimeFailed != sumFail || fr.LifetimeNeedsLanding != sumNL || fr.Last7Finished != sumL7Fin || fr.Last7Failed != sumL7Fail {
		t.Errorf("AdminRunOutcomes = %d/%d/%d/%d/%d, want %d/%d/%d/%d/%d", fr.LifetimeFinished, fr.LifetimeFailed, fr.LifetimeNeedsLanding, fr.Last7Finished, fr.Last7Failed, sumFin, sumFail, sumNL, sumL7Fin, sumL7Fail)
	}
	if fr.LastFailedRunID == uuid.Nil {
		t.Error("factory last failure lost")
	}
	assertOrigins(t, "factory lifetime", fr.LifetimeFailOrigins, mergeCounts(origins[0], origins[1], origins[2]), fr.LifetimeFailed)
	assertOrigins(t, "factory last7", fr.Last7FailOrigins, mergeCounts(l7origins[0], l7origins[1], l7origins[2]), fr.Last7Failed)

	per, err := q.AdminRunOutcomesPerUser(ctx, landableList)
	if err != nil {
		t.Fatal(err)
	}
	if len(per) != 2 {
		t.Fatalf("per-user outcome rows = %d, want 2 (helper-only user has no primary outcomes)", len(per))
	}
	for _, r := range per {
		for i, u := range users {
			if u != r.UserID {
				continue
			}
			e := want[i]
			got := [9]int64{r.Finished, r.Completed, r.Cancelled, r.PlanRejected, r.LifetimeBaseDiverged, r.Failed, r.NeedsLanding, r.Last7Finished, r.Last7Failed}
			exps := [9]int64{e.fin, e.comp, e.canc, e.rej, e.div, e.fail, e.nl, e.l7fin, e.l7fail}
			if got != exps {
				t.Errorf("AdminRunOutcomesPerUser user %d: got %v want %v", i, got, exps)
			}
			// The last-failure fields come from the primary runs alone: a newer helper failure
			// and later helper completions must leave them as they were before the helpers.
			var b *store.AdminRunOutcomesPerUserRow
			for k := range perBefore {
				if perBefore[k].UserID == u {
					b = &perBefore[k]
				}
			}
			if b == nil {
				t.Fatalf("per-user %d missing from the pre-helper snapshot", i)
			}
			if r.LastFailedAt != b.LastFailedAt || r.LastFailedRunID != b.LastFailedRunID || r.LastFailedOrigin != b.LastFailedOrigin || r.CompletedSinceLastFailure != b.CompletedSinceLastFailure {
				t.Errorf("AdminRunOutcomesPerUser user %d last-failure fields moved after helpers:\nbefore %v %v %q %d\nafter  %v %v %q %d", i,
					b.LastFailedAt, b.LastFailedRunID, b.LastFailedOrigin, b.CompletedSinceLastFailure, r.LastFailedAt, r.LastFailedRunID, r.LastFailedOrigin, r.CompletedSinceLastFailure)
			}
			if lf := lastFails[i]; r.LastFailedRunID != lf.id || r.LastFailedOrigin != lf.origin || !r.LastFailedAt.Valid {
				t.Errorf("AdminRunOutcomesPerUser user %d last failure = %v/%q (valid %v), want %v/%q", i, r.LastFailedRunID, r.LastFailedOrigin, r.LastFailedAt.Valid, lf.id, lf.origin)
			}
			if r.CompletedSinceLastFailure != wantSince[i] {
				t.Errorf("AdminRunOutcomesPerUser user %d completed_since_last_failure = %d, want %d", i, r.CompletedSinceLastFailure, wantSince[i])
			}
			assertOrigins(t, fmt.Sprintf("per-user %d lifetime", i), r.FailOrigins, origins[i], r.Failed)
			assertOrigins(t, fmt.Sprintf("per-user %d last7", i), r.Last7FailOrigins, l7origins[i], r.Last7Failed)
		}
	}
}

func sameOutcomeRow(a, b store.SelfRunOutcomesRow) bool {
	return a.LastFailedAt == b.LastFailedAt && a.LastFailedRunID == b.LastFailedRunID && a.LastFailedOrigin == b.LastFailedOrigin &&
		a.CompletedSinceLastFailure == b.CompletedSinceLastFailure && a.LifetimeFinished == b.LifetimeFinished &&
		a.LifetimeFailed == b.LifetimeFailed && a.LifetimeNeedsLanding == b.LifetimeNeedsLanding &&
		a.Last7Finished == b.Last7Finished && a.Last7Failed == b.Last7Failed && a.Last7NeedsLanding == b.Last7NeedsLanding &&
		string(a.LifetimeFailOrigins) == string(b.LifetimeFailOrigins) && string(a.Last7FailOrigins) == string(b.Last7FailOrigins)
}

func mergeCounts(ms ...map[string]int64) map[string]int64 {
	out := map[string]int64{}
	for _, m := range ms {
		for k, v := range m {
			out[k] += v
		}
	}
	return out
}

func assertOrigins(t *testing.T, label string, raw []byte, want map[string]int64, failed int64) {
	t.Helper()
	var got map[string]int64
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("%s: %v", label, err)
	}
	var sum int64
	for k, v := range got {
		sum += v
		if want[k] != v {
			t.Errorf("%s: origin %s = %d, want %d", label, k, v, want[k])
		}
	}
	for k, v := range want {
		if _, seen := got[k]; !seen && v != 0 {
			t.Errorf("%s: origin %s = %d, want %d", label, k, got[k], v)
		}
	}
	if sum != failed {
		t.Errorf("%s: sum(fail_origins) = %d, failed = %d", label, sum, failed)
	}
}

// PR counts are not edited by #4209: a helper run that owns a PR still counts, and an
// mr_rework reusing a primary run's PR stays deduplicated to the earlier primary run's owner.
func TestUsagePrimaryPRCountsLiveDB(t *testing.T) {
	f, q, users, cleanup := newPrimaryKindFixture(t)
	defer cleanup()
	ctx := f.ctx
	// The fixture's 30-day issue run (user 0) has no PR. A primary run with a PR, user 0:
	f.seed(pkRun{user: users[0], kind: "issue", status: "completed", age: "5 days", mrIID: 1, factor: 3})
	// A later mr_rework by user 1 on the same PR: dedup keeps user 0 as owner.
	f.seed(pkRun{user: users[1], kind: "mr_rework", status: "completed", age: "2 days", mrIID: 1, factor: 4})
	// A ci_fix by user 1 with its own PR counts for user 1.
	f.seed(pkRun{user: users[1], kind: "ci_fix", status: "completed", age: "2 days", mrIID: 2, factor: 5})
	// An old (10 days) mr_rework with its own PR for helper-only user 2.
	f.seed(pkRun{user: users[2], kind: "mr_rework", status: "completed", age: "10 days", mrIID: 3, factor: 6})

	wantSelf := map[int][2]int64{0: {1, 1}, 1: {1, 1}, 2: {1, 0}}
	for i, u := range users {
		s, err := q.SelfPRCounts(ctx, u)
		if err != nil {
			t.Fatal(err)
		}
		if got := [2]int64{s.LifetimePrCount, s.Last7PrCount}; got != wantSelf[i] {
			t.Errorf("SelfPRCounts user %d: got %v want %v", i, got, wantSelf[i])
		}
	}
	fac, err := q.FactoryPRCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if fac.LifetimePrCount != 3 || fac.Last7PrCount != 2 {
		t.Errorf("FactoryPRCounts = %d/%d, want 3/2", fac.LifetimePrCount, fac.Last7PrCount)
	}
	per, err := q.PRCountsPerUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(per) != 3 {
		t.Fatalf("PRCountsPerUser rows = %d, want 3", len(per))
	}
	var l, l7 int64
	for _, r := range per {
		for i, u := range users {
			if u == r.UserID {
				if got := [2]int64{r.LifetimePrCount, r.Last7PrCount}; got != wantSelf[i] {
					t.Errorf("PRCountsPerUser user %d: got %v want %v", i, got, wantSelf[i])
				}
			}
		}
		l += r.LifetimePrCount
		l7 += r.Last7PrCount
	}
	if l != fac.LifetimePrCount || l7 != fac.Last7PrCount {
		t.Errorf("per-user PR sums %d/%d != factory %d/%d", l, l7, fac.LifetimePrCount, fac.Last7PrCount)
	}
}
