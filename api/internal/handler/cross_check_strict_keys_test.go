package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type wireEmbedded struct {
	Inner string `json:"inner_name"`
	Plain string
}

type wireProbe struct {
	hidden   bool
	Skipped  string `json:"-"`
	Tagged   string `json:"tagged_name,omitempty"`
	Untagged string
	Ptr      *bool `json:"ptr_name"`
	wireEmbedded
}

func TestJSONWireNamesReflection(t *testing.T) {
	got := []string{}
	for name := range jsonWireNames(reflect.TypeOf(wireProbe{hidden: true})) {
		got = append(got, name)
	}
	sort.Strings(got)
	want := []string{"Plain", "Untagged", "inner_name", "ptr_name", "tagged_name"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("names = %v, want %v", got, want)
	}
}

func TestJSONWireNamesIncludeEmbeddedPlanCandidate(t *testing.T) {
	names := jsonWireNames(reflect.TypeOf(planCrossCheckRequest{}))
	for _, n := range []string{"plan_md", "milestones", "required_capabilities", "required_tools", "size_class", "base_commit", "planning_diff", "repo_instructions_enabled", "repo_instructions_text", "claim_generation", "stage", "round", "head_commit"} {
		if _, ok := names[n]; !ok {
			t.Errorf("missing %s", n)
		}
	}
	for _, n := range []string{"instructionFields", "planFields", "PlanCrossCheckCandidate"} {
		if _, ok := names[n]; ok {
			t.Errorf("unexpected %s", n)
		}
	}
}

// A json:"-" field must contribute no name at all, not even "-". TestJSONWireNamesReflection
// covers the same tag through wireProbe's Skipped field among other tag forms; this isolates it
// in a struct whose only other field is a plain tagged one.
func TestJSONWireNamesSkipsDashTag(t *testing.T) {
	type onlySkipped struct {
		Skipped string `json:"-"`
		Kept    string `json:"kept"`
	}
	names := jsonWireNames(reflect.TypeOf(onlySkipped{}))
	if _, ok := names["-"]; ok {
		t.Fatalf(`json:"-" field produced the name "-": %v`, names)
	}
	if _, ok := names["kept"]; !ok || len(names) != 1 {
		t.Fatalf("names = %v, want only kept", names)
	}
}

// Two allowed wire names that differ only by case must still not both appear:
// encoding/json binds keys case-insensitively, so the pair is one field.
func TestRequireExactTopLevelKeysRefusesCaseFoldedPair(t *testing.T) {
	type caseTwins struct {
		A string `json:"k"`
		B string `json:"K"`
	}
	allowed := jsonWireNames(reflect.TypeOf(caseTwins{}))
	for _, body := range []string{`{"k":"1","K":"2"}`, `{"K":"2","k":"1"}`} {
		if err := requireExactTopLevelKeys([]byte(body), allowed); err == nil {
			t.Errorf("%s: accepted", body)
		}
	}
	for _, body := range []string{`{"k":"1"}`, `{"K":"2"}`} {
		if err := requireExactTopLevelKeys([]byte(body), allowed); err != nil {
			t.Errorf("%s: %v", body, err)
		}
	}
}

func decodePlanBody(body string) (planCrossCheckRequest, error) {
	var req planCrossCheckRequest
	return req, json.Unmarshal([]byte(body), &req)
}

func TestPlanCrossCheckRequestExactKeys(t *testing.T) {
	good := []string{
		`{"stage":"code","claim_generation":1,"reason_class":"snapshot_failed"}`,
		`{"stage":"code","claim_generation":1,"repo_instructions_enabled":true,"repo_instructions_text":"x"}`,
		`{"stage":"plan","claim_generation":1,"plan_md":"x","milestones":[],"size_class":"s"}`,
	}
	for _, b := range good {
		if _, err := decodePlanBody(b); err != nil {
			t.Errorf("%s: %v", b, err)
		}
	}
	bad := []string{
		`{"stage":"code","claim_generation":1,"reason_class":"snapshot_failed","REPO_INSTRUCTIONS_ENABLED":null}`,
		`{"stage":"code","claim_generation":1,"reason_class":"snapshot_failed","REPO_INSTRUCTIONS_ENABLED":true}`,
		`{"stage":"code","claim_generation":1,"reason_class":"snapshot_failed","Repo_Instructions_Text":null}`,
		`{"stage":"code","claim_generation":1,"reason_class":"snapshot_failed","Repo_Instructions_Text":"x"}`,
		`{"stage":"plan","claim_generation":1,"plan_md":"x","REPO_INSTRUCTIONS_ENABLED":null}`,
		`{"stage":"plan","claim_generation":1,"plan_md":"x","Repo_Instructions_Text":"x"}`,
		`{"stage":"code","claim_generation":1,"PLAN_MD":"x"}`,
		`{"stage":"code","claim_generation":1,"Milestones":[]}`,
		`{"stage":"code","claim_generation":1,"Size_Class":"s"}`,
		`{"stage":"code","claim_generation":1,"repo_instructions_text":"a","REPO_INSTRUCTIONS_TEXT":"b"}`,
		`{"stage":"code","claim_generation":1,"plan_md":"a","plan_md":"b"}`,
		`{"stage":"code","claim_generation":1,"Stage":"plan"}`,
		`{"stage":"code","claim_generation":1} {"stage":"plan"}`,
		`{"stage":"code","claim_generation":1}x`,
		`[]`,
		`null`,
	}
	for _, b := range bad {
		if _, err := decodePlanBody(b); err == nil {
			t.Errorf("accepted %s", b)
		}
	}
}

