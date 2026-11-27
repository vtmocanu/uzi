package workersvc

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/providers"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// checker_lane_test.go is PRD #4574 M2's lane decision (checkerLaneDecision), the no-transaction arms
// and the consent toggle over in-memory stores. The LiveDB twins (the create transaction, the lock
// order and the races) are in checker_lane_livedb_test.go.

func laneRow(claude, codex bool) store.GetUserProviderLaneSourcesRow {
	var r store.GetUserProviderLaneSourcesRow
	if claude {
		r.ClaudeProviderID = pgtype.UUID{Bytes: uuid.New(), Valid: true}
	}
	if codex {
		r.CodexProviderID = pgtype.UUID{Bytes: uuid.New(), Valid: true}
	}
	return r
}

// TestCheckerLaneDecisionTable is the full decision table (PRD #4574 M2 lines on the Codex and Claude
// checker): every combination of the lead's provider-boundness, the checker engine and the two selected
// sources. The expected values are written out, never derived from the predicate.
func TestCheckerLaneDecisionTable(t *testing.T) {
	const (
		R = checkerLaneRefused
		V = checkerLaneVendor
		P = checkerLaneProvider
	)
	type key struct {
		leadBound, claudeSet, codexSet bool
		engine                         Harness
	}
	want := map[key]checkerLaneKind{
		// Codex checker: a selected Codex source is always Provider; else a bound lead or a Claude source refuses.
		{false, false, false, HarnessCodex}: V,
		{false, true, false, HarnessCodex}:  R,
		{false, false, true, HarnessCodex}:  P,
		{false, true, true, HarnessCodex}:   P,
		{true, false, false, HarnessCodex}:  R,
		{true, true, false, HarnessCodex}:   R,
		{true, false, true, HarnessCodex}:   P,
		{true, true, true, HarnessCodex}:    P,
		// Claude checker: today's rule; a Codex source is irrelevant.
		{false, false, false, HarnessClaude}: V,
		{false, true, false, HarnessClaude}:  R,
		{false, false, true, HarnessClaude}:  V,
		{false, true, true, HarnessClaude}:   R,
		{true, false, false, HarnessClaude}:  R,
		{true, true, false, HarnessClaude}:   R,
		{true, false, true, HarnessClaude}:   R,
		{true, true, true, HarnessClaude}:    R,
	}
	if len(want) != 16 {
		t.Fatalf("table has %d rows, want 16", len(want))
	}
	for k, w := range want {
		name := fmt.Sprintf("lead=%t/claude=%t/codex=%t/%s", k.leadBound, k.claudeSet, k.codexSet, k.engine)
		if got := checkerLaneDecision(k.leadBound, k.engine, laneRow(k.claudeSet, k.codexSet)); got != w {
			t.Errorf("%s: decision = %d, want %d", name, got, w)
		}
	}
	for _, bad := range []Harness{"", "unknown"} {
		if got := checkerLaneDecision(false, bad, laneRow(false, true)); got != R {
			t.Errorf("engine %q: decision = %d, want Refused", bad, got)
		}
	}
}

// checkerLaneFake is an in-memory resolver store with selectable sources and a Codex vendor credential.
type checkerLaneFake struct {
	poolFakeStore
	lanes       store.GetUserProviderLaneSourcesRow
	codexSrc    store.GetUserProviderSourceRow
	claudeSrc   store.GetUserProviderSourceRow
	laneReads   int
	sourceReads int
}

func (f *checkerLaneFake) GetUserProviderLaneSources(context.Context, uuid.UUID) (store.GetUserProviderLaneSourcesRow, error) {
	f.laneReads++
	return f.lanes, nil
}

func (f *checkerLaneFake) GetUserProviderSource(_ context.Context, p store.GetUserProviderSourceParams) (store.GetUserProviderSourceRow, error) {
	f.sourceReads++
	if p.Engine == providers.EngineCodex {
		return f.codexSrc, nil
	}
	return f.claudeSrc, nil
}

func claudeSourceRowFor() store.GetUserProviderSourceRow {
	r := codexSourceRow()
	r.Engine.String = providers.EngineClaude
	r.Protocol.String = providers.ProtocolAnth
	r.KeyKind.String = store.KindAnthropicCompatibleKey
	r.ModelMap = []byte(pvDefaultMap)
	return r
}

// with sets a selected source's presence on the lane read and its resolution row.
func (f *checkerLaneFake) withCodex(usable bool) *checkerLaneFake {
	f.codexSrc = codexSourceRow()
	if !usable {
		f.codexSrc.KeySecretID = pgtype.UUID{}
	}
	f.lanes.CodexProviderID = f.codexSrc.ProviderID
	return f
}

func (f *checkerLaneFake) withClaude(usable bool) *checkerLaneFake {
	f.claudeSrc = claudeSourceRowFor()
	if !usable {
		f.claudeSrc.KeySecretID = pgtype.UUID{}
	}
	f.lanes.ClaudeProviderID = f.claudeSrc.ProviderID
	return f
}

func (f *checkerLaneFake) service() *Service {
	svc := &Service{q: f}
	svc.p.ProviderAllowedBaseURLs = []string{pvBase}
	return svc
}

