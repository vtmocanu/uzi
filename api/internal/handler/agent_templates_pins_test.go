package handler

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/agenttmpl"
)

func TestTemplatePinRequestSemantics(t *testing.T) {
	for _, tc := range []struct {
		name, body                 string
		present, valid, customized bool
		want                       string
	}{
		{"omitted", "{}", false, true, true, "gpt-6-sol"},
		{"null", `{"codex_model":null}`, true, false, false, ""},
		{"blank", `{"codex_model":"  "}`, true, false, false, ""},
		{"explicit", `{"codex_model":" gpt-6-luna "}`, true, true, true, "gpt-6-luna"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := seededRow(t, "coder")
			row.CodexModel = pgtype.Text{String: "gpt-6-sol", Valid: true}
			dto := templateDTO(row)
			req := templateWriteRequest{Description: dto.Description, Model: dto.Model, Tools: dto.Tools, PromptBody: dto.PromptBody}
			if err := json.Unmarshal([]byte(tc.body), &req); err != nil {
				t.Fatal(err)
			}
			if req.CodexModel.Present != tc.present {
				t.Fatal("presence mismatch")
			}
			fields, err := validateTemplateFields(req)
			if err != nil {
				t.Fatal(err)
			}
			// Create has no current row to merge: omission is NULL.
			if !tc.present && fields.codexModel.Valid {
				t.Fatal("omitted create pin is not NULL")
			}
			fields = mergeOmittedTemplatePins(req, fields, row)
			if fields.codexModel.Valid != tc.valid || fields.codexModel.String != tc.want {
				t.Fatalf("merged pin=%+v", fields.codexModel)
			}
			if got := updateMarksCustomized(row, fields); got != tc.customized {
				t.Errorf("customized=%v want %v", got, tc.customized)
			}
			if got := differsFromBuiltin(row); !got {
				t.Error("stored Codex pin must drift")
			}
			if got := templateToDefinition(row).CodexModel; got != "gpt-6-sol" {
				t.Errorf("definition pin=%q", got)
			}
			if dto.CodexModel == nil || *dto.CodexModel != "gpt-6-sol" {
				t.Error("DTO lost pin")
			}
		})
	}
}

func TestTemplatePinValidationErrorsNameField(t *testing.T) {
	for _, tc := range []struct{ body, field string }{
		{`{"model":"gpt-6-luna"}`, "model:"},
		{`{"codex_model":"sonnet"}`, "codex_model:"},
		{`{"codex_model":"custom-codex"}`, "codex_model:"},
		{`{"codex_model":"gpt-6-sol\u202e"}`, "codex_model:"},
	} {
		req := templateWriteRequest{Description: "Code", PromptBody: "Code"}
		if err := json.Unmarshal([]byte(tc.body), &req); err != nil {
			t.Fatal(err)
		}
		if _, err := validateTemplateFields(req); err == nil || !strings.HasPrefix(err.Error(), tc.field) {
			t.Errorf("%s: error=%v", tc.body, err)
		}
	}
	for _, body := range []string{`{"codex_model":1}`, `{"codex_model":{}}`, `{"codex_model":[]}`} {
		var req templateWriteRequest
		if err := json.Unmarshal([]byte(body), &req); err == nil {
			t.Errorf("accepted %s", body)
		}
	}
}

