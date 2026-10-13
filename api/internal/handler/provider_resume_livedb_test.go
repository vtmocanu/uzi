package handler

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestProviderResumeHintLiveDB(t *testing.T) {
	e := newWallParkExtendHTTPEnv(t, 57600)
	e.iid++
	id := uuid.New()
	e.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description,
		status, worker_id, recovery_wait_cause, provider_park_count, provider_park_streak_started_at)
		VALUES ($1,$2,$3,'issue',$4,'t','d','recovery_wait',$5,'provider_outage',3,now()-interval '25 hours')`,
		id, e.owner.ID, e.repoID, e.iid, e.worker)

	for _, tc := range []struct {
		name  string
		bound time.Duration
		want  string
	}{
		{"disabled", 0, "run is waiting out a provider outage and retries automatically"},
		{"enabled", 24 * time.Hour, "run is waiting out a provider outage and retries automatically; once the outage streak passes its limit it pauses for you to resume"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e.h.cfg.RunProviderParkMaxDuration = tc.bound
			rec := e.post(e.h.ResumeRunNow, id, "")
			if rec.Code != http.StatusConflict {
				t.Fatalf("code = %d, want 409; body=%s", rec.Code, rec.Body.String())
			}
			var body struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Error != tc.want {
				t.Fatalf("error = %q, want %q", body.Error, tc.want)
			}
			if row := e.providerHoldRow(id); row.status != "recovery_wait" || row.count != 3 || !row.startValid {
				t.Fatalf("refused resume changed the provider park: %+v", row)
			}
			var inputs int
			if err := e.query("SELECT count(*) FROM run_user_inputs WHERE run_id=$1 AND kind='resume'", id).Scan(&inputs); err != nil {
				t.Fatal(err)
			}
			if inputs != 0 {
				t.Fatalf("refused resume wrote %d audit inputs", inputs)
			}
		})
	}
}
