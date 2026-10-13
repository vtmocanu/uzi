package apiclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
)

func TestReportReadinessSetsKindOnWire(t *testing.T) {
	for _, kind := range []string{"", "fleet", "readiness"} {
		t.Run("input-"+kind, func(t *testing.T) {
			reports := make(chan protocol.StatusReport, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/api/controller/status" {
					t.Errorf("request = %s %s", r.Method, r.URL.Path)
				}
				var report protocol.StatusReport
				if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
					t.Error(err)
				}
				reports <- report
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()
			in := protocol.StatusReport{
				Kind: kind, ReportedAt: time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC),
				PollIntervalSeconds: 10, WorkerImageTag: "tag",
				Workers: []protocol.WorkerStatus{{ID: "worker", Phase: protocol.PhaseSettled}},
			}
			c := New(server.URL, "token", time.Second, nil, nil)
			if err := c.ReportReadiness(context.Background(), in); err != nil {
				t.Fatal(err)
			}
			want := in
			want.Kind = "readiness"
			if got := <-reports; !reflect.DeepEqual(got, want) {
				t.Fatalf("readiness wire = %+v, want %+v", got, want)
			}
			if in.Kind != kind {
				t.Fatal("caller report mutated")
			}
		})
	}
}
