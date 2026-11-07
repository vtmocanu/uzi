package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/agenttmpl"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
	"github.com/vtmocanu/uzi/api/internal/schedsvc"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// effective_settings_schedule_livedb_test.go drives POST /api/me/effective-settings/schedule-preview
// against a real Postgres. Each prediction is compared with what the real code does for the same
// schedule: POST /api/repos/{id}/schedules for the save step, a real schedsvc fire (RunNow) for the
// fire step and the run it creates. What the claim ships for the lead model, effort and subagents is
// pinned against the same prediction in workersvc (effective_settings_schedule_livedb_test.go
// there), because assembling a claim needs that package's worker fixture. Skipped unless
// UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via ./e2e/run-store-it.sh.

const schedPath = "/api/me/effective-settings/schedule-preview"

func (f *effFix) schedulePreviewRaw(user uuid.UUID, body string) *httptest.ResponseRecorder {
	f.t.Helper()
	rec := httptest.NewRecorder()
	f.h.PreviewScheduleSettings(rec, userReq(http.MethodPost, schedPath, body, user, nil))
	return rec
}

func (f *effFix) schedulePreview(body string) (apitypes.ScheduleSettingsPreviewDTO, []byte) {
	f.t.Helper()
	rec := f.schedulePreviewRaw(f.user, body)
	if rec.Code != http.StatusOK {
		f.t.Fatalf("schedule-preview %s = %d %s", body, rec.Code, rec.Body.String())
	}
	var dto apitypes.ScheduleSettingsPreviewDTO
	strictDecode(f.t, rec.Body.Bytes(), &dto)
	return dto, rec.Body.Bytes()
}

// saveSchedule is the real POST /api/repos/{id}/schedules.
func (f *effFix) saveSchedule(body string) (id string, code int, resp string) {
	f.t.Helper()
	rec := httptest.NewRecorder()
	f.h.CreateSchedule(rec, userReq(http.MethodPost, "/api/repos/"+f.repo.String()+"/schedules", body, f.user, map[string]string{"id": f.repo.String()}))
	var out struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out.ID, rec.Code, rec.Body.String()
}

// sweepBody is a sweep schedule save with extra JSON members (each starting with a comma).
func sweepBody(extra string) string {
	return `{"target":"sweep","timing":"recurring","cron_expr":"0 * * * *","labels":["bug"],"max_issues":1` + extra + `}`
}

// insertSweep stores a sweep directly, for states a save cannot produce (a pin disabled or deleted
// since, an override a harness change left behind).
func (f *effFix) insertSweep(harness, model string, overrideSub bool, mode string, secret *uuid.UUID) string {
	f.t.Helper()
	id := uuid.New()
	f.exec(`INSERT INTO run_schedules (id, user_id, repo_id, target, labels, timing, cron_expr, timezone, next_fire_at, auto_approve, status, enabled,
			max_issues, harness, model, override_subagent_model, credential_override_mode, credential_override_secret_id)
		VALUES ($1, $2, $3, 'sweep', '["bug"]'::jsonb, 'recurring', '0 * * * *', 'UTC', now() + interval '1 hour', true, 'active', true,
			1, NULLIF($4,''), NULLIF($5,''), $6, NULLIF($7,''), $8)`, id, f.user, f.repo, harness, model, overrideSub, mode, secret)
	return id.String()
}

// fire runs the real scheduler once for the schedule over a fresh eligible sweep candidate. It returns
// the run it started (nil when none) and the skip reasons it recorded.
func (f *effFix) fire(scheduleID string) (*store.Run, []string) {
	f.t.Helper()
	f.iid++
	f.exec(`INSERT INTO issues (id, repo_id, forge_issue_iid, title, state, labels, web_url, has_prd_link, forge_updated_at, synced_at)
		VALUES ($1, $2, $3, 'eff sweep issue', 'opened', '["bug","uzi"]', 'https://forge.e2e/i', false, now(), now())`, uuid.New(), f.repo, f.iid)
	sched := schedsvc.New(f.h.q, f.h.wsvc, sweepGateBuilder{f: &sweepGateForge{}}, sweepGateSettings{}, nil, nil, time.Minute, nil)
	row, err := f.h.q.GetRunSchedule(f.ctx, uuid.MustParse(scheduleID))
	if err != nil {
		f.t.Fatal(err)
	}
	out, err := sched.RunNow(f.ctx, row)
	if err != nil {
		f.t.Fatalf("fire: %v", err)
	}
	var skips []string
	for _, s := range out.Skips {
		skips = append(skips, string(s.Reason))
	}
	if len(out.Started) == 0 {
		return nil, skips
	}
	run, err := f.h.q.GetRunByID(f.ctx, out.Started[0].RunID)
	if err != nil {
		f.t.Fatal(err)
	}
	return &run, skips
}

func pgText(t pgtype.Text) *string {
	if !t.Valid {
		return nil
	}
	s := t.String
	return &s
}

func (f *effFix) subagent(name, claudePin, codexPin string) {
	f.t.Helper()
	id := uuid.New()
	f.exec(`INSERT INTO agent_templates(id,name,description,prompt_body,scope,user_id,model,codex_model)
		VALUES($1,$2,'d','b','user',$3,NULLIF($4,''),NULLIF($5,''))`, id, name, f.user, claudePin, codexPin)
	f.exec(`INSERT INTO agent_template_allocations(template_id,user_id,enabled) VALUES($1,$2,true)`, id, f.user)
}

