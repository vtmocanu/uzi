package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/agenttmpl"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/autoselect"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// effective_settings_livedb_test.go drives the effective-settings routes against a real Postgres:
// each case seeds a user, reads the routes through the handler, then creates real runs and
// compares what creation did with what the response promised. The lead model and effort values
// the claim ships are pinned against the same predictions in workersvc
// (effective_settings_livedb_test.go there). Skipped unless UZI_TEST_DATABASE_URL points at a
// throwaway Postgres; run via ./e2e/run-store-it.sh.

type effForge struct{ forge.Forge }

func (effForge) GetIssue(_ context.Context, _ int64, iid int64) (forge.Issue, error) {
	return forge.Issue{IID: iid, Title: "effective settings", Description: "d", State: "opened"}, nil
}

func (effForge) ListIssueComments(context.Context, int64, int64) ([]forge.IssueComment, error) {
	return nil, nil
}

type effForges struct{}

func (effForges) ForgeForConnection(string, string, []byte) (forge.Forge, error) {
	return effForge{}, nil
}

// effCatalog records the Codex model each catalog default read was keyed by.
type effCatalog struct {
	mu     sync.Mutex
	models []string
	state  string
}

func (c *effCatalog) CodexCatalogDefault(_ context.Context, _ uuid.UUID, model string) (workersvc.CatalogDefault, error) {
	c.mu.Lock()
	c.models = append(c.models, model)
	c.mu.Unlock()
	if c.state != workersvc.CatalogStateKnown {
		return workersvc.CatalogDefault{State: workersvc.CatalogStateUnknown}, nil
	}
	return workersvc.CatalogDefault{State: workersvc.CatalogStateKnown, Effort: "low", Model: model, RuntimeVersion: "0.164.0",
		ObservedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}, nil
}

func (c *effCatalog) saw(model string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return effHas(c.models, model)
}

func (c *effCatalog) reset() {
	c.mu.Lock()
	c.models = nil
	c.mu.Unlock()
}

type effFix struct {
	t       *testing.T
	ctx     context.Context
	pool    *pgxpool.Pool
	h       *Handler
	user    uuid.UUID
	repo    uuid.UUID
	iid     int64
	catalog *effCatalog

	token         uuid.UUID
	tokenLabel    string
	codexAlias    uuid.UUID
	codexAccount  uuid.UUID
	codexLabel    string
	secretsToScan []string
}

