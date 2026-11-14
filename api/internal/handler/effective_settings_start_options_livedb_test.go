package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// effective_settings_start_options_livedb_test.go covers the start-time model and effort of the
// start preview (PRD #3250 section 8) against a real Postgres: the preview equals what create
// froze, an unhonorable model is a refusal and never a substitution, and a model choice opts the
// run out of automatic harness selection (D22) while effort alone does not. Skipped unless
// UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via ./e2e/run-store-it.sh.

func (f *effFix) createWithOptions(explicit *workersvc.Harness, model, effort *string) (store.Run, error) {
	f.t.Helper()
	f.iid++
	f.exec(`INSERT INTO issues (id, repo_id, forge_issue_iid, title, state, labels, web_url, has_prd_link, forge_updated_at, synced_at)
		VALUES (gen_random_uuid(), $1, $2, 'eff issue', 'opened', '["uzi"]', 'https://forge.e2e/i', false, now(), now())`, f.repo, f.iid)
	return f.h.wsvc.CreateRun(f.ctx, f.user, f.repo, f.iid, "eff", nil, nil, false, nil, nil, explicit, nil, model, effort)
}

func TestStartPreviewRunOverrideEqualsCreateLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name, body, harness string
		model, effort       *string
	}{
		{"claude model and effort", `{"harness":"claude","model":"opus","effort":"xhigh"}`, "claude", sp("opus"), sp("xhigh")},
		{"codex model and effort", `{"harness":"codex","model":"gpt-6-sol","effort":"max"}`, "codex", sp("gpt-6-sol"), sp("max")},
		{"claude model only", `{"harness":"claude","model":"sonnet"}`, "claude", sp("sonnet"), nil},
		{"codex effort only", `{"harness":"codex","effort":"low"}`, "codex", nil, sp("low")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newEffFix(t, nil)
			f.bothUsable(10, 10)
			dto, body := f.preview(tc.body)
			wantHarness(t, dto.Harness, tc.harness, "run_override")
			if dto.Refusal != nil || len(dto.Alternatives) != 0 {
				t.Fatalf("preview = %s", body)
			}
			run, err := f.createWithOptions(hptr(workersvc.Harness(tc.harness)), tc.model, tc.effort)
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			if run.Harness != tc.harness {
				t.Fatalf("create froze %q, preview promised %q", run.Harness, tc.harness)
			}
			if tc.model == nil {
				if dto.WorkerModel != nil {
					t.Errorf("worker_model = %+v without a requested model", dto.WorkerModel)
				}
			} else {
				m := dto.WorkerModel
				if m == nil || m.Source != "run_override" || m.ResolvedModel != *tc.model || strs(m.Model) != *tc.model || m.Harness != tc.harness {
					t.Fatalf("worker_model = %+v, want run_override %q", m, *tc.model)
				}
				if strs(&run.Model.String) != *tc.model || run.ModelSource.String != m.Source {
					t.Errorf("create froze model %q (%s), preview %q (%s)", run.Model.String, run.ModelSource.String, m.ResolvedModel, m.Source)
				}
			}
			if tc.effort == nil {
				if dto.WorkerEffort != nil {
					t.Errorf("worker_effort = %+v without a requested effort", dto.WorkerEffort)
				}
			} else {
				e := dto.WorkerEffort
				if e == nil || e.Source != "run_override" || e.Mode != "explicit" || strs(e.ResolvedEffort) != *tc.effort || strs(e.Effort) != *tc.effort || e.Harness != tc.harness {
					t.Fatalf("worker_effort = %+v, want explicit run_override %q", e, *tc.effort)
				}
				if run.Effort.String != *tc.effort || run.EffortSource.String != e.Source {
					t.Errorf("create froze effort %q (%s), preview %q (%s)", run.Effort.String, run.EffortSource.String, strs(e.ResolvedEffort), e.Source)
				}
			}
		})
	}
}

