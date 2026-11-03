package handler

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
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
