package handler

import (
	"testing"

	"github.com/google/uuid"
)

// effective_settings_template_effort_livedb_test.go covers the lead-template effort tier (issue
// #3258 N3) in the effective-settings view: an unset preference shows the lead template's effort
// with source template, an owner choice (a level or `runtime`) still wins, and the start preview
// keeps its contract (an effort cell only when the request carries an effort, source run_override).
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via ./e2e/run-store-it.sh.

// leadTemplateEffort installs the delivered lead template with effort pins (""  is NULL).
func (f *effFix) leadTemplateEffort(effort, codexEffort string) {
	f.t.Helper()
	id := uuid.New()
	f.exec(`INSERT INTO agent_templates(id,name,description,prompt_body,scope,user_id,effort,codex_effort)
		VALUES($1,$2,'d','b','user',$3,NULLIF($4,''),NULLIF($5,''))`, id, effLeadName, f.user, effort, codexEffort)
	f.exec(`INSERT INTO agent_template_allocations(template_id,user_id,enabled) VALUES($1,$2,true)`, id, f.user)
}

func strOrEmpty(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func TestEffectiveSettingsTemplateEffortSourceLiveDB(t *testing.T) {
	type cell struct {
		stored, resolved, source, mode string // "" stored/resolved = nil
	}
	for _, tc := range []struct {
		name           string
		claude, codex  string // the preference columns ("" = NULL)
		wantC, wantCdx cell
	}{
		{"unset preference shows the template", "", "",
			cell{"", "high", "template", "explicit"}, cell{"", "low", "template", "explicit"}},
		{"user level wins", "max", "xhigh",
			cell{"max", "max", "user_default", "explicit"}, cell{"xhigh", "xhigh", "user_default", "explicit"}},
		{"runtime wins and resolves to nothing", "runtime", "runtime",
			cell{"runtime", "", "user_default", "runtime_default"}, cell{"runtime", "", "user_default", "runtime_default"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newEffFix(t, nil)
			f.anthropic()
			f.codexDefault()
			f.leadTemplateEffort("high", "low")
			f.set("default_effort", tc.claude)
			f.set("default_codex_effort", tc.codex)
			dto, _ := f.get()
			for harness, want := range map[string]cell{"claude": tc.wantC, "codex": tc.wantCdx} {
				e := effortCell(t, dto.Efforts, "worker_effort", harness)
				if strOrEmpty(e.Effort) != want.stored || strOrEmpty(e.ResolvedEffort) != want.resolved || e.Source != want.source || e.Mode != want.mode {
					t.Errorf("%s worker_effort = stored %q resolved %q source %q mode %q, want %+v", harness, strOrEmpty(e.Effort), strOrEmpty(e.ResolvedEffort), e.Source, e.Mode, want)
				}
				if (e.ResolvedEffort == nil) != (want.resolved == "") {
					t.Errorf("%s resolved_effort nil-ness wrong: %+v", harness, e)
				}
				// Choosing Default (a NULL preference) resolves to the lead template's effort.
				wantDefault := map[string]string{"claude": "high", "codex": "low"}[harness]
				if e.DefaultEffort != wantDefault || e.DefaultSource != "template" {
					t.Errorf("%s default = %q (%s), want the lead template's %s", harness, e.DefaultEffort, e.DefaultSource, wantDefault)
				}
			}
		})
	}
}

// With no template effort the cell is unchanged: the builtin default.
func TestEffectiveSettingsNoTemplateEffortStaysBuiltinLiveDB(t *testing.T) {
	f := newEffFix(t, nil)
	f.anthropic()
	f.leadTemplate("opus", "")
	dto, _ := f.get()
	e := effortCell(t, dto.Efforts, "worker_effort", "claude")
	if e.Source != "builtin" || strs(e.ResolvedEffort) != "medium" || e.Effort != nil {
		t.Fatalf("worker_effort = %+v, want builtin medium", e)
	}
}

// The start preview contract is unchanged: with a lead template effort and no effort in the request
// there is no effort cell; with an effort in the request the cell is the run_override choice, not
// the template's.
func TestStartPreviewEffortOverrideBeatsTemplateEffortLiveDB(t *testing.T) {
	f := newEffFix(t, nil)
	f.bothUsable(10, 10)
	f.leadTemplateEffort("high", "low")

	dto, body := f.preview(`{"harness":"claude"}`)
	if dto.WorkerEffort != nil {
		t.Fatalf("worker_effort = %+v without a requested effort, want none: %s", dto.WorkerEffort, body)
	}

	for _, tc := range []struct{ body, harness, want string }{
		{`{"harness":"claude","effort":"max"}`, "claude", "max"},
		{`{"harness":"codex","effort":"xhigh"}`, "codex", "xhigh"},
	} {
		dto, body := f.preview(tc.body)
		e := dto.WorkerEffort
		if e == nil || e.Source != "run_override" || e.Mode != "explicit" || strs(e.ResolvedEffort) != tc.want || e.Harness != tc.harness {
			t.Fatalf("%s: worker_effort = %+v, want run_override %q: %s", tc.body, e, tc.want, body)
		}
	}
}