func newEffFix(t *testing.T, instance settings.Store) *effFix {
	t.Helper()
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
	if instance == nil {
		instance = &settingsStore{}
	}
	wsvc := workersvc.New(q, newHandlerTestBox(t), workersvc.Params{
		CustodyHoldLimit:       defaultCustodyLimit(),
		Autoselect:             autoselect.Policy{Order: autoselect.OrderHeadroomFirst, MinHeadroom: 10, HeadroomTiePct: 5, InflightPenalty: 10, MaxStaleness: 15 * time.Minute},
		UsagePollInterval:      5 * time.Minute,
		CodexUsagePollInterval: 10 * time.Minute,
	})
	wsvc.SetTxBeginner(pool)
	wsvc.SetForges(effForges{})
	f := &effFix{t: t, ctx: ctx, pool: pool, iid: 100, catalog: &effCatalog{}}
	f.h = &Handler{pool: pool, q: q, wsvc: wsvc, settings: settings.New(instance, time.Minute), catalogDefaults: f.catalog}
	f.user = mkSecretUser(t, pool)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, f.user) })
	conn := uuid.New()
	f.repo = uuid.New()
	f.exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, conn, f.user, []byte("x"))
	f.exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		VALUES ($1, $2, 1, $3, $4, 'main', true)`, f.repo, conn, "g/"+f.repo.String(), "https://forge.e2e/g/"+f.repo.String())
	// The shared lead roster must not decide: every shared template is off for this user.
	f.exec(`INSERT INTO agent_template_allocations(template_id,user_id,enabled)
		SELECT id,$1,false FROM agent_templates
		ON CONFLICT (template_id,(COALESCE(user_id,'00000000-0000-0000-0000-000000000000'::uuid)))
		DO UPDATE SET enabled=false`, f.user)
	return f
}

func (f *effFix) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatalf("exec %q: %v", sql, err)
	}
}

// addToken inserts an Anthropic token (the first is the default) and returns its id.
func (f *effFix) addToken(id uuid.UUID, label string, disabled bool) uuid.UUID {
	f.t.Helper()
	var def bool
	if err := f.pool.QueryRow(f.ctx, `SELECT NOT EXISTS (SELECT 1 FROM user_secrets WHERE user_id=$1 AND kind='anthropic_token')`, f.user).Scan(&def); err != nil {
		f.t.Fatal(err)
	}
	ciphertext := []byte("ciphertext-" + label)
	f.secretsToScan = append(f.secretsToScan, string(ciphertext))
	f.exec(`DELETE FROM user_secrets WHERE id = $1`, id)
	f.exec(`INSERT INTO user_secrets (id, user_id, kind, label, is_default, ciphertext, sealed_with, disabled_at)
		VALUES ($1, $2, 'anthropic_token', $3, $4, $5, 'master', CASE WHEN $6 THEN now() END)`, id, f.user, label, def && !disabled, ciphertext, disabled)
	f.t.Cleanup(func() { _, _ = f.pool.Exec(context.Background(), `DELETE FROM user_secrets WHERE id = $1`, id) })
	f.secretsToScan = append(f.secretsToScan, id.String(), label)
	return id
}

func (f *effFix) anthropic() {
	f.t.Helper()
	f.tokenLabel = "eff-claude-" + uuid.NewString()[:8]
	f.token = f.addToken(uuid.New(), f.tokenLabel, false)
}

// codexDefault gives the user a linked default Codex subscription.
func (f *effFix) codexDefault() {
	f.t.Helper()
	q := f.h.q
	f.codexLabel = "eff-codex-" + uuid.NewString()[:8]
	secret, err := q.InsertCodexSecret(f.ctx, store.InsertCodexSecretParams{
		UserID: f.user, Kind: store.KindCodexAuth, Label: f.codexLabel, WantDefault: true, Ciphertext: []byte("codex-ciphertext-fixture"), SealedWith: store.SealedWithMaster,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	account, err := q.InsertCodexProviderAccount(f.ctx, store.InsertCodexProviderAccountParams{
		UserID: f.user, ProviderUserID: "principal-" + uuid.NewString(), WorkspaceAccountID: "workspace-" + uuid.NewString(),
		SealedLogin: []byte("fixture"), SealedWith: store.SealedWithMaster,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err = q.InsertCodexCredentialState(f.ctx, store.InsertCodexCredentialStateParams{UserSecretID: secret.ID, UserID: f.user, Status: "staging"}); err != nil {
		f.t.Fatal(err)
	}
	if _, err = q.LinkCodexCredentialState(f.ctx, store.LinkCodexCredentialStateParams{ProviderAccountID: pgconv.UUID(account.ID), UserSecretID: secret.ID, UserID: f.user, MaterialRevision: 0}); err != nil {
		f.t.Fatal(err)
	}
	f.codexAlias, f.codexAccount = secret.ID, account.ID
	f.secretsToScan = append(f.secretsToScan, secret.ID.String(), f.codexLabel, "codex-ciphertext-fixture")
}

func (f *effFix) claudeReading(usedPct int) {
	f.t.Helper()
	f.exec(`INSERT INTO anthropic_rate_limits (user_secret_id,user_id,five_hour_pct,seven_day_pct,
            seven_day_resets_at,source,synced_at,enablement_rev)
        SELECT id,user_id,$2,$2,now()+interval '2 days','usage_endpoint',now()-interval '1 minute',enablement_rev
        FROM user_secrets WHERE id=$1
        ON CONFLICT (user_secret_id) DO UPDATE SET five_hour_pct=EXCLUDED.five_hour_pct,
            seven_day_pct=EXCLUDED.seven_day_pct,seven_day_resets_at=EXCLUDED.seven_day_resets_at,
            synced_at=EXCLUDED.synced_at,enablement_rev=EXCLUDED.enablement_rev`, f.token, usedPct)
}

func (f *effFix) codexReading(util float64) {
	f.t.Helper()
	reset := time.Now().Add(28 * time.Hour).Unix()
	window := func(secs int64) string {
		return fmt.Sprintf(`{"used_percent":%v,"limit_window_seconds":%d,"reset_after_seconds":null,"reset_at":%d}`, util, secs, reset)
	}
	buckets := fmt.Sprintf(`[{"id":"codex","allowed":true,"limit_reached":false,"primary":%s,"secondary":%s}]`, window(18000), window(604800))
	f.exec(`INSERT INTO codex_account_rate_limits (user_id, provider_account_id, buckets, last_success_at, last_attempt_at, attempt_status)
		VALUES ($1, $2, $3::jsonb, now() - interval '1 minute', now() - interval '1 minute', 'ok')
		ON CONFLICT (user_id, provider_account_id) DO UPDATE SET buckets = EXCLUDED.buckets,
			last_success_at = EXCLUDED.last_success_at, last_attempt_at = EXCLUDED.last_attempt_at`, f.user, f.codexAccount, buckets)
}

func (f *effFix) bothUsable(claudeUsedPct int, codexUtil float64) {
	f.anthropic()
	f.codexDefault()
	f.claudeReading(claudeUsedPct)
	f.codexReading(codexUtil)
}

func (f *effFix) policy(defaultHarness string, overflow bool) {
	f.t.Helper()
	f.exec(`UPDATE users SET default_harness = NULLIF($2,''), harness_overflow_enabled = $3 WHERE id = $1`, f.user, defaultHarness, overflow)
}

func (f *effFix) set(column, value string) {
	f.t.Helper()
	f.exec(`UPDATE users SET `+column+` = NULLIF($2,'') WHERE id = $1`, f.user, value)
}

func (f *effFix) leadTemplate(claudePin, codexPin string) {
	f.t.Helper()
	id := uuid.New()
	f.exec(`INSERT INTO agent_templates(id,name,description,prompt_body,scope,user_id,model,codex_model)
		VALUES($1,'lead','d','b','user',$2,NULLIF($3,''),NULLIF($4,''))`, id, f.user, claudePin, codexPin)
	f.exec(`INSERT INTO agent_template_allocations(template_id,user_id,enabled) VALUES($1,$2,true)`, id, f.user)
}

func (f *effFix) get() (apitypes.EffectiveSettingsDTO, []byte) {
	f.t.Helper()
	rec := httptest.NewRecorder()
	f.h.GetEffectiveSettings(rec, userReq(http.MethodGet, "/api/me/effective-settings", "", f.user, nil))
	if rec.Code != http.StatusOK {
		f.t.Fatalf("GET effective-settings = %d %s", rec.Code, rec.Body.String())
	}
	var dto apitypes.EffectiveSettingsDTO
	strictDecode(f.t, rec.Body.Bytes(), &dto)
	return dto, rec.Body.Bytes()
}

func (f *effFix) preview(body string) (apitypes.StartPreviewDTO, []byte) {
	f.t.Helper()
	rec := httptest.NewRecorder()
	f.h.PreviewStartSettings(rec, userReq(http.MethodPost, "/api/me/effective-settings/start-preview", body, f.user, nil))
	if rec.Code != http.StatusOK {
		f.t.Fatalf("start-preview %s = %d %s", body, rec.Code, rec.Body.String())
	}
	var dto apitypes.StartPreviewDTO
	strictDecode(f.t, rec.Body.Bytes(), &dto)
	return dto, rec.Body.Bytes()
}

func strictDecode(t *testing.T, raw []byte, dst any) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		t.Fatalf("response does not decode into %T: %v\n%s", dst, err, raw)
	}
}

func (f *effFix) create(explicit *workersvc.Harness, override *workersvc.RawCredentialOverride) (store.Run, error) {
	f.t.Helper()
	f.iid++
	f.exec(`INSERT INTO issues (id, repo_id, forge_issue_iid, title, state, labels, web_url, has_prd_link, forge_updated_at, synced_at)
		VALUES ($1, $2, $3, 'eff issue', 'opened', '["uzi"]', 'https://forge.e2e/i', false, now(), now())`, uuid.New(), f.repo, f.iid)
	return f.h.wsvc.CreateRun(f.ctx, f.user, f.repo, f.iid, "eff", nil, nil, false, nil, nil, explicit, override)
}

func hptr(h workersvc.Harness) *workersvc.Harness { return &h }

func (d *effFix) choice(dto apitypes.EffectiveSettingsDTO, name string) apitypes.EffectiveStartChoiceDTO {
	d.t.Helper()
	for _, s := range dto.Start {
		if s.Choice == name {
			return s
		}
	}
	d.t.Fatalf("no start choice %q", name)
	return apitypes.EffectiveStartChoiceDTO{}
}

func modelCell(t *testing.T, cells []apitypes.EffectiveModelCellDTO, setting, harness string) apitypes.EffectiveModelCellDTO {
	t.Helper()
	for _, c := range cells {
		if c.Setting == setting && c.Harness == harness {
			return c
		}
	}
	t.Fatalf("no model cell %s/%s", setting, harness)
	return apitypes.EffectiveModelCellDTO{}
}

func effortCell(t *testing.T, cells []apitypes.EffectiveEffortCellDTO, setting, harness string) apitypes.EffectiveEffortCellDTO {
	t.Helper()
	for _, c := range cells {
		if c.Setting == setting && c.Harness == harness {
			return c
		}
	}
	t.Fatalf("no effort cell %s/%s", setting, harness)
	return apitypes.EffectiveEffortCellDTO{}
}

func sp(s string) *string { return &s }

func sameCell(a, b apitypes.ModelSettingCellDTO) bool {
	return a.Harness == b.Harness && eqStr(a.Model, b.Model) && a.DefaultModel == b.DefaultModel &&
		a.DefaultSource == b.DefaultSource && a.ResolvedModel == b.ResolvedModel && a.Source == b.Source
}

func eqStr(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func strs(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

func wantHarness(t *testing.T, v *apitypes.HarnessValueDTO, value, source string, notes ...string) {
	t.Helper()
	if v == nil {
		t.Fatalf("harness is null, want %s/%s", value, source)
	}
	if strs(v.Value) != strs(sp(value)) && !(value == "" && v.Value == nil) || v.Source != source || v.FixedAt != "create" || strings.Join(v.Notes, ",") != strings.Join(notes, ",") {
		t.Fatalf("harness = {value:%s source:%s fixed_at:%s notes:%v}, want {%q %s create %v}", strs(v.Value), v.Source, v.FixedAt, v.Notes, value, source, notes)
	}
}

func TestEffectiveSettingsClaudeOnlyLiveDB(t *testing.T) {
	f := newEffFix(t, nil)
	f.anthropic()
	f.leadTemplate("opus", "")
	dto, _ := f.get()

	if dto.Version != 1 || len(dto.Harnesses) != 2 || len(dto.Start) != 3 || len(dto.Models) != 8 || len(dto.Efforts) != 4 ||
		len(dto.CrossCheck.Models) != 4 || len(dto.CrossCheck.Efforts) != 4 {
		t.Fatalf("shape: version=%d harnesses=%d start=%d models=%d efforts=%d cross-check=%d/%d", dto.Version, len(dto.Harnesses), len(dto.Start),
			len(dto.Models), len(dto.Efforts), len(dto.CrossCheck.Models), len(dto.CrossCheck.Efforts))
	}
	def := f.choice(dto, "default")
	wantHarness(t, def.Harness, "claude", "automatic")
	wantHarness(t, f.choice(dto, "claude").Harness, "claude", "run_override")
	codex := f.choice(dto, "codex")
	if codex.Harness != nil || codex.Refusal == nil || *codex.Refusal != "no_credential_for_harness" || codex.RefusalDetail != nil {
		t.Fatalf("codex choice = %+v, want the plain no_credential_for_harness refusal", codex)
	}
	if !def.AnthropicOverrideAllowed || codex.AnthropicOverrideAllowed {
		t.Errorf("anthropic_override_allowed: default=%v codex=%v", def.AnthropicOverrideAllowed, codex.AnthropicOverrideAllowed)
	}
	if dto.Harnesses[0].Harness != "claude" || !dto.Harnesses[0].Usable || dto.Harnesses[0].Credential == nil ||
		dto.Harnesses[0].Credential.Source != "claim_selection" || dto.Harnesses[0].Credential.FixedAt != "claim" || dto.Harnesses[0].Credential.AuthMode != nil {
		t.Errorf("claude harness = %+v", dto.Harnesses[0])
	}
	if dto.Harnesses[1].Usable || dto.Harnesses[1].Refusal == nil || dto.Harnesses[1].Credential != nil {
		t.Errorf("codex harness = %+v", dto.Harnesses[1])
	}

	m := modelCell(t, dto.Models, "worker_model", "claude")
	if m.Model != nil || m.DefaultModel != "opus" || m.DefaultSource != "template" || m.ResolvedModel != "opus" || m.Source != "template" || m.FixedAt != "claim" || len(m.Notes) != 0 || m.Ignored != nil {
		t.Errorf("claude worker model = %+v", m)
	}
	e := effortCell(t, dto.Efforts, "worker_effort", "claude")
	if e.Effort != nil || e.ResolvedEffort == nil || *e.ResolvedEffort != "medium" || e.Source != "builtin" || e.Mode != "explicit" || e.CatalogDefault != nil || e.FixedAt != "claim" {
		t.Errorf("claude worker effort = %+v", e)
	}
	if run, err := f.create(nil, nil); err != nil || run.Harness != "claude" {
		t.Fatalf("create the default start: %+v %v", run.Harness, err)
	}
	if _, err := f.create(hptr(workersvc.HarnessCodex), nil); !errors.Is(err, workersvc.ErrNoCredentialForHarness) {
		t.Fatalf("create explicit codex = %v, want the refusal the choice reports", err)
	}
}

func TestEffectiveSettingsRuntimeDefaultEffortLiveDB(t *testing.T) {
	f := newEffFix(t, nil)
	f.anthropic()
	f.codexDefault()
	f.set("default_codex_effort", "runtime")
	f.set("default_codex_model", "gpt-6-sol")
	f.set("default_effort", "runtime")
	f.catalog.state = workersvc.CatalogStateKnown
	dto, _ := f.get()

	codex := effortCell(t, dto.Efforts, "worker_effort", "codex")
	if codex.Effort == nil || *codex.Effort != "runtime" || codex.ResolvedEffort != nil || codex.Source != "user_default" || codex.Mode != "runtime_default" {
		t.Fatalf("codex worker effort = %+v", codex)
	}
	cd := codex.CatalogDefault
	if cd == nil || cd.State != "known" || strs(cd.Effort) != "low" || strs(cd.Model) != "gpt-6-sol" || strs(cd.RuntimeVersion) != "0.164.0" || cd.ObservedAt == nil {
		t.Fatalf("catalog default = %+v", cd)
	}
	claude := effortCell(t, dto.Efforts, "worker_effort", "claude")
	if claude.Mode != "runtime_default" || claude.ResolvedEffort != nil || claude.CatalogDefault != nil {
		t.Fatalf("the Claude cell never carries a catalog default: %+v", claude)
	}

	// The catalog default is keyed by the Codex model the run would use, not the raw lane.
	f.set("default_codex_model", "")
	f.leadTemplate("", "gpt-6-astra")
	f.catalog.state = workersvc.CatalogStateUnknown
	f.catalog.reset()
	dto, _ = f.get()
	if !f.catalog.saw("gpt-6-astra") {
		t.Errorf("catalog default reads keyed by %v, want the lead template's Codex pin gpt-6-astra for the worker cell", f.catalog.models)
	}
	if cd := effortCell(t, dto.Efforts, "worker_effort", "codex").CatalogDefault; cd == nil || cd.State != "unknown" || cd.Effort != nil {
		t.Errorf("unknown catalog default = %+v", cd)
	}
}

func TestEffectiveSettingsTaskReviewCellsLiveDB(t *testing.T) {
	f := newEffFix(t, nil)
	f.anthropic()
	f.codexDefault()
	f.set("default_codex_model", "custom-codex-preview") // whatever the lane holds, the review built-in is fixed
	f.set("default_claude_model", "sonnet")
	dto, _ := f.get()

	codex := modelCell(t, dto.Models, "task_review_model", "codex")
	if codex.ResolvedModel != agenttmpl.CodexTaskReviewModel || codex.Source != "builtin" || codex.Model != nil || strings.Join(codex.Notes, ",") != "not_configurable" || codex.FixedAt != "claim" {
		t.Errorf("codex task review model = %+v", codex)
	}
	claude := modelCell(t, dto.Models, "task_review_model", "claude")
	if claude.ResolvedModel != "" || claude.Source != "builtin" || strings.Join(claude.Notes, ",") != "sdk_default" {
		t.Errorf("claude task review model = %+v (the Claude lane is ignored by the reviewer)", claude)
	}
	for _, h := range []string{"claude", "codex"} {
		e := effortCell(t, dto.Efforts, "task_review_effort", h)
		if e.ResolvedEffort != nil || e.Mode != "runtime_default" || e.Source != "builtin" || strings.Join(e.Notes, ",") != "not_sent" || e.Effort != nil {
			t.Errorf("%s task review effort = %+v", h, e)
		}
	}
}

func TestEffectiveSettingsHarnessChoicesLiveDB(t *testing.T) {
	t.Run("default automatic: both usable, no stored default", func(t *testing.T) {
		f := newEffFix(t, nil)
		f.bothUsable(10, 10)
		dto, _ := f.get()
		def := f.choice(dto, "default")
		wantHarness(t, def.Harness, "", "chosen_at_create", "automatic_weekly_reset")
		if def.Refusal != nil || !def.AnthropicOverrideAllowed {
			t.Errorf("default = %+v", def)
		}
		wantHarness(t, f.choice(dto, "claude").Harness, "claude", "run_override")
		wantHarness(t, f.choice(dto, "codex").Harness, "codex", "run_override")
		cred := dto.Harnesses[1].Credential
		if cred == nil || cred.Kind != "codex" || cred.Source != "codex_default_credential" || cred.FixedAt != "create" || cred.AuthMode == nil || *cred.AuthMode != "subscription" {
			t.Errorf("codex credential = %+v", cred)
		}
		run, err := f.create(nil, nil)
		if err != nil || (run.Harness != "claude" && run.Harness != "codex") {
			t.Fatalf("create = %q %v", run.Harness, err)
		}
	})
	t.Run("stored usable default with overflow off is exact", func(t *testing.T) {
		f := newEffFix(t, nil)
		f.bothUsable(10, 10)
		f.policy("codex", false)
		dto, _ := f.get()
		wantHarness(t, f.choice(dto, "default").Harness, "codex", "user_default")
		if run, err := f.create(nil, nil); err != nil || run.Harness != "codex" {
			t.Fatalf("create = %q %v", run.Harness, err)
		}
	})
	t.Run("stored default with overflow on is chosen at create", func(t *testing.T) {
		f := newEffFix(t, nil)
		f.bothUsable(10, 10)
		f.policy("codex", true)
		dto, _ := f.get()
		wantHarness(t, f.choice(dto, "default").Harness, "", "chosen_at_create", "overflow")
		// Codex is preferred, so an Anthropic override is not accepted: creation stays on exact Codex.
		if f.choice(dto, "default").AnthropicOverrideAllowed {
			t.Errorf("anthropic_override_allowed = true for an overflow with codex preferred, want false")
		}
		pv, body := f.preview(`{"harness":null,"credential_override":{"mode":"auto"}}`)
		if strs(pv.Refusal) != "credential_override_harness_unsupported" {
			t.Fatalf("auto override preview = %s, want credential_override_harness_unsupported", body)
		}
		if _, err := f.create(nil, &workersvc.RawCredentialOverride{Mode: "auto"}); !errors.Is(err, workersvc.ErrCredentialOverrideHarnessUnsupported) {
			t.Fatalf("create with an auto override = %v, want ErrCredentialOverrideHarnessUnsupported", err)
		}
	})
	t.Run("default codex unusable falls back with the note", func(t *testing.T) {
		f := newEffFix(t, nil)
		f.anthropic()
		f.policy("codex", false)
		dto, _ := f.get()
		wantHarness(t, f.choice(dto, "default").Harness, "claude", "automatic", "user_default_harness_unusable")
		if run, err := f.create(nil, nil); err != nil || run.Harness != "claude" {
			t.Fatalf("create = %q %v", run.Harness, err)
		}
	})
	t.Run("explicit codex with every credential disabled", func(t *testing.T) {
		f := newEffFix(t, nil)
		f.anthropic()
		f.codexDefault()
		f.exec(`UPDATE user_secrets SET disabled_at = now(), is_default = false WHERE user_id = $1 AND kind = 'codex_auth'`, f.user)
		dto, _ := f.get()
		c := f.choice(dto, "codex")
		if c.Harness != nil || strs(c.Refusal) != "no_credential_for_harness" || strs(c.RefusalDetail) != "credential_disabled" {
			t.Fatalf("codex choice = %+v", c)
		}
		if _, err := f.create(hptr(workersvc.HarnessCodex), nil); !errors.Is(err, workersvc.ErrHarnessCredentialDisabled) {
			t.Fatalf("create = %v, want the same all-disabled refusal", err)
		}
		wantHarness(t, f.choice(dto, "default").Harness, "claude", "automatic")
	})
	t.Run("neither usable", func(t *testing.T) {
		f := newEffFix(t, nil)
		dto, _ := f.get()
		def := f.choice(dto, "default")
		if def.Harness != nil || strs(def.Refusal) != "no_usable_credential" {
			t.Fatalf("default = %+v", def)
		}
		for _, name := range []string{"claude", "codex"} {
			if c := f.choice(dto, name); c.Harness != nil || strs(c.Refusal) != "no_credential_for_harness" {
				t.Errorf("%s = %+v", name, c)
			}
		}
		if _, err := f.create(nil, nil); !errors.Is(err, workersvc.ErrNoUsableCredential) {
			t.Fatalf("create = %v", err)
		}
	})
	t.Run("codex account mode auto picks the account at create", func(t *testing.T) {
		f := newEffFix(t, nil)
		f.codexDefault()
		f.exec(`UPDATE user_secrets SET auto_eligible = true WHERE id = $1`, f.codexAlias)
		f.exec(`UPDATE users SET codex_account_mode = 'auto' WHERE id = $1`, f.user)
		f.codexReading(10)
		dto, _ := f.get()
		cred := dto.Harnesses[1].Credential
		if cred == nil || cred.Source != "chosen_at_create" || cred.FixedAt != "create" || cred.Kind != "codex" {
			t.Fatalf("codex credential = %+v", cred)
		}
	})
	t.Run("codex pool unavailable reads as the detail", func(t *testing.T) {
		f := newEffFix(t, nil)
		f.anthropic()
		f.exec(`UPDATE users SET codex_account_mode = 'auto' WHERE id = $1`, f.user)
		f.exec(`INSERT INTO user_secrets (user_id, kind, label, is_default, ciphertext, sealed_with, auto_eligible)
			VALUES ($1, 'codex_auth', 'pooled-unlinked', false, $2, 'master', true)`, f.user, []byte{3})
		dto, _ := f.get()
		c := f.choice(dto, "codex")
		if c.Harness != nil || strs(c.Refusal) != "no_credential_for_harness" || strs(c.RefusalDetail) != "codex_pool_unavailable" {
			t.Fatalf("codex choice = %+v", c)
		}
		if _, err := f.create(hptr(workersvc.HarnessCodex), nil); !errors.Is(err, workersvc.ErrCodexPoolUnavailable) {
			t.Fatalf("create = %v", err)
		}
	})
	// Deviation 7: creation excludes overflow for a Claude-preferred run whose Codex default is
	// structurally unavailable, so the GET names the exact harness.
	t.Run("overflow with an unavailable codex default is exact", func(t *testing.T) {
		f := newEffFix(t, nil)
		f.bothUsable(90, 10)
		f.policy("claude", true)
		f.exec(`UPDATE codex_provider_account SET coord_state='quarantined' WHERE id = $1`, f.codexAccount)
		dto, _ := f.get()
		wantHarness(t, f.choice(dto, "default").Harness, "claude", "user_default")
		run, err := f.create(nil, nil)
		if err != nil || run.Harness != "claude" || run.HarnessDecisionReason.String != "excluded" {
			t.Fatalf("create = %q reason=%v %v", run.Harness, run.HarnessDecisionReason, err)
		}
		// And with the account healthy again it is chosen at create.
		f.exec(`UPDATE codex_provider_account SET coord_state='idle' WHERE id = $1`, f.codexAccount)
		dto, _ = f.get()
		wantHarness(t, f.choice(dto, "default").Harness, "", "chosen_at_create", "overflow")
	})
}

// ---- start-preview ----

func TestStartPreviewTokenChoicesLiveDB(t *testing.T) {
	f := newEffFix(t, nil)
	f.bothUsable(10, 10)
	disabled := f.addToken(uuid.New(), "eff-disabled-"+uuid.NewString()[:8], true)
	enabled := f.addToken(uuid.New(), "eff-enabled-"+uuid.NewString()[:8], false)
	other := newEffFix(t, nil)
	other.anthropic()
	deleted := uuid.New()

	pinned := func(id uuid.UUID) string {
		return fmt.Sprintf(`{"harness":null,"credential_override":{"mode":"pinned","secret_id":%q}}`, id)
	}
	cases := []struct {
		name       string
		body       string
		explicit   *workersvc.Harness
		raw        *workersvc.RawCredentialOverride
		wantRef    string
		wantErr    error
		wantSource string
		wantFixed  string
	}{
		{"disabled pinned token", pinned(disabled), nil, &workersvc.RawCredentialOverride{Mode: "pinned", SecretID: &disabled}, "credential_disabled", workersvc.ErrCredentialDisabled, "", ""},
		{"deleted pinned token", pinned(deleted), nil, &workersvc.RawCredentialOverride{Mode: "pinned", SecretID: &deleted}, "credential_not_found", workersvc.ErrCredentialOverrideSecretNotFound, "", ""},
		{"foreign pinned token", pinned(other.token), nil, &workersvc.RawCredentialOverride{Mode: "pinned", SecretID: &other.token}, "credential_not_found", workersvc.ErrCredentialOverrideSecretNotFound, "", ""},
		{"pinned without a token", `{"harness":null,"credential_override":{"mode":"pinned"}}`, nil, &workersvc.RawCredentialOverride{Mode: "pinned"}, "credential_override_invalid", workersvc.ErrCredentialOverridePinnedNeedsSecret, "", ""},
		{"enabled pinned token", pinned(enabled), nil, &workersvc.RawCredentialOverride{Mode: "pinned", SecretID: &enabled}, "", nil, "token_override", "create"},
		{"auto", `{"harness":null,"credential_override":{"mode":"auto"}}`, nil, &workersvc.RawCredentialOverride{Mode: "auto"}, "", nil, "auto_selection", "claim"},
		{"default", `{"harness":"claude","credential_override":{"mode":"default"}}`, hptr(workersvc.HarnessClaude), &workersvc.RawCredentialOverride{Mode: "default"}, "", nil, "owner_default", "claim"},
		{"explicit codex refuses any override", `{"harness":"codex","credential_override":{"mode":"auto"}}`, hptr(workersvc.HarnessCodex), &workersvc.RawCredentialOverride{Mode: "auto"}, "credential_override_harness_unsupported", workersvc.ErrCredentialOverrideHarnessUnsupported, "", ""},
		{"explicit codex refuses even inherit", `{"harness":"codex","credential_override":{"mode":"inherit"}}`, hptr(workersvc.HarnessCodex), &workersvc.RawCredentialOverride{Mode: "inherit"}, "credential_override_harness_unsupported", workersvc.ErrCredentialOverrideHarnessUnsupported, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dto, raw := f.preview(c.body)
			_, createErr := f.create(c.explicit, c.raw)
			if c.wantRef != "" {
				if strs(dto.Refusal) != c.wantRef || dto.Credential != nil {
					t.Fatalf("preview = %s", raw)
				}
				if !errors.Is(createErr, c.wantErr) {
					t.Fatalf("create = %v, want %v (the refusal the preview reports)", createErr, c.wantErr)
				}
				return
			}
			if createErr != nil {
				t.Fatalf("create = %v, but the preview accepted the choice: %s", createErr, raw)
			}
			if dto.Refusal != nil || dto.Credential == nil || dto.Credential.Source != c.wantSource || dto.Credential.FixedAt != c.wantFixed || dto.Credential.Kind != "anthropic" || dto.Harness == nil {
				t.Fatalf("preview = %s", raw)
			}
			if dto.Harness.Value == nil || *dto.Harness.Value != "claude" || len(dto.Alternatives) != 0 {
				t.Fatalf("a non-inherit override makes the policies ineligible, so the harness is exact: %s", raw)
			}
		})
	}
}

func TestStartPreviewExplicitHarnessCredentialsLiveDB(t *testing.T) {
	f := newEffFix(t, nil)
	f.bothUsable(10, 10)
	dto, _ := f.preview(`{"harness":"codex","credential_override":null}`)
	wantHarness(t, dto.Harness, "codex", "run_override")
	if dto.Credential == nil || dto.Credential.Source != "codex_default_credential" || dto.Credential.AuthMode == nil || *dto.Credential.AuthMode != "subscription" || len(dto.Alternatives) != 0 {
		t.Fatalf("preview = %+v", dto)
	}
	dto, _ = f.preview(`{"harness":"claude"}`)
	if dto.Credential == nil || dto.Credential.Source != "claim_selection" {
		t.Fatalf("preview = %+v", dto)
	}
}

func TestStartPreviewRefusalBeforeAHarnessIsDecidedLiveDB(t *testing.T) {
	f := newEffFix(t, nil)
	f.anthropic()
	dto, _ := f.preview(`{"harness":"codex"}`)
	if dto.Harness != nil || strs(dto.Refusal) != "no_credential_for_harness" || dto.Credential != nil || len(dto.Alternatives) != 0 {
		t.Fatalf("preview = %+v", dto)
	}
	empty := newEffFix(t, nil)
	dto, _ = empty.preview(`{}`)
	if dto.Harness != nil || strs(dto.Refusal) != "no_usable_credential" {
		t.Fatalf("preview = %+v", dto)
	}
}

// altOf returns the alternative for a harness.
func altOf(t *testing.T, dto apitypes.StartPreviewDTO, harness string) apitypes.StartPreviewAlternativeDTO {
	t.Helper()
	for _, a := range dto.Alternatives {
		if a.Harness == harness {
			return a
		}
	}
	t.Fatalf("no alternative for %s in %+v", harness, dto)
	return apitypes.StartPreviewAlternativeDTO{}
}

func requireValidAlt(t *testing.T, a apitypes.StartPreviewAlternativeDTO, source string) {
	t.Helper()
	if a.Refusal != nil || a.RefusalDetail != nil || a.Credential == nil || a.Credential.Source != source {
		t.Fatalf("%s alternative = %+v, want a valid branch with credential source %s", a.Harness, a, source)
	}
}

// TestStartPreviewAlternativesLiveDB: deviations 6 and 12. A chosen-at-create harness answers
// per branch, as creation does once it lands on each harness in turn.
func TestStartPreviewAlternativesLiveDB(t *testing.T) {
	inherit := `{"harness":null,"credential_override":{"mode":"inherit"}}`
	raw := &workersvc.RawCredentialOverride{Mode: "inherit"}

	t.Run("automatic policy normalises inherit on the codex alternative", func(t *testing.T) {
		for _, tc := range []struct {
			name      string
			claudePct int
			codexUtil float64
			lands     string
		}{{"lands on claude", 10, 95, "claude"}, {"lands on codex", 95, 10, "codex"}} {
			t.Run(tc.name, func(t *testing.T) {
				f := newEffFix(t, nil)
				f.bothUsable(tc.claudePct, tc.codexUtil)
				dto, body := f.preview(inherit)
				wantHarness(t, dto.Harness, "", "chosen_at_create", "automatic_weekly_reset")
				if dto.Refusal != nil || dto.Credential != nil || len(dto.Alternatives) != 2 || dto.Alternatives[0].Harness != "claude" || dto.Alternatives[1].Harness != "codex" {
					t.Fatalf("preview = %s", body)
				}
				requireValidAlt(t, altOf(t, dto, "claude"), "claim_selection")
				requireValidAlt(t, altOf(t, dto, "codex"), "codex_default_credential")
				run, err := f.create(nil, raw)
				if err != nil || run.Harness != tc.lands {
					t.Fatalf("create with inherit = %q %v, want a run on %s", run.Harness, err, tc.lands)
				}
				if run.CredentialOverrideMode.Valid {
					t.Errorf("an inherit override leaves no override columns, got %v", run.CredentialOverrideMode)
				}
			})
		}
	})

	// Deviation 12's case: default Codex, both usable, overflow on, no cross-check.
	t.Run("overflow with codex preferred: claude valid, codex refused", func(t *testing.T) {
		f := newEffFix(t, nil)
		f.bothUsable(0, 90) // codex near its limit, claude wide open: overflow moves to claude
		f.policy("codex", true)
		dto, body := f.preview(inherit)
		wantHarness(t, dto.Harness, "", "chosen_at_create", "overflow")
		if dto.Refusal != nil || dto.RefusalDetail != nil || dto.Credential != nil || len(dto.Alternatives) != 2 {
			t.Fatalf("preview = %s", body)
		}
		requireValidAlt(t, altOf(t, dto, "claude"), "claim_selection")
		codex := altOf(t, dto, "codex")
		if strs(codex.Refusal) != "credential_override_harness_unsupported" || codex.RefusalDetail != nil || codex.Credential != nil {
			t.Fatalf("codex alternative = %+v, want the harness-unsupported refusal", codex)
		}
		// Creation lands on claude (overflow clears the inherit) and keeps no override columns.
		run, err := f.create(nil, raw)
		if err != nil || run.Harness != "claude" || run.CredentialOverrideMode.Valid {
			t.Fatalf("create landing on claude = %q %v override=%v", run.Harness, err, run.CredentialOverrideMode)
		}
		// With codex healthy the same request stays on codex, which creation refuses.
		f.codexReading(10)
		if _, err := f.create(nil, raw); !errors.Is(err, workersvc.ErrCredentialOverrideHarnessUnsupported) {
			t.Fatalf("create landing on codex = %v, want ErrCredentialOverrideHarnessUnsupported", err)
		}
	})

	t.Run("overflow with claude preferred: both valid", func(t *testing.T) {
		f := newEffFix(t, nil)
		f.bothUsable(90, 10) // claude near its limit: overflow moves to codex
		f.policy("claude", true)
		dto, body := f.preview(inherit)
		wantHarness(t, dto.Harness, "", "chosen_at_create", "overflow")
		if dto.Refusal != nil || len(dto.Alternatives) != 2 {
			t.Fatalf("preview = %s", body)
		}
		requireValidAlt(t, altOf(t, dto, "claude"), "claim_selection")
		requireValidAlt(t, altOf(t, dto, "codex"), "codex_default_credential")
		if g, _ := f.get(); !f.choice(g, "default").AnthropicOverrideAllowed {
			t.Errorf("anthropic_override_allowed = false for an overflow with claude preferred, want true")
		}
		if run, err := f.create(nil, raw); err != nil || run.Harness != "codex" {
			t.Fatalf("create landing on codex = %q %v (overflow normalises the inherit)", run.Harness, err)
		}
	})

	t.Run("no override at all", func(t *testing.T) {
		f := newEffFix(t, nil)
		f.bothUsable(10, 10)
		dto, _ := f.preview(`{"harness":null,"credential_override":null}`)
		if dto.Harness == nil || dto.Harness.Value != nil || len(dto.Alternatives) != 2 {
			t.Fatalf("preview = %+v", dto)
		}
	})
}

// ---- cells equal GET /me/settings ----

func TestEffectiveSettingsCellsEqualMySettingsLiveDB(t *testing.T) {
	f := newEffFix(t, &settingsStore{rows: []store.AppSetting{
		{Key: settings.KeyJudgeModel, Value: "haiku"},
		{Key: settings.KeyCodexJudgeModel, Value: "gpt-6-sol"},
		{Key: settings.KeySummaryModel, Value: "sonnet"},
	}})
	f.bothUsable(10, 10)
	f.set("judge_model", "opus")
	f.set("codex_summary_model", "gpt-6-luna")
	f.set("default_effort", "low")
	f.exec(`INSERT INTO user_cross_check_pins(user_id,stage,harness,model) VALUES($1,'plan','codex','gpt-6-astra')`, f.user)
	f.exec(`INSERT INTO user_cross_check_pins(user_id,stage,harness,effort) VALUES($1,'code','claude','max')`, f.user)

	dto, _ := f.get()
	rec := httptest.NewRecorder()
	f.h.GetMySettings(rec, userReq(http.MethodGet, "/api/me/settings", "", f.user, nil))
	var mine struct {
		Settings struct {
			JudgeModels   []apitypes.ModelSettingCellDTO `json:"judge_models"`
			SummaryModels []apitypes.ModelSettingCellDTO `json:"summary_models"`
			Pins          []crossCheckPinDTO             `json:"cross_check_pins"`
		} `json:"settings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &mine); err != nil || len(mine.Settings.JudgeModels) != 2 || len(mine.Settings.Pins) != 4 {
		t.Fatalf("GET /me/settings: %v %s", err, rec.Body.String())
	}
	for i, h := range []string{"claude", "codex"} {
		if got := modelCell(t, dto.Models, "judge_model", h).ModelSettingCellDTO; !sameCell(got, mine.Settings.JudgeModels[i]) {
			t.Errorf("judge %s: %+v != %+v", h, got, mine.Settings.JudgeModels[i])
		}
		if got := modelCell(t, dto.Models, "summary_model", h).ModelSettingCellDTO; !sameCell(got, mine.Settings.SummaryModels[i]) {
			t.Errorf("summary %s: %+v != %+v", h, got, mine.Settings.SummaryModels[i])
		}
	}
	if judge := modelCell(t, dto.Models, "judge_model", "claude"); judge.Source != "user_default" || judge.ResolvedModel != "opus" {
		t.Errorf("user judge lane wins: %+v", judge)
	}
	if judge := modelCell(t, dto.Models, "judge_model", "codex"); judge.Source != "instance_default" || judge.ResolvedModel != "gpt-6-sol" {
		t.Errorf("instance codex judge: %+v", judge)
	}
	for i, p := range mine.Settings.Pins {
		m := modelCell(t, dto.CrossCheck.Models, "cross_check_"+p.Stage, p.Harness)
		e := effortCell(t, dto.CrossCheck.Efforts, "cross_check_"+p.Stage, p.Harness)
		wantModelSrc := map[string]string{"pin": "cross_check_pin", "worker default": "cross_check_worker_default"}[p.ModelSource]
		wantEffortSrc := map[string]string{"pin": "cross_check_pin", "cross-check default": "cross_check_default"}[p.EffortSource]
		resolvedModel := ""
		if p.ResolvedModel != nil {
			resolvedModel = *p.ResolvedModel
		}
		if !eqStr(m.Model, p.Model) || m.ResolvedModel != resolvedModel || m.Source != wantModelSrc || wantModelSrc == "" ||
			!eqStr(e.Effort, p.Effort) || e.ResolvedEffort == nil || *e.ResolvedEffort != p.ResolvedEffort || e.Source != wantEffortSrc || wantEffortSrc == "" {
			t.Errorf("cross-check cell %d (%s/%s): view %+v / %+v, settings %+v", i, p.Stage, p.Harness, m, e, p)
		}
	}
	// An unpinned checker keeps the cross-check default whatever the worker effort is.
	e := effortCell(t, dto.CrossCheck.Efforts, "cross_check_plan", "claude")
	if e.Effort != nil || *e.ResolvedEffort != "high" || e.Source != "cross_check_default" {
		t.Errorf("unpinned checker effort with worker effort low = %+v, want high / cross_check_default", e)
	}
	if e := effortCell(t, dto.CrossCheck.Efforts, "cross_check_code", "claude"); *e.ResolvedEffort != "max" || e.Source != "cross_check_pin" {
		t.Errorf("pinned checker effort = %+v", e)
	}
}

