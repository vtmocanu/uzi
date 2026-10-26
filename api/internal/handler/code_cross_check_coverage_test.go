package handler

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestCodeCrossCheckCoverageRawStageField(t *testing.T) {
	for _, value := range []string{"null", "false", "[]", `{"partial":true}`} {
		var req crossCheckVerdictRequest
		raw := `{"claim_generation":1,"coverage":` + value + "}"
		if err := json.Unmarshal([]byte(raw), &req); err != nil || !req.codeFields || req.planFields || string(req.Coverage) != value {
			t.Fatalf("raw coverage %s: %+v", value, req)
		}
	}
}
func TestCodeCrossCheckCoverageDTO(t *testing.T) {
	raw, err := json.Marshal(codeCrossCheckResponse(store.CrossCheck{}))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || string(fields["coverage"]) != "null" {
		t.Fatalf("legacy coverage: %s", raw)
	}
	account := `{"version":1,"entries":[],"inventory_incomplete":true,"omitted_paths":0,"omitted_count_exact":false,"unknown_judgments":0,"invalid_judgments":0,"omitted_judgments":0,"activity_complete":false,"reasons":["coverage_invalid"]}`
	response := codeCrossCheckResponse(store.CrossCheck{Coverage: []byte(account)})
	raw, err = json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal(raw, &fields) != nil || string(fields["coverage"]) != account {
		t.Fatalf("coverage changed: %s", raw)
	}
	names := []string{}
	for name := range jsonWireNames(reflect.TypeOf(apitypes.CodeCrossCheckCoverage{})) {
		names = append(names, name)
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "activity_complete,entries,invalid_judgments,inventory_incomplete,omitted_count_exact,omitted_judgments,omitted_paths,reasons,unknown_judgments,version" {
		t.Fatalf("coverage wire keys: %v", names)
	}
	names = nil
	for name := range jsonWireNames(reflect.TypeOf(apitypes.CodeCrossCheckCoverageEntry{})) {
		names = append(names, name)
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "path,reason,recorded_read,status" {
		t.Fatalf("entry keys: %v", names)
	}
}