func wantModelCell(t *testing.T, c *apitypes.EffectiveModelCellDTO, resolved, source string, ignored string, notes ...string) {
	t.Helper()
	if c == nil {
		t.Fatalf("worker_model is null, want %s (%s)", resolved, source)
	}
	gotIgnored := ""
	if c.Ignored != nil {
		gotIgnored = c.Ignored.Value + "/" + c.Ignored.Source
	}
	if c.Setting != "worker_model" || c.FixedAt != "claim" || c.ResolvedModel != resolved || c.Source != source ||
		gotIgnored != ignored || strings.Join(c.Notes, ",") != strings.Join(notes, ",") {
		t.Fatalf("worker_model = %+v (ignored %q), want %q (%s) ignored %q notes %v", c, gotIgnored, resolved, source, ignored, notes)
	}
}

func wantSubagents(t *testing.T, s *apitypes.EffectiveSubagentsDTO, mode, lead string, pinned ...string) {
	t.Helper()
	if s == nil {
		t.Fatalf("subagents is null, want %s", mode)
	}
	var got []string
	for _, p := range s.Pinned {
		got = append(got, p.Name+"="+p.Model)
	}
	if s.Mode != mode || s.LeadValue != lead || strings.Join(got, ",") != strings.Join(pinned, ",") || s.Pinned == nil ||
		strings.Join(s.Notes, ",") != "repo_agents_not_predicted" {
		t.Fatalf("subagents = %+v, want %s lead %q pins %v and the repo_agents_not_predicted note", s, mode, lead, pinned)
	}
}

func wantCredential(t *testing.T, c *apitypes.CredentialDTO, kind, source, fixed string) {
	t.Helper()
	if c == nil || c.Kind != kind || c.Source != source || c.FixedAt != fixed {
		t.Fatalf("credential = %+v, want %s/%s/%s", c, kind, source, fixed)
	}
}

func wantRefusal(t *testing.T, dto apitypes.ScheduleSettingsPreviewDTO, body []byte, token string) {
	t.Helper()
	if strs(dto.Refusal) != token || dto.RefusalDetail != nil || dto.WorkerModel != nil || dto.WorkerEffort != nil || dto.Credential != nil || dto.Subagents != nil || len(dto.Alternatives) != 0 {
		t.Fatalf("preview = %s, want only the refusal %s", body, token)
	}
}

// ---- example 3 ----

// TestSchedulePreviewExample3LiveDB: a sweep pinned to a Claude model, the subagent toggle on, and a
// user whose default harness is Codex. Firing it creates the Codex run the preview describes.
func TestSchedulePreviewExample3LiveDB(t *testing.T) {
	setup := func(f *effFix) {
		f.bothUsable(10, 10)
		f.policy("codex", false)
		f.set("default_codex_model", "gpt-6-sol")
	}
	t.Run("override omitted: preview, save, fire", func(t *testing.T) {
		f := newEffFix(t, nil)
		setup(f)
		f.subagent("coder", "sonnet", "gpt-6-astra")
		dto, body := f.schedulePreview(`{"target":"sweep","harness":null,"model":"sonnet","override_subagent_model":true}`)
		wantHarness(t, dto.Harness, "codex", "user_default")
		if dto.Refusal != nil || len(dto.Alternatives) != 0 || strings.Join(dto.Notes, ",") != "issue_labels_may_select_harness" {
			t.Fatalf("preview = %s", body)
		}
		wantModelCell(t, dto.WorkerModel, "gpt-6-sol", "user_default", "sonnet/schedule_pin", "schedule_pin_ignored")
		if e := dto.WorkerEffort; e == nil || e.Setting != "worker_effort" || e.Harness != "codex" || e.CatalogDefault == nil {
			t.Fatalf("worker_effort = %+v", e)
		}
		wantCredential(t, dto.Credential, "codex", "codex_default_credential", "create")
		wantSubagents(t, dto.Subagents, "follow_lead", "gpt-6-sol")

		// The real save accepts the same draft, and the saved schedule previews identically.
		id, code, resp := f.saveSchedule(sweepBody(`,"model":"sonnet","override_subagent_model":true`))
		if code != http.StatusCreated {
			t.Fatalf("POST /schedules = %d %s", code, resp)
		}
		_, savedBody := f.schedulePreview(fmt.Sprintf(`{"schedule_id":%q}`, id))
		if !bytes.Equal(body, savedBody) {
			t.Errorf("a saved schedule previews differently from its draft:\n%s\n%s", body, savedBody)
		}
		// Fire: a Codex run with the schedule's pin frozen and the toggle on.
		run, skips := f.fire(id)
		if run == nil {
			t.Fatalf("fire started no run (skips %v)", skips)
		}
		if run.Harness != "codex" || strs(pgText(run.Model)) != "sonnet" || !run.OverrideSubagentModel || run.CredentialOverrideMode.Valid {
			t.Fatalf("fired run = harness %q model %q override_subagent_model %v override %v", run.Harness, strs(pgText(run.Model)), run.OverrideSubagentModel, run.CredentialOverrideMode)
		}
	})

	t.Run("explicit null override is the save refusal POST /schedules gives", func(t *testing.T) {
		f := newEffFix(t, nil)
		setup(f)
		dto, body := f.schedulePreview(`{"target":"sweep","harness":null,"model":"sonnet","override_subagent_model":true,"credential_override":null}`)
		if dto.Harness != nil {
			t.Fatalf("a save refusal decides no harness: %s", body)
		}
		wantRefusal(t, dto, body, "credential_override_harness_unsupported")
		_, code, resp := f.saveSchedule(sweepBody(`,"model":"sonnet","override_subagent_model":true,"credential_override":null`))
		if code != http.StatusUnprocessableEntity || !strings.Contains(resp, "not supported on the codex harness") {
			t.Fatalf("POST /schedules = %d %s, want the 422 harness-unsupported refusal", code, resp)
		}
		// Omitting the override is the draft that saves.
		if _, code, resp := f.saveSchedule(sweepBody(`,"model":"sonnet"`)); code != http.StatusCreated {
			t.Fatalf("POST /schedules without the override = %d %s", code, resp)
		}
	})

	t.Run("pinned to claude with a claude lead model that resolves to empty gives own pins", func(t *testing.T) {
		f := newEffFix(t, nil)
		f.anthropic()
		f.subagent("coder", "sonnet", "")
		f.subagent("scribe", "haiku", "")
		f.subagent("plain", "", "gpt-6-astra")
		dto, body := f.schedulePreview(`{"target":"sweep","harness":"claude","override_subagent_model":true}`)
		wantHarness(t, dto.Harness, "claude", "schedule_pin")
		wantModelCell(t, dto.WorkerModel, "", "builtin", "", "sdk_default")
		wantSubagents(t, dto.Subagents, "own_pins", "", "coder=sonnet", "scribe=haiku")
		wantCredential(t, dto.Credential, "anthropic", "claim_selection", "claim")
		if dto.Refusal != nil || len(dto.Alternatives) != 0 {
			t.Fatalf("preview = %s", body)
		}
		// With a lead model the same toggle means every subagent follows the lead.
		f.leadTemplate("opus", "")
		dto, _ = f.schedulePreview(`{"target":"sweep","harness":"claude","override_subagent_model":true}`)
		wantModelCell(t, dto.WorkerModel, "opus", "template", "")
		wantSubagents(t, dto.Subagents, "follow_lead", "opus")
		// The fired run carries the toggle the claim then applies.
		id := f.insertSweep("claude", "", true, "", nil)
		if run, skips := f.fire(id); run == nil || run.Harness != "claude" || !run.OverrideSubagentModel {
			t.Fatalf("fired run = %+v skips %v", run, skips)
		}
	})
}

