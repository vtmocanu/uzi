package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// detailRows renders `run get` and returns each row's value keyed by its leading label.
func detailRows(t *testing.T, run apitypes.RunDTO) (map[string][]string, string) {
	t.Helper()
	var buf bytes.Buffer
	if err := renderRunDetail(uzicli.NewPrinter(&buf, false, false, true, false), run); err != nil {
		t.Fatal(err)
	}
	rows := map[string][]string{}
	for _, line := range strings.Split(buf.String(), "\n") {
		key, val, ok := strings.Cut(strings.TrimSpace(line), " ")
		if ok {
			rows[key] = append(rows[key], strings.TrimSpace(val))
		}
	}
	return rows, buf.String()
}

func TestCheckpointPublicationWithheldFixtureRenders(t *testing.T) {
	var run apitypes.RunDTO
	readContractFixture(t, "run.checkpoint_publication_withheld.json", &run)
	if run.Health != "publication_withheld" || run.HealthReason == nil || *run.HealthReason != apitypes.ReasonCheckpointPublicationWithheld {
		t.Fatalf("fixture health/reason = %q / %v, want the withheld pair", run.Health, run.HealthReason)
	}
	if run.HealthSince == nil {
		t.Fatal("fixture lacks health_since")
	}
	// Canaries in fields unrelated to health must not reach the health rows.
	canary := "CANARY-path/secret.env CANARY-rule-aws-key CANARY-sha-0123456789abcdef"
	run.IssueDescription = canary
	run.Branch = &canary
	rows, out := detailRows(t, run)
	if got := rows["HEALTH_REASON"]; len(got) != 1 || got[0] != apitypes.ReasonCheckpointPublicationWithheld {
		t.Fatalf("HEALTH_REASON rows = %q", got)
	}
	if got := rows["HEALTH_HINT"]; len(got) != 1 || got[0] != apitypes.HintCheckpointPublicationWithheld {
		t.Fatalf("HEALTH_HINT rows = %q", got)
	}
	if got := rows["HEALTH"]; len(got) != 1 || !strings.HasPrefix(got[0], "publication withheld") {
		t.Fatalf("HEALTH rows = %q, want the explicit label", got)
	}
	for _, key := range []string{"HEALTH", "HEALTH_REASON", "HEALTH_HINT"} {
		if strings.Contains(strings.Join(rows[key], "\n"), "CANARY") {
			t.Fatalf("%s row carries unrelated content:\n%s", key, out)
		}
	}
}

func TestRunHealthHintOnlyForPublicationWithheld(t *testing.T) {
	for _, health := range []string{"ok", "stalled", "slow", "waiting_worker", "approval_idle", "waiting_account_cooldown"} {
		run := apitypes.RunDTO{Status: "running", Health: health}
		if rows, _ := detailRows(t, run); len(rows["HEALTH_HINT"]) != 0 {
			t.Fatalf("health %q printed a HEALTH_HINT row: %q", health, rows["HEALTH_HINT"])
		}
	}
}

func TestRailShowsWithheldReasonAndHintOnRunningRun(t *testing.T) {
	m := codexFloorRunAt(t, 60, false)
	reason := apitypes.ReasonCheckpointPublicationWithheld
	m.detail.run.Status, m.detail.run.Health, m.detail.run.HealthReason = "running", "publication_withheld", &reason
	rail := stripANSI(m.railWaitingLineAt(time.Now()))
	flat := strings.Join(strings.Fields(rail), " ")
	for _, want := range []string{"withheld ▸ checkpoint publication is withheld", "secret-scan restriction", "an eligible finding reaches the lead"} {
		if !strings.Contains(flat, want) {
			t.Fatalf("rail lacks %q:\n%s", want, rail)
		}
	}
	for _, line := range strings.Split(rail, "\n") {
		if visualWidth(line) > laneRailWidth {
			t.Fatalf("rail row too wide: %q", line)
		}
	}
	// A hostile reason on a withheld run is sanitized, and other health values draw nothing.
	hostile := "bad\x1b]0;pwn\x07tail"
	m.detail.run.HealthReason = &hostile
	if got := m.railWaitingLineAt(time.Now()); strings.ContainsAny(got, "\x07") || strings.Contains(got, "\x1b]0") {
		t.Fatalf("rail drew control bytes: %q", got)
	}
	m.detail.run.Health = "ok"
	if got := m.railWaitingLineAt(time.Now()); got != "" {
		t.Fatalf("a healthy running run drew a waiting line: %q", got)
	}
}
