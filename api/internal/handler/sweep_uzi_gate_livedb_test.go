package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/schedsvc"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// The PRD #764 M3 discriminating end-to-end case, against a REAL Postgres. It drives one
// sweep tick through a REAL schedsvc.Scheduler → REAL workersvc.Service (the actual
// uzi_label gate) → live store.Queries, and proves the M3 contract: a `Planned`/`bug`
// sweep is a pure SELECTOR that fires a candidate ONLY when it also carries `uzi`.
//
//   - The `["bug","uzi"]` issue FIRES — a runs row is created for it — even though it has
//     has_prd_link=false and NO prds link. That last part is the fail-pre-change
//     discriminator: pre-PRD #764 M1 the removed PRD-link gate (Gate B) would have refused
//     a link-less swept issue with the old no-PRD-link sentinel, so no run row.
//   - The `["bug"]`-only issue (bare selector, no `uzi`) is a benign not_eligible skip and
//     produces NO run row — the real uzi_label gate refuses it with ErrNotPRDIssue.
//   - The SCHEDULE ADVANCES: next_fire_at moves to a future instant and status stays
//     'active' (not parked/errored), so a bare-selector candidate never wedges the cadence.
//
// It lives in the handler package (not schedsvc) on purpose, exactly like
// seeded_plan_livedb_test.go: ./e2e/run-store-it.sh (and CI's store-it sweep) only run the
// *LiveDB suffix under ./internal/store/... and ./internal/handler/..., so a schedsvc-package
// LiveDB test would self-skip forever and never gate in CI. The scheduler and service are
// exercised directly — no HTTP layer is on the sweep→create→advance path.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.

// sweepGateForge stubs forge.Forge (nil embed) and answers only GetIssue: the M1
// eligibility gate reads the CACHED issues.labels jsonb, never the forge, so the fake need
// only hand back a title/description/web URL for whichever candidate the sweep fetches.
type sweepGateForge struct {
	forge.Forge
}

func (sweepGateForge) GetIssue(_ context.Context, _ int64, iid int64) (forge.Issue, error) {
	return forge.Issue{
		IID:         iid,
		Title:       fmt.Sprintf("swept issue %d", iid),
		Description: "do the swept work",
		WebURL:      fmt.Sprintf("https://forge.example/i/%d", iid),
	}, nil
}

type sweepGateBuilder struct{ f forge.Forge }

func (b sweepGateBuilder) ForgeForConnection(string, string, []byte) (forge.Forge, error) {
	return b.f, nil
}

// sweepGateSettings satisfies schedsvc.SettingsReader. The sweep selector below is an
// explicit ["bug"], so resolveSweepLabels never falls back to UziLabel — this is here only
// to hand New a non-nil reader.
type sweepGateSettings struct{}

func (sweepGateSettings) UziLabel(context.Context) (string, error) { return "uzi", nil }

