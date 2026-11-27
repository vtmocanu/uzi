package store_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/analytics"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Issue #4730: shared fixtures for the Quality statement LiveDB tests. They build on the Spend
// fixture (users, connections, repos, runs) and add the cross-check, review and judge-skip rows.

type qualityFixture struct{ *spendFixture }

func newQualityFixture(t *testing.T) *qualityFixture {
	t.Helper()
	return &qualityFixture{newSpendFixture(t)}
}

// qualDay is the instant h hours into day k of the 30d window ending at qualNow (the
// Spend fixture's calendar: day 1 is 2026-10-27, bucket index k-1).
func qualDay(k, h int) time.Time { return spendDay(k, h) }

// qualAllNow / qualAllDay are the same calendar in a private year, for the all-scope statement.
var qualAllNow = utcAt(2012, 11, 25, 12, 0, 0)

func qualAllDay(k, h int) time.Time {
	return utcAt(2012, 10, 27, 0, 0, 0).AddDate(0, 0, k-1).Add(time.Duration(h) * time.Hour)
}

func qualTS(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

var (
	qualNull   = pgtype.Timestamptz{}
	qualInf    = pgtype.Timestamptz{InfinityModifier: pgtype.Infinity, Valid: true}
	qualNegInf = pgtype.Timestamptz{InfinityModifier: pgtype.NegativeInfinity, Valid: true}
)

// set applies one column assignment to a run.
func (f *qualityFixture) set(run uuid.UUID, assignment string, args ...any) {
	f.t.Helper()
	mustExec(f.ctx, f.t, f.pool, "UPDATE runs SET "+assignment+" WHERE id = $1", append([]any{run}, args...)...)
}

// connection adds a second forge connection for the owner and returns its id.
func (f *qualityFixture) connection(o spendOwner, forgeType, baseURL string) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	mustExec(f.ctx, f.t, f.pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, $3, $4, 'bot', 1, $5)`, id, o.User, forgeType, baseURL, []byte{0x1})
	return id
}

// repoOn adds a repo on a given connection with an explicit forge project.
func (f *qualityFixture) repoOn(conn uuid.UUID, project int64) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	mustExec(f.ctx, f.t, f.pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, $3, $4, 'https://forge.e2e/g/r', 'main', true)`,
		id, conn, project, fmt.Sprintf("g/qual-%s", id))
	return id
}

// pr seeds an issue run carrying an MR in a repo.
func (f *qualityFixture) pr(o spendOwner, repo uuid.UUID, mr int64, state string, at time.Time) uuid.UUID {
	f.t.Helper()
	return f.run(spendRun{Owner: o.User, Repo: repo, MR: mr, MRState: state, At: at})
}

// rework seeds a completed mr_rework run on (repo, mr) with the given target and trigger.
func (f *qualityFixture) rework(o spendOwner, repo uuid.UUID, mr int64, target uuid.UUID, trigger string, at time.Time) uuid.UUID {
	f.t.Helper()
	id := f.run(spendRun{Owner: o.User, Repo: repo, Kind: runkind.MRRework, MR: mr, Target: target, At: at, Finished: at.Add(time.Hour)})
	f.set(id, "trigger_source = $2", trigger)
	return id
}

// lead seeds a lead run for a cross-check attempt.
func (f *qualityFixture) lead(o spendOwner, repo uuid.UUID, harness string, at time.Time) uuid.UUID {
	f.t.Helper()
	return f.run(spendRun{Owner: o.User, Repo: repo, Harness: harness, Status: "running", At: at})
}

// ccPlan inserts one plan cross-check attempt.
func (f *qualityFixture) ccPlan(lead uuid.UUID, verdict string, reason any, created time.Time, decided, interrupted pgtype.Timestamptz, checker any) {
	f.t.Helper()
	f.ccPlanTS(lead, verdict, reason, qualTS(created), decided, interrupted, checker)
}

