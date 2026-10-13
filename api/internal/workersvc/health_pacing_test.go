package workersvc

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestHealthPacingQueuedReasons(t *testing.T) {
	for _, tc := range []struct {
		name, state       string
		age               time.Duration
		suitable, rolling int64
		want              string
	}{
		{"held", "held", 0, 1, 1, "waiting for worker roll (held) to repo:bad: ImagePullBackOff"},
		{"active", "active", 0, 1, 1, "waiting for worker roll (active) to repo:bad: ImagePullBackOff"},
		{"evidence", "waiting_evidence", 0, 1, 1, "waiting for worker roll (waiting_evidence) to repo:bad: ImagePullBackOff"},
		{"override", "override", 0, 1, 1, "waiting for worker roll (override) to repo:bad: ImagePullBackOff"},
		{"busy retained", "held", 0, 2, 1, reasonNoWorker},
		{"no candidates", "held", 0, 0, 0, reasonNoWorker},
		{"stale", "held", 61 * time.Second, 1, 1, reasonNoWorker},
		{"invalid", "unknown", 0, 1, 1, reasonNoWorker},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(map[string]any{
				"pacing":      map[string]any{"state": tc.state, "target_image": "repo:bad", "blocking_reason": "ImagePullBackOff"},
				"observed_at": t0.Add(-tc.age),
			})
			if err != nil {
				t.Fatal(err)
			}
			fs := &healthFakeStore{eligibility: store.CountOnlineWorkersClaimableForRunRow{
				DiagnosticSuitable: tc.suitable, DiagnosticRolling: tc.rolling, DiagnosticRollContext: data,
			}}
			svc := healthSvc(fs, defaultHealthSettings())
			if got := svc.queuedReason(context.Background(), t0, runRow("queued")); got != tc.want {
				t.Fatalf("reason=%q want=%q", got, tc.want)
			}
			if len(fs.writes) != 0 {
				t.Fatal("diagnostic lookup wrote health")
			}
		})
	}
}

func TestParseFreshPacingReport(t *testing.T) {
	now := time.Now()
	data := []byte(`{"state":"held","target_image":"repo:\u001bv2","blocking_reason":"ImagePullBackOff"}`)
	for _, tc := range []struct {
		name    string
		receipt pgtype.Timestamptz
		want    bool
	}{
		{"fresh", pgtype.Timestamptz{Time: now, Valid: true}, true},
		{"boundary", pgtype.Timestamptz{Time: now.Add(-DefaultControllerSignalTTL), Valid: true}, true},
		{"expired", pgtype.Timestamptz{Time: now.Add(-DefaultControllerSignalTTL - time.Nanosecond), Valid: true}, false},
		{"future", pgtype.Timestamptz{Time: now.Add(time.Nanosecond), Valid: true}, false},
		{"absent", pgtype.Timestamptz{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseFreshPacingReport(data, tc.receipt, now)
			if (got != nil) != tc.want {
				t.Fatalf("report=%+v", got)
			}
			if got != nil && got.TargetImage != "repo:v2" {
				t.Fatalf("unsafe image %q", got.TargetImage)
			}
		})
	}
	for _, invalid := range []string{`{"state":"unknown"}`, `{"state":"held","canary_id":"bad"}`, `{"state":"held","target_image":"` + strings.Repeat("x", 513) + `"}`} {
		if got := ParseFreshPacingReport([]byte(invalid), pgtype.Timestamptz{Time: now, Valid: true}, now); got != nil {
			t.Fatalf("invalid accepted %+v", got)
		}
	}
}