// ---- schedule pins ----

func TestSchedulePreviewModelPinsLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name        string
		harness     string // the schedule's pin ("" = unpinned)
		model       string
		wantHarness string
		wantModel   string
		wantSource  string
		ignored     string
		notes       []string
	}{
		{"claude pin on claude", "claude", "sonnet", "claude", "sonnet", "schedule_pin", "", nil},
		{"claude pin on codex is ignored", "codex", "sonnet", "codex", "gpt-6-sol", "user_default", "sonnet/schedule_pin", []string{"schedule_pin_ignored"}},
		{"custom pin on codex is ignored", "codex", "my-custom-model", "codex", "gpt-6-sol", "user_default", "my-custom-model/schedule_pin", []string{"schedule_pin_ignored"}},
		{"codex pin on codex", "codex", "gpt-6-astra", "codex", "gpt-6-astra", "schedule_pin", "", nil},
		{"custom pin on claude", "claude", "my-custom-model", "claude", "my-custom-model", "schedule_pin", "", nil},
		{"codex role pin on claude is ignored", "claude", "gpt-6-luna", "claude", "", "builtin", "gpt-6-luna/schedule_pin", []string{"schedule_pin_ignored", "sdk_default"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newEffFix(t, nil)
			f.bothUsable(10, 10)
			f.set("default_codex_model", "gpt-6-sol")
			harness := "null"
			if tc.harness != "" {
				harness = fmt.Sprintf("%q", tc.harness)
			}
			dto, body := f.schedulePreview(fmt.Sprintf(`{"target":"sweep","harness":%s,"model":%q}`, harness, tc.model))
			wantHarness(t, dto.Harness, tc.wantHarness, "schedule_pin")
			if dto.Refusal != nil {
				t.Fatalf("preview = %s", body)
			}
			wantModelCell(t, dto.WorkerModel, tc.wantModel, tc.wantSource, tc.ignored, tc.notes...)

			id := f.insertSweep(tc.harness, tc.model, false, "", nil)
			run, skips := f.fire(id)
			if run == nil || run.Harness != tc.wantHarness || strs(pgText(run.Model)) != tc.model {
				t.Fatalf("fired run = %+v skips %v, want harness %s with the pin %q frozen", run, skips, tc.wantHarness, tc.model)
			}
		})
	}
}

// ---- a stored pinned token ----

