package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/agentsource"
	"github.com/vtmocanu/uzi/api/internal/agenttmpl"
	"github.com/vtmocanu/uzi/api/internal/config"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestAgentSourceApplyLiveDB drives the M4 approve-and-apply gate end to end on real
// rows: all four upsert cases (override / add / update-synced-global / conflict-skip)
// plus de-provisioning (global delete + overridden-builtin reset), the parse-skip
// carry-forward, the display-sanitization asymmetry (DTO sanitized, DB body raw), and
// the already-applied no-op.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; ./e2e/run-store-it.sh
// provides one and sweeps this package for the LiveDB suffix.

// A bidi override (Cf, U+202E) in a body: SanitizeTTY strips it from the DTO, Apply
// keeps it in the DB. Written as an escape so this source file carries no raw bidi.
const bidiRune = '\u202E'
const bidiBody = "Do \u202Ethe thing\n"

type agentSourceFixture struct {
	h     *Handler
	rec   *agentsource.Reconciler
	pool  *pgxpool.Pool
	q     *store.Queries
	admin uuid.UUID
}

// agentSourceSharedTemplateNames is every shared-namespace (scope<>'user') template
// name this suite seeds — cleared at both setup and teardown (see newAgentSourceFixture).
var agentSourceSharedTemplateNames = []string{
	"coder", "my-admin-global", "kept-synced-global", "changed-synced-global",
	"old-synced-global", "tester", "admin-untouched", "reviewer-bot", "endpoint-role",
	"endpoint-role-2", "restage-victim",
}

// clearAgentSourceSharedState removes this suite's instance-wide rows (the singleton
// staged snapshot, the shared-namespace templates, the engine app_settings) so a test
// starts and ends clean against the shared LiveDB. It does NOT touch users (per-test).
func clearAgentSourceSharedState(bg context.Context, pool *pgxpool.Pool) {
	_, _ = pool.Exec(bg, `DELETE FROM agent_source_staged`)
	_, _ = pool.Exec(bg, `DELETE FROM agent_templates WHERE name = ANY($1)`, agentSourceSharedTemplateNames)
	// The apply de-provision sweep is instance-wide (one source per instance), so any
	// leftover origin='synced' row from a sibling LiveDB test would inflate this suite's
	// de-provision count. Under run-store-it.sh the store suite runs BEFORE this one
	// (`./internal/store/... ./internal/handler/...`, -p 1, shared Postgres), and its
	// origin suite leaves a synced global + a synced builtin. Neutralize ALL synced
	// provenance so the count reflects only what this suite seeds after the clear.
	_, _ = pool.Exec(bg, `DELETE FROM agent_templates WHERE scope = 'global' AND origin = 'synced'`)
	_, _ = pool.Exec(bg, `UPDATE agent_templates SET origin = 'embedded', customized = false WHERE scope = 'builtin' AND origin = 'synced'`)
	_, _ = pool.Exec(bg, `DELETE FROM app_settings WHERE key LIKE 'agent_source_%'`)
}

func newAgentSourceFixture(ctx context.Context, t *testing.T) agentSourceFixture {
	t.Helper()
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
	q := store.New(pool)
	cache := settings.New(q, time.Millisecond)
	rec := agentsource.NewReconciler(q, cache, config.Config{}, pool, nil)

	f := agentSourceFixture{h: &Handler{pool: pool, q: q, cfg: config.Config{}, settings: cache, agentSource: rec}, rec: rec, pool: pool, q: q, admin: uuid.New()}
	mustExecT(ctx, t, pool, `INSERT INTO users (id, email, password_hash, is_admin) VALUES ($1,$2,'x',true)`,
		f.admin, fmt.Sprintf("as-admin-%s@e2e", uuid.NewString()[:8]))
	// Hermetic START (not just teardown): uq_agent_templates_shared_name makes a name
	// unique across builtin+global, and under the shared store-it DB (run-store-it.sh
	// runs `-p 1`, so packages are serial but share one Postgres) an earlier package's
	// builtin-reconcile test can leave a "coder"/"tester" builtin row that would collide
	// with this suite's seed at setup. Clear the shared namespace before seeding so setup
	// is idempotent against a dirty shared DB, symmetric to the teardown below (none of
	// these rows hang off the admin user, so they need clearing by name/key, not a cascade).
	clearAgentSourceSharedState(ctx, pool)
	t.Cleanup(func() {
		bg := context.Background()
		clearAgentSourceSharedState(bg, pool)
		_, _ = pool.Exec(bg, `DELETE FROM users WHERE id = $1`, f.admin)
	})
	return f
}

