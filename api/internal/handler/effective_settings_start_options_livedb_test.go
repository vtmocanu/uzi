package handler

import (
	"bytes"
	"errors"
	"testing"

	"github.com/google/uuid"

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

	t.Run("a credential refusal keeps precedence over the model refusal", func(t *testing.T) {
		f := newEffFix(t, nil)
		f.bothUsable(10, 10)
		f.addToken(uuid.MustParse(corpusDisabledToken), "pre-disabled", true)
		dto, _ := f.preview(`{"harness":"claude","model":"gpt-6-sol","credential_override":{"mode":"pinned","secret_id":"` + corpusDisabledToken + `"}}`)
		if strs(dto.Refusal) != "credential_disabled" {
			t.Fatalf("refusal = %s, want credential_disabled", strs(dto.Refusal))
		}
	})
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
				if e == nil || e.Harness != hn || e.Source != "run_override" || e.Mode != "explicit" || strs(e.ResolvedEffort) != "high" || a.WorkerModel != nil || a.Refusal != nil {
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