func TestSchedulePreviewStoredPinnedTokenLiveDB(t *testing.T) {
	setup := func(f *effFix) uuid.UUID {
		f.anthropic()
		return f.addToken(uuid.New(), "eff-pin-"+uuid.NewString()[:8], false)
	}
	t.Run("enabled: token_override fixed at create", func(t *testing.T) {
		f := newEffFix(t, nil)
		pin := setup(f)
		id := f.insertSweep("", "", false, "pinned", &pin)
		dto, body := f.schedulePreview(fmt.Sprintf(`{"schedule_id":%q}`, id))
		wantHarness(t, dto.Harness, "claude", "automatic")
		wantCredential(t, dto.Credential, "anthropic", "token_override", "create")
		run, skips := f.fire(id)
		if run == nil || !run.CredentialOverrideSecretID.Valid || uuid.UUID(run.CredentialOverrideSecretID.Bytes) != pin {
			t.Fatalf("fired run = %+v skips %v; preview %s", run, skips, body)
		}
	})
	t.Run("disabled since: credential_disabled, the fire skips", func(t *testing.T) {
		f := newEffFix(t, nil)
		pin := setup(f)
		id := f.insertSweep("", "", false, "pinned", &pin)
		f.exec(`UPDATE user_secrets SET disabled_at = now() WHERE id = $1`, pin)
		dto, body := f.schedulePreview(fmt.Sprintf(`{"schedule_id":%q}`, id))
		wantHarness(t, dto.Harness, "claude", "automatic")
		if strs(dto.Refusal) != "credential_disabled" || dto.WorkerModel != nil || dto.Credential != nil || dto.Subagents != nil {
			t.Fatalf("preview = %s", body)
		}
		run, skips := f.fire(id)
		if run != nil || strings.Join(skips, ",") != "credential_disabled" {
			t.Fatalf("fire = run %v skips %v, want the credential_disabled skip", run, skips)
		}
	})
	t.Run("deleted since: the pin inherits, claim_selection", func(t *testing.T) {
		f := newEffFix(t, nil)
		pin := setup(f)
		id := f.insertSweep("", "", false, "pinned", &pin)
		f.exec(`DELETE FROM user_secrets WHERE id = $1`, pin)
		dto, body := f.schedulePreview(fmt.Sprintf(`{"schedule_id":%q}`, id))
		if dto.Refusal != nil {
			t.Fatalf("preview = %s", body)
		}
		wantCredential(t, dto.Credential, "anthropic", "claim_selection", "claim")
		run, skips := f.fire(id)
		if run == nil || run.Harness != "claude" || run.CredentialOverrideSecretID.Valid {
			t.Fatalf("fire = run %+v skips %v, want a run with the pin's id nulled", run, skips)
		}
	})
	t.Run("a pin supplied in the request is validated at save semantics", func(t *testing.T) {
		f := newEffFix(t, nil)
		pin := setup(f)
		disabled := f.addToken(uuid.New(), "eff-disabled-"+uuid.NewString()[:8], true)
		missing := uuid.New()
		for _, tc := range []struct {
			name, override string
			want           string
			status         int
		}{
			{"disabled", fmt.Sprintf(`{"mode":"pinned","secret_id":%q}`, disabled), "credential_disabled", http.StatusConflict},
			{"missing", fmt.Sprintf(`{"mode":"pinned","secret_id":%q}`, missing), "credential_not_found", http.StatusNotFound},
			{"no id", `{"mode":"pinned"}`, "credential_override_invalid", http.StatusBadRequest},
		} {
			dto, body := f.schedulePreview(`{"target":"sweep","credential_override":` + tc.override + `}`)
			wantRefusal(t, dto, body, tc.want)
			if _, code, resp := f.saveSchedule(sweepBody(`,"credential_override":` + tc.override)); code != tc.status {
				t.Errorf("%s: POST /schedules = %d %s, want %d", tc.name, code, resp, tc.status)
			}
		}
		dto, _ := f.schedulePreview(fmt.Sprintf(`{"target":"sweep","credential_override":{"mode":"pinned","secret_id":%q}}`, pin))
		wantCredential(t, dto.Credential, "anthropic", "token_override", "create")
	})
	t.Run("the other save refusals", func(t *testing.T) {
		f := newEffFix(t, nil)
		f.anthropic()
		dto, body := f.schedulePreview(`{"target":"sweep","model":"two words"}`)
		wantRefusal(t, dto, body, "invalid_model")
		if _, code, resp := f.saveSchedule(sweepBody(`,"model":"two words"`)); code != http.StatusBadRequest {
			t.Errorf("POST /schedules with a bad model = %d %s", code, resp)
		}
		// A self_improve schedule is catalog-enable-only (no direct save), and its lane never switches tokens.
		dto, body = f.schedulePreview(`{"target":"self_improve","credential_override":{"mode":"auto"}}`)
		wantRefusal(t, dto, body, "credential_override_lane_not_switchable")
	})
}

// ---- subagents ----

func TestSchedulePreviewSubagentsLiveDB(t *testing.T) {
	for _, tc := range []struct {
		harness, toggle string
		mode, lead      string
		pins            []string
		model           string
	}{
		{"claude", "false", "own_pins", "opus", []string{"coder=sonnet", "scribe=haiku"}, ""},
		{"claude", "true", "follow_lead", "opus", nil, ""},
		{"codex", "false", "own_pins", "gpt-6-sol", []string{"coder=gpt-6-astra"}, ""},
		{"codex", "true", "follow_lead", "gpt-6-sol", nil, ""},
		// A frozen schedule model becomes the lead the subagents follow.
		{"claude", "true", "follow_lead", "haiku", nil, "haiku"},
		{"codex", "true", "follow_lead", "gpt-6-astra", nil, "gpt-6-astra"},
	} {
		t.Run(fmt.Sprintf("%s toggle %s model %q", tc.harness, tc.toggle, tc.model), func(t *testing.T) {
			f := newEffFix(t, nil)
			f.bothUsable(10, 10)
			f.leadTemplate("opus", "gpt-6-sol")
			f.subagent("coder", "sonnet", "gpt-6-astra")
			f.subagent("scribe", "haiku", "")
			f.subagent("plain", "", "")
			body := fmt.Sprintf(`{"target":"sweep","harness":%q,"override_subagent_model":%s`, tc.harness, tc.toggle)
			if tc.model != "" {
				body += fmt.Sprintf(`,"model":%q`, tc.model)
			}
			dto, raw := f.schedulePreview(body + "}")
			if dto.Refusal != nil {
				t.Fatalf("preview = %s", raw)
			}
			wantSubagents(t, dto.Subagents, tc.mode, tc.lead, tc.pins...)
			id := f.insertSweep(tc.harness, tc.model, tc.toggle == "true", "", nil)
			run, skips := f.fire(id)
			if run == nil || run.Harness != tc.harness || run.OverrideSubagentModel != (tc.toggle == "true") {
				t.Fatalf("fired run = %+v skips %v", run, skips)
			}
		})
	}
}

