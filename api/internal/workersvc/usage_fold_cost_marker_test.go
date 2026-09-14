package workersvc

import (
	"context"
	"encoding/json"
	"math"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestDeriveUsageCostMarkerMatrix is the fast, DB-free unit of D5's cost-status projection
// (PRD #1332 C4b). It exercises the full marker matrix of deriveUsageCost directly — no
// Postgres, no fold pipeline — and asserts C1's non-metered-zero invariant on every row.
//
// The two load-bearing arms, calibrated by mutation:
//   - Codex + 'metered' MUST carry the emitted (price-table) cost. Break it (return unreported
//     for the metered case) and "codex metered carries the price-table amount" reddens.
//   - Codex + a missing/unknown marker MUST be 'unreported'. Break it (return metered in the
//     default arm) and "codex missing marker is unreported" / "...unknown..." redden.
func TestDeriveUsageCostMarkerMatrix(t *testing.T) {
	cases := []struct {
		name           string
		harness        string
		marker         string
		emittedCostUSD float64
		costPresent    bool
		wantStatus     string
		wantCost       float64
	}{
		// Claude: metered under the provider costUSD, marker IGNORED whatever it says (D5 backward compat).
		{"claude no marker", harnessClaude, "", 0.0123, true, costStatusMetered, 0.0123},
		{"claude subscription marker ignored", harnessClaude, costStatusSubscription, 0.0123, true, costStatusMetered, 0.0123},
		{"claude metered marker", harnessClaude, costStatusMetered, 0.0123, true, costStatusMetered, 0.0123},
		{"claude unreported marker ignored", harnessClaude, costStatusUnreported, 0.0123, true, costStatusMetered, 0.0123},
		{"claude unknown marker ignored", harnessClaude, "flex-tier", 0.0123, true, costStatusMetered, 0.0123},
		// An unexpected harness (impossible under the runs CHECK) falls to the Claude/metered branch.
		{"empty harness falls to metered", "", costStatusUnreported, 0.0123, true, costStatusMetered, 0.0123},
		{"unknown harness falls to metered", "gemini", "", 0.0123, true, costStatusMetered, 0.0123},
		// Codex: HONOR the closed marker.
		{"codex subscription", harnessCodex, costStatusSubscription, 0, true, costStatusSubscription, 0},
		{"codex subscription zeroes an emitted cost", harnessCodex, costStatusSubscription, 9.99, true, costStatusSubscription, 0},
		{"codex metered carries the price-table amount", harnessCodex, costStatusMetered, 5.55, true, costStatusMetered, 5.55},
		{"codex metered zero cost stays metered", harnessCodex, costStatusMetered, 0, true, costStatusMetered, 0},
		{"codex explicit unreported", harnessCodex, costStatusUnreported, 3.33, true, costStatusUnreported, 0},
		{"codex missing marker is unreported", harnessCodex, "", 3.33, true, costStatusUnreported, 0},
		{"codex unknown marker is unreported", harnessCodex, "flex-tier", 3.33, true, costStatusUnreported, 0},
		// m5: a Codex 'metered' marker with an ABSENT or INVALID amount fails safe to unreported/0
		// rather than let numericUSD clamp a bogus figure into a metered row.
		{"codex metered but amount absent is unreported", harnessCodex, costStatusMetered, 0, false, costStatusUnreported, 0},
		{"codex metered negative amount is unreported", harnessCodex, costStatusMetered, -5.5, true, costStatusUnreported, 0},
		{"codex metered NaN amount is unreported", harnessCodex, costStatusMetered, math.NaN(), true, costStatusUnreported, 0},
		{"codex metered +Inf amount is unreported", harnessCodex, costStatusMetered, math.Inf(1), true, costStatusUnreported, 0},
		{"codex metered above storage domain is unreported", harnessCodex, costStatusMetered, maxCostUSD + 1, true, costStatusUnreported, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotStatus, gotCost := deriveUsageCost(c.harness, c.marker, c.emittedCostUSD, c.costPresent)
			if gotStatus != c.wantStatus {
				t.Fatalf("cost_status = %q, want %q", gotStatus, c.wantStatus)
			}
			if d := math.Abs(costFloat(t, gotCost) - c.wantCost); d > 5e-7 {
				t.Fatalf("cost_usd = %v, want %v (delta %v)", costFloat(t, gotCost), c.wantCost, d)
			}
			// C1's run_usage_nonmetered_zero_check: only 'metered' may carry a non-zero cost.
			if gotStatus != costStatusMetered && costFloat(t, gotCost) != 0 {
				t.Fatalf("non-metered status %q carries non-zero cost %v — violates run_usage_nonmetered_zero_check",
					gotStatus, costFloat(t, gotCost))
			}
		})
	}
}

