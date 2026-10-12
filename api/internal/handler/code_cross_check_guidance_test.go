package handler

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestCodeCrossCheckPlanRequestOmitsAbsentInstructionFields(t *testing.T) {
	raw, err := json.Marshal(planCrossCheckRequest{Stage: "plan"})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"repo_instructions_enabled", "repo_instructions_text"} {
		if _, present := fields[key]; present {
			t.Fatalf("legacy plan request includes %s: %s", key, raw)
		}
	}
	var request planCrossCheckRequest
	if err = json.Unmarshal(raw, &request); err != nil || request.instructionFields {
		t.Fatalf("legacy plan decode: %v", err)
	}
}

func TestCodeCrossCheckSummaryFrozenMetadata(t *testing.T) {
	cc := store.CrossCheck{GuidanceText: pgtype.Text{String: "private owner checks", Valid: true},
		GuidanceDigest: []byte{1, 2}, RepoInstructionsEnabled: true,
		RepoInstructionsText: pgtype.Text{String: "private instructions", Valid: true}, RepoInstructionsDigest: []byte{3, 4}}
	raw, err := json.Marshal(codeCrossCheckResponse(cc))
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"custom_guidance_applied": "true", "guidance_digest": `"0102"`, "repo_instructions_enabled": "true", "repo_instructions_digest": `"0304"`} {
		if string(body[key]) != want {
			t.Fatalf("%s=%s", key, body[key])
		}
	}
	if strings.Contains(string(raw), "private") || len(body["guidance_text"]) != 0 || len(body["repo_instructions_text"]) != 0 {
		t.Fatalf("raw summary text: %s", raw)
	}
}

// The worker decodes this body with a strict key set; pin it here so the two sides cannot drift.
func TestCodeCrossCheckNoRowBodyKeys(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		raw, err := json.Marshal(codeCrossCheckNoRowBody(enabled))
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatal(err)
		}
		keys := make([]string, 0, len(body))
		for key := range body {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		if got := strings.Join(keys, ","); got != "repo_instructions_enabled,result,stage" {
			t.Fatalf("no-row keys = %s", got)
		}
		if string(body["stage"]) != `"code"` || string(body["result"]) != `"no_row"` ||
			string(body["repo_instructions_enabled"]) != strconv.FormatBool(enabled) {
			t.Fatalf("no-row body = %s", raw)
		}
	}
}