// TestCheckerLaneNoTransactionArms: both no-transaction arms (createRunResolved's in-memory arm and
// createRunAtomic with no transaction beginner) compute checkerLaneDecision from one source read where
// refuseProviderLane was called. ONLY a Provider decision is errProviderCreateRequiresTx; Refused keeps
// ErrProviderLaneUnsupported; Vendor keeps today's outcome per checker engine. Every row asserts the
// exact error and the insert count.
func TestCheckerLaneNoTransactionArms(t *testing.T) {
	codexVendor := func() *checkerLaneFake {
		return &checkerLaneFake{poolFakeStore: poolFakeStore{hasAnthropic: true, totalCodex: 1, enabledCodex: 1, apiDefault: subDefault()}}
	}
	for _, tc := range []struct {
		name      string
		build     func() *checkerLaneFake
		explicit  *Harness
		leadBound bool
		// resolved is the createRunResolved (in-memory) arm; atomic is createRunAtomic with no beginner.
		resolvedErr, atomicErr         error
		resolvedInserts, atomicInserts int
	}{
		{name: "provider decision", build: func() *checkerLaneFake { return codexVendor().withCodex(true) }, explicit: laneHarness(HarnessCodex),
			resolvedErr: errProviderCreateRequiresTx, atomicErr: errProviderCreateRequiresTx},
		{name: "provider decision, unusable source", build: func() *checkerLaneFake { return codexVendor().withCodex(false) }, explicit: laneHarness(HarnessCodex),
			resolvedErr: errProviderCreateRequiresTx, atomicErr: errProviderCreateRequiresTx},
		{name: "provider decision, bound lead", build: func() *checkerLaneFake { return codexVendor().withCodex(true) }, explicit: laneHarness(HarnessCodex), leadBound: true,
			resolvedErr: errProviderCreateRequiresTx, atomicErr: errProviderCreateRequiresTx},
		{name: "refused: claude source, codex checker", build: func() *checkerLaneFake { return codexVendor().withClaude(true) }, explicit: laneHarness(HarnessCodex),
			resolvedErr: ErrProviderLaneUnsupported, atomicErr: ErrProviderLaneUnsupported},
		{name: "refused: bound lead, codex checker, no codex source", build: codexVendor, explicit: laneHarness(HarnessCodex), leadBound: true,
			resolvedErr: ErrProviderLaneUnsupported, atomicErr: ErrProviderLaneUnsupported},
		{name: "refused: bound lead, claude checker", build: codexVendor, explicit: laneHarness(HarnessClaude), leadBound: true,
			resolvedErr: ErrProviderLaneUnsupported, atomicErr: ErrProviderLaneUnsupported},
		{name: "refused: claude source, claude checker", build: func() *checkerLaneFake { return codexVendor().withClaude(true) }, explicit: laneHarness(HarnessClaude),
			resolvedErr: ErrProviderLaneUnsupported, atomicErr: ErrProviderLaneUnsupported},
		{name: "refused: no explicit harness", build: codexVendor, explicit: nil,
			resolvedErr: ErrProviderLaneUnsupported, atomicErr: ErrProviderLaneUnsupported},
		// A Claude checker with only a Codex source selected is Vendor: the in-memory arm inserts; the
		// atomic no-beginner arm has no *store.Queries in this fake (its Claude insert is LiveDB-proved).
		{name: "vendor claude checker, codex source selected", build: func() *checkerLaneFake { return codexVendor().withCodex(true) }, explicit: laneHarness(HarnessClaude),
			resolvedInserts: 1, atomicErr: errHarnessStoreUnavailable},
		{name: "vendor claude checker, no source", build: codexVendor, explicit: laneHarness(HarnessClaude),
			resolvedInserts: 1, atomicErr: errHarnessStoreUnavailable},
		// A Codex checker with no Codex source keeps today's Codex outcomes.
		{name: "vendor codex checker", build: codexVendor, explicit: laneHarness(HarnessCodex),
			resolvedErr: errNoCredentialForHarness, atomicErr: errCodexCreateRequiresTx},
	} {
		t.Run(tc.name, func(t *testing.T) {
			origin := harnessCreationOrigin{CheckerLane: true, LeadProviderBound: tc.leadBound}
			for _, arm := range []string{"createRunResolved", "createRunAtomic"} {
				f := tc.build()
				svc := f.service()
				inserts := 0
				var err error
				if arm == "createRunResolved" {
					_, err = svc.createRunResolved(context.Background(), uuid.New(), tc.explicit, func(Store, resolvedHarness) (store.Run, error) {
						inserts++
						return store.Run{}, nil
					}, origin)
				} else {
					_, _, err = svc.createRunAtomic(context.Background(), uuid.New(), tc.explicit, func(*store.Queries, resolvedHarness) (store.Run, error) {
						inserts++
						return store.Run{}, nil
					}, origin)
				}
				wantErr, wantInserts := tc.resolvedErr, tc.resolvedInserts
				if arm == "createRunAtomic" {
					wantErr, wantInserts = tc.atomicErr, tc.atomicInserts
				}
				if wantErr == nil && err != nil || wantErr != nil && !errors.Is(err, wantErr) || inserts != wantInserts {
					t.Errorf("%s: err = %v inserts = %d, want %v and %d", arm, err, inserts, wantErr, wantInserts)
				}
				// A Provider decision must never have reached the vendor Codex credential reads.
				if tc.name == "provider decision" && f.poolReadCalls != 0 {
					t.Errorf("%s: a provider decision read the vendor pool", arm)
				}
			}
		})
	}
}

