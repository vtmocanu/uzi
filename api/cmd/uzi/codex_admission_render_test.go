package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func TestCodexAdmissionCLIConsumers(t *testing.T) {
	at := time.Date(2099, 1, 2, 3, 4, 5, 123456789, time.FixedZone("fixture", 3600))
	for _, tc := range []struct{ class, copy string }{
		{"rate_limit", "waiting: provider reported a rate limit; account admission eligible after 2099-01-02T02:04:05.123456789Z"},
		{"overload", "waiting: provider reported overload; account admission eligible after 2099-01-02T02:04:05.123456789Z"},
	} {
		t.Run(tc.class, func(t *testing.T) {
			run := apitypes.RunDTO{ID: "admission", Kind: "issue", Status: "queued", CodexAdmissionClass: &tc.class, CodexAdmissionEligibleAt: &at, QueueReason: sp("maintenance"), HealthReason: sp("waiting: uzi is in maintenance mode")}
			var detail bytes.Buffer
			if err := renderRunDetail(uzicli.NewPrinter(&detail, false, false, true, false), run); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(detail.String(), tc.copy) || !strings.Contains(detail.String(), "maintenance mode") {
				t.Fatalf("detail: %s", &detail)
			}
			srv := newListViewServer(t, []apitypes.RunListItemDTO{{RunDTO: run}})
			for _, args := range [][]string{{"run", "list"}, {"admin", "runs"}} {
				out, errs, code := runCLI(t, httpEnv(srv.Server), args...)
				if code != 0 || !strings.Contains(out, tc.copy) || !strings.Contains(out, "maintenance") {
					t.Fatalf("%v exit=%d stderr=%s output=%s", args, code, errs, out)
				}
			}
			m := waitingRailModel(t, func(m *tuiModel) { m.detail.run = run })
			rail := stripANSI(m.railWaitingLine())
			joined := strings.Join(strings.Fields(rail), "")
			for _, want := range []string{strings.ReplaceAll(tc.class, "_", " "), "2099-01-02T02:04:05.123456789Z"} {
				// The rail may wrap inside the timestamp at its minimum width.
				if !strings.Contains(joined, strings.ReplaceAll(want, " ", "")) {
					t.Fatalf("rail missing %q: %s", want, rail)
				}
			}
		})
	}
}

func TestCodexAdmissionQueuedBlockerPriority(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	unavailable := "your Codex account is unavailable (quarantined or being re-linked), so this run can't start"
	for _, class := range []string{"rate_limit", "overload"} {
		for _, tc := range []struct {
			name, status, queue, health, want string
		}{
			{"queued account unavailable", "queued", "codex_" + class, unavailable, "queued (" + unavailable + ")"},
			{"queued handoff", "queued", "codex_" + class, "handoff setup has not finished", "queued (handoff setup has not finished)"},
			{"queued vault", "queued", "codex_" + class, "your vault is locked, so this run can't start", "queued (your vault is locked, so this run can't start)"},
			{"queued custody", "queued", "codex_" + class, "you have too much unpublished work awaiting recovery; resolve or discard some recovery archives to start new runs", "queued (you have too much unpublished work awaiting recovery; resolve or discard some recovery archives to start new runs)"},
			{"queued custody unavailable", "queued", "codex_" + class, "new runs are paused: the custody hold limit setting could not be read; an admin should check Admin → Instance settings", "queued (new runs are paused: the custody hold limit setting could not be read; an admin should check Admin → Instance settings)"},
			{"maintenance coexistence", "queued", "maintenance", unavailable, "queued (maintenance (uzi is in maintenance mode))"},
			{"recovery wait", "recovery_wait", "codex_" + class, unavailable, "recovery_wait (Codex reconciling)"},
			{"untyped provider text", "queued", "codex_" + class, "provider reported overload", "queued"},
		} {
			for _, delta := range []time.Duration{time.Minute, 0, -time.Nanosecond} {
				t.Run(class+"/"+tc.name+"/"+delta.String(), func(t *testing.T) {
					at := now.Add(delta)
					run := apitypes.RunDTO{Status: tc.status, QueueReason: sp(tc.queue), HealthReason: sp(tc.health)}
					if tc.name != "untyped provider text" {
						run.CodexAdmissionClass, run.CodexAdmissionEligibleAt = &class, &at
					}
					if tc.status == "recovery_wait" {
						run.RecoveryWaitCause = sp(codexAccountUnavailableCause)
						run.CodexAccountAction = sp(codexActionReconciling)
					}
					want := tc.want
					if strings.HasPrefix(tc.name, "queued ") && tc.name != "queued account unavailable" && delta <= 0 {
						// The existing compact list only adds these blockers alongside active admission.
						want = "queued"
					}
					if tc.status == "queued" && delta > 0 && tc.name != "untyped provider text" {
						copy := "waiting: provider reported overload; account admission eligible after 2026-10-27T12:01:00Z"
						if class == "rate_limit" {
							copy = "waiting: provider reported a rate limit; account admission eligible after 2026-10-27T12:01:00Z"
						}
						want += " (" + copy + ")"
					}
					if got := runStatusCellAt(apitypes.RunListItemDTO{RunDTO: run}, now); got != want {
						t.Fatalf("status = %q, want %q", got, want)
					}
				})
			}
		}
	}
}