// seedTemplate inserts one agent_templates row with an explicit provenance, returning its id.
func (f agentSourceFixture) seedTemplate(ctx context.Context, t *testing.T, name, scope, origin string, isBuiltin bool, desc, body string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	var originArg any
	if origin == "" {
		originArg = nil
	} else {
		originArg = origin
	}
	mustExecT(ctx, t, f.pool,
		`INSERT INTO agent_templates (id, name, description, model, tools, prompt_body, is_builtin, scope, origin, customized)
		 VALUES ($1,$2,$3,NULL,NULL,$4,$5,$6,$7,false)`,
		id, name, desc, body, isBuiltin, scope, originArg)
	return id
}

func (f agentSourceFixture) seedAllocation(ctx context.Context, t *testing.T, templateID uuid.UUID) {
	t.Helper()
	mustExecT(ctx, t, f.pool,
		`INSERT INTO agent_template_allocations (template_id, user_id, enabled) VALUES ($1, NULL, true)`, templateID)
}

func (f agentSourceFixture) row(ctx context.Context, t *testing.T, name string) (scope, origin, body string, isBuiltin, exists bool) {
	t.Helper()
	err := f.pool.QueryRow(ctx,
		`SELECT scope, COALESCE(origin,''), prompt_body, is_builtin FROM agent_templates WHERE name=$1`, name).
		Scan(&scope, &origin, &body, &isBuiltin)
	if err != nil {
		return "", "", "", false, false
	}
	return scope, origin, body, isBuiltin, true
}