// ---- alternatives and deviation 12 ----

func TestSchedulePreviewAlternativesLiveDB(t *testing.T) {
	t.Run("an unpinned schedule is chosen at create, one branch per harness", func(t *testing.T) {
		f := newEffFix(t, nil)
		f.bothUsable(10, 10)
		f.leadTemplate("opus", "gpt-6-sol")
		f.set("default_codex_effort", "high")
		dto, body := f.schedulePreview(`{"target":"sweep"}`)
		wantHarness(t, dto.Harness, "", "chosen_at_create", "automatic_weekly_reset")
		if dto.Refusal != nil || dto.WorkerModel != nil || dto.WorkerEffort != nil || dto.Credential != nil || dto.Subagents != nil ||
			len(dto.Alternatives) != 2 || dto.Alternatives[0].Harness != "claude" || dto.Alternatives[1].Harness != "codex" {
			t.Fatalf("preview = %s", body)
		}
		claude, codex := dto.Alternatives[0], dto.Alternatives[1]
		wantModelCell(t, claude.WorkerModel, "opus", "template", "")
		wantCredential(t, claude.Credential, "anthropic", "claim_selection", "claim")
		wantSubagents(t, claude.Subagents, "own_pins", "opus")
		wantModelCell(t, codex.WorkerModel, "gpt-6-sol", "template", "")
		wantCredential(t, codex.Credential, "codex", "codex_default_credential", "create")
		if e := codex.WorkerEffort; e == nil || strs(e.ResolvedEffort) != "high" {
			t.Fatalf("codex worker_effort = %+v", codex.WorkerEffort)
		}
		if claude.Refusal != nil || codex.Refusal != nil {
			t.Fatalf("both branches are valid: %s", body)
		}
		// The fire lands on one of them, and the run matches that branch.
		run, skips := f.fire(f.insertSweep("", "", false, "", nil))
		if run == nil {
			t.Fatalf("fire started no run (skips %v)", skips)
		}
		if run.Harness != "claude" && run.Harness != "codex" {
			t.Fatalf("fired run harness = %q", run.Harness)
		}
	})

	// Deviation 12, schedule side. A stored Anthropic override makes the creation-time policies
	// ineligible (the create seam is called with the override), so a schedule that stores one is
	// never chosen at create: the harness is the exact D11 answer and the Codex alternative the
	// start-preview shows does not exist here.
	t.Run("a stored auto override: exact claude, no alternatives, the fire matches", func(t *testing.T) {
		f := newEffFix(t, nil)
		f.bothUsable(10, 10)
		id := f.insertSweep("", "", false, "auto", nil)
		dto, body := f.schedulePreview(fmt.Sprintf(`{"schedule_id":%q}`, id))
		wantHarness(t, dto.Harness, "claude", "automatic")
		if dto.Refusal != nil || len(dto.Alternatives) != 0 {
			t.Fatalf("preview = %s", body)
		}
		wantCredential(t, dto.Credential, "anthropic", "auto_selection", "claim")
		run, skips := f.fire(id)
		if run == nil || run.Harness != "claude" || strs(pgText(run.CredentialOverrideMode)) != "auto" {
			t.Fatalf("fire = %+v skips %v", run, skips)
		}
	})
	t.Run("a stored auto override on a user whose only usable harness is codex: codex_override_conflict", func(t *testing.T) {
		f := newEffFix(t, nil)
		f.codexDefault()
		id := f.insertSweep("", "", false, "auto", nil)
		dto, body := f.schedulePreview(fmt.Sprintf(`{"schedule_id":%q}`, id))
		wantHarness(t, dto.Harness, "codex", "automatic")
		if strs(dto.Refusal) != "codex_override_conflict" || dto.WorkerModel != nil || dto.Credential != nil || dto.Subagents != nil || len(dto.Alternatives) != 0 {
			t.Fatalf("preview = %s", body)
		}
		run, skips := f.fire(id)
		if run != nil || strings.Join(skips, ",") != "codex_override_conflict" {
			t.Fatalf("fire = run %v skips %v, want the codex_override_conflict skip", run, skips)
		}
	})
	t.Run("a schedule pinned to codex that still stores an override", func(t *testing.T) {
		f := newEffFix(t, nil)
		f.bothUsable(10, 10)
		id := f.insertSweep("codex", "", false, "default", nil)
		dto, body := f.schedulePreview(fmt.Sprintf(`{"schedule_id":%q}`, id))
		wantHarness(t, dto.Harness, "codex", "schedule_pin")
		if strs(dto.Refusal) != "codex_override_conflict" || dto.WorkerModel != nil {
			t.Fatalf("preview = %s", body)
		}
		if run, skips := f.fire(id); run != nil || strings.Join(skips, ",") != "codex_override_conflict" {
			t.Fatalf("fire = run %v skips %v", run, skips)
		}
		// Clearing the pin in the request replaces the stored one: the fire would then run on claude.
		dto, _ = f.schedulePreview(fmt.Sprintf(`{"schedule_id":%q,"harness":"claude"}`, id))
		wantHarness(t, dto.Harness, "claude", "schedule_pin")
		wantCredential(t, dto.Credential, "anthropic", "owner_default", "claim")
	})
	t.Run("overflow keeps the harness chosen at create and records the policy", func(t *testing.T) {
		f := newEffFix(t, nil)
		f.bothUsable(90, 10)
		f.policy("claude", true)
		dto, _ := f.schedulePreview(`{"target":"issue"}`)
		wantHarness(t, dto.Harness, "", "chosen_at_create", "overflow")
		if len(dto.Alternatives) != 2 || dto.Alternatives[0].Refusal != nil || dto.Alternatives[1].Refusal != nil {
			t.Fatalf("preview = %+v", dto)
		}
	})
	t.Run("other targets resolve exactly", func(t *testing.T) {
		f := newEffFix(t, nil)
		f.bothUsable(10, 10)
		for _, target := range []string{"prompt", "ci_watch"} {
			dto, body := f.schedulePreview(fmt.Sprintf(`{"target":%q}`, target))
			wantHarness(t, dto.Harness, "claude", "automatic")
			if len(dto.Alternatives) != 0 || len(dto.Notes) != 0 || dto.WorkerModel == nil {
				t.Fatalf("%s preview = %s", target, body)
			}
		}
		dto, _ := f.schedulePreview(`{"target":"self_improve"}`)
		wantHarness(t, dto.Harness, "claude", "automatic")
		wantCredential(t, dto.Credential, "anthropic", "owner_default", "claim")
		if strings.Join(dto.Notes, ",") != "token_fixed" {
			t.Fatalf("self_improve notes = %v", dto.Notes)
		}
		dto, _ = f.schedulePreview(`{"target":"self_improve","harness":"codex"}`)
		wantCredential(t, dto.Credential, "codex", "codex_default_credential", "create")
		if len(dto.Notes) != 0 {
			t.Fatalf("a codex self_improve run has no fixed token: %v", dto.Notes)
		}
	})
}

