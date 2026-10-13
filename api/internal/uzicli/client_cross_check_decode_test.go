package uzicli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSettingsDecodeCrossCheckPins(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/me/settings" || r.Header.Get("Authorization") != "Bearer uzc_test" {
			t.Errorf("unexpected request %s %s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`{"settings":{"cross_check_pins":[
			{"stage":"plan","harness":"claude","model":null,"effort":"high","worker_default_model":null,"resolved_model":null,"resolved_effort":"high","model_source":"worker default","effort_source":"pin","active":false},
			{"stage":"plan","harness":"codex","model":"custom-checker","effort":null,"worker_default_model":"worker-model","resolved_model":"custom-checker","resolved_effort":"medium","model_source":"pin","effort_source":"worker default","active":true}
		]}}`))
	}))
	defer srv.Close()
	settings, err := newTestClient(srv).GetMySettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(settings.CrossCheckPins) != 2 {
		t.Fatalf("lost cells: %+v", settings)
	}
	claude, codex := settings.CrossCheckPins[0], settings.CrossCheckPins[1]
	if claude.Stage != "plan" || claude.Harness != "claude" || claude.Model != nil ||
		claude.WorkerDefaultModel != nil || claude.ResolvedModel != nil || claude.Active ||
		claude.Effort == nil || *claude.Effort != "high" || claude.ResolvedEffort != "high" ||
		claude.ModelSource != "worker default" || claude.EffortSource != "pin" {
		t.Fatalf("lost nullable inactive Claude contract: %+v", claude)
	}
	if codex.Stage != "plan" || codex.Harness != "codex" || codex.Model == nil || *codex.Model != "custom-checker" ||
		codex.WorkerDefaultModel == nil || *codex.WorkerDefaultModel != "worker-model" ||
		codex.ResolvedModel == nil || *codex.ResolvedModel != "custom-checker" || codex.Effort != nil ||
		codex.ResolvedEffort != "medium" || codex.ModelSource != "pin" || codex.EffortSource != "worker default" || !codex.Active {
		t.Fatalf("lost Codex pin/default contract: %+v", codex)
	}
}

func TestSettingsDecodeFourCellsAndLegacy(t *testing.T) {
	for _, four := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "four"}[four], func(t *testing.T) {
			payload := `[{"stage":"plan","harness":"claude","model":null,"effort":null,"active":true},{"stage":"plan","harness":"codex","model":null,"effort":null,"active":false}`
			if four {
				payload += `,{"stage":"code","harness":"claude","model":null,"effort":"low","resolved_model":null,"resolved_effort":"low","model_source":"worker default","effort_source":"pin","active":false},{"stage":"code","harness":"codex","model":"code-only","effort":null,"worker_default_model":"code-worker","resolved_model":"code-only","resolved_effort":"medium","model_source":"pin","effort_source":"worker default","active":true}`
			}
			payload += "]"
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"settings":{"cross_check_pins":` + payload + `}}`))
			}))
			defer srv.Close()
			s, err := newTestClient(srv).GetMySettings(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			want := 2
			if four {
				want = 4
			}
			if len(s.CrossCheckPins) != want {
				t.Fatalf("cells: %+v", s.CrossCheckPins)
			}
			if four {
				c, d := s.CrossCheckPins[2], s.CrossCheckPins[3]
				if c.Stage != "code" || c.Harness != "claude" || c.Model != nil || c.ResolvedModel != nil || c.Active || c.Effort == nil || *c.Effort != "low" || c.ModelSource != "worker default" || c.EffortSource != "pin" {
					t.Fatalf("Claude: %+v", c)
				}
				if d.Stage != "code" || d.Harness != "codex" || d.Model == nil || *d.Model != "code-only" || d.Effort != nil || d.WorkerDefaultModel == nil || *d.WorkerDefaultModel != "code-worker" || d.ResolvedModel == nil || *d.ResolvedModel != "code-only" || d.ResolvedEffort != "medium" || d.ModelSource != "pin" || d.EffortSource != "worker default" || !d.Active {
					t.Fatalf("Codex: %+v", d)
				}
			}
		})
	}
}

func TestRunDecodeCrossCheckSources(t *testing.T) {
	for _, tc := range []struct {
		name, fields string
	}{
		{"recorded", `,"checker_model_source":"pin","checker_effort_source":"worker default"`},
		{"legacy", ""},
		{"null", `,"checker_model_source":null,"checker_effort_source":null`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/api/runs/r1" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				_, _ = w.Write([]byte(`{"run":{"id":"r1","plan_cross_check_summary":{"checker_model":"recorded-model","checker_effort":"high"` + tc.fields + `}}}`))
			}))
			defer srv.Close()
			run, err := newTestClient(srv).GetRun(context.Background(), "r1")
			if err != nil {
				t.Fatal(err)
			}
			s := run.PlanCrossCheckSummary
			if s == nil || s.CheckerModel == nil || *s.CheckerModel != "recorded-model" ||
				s.CheckerEffort == nil || *s.CheckerEffort != "high" {
				t.Fatalf("lost recorded checker values: %+v", s)
			}
			if tc.name == "recorded" {
				if s.CheckerModelSource == nil || *s.CheckerModelSource != "pin" ||
					s.CheckerEffortSource == nil || *s.CheckerEffortSource != "worker default" {
					t.Fatalf("lost recorded sources: %+v", s)
				}
			} else if s.CheckerModelSource != nil || s.CheckerEffortSource != nil {
				t.Fatalf("invented legacy sources: %+v", s)
			}
		})
	}
}
