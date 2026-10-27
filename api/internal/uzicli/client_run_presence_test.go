package uzicli

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

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