// ---- request handling ----

func TestSchedulePreviewRequestsLiveDB(t *testing.T) {
	f := newEffFix(t, nil)
	f.anthropic()
	mine := f.insertSweep("", "", false, "", nil)
	stranger := mkSecretUser(t, f.pool)
	t.Cleanup(func() { _, _ = f.pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, stranger) })

	t.Run("a foreign or missing schedule is 404", func(t *testing.T) {
		if rec := f.schedulePreviewRaw(stranger, fmt.Sprintf(`{"schedule_id":%q}`, mine)); rec.Code != http.StatusNotFound {
			t.Errorf("foreign schedule = %d %s, want 404", rec.Code, rec.Body.String())
		}
		if rec := f.schedulePreviewRaw(f.user, fmt.Sprintf(`{"schedule_id":%q}`, uuid.New())); rec.Code != http.StatusNotFound {
			t.Errorf("missing schedule = %d, want 404", rec.Code)
		}
		if rec := f.schedulePreviewRaw(f.user, fmt.Sprintf(`{"schedule_id":%q}`, mine)); rec.Code != http.StatusOK {
			t.Errorf("own schedule = %d, want 200", rec.Code)
		}
	})
	t.Run("bad bodies are 400", func(t *testing.T) {
		for name, body := range map[string]string{
			"empty body":              ``,
			"not json":                `nope`,
			"no target and no id":     `{}`,
			"unknown field":           `{"target":"sweep","nope":1}`,
			"trailing value":          `{"target":"sweep"} {}`,
			"unknown target":          `{"target":"chat"}`,
			"unknown harness":         `{"target":"sweep","harness":"gpt"}`,
			"harness not a string":    `{"target":"sweep","harness":3}`,
			"malformed schedule id":   `{"schedule_id":"not-a-uuid"}`,
			"unknown override mode":   `{"target":"sweep","credential_override":{"mode":"always"}}`,
			"malformed secret id":     `{"target":"sweep","credential_override":{"mode":"pinned","secret_id":"x"}}`,
			"override unknown member": `{"target":"sweep","credential_override":{"mode":"auto","x":1}}`,
			"toggle not a bool":       `{"target":"sweep","override_subagent_model":"yes"}`,
			"over the 2 KiB bound":    `{"target":"sweep","model":"` + strings.Repeat("m", 2100) + `"}`,
		} {
			if rec := f.schedulePreviewRaw(f.user, body); rec.Code != http.StatusBadRequest {
				t.Errorf("%s = %d %s, want 400", name, rec.Code, rec.Body.String())
			}
		}
	})
	t.Run("supplied fields replace stored ones and absent ones are as stored", func(t *testing.T) {
		f := newEffFix(t, nil)
		f.bothUsable(10, 10)
		id := f.insertSweep("codex", "gpt-6-astra", true, "", nil)
		stored, _ := f.schedulePreview(fmt.Sprintf(`{"schedule_id":%q}`, id))
		wantHarness(t, stored.Harness, "codex", "schedule_pin")
		wantModelCell(t, stored.WorkerModel, "gpt-6-astra", "schedule_pin", "")
		wantSubagents(t, stored.Subagents, "follow_lead", "gpt-6-astra")
		over, _ := f.schedulePreview(fmt.Sprintf(`{"schedule_id":%q,"harness":"claude","model":null,"override_subagent_model":false}`, id))
		wantHarness(t, over.Harness, "claude", "schedule_pin")
		wantModelCell(t, over.WorkerModel, "", "builtin", "", "sdk_default")
		wantSubagents(t, over.Subagents, "own_pins", "")
		// A cleared pin with the stored model still pinned is an exact answer: a frozen run model
		// makes the creation-time policies ineligible. Clearing the model too leaves it open.
		cleared, _ := f.schedulePreview(fmt.Sprintf(`{"schedule_id":%q,"harness":null}`, id))
		wantHarness(t, cleared.Harness, "claude", "automatic")
		if len(cleared.Alternatives) != 0 {
			t.Fatalf("a schedule with a model pin is never chosen at create: %+v", cleared)
		}
		open, _ := f.schedulePreview(fmt.Sprintf(`{"schedule_id":%q,"harness":null,"model":null}`, id))
		wantHarness(t, open.Harness, "", "chosen_at_create", "automatic_weekly_reset")
		if len(open.Alternatives) != 2 {
			t.Fatalf("an unpinned, unmodelled schedule is chosen at create: %+v", open)
		}
	})
}