func TestCodexAdmissionQueuedAccountCLIConsumers(t *testing.T) {
	at := time.Date(2099, 1, 2, 3, 4, 5, 0, time.UTC)
	unavailable := "your Codex account is unavailable (quarantined or being re-linked), so this run can't start"
	for _, class := range []string{"rate_limit", "overload"} {
		t.Run(class, func(t *testing.T) {
			run := apitypes.RunDTO{ID: "admission", Kind: "issue", Status: "queued", HealthReason: &unavailable, QueueReason: sp("codex_" + class), CodexAdmissionClass: &class, CodexAdmissionEligibleAt: &at}
			srv := newListViewServer(t, []apitypes.RunListItemDTO{{RunDTO: run}})
			for _, args := range [][]string{{"run", "list"}, {"admin", "runs"}} {
				out, errs, code := runCLI(t, httpEnv(srv.Server), args...)
				blocker, admission := strings.Index(out, unavailable), strings.Index(out, "waiting: provider reported")
				if code != 0 || blocker < 0 || admission <= blocker || !strings.Contains(out, "2099-01-02T03:04:05Z") {
					t.Fatalf("%v exit=%d stderr=%s output=%s", args, code, errs, out)
				}
			}
		})
	}
}

func TestCodexAdmissionReadTimeRendering(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	future, past := now.Add(time.Minute), now.Add(-time.Nanosecond)
	for _, class := range []string{"rate_limit", "overload"} {
		for _, tc := range []struct {
			name, status string
			class        *string
			at           *time.Time
			active       bool
		}{
			{"future", "queued", &class, &future, true},
			{"equal", "queued", &class, &now, false},
			{"past", "queued", &class, &past, false},
			{"running", "running", &class, &future, false},
			{"account unavailable", "recovery_wait", &class, &future, false},
			{"missing class", "queued", nil, &future, false},
			{"missing time", "queued", &class, nil, false},
			{"unknown class", "queued", sp("future_provider_class"), &future, false},
			{"hostile class", "queued", sp(hostileANSI), &future, false},
			{"owner redacted", "queued", nil, nil, false},
		} {
			t.Run(class+"/"+tc.name, func(t *testing.T) {
				// A persisted reason cannot activate an omitted, expired or unknown typed field.
				stale := "waiting: provider reported overload; account admission eligible after 2020-01-01T00:00:00Z"
				run := apitypes.RunDTO{ID: "admission", Status: tc.status, Health: "waiting_worker", HealthReason: &stale, QueueReason: sp("codex_" + class), CodexAdmissionClass: tc.class, CodexAdmissionEligibleAt: tc.at}
				if tc.name == "account unavailable" {
					run.RecoveryWaitCause = sp(codexAccountUnavailableCause)
					run.CodexAccountAction = sp(codexActionReconciling)
				}
				var detail bytes.Buffer
				if err := renderRunDetailAt(uzicli.NewPrinter(&detail, false, false, true, false), run, now); err != nil {
					t.Fatal(err)
				}
				m := waitingRailModel(t, func(m *tuiModel) { m.detail.run = run })
				outputs := map[string]string{"detail": detail.String(), "list": runStatusCellAt(apitypes.RunListItemDTO{RunDTO: run}, now), "rail": stripANSI(m.railWaitingLineAt(now))}
				for name, out := range outputs {
					if strings.Contains(out, "provider reported") != tc.active || strings.Contains(out, "2020-01-01") || strings.Contains(out, "codex_"+class) {
						t.Fatalf("%s active=%v output=%s", name, tc.active, out)
					}
					if tc.active {
						compact := strings.Join(strings.Fields(out), "")
						for _, want := range []string{strings.ReplaceAll(class, "_", ""), "2026-10-27T12:01:00Z"} {
							if !strings.Contains(compact, want) {
								t.Fatalf("%s missing %q: %s", name, want, out)
							}
						}
					}
				}
				if tc.name == "account unavailable" && (!strings.Contains(outputs["detail"], "Codex account is reconciling") || !strings.Contains(outputs["list"], "Codex reconciling")) {
					t.Fatalf("account priority lost: %v", outputs)
				}
			})
		}
	}
}