// ---- instance cache posture, through the route ----

func TestEffectiveSettingsInstanceSettingsUnavailableLiveDB(t *testing.T) {
	f := newEffFix(t, &settingsStore{err: errors.New("settings store down")})
	f.anthropic()
	dto, _ := f.get() // a cold settings cache must not fail the read
	for _, setting := range []string{"judge_model", "summary_model"} {
		for _, h := range []string{"claude", "codex"} {
			c := modelCell(t, dto.Models, setting, h)
			if c.Source != "builtin" || !effHas(c.Notes, "instance_settings_unavailable") {
				t.Errorf("%s/%s = %+v, want the built-in with the unavailable note", setting, h, c)
			}
		}
	}
	for _, c := range dto.Models {
		if (c.Setting == "worker_model" || c.Setting == "task_review_model") && effHas(c.Notes, "instance_settings_unavailable") {
			t.Errorf("%s/%s does not read instance settings and must not carry the note", c.Setting, c.Harness)
		}
	}
}

func effHas(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// ---- bounds and credential material ----

func TestEffectiveSettingsSizeBoundsAndNoCredentialMaterialLiveDB(t *testing.T) {
	f := newEffFix(t, nil)
	f.bothUsable(10, 10)
	long := strings.Repeat("m", agenttmpl.MaxModelLen)
	f.set("default_claude_model", long)
	f.set("default_codex_model", long)
	f.set("judge_model", long)
	f.set("codex_judge_model", long)
	f.set("summary_model", long)
	f.set("codex_summary_model", long)
	f.leadTemplate(long, long)
	f.exec(`INSERT INTO user_cross_check_pins(user_id,stage,harness,model) VALUES($1,'plan','claude',$2),($1,'code','claude',$2)`, f.user, long)
	f.exec(`INSERT INTO user_cross_check_pins(user_id,stage,harness,model) VALUES($1,'plan','codex',$2),($1,'code','codex',$2)`, f.user, long)

	dto, body := f.get()
	if len(body) >= 16<<10 {
		t.Errorf("GET body = %d bytes, want < 16 KiB", len(body))
	}
	if len(dto.Models) != 8 {
		t.Fatalf("models = %d", len(dto.Models))
	}
	const inherit = `{"harness":null,"credential_override":{"mode":"inherit"}}`
	_, small := f.preview(inherit)
	if len(small) >= 2<<10 {
		t.Errorf("start-preview body = %d bytes, want < 2 KiB", len(small))
	}
	// A refused branch is the largest start-preview shape.
	f.policy("codex", true)
	f.codexReading(90)
	f.claudeReading(0)
	_, refused := f.preview(inherit)
	if len(refused) >= 2<<10 {
		t.Errorf("start-preview body with a refused alternative = %d bytes, want < 2 KiB", len(refused))
	}

	f.addToken(uuid.New(), "eff-extra-"+uuid.NewString()[:8], false)
	ids := f.secretsToScan
	_, mixed := f.preview(fmt.Sprintf(`{"harness":"claude","credential_override":{"mode":"pinned","secret_id":%q}}`, f.token))
	for name, b := range map[string][]byte{"GET": body, "preview": small, "refused preview": refused, "pinned preview": mixed} {
		for _, secret := range ids {
			if secret != "" && bytes.Contains(b, []byte(secret)) {
				t.Errorf("%s response carries credential material %q", name, secret)
			}
		}
	}
}

// ---- the recorded behavior corpus ----

type effCorpusPreview struct {
	Name     string                       `json:"name"`
	Request  apitypes.StartPreviewRequest `json:"request"`
	Response apitypes.StartPreviewDTO     `json:"response"`
}

type effCorpusScenario struct {
	Name          string                        `json:"name"`
	Get           apitypes.EffectiveSettingsDTO `json:"get"`
	StartPreviews []effCorpusPreview            `json:"start_previews"`
}

type effCorpus struct {
	Scenarios []effCorpusScenario `json:"scenarios"`
}

const (
	corpusEnabledToken  = "00000000-0000-4000-8000-0000000000a1"
	corpusDisabledToken = "00000000-0000-4000-8000-0000000000a2"
	corpusMissingToken  = "00000000-0000-4000-8000-0000000000a3"
)

// TestEffectiveSettingsCorpusLiveDB records the responses the web mock serves
// (fixtures/api-contract/effective_settings.behavior.json): the default scenario (both harnesses
// usable), codexOnly, and a Codex-default user with overflow on, whose inherit preview carries a
// refused alternative. The corpus is recorded from the live routes, not authored.
func TestEffectiveSettingsCorpusLiveDB(t *testing.T) {
	previews := func(f *effFix, specs ...[2]string) []effCorpusPreview {
		var out []effCorpusPreview
		for _, s := range specs {
			dto, _ := f.preview(s[1])
			var req apitypes.StartPreviewRequest
			strictDecode(t, []byte(s[1]), &req)
			out = append(out, effCorpusPreview{Name: s[0], Request: req, Response: dto})
		}
		return out
	}
	both := func(f *effFix) {
		f.bothUsable(10, 10)
		f.leadTemplate("opus", "gpt-6-sol")
		f.set("default_claude_model", "")
		f.addToken(uuid.MustParse(corpusEnabledToken), "corpus-enabled", false)
		f.addToken(uuid.MustParse(corpusDisabledToken), "corpus-disabled", true)
		f.catalog.state = workersvc.CatalogStateKnown
	}
	var corpus effCorpus

	f := newEffFix(t, nil)
	both(f)
	dto, _ := f.get()
	corpus.Scenarios = append(corpus.Scenarios, effCorpusScenario{Name: "default", Get: dto, StartPreviews: previews(f,
		[2]string{"default start", `{"harness":null,"credential_override":null}`},
		[2]string{"claude", `{"harness":"claude","credential_override":null}`},
		[2]string{"codex", `{"harness":"codex","credential_override":null}`},
		[2]string{"codex with a token override", `{"harness":"codex","credential_override":{"mode":"auto"}}`},
		[2]string{"inherit", `{"harness":null,"credential_override":{"mode":"inherit"}}`},
		[2]string{"auto token", `{"harness":null,"credential_override":{"mode":"auto"}}`},
		[2]string{"enabled pinned token", `{"harness":null,"credential_override":{"mode":"pinned","secret_id":"` + corpusEnabledToken + `"}}`},
		[2]string{"disabled pinned token", `{"harness":null,"credential_override":{"mode":"pinned","secret_id":"` + corpusDisabledToken + `"}}`},
		[2]string{"missing pinned token", `{"harness":null,"credential_override":{"mode":"pinned","secret_id":"` + corpusMissingToken + `"}}`},
		[2]string{"pinned without a token", `{"harness":null,"credential_override":{"mode":"pinned"}}`},
	)})

	f = newEffFix(t, nil)
	f.codexDefault()
	f.catalog.state = workersvc.CatalogStateUnknown
	dto, _ = f.get()
	corpus.Scenarios = append(corpus.Scenarios, effCorpusScenario{Name: "codexOnly", Get: dto, StartPreviews: previews(f,
		[2]string{"default start", `{"harness":null,"credential_override":null}`},
		[2]string{"claude", `{"harness":"claude","credential_override":null}`},
		[2]string{"codex", `{"harness":"codex","credential_override":null}`},
	)})

	f = newEffFix(t, nil)
	both(f)
	f.policy("codex", true)
	dto, _ = f.get()
	corpus.Scenarios = append(corpus.Scenarios, effCorpusScenario{Name: "overflowCodexDefault", Get: dto, StartPreviews: previews(f,
		[2]string{"default start", `{"harness":null,"credential_override":null}`},
		[2]string{"inherit", `{"harness":null,"credential_override":{"mode":"inherit"}}`},
	)})

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(corpus); err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSuffix(buf.String(), "\n")
	name := "effective_settings.behavior.json"
	want, err := os.ReadFile(filepath.Join(contractFixtureDir, name))
	if err != nil || strings.TrimSuffix(string(want), "\n") != got {
		t.Errorf("fixture %s is stale -- re-record it from this exact output (recorded, not authored):\n%s", name, got)
	}
}