func (f agentSourceFixture) allocationCount(ctx context.Context, t *testing.T, name string) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM agent_template_allocations a JOIN agent_templates t ON t.id=a.template_id
		 WHERE t.name=$1 AND a.user_id IS NULL`, name).Scan(&n); err != nil {
		t.Fatalf("allocation count %q: %v", name, err)
	}
	return n
}

// seedStaged writes the singleton staged snapshot with the given OK roles (plus one
// parse-skip) at sha. The diff blob is deliberately junk — Apply must recompute from
// the roles against the LIVE templates and never trust the stored diff.
func (f agentSourceFixture) seedStaged(ctx context.Context, t *testing.T, sha string, defs []agenttmpl.Definition) {
	t.Helper()
	roles := make([]agentsource.StagedRole, 0, len(defs)+1)
	for _, d := range defs {
		roles = append(roles, agentsource.StagedRole{
			Name: d.Name, OK: true, Description: d.Description, Model: d.Model, Tools: d.Tools, PromptBody: d.PromptBody,
		})
	}
	roles = append(roles, agentsource.StagedRole{Name: "bad-role", OK: false, Reason: "invalid"})
	rolesJSON, _ := json.Marshal(roles)
	if _, err := f.q.UpsertAgentSourceStaged(ctx, store.UpsertAgentSourceStagedParams{
		FetchedAt:  pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
		FetchedSha: sha,
		SourceUrl:  "https://example.com/agents.git",
		SourceRef:  "v1",
		Roles:      rolesJSON,
		Diff:       []byte(`[{"name":"junk","action":"nonsense"}]`),
	}); err != nil {
		t.Fatalf("seed staged: %v", err)
	}
}

func TestAgentSourceApplyLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newAgentSourceFixture(ctx, t)

	// Current templates BEFORE apply.
	coderID := f.seedTemplate(ctx, t, "coder", "builtin", "embedded", true, "coder desc", "old coder body\n")
	_ = f.seedTemplate(ctx, t, "my-admin-global", "global", "", false, "admin desc", "admin body\n")
	_ = f.seedTemplate(ctx, t, "kept-synced-global", "global", "synced", false, "kept desc", "kept body\n")
	_ = f.seedTemplate(ctx, t, "changed-synced-global", "global", "synced", false, "changed desc", "old changed body\n")
	oldID := f.seedTemplate(ctx, t, "old-synced-global", "global", "synced", false, "old desc", "old body\n")
	f.seedAllocation(ctx, t, oldID)
	_ = f.seedTemplate(ctx, t, "tester", "builtin", "synced", true, "tester desc", "overridden tester body\n")
	_ = f.seedTemplate(ctx, t, "admin-untouched", "global", "", false, "au desc", "admin untouched\n")

	// The fetched role set (successfully-parsed). kept-synced-global's fields MUST match
	// its row exactly so it classifies UNCHANGED (no write).
	defs := []agenttmpl.Definition{
		{Name: "coder", Description: "coder desc", PromptBody: "NEW coder body\n"},
		{Name: "reviewer-bot", Description: "reviewer bot desc", PromptBody: bidiBody},
		{Name: "my-admin-global", Description: "collide desc", PromptBody: "collide body\n"},
		{Name: "kept-synced-global", Description: "kept desc", PromptBody: "kept body\n"},
		{Name: "changed-synced-global", Description: "changed desc", PromptBody: "NEW changed body\n"},
	}
	f.seedStaged(ctx, t, "sha-v1", defs)

	// Staging alone must NOT have written agent_templates: coder still has its old body.
	if _, _, body, _, ok := f.row(ctx, t, "coder"); !ok || body != "old coder body\n" {
		t.Fatalf("staging changed templates before apply: coder body=%q ok=%v", body, ok)
	}

	// GET the DTO BEFORE apply: pending=true, and reviewer-bot's body is display-sanitized.
	dto := f.getDTO(ctx, t)
	if dto.Staged == nil || !dto.Staged.Pending {
		t.Fatalf("expected a pending staged snapshot, got %+v", dto.Staged)
	}
	revDTO := dto.stagedRole(t, "reviewer-bot")
	if strings.ContainsRune(revDTO.PromptBody, bidiRune) {
		t.Fatalf("DTO body was NOT sanitized: %q", revDTO.PromptBody)
	}

	// Apply, bound to the reviewed snapshot's SHA (mandatory expected-sha bind).
	res, err := f.rec.Apply(ctx, pgconv.UUID(f.admin), "sha-v1")
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.AlreadyApplied {
		t.Fatalf("first apply reported already-applied")
	}
	if res.Applied != 3 || res.Unchanged != 1 || res.Conflicts != 1 || res.Deprovisioned != 2 || res.SkippedParse != 1 {
		t.Fatalf("counts = applied %d, unchanged %d, conflicts %d, deprovisioned %d, skipped %d; want 3/1/1/2/1 (%s)",
			res.Applied, res.Unchanged, res.Conflicts, res.Deprovisioned, res.SkippedParse, res.Message)
	}

	// 1. override: coder is now a synced builtin, body replaced, still a builtin (reset-able).
	if scope, origin, body, isBuiltin, ok := f.row(ctx, t, "coder"); !ok || scope != "builtin" || origin != "synced" || body != "NEW coder body\n" || !isBuiltin {
		t.Fatalf("coder after override: scope=%q origin=%q body=%q builtin=%v", scope, origin, body, isBuiltin)
	}
	// still reset-able to embedded (it remained a builtin with an embedded default).
	if _, err := f.q.ResetBuiltinAgentTemplate(ctx, store.ResetBuiltinAgentTemplateParams{
		Description: "coder desc", PromptBody: "old coder body\n", UpdatedBy: pgconv.UUID(f.admin), ID: coderID,
	}); err != nil {
		t.Fatalf("overridden builtin should stay reset-able: %v", err)
	}

	// 2. add: reviewer-bot is a synced global with a default allocation, and the DB body is RAW.
	if scope, origin, body, isBuiltin, ok := f.row(ctx, t, "reviewer-bot"); !ok || scope != "global" || origin != "synced" || isBuiltin || !strings.ContainsRune(body, bidiRune) {
		t.Fatalf("reviewer-bot after add: scope=%q origin=%q builtin=%v rawBody=%q", scope, origin, isBuiltin, body)
	}
	if n := f.allocationCount(ctx, t, "reviewer-bot"); n != 1 {
		t.Fatalf("reviewer-bot allocation count = %d, want 1 (so ListClaimAgentTemplates delivers it)", n)
	}

	// 3. update-synced-global: origin stays synced, body replaced.
	if _, origin, body, _, _ := f.row(ctx, t, "changed-synced-global"); origin != "synced" || body != "NEW changed body\n" {
		t.Fatalf("changed-synced-global: origin=%q body=%q", origin, body)
	}
	// unchanged: kept-synced-global untouched.
	if _, origin, body, _, _ := f.row(ctx, t, "kept-synced-global"); origin != "synced" || body != "kept body\n" {
		t.Fatalf("kept-synced-global should be unchanged: origin=%q body=%q", origin, body)
	}

	// 4. conflict: an admin global row of the same name is never overwritten.
	if _, origin, body, _, _ := f.row(ctx, t, "my-admin-global"); origin != "" || body != "admin body\n" {
		t.Fatalf("admin global was touched: origin=%q body=%q", origin, body)
	}

	// de-provision: synced global removed from source is deleted (allocation cascades).
	if _, _, _, _, ok := f.row(ctx, t, "old-synced-global"); ok {
		t.Fatalf("old-synced-global should have been de-provisioned")
	}
	if n := f.allocationCount(ctx, t, "old-synced-global"); n != 0 {
		t.Fatalf("old-synced-global allocation not cascaded: %d", n)
	}
	// de-provision: overridden builtin removed from source is reset to embedded.
	embTester, _ := agenttmpl.BuiltinByName("tester")
	if scope, origin, body, _, ok := f.row(ctx, t, "tester"); !ok || scope != "builtin" || origin != "embedded" || body != embTester.PromptBody {
		t.Fatalf("tester reset: scope=%q origin=%q bodyMatchesEmbedded=%v", scope, origin, body == embTester.PromptBody)
	}
	// admin-authored global absent from source is NEVER de-provisioned.
	if _, origin, body, _, ok := f.row(ctx, t, "admin-untouched"); !ok || origin != "" || body != "admin untouched\n" {
		t.Fatalf("admin-untouched was de-provisioned or changed: origin=%q body=%q ok=%v", origin, body, ok)
	}

	// Applied-tracking: a second apply of the same snapshot is a no-op. The
	// expected-sha bind still matches (the snapshot did not change), so this is the
	// genuine already-applied path, not a stale-approval 409.
	res2, err := f.rec.Apply(ctx, pgconv.UUID(f.admin), "sha-v1")
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if !res2.AlreadyApplied {
		t.Fatalf("second apply should report already-applied, got %+v", res2)
	}

	// GET after apply: the snapshot is no longer pending.
	dto2 := f.getDTO(ctx, t)
	if dto2.Staged == nil || dto2.Staged.Pending {
		t.Fatalf("snapshot should be non-pending after apply: %+v", dto2.Staged)
	}
	if dto2.Status.LastAppliedSHA != "sha-v1" {
		t.Fatalf("last_applied_sha = %q, want sha-v1", dto2.Status.LastAppliedSHA)
	}
}

// TestAgentSourceEndpointsLiveDB covers the two write endpoints and their auth/guard
// wiring: "Sync now" with the feature disabled is a clean no-op, and the apply
// expected-sha guard 409s a stale approval before it mutates anything.
func TestAgentSourceEndpointsLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newAgentSourceFixture(ctx, t)

	// "Sync now" with the source disabled (fresh-install default) is a no-op that
	// still returns the config/status DTO.
	{
		r := httptest.NewRequest(http.MethodPost, "/admin/agent-source/sync", nil).WithContext(ctx)
		w := httptest.NewRecorder()
		f.h.PostAgentSourceSync(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("sync = %d (%s)", w.Code, w.Body.String())
		}
		var resp struct {
			AgentSource agentSourceDTO `json:"agent_source"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode sync dto: %v", err)
		}
		if resp.AgentSource.Config.Enabled {
			t.Fatalf("expected disabled config on a fresh instance")
		}
	}

	// Stage a snapshot with one brand-new synced role.
	f.seedStaged(ctx, t, "sha-endpoints", []agenttmpl.Definition{
		{Name: "endpoint-role", Description: "ep desc", PromptBody: "ep body\n"},
	})

	// expected_sha is now MANDATORY: an apply with no body / no expected_sha is 400
	// (the admin must approve a specific reviewed snapshot) and writes nothing.
	for _, body := range []string{"", `{}`, `{"expected_sha":""}`, `{"expected_sha":"   "}`} {
		r := f.applyReq(ctx, body)
		w := httptest.NewRecorder()
		f.h.PostAgentSourceApply(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("apply with missing expected_sha (body=%q) = %d, want 400 (%s)", body, w.Code, w.Body.String())
		}
		if _, _, _, _, ok := f.row(ctx, t, "endpoint-role"); ok {
			t.Fatalf("400 (missing expected_sha) still applied the snapshot")
		}
	}

	// A stale expected_sha is rejected 409 and writes nothing.
	{
		r := f.applyReq(ctx, `{"expected_sha":"stale-sha"}`)
		w := httptest.NewRecorder()
		f.h.PostAgentSourceApply(w, r)
		if w.Code != http.StatusConflict {
			t.Fatalf("stale apply = %d, want 409 (%s)", w.Code, w.Body.String())
		}
		if _, _, _, _, ok := f.row(ctx, t, "endpoint-role"); ok {
			t.Fatalf("409 guard still applied the snapshot")
		}
	}

	// The matching expected_sha applies.
	{
		r := f.applyReq(ctx, `{"expected_sha":"sha-endpoints"}`)
		w := httptest.NewRecorder()
		f.h.PostAgentSourceApply(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("apply = %d, want 200 (%s)", w.Code, w.Body.String())
		}
		if scope, origin, _, _, ok := f.row(ctx, t, "endpoint-role"); !ok || scope != "global" || origin != "synced" {
			t.Fatalf("endpoint-role after apply: scope=%q origin=%q ok=%v", scope, origin, ok)
		}
	}

	// Concurrent-restage guard (the PRIMARY supply-chain control): the admin reviewed
	// the OLD sha, but a restage replaced the singleton snapshot with a NEW sha carrying
	// an unreviewed role. Applying with the OLD sha must 409 (the in-tx bind sees the
	// new sha) and must NOT apply endpoint-role-2 — the admin never reviewed it.
	{
		f.seedStaged(ctx, t, "sha-endpoints-2", []agenttmpl.Definition{
			{Name: "endpoint-role-2", Description: "unreviewed", PromptBody: "unreviewed body\n"},
		})
		r := f.applyReq(ctx, `{"expected_sha":"sha-endpoints"}`)
		w := httptest.NewRecorder()
		f.h.PostAgentSourceApply(w, r)
		if w.Code != http.StatusConflict {
			t.Fatalf("apply of a restaged snapshot with the OLD sha = %d, want 409 (%s)", w.Code, w.Body.String())
		}
		if _, _, _, _, ok := f.row(ctx, t, "endpoint-role-2"); ok {
			t.Fatalf("stale-approval 409 applied the UNREVIEWED restaged snapshot")
		}
	}
}

