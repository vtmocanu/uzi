package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/runpresentation"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func pauseMarker(seq int32, at time.Time, origin string) apitypes.MessageDTO {
	payload, _ := json.Marshal(map[string]string{"origin": origin})
	return apitypes.MessageDTO{Seq: seq, Kind: "paused", Payload: payload, CreatedAt: at}
}

type pauseReadClient struct {
	*uzicli.FakeClient
	readErr   error
	fullReads int
}

func (c *pauseReadClient) RunLogsPage(ctx context.Context, id string, q uzicli.LogsPageQuery) ([]apitypes.MessageDTO, error) {
	messages, err := c.FakeClient.RunLogsPage(ctx, id, q)
	if c.readErr != nil {
		return nil, c.readErr
	}
	return messages, err
}
func (c *pauseReadClient) RunLogs(ctx context.Context, id string, after int32) ([]apitypes.MessageDTO, error) {
	c.fullReads++
	return c.FakeClient.RunLogs(ctx, id, after)
}

func TestPauseOriginSystemHoldRowsUnchanged(t *testing.T) {
	now := time.Now()
	for _, hold := range []string{holdBudgetExhausted, holdCredentialDisabled, "provider_unavailable", "unknown"} {
		run := apitypes.RunDTO{Status: "paused", HoldReason: &hold, UpdatedAt: now}
		rows := pauseDetailRows(run, now, "lead_park")
		if len(rows) != 1 || len(rows[0]) != 2 || rows[0][0] != "PAUSED" || rows[0][1] != "since "+now.UTC().Format("15:04")+" (0s)" {
			t.Fatalf("hold %q pause rows changed: %v", hold, rows)
		}
	}
}

func TestRunGetPauseOrigin(t *testing.T) {
	at := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, origin, status, hold string
		args                       []string
		readErr                    bool
		reads                      int
		want                       string
	}{
		{"lead", "lead_park", "paused", "", nil, false, 1, "paused by the run's lead"},
		{"owner", "owner", "paused", "", nil, false, 1, "paused by its owner"},
		{"absent", "", "paused", "", nil, false, 1, "paused ·"},
		{"unknown", "alien", "paused", "", nil, false, 1, "paused ·"},
		{"hostile", "owner\x1b", "paused", "", nil, false, 1, "paused ·"},
		{"error", "owner", "paused", "", nil, true, 1, "paused ·"},
		{"json", "lead_park", "paused", "", []string{"--json"}, false, 0, ""},
		{"field", "lead_park", "paused", "", []string{"--field", "status"}, false, 0, "paused"},
		{"hold", "owner", "paused", holdBudgetExhausted, nil, false, 0, ""},
		{"running", "owner", "running", "", nil, false, 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := apitypes.RunDTO{ID: "r1", Kind: "prompt", Status: tc.status, StatusSince: &at, UpdatedAt: at}
			if tc.hold != "" {
				run.HoldReason = &tc.hold
			}
			c := &pauseReadClient{FakeClient: &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{"r1": run}, LogsByID: map[string][]apitypes.MessageDTO{"r1": {pauseMarker(101, at, tc.origin)}}}}
			if tc.readErr {
				c.readErr = errors.New("message read failed")
			}
			args := append([]string{"run", "get", "r1"}, tc.args...)
			out, stderr, code := runCLI(t, fakeEnv(c), args...)
			if code != 0 {
				t.Fatalf("exit %d: %s", code, stderr)
			}
			if len(c.RunLogsPageCalls) != tc.reads || c.fullReads != 0 {
				t.Fatalf("page reads %v full reads %d", c.RunLogsPageCalls, c.fullReads)
			}
			if tc.reads == 1 && c.RunLogsPageCalls[0] != (uzicli.LogsPageQuery{Tail: 100}) {
				t.Fatalf("query %v", c.RunLogsPageCalls)
			}
			if !strings.Contains(out, tc.want) {
				t.Fatalf("missing %q in %s", tc.want, out)
			}
			if tc.want == "paused ·" && strings.Contains(out, "paused by") {
				t.Fatalf("unsupported origin: %s", out)
			}
		})
	}
}

func TestPauseOriginNotice(t *testing.T) {
	at := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, marker, hold, want string
	}{
		{"lead", "lead_park", "", "paused by the run's lead; resume with uzi run resume r1"},
		{"owner", "owner", "", "paused by its owner; resume with uzi run resume r1"},
		{"neutral", "", "", "paused; resume with uzi run resume r1"},
		{"malicious", "owner\x1b[31m\ninjected", "", "paused; resume with uzi run resume r1"},
		{"budget", "lead_park", holdBudgetExhausted, "parked at its time limit; extend: uzi run extend r1 --by 2h"},
		{"credential", "owner", holdCredentialDisabled, ""},
		{"provider", "lead_park", holdProviderUnavailable, ""},
		{"unknown hold", "owner", "unknown", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := apitypes.RunDTO{ID: "r1", Status: "paused", StatusSince: &at}
			messages := []apitypes.MessageDTO{pauseMarker(1, at, tc.marker)}
			origin := runpresentation.PauseOrigin(run, messages, true)
			if tc.hold != "" {
				run.HoldReason = &tc.hold
			}
			got := pausedHoldNotice(run, origin)
			if tc.want != "" && got != tc.want {
				t.Fatalf("notice %q want %q", got, tc.want)
			}
			if tc.hold != "" && got != pausedHoldNotice(run) {
				t.Fatalf("origin changed system hold notice: %q", got)
			}
			if strings.Contains(got, "injected") || strings.ContainsAny(got, "\n\x1b") {
				t.Fatalf("unsafe notice %q", got)
			}
		})
	}
}