// ccPlanTS is ccPlan with a created_at that may be infinite; the deadline is created_at plus an hour.
func (f *qualityFixture) ccPlanTS(lead uuid.UUID, verdict string, reason any, created, decided, interrupted pgtype.Timestamptz, checker any) {
	f.t.Helper()
	mustExec(f.ctx, f.t, f.pool,
		`INSERT INTO cross_checks (lead_run_id, stage, round, lead_claim_generation, plan_md, milestones, size_class,
		     base_commit, candidate_digest, verdict, reason_class, decided_at, deadline_at, created_at, interrupted_at, checker_harness)
		 VALUES ($1, 'plan', 1, 0, 'p', '[]'::jsonb, 'S', 'base', '\x01'::bytea, $2, $3, $4, $5::timestamptz + interval '1 hour', $5, $6, $7)`,
		lead, verdict, reason, decided, created, interrupted, checker)
}

// ccCode inserts one code cross-check attempt. The legacy verdict column is 'failed' on every
// code row (cross_checks_code_shape), which is what makes reading it a bug.
func (f *qualityFixture) ccCode(lead uuid.UUID, outcome string, reason any, findings any, created time.Time, decided, interrupted pgtype.Timestamptz, checker any) {
	f.t.Helper()
	f.ccCodeTS(lead, outcome, reason, findings, qualTS(created), decided, interrupted, checker)
}

// ccCodeTS is ccCode with a created_at that may be infinite; the deadline is created_at plus an hour.
func (f *qualityFixture) ccCodeTS(lead uuid.UUID, outcome string, reason any, findings any, created, decided, interrupted pgtype.Timestamptz, checker any) {
	f.t.Helper()
	mustExec(f.ctx, f.t, f.pool,
		`INSERT INTO cross_checks (lead_run_id, stage, round, lead_claim_generation, base_commit, head_commit, candidate_digest,
		     verdict, outcome, reason_class, findings, decided_at, deadline_at, created_at, interrupted_at, checker_harness,
		     code_context, guidance_snapshot, guidance_text, guidance_digest, repo_instructions_enabled,
		     repo_instructions_text, repo_instructions_digest)
		 VALUES ($1, 'code', 1, 0, repeat('a', 40), repeat('b', 40), '\x01'::bytea,
		     'failed', $2, $3, $4::jsonb, $5, $6::timestamptz + interval '1 hour', $6, $7, $8,
		     '{}'::jsonb, '', '', sha256(convert_to('', 'UTF8')), false, '', sha256(convert_to('', 'UTF8')))`,
		lead, outcome, reason, findings, decided, created, interrupted, checker)
}

// checkerRun seeds a running cross_check child run for a lead and returns its id.
func (f *qualityFixture) checkerRun(o spendOwner, repo, lead uuid.UUID, harness string, at time.Time) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	mustExec(f.ctx, f.t, f.pool,
		`INSERT INTO runs (id, user_id, repo_id, status, kind, created_at, status_since, harness, target_run_id, report_only,
		                   budget_wall_seconds, issue_title, issue_description)
		 VALUES ($1, $2, $3, 'running', 'cross_check', $4, $4, $5, $6, true, 600, 't', 'd')`,
		id, o.User, repo, at, harness, lead)
	return id
}

// linkChecker points every cross-check attempt of a lead at its checker child run.
func (f *qualityFixture) linkChecker(lead, checker uuid.UUID) {
	f.t.Helper()
	mustExec(f.ctx, f.t, f.pool, `UPDATE cross_checks SET checker_run_id = $2 WHERE lead_run_id = $1`, lead, checker)
}

// review inserts the retained review of a target.
func (f *qualityFixture) review(target, user uuid.UUID, status, verdict string, at time.Time) {
	f.t.Helper()
	mustExec(f.ctx, f.t, f.pool,
		`INSERT INTO run_reviews (target_run_id, user_id, verdict, status, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $5)`,
		target, user, verdict, status, at)
}

func (f *qualityFixture) judgeSkip(target uuid.UUID) {
	f.t.Helper()
	mustExec(f.ctx, f.t, f.pool,
		`INSERT INTO judge_skips (target_run_id, harness, reason) VALUES ($1, 'claude', 'no_usable_credential')`, target)
}

// qualityCall is one Quality statement invocation; the zero value of every slice is the
// production default.
type qualityCall struct {
	Owner         uuid.UUID // zero = the all-scope statement
	Now           time.Time
	Zone          string
	SessionTZ     string
	Window        analytics.Window
	Repo          uuid.UUID
	Harness       string
	Kinds         []string
	DeliveryKinds []string
	JudgeKinds    []string
}