// TestBothHarnessesUsableFollowsTheLaneDecision is the consent toggle's service matrix (PRD #4574 M2,
// owner decision Q3). Each case isolates one half: every refusal has the other half usable, and every
// success has the half that should count its source WITHOUT a vendor credential, so a wrong vendor
// fallback cannot pass.
func TestBothHarnessesUsableFollowsTheLaneDecision(t *testing.T) {
	vendorCodex := poolFakeStore{totalCodex: 1, enabledCodex: 1, apiDefault: subDefault()}
	for _, tc := range []struct {
		name  string
		build func() *checkerLaneFake
		want  bool
	}{
		{"usable codex source, no vendor codex, vendor anthropic", func() *checkerLaneFake {
			return (&checkerLaneFake{poolFakeStore: poolFakeStore{hasAnthropic: true}}).withCodex(true)
		}, true},
		{"usable claude source, no vendor anthropic, usable codex source", func() *checkerLaneFake {
			return (&checkerLaneFake{}).withCodex(true).withClaude(true)
		}, true},
		{"unusable codex source beats a usable vendor codex credential", func() *checkerLaneFake {
			f := &checkerLaneFake{poolFakeStore: vendorCodex}
			f.hasAnthropic = true
			return f.withCodex(false)
		}, false},
		{"unusable claude source beats a vendor anthropic token", func() *checkerLaneFake {
			return (&checkerLaneFake{poolFakeStore: poolFakeStore{hasAnthropic: true}}).withCodex(true).withClaude(false)
		}, false},
		{"usable claude source with no codex source stays refused despite vendor codex", func() *checkerLaneFake {
			return (&checkerLaneFake{poolFakeStore: vendorCodex}).withClaude(true)
		}, false},
		{"unusable claude source with no codex source stays refused", func() *checkerLaneFake {
			return (&checkerLaneFake{poolFakeStore: vendorCodex}).withClaude(false)
		}, false},
		{"vendor/vendor, both usable", func() *checkerLaneFake {
			f := &checkerLaneFake{poolFakeStore: vendorCodex}
			f.hasAnthropic = true
			return f
		}, true},
		{"vendor/vendor, no anthropic token", func() *checkerLaneFake { return &checkerLaneFake{poolFakeStore: vendorCodex} }, false},
		{"vendor/vendor, no codex credential", func() *checkerLaneFake {
			return &checkerLaneFake{poolFakeStore: poolFakeStore{hasAnthropic: true}}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.build()
			got, err := f.service().BothHarnessesUsable(context.Background(), uuid.New())
			if err != nil || got != tc.want {
				t.Fatalf("BothHarnessesUsable = %t, %v; want %t", got, err, tc.want)
			}
			if f.laneReads != 1 {
				t.Errorf("lane source reads = %d, want 1", f.laneReads)
			}
		})
	}
}

// TestVerifyRunBindingCrossCheckArmChecksOnlyTheMap: a checker child is verified against its frozen map
// alone (PRD #4574 M2): no lead or template model is read (a nil query surface would panic on any read,
// and a Claude-engine source would reach resolveLeadSettings without the arm), and an invalid map for the
// engine is an internal binding mismatch.
func TestVerifyRunBindingCrossCheckArmChecksOnlyTheMap(t *testing.T) {
	for _, engine := range []string{providers.EngineCodex, providers.EngineClaude} {
		t.Run(engine, func(t *testing.T) {
			row := codexSourceRow()
			if engine == providers.EngineClaude {
				row = claudeSourceRowFor()
			}
			src := EvaluateProviderSource(engine, row, allowOnly(pvBase)).Source
			res := resolvedHarness{Harness: Harness(engine), ProviderSource: &src}
			b := bindingOf(res)
			run := store.Run{Kind: "cross_check", Harness: engine, Model: pgtype.Text{String: "not-in-any-map", Valid: true},
				ModelProviderID: b.ProviderID, ModelProviderRevision: b.Revision, ModelProviderKeyMode: b.KeyMode, ModelProviderKeySecretID: b.KeySecret, ModelProviderKeyRev: b.KeyRev}
			if err := (&Service{}).verifyRunBinding(context.Background(), nil, run, res); err != nil {
				t.Fatalf("a valid map must verify with no model reads: %v", err)
			}
			src.ModelMap = providers.ModelMap{"not-a-vocabulary-key": {Model: "x"}}
			if err := (&Service{}).verifyRunBinding(context.Background(), nil, run, res); !errors.Is(err, errProviderBindingMismatch) {
				t.Fatalf("an invalid map err = %v, want errProviderBindingMismatch", err)
			}
		})
	}
}