// TestFoldHonorsCodexMarkerThroughFakeStore drives the REAL AppendMessages fold over a
// Codex-harness run and asserts the per-model costStatus decoded from the frame's modelUsage
// lands in the upsert's CostStatus/CostUsd. It proves the two wiring points a pure
// deriveUsageCost test cannot: that resultModelUsage decodes the camelCase `costStatus` key
// off the wire, and that the fold call site passes it through — all without a database. The
// harness on every upsert stays server-derived ('codex'), never taken from the frame.
func TestFoldHonorsCodexMarkerThroughFakeStore(t *testing.T) {
	w := worker()
	fs := &fakeStore{runOwned: store.Run{
		ID:        uuid.New(),
		WorkerID:  pgconv.UUID(w.ID),
		Harness:   harnessCodex,
		SessionID: pgconv.TextOrNull("sess-codex"),
	}}
	svc := New(fs, newBox(t), testParams())

	// Three of these models carry a malformed/absent cost field: codex-badstatus has a
	// non-string costStatus (`false`), codex-badcost a non-numeric costUSD (`"lots"`) under a
	// metered marker, and codex-nullcost a JSON `null` costUSD (the 4-byte literal, NOT an
	// omitted key) under a metered marker. Before m4 decoded costStatus/costUSD as
	// json.RawMessage, badstatus/badcost would fail the frame's json.Unmarshal and drop EVERY
	// model's usage; the null-costUSD case additionally exercises resolveCostUSD's null path —
	// json.Unmarshal([]byte("null"), &float64) is a no-op leaving 0, so before Fix 1 it resolved
	// to (0, present=true) and fabricated a metered $0 row. The assertions below prove the valid
	// siblings STILL persist and each malformed/absent model resolves to unreported/0.
	msgs := []IncomingMessage{{Seq: 1, Kind: "status", Agent: "lead", Payload: json.RawMessage(`{
		"event":"result",
		"modelUsage":{
			"gpt-6-astra":{"inputTokens":2000,"outputTokens":800,"costUSD":5.55,"costStatus":"metered"},
			"gpt-5.6-sol":{"inputTokens":1000,"outputTokens":400,"costUSD":0,"costStatus":"subscription"},
			"codex-missing":{"inputTokens":10,"outputTokens":5,"costUSD":3.33},
			"codex-badstatus":{"inputTokens":20,"outputTokens":8,"costUSD":1.11,"costStatus":false},
			"codex-badcost":{"inputTokens":30,"outputTokens":12,"costUSD":"lots","costStatus":"metered"},
			"codex-nullcost":{"inputTokens":40,"outputTokens":16,"costUSD":null,"costStatus":"metered"}
		}}`)}}
	if err := svc.AppendMessages(context.Background(), w, fs.runOwned.ID, msgs); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}

	byModel := map[string]store.UpsertRunUsageParams{}
	for _, u := range fs.upsertedUsage {
		byModel[u.Model] = u
		if u.Harness != harnessCodex {
			t.Fatalf("model %s harness = %q, want codex (server-derived)", u.Model, u.Harness)
		}
	}
	check := func(model, wantStatus string, wantCost float64) {
		t.Helper()
		u, ok := byModel[model]
		if !ok {
			t.Fatalf("no upsert for model %q", model)
		}
		if u.CostStatus != wantStatus {
			t.Fatalf("model %s cost_status = %q, want %q", model, u.CostStatus, wantStatus)
		}
		if d := math.Abs(costFloat(t, u.CostUsd) - wantCost); d > 5e-7 {
			t.Fatalf("model %s cost_usd = %v, want %v", model, costFloat(t, u.CostUsd), wantCost)
		}
	}
	check("gpt-6-astra", costStatusMetered, 5.55) // metered marker → agent price-table amount
	check("gpt-5.6-sol", costStatusSubscription, 0)
	check("codex-missing", costStatusUnreported, 0)   // no marker → unreported, cost zeroed
	check("codex-badstatus", costStatusUnreported, 0) // non-string costStatus → invalid marker → unreported
	check("codex-badcost", costStatusUnreported, 0)   // metered marker + non-numeric costUSD → unreported (m5)
	check("codex-nullcost", costStatusUnreported, 0)  // metered marker + JSON null costUSD → absent → unreported (Fix 1)
}

// TestResolveCostUSD pins the raw-JSON cost decode, including the JSON `null` path Fix 1
// closed: a `null` literal is the 4-byte RawMessage `null` (len 4, non-nil), and
// json.Unmarshal([]byte("null"), &float64) is a no-op that returns nil and leaves the value
// at 0 — so before Fix 1 it resolved to (0, present=true) and fed deriveUsageCost a fabricated
// metered $0. It must resolve to (0, present=false) — ABSENT — exactly like an omitted key.
func TestResolveCostUSD(t *testing.T) {
	cases := []struct {
		name        string
		raw         string // "" means a nil/empty RawMessage
		wantValue   float64
		wantPresent bool
	}{
		{"empty is absent", "", 0, false},
		{"json null is absent", "null", 0, false},
		{"json null with whitespace is absent", " null ", 0, false},
		{"finite number is present", "5.55", 5.55, true},
		{"zero is present", "0", 0, true},
		{"negative finite stays present", "-5.5", -5.5, true}, // deriveUsageCost's <0 guard rejects it
		{"non-numeric string is absent", `"lots"`, 0, false},
		{"bool is absent", "false", 0, false},
		{"object is absent", "{}", 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var raw json.RawMessage
			if c.raw != "" {
				raw = json.RawMessage(c.raw)
			}
			gotValue, gotPresent := resolveCostUSD(raw)
			if gotPresent != c.wantPresent {
				t.Fatalf("resolveCostUSD(%q) present = %v, want %v", c.raw, gotPresent, c.wantPresent)
			}
			if math.Abs(gotValue-c.wantValue) > 5e-7 {
				t.Fatalf("resolveCostUSD(%q) value = %v, want %v", c.raw, gotValue, c.wantValue)
			}
		})
	}
}