func TestCrossCheckVerdictRequestExactKeys(t *testing.T) {
	for _, b := range []string{
		`{"claim_generation":1,"outcome":"clean","findings":[]}`,
		`{"claim_generation":1,"verdict":"approve","summary":"ok","items":[]}`,
	} {
		var req crossCheckVerdictRequest
		if err := json.Unmarshal([]byte(b), &req); err != nil {
			t.Errorf("%s: %v", b, err)
		}
	}
	for _, b := range []string{
		`{"claim_generation":1,"OUTCOME":"clean"}`,
		`{"claim_generation":1,"Findings":[]}`,
		`{"claim_generation":1,"VERDICT":"approve"}`,
		`{"claim_generation":1,"Summary":"x"}`,
		`{"claim_generation":1,"ITEMS":[]}`,
		`{"claim_generation":1,"verdict":"approve","VERDICT":"reject"}`,
		`{"claim_generation":1,"outcome":"a","outcome":"b"}`,
		`{"claim_generation":1} []`,
	} {
		var req crossCheckVerdictRequest
		if err := json.Unmarshal([]byte(b), &req); err == nil {
			t.Errorf("accepted %s", b)
		}
	}
}

func crossCheckRouteRequest(body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/worker/runs/x", strings.NewReader(body))
	route := chi.NewRouteContext()
	route.URLParams.Add("id", uuid.New().String())
	worker := store.Worker{ID: uuid.New(), UserID: uuid.New()}
	return req.WithContext(mw.ContextWithWorker(context.WithValue(req.Context(), chi.RouteCtxKey, route), worker))
}

// Case variants must be refused by the route exactly like lowercase keys, which
// fail "invalid pre-snapshot failure" before reaching the (nil) service.
func TestCrossCheckRoutesRefuseCaseVariantKeys(t *testing.T) {
	h := &Handler{}
	pre := `"stage":"code","claim_generation":1,"reason_class":"snapshot_failed"`
	for _, tc := range []struct {
		name, body string
		call       func(http.ResponseWriter, *http.Request)
	}{
		{"lowercase null", `{` + pre + `,"repo_instructions_enabled":null}`, h.WorkerSubmitPlanCrossCheck},
		{"upper null", `{` + pre + `,"REPO_INSTRUCTIONS_ENABLED":null}`, h.WorkerSubmitPlanCrossCheck},
		{"mixed text", `{` + pre + `,"Repo_Instructions_Text":"x"}`, h.WorkerSubmitPlanCrossCheck},
		{"plan stage upper", `{"stage":"plan","claim_generation":1,"plan_md":"x","REPO_INSTRUCTIONS_ENABLED":false}`, h.WorkerSubmitPlanCrossCheck},
		{"code with mixed plan fields", `{"stage":"code","claim_generation":1,"PLAN_MD":"x","Milestones":[]}`, h.WorkerSubmitPlanCrossCheck},
		{"verdict upper", `{"claim_generation":1,"OUTCOME":"clean","Findings":[]}`, h.WorkerCrossCheckVerdict},
		{"verdict plan upper", `{"claim_generation":1,"VERDICT":"approve"}`, h.WorkerCrossCheckVerdict},
		{"verdict duplicate", `{"claim_generation":1,"verdict":"approve","Verdict":"reject"}`, h.WorkerCrossCheckVerdict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tc.call(rec, crossCheckRouteRequest(tc.body))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// Audit pins: these decoders check presence on the bound value itself, so a
// case variant or mixed-case duplicate cannot make presence disagree with it.
func TestDispositionsPresenceFollowsBoundValue(t *testing.T) {
	for body, wantNil := range map[string]bool{
		`{"claim_generation":1,"dispositions":[]}`:                     false,
		`{"claim_generation":1,"DISPOSITIONS":[]}`:                     false,
		`{"claim_generation":1,"dispositions":[],"DISPOSITIONS":null}`: true,
		`{"claim_generation":1,"DISPOSITIONS":null,"dispositions":[]}`: false,
	} {
		var req struct {
			ClaimGeneration *int64            `json:"claim_generation"`
			Dispositions    []json.RawMessage `json:"dispositions"`
		}
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Fatal(err)
		}
		if (req.Dispositions == nil) != wantNil {
			t.Errorf("%s: nil=%v want %v", body, req.Dispositions == nil, wantNil)
		}
	}
}

func TestPatchRepoGuidancePresenceFollowsBoundValue(t *testing.T) {
	for body, want := range map[string]string{
		`{"code_cross_check_guidance":"x"}`:                                  `"x"`,
		`{"CODE_CROSS_CHECK_GUIDANCE":"x"}`:                                  `"x"`,
		`{"code_cross_check_guidance":"x","CODE_CROSS_CHECK_GUIDANCE":null}`: `null`,
		`{"CODE_CROSS_CHECK_GUIDANCE":null,"code_cross_check_guidance":"x"}`: `"x"`,
		`{}`: ``,
	} {
		var req patchRepoRequest
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Fatal(err)
		}
		if string(req.CodeCrossCheckGuidance) != want {
			t.Errorf("%s: bound %q want %q", body, req.CodeCrossCheckGuidance, want)
		}
	}
}

func TestCrossCheckToggleBindsLastCaseVariant(t *testing.T) {
	var req struct {
		Code *bool `json:"code"`
	}
	if err := json.Unmarshal([]byte(`{"code":true,"CODE":null}`), &req); err != nil || req.Code != nil {
		t.Fatalf("code=%v err=%v", req.Code, err)
	}
	if err := json.Unmarshal([]byte(`{"CODE":false,"code":true}`), &req); err != nil || req.Code == nil || !*req.Code {
		t.Fatalf("code=%v err=%v", req.Code, err)
	}
}