// TestAgentSourceApplyAbortsOnBadRolesLiveDB pins FIX 2: a staged snapshot whose Roles
// blob is undecodable makes Apply return an error and write NOTHING — in particular it
// must never interpret the corrupt blob as "the source removed every role" and
// de-provision the live synced templates.
func TestAgentSourceApplyAbortsOnBadRolesLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newAgentSourceFixture(ctx, t)

	// A synced global that de-provisioning would DELETE if the bad blob decoded to empty.
	victimID := f.seedTemplate(ctx, t, "restage-victim", "global", "synced", false, "victim desc", "victim body\n")
	f.seedAllocation(ctx, t, victimID)

	// Stage a snapshot whose Roles column is well-formed jsonb (so Postgres accepts it)
	// but the WRONG SHAPE — a JSON object where decodeStagedRoles expects an array — so
	// json.Unmarshal into []StagedRole fails. This is the undecodable-blob path Apply
	// must abort on rather than treat as "the source removed every role".
	if _, err := f.q.UpsertAgentSourceStaged(ctx, store.UpsertAgentSourceStagedParams{
		FetchedAt:  pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
		FetchedSha: "sha-bad-roles",
		SourceUrl:  "https://example.com/agents.git",
		SourceRef:  "v1",
		Roles:      []byte(`{"not":"a roles array"}`),
		Diff:       []byte(`[]`),
	}); err != nil {
		t.Fatalf("seed bad-roles staged: %v", err)
	}

	res, err := f.rec.Apply(ctx, pgconv.UUID(f.admin), "sha-bad-roles")
	if err == nil {
		t.Fatalf("apply of an undecodable roles blob must error, got result %+v", res)
	}

	// Nothing was written: the synced victim (and its allocation) survives.
	if _, origin, body, _, ok := f.row(ctx, t, "restage-victim"); !ok || origin != "synced" || body != "victim body\n" {
		t.Fatalf("bad-roles apply de-provisioned/changed a live synced row: origin=%q body=%q ok=%v", origin, body, ok)
	}
	if n := f.allocationCount(ctx, t, "restage-victim"); n != 1 {
		t.Fatalf("bad-roles apply cascaded the victim's allocation: %d", n)
	}
}

// applyReq builds a POST /admin/agent-source/apply request authenticated as the admin.
func (f agentSourceFixture) applyReq(ctx context.Context, body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/admin/agent-source/apply", strings.NewReader(body))
	user := store.User{ID: f.admin, IsAdmin: true}
	return r.WithContext(mw.ContextWithUser(ctx, user))
}

// getDTO calls GET /admin/agent-source and decodes the response.
func (f agentSourceFixture) getDTO(ctx context.Context, t *testing.T) agentSourceDTO {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/admin/agent-source", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	f.h.GetAgentSource(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("GET agent-source = %d (%s)", w.Code, w.Body.String())
	}
	var resp struct {
		AgentSource agentSourceDTO `json:"agent_source"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode dto: %v (%s)", err, w.Body.String())
	}
	return resp.AgentSource
}

func (d agentSourceDTO) stagedRole(t *testing.T, name string) agentSourceRoleDTO {
	t.Helper()
	if d.Staged == nil {
		t.Fatalf("no staged snapshot in dto")
	}
	for _, r := range d.Staged.Roles {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("role %q not in staged dto", name)
	return agentSourceRoleDTO{}
}
