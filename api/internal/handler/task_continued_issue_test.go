package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func TestLinkedTaskRequestAndErrors(t *testing.T) {
	for _, tc := range []struct {
		body           string
		valid, present bool
	}{
		{`{"context":"task"}`, true, false},
		{`{"context":"task","continued_issue_iid":null}`, true, false},
		{`{"context":"task","continued_issue_iid":42}`, true, true},
		{`{"context":"task","continued_issue_iid":9223372036854775808}`, false, false},
	} {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body))
		var body CreateTaskRunRequest
		err := httpx.DecodeJSON(req, &body)
		if (err == nil) != tc.valid || (err == nil && (body.ContinuedIssueIID != nil) != tc.present) {
			t.Fatalf("decode %s: %+v %v", tc.body, body, err)
		}
	}
	for _, tc := range []struct {
		err    error
		status int
		text   string
	}{
		{workersvc.ErrContinuedIssueInvalid, 400, "not a pull request"},
		{workersvc.ErrForgeIssueRead, 502, "could not read"},
		{workersvc.ErrContinuedIssueNotFound, 404, "continued issue not found on this repository's forge"},
		{workersvc.ErrActiveRunExists, 409, "a run is already in progress for this issue"},
	} {
		rec := httptest.NewRecorder()
		(&Handler{}).writeStartRunError(rec, httptest.NewRequest(http.MethodPost, "/", nil), tc.err)
		if rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.text) {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	}
}

func TestLinkedTaskDTOAndSummary(t *testing.T) {
	for _, link := range []pgtype.Int8{{}, {Int64: 42, Valid: true}} {
		dto := runToDTO(store.Run{Kind: "task", ContinuedIssueIid: link}, "normal", 0, 0, 0, dtoTestNow, 0)
		if dto.IssueIID != nil || (dto.ContinuedIssueIID != nil) != link.Valid {
			t.Fatalf("detail=%+v", dto)
		}
		summary := apitypes.RunSummaryOf(apitypes.RunListItemDTO{RunDTO: dto})
		back := summary.ListItem()
		for _, v := range []any{dto, summary, back} {
			raw, err := json.Marshal(v)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatal(err)
			}
			want := "null"
			if link.Valid {
				want = "42"
			}
			if string(fields["continued_issue_iid"]) != want || string(fields["issue_iid"]) != "null" {
				t.Fatalf("wire identity=%s", raw)
			}
		}
	}
}
