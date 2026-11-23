package workersvc

import (
	"context"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/leadcfg"
	"github.com/vtmocanu/uzi/api/internal/providers"
	"github.com/vtmocanu/uzi/api/internal/store"
	"testing"
)

func codexSourceRow() store.GetUserProviderSourceRow {
	pid := uuid.New()
	return store.GetUserProviderSourceRow{ProviderID: pgtype.UUID{Bytes: pid, Valid: true}, Engine: pgtype.Text{String: "codex", Valid: true}, Protocol: pgtype.Text{String: "openai_responses", Valid: true}, CurrentRevision: pgtype.Int4{Int32: 2, Valid: true}, BaseUrl: pgtype.Text{String: pvBase, Valid: true}, AuthStyle: pgtype.Text{String: "bearer", Valid: true}, ModelMap: []byte(`{"default":{"model":"gateway"}}`), KeySecretID: pgtype.UUID{Bytes: uuid.New(), Valid: true}, KeyKind: pgtype.Text{String: store.KindOpenAICompatibleKey, Valid: true}, KeyProviderID: pgtype.UUID{Bytes: pid, Valid: true}, KeyEnablementRev: pgtype.Int8{Int64: 7, Valid: true}}
}

type sourceMatrixStore struct {
	poolFakeStore
	source      store.GetUserProviderSourceRow
	claude      store.GetUserProviderSourceRow
	sourceReads int
	pref        pgtype.Text
}

func (f *sourceMatrixStore) GetUserProviderSource(_ context.Context, p store.GetUserProviderSourceParams) (store.GetUserProviderSourceRow, error) {
	f.sourceReads++
	if p.Engine == "codex" {
		return f.source, nil
	}
	return f.claude, nil
}
func (f *sourceMatrixStore) GetUserDefaultHarness(context.Context, uuid.UUID) (pgtype.Text, error) {
	return f.pref, nil
}

func TestCodexProviderSourceMatrix(t *testing.T) {
	for _, broken := range []bool{false, true} {
		for _, pref := range []string{"", "claude", "codex"} {
			for _, explicit := range []string{"", "claude", "codex"} {
				t.Run(pref+"/"+explicit+"/"+map[bool]string{true: "broken", false: "usable"}[broken], func(t *testing.T) {
					f := &sourceMatrixStore{poolFakeStore: poolFakeStore{mode: "auto", pool: []store.ListCodexPoolAliasesRow{eligiblePoolRow()}, hasAnthropic: true}, source: codexSourceRow(), pref: pgtype.Text{String: pref, Valid: pref != ""}}
					if broken {
						f.source.KeySecretID = pgtype.UUID{}
					}
					svc := &Service{}
					svc.p.ProviderAllowedBaseURLs = []string{pvBase}
					var pin *Harness
					if explicit != "" {
						h := Harness(explicit)
						pin = &h
					}
					got, err := svc.resolveRunHarnessQ(context.Background(), uuid.New(), pin, f)
					selected := explicit
					if selected == "" {
						selected = pref
					}
					refuse := broken && selected != "claude"
					if refuse {
						if !errors.Is(err, ErrProviderSourceUnusable) {
							t.Fatalf("error=%v", err)
						}
					} else {
						if err != nil {
							t.Fatal(err)
						}
						if selected == "" {
							selected = "claude"
						}
						if got.Harness != Harness(selected) {
							t.Fatalf("harness=%s want %s", got.Harness, selected)
						}
						if selected == "codex" && (got.ProviderSource == nil || got.Codex != nil || got.ProviderSource.KeyRev != 7) {
							t.Fatalf("binding=%+v", got)
						}
						if !got.CodexSourceSet {
							t.Fatal("missing source flag")
						}
					}
					if f.poolReadCalls != 0 {
						t.Fatalf("selected provider consulted vendor pool %d times", f.poolReadCalls)
					}
				})
			}
		}
	}
}

func TestEvaluateCodexProviderPairing(t *testing.T) {
	for _, field := range []string{"engine", "protocol", "auth", "kind", "provider", "default", "bounds"} {
		t.Run(field, func(t *testing.T) {
			row := codexSourceRow()
			switch field {
			case "engine":
				row.Engine.String = "claude"
			case "protocol":
				row.Protocol.String = "anthropic_messages"
			case "auth":
				row.AuthStyle.String = "x_api_key"
			case "kind":
				row.KeyKind.String = store.KindOpenAIAPIKey
			case "provider":
				row.KeyProviderID.Bytes = uuid.New()
			case "default":
				row.ModelMap = []byte(`{"gpt-5":{"model":"gateway"}}`)
			case "bounds":
				row.ModelMap = []byte(`{"default":{"model":"gateway","context_window":-1}}`)
			}
			if got := EvaluateProviderSource("codex", row, allowOnly(pvBase)); got.State != ProviderSourceUnusable {
				t.Fatalf("accepted invalid %s: %+v", field, got)
			}
		})
	}
}