func TestCodexAdmissionRailBoundsAndProfiles(t *testing.T) {
	at := time.Date(2099, 1, 2, 3, 4, 5, 123456789, time.UTC)
	for _, class := range []string{"rate_limit", "overload"} {
		for _, profile := range []colorprofile.Profile{colorprofile.TrueColor, colorprofile.Ascii, colorprofile.NoTTY} {
			m := waitingRailModel(t, func(m *tuiModel) {
				m.detail.run.CodexAdmissionClass = &class
				m.detail.run.CodexAdmissionEligibleAt = &at
			})
			next, _ := m.Update(tea.ColorProfileMsg{Profile: profile})
			m = next.(tuiModel)
			rail := stripANSI(m.railWaitingLine())
			if !strings.Contains(rail, "previous worker") {
				t.Fatalf("lost existing waiting reason: %s", rail)
			}
			if strings.Count(rail, "\n") > railWaitingMaxRows+8 {
				t.Fatalf("unbounded rail: %s", rail)
			}
			for _, line := range strings.Split(rail, "\n") {
				if visualWidth(line) > m.railWidth() {
					t.Fatalf("rail overflow: %q", line)
				}
			}
			view := stripANSI(m.View().Content)
			// Check each wrapped row in the composed view: transcript columns separate rail rows.
			for _, line := range strings.Split(strings.TrimSpace(rail), "\n") {
				if !strings.Contains(view, strings.TrimSpace(line)) {
					t.Fatalf("profile %v lost rail row %q in composed view: %s", profile, line, view)
				}
			}
			if !strings.Contains(rail, "provider reported") || !strings.Contains(strings.Join(strings.Fields(rail), ""), "2099-01-02T03:04:05.123456789Z") {
				t.Fatalf("profile %v lost admission: %s", profile, rail)
			}
		}
	}
}

func TestCodexAdmissionCreatedRunClearsStaleWarning(t *testing.T) {
	stale := "waiting: provider reported a rate limit; account admission eligible after 2020-01-01T00:00:00Z"
	var out, errs bytes.Buffer
	err := renderCreatedRun(Env{Stdout: &out, Stderr: &errs}, &globalFlags{}, apitypes.RunDTO{ID: "redacted", Status: "queued", HealthReason: &stale}, "")
	if err != nil || strings.Contains(out.String(), "provider reported") || strings.Contains(errs.String(), "provider reported") {
		t.Fatalf("err=%v stdout=%s stderr=%s", err, &out, &errs)
	}
}

func TestCodexAdmissionSummaryConsumers(t *testing.T) {
	at := time.Date(2099, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, admin := range []bool{false, true} {
		items := []apitypes.RunListItemDTO{
			{RunDTO: apitypes.RunDTO{ID: "own", Status: "queued", CodexAdmissionClass: sp("overload"), CodexAdmissionEligibleAt: &at}},
			{RunDTO: apitypes.RunDTO{ID: "foreign", Status: "queued"}},
		}
		srv := newListViewServer(t, items)
		client := &uzicli.HTTPClient{BaseURL: srv.URL, Token: "uzc_test", HTTP: srv.Client()}
		model := newTUIModel(context.Background(), client, "")
		msg, ok := model.fetchRunsCmd(admin, 7)().(boardRunsMsg)
		if !ok || msg.err != nil || len(msg.runs) != 2 {
			t.Fatalf("summary fetch=%#v", msg)
		}
		own, foreign := msg.runs[0], msg.runs[1]
		if own.CodexAdmissionClass == nil || *own.CodexAdmissionClass != "overload" || own.CodexAdmissionEligibleAt == nil || !own.CodexAdmissionEligibleAt.Equal(at) {
			t.Fatalf("admin=%v summary lost owner admission: %+v", admin, own)
		}
		if foreign.CodexAdmissionClass != nil || foreign.CodexAdmissionEligibleAt != nil || strings.Contains(runStatusCell(foreign), "provider reported") {
			t.Fatalf("admin=%v redacted summary invented admission: %+v", admin, foreign)
		}
	}
}
