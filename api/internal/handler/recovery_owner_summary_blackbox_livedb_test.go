package handler

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/clitoken"
)

// TestOwnerSummaryClosureMetadataBlackboxLiveDB must fail on the baseline
// specifically because the persisted closure metadata is missing from Summary.
// It decodes generically and needs no owner DTO or new listing row type.
func TestOwnerSummaryClosureMetadataBlackboxLiveDB(t *testing.T) {
	f := newOwnerSummaryFixture(t)
	token := cliMintToken(t, f.pool, f.owner, clitoken.ScopeUser)
	rec := bearerReq(f.router, http.MethodGet, "/api/runs/"+f.run.String()+"/archives", token)
	if rec.Code != http.StatusOK {
		t.Fatalf("Summary status=%d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Archives []map[string]json.RawMessage `json:"archives"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Archives) != 1 {
		t.Fatalf("archives=%d want one real capture", len(got.Archives))
	}
	var id string
	if err := json.Unmarshal(got.Archives[0]["id"], &id); err != nil || id != f.capture.String() {
		t.Fatalf("capture id=%q err=%v", id, err)
	}
	raw, ok := got.Archives[0]["prerequisite_closure"]
	if !ok {
		t.Fatal("Summary missing prerequisite_closure metadata for a real persisted pending closure")
	}
	var closure map[string]any
	if err := json.Unmarshal(raw, &closure); err != nil {
		t.Fatal(err)
	}
	if closure["state"] != "pending" || closure["pin_ref"] != "refs/uzi-prereq/"+id {
		t.Fatalf("persisted closure metadata=%v", closure)
	}
	if _, ok := closure["pin_tip"]; ok {
		t.Fatal("pending closure exposed pin_tip before invocation")
	}
}
