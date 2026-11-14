package handler

import (
	"math/big"
	"math/rand"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

func usageRow(model string, leg, lineage int32, basis string, gen *int64, ord *int32, in, out int64, micros int64) store.ListRunUsageByCredentialEpochRow {
	r := store.ListRunUsageByCredentialEpochRow{
		Model: model, LineageEpoch: leg, LineageIndex: lineage, UsageBasis: basis,
		InputTokens: in, OutputTokens: out, CostUsd: pgtype.Numeric{Int: big.NewInt(micros), Exp: -6, Valid: true},
	}
	if gen != nil {
		r.ClaimGeneration = pgtype.Int8{Int64: *gen, Valid: true}
	}
	if ord != nil {
		r.CredentialEpochOrdinal = pgtype.Int4{Int32: *ord, Valid: true}
	}
	return r
}

func g64(v int64) *int64 { return &v }
func o32(v int32) *int32 { return &v }

func TestTelescopeCredentialEpochUsagePerLegSumsAndUnattributed(t *testing.T) {
	rows := []store.ListRunUsageByCredentialEpochRow{
		usageRow("m", 1, 0, "per_leg", g64(1), nil, 100, 10, 500000),
		usageRow("m", 2, 0, "per_leg", g64(1), o32(1), 70, 7, 100000),
		usageRow("m", 3, 0, "per_leg", g64(1), o32(2), 30, 3, 50000),
		usageRow("m", 4, 0, "per_leg", g64(1), o32(-1), 20, 2, 20000),
	}
	known := map[epochKey]bool{{1, 0}: true, {1, 1}: true, {1, 2}: true}
	got, un := telescopeCredentialEpochUsage(rows, known)
	want := map[epochKey]usageVec{
		{1, 0}: {100, 10, 0, 0, 500000},
		{1, 1}: {70, 7, 0, 0, 100000},
		{1, 2}: {30, 3, 0, 0, 50000},
	}
	for k, w := range want {
		if got[k] != w {
			t.Fatalf("epoch %v = %+v, want %+v", k, got[k], w)
		}
	}
	if un != (usageVec{20, 2, 0, 0, 20000}) {
		t.Fatalf("unattributed = %+v", un)
	}
}

func TestTelescopeCredentialEpochUsageCumulativeLegsAreNotDoubleCounted(t *testing.T) {
	// One session: a per_leg leg of 100, then a session_cumulative reading of 250 that already
	// contains it. The run total is 250; the epochs split it 100 / 150, never 100 / 250.
	rows := []store.ListRunUsageByCredentialEpochRow{
		usageRow("m", 1, 0, "per_leg", g64(1), nil, 100, 0, 0),
		usageRow("m", 2, 0, "session_cumulative", g64(1), o32(1), 250, 0, 0),
	}
	known := map[epochKey]bool{{1, 0}: true, {1, 1}: true}
	got, un := telescopeCredentialEpochUsage(rows, known)
	if got[epochKey{1, 0}].input != 100 || got[epochKey{1, 1}].input != 150 || un.input != 0 {
		t.Fatalf("epochs = %+v / %+v, unattributed %+v, want 100 / 150 / 0", got[epochKey{1, 0}], got[epochKey{1, 1}], un)
	}
	if total := foldUsageRows(rows); total.input != 250 {
		t.Fatalf("total = %+v, want 250", total)
	}
}

func TestTelescopeCredentialEpochUsageOrphansJoinUnattributed(t *testing.T) {
	rows := []store.ListRunUsageByCredentialEpochRow{
		usageRow("m", 1, 0, "per_leg", nil, nil, 5, 0, 0),        // a legacy leg with no generation
		usageRow("m", 2, 0, "per_leg", g64(1), nil, 100, 0, 0),   // the base epoch
		usageRow("m", 3, 0, "per_leg", g64(1), o32(4), 40, 0, 0), // an ordinal with no journal row
	}
	got, un := telescopeCredentialEpochUsage(rows, map[epochKey]bool{{1, 0}: true})
	if got[epochKey{1, 0}].input != 100 || un.input != 45 {
		t.Fatalf("base = %+v unattributed = %+v, want 100 and 45", got[epochKey{1, 0}], un)
	}
}

// TestTelescopeCredentialEpochUsageSumInvariant: for random leg sets, the epoch parts plus the
// unattributed bucket equal the full-run total, and no part is negative.
func TestTelescopeCredentialEpochUsageSumInvariant(t *testing.T) {
	rng := rand.New(rand.NewSource(4201)) //nolint:gosec // G404: a fixed-seed generator for a reproducible property test, not a security use.
	for iter := 0; iter < 400; iter++ {
		var rows []store.ListRunUsageByCredentialEpochRow
		known := map[epochKey]bool{}
		legs := 1 + rng.Intn(8)
		for leg := 1; leg <= legs; leg++ {
			model := []string{"a", "b"}[rng.Intn(2)]
			lineage := rng.Int31n(2)
			basis := "per_leg"
			if rng.Intn(3) == 0 {
				basis = "session_cumulative"
			}
			var gen *int64
			if rng.Intn(10) != 0 {
				gen = g64(int64(1 + rng.Intn(2)))
			}
			var ord *int32
			switch rng.Intn(5) {
			case 0:
			case 1:
				ord = o32(-1)
			default:
				ord = o32(rng.Int31n(3))
			}
			rows = append(rows, usageRow(model, int32(leg), lineage, basis, gen, ord, int64(rng.Intn(1000)), int64(rng.Intn(100)), int64(rng.Intn(1000000))))
			if gen != nil && (ord == nil || *ord >= 0) {
				o := int32(0)
				if ord != nil {
					o = *ord
				}
				known[epochKey{*gen, o}] = rng.Intn(4) != 0 // some attributed keys have no journal row
			}
		}
		parts, un := telescopeCredentialEpochUsage(rows, known)
		sum := un
		nonNeg := func(v usageVec, what string) {
			if v.input < 0 || v.output < 0 || v.cacheRead < 0 || v.cacheCreation < 0 || v.costMicros < 0 {
				t.Fatalf("iter %d: %s is negative: %+v", iter, what, v)
			}
		}
		nonNeg(un, "unattributed")
		for k, v := range parts {
			nonNeg(v, "epoch")
			_ = k
			sum = sum.plus(v)
		}
		if total := foldUsageRows(rows); sum != total {
			t.Fatalf("iter %d: parts %+v != run total %+v (rows %+v)", iter, sum, total, rows)
		}
	}
}

func TestNumericToMicros(t *testing.T) {
	for _, tc := range []struct {
		n    pgtype.Numeric
		want int64
	}{
		{pgtype.Numeric{Int: big.NewInt(1500000), Exp: -6, Valid: true}, 1500000},
		{pgtype.Numeric{Int: big.NewInt(15), Exp: -1, Valid: true}, 1500000},
		{pgtype.Numeric{Int: big.NewInt(2), Exp: 0, Valid: true}, 2000000},
		{pgtype.Numeric{Int: big.NewInt(12345678), Exp: -8, Valid: true}, 123457},
		{pgtype.Numeric{}, 0},
	} {
		if got := numericToMicros(tc.n); got != tc.want {
			t.Fatalf("numericToMicros(%v e%d) = %d, want %d", tc.n.Int, tc.n.Exp, got, tc.want)
		}
	}
}

func TestCredentialEpochsToDTOMergesAndAttachesUsage(t *testing.T) {
	run := uuid.New()
	secret := pgtype.UUID{Bytes: uuid.New(), Valid: true}
	at := pgtype.Timestamptz{Time: time.Unix(1700000000, 0).UTC(), Valid: true}
	base := []store.RunCredentialEpoch{{RunID: run, ClaimGeneration: 1, SecretID: secret, AppliedAt: at}, {RunID: run, ClaimGeneration: 2, AppliedAt: at}}
	switches := []store.RunCredentialSwitchEpoch{
		{RunID: run, ClaimGeneration: 2, Ordinal: 2, MessageSeq: 20, AppliedAt: at},
		{RunID: run, ClaimGeneration: 1, Ordinal: 1, MessageSeq: 10, AppliedAt: at},
	}
	usage := []store.ListRunUsageByCredentialEpochRow{
		usageRow("m", 1, 0, "per_leg", g64(1), nil, 100, 0, 1000000),
		usageRow("m", 2, 0, "per_leg", g64(1), o32(1), 50, 0, 500000),
		usageRow("m", 3, 0, "per_leg", g64(2), nil, 25, 0, 0),
	}
	got := credentialEpochsToDTO(base, switches, usage)
	type pos struct {
		gen int64
		ord int32
	}
	var order []pos
	for _, e := range got {
		order = append(order, pos{e.ClaimGeneration, e.Ordinal})
	}
	wantOrder := []pos{{1, 0}, {1, 1}, {2, 0}, {2, 2}}
	if len(order) != len(wantOrder) {
		t.Fatalf("epochs = %v", order)
	}
	for i := range order {
		if order[i] != wantOrder[i] {
			t.Fatalf("order = %v, want %v", order, wantOrder)
		}
	}
	if got[0].MessageSeq != nil || got[1].MessageSeq == nil || *got[1].MessageSeq != 10 {
		t.Fatalf("message_seq must be set on switch epochs only: %v %v", got[0].MessageSeq, got[1].MessageSeq)
	}
	if got[0].Usage == nil || got[0].Usage.InputTokens != 100 || got[0].Usage.CostUSD != 1 ||
		got[1].Usage.InputTokens != 50 || got[2].Usage.InputTokens != 25 || got[3].Usage.InputTokens != 0 {
		t.Fatalf("usage = %+v %+v %+v %+v", got[0].Usage, got[1].Usage, got[2].Usage, got[3].Usage)
	}
	// With no usage legs the epochs carry none.
	if bare := credentialEpochsToDTO(base, nil, nil); bare[0].Usage != nil {
		t.Fatal("a run with no usage legs must omit usage")
	}
}