func TestProviderSettingsProjectionMatrix(t *testing.T) {
	// Each engine independently has no source, a usable source, or a broken source.
	for c := 0; c < 3; c++ {
		for x := 0; x < 3; x++ {
			for _, vendor := range []bool{false, true} {
				for _, pref := range []string{"", "claude", "codex"} {
					t.Run(fmt.Sprintf("claude%d/codex%d/vendor%t/%s", c, x, vendor, pref), func(t *testing.T) {
						f := &sourceMatrixStore{poolFakeStore: poolFakeStore{hasAnthropic: vendor, mode: "default"}, pref: pgtype.Text{String: pref, Valid: pref != ""}}
						if vendor {
							f.apiDefault = subDefault()
							f.totalCodex = 1
							f.enabledCodex = 1
						}
						if c > 0 {
							f.claude = codexSourceRow()
							f.claude.Engine.String = "claude"
							f.claude.Protocol.String = providers.ProtocolAnth
							f.claude.KeyKind.String = store.KindAnthropicCompatibleKey
							f.claude.ModelMap = []byte(pvDefaultMap)
							if c == 2 {
								f.claude.KeySecretID = pgtype.UUID{}
							}
						}
						if x > 0 {
							f.source = codexSourceRow()
							if x == 2 {
								f.source.KeySecretID = pgtype.UUID{}
							}
						}
						svc := &Service{q: f}
						svc.p.ProviderAllowedBaseURLs = []string{pvBase}
						var proposed *Harness
						if pref != "" {
							h := Harness(pref)
							proposed = &h
						}
						cu := c == 1 || (c == 0 && vendor)
						xu := x == 1 || (x == 0 && vendor)
						want := HarnessClaude
						// Match creation's refusal order while projecting its refused source engine.
						if pref == "claude" && c == 2 {
							want = HarnessClaude
						} else if pref == "codex" && x == 2 {
							want = HarnessCodex
						} else if x == 2 && (pref != "claude" || !cu) {
							want = HarnessCodex
						} else if c == 2 && (pref != "codex" || !xu) {
							want = HarnessClaude
						} else if (pref == "codex" && xu) || (!cu && xu) {
							want = HarnessCodex
						}
						for _, stored := range []bool{false, true} {
							var got Harness
							var err error
							if stored {
								got, err = svc.ResolveSettingsHarness(context.Background(), uuid.New())
							} else {
								got, err = svc.ResolveSettingsHarnessWithDefault(context.Background(), uuid.New(), proposed)
							}
							if err != nil || got != want {
								t.Fatalf("stored=%t got=%s err=%v want=%s", stored, got, err, want)
							}
						}
						actual, err := svc.resolveRunHarnessQ(context.Background(), uuid.New(), nil, f)
						if err == nil && actual.Harness != want {
							t.Fatalf("projection %s differs from creation %s", want, actual.Harness)
						}
					})
				}
			}
		}
	}
}

func TestUnknownProviderEngineFailsClosed(t *testing.T) {
	f := &sourceMatrixStore{}
	if _, err := ResolveProviderSource(context.Background(), f, uuid.New(), "unknown", allowOnly(pvBase)); err == nil {
		t.Fatal("unknown engine accepted")
	}
	if f.sourceReads != 0 {
		t.Fatal("unknown engine queried source")
	}
	if got := EvaluateProviderSource("unknown", store.GetUserProviderSourceRow{}, nil); got.State != ProviderSourceUnusable || got.Reason != SourceUnusableInvalid {
		t.Fatalf("empty row accepted: %+v", got)
	}
}

func TestCodexProviderModelPins(t *testing.T) {
	src := EvaluateProviderSource("codex", codexSourceRow(), allowOnly(pvBase)).Source
	for _, origin := range []string{"", string(leadcfg.ModelSourceRunOverride), string(leadcfg.ModelSourceSchedulePin), "internal"} {
		for _, model := range []string{"default", "gateway", "missing"} {
			run := store.Run{Model: pgtype.Text{String: model, Valid: true}, ModelSource: pgtype.Text{String: origin, Valid: origin != ""}}
			err := (&Service{}).checkProviderModels(context.Background(), nil, run, src)
			wantRefusal := origin != "internal" && model != "default"
			if errors.Is(err, ErrProviderModelUnmapped) != wantRefusal {
				t.Fatalf("origin=%s model=%s err=%v", origin, model, err)
			}
		}
	}
}