func TestStartPreviewUnhonorableModelIsARefusalLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		explicit   *workersvc.Harness
		model      string
	}{
		{"codex role id on claude", `{"harness":"claude","model":"gpt-6-sol"}`, hptr(workersvc.HarnessClaude), "gpt-6-sol"},
		{"claude alias on codex", `{"harness":"codex","model":"opus"}`, hptr(workersvc.HarnessCodex), "opus"},
		{"unknown id on codex", `{"harness":"codex","model":"gpt-foo"}`, hptr(workersvc.HarnessCodex), "gpt-foo"},
		{"codex role id with the harness left to D11", `{"model":"gpt-6-sol"}`, nil, "gpt-6-sol"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newEffFix(t, nil)
			f.bothUsable(10, 10)
			dto, body := f.preview(tc.body)
			if strs(dto.Refusal) != "model_incompatible_with_harness" || dto.RefusalDetail != nil || dto.WorkerModel != nil || dto.WorkerEffort != nil ||
				dto.Credential == nil || dto.Harness == nil || dto.Harness.Value == nil {
				t.Fatalf("preview = %s, want the model refusal on a decided harness and no substituted cell", body)
			}
			_, err := f.createWithOptions(tc.explicit, &tc.model, nil)
			var inc *workersvc.ModelIncompatibleError
			if !errors.As(err, &inc) || inc.Model != tc.model || string(inc.Harness) != *dto.Harness.Value {
				t.Fatalf("create = %v, want ModelIncompatibleError for %q on %s", err, tc.model, *dto.Harness.Value)
			}
		})
	}

	t.Run("an explicit harness answers the model refusal before a credential refusal", func(t *testing.T) {
		f := newEffFix(t, nil)
		f.bothUsable(10, 10)
		f.addToken(uuid.MustParse(corpusDisabledToken), "pre-disabled", true)
		dto, _ := f.preview(`{"harness":"claude","model":"gpt-6-sol","credential_override":{"mode":"pinned","secret_id":"` + corpusDisabledToken + `"}}`)
		if strs(dto.Refusal) != "model_incompatible_with_harness" || dto.Credential != nil || dto.Harness == nil || strs(dto.Harness.Value) != "claude" {
			t.Fatalf("preview = %+v, want the model refusal on claude with no credential cell", dto)
		}
	})
	t.Run("a harness the server decides keeps the credential refusal first", func(t *testing.T) {
		f := newEffFix(t, nil)
		f.bothUsable(10, 10)
		f.addToken(uuid.MustParse(corpusDisabledToken), "pre-disabled", true)
		dto, _ := f.preview(`{"model":"gpt-6-sol","credential_override":{"mode":"pinned","secret_id":"` + corpusDisabledToken + `"}}`)
		if strs(dto.Refusal) != "credential_disabled" {
			t.Fatalf("refusal = %s, want credential_disabled", strs(dto.Refusal))
		}
	})
}

// TestStartPreviewRefusalPrecedenceMatchesCreateRouteLiveDB drives the REAL create route and the
// preview route with the same body and asserts they refuse with the same reason: an explicit
// harness gets the model refusal ahead of any credential refusal, a harness the server decides
// gets the credential refusal ahead of the model refusal.
func TestStartPreviewRefusalPrecedenceMatchesCreateRouteLiveDB(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, body, wantCode, wantRefusal string
		onlyAnthropic, disabledPin        bool
	}{
		{
			name: "explicit codex with a claude alias, owner holds only an Anthropic token",
			body: `{"harness":"codex","model":"opus"}`, wantCode: "model_incompatible_with_harness", wantRefusal: "model_incompatible_with_harness",
			onlyAnthropic: true,
		},
		{
			name:     "explicit claude with a codex id, pinned to a disabled token",
			body:     `{"harness":"claude","model":"gpt-6-sol","credential_override":{"mode":"pinned","secret_id":"DISABLED_ID"}}`,
			wantCode: "model_incompatible_with_harness", wantRefusal: "model_incompatible_with_harness",
			disabledPin: true,
		},
		{
			name:     "server-decided harness, pinned to a disabled token, incompatible model",
			body:     `{"model":"gpt-6-sol","credential_override":{"mode":"pinned","secret_id":"DISABLED_ID"}}`,
			wantCode: "credential_disabled", wantRefusal: "credential_disabled",
			disabledPin: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRunStartFixture(ctx, t)
			cliMustExec(t, f.pool, `UPDATE users SET default_harness = 'claude' WHERE id = $1`, f.owner)
			if tc.onlyAnthropic {
				cliMustExec(t, f.pool, `DELETE FROM user_secrets WHERE user_id = $1 AND kind = 'openai_api_key'`, f.owner)
			}
			body := tc.body
			if tc.disabledPin {
				id := uuid.New()
				cliMustExec(t, f.pool,
					`INSERT INTO user_secrets (id, user_id, kind, label, is_default, ciphertext, sealed_with, disabled_at)
					 VALUES ($1, $2, 'anthropic_token', 'pre-disabled', false, $3, 'master', now())`, id, f.owner, []byte("ct"))
				body = strings.ReplaceAll(body, "DISABLED_ID", id.String())
			}
			token := cliMintToken(t, f.pool, f.owner, clitoken.ScopeUser)
			repoID, path := f.seedIssue(ctx, t, 7)

			var m map[string]any
			if err := json.Unmarshal([]byte(body), &m); err != nil {
				t.Fatal(err)
			}
			m["issue_iid"] = 7
			createBody, _ := json.Marshal(m)
			rec := bearerReqBody(f.router, http.MethodPost, path, token, string(createBody))
			if rec.Code != http.StatusUnprocessableEntity && rec.Code != http.StatusConflict && rec.Code != http.StatusNotFound {
				t.Fatalf("create status = %d, want a refusal (body %s)", rec.Code, rec.Body.String())
			}
			created := decodeErrBody(t, rec.Body.Bytes())
			if tc.wantCode == "model_incompatible_with_harness" && created["code"] != tc.wantCode {
				t.Fatalf("create body = %v, want code %s", created, tc.wantCode)
			}
			if tc.wantCode != "model_incompatible_with_harness" && created["code"] == "model_incompatible_with_harness" {
				t.Fatalf("create body = %v, want the credential refusal, not the model one", created)
			}
			if rows := f.runRows(ctx, t, repoID); len(rows) != 0 {
				t.Errorf("create left %d run rows", len(rows))
			}

			prev := bearerReqBody(f.router, http.MethodPost, "/api/me/effective-settings/start-preview", token, body)
			if prev.Code != http.StatusOK {
				t.Fatalf("preview status = %d (body %s)", prev.Code, prev.Body.String())
			}
			var dto apitypes.StartPreviewDTO
			strictDecode(t, prev.Body.Bytes(), &dto)
			if strs(dto.Refusal) != tc.wantRefusal {
				t.Fatalf("preview refusal = %s, create answered %v; want %s", strs(dto.Refusal), created, tc.wantRefusal)
			}
		})
	}
}

