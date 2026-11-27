package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/agenttmpl"
	"github.com/vtmocanu/uzi/api/internal/providers"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// The guard is exercised through checkSameModel, the one function assemblePlanCrossCheckInput calls
// under the lead verdict lock, with a reader that records every read. The wiring into the claim
// transaction is proven by the LiveDB tests in cross_check_same_model_livedb_test.go.

type smCfgKey struct {
	gen   int64
	phase string
}
type smRevKey struct {
	provider uuid.UUID
	revision int32
}

type fakeSameModelReader struct {
	cfgs    map[smCfgKey]store.RunLeadConfig
	revs    map[smRevKey]store.GetModelProviderRevisionRow
	cfgErr  error
	revErr  error
	cfgReqs []store.GetRunLeadConfigForGenerationParams
	revReqs []store.GetModelProviderRevisionParams
}

func (f *fakeSameModelReader) GetRunLeadConfigForGeneration(_ context.Context, a store.GetRunLeadConfigForGenerationParams) (store.RunLeadConfig, error) {
	f.cfgReqs = append(f.cfgReqs, a)
	if f.cfgErr != nil {
		return store.RunLeadConfig{}, f.cfgErr
	}
	if c, ok := f.cfgs[smCfgKey{a.ClaimGeneration, a.Phase}]; ok {
		return c, nil
	}
	return store.RunLeadConfig{}, pgx.ErrNoRows
}

func (f *fakeSameModelReader) GetModelProviderRevision(_ context.Context, a store.GetModelProviderRevisionParams) (store.GetModelProviderRevisionRow, error) {
	f.revReqs = append(f.revReqs, a)
	if f.revErr != nil {
		return store.GetModelProviderRevisionRow{}, f.revErr
	}
	if r, ok := f.revs[smRevKey{a.ProviderID, a.Revision}]; ok {
		return r, nil
	}
	return store.GetModelProviderRevisionRow{}, pgx.ErrNoRows
}

const (
	smLeadMap    = `{"opus":{"model":"corp-opus"},"sonnet":{"model":"corp-sonnet"},"haiku":{"model":"corp-haiku"}}`
	smLeadHost   = "https://gw.example.com/anthropic"
	smOtherHost  = "https://other.example.net/v1"
	smLeadGen    = int64(3)
	smLeadRevNum = int32(2)
)

var smLeadProvider = uuid.MustParse("00000000-0000-4000-8000-0000000000a1")

func smPgText(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }

func smProviderLead() store.Run {
	return store.Run{ID: uuid.New(), Harness: harnessClaude,
		ModelProviderID:       pgtype.UUID{Bytes: smLeadProvider, Valid: true},
		ModelProviderRevision: pgtype.Int4{Int32: smLeadRevNum, Valid: true}}
}

func smVendorLead() store.Run { return store.Run{ID: uuid.New(), Harness: harnessClaude} }

func smProviderChild() store.Run {
	return store.Run{ID: uuid.New(), Harness: harnessCodex, Kind: "cross_check",
		ModelProviderID: pgtype.UUID{Bytes: uuid.New(), Valid: true}}
}

func smVendorChild() store.Run {
	return store.Run{ID: uuid.New(), Harness: harnessCodex, Kind: "cross_check"}
}

func smPayload(model string, hasModel bool, host, modelMap string) *ClaimPayload {
	p := &ClaimPayload{}
	if hasModel {
		p.Config.DefaultModel = &model
	}
	if modelMap != "" {
		var m providers.ModelMap
		if err := json.Unmarshal([]byte(modelMap), &m); err != nil {
			panic(err)
		}
		p.Secrets.Provider = &ClaimProviderBinding{Engine: harnessCodex, BaseURL: host, ModelMap: m}
	}
	return p
}

func smRevRow(host, modelMap string) store.GetModelProviderRevisionRow {
	return store.GetModelProviderRevisionRow{ProviderID: smLeadProvider, Revision: smLeadRevNum, BaseUrl: host, ModelMap: []byte(modelMap)}
}

func smCfg(requested, observed *string) store.RunLeadConfig {
	c := store.RunLeadConfig{}
	if requested != nil {
		c.RequestedModel = smPgText(*requested)
	}
	if observed != nil {
		c.ObservedModel = smPgText(*observed)
	}
	return c
}

func smStr(s string) *string { return &s }