// ---- bounds and credential material ----

func TestSchedulePreviewSizeBoundsAndNoCredentialMaterialLiveDB(t *testing.T) {
	f := newEffFix(t, nil)
	f.bothUsable(10, 10)
	long := strings.Repeat("m", agenttmpl.MaxModelLen)
	f.set("default_claude_model", long)
	f.set("default_codex_model", long)
	f.leadTemplate(long, long)
	for i := range 33 {
		name := fmt.Sprintf("%s%02d", strings.Repeat("n", 62), i)
		f.subagent(name, long, "gpt-6-astra") // a Codex subagent can pin only a Codex role id
	}
	dto, body := f.schedulePreview(`{"target":"sweep"}`)
	if len(dto.Alternatives) != 2 {
		t.Fatalf("want two branches: %s", body)
	}
	// The Claude branch is the maximum: 32 pins of 100 bytes behind 64-byte names, truncated.
	if c := dto.Alternatives[0].Subagents; c == nil || len(c.Pinned) != 32 || !c.PinnedTruncated {
		t.Fatalf("claude subagents = %+v, want 32 pins and truncated", c)
	}
	if c := dto.Alternatives[1].Subagents; c == nil || len(c.Pinned) != 32 || !c.PinnedTruncated {
		t.Fatalf("codex subagents = %+v, want 32 pins and truncated", c)
	}
	if len(body) >= 24<<10 {
		t.Errorf("two-branch schedule-preview = %d bytes, want < 24 KiB", len(body))
	}
	t.Logf("two-branch maximum = %d bytes", len(body))

	// Credential material never appears, including a stored pinned token's id and label.
	pin := f.addToken(uuid.New(), "eff-sched-pin-"+uuid.NewString()[:8], false)
	id := f.insertSweep("claude", "", false, "pinned", &pin)
	_, stored := f.schedulePreview(fmt.Sprintf(`{"schedule_id":%q}`, id))
	_, supplied := f.schedulePreview(fmt.Sprintf(`{"target":"sweep","credential_override":{"mode":"pinned","secret_id":%q}}`, pin))
	_, refused := f.schedulePreview(`{"target":"sweep","credential_override":{"mode":"pinned"}}`)
	// The scan must have material to look for, or it passes vacuously: the pinned token's id and
	// label are among it.
	if len(f.secretsToScan) == 0 || !slices.Contains(f.secretsToScan, pin.String()) {
		t.Fatalf("the credential-material scan list = %v, want it non-empty and holding the pinned token id", f.secretsToScan)
	}
	for name, b := range map[string][]byte{"alternatives": body, "stored pin": stored, "supplied pin": supplied, "refusal": refused} {
		for _, secret := range f.secretsToScan {
			if secret != "" && bytes.Contains(b, []byte(secret)) {
				t.Errorf("%s response carries credential material %q", name, secret)
			}
		}
	}
}

// ---- a stored pinned token disabled since the save, on a user who resolves to Codex ----

// ciRedForge reports one failed default-branch workflow execution, enough for a ci_watch fire to
// reach the point where it starts a repair run.
type ciRedForge struct{ forgetest.BaseFake }

func (*ciRedForge) ListWorkflowRuns(context.Context, int64, forge.ListWorkflowRunsOptions) ([]forge.WorkflowRun, error) {
	now := time.Now()
	return []forge.WorkflowRun{{
		ID: 7001, WorkflowKey: "wf-eff", Name: "ci", Event: "push", Branch: "main", SHA: "deadbeef",
		Status: "completed", Conclusion: "failure", WebURL: "https://forge.e2e/runs/7001", CreatedAt: now, UpdatedAt: now,
	}}, nil
}