// TestStartPreviewModelOptsOutOfAutomaticHarnessLiveDB is D22 (user decision 2026-11-14): a model
// choice makes the creation origin ineligible for the automatic and overflow policies, so the
// preview resolves to exactly the harness create freezes, with no alternatives; effort alone leaves
// the harness undecided and each alternative carries its own worker_effort. There is deliberately no
// case for an undecided harness whose alternatives differ in model compatibility: a model request is
// never undecided, so that case cannot be built.
func TestStartPreviewModelOptsOutOfAutomaticHarnessLiveDB(t *testing.T) {
	setups := map[string]func(f *effFix){
		"automatic policy": func(f *effFix) { f.bothUsable(95, 10) }, // the readings choose Codex when the policy applies
		"overflow policy": func(f *effFix) {
			f.bothUsable(90, 10)
			f.policy("claude", true)
		},
	}
	for name, setup := range setups {
		t.Run(name, func(t *testing.T) {
			f := newEffFix(t, nil)
			setup(f)

			control, _ := f.preview(`{"harness":null,"credential_override":null}`)
			if control.Harness == nil || control.Harness.Value != nil || len(control.Alternatives) != 2 {
				t.Fatalf("control preview = %+v, want a harness chosen at create", control)
			}
			// The premise, under the same readings: a create without a model is steered to Codex, so
			// the opus create below choosing Claude is the opt-out and not the default.
			noModel, err := f.createWithOptions(nil, nil, nil)
			if err != nil || noModel.Harness != "codex" {
				t.Fatalf("create without a model = %q %v, want a run frozen on codex under these readings", noModel.Harness, err)
			}

			dto, body := f.preview(`{"harness":null,"credential_override":null,"model":"opus"}`)
			if dto.Harness == nil || strs(dto.Harness.Value) != "claude" || dto.Refusal != nil || len(dto.Alternatives) != 0 || dto.Credential == nil {
				t.Fatalf("preview with a model = %s, want exactly claude and no alternatives", body)
			}
			if dto.WorkerModel == nil || dto.WorkerModel.ResolvedModel != "opus" || dto.WorkerModel.Source != "run_override" {
				t.Fatalf("worker_model = %+v", dto.WorkerModel)
			}
			run, err := f.createWithOptions(nil, sp("opus"), nil)
			if err != nil || run.Harness != "claude" {
				t.Fatalf("create with the model = %q %v, want a run frozen on claude", run.Harness, err)
			}

			// Effort alone is still eligible: the harness is undecided and each branch has its own cell.
			eff, body := f.preview(`{"harness":null,"credential_override":null,"effort":"high"}`)
			if eff.Harness == nil || eff.Harness.Value != nil || eff.Harness.Source != "chosen_at_create" || eff.WorkerModel != nil || eff.WorkerEffort != nil || len(eff.Alternatives) != 2 {
				t.Fatalf("effort-only preview = %s, want an undecided harness without top-level cells", body)
			}
			for _, hn := range []string{"claude", "codex"} {
				a := altOf(t, eff, hn)
				e := a.WorkerEffort
				if e == nil || e.Harness != hn || e.Source != "run_override" || e.Mode != "explicit" || strs(e.ResolvedEffort) != "high" || a.Refusal != nil {
					t.Fatalf("%s alternative = %+v, want worker_effort high from run_override", hn, a)
				}
			}
		})
	}
}

// TestStartPreviewWithoutOptionsKeepsTheWireLiveDB: a request with no model or effort has neither
// new key in its response, on a decided harness and on a chosen-at-create one.
func TestStartPreviewWithoutOptionsKeepsTheWireLiveDB(t *testing.T) {
	f := newEffFix(t, nil)
	f.bothUsable(10, 10)
	for _, body := range []string{
		`{"harness":null,"credential_override":null}`,
		`{"harness":"claude","credential_override":null}`,
		`{"harness":"codex","credential_override":null,"model":null,"effort":null}`,
	} {
		_, raw := f.preview(body)
		if bytes.Contains(raw, []byte("worker_model")) || bytes.Contains(raw, []byte("worker_effort")) {
			t.Errorf("%s answered %s, want neither worker_model nor worker_effort", body, raw)
		}
	}
}
