package workersvc

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestProviderGatewayRetrySchedule pins the approved gateway-limit schedule (PRD #4320 module 5,
// P5/P11): the value of the streak BEFORE the park selects the row, and the fourth park onward has no
// retry time at all (the owner Resumes or Cancels).
func TestProviderGatewayRetrySchedule(t *testing.T) {
	cases := []struct {
		streak int32
		want   time.Duration // 0 = NULL, owner wait
	}{
		{0, time.Minute}, {1, 5 * time.Minute}, {2, 15 * time.Minute}, {3, 0}, {4, 0}, {100, 0},
	}
	for _, c := range cases {
		got, ok := providerGatewayRetryNotBefore(c.streak, parkNow)
		if c.want == 0 {
			if ok || !got.IsZero() {
				t.Errorf("streak %d: got (%v, %v), want the owner-wait row", c.streak, got, ok)
			}
			continue
		}
		if !ok || !got.Equal(parkNow.Add(c.want)) {
			t.Errorf("streak %d: got (%v, %v), want %v", c.streak, got, ok, parkNow.Add(c.want))
		}
	}
	if _, ok := providerGatewayRetryNotBefore(-1, parkNow); ok {
		t.Error("a negative streak must not index the schedule")
	}
}

// TestDecideProviderLimitParkIgnoresTheReport: whatever the worker reports, a provider run's park is
// provider_gateway_limit with no reset; only the run's own opt-out fails it.
func TestDecideProviderLimitParkIgnoresTheReport(t *testing.T) {
	d := decideProviderLimitPark(true, 0, parkNow)
	if !d.Park || d.RateLimitType == nil || *d.RateLimitType != RateLimitTypeProviderGatewayLimit || d.LimitResetsAt != nil {
		t.Fatalf("decision = %+v", d)
	}
	off := decideProviderLimitPark(false, 0, parkNow)
	if off.Park || off.Reason == "" {
		t.Fatalf("opt-out decision = %+v, want a server-composed failure", off)
	}
}

// TestVendorRunCannotStoreTheProviderGatewayType: the literal belongs to provider runs; a vendor park
// reporting it stores unknown, so the web never offers an owner Resume the server refuses.
func TestVendorRunCannotStoreTheProviderGatewayType(t *testing.T) {
	in := parkIn()
	in.ReportedType = strPtr(RateLimitTypeProviderGatewayLimit)
	d := decideLimitPark(in)
	if !d.Park || d.RateLimitType == nil || *d.RateLimitType != RateLimitTypeUnknown {
		t.Fatalf("decision type = %v, want unknown", d.RateLimitType)
	}
}

// TestProviderRunUsageCostIsAlwaysUnreported: the cost lane of a provider-bound run resolves to
// unreported with cost 0 whatever the worker claims, while vendor lanes are unchanged.
func TestProviderRunUsageCostIsAlwaysUnreported(t *testing.T) {
	status, usd := deriveUsageCost(costLaneProviderUnpriced, costStatusMetered, 12.5, true)
	if status != costStatusUnreported || numericToFloat64(usd) != 0 {
		t.Fatalf("provider lane = (%s, %v), want (unreported, 0)", status, usd)
	}
	if status, _ := deriveUsageCost(harnessClaude, "", 1.5, true); status != costStatusMetered {
		t.Fatalf("vendor claude lane = %s, want metered (unchanged)", status)
	}
	// A progress-note entry with no provider cost is table-priced for a vendor run, never for a provider one.
	var mu progressNoteModelUsage
	mu.InputTokens, mu.OutputTokens = 1000, 1000
	if status, _ := resolveProgressNoteCost(costLaneProviderUnpriced, "claude-sonnet-4-5", mu); status != costStatusUnreported {
		t.Fatalf("provider note cost = %s, want unreported", status)
	}
}

// TestProviderRunUsageReportsRecordUnreportedThroughTheAppendPath drives the real AppendMessages
// fold: a provider-bound run's result frame (with and without a provider-reported cost) and its
// progress note record cost_status 'unreported' with cost 0 and keep their tokens, while the same
// frames on a vendor run stay metered.
func TestProviderRunUsageReportsRecordUnreportedThroughTheAppendPath(t *testing.T) {
	frames := map[string]IncomingMessage{
		"result with a gateway cost": {Seq: 1, Kind: "status", Agent: "worker", Payload: json.RawMessage(
			`{"event":"result","modelUsage":{"corp-sonnet":{"inputTokens":120,"outputTokens":30,"costUSD":0.42}}}`)},
		"result without a cost": {Seq: 2, Kind: "status", Agent: "worker", Payload: json.RawMessage(
			`{"event":"result","modelUsage":{"corp-sonnet":{"inputTokens":120,"outputTokens":30}}}`)},
		"progress note priced by the table for a vendor": {Seq: 3, Kind: "progress_note", Agent: "worker", Payload: json.RawMessage(
			`{"text":"x","milestone_id":"m1","model_usage":{"claude-haiku-4-5-20251001":{"inputTokens":400,"outputTokens":40}}}`)},
	}
	for name, msg := range frames {
		for _, provider := range []bool{true, false} {
			w := worker()
			run := store.Run{ID: uuid.New(), WorkerID: pgconv.UUID(w.ID), Harness: "claude", Kind: "issue"}
			if provider {
				run.ModelProviderKeyMode = pgtype.Text{String: "user_key", Valid: true}
			}
			fs := &fakeStore{runOwned: run}
			svc := New(fs, newBox(t), testParams())
			if err := svc.AppendMessages(context.Background(), w, run.ID, []IncomingMessage{msg}); err != nil {
				t.Fatal(err)
			}
			if len(fs.upsertedUsage) != 1 {
				t.Fatalf("%s (provider=%v): %d usage upserts, want 1", name, provider, len(fs.upsertedUsage))
			}
			row := fs.upsertedUsage[0]
			if row.Harness != "claude" || row.InputTokens == 0 {
				t.Fatalf("%s (provider=%v): row harness=%s input=%d, want the real harness and the tokens kept", name, provider, row.Harness, row.InputTokens)
			}
			if provider && (row.CostStatus != costStatusUnreported || numericToFloat64(row.CostUsd) != 0) {
				t.Errorf("%s: provider run recorded %s/%v, want unreported/0", name, row.CostStatus, row.CostUsd)
			}
			if !provider && row.CostStatus != costStatusMetered {
				t.Errorf("%s: vendor run recorded %s, want metered (unchanged)", name, row.CostStatus)
			}
		}
	}
}
