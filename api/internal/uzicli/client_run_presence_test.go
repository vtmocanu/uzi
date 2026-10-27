package uzicli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

func TestCreatedRunResultFlattenedNotice(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timing  string
		present bool
		finite  bool
	}{
		{name: "omitted"},
		{name: "null", timing: `,"recovery_retry_not_before":null`, present: true},
		{name: "finite", timing: `,"recovery_retry_not_before":"2099-01-01T00:00:00Z"`, present: true, finite: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"id":"r1","status":"recovery_wait","repo_path":"group/repo","issue_description":"full run detail","created_at":"2026-10-27T12:00:00Z","Notice":"queued by maintenance"` + tc.timing + `}`
			var got CreatedRunResult
			if err := json.Unmarshal([]byte(body), &got); err != nil {
				t.Fatal(err)
			}
			if got.Notice != "queued by maintenance" || got.ID != "r1" ||
				got.Status != "recovery_wait" || got.RepoPath != "group/repo" ||
				got.IssueDescription != "full run detail" ||
				got.CreatedAt.Format("2006-01-02T15:04:05Z07:00") != "2026-10-27T12:00:00Z" ||
				got.RecoveryRetryNotBeforePresent != tc.present ||
				(got.RecoveryRetryNotBefore != nil) != tc.finite {
				t.Fatalf("flattened reply lost fields: %+v", got)
			}
			if tc.finite && got.RecoveryRetryNotBefore.Format("2006-01-02T15:04:05Z07:00") != "2099-01-01T00:00:00Z" {
				t.Fatalf("retry time = %v", got.RecoveryRetryNotBefore)
			}
			wire, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(wire), `"Notice":"queued by maintenance"`) ||
				strings.Contains(string(wire), `"notice":`) {
				t.Fatalf("notice marshal key changed: %s", wire)
			}
			var roundTrip CreatedRunResult
			if err := json.Unmarshal(wire, &roundTrip); err != nil {
				t.Fatal(err)
			}
			// RunDTO marshals its nil timing pointer as explicit null.
			want := got
			want.RecoveryRetryNotBeforePresent = true
			if !reflect.DeepEqual(roundTrip, want) {
				t.Fatal("round trip changed run fields or notice")
			}
		})
	}
}

func TestCreatedRunResultNoticeKeyCompatibility(t *testing.T) {
	for _, tc := range []struct {
		body string
		want string
	}{
		{`{"notice":"lower"}`, "lower"},
		{`{"NOTICE":"upper"}`, "upper"},
		{`{"nOtIcE":"mixed"}`, "mixed"},
		{`{"Notice":"first","notice":"last"}`, "last"},
		{`{"notice":"first","Notice":"last"}`, "last"},
		{`{"Notice":"first","NOTICE":null}`, "first"},
		{`{}`, "existing"},
		{`{"Notice":null}`, "existing"},
	} {
		t.Run(tc.body, func(t *testing.T) {
			got := CreatedRunResult{Notice: "existing"}
			if err := json.Unmarshal([]byte(tc.body), &got); err != nil {
				t.Fatal(err)
			}
			if got.Notice != tc.want {
				t.Fatalf("notice = %q, want %q", got.Notice, tc.want)
			}
		})
	}
}

func TestCreatedRunResultRejectsInvalidJSON(t *testing.T) {
	for _, body := range []string{
		`{"Notice":42}`,
		`{"Notice":{}}`,
		`{"notice":false,"Notice":"later valid value"}`,
		`{"id":42,"Notice":"notice"}`,
		`{"created_at":"invalid","Notice":"notice"}`,
		`{"recovery_retry_not_before":"invalid","Notice":"notice"}`,
		`{"recovery_retry_not_before":42,"Notice":"notice"}`,
		`{"Notice":`,
		`[]`,
	} {
		t.Run(body, func(t *testing.T) {
			var got CreatedRunResult
			if err := json.Unmarshal([]byte(body), &got); err == nil {
				t.Fatal("invalid reply accepted")
			}
		})
	}
}

func TestHTTPRecoveryTimingPresence(t *testing.T) {
	for _, field := range []string{"", `,"recovery_retry_not_before":null`, `,"recovery_retry_not_before":"2099-01-01T00:00:00Z"`} {
		t.Run(field, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				row := `{"id":"r1","status":"recovery_wait","recovery_wait_cause":"codex_idle_stall","repo_path":"group/repo"` + field + `}`
				if r.URL.Path == "/api/runs" {
					_, _ = fmt.Fprintf(w, `{"runs":[%s]}`, row)
				} else {
					_, _ = fmt.Fprintf(w, `{"run":%s}`, row)
				}
			}))
			defer srv.Close()
			client := newTestClient(srv)
			full, err := client.GetRun(context.Background(), "r1")
			if err != nil {
				t.Fatal(err)
			}
			list, err := client.ListRuns(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			summary, err := client.ListRunSummaries(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for _, run := range []apitypes.RunDTO{full, list[0].RunDTO, summary[0].ListItem().RunDTO} {
				if run.RecoveryRetryNotBeforePresent != (field != "") {
					t.Fatalf("presence lost: %+v", run)
				}
			}
		})
	}
}