// insertPinned stores an unpinned-harness schedule of the given target and timing whose stored
// override is a pinned token, for states a save cannot produce. A once schedule is already due.
func (f *effFix) insertPinned(target, timing string, secret uuid.UUID) string {
	f.t.Helper()
	id := uuid.New()
	var prompt, issueIID any
	switch target {
	case "issue":
		f.iid++
		issueIID = f.iid
	default:
		prompt = "eff prompt"
	}
	cron, runAt, next := any("0 * * * *"), any(nil), "now() + interval '1 hour'"
	if timing == "once" {
		cron, runAt, next = nil, time.Now().Add(-time.Hour), "now() - interval '1 minute'"
	}
	f.exec(`INSERT INTO run_schedules (id, user_id, repo_id, target, issue_iid, prompt, timing, cron_expr, run_at, timezone, next_fire_at, auto_approve, status, enabled,
			credential_override_mode, credential_override_secret_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'UTC', `+next+`, true, 'active', true, 'pinned', $10)`,
		id, f.user, f.repo, target, issueIID, prompt, timing, cron, runAt, secret)
	return id.String()
}

// codexOnlyWithDisabledPin is a user whose only usable harness is Codex and whose schedule's stored
// pinned token was disabled after the save.
func codexOnlyWithDisabledPin(f *effFix) uuid.UUID {
	f.t.Helper()
	f.codexDefault()
	pin := f.addToken(uuid.New(), "eff-codex-pin-"+uuid.NewString()[:8], false)
	return pin
}

func TestSchedulePreviewUnpinnedCodexWithDisabledPinLiveDB(t *testing.T) {
	t.Run("a ci_watch schedule has the scheduler's codex_override_conflict pre-check", func(t *testing.T) {
		f := newEffFix(t, nil)
		pin := codexOnlyWithDisabledPin(f)
		id := f.insertPinned("ci_watch", "recurring", pin)
		f.exec(`UPDATE user_secrets SET disabled_at = now() WHERE id = $1`, pin)
		dto, body := f.schedulePreview(fmt.Sprintf(`{"schedule_id":%q}`, id))
		wantHarness(t, dto.Harness, "codex", "automatic")
		if strs(dto.Refusal) != "codex_override_conflict" || dto.WorkerModel != nil || dto.Credential != nil || dto.Subagents != nil {
			t.Fatalf("preview = %s, want codex_override_conflict", body)
		}
		sched := schedsvc.New(f.h.q, f.h.wsvc, sweepGateBuilder{f: &ciRedForge{}}, sweepGateSettings{}, nil, nil, time.Minute, nil)
		row, err := f.h.q.GetRunSchedule(f.ctx, uuid.MustParse(id))
		if err != nil {
			t.Fatal(err)
		}
		out, err := sched.RunNow(f.ctx, row)
		if err != nil {
			t.Fatalf("fire: %v", err)
		}
		var skips []string
		for _, s := range out.Skips {
			skips = append(skips, string(s.Reason))
		}
		if len(out.Started) != 0 || strings.Join(skips, ",") != "codex_override_conflict" {
			t.Fatalf("fire = started %v skips %v, want only the codex_override_conflict skip", out.Started, skips)
		}
	})

	// A one-time schedule runs the credential_disabled pre-check before the fire, so the tick holds
	// it as credential_disabled and never reaches the codex_override_conflict gate.
	for _, target := range []string{"prompt", "issue"} {
		t.Run("a once "+target+" schedule is held as credential_disabled by the tick", func(t *testing.T) {
			f := newEffFix(t, nil)
			pin := codexOnlyWithDisabledPin(f)
			id := f.insertPinned(target, "once", pin)
			f.exec(`UPDATE user_secrets SET disabled_at = now() WHERE id = $1`, pin)
			dto, body := f.schedulePreview(fmt.Sprintf(`{"schedule_id":%q}`, id))
			wantHarness(t, dto.Harness, "codex", "automatic")
			if strs(dto.Refusal) != "credential_disabled" || dto.WorkerModel != nil || dto.Credential != nil || dto.Subagents != nil {
				t.Fatalf("preview = %s, want credential_disabled", body)
			}
			// The real tick: the once schedule is held, not advanced, with the skip recorded.
			sched := schedsvc.New(f.h.q, f.h.wsvc, sweepGateBuilder{f: &sweepGateForge{}}, sweepGateSettings{}, nil, nil, time.Minute, nil)
			sched.Boot(f.ctx)
			var lastFire, status string
			var due bool
			if err := f.pool.QueryRow(f.ctx, `SELECT COALESCE(last_fire::text, ''), status, next_fire_at IS NOT NULL FROM run_schedules WHERE id = $1`, id).Scan(&lastFire, &status, &due); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(lastFire, `"credential_disabled"`) || strings.Contains(lastFire, "codex_override_conflict") || status != "active" || !due {
				t.Fatalf("after the tick: last_fire %s status %s due %v, want the credential_disabled hold", lastFire, status, due)
			}
			var runs int
			if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM runs WHERE user_id = $1`, f.user).Scan(&runs); err != nil || runs != 0 {
				t.Fatalf("the held schedule created %d runs (err %v)", runs, err)
			}
			// The same stored shape recurring is not held: it reaches the conflict gate.
			rec := f.insertPinned(target, "recurring", pin)
			dto, body = f.schedulePreview(fmt.Sprintf(`{"schedule_id":%q}`, rec))
			if strs(dto.Refusal) != "codex_override_conflict" {
				t.Fatalf("recurring preview = %s, want codex_override_conflict", body)
			}
		})
	}
}