func TestProviderSourcesExcludeAutomaticAndOverflow(t *testing.T) {
	for _, engine := range []Harness{HarnessClaude, HarnessCodex} {
		for _, selected := range []Harness{HarnessClaude, HarnessCodex} {
			res := resolvedHarness{Harness: selected, DefaultUnset: true, BothUsable: true, ClaudeSourceSet: engine == HarnessClaude, CodexSourceSet: engine == HarnessCodex}
			origin := harnessCreationOrigin{Kind: "issue", TriggerSource: "manual"}
			policy := store.GetHarnessCreationPolicyRow{HarnessOverflowEnabled: true}
			if automaticHarnessEligible(res, nil, origin, policy) {
				t.Fatalf("automatic eligible: %s/%s", engine, selected)
			}
			got, pick, err := (&Service{}).evaluateCreationOverflow(context.Background(), nil, uuid.New(), nil, res, policy, origin)
			if err != nil || pick != nil || got.Harness != selected || got.Decision.Reason != harnessReasonExcluded {
				t.Fatalf("overflow %s/%s: %+v %v", engine, selected, got, err)
			}
		}
	}
}

func TestBrokenProviderSourcesPreservePreferredEngine(t *testing.T) {
	for _, engine := range []Harness{HarnessClaude, HarnessCodex} {
		t.Run(string(engine), func(t *testing.T) {
			f := &sourceMatrixStore{poolFakeStore: poolFakeStore{hasAnthropic: true, mode: "auto", pool: []store.ListCodexPoolAliasesRow{eligiblePoolRow()}}, pref: pgtype.Text{String: string(engine), Valid: true}, source: codexSourceRow(), claude: codexSourceRow()}
			f.claude.Engine.String = "claude"
			f.claude.Protocol.String = providers.ProtocolAnth
			f.claude.KeyKind.String = store.KindAnthropicCompatibleKey
			f.claude.ModelMap = []byte(pvDefaultMap)
			f.claude.KeySecretID = pgtype.UUID{}
			f.source.KeySecretID = pgtype.UUID{}
			want := uuid.UUID(f.source.ProviderID.Bytes)
			if engine == HarnessClaude {
				want = uuid.UUID(f.claude.ProviderID.Bytes)
			}
			svc := &Service{q: f}
			svc.p.ProviderAllowedBaseURLs = []string{pvBase}
			_, err := svc.resolveRunHarnessQ(context.Background(), uuid.New(), nil, f)
			var unusable *ProviderSourceUnusableError
			if !errors.As(err, &unusable) || unusable.ProviderID != want {
				t.Fatalf("preferred %s refusal=%v; want source %s", engine, err, want)
			}
			got, err := svc.ResolveSettingsHarness(context.Background(), uuid.New())
			if err != nil || got != engine {
				t.Fatalf("projection=%s error=%v; want %s", got, err, engine)
			}
		})
	}
}

func TestCodexProviderVisibleModelPin(t *testing.T) {
	src := EvaluateProviderSource("codex", codexSourceRow(), allowOnly(pvBase)).Source
	src.ModelMap["gpt-6-sol"] = providers.ModelEntry{Model: "gateway-specific"}
	run := store.Run{Model: pgtype.Text{String: "gpt-6-sol", Valid: true}, ModelSource: pgtype.Text{String: string(leadcfg.ModelSourceRunOverride), Valid: true}}
	if err := (&Service{}).checkProviderModels(context.Background(), nil, run, src); err != nil {
		t.Fatal(err)
	}
	delete(src.ModelMap, "gpt-6-sol")
	if err := (&Service{}).checkProviderModels(context.Background(), nil, run, src); !errors.Is(err, ErrProviderModelUnmapped) {
		t.Fatalf("explicit pin used default: %v", err)
	}
	run.ModelSource.String = string(leadcfg.ModelSourceUserDefault)
	if err := (&Service{}).checkProviderModels(context.Background(), nil, run, src); err != nil {
		t.Fatalf("internal model did not use default: %v", err)
	}
}

func (f *sourceMatrixStore) GetUserProviderLaneSources(context.Context, uuid.UUID) (store.GetUserProviderLaneSourcesRow, error) {
	f.sourceReads++
	return store.GetUserProviderLaneSourcesRow{ClaudeProviderID: f.claude.ProviderID, CodexProviderID: f.source.ProviderID}, nil
}

// A Codex provider-bound lead-lane run never consults the owner's planning default at create: it is
// uzi-selected and resolves through its own map key or `default`. The nil Queries would panic on any
// planning lookup, so reaching nil proves none happened.
func TestCodexProviderCreateIgnoresUnmappedPlanningDefault(t *testing.T) {
	src := EvaluateProviderSource("codex", codexSourceRow(), allowOnly(pvBase)).Source
	for _, origin := range []string{"", string(leadcfg.ModelSourceUserDefault), string(leadcfg.ModelSourceUserPlanningDefault)} {
		run := store.Run{Kind: "issue", Model: pgtype.Text{String: "not-in-map-planning", Valid: true}, ModelSource: pgtype.Text{String: origin, Valid: origin != ""}}
		if origin == "" {
			run.Model = pgtype.Text{}
		}
		if err := (&Service{}).checkProviderModels(context.Background(), nil, run, src); err != nil {
			t.Fatalf("origin=%q: %v", origin, err)
		}
	}
}