func TestSweepFiresOnlyUziLabelledCandidateLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
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
	q := store.New(pool)
	box := newHandlerTestBox(t)

	owner := uuid.New()
	connID, repoID := uuid.New(), uuid.New()
	projectID := int64(uuid.New().ID())
	const (
		uziIID  = int64(101) // ["bug","uzi"] → fires
		bareIID = int64(102) // ["bug"] only → not_eligible skip
	)

	t.Cleanup(func() { mustExecT(ctx, t, pool, `DELETE FROM users WHERE id = $1`, owner) })
	mustExecT(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		owner, fmt.Sprintf("m3sweep-%s@e2e", uuid.NewString()[:8]))
	mustExecT(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.example', 'uzi-bot', $3, $4)`,
		connID, owner, projectID, []byte{0x1})
	mustExecT(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, $3, 'g/m3sweep', 'https://forge.example/g/m3sweep', 'main', true)`,
		repoID, connID, projectID)

	// Two open cached issues, both carrying the `bug` SELECTOR so both are sweep
	// candidates. Only one also carries `uzi`; NEITHER has a PRD link (has_prd_link=false,
	// no prds/*.md), which is the pre-M1 fail discriminator for the uzi one.
	mustExecT(ctx, t, pool,
		`INSERT INTO issues (repo_id, forge_issue_iid, title, state, labels, web_url, has_prd_link, forge_updated_at, synced_at)
		 VALUES ($1, $2, 'bug and uzi', 'opened', '["bug","uzi"]'::jsonb, 'https://x', false, now(), now())`,
		repoID, uziIID)
	mustExecT(ctx, t, pool,
		`INSERT INTO issues (repo_id, forge_issue_iid, title, state, labels, web_url, has_prd_link, forge_updated_at, synced_at)
		 VALUES ($1, $2, 'bug only', 'opened', '["bug"]'::jsonb, 'https://x', false, now(), now())`,
		repoID, bareIID)

	// A recurring SWEEP schedule with selector labels ["bug"], auto-approve (sweeps
	// auto-approve, D6), and a PAST next_fire_at so ClaimDueSchedules picks it up this tick.
	var schedID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO run_schedules (user_id, repo_id, target, labels, timing, cron_expr, timezone, next_fire_at, auto_approve, status, enabled)
		 VALUES ($1, $2, 'sweep', '["bug"]'::jsonb, 'recurring', '0 * * * *', 'UTC', now() - interval '1 minute', true, 'active', true)
		 RETURNING id`, owner, repoID).Scan(&schedID); err != nil {
		t.Fatalf("insert sweep schedule: %v", err)
	}

	// Real workersvc.Service (the real uzi_label gate) and a real Scheduler firing through
	// it. No forges/repo-guard wired on the service: the create path needs neither, and
	// guardDefaultBranch is inert without a guard.
	wsvc := workersvc.New(q, box, workersvc.Params{})
	sched := schedsvc.New(q, wsvc, sweepGateBuilder{f: sweepGateForge{}}, sweepGateSettings{}, nil, nil, time.Minute, nil)

	tickAt := time.Now()
	sched.Boot(ctx) // one immediate tick: claim the due sweep, fan out, advance

	// Positive control: exactly one run row exists, and it is for the uzi-labelled issue —
	// so the fire genuinely happened AND fired a no-PRD-link issue (the pre-change refusal).
	var runCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM runs WHERE repo_id = $1`, repoID).Scan(&runCount); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runCount != 1 {
		t.Fatalf("runs for repo = %d, want exactly 1 (only the uzi-labelled candidate fires)", runCount)
	}
	var firedIID int64
	if err := pool.QueryRow(ctx, `SELECT issue_iid FROM runs WHERE repo_id = $1`, repoID).Scan(&firedIID); err != nil {
		t.Fatalf("read fired run issue_iid: %v", err)
	}
	if firedIID != uziIID {
		t.Fatalf("fired run issue_iid = %d, want %d (the ['bug','uzi'] issue)", firedIID, uziIID)
	}

	// The bare-selector issue produced NO run row.
	var bareRuns int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM runs WHERE repo_id = $1 AND issue_iid = $2`, repoID, bareIID).Scan(&bareRuns); err != nil {
		t.Fatalf("count bare-selector runs: %v", err)
	}
	if bareRuns != 0 {
		t.Fatalf("bare-selector ['bug'] issue produced %d runs, want 0 (refused by the uzi_label gate)", bareRuns)
	}

	// The schedule ADVANCED: next_fire_at moved to a future instant and status stayed active.
	var nextFire time.Time
	var status string
	if err := pool.QueryRow(ctx, `SELECT next_fire_at, status FROM run_schedules WHERE id = $1`, schedID).Scan(&nextFire, &status); err != nil {
		t.Fatalf("read advanced schedule: %v", err)
	}
	if status != "active" {
		t.Fatalf("schedule status = %q, want active (a benign not_eligible skip must not park it)", status)
	}
	if !nextFire.After(tickAt) {
		t.Fatalf("next_fire_at = %s, want a future instant after the tick (%s) — the schedule must advance", nextFire, tickAt)
	}

	// And the persisted last_fire records the bare candidate as exactly one not_eligible
	// skip alongside the one start, so "advanced" is not hiding a mis-bucketed fire.
	var lastFireRaw []byte
	if err := pool.QueryRow(ctx, `SELECT last_fire FROM run_schedules WHERE id = $1`, schedID).Scan(&lastFireRaw); err != nil {
		t.Fatalf("read last_fire: %v", err)
	}
	var lf struct {
		Matched int `json:"matched"`
		Started []struct {
			IssueIID *int64 `json:"issue_iid"`
		} `json:"started"`
		Skips []struct {
			IssueIID *int64 `json:"issue_iid"`
			Reason   string `json:"reason"`
		} `json:"skips"`
	}
	if err := json.Unmarshal(lastFireRaw, &lf); err != nil {
		t.Fatalf("decode last_fire: %v (raw %s)", err, lastFireRaw)
	}
	if lf.Matched != 2 || len(lf.Started) != 1 || len(lf.Skips) != 1 {
		t.Fatalf("last_fire = %+v, want matched:2 one start one skip", lf)
	}
	if lf.Started[0].IssueIID == nil || *lf.Started[0].IssueIID != uziIID {
		t.Fatalf("last_fire start iid = %v, want %d", lf.Started[0].IssueIID, uziIID)
	}
	if lf.Skips[0].Reason != string(schedsvc.SkipNotEligible) {
		t.Fatalf("last_fire skip reason = %q, want not_eligible", lf.Skips[0].Reason)
	}
	if lf.Skips[0].IssueIID == nil || *lf.Skips[0].IssueIID != bareIID {
		t.Fatalf("last_fire skip iid = %v, want %d", lf.Skips[0].IssueIID, bareIID)
	}
}
