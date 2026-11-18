package handler

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/google/uuid"
)

// TestResumeRunNowProviderGatewayLimitLiveDB (PRD #4320 module 5): the owner's Resume of a provider
// gateway-limit park re-queues the run from every row of the schedule (the three timed rows and the
// owner-wait row), resets model_provider_limit_streak to 0, clears retry_not_before and writes the
// resume audit row; a vendor limit_wait and a provider-bound park of another type stay 409.
//
// MUTATION: delete the limit_wait arm of ResumeRunNow and every case 409s; widen the query's
// model_provider_key_mode predicate away and the vendor case resumes.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.
func TestResumeRunNowProviderGatewayLimitLiveDB(t *testing.T) {
	e := newWallParkExtendHTTPEnv(t, 57600)
	park := func(typ string, bound bool, streak int32, retry string) uuid.UUID {
		e.iid++
		id := uuid.New()
		mode := "NULL"
		if bound {
			mode = "'user_key'"
		}
		binding := "NULL, NULL, NULL, NULL"
		if bound {
			binding = "$7, 1, $8, 1"
		}
		args := []any{id, e.owner.ID, e.repoID, e.iid, e.worker, typ}
		sql := `INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id,
		    rate_limit_type, limit_wait_count, retry_not_before, model_provider_limit_streak, started_at, status_since,
		    model_provider_key_mode, model_provider_id, model_provider_revision, model_provider_key_secret_id, model_provider_key_rev)
		    VALUES ($1,$2,$3,'issue',$4,'t','d','limit_wait',$5,$6,1,` + retry + `,` + itoa32(streak) + `, now() - interval '1 hour', now() - interval '1 minute',` + mode + `, ` + binding + `)`
		if bound {
			args = append(args, uuid.New(), uuid.New())
		} else {
			sql = `INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id,
		    rate_limit_type, limit_wait_count, retry_not_before, model_provider_limit_streak, started_at, status_since)
		    VALUES ($1,$2,$3,'issue',$4,'t','d','limit_wait',$5,$6,1,` + retry + `,` + itoa32(streak) + `, now() - interval '1 hour', now() - interval '1 minute')`
		}
		e.exec(sql, args...)
		return id
	}
	type row struct {
		status string
		streak int32
		retry  *string
	}
	read := func(id uuid.UUID) row {
		var r row
		if err := e.query(`SELECT status, model_provider_limit_streak, retry_not_before::text FROM runs WHERE id = $1`, id).Scan(&r.status, &r.streak, &r.retry); err != nil {
			t.Fatal(err)
		}
		return r
	}
	for _, c := range []struct {
		name   string
		streak int32
		retry  string
	}{
		{"row 1m", 1, "now() + interval '1 minute'"},
		{"row 5m", 2, "now() + interval '5 minutes'"},
		{"row 15m", 3, "now() + interval '15 minutes'"},
		{"owner wait 3+1", 4, "NULL"},
		{"owner wait 4+1", 5, "NULL"},
	} {
		t.Run(c.name, func(t *testing.T) {
			id := park("provider_gateway_limit", true, c.streak, c.retry)
			rec := e.post(e.h.ResumeRunNow, id, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("code = %d, want 200; body=%s", rec.Code, rec.Body.String())
			}
			var body struct {
				Run struct {
					Status string `json:"status"`
				} `json:"run"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Run.Status != "queued" {
				t.Fatalf("response run status = %q (%v), want queued", body.Run.Status, err)
			}
			if got := read(id); got.status != "queued" || got.streak != 0 || got.retry != nil {
				t.Fatalf("row after resume = %+v, want queued, streak 0, retry NULL", got)
			}
			var inputs int
			if err := e.query(`SELECT count(*) FROM run_user_inputs WHERE run_id = $1 AND kind = 'resume'`, id).Scan(&inputs); err != nil || inputs != 1 {
				t.Fatalf("resume audit rows = %d (%v), want 1", inputs, err)
			}
		})
	}
	for name, id := range map[string]uuid.UUID{
		"vendor five_hour park":                 park("five_hour", false, 0, "now() + interval '1 hour'"),
		"vendor park forging the gateway type":  park("provider_gateway_limit", false, 0, "now() + interval '1 hour'"),
		"provider-bound park of another window": park("five_hour", true, 2, "now() + interval '1 hour'"),
	} {
		t.Run(name, func(t *testing.T) {
			before := read(id)
			rec := e.post(e.h.ResumeRunNow, id, "")
			if rec.Code != http.StatusConflict {
				t.Fatalf("code = %d, want 409; body=%s", rec.Code, rec.Body.String())
			}
			if after := read(id); after.status != "limit_wait" || after.streak != before.streak {
				t.Fatalf("refused resume changed the row: %+v -> %+v", before, after)
			}
		})
	}
}

func itoa32(n int32) string { return strconv.Itoa(int(n)) }