// leadKinds returns the production plan and code cross-check lead-kind lists.
func (c qualityCall) leadKinds() (plan, code []string) {
	plan, code = []string{}, []string{}
	for _, k := range runkind.All() {
		if runkind.PlanCrossCheckable(k) {
			plan = append(plan, k)
		}
		if runkind.CodeCrossCheckable(k) {
			code = append(code, k)
		}
	}
	return plan, code
}

type qualityResult struct {
	Raw    []byte
	Doc    store.AnalyticsQualityDoc
	Bounds analytics.Bounds
}

func (c qualityCall) params(b analytics.Bounds) (starts, ends []pgtype.Timestamptz, repo pgtype.UUID, harness pgtype.Text, kinds, delivery, judge, always, prestart []string) {
	starts, ends = spendTSs(b.BucketStarts), spendTSs(b.BucketEnds)
	if c.Repo != uuid.Nil {
		repo = pgtype.UUID{Bytes: c.Repo, Valid: true}
	}
	if c.Harness != "" {
		harness = pgtype.Text{String: c.Harness, Valid: true}
	}
	kinds = c.Kinds
	if kinds == nil {
		kinds = runkind.All()
	}
	delivery = c.DeliveryKinds
	if delivery == nil {
		delivery = runkind.Delivery()
	}
	judge = c.JudgeKinds
	if judge == nil {
		for _, k := range runkind.All() {
			if runkind.JudgeEligible(k) {
				judge = append(judge, k)
			}
		}
	}
	always, prestart = workersvc.JudgeSkipFailOrigins()
	return
}

// quality runs the statement for c with Go-computed bounds and decodes the document.
func (f *qualityFixture) quality(c qualityCall) qualityResult {
	f.t.Helper()
	b := f.bounds(spendCall{Owner: c.Owner, Now: c.Now, Zone: c.Zone, Window: c.Window, Repo: c.Repo, Harness: c.Harness, Kinds: c.Kinds})
	raw := f.qualityRaw(c, b)
	doc, err := store.DecodeAnalyticsQualityDoc(raw)
	if err != nil {
		f.t.Fatalf("%v\n%s", err, raw)
	}
	return qualityResult{Raw: raw, Doc: doc, Bounds: b}
}

// qualityRaw runs the statement for c against fixed bounds, in a pinned session time zone when
// c.SessionTZ is set.
func (f *qualityFixture) qualityRaw(c qualityCall, b analytics.Bounds) []byte {
	f.t.Helper()
	starts, ends, repo, harness, kinds, delivery, judge, always, prestart := c.params(b)
	planLead, codeLead := c.leadKinds()
	conn, err := f.pool.Acquire(f.ctx)
	if err != nil {
		f.t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	if c.SessionTZ != "" {
		if _, err := conn.Exec(f.ctx, fmt.Sprintf("SET TIME ZONE '%s'", c.SessionTZ)); err != nil {
			f.t.Fatalf("set time zone: %v", err)
		}
		defer func() { _, _ = conn.Exec(context.Background(), "RESET TIME ZONE") }()
	}
	qs := store.New(conn)
	var raw []byte
	if c.Owner != uuid.Nil {
		raw, err = qs.AnalyticsOwnerQuality(f.ctx, store.AnalyticsOwnerQualityParams{
			UserID: c.Owner, Now: spendTS(b.Now), From: spendTS(b.From), BucketStarts: starts, BucketEnds: ends,
			RepoID: repo, Harness: harness, Kinds: kinds, DeliveryKinds: delivery, JudgeKinds: judge,
			AlwaysSkipOrigins: always, PrestartOrigins: prestart, PlanLeadKinds: planLead, CodeLeadKinds: codeLead,
		})
	} else {
		raw, err = qs.AnalyticsAllQuality(f.ctx, store.AnalyticsAllQualityParams{
			Now: spendTS(b.Now), From: spendTS(b.From), BucketStarts: starts, BucketEnds: ends,
			RepoID: repo, Harness: harness, Kinds: kinds, DeliveryKinds: delivery, JudgeKinds: judge,
			AlwaysSkipOrigins: always, PrestartOrigins: prestart, PlanLeadKinds: planLead, CodeLeadKinds: codeLead,
		})
	}
	if err != nil {
		f.t.Fatalf("quality statement: %v", err)
	}
	return raw
}

// wantEq fails with the label unless got == want.
func wantEq[T comparable](t *testing.T, what string, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}