func TestTemplateEffortRequestSemantics(t *testing.T) {
	for _, tc := range []struct {
		name, body                 string
		present, valid, customized bool
		want                       string
	}{
		{"omitted keeps", "{}", false, true, true, "high"},
		{"null clears", `{"effort":null}`, true, false, false, ""},
		{"blank clears", `{"effort":""}`, true, false, false, ""},
		{"whitespace clears", `{"effort":"  \t"}`, true, false, false, ""},
		{"explicit trimmed", `{"effort":" max "}`, true, true, true, "max"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := seededRow(t, "coder")
			row.Effort = pgtype.Text{String: "high", Valid: true}
			dto := templateDTO(row)
			req := templateWriteRequest{Description: dto.Description, Model: dto.Model, Tools: dto.Tools, PromptBody: dto.PromptBody}
			if err := json.Unmarshal([]byte(tc.body), &req); err != nil {
				t.Fatal(err)
			}
			if req.Effort.Present != tc.present {
				t.Fatal("presence mismatch")
			}
			fields, err := validateTemplateFields(req)
			if err != nil {
				t.Fatal(err)
			}
			// Create has no current row to merge: omission is NULL.
			if !tc.present && fields.effort.Valid {
				t.Fatal("omitted create effort is not NULL")
			}
			fields = mergeOmittedTemplatePins(req, fields, row)
			if fields.effort.Valid != tc.valid || fields.effort.String != tc.want {
				t.Fatalf("merged effort=%+v", fields.effort)
			}
			// The seeded builtin ships no effort, so any stored effort is customized.
			if got := updateMarksCustomized(row, fields); got != tc.customized {
				t.Errorf("customized=%v want %v", got, tc.customized)
			}
			if !differsFromBuiltin(row) {
				t.Error("a stored effort pin must drift from the shipped NULL")
			}
			if got := templateToDefinition(row).Effort; got != "high" {
				t.Errorf("definition effort=%q", got)
			}
			if dto.Effort == nil || *dto.Effort != "high" || dto.CodexEffort != nil {
				t.Errorf("DTO effort=%v codex_effort=%v", dto.Effort, dto.CodexEffort)
			}
		})
	}
}

func TestTemplateCodexEffortIsIndependentOfEffort(t *testing.T) {
	row := seededRow(t, "coder")
	row.Effort = pgtype.Text{String: "low", Valid: true}
	row.CodexEffort = pgtype.Text{String: "xhigh", Valid: true}
	var req templateWriteRequest
	if err := json.Unmarshal([]byte(`{"effort":null}`), &req); err != nil {
		t.Fatal(err)
	}
	req.Description, req.PromptBody = "d", "p"
	fields, err := validateTemplateFields(req)
	if err != nil {
		t.Fatal(err)
	}
	fields = mergeOmittedTemplatePins(req, fields, row)
	if fields.effort.Valid || !fields.codexEffort.Valid || fields.codexEffort.String != "xhigh" {
		t.Fatalf("effort=%+v codex_effort=%+v", fields.effort, fields.codexEffort)
	}
}

func TestTemplateEffortValidationErrorsNameField(t *testing.T) {
	for _, tc := range []struct{ body, field string }{
		{`{"effort":"runtime"}`, "effort:"},
		{`{"effort":"High"}`, "effort:"},
		{`{"effort":"extreme"}`, "effort:"},
		{`{"effort":"hi gh"}`, "effort:"},
		{`{"codex_effort":"runtime"}`, "codex_effort:"},
		{`{"codex_effort":"ultra"}`, "codex_effort:"},
	} {
		req := templateWriteRequest{Description: "Code", PromptBody: "Code"}
		if err := json.Unmarshal([]byte(tc.body), &req); err != nil {
			t.Fatal(err)
		}
		if _, err := validateTemplateFields(req); err == nil || !strings.HasPrefix(err.Error(), tc.field) {
			t.Errorf("%s: error=%v", tc.body, err)
		}
	}
	for _, body := range []string{`{"effort":1}`, `{"codex_effort":{}}`, `{"effort":[]}`} {
		var req templateWriteRequest
		if err := json.Unmarshal([]byte(body), &req); err == nil {
			t.Errorf("accepted %s", body)
		}
	}
}

func TestBuiltinDefinitionViewCarriesEffortPins(t *testing.T) {
	dto := builtinDefinitionView(agenttmpl.Definition{Name: "n", Effort: " high ", CodexEffort: "  "})
	if dto.Effort == nil || *dto.Effort != "high" || dto.CodexEffort != nil {
		t.Fatalf("effort=%v codex_effort=%v", dto.Effort, dto.CodexEffort)
	}
	raw, err := json.Marshal(builtinDefinitionView(agenttmpl.Definition{Name: "n"}))
	if err != nil || !strings.Contains(string(raw), `"effort":null`) || !strings.Contains(string(raw), `"codex_effort":null`) {
		t.Fatalf("a shipped definition without efforts must serialize explicit nulls: %s err=%v", raw, err)
	}
}