func smReader(stage string, cfg store.RunLeadConfig) *fakeSameModelReader {
	return &fakeSameModelReader{
		cfgs: map[smCfgKey]store.RunLeadConfig{{smLeadGen, leadConfigPhase(stage)}: cfg},
		revs: map[smRevKey]store.GetModelProviderRevisionRow{{smLeadProvider, smLeadRevNum}: smRevRow(smLeadHost, smLeadMap)},
	}
}

func smRun(rd *fakeSameModelReader, stage string, lead, child store.Run, payload *ClaimPayload, source string) error {
	return checkSameModel(context.Background(), rd, lead, child,
		store.CrossCheck{Stage: stage, LeadClaimGeneration: smLeadGen}, payload, &agenttmpl.CrossCheckResolution{ModelSource: source})
}

func wantSameModelRefusal(t *testing.T, err error, want error) {
	t.Helper()
	if !errors.Is(err, errCheckerPinUnavailable) {
		t.Fatalf("err = %v, want one wrapping errCheckerPinUnavailable", err)
	}
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

// A CP lead (Claude provider) against an XP checker (Codex provider), both stages.
func TestCheckSameModelProviderLeadProviderChecker(t *testing.T) {
	const codexMap = `{"default":{"model":"corp-sonnet"},"gpt-6-sol":{"model":"gw-gpt-6-sol"}}`
	for _, tc := range []struct {
		name      string
		requested *string
		observed  *string
		model     string
		hasModel  bool
		source    string
		host      string
		codexMap  string
		want      error // nil = independent
	}{
		{"alias maps to the checker default, same host", smStr("sonnet"), nil, "", false, "default", smLeadHost, codexMap, errCheckerSameModel},
		{"key absent and not explicit falls to default, same id", smStr("sonnet"), nil, "unlisted", true, "default", smLeadHost, codexMap, errCheckerSameModel},
		{"different targets", smStr("opus"), nil, "", false, "default", smLeadHost, codexMap, nil},
		{"checker key with a differing target", smStr("sonnet"), nil, "gpt-6-sol", true, "pin", smLeadHost, codexMap, nil},
		{"pinned key whose target equals the lead", smStr("sonnet"), nil, "default", true, "pin", smLeadHost, codexMap, errCheckerSameModel},
		{"explicit pin absent from the map", smStr("sonnet"), nil, "unlisted", true, "pin", smLeadHost, codexMap, errCheckerModelUnresolved},
		{"effort-only pin leaves the model source at default", smStr("sonnet"), nil, "unlisted", true, "default", smLeadHost, codexMap, errCheckerSameModel},
		{"same id on a different host", smStr("sonnet"), nil, "", false, "default", smOtherHost, codexMap, nil},
		{"blank request goes through sonnet", smStr(""), nil, "", false, "default", smLeadHost, codexMap, errCheckerSameModel},
		{"null request goes through sonnet", nil, nil, "", false, "default", smLeadHost, codexMap, errCheckerSameModel},
		{"lead request unmapped", smStr("mystery"), nil, "", false, "default", smLeadHost, codexMap, errCheckerModelUnresolved},
		{"lead request equal to a target maps to itself", smStr("corp-sonnet"), nil, "", false, "default", smLeadHost, codexMap, errCheckerSameModel},
		{"checker map without a default", smStr("sonnet"), nil, "", false, "default", smLeadHost, `{"gpt-6-sol":{"model":"gw-gpt-6-sol"}}`, errCheckerModelUnresolved},
		{"observed model is not part of a provider lead's identity", smStr("opus"), smStr("corp-sonnet"), "", false, "default", smLeadHost, codexMap, nil},
		{"an observed target that is not a map key is never resolved through default", smStr("sonnet"), smStr("something-else"), "", false, "default", smLeadHost, codexMap, errCheckerSameModel},
	} {
		for _, stage := range []string{"plan", "code"} {
			t.Run(tc.name+"/"+stage, func(t *testing.T) {
				rd := smReader(stage, smCfg(tc.requested, tc.observed))
				err := smRun(rd, stage, smProviderLead(), smProviderChild(), smPayload(tc.model, tc.hasModel, tc.host, tc.codexMap), tc.source)
				if tc.want == nil {
					if err != nil {
						t.Fatalf("independent pair refused: %v", err)
					}
					return
				}
				wantSameModelRefusal(t, err, tc.want)
			})
		}
	}
}

// The lead evidence is the row of the check's own generation and phase; anything else is Unresolved.
func TestCheckSameModelLeadEvidenceFailsClosed(t *testing.T) {
	codexMap := `{"default":{"model":"corp-sonnet"}}`
	cfg := smCfg(smStr("sonnet"), nil)
	for _, stage := range []string{"plan", "code"} {
		wantPhase := map[string]string{"plan": "plan", "code": "implement"}[stage]
		otherPhase := map[string]string{"plan": "implement", "code": "plan"}[stage]
		t.Run(stage+"/matching row is read at the check generation and its phase", func(t *testing.T) {
			rd := smReader(stage, cfg)
			err := smRun(rd, stage, smProviderLead(), smProviderChild(), smPayload("", false, smLeadHost, codexMap), "default")
			wantSameModelRefusal(t, err, errCheckerSameModel)
			if len(rd.cfgReqs) != 1 || rd.cfgReqs[0].ClaimGeneration != smLeadGen || rd.cfgReqs[0].Phase != wantPhase {
				t.Fatalf("lead-config reads = %+v, want one at generation %d phase %s", rd.cfgReqs, smLeadGen, wantPhase)
			}
		})
		for name, mutate := range map[string]func(*fakeSameModelReader){
			"missing row": func(rd *fakeSameModelReader) { rd.cfgs = nil },
			"dropped row": func(rd *fakeSameModelReader) { rd.cfgs = map[smCfgKey]store.RunLeadConfig{} },
			"other generation": func(rd *fakeSameModelReader) {
				rd.cfgs = map[smCfgKey]store.RunLeadConfig{{smLeadGen + 1, wantPhase}: cfg}
			},
			"other phase": func(rd *fakeSameModelReader) {
				rd.cfgs = map[smCfgKey]store.RunLeadConfig{{smLeadGen, otherPhase}: cfg}
			},
			"earlier generation": func(rd *fakeSameModelReader) {
				rd.cfgs = map[smCfgKey]store.RunLeadConfig{{smLeadGen - 1, wantPhase}: cfg}
			},
		} {
			t.Run(stage+"/"+name, func(t *testing.T) {
				rd := smReader(stage, cfg)
				mutate(rd)
				err := smRun(rd, stage, smProviderLead(), smProviderChild(), smPayload("", false, smLeadHost, codexMap), "default")
				wantSameModelRefusal(t, err, errCheckerModelUnresolved)
				if len(rd.revReqs) != 0 {
					t.Fatalf("a missing lead row still read the provider revision: %+v", rd.revReqs)
				}
			})
		}
		t.Run(stage+"/competing rows: only the check's generation and phase decide", func(t *testing.T) {
			rd := smReader(stage, smCfg(smStr("opus"), nil)) // matching row: opus -> corp-opus, independent
			rd.cfgs[smCfgKey{smLeadGen + 1, wantPhase}] = cfg
			rd.cfgs[smCfgKey{smLeadGen, otherPhase}] = cfg // both of these would be SameModel
			if err := smRun(rd, stage, smProviderLead(), smProviderChild(), smPayload("", false, smLeadHost, codexMap), "default"); err != nil {
				t.Fatalf("a competing generation or phase decided the check: %v", err)
			}
		})
		t.Run(stage+"/request-only row of a provider lead with an unmapped value", func(t *testing.T) {
			rd := smReader(stage, smCfg(smStr("not-in-the-map"), nil))
			err := smRun(rd, stage, smProviderLead(), smProviderChild(), smPayload("", false, smLeadHost, codexMap), "default")
			wantSameModelRefusal(t, err, errCheckerModelUnresolved)
		})
		t.Run(stage+"/a lead-config read failure is an error, not a refusal", func(t *testing.T) {
			rd := smReader(stage, cfg)
			rd.cfgErr = errors.New("boom")
			err := smRun(rd, stage, smProviderLead(), smProviderChild(), smPayload("", false, smLeadHost, codexMap), "default")
			if err == nil || errors.Is(err, errCheckerPinUnavailable) {
				t.Fatalf("err = %v, want the raw read error", err)
			}
		})
	}
}

// The lead's frozen revision decides its map: a missing revision is Unresolved, another revision of
// the same provider is never consulted, and a read failure other than no-rows stays an error.
func TestCheckSameModelFrozenRevision(t *testing.T) {
	codexMap := `{"default":{"model":"corp-sonnet"}}`
	cfg := smCfg(smStr("sonnet"), nil)
	for _, stage := range []string{"plan", "code"} {
		t.Run(stage+"/missing frozen revision is Unresolved", func(t *testing.T) {
			rd := smReader(stage, cfg)
			rd.revs = map[smRevKey]store.GetModelProviderRevisionRow{}
			err := smRun(rd, stage, smProviderLead(), smProviderChild(), smPayload("", false, smLeadHost, codexMap), "default")
			wantSameModelRefusal(t, err, errCheckerModelUnresolved)
			if len(rd.revReqs) != 1 || rd.revReqs[0].ProviderID != smLeadProvider || rd.revReqs[0].Revision != smLeadRevNum {
				t.Fatalf("revision reads = %+v, want the lead's frozen (provider, revision)", rd.revReqs)
			}
		})
		t.Run(stage+"/competing revision: the frozen one decides", func(t *testing.T) {
			rd := smReader(stage, cfg)
			// A newer revision of the same provider maps sonnet elsewhere; the frozen one equals the checker.
			rd.revs[smRevKey{smLeadProvider, smLeadRevNum + 1}] = smRevRow(smLeadHost, `{"sonnet":{"model":"new-sonnet"},"haiku":{"model":"h"},"opus":{"model":"o"}}`)
			err := smRun(rd, stage, smProviderLead(), smProviderChild(), smPayload("", false, smLeadHost, codexMap), "default")
			wantSameModelRefusal(t, err, errCheckerSameModel)
		})
		t.Run(stage+"/revision read error stays an error", func(t *testing.T) {
			rd := smReader(stage, cfg)
			rd.revErr = errors.New("boom")
			err := smRun(rd, stage, smProviderLead(), smProviderChild(), smPayload("", false, smLeadHost, codexMap), "default")
			if err == nil || errors.Is(err, errCheckerPinUnavailable) {
				t.Fatalf("err = %v, want the raw read error", err)
			}
		})
		t.Run(stage+"/unparseable frozen map is Unresolved", func(t *testing.T) {
			rd := smReader(stage, cfg)
			rd.revs[smRevKey{smLeadProvider, smLeadRevNum}] = smRevRow(smLeadHost, `not json`)
			err := smRun(rd, stage, smProviderLead(), smProviderChild(), smPayload("", false, smLeadHost, codexMap), "default")
			wantSameModelRefusal(t, err, errCheckerModelUnresolved)
		})
	}
}

// Vendor lead rows (CV) against a provider checker: the observed id wins, else the requested one.
func TestCheckSameModelVendorLeadRows(t *testing.T) {
	const codexMap = `{"default":{"model":"claude-opus-5-5"},"alt":{"model":"gw-other"}}`
	for _, tc := range []struct {
		name      string
		requested *string
		observed  *string
		model     string
		hasModel  bool
		source    string
		want      error
	}{
		{"alias request, canonical observed, checker target equals observed", smStr("opus"), smStr("claude-opus-5-5"), "", false, "default", errCheckerSameModel},
		{"alias request, canonical observed, a different checker target", smStr("opus"), smStr("claude-opus-5-5"), "alt", true, "pin", nil},
		{"request-only row uses the requested value", smStr("claude-opus-5-5"), nil, "", false, "default", errCheckerSameModel},
		{"request-only alias is the identity (the documented alias limit)", smStr("opus"), nil, "", false, "default", nil},
		{"builtin row: requested NULL, observed present", nil, smStr("claude-opus-5-5"), "", false, "default", errCheckerSameModel},
		{"blank observed falls back to requested", smStr("claude-opus-5-5"), smStr("  "), "", false, "default", errCheckerSameModel},
		{"neither value present", nil, nil, "", false, "default", errCheckerModelUnresolved},
	} {
		for _, stage := range []string{"plan", "code"} {
			t.Run(tc.name+"/"+stage, func(t *testing.T) {
				rd := smReader(stage, smCfg(tc.requested, tc.observed))
				err := smRun(rd, stage, smVendorLead(), smProviderChild(), smPayload(tc.model, tc.hasModel, smOtherHost, codexMap), tc.source)
				if len(rd.revReqs) != 0 {
					t.Fatalf("a vendor lead read a provider revision: %+v", rd.revReqs)
				}
				if tc.want == nil {
					if err != nil {
						t.Fatalf("independent pair refused: %v", err)
					}
					return
				}
				wantSameModelRefusal(t, err, tc.want)
			})
		}
	}
}

// A provider lead against a vendor checker (CP/XV, a later slice) compares the vendor model id.
func TestCheckSameModelProviderLeadVendorChecker(t *testing.T) {
	rd := smReader("plan", smCfg(smStr("sonnet"), nil))
	if err := smRun(rd, "plan", smProviderLead(), smVendorChild(), smPayload("corp-sonnet", true, "", ""), ""); !errors.Is(err, errCheckerSameModel) {
		t.Fatalf("same id against a vendor checker: %v", err)
	}
	rd = smReader("plan", smCfg(smStr("sonnet"), nil))
	if err := smRun(rd, "plan", smProviderLead(), smVendorChild(), smPayload("gpt-5", true, "", ""), ""); err != nil {
		t.Fatalf("different id against a vendor checker: %v", err)
	}
	rd = smReader("plan", smCfg(smStr("sonnet"), nil))
	if err := smRun(rd, "plan", smProviderLead(), smVendorChild(), smPayload("", false, "", ""), ""); !errors.Is(err, errCheckerModelUnresolved) {
		t.Fatalf("vendor checker with no model: %v", err)
	}
}

// A provider checker whose payload carries no binding cannot be compared: fail closed.
func TestCheckSameModelProviderCheckerWithoutBinding(t *testing.T) {
	rd := smReader("plan", smCfg(smStr("sonnet"), nil))
	err := smRun(rd, "plan", smProviderLead(), smProviderChild(), smPayload("", false, "", ""), "default")
	wantSameModelRefusal(t, err, errCheckerModelUnresolved)
}

// Vendor/vendor performs no read at all, even when every read would fail.
func TestCheckSameModelVendorVendorReadsNothing(t *testing.T) {
	for _, stage := range []string{"plan", "code"} {
		rd := &fakeSameModelReader{cfgErr: errors.New("lead config read must not happen"), revErr: errors.New("revision read must not happen")}
		if err := smRun(rd, stage, smVendorLead(), smVendorChild(), smPayload("gpt-5", true, "", ""), "pin"); err != nil {
			t.Fatalf("%s: vendor/vendor refused: %v", stage, err)
		}
		if len(rd.cfgReqs) != 0 || len(rd.revReqs) != 0 {
			t.Fatalf("%s: vendor/vendor read lead config %d / revision %d times", stage, len(rd.cfgReqs), len(rd.revReqs))
		}
	}
}

// A provider checker child's assembly refusals decide the check lead-first instead of parking it;
// vault-locked and seal-stale stay transient, and other run shapes are untouched.
func TestCheckerClaimErrorMapsProviderRefusals(t *testing.T) {
	provider := smProviderChild()
	vendorCodex := smVendorChild()
	claude := store.Run{Kind: "cross_check", Harness: harnessClaude}
	judge := smProviderChild()
	judge.Kind = "judge"
	refusal := &providerClaimRefusal{Reason: holdProviderBindingStale}
	for _, tc := range []struct {
		name string
		run  store.Run
		err  error
		want bool // mapped to errCheckerPinUnavailable
	}{
		{"binding stale refusal", provider, refusal, true},
		{"destination refusal", provider, &providerClaimRefusal{Reason: holdProviderDestinationNotAllowed}, true},
		{"wrapped refusal", provider, errors.Join(errors.New("ctx"), refusal), true},
		{"capability backstop", provider, errProviderLaneClaimRefused, true},
		{"credential unavailable", provider, errCredentialUnavailable, true},
		{"vault locked stays transient", provider, errVaultLocked, false},
		{"seal stale stays transient", provider, store.ErrVaultSealStale, false},
		{"unrelated error", provider, errors.New("db down"), false},
		{"vendor codex checker is not mapped for a provider refusal", vendorCodex, refusal, false},
		{"claude checker is not mapped for a provider refusal", claude, refusal, false},
		{"provider judge keeps its own arm", judge, refusal, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := claudeCheckerClaimError(tc.run, tc.err)
			if mapped := got == errCheckerPinUnavailable; mapped != tc.want {
				t.Fatalf("claudeCheckerClaimError = %v, mapped=%v want %v", got, mapped, tc.want)
			}
		})
	}
	if !isProviderCheckerChild(provider) || isProviderCheckerChild(vendorCodex) || isProviderCheckerChild(judge) {
		t.Fatal("isProviderCheckerChild must be exactly cross_check kind and provider-bound")
	}
}

var _ sameModelReader = (*store.Queries)(nil)
