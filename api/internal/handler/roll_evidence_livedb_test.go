package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/hostedsvc"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func TestRollEvidenceRegisterNamesAndPollLiveDB(t *testing.T) {
	h, pool, q, box, user := hostedLiveDB(t, "5")
	ctx := context.Background()
	h.wsvc = workersvc.New(q, box, workersvc.Params{CustodyHoldLimit: defaultCustodyLimit(), WorkerHeartbeatStale: time.Minute})
	h.wsvc.SetTxBeginner(pool)
	h.hsvc = hostedsvc.New(q, box, time.Now, time.Minute)
	worker, err := h.provisionHostedWorker(ctx, user, "user chosen", "base", "m", false, 5)
	if err != nil {
		t.Fatal(err)
	}
	current, old := uuid.New(), uuid.New()
	fullVersion := "1.2.3+" + strings.Repeat("a", maxSelfReportedBytes-len("1.2.3+"))
	for _, tc := range []struct{ name, kind, uid, version string }{
		{"uzi-pod:" + current.String(), "hosted", current.String(), "1.2.3"},
		{"uzi-pod:" + old.String(), "hosted", old.String(), "1.2.3"},
		{"", "hosted", "", "1.2.3"},
		{"legacy name", "hosted", "", "1.2.3"},
		{"uzi-pod:invalid", "hosted", "", "1.2.3"},
		{"uzi-pod:" + uuid.Nil.String(), "hosted", "", "1.2.3"},
		{"uzi-pod:" + current.String() + "suffix", "hosted", "", "1.2.3"},
		{"uzi-pod:" + current.String(), "external", "", "1.2.3"},
		{"uzi-pod:" + current.String(), "hosted", current.String(), fullVersion},
		// These two builds share the same UI prefix. Neither may prove that prefix
		// is the exact replacement version, even after a current-nonce heartbeat.
		{"uzi-pod:" + current.String(), "hosted", "", fullVersion + "a"},
		{"uzi-pod:" + current.String(), "hosted", "", fullVersion + "b"},
		{"uzi-pod:" + current.String(), "hosted", "", "1.2.3\n"},
	} {
		t.Run(tc.kind+"/"+tc.name+"/"+tc.version, func(t *testing.T) {
			if _, err := pool.Exec(ctx, "UPDATE workers SET kind=$2,hosted_size=CASE WHEN $2='hosted' THEN 'm' ELSE NULL END WHERE id=$1", worker.ID, tc.kind); err != nil {
				t.Fatal(err)
			}
			worker, err = q.GetWorkerByID(ctx, worker.ID)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := json.Marshal(map[string]any{"name": tc.name, "version": tc.version, "template": "base", "capabilities": []string{"docker"}, "protocol_capabilities": []string{"cross_check_v1", "cross_check_lane_v1"}, "max_cross_check_slots": 2})
			req := httptest.NewRequest(http.MethodPost, "/api/worker/register", strings.NewReader(string(body)))
			req = req.WithContext(mw.ContextWithWorker(ctx, worker))
			rec := httptest.NewRecorder()
			h.WorkerRegister(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("register %d: %s", rec.Code, rec.Body.String())
			}
			worker, err = q.GetWorkerByID(ctx, worker.ID)
			if err != nil {
				t.Fatal(err)
			}
			if worker.Name != "user chosen" || worker.RollHeartbeatAt.Valid || worker.Version.String != sanitizeSelfReported(tc.version, maxSelfReportedBytes) {
				t.Fatalf("register changed label or retained heartbeat: %+v", worker)
			}
			if tc.uid == "" {
				if worker.RegisteredPodUid.Valid {
					t.Fatal("invalid/legacy/external name stored pod UID")
				}
			} else if !worker.RegisteredPodUid.Valid || uuid.UUID(worker.RegisteredPodUid.Bytes).String() != tc.uid {
				t.Fatal("registration UID differs from transport")
			}
			worker, err = h.wsvc.Heartbeat(ctx, worker, nil, nil, &workersvc.ActiveSnapshot{RegisterNonce: worker.SnapshotRegisterNonce.String, SnapshotEpoch: 1})
			if err != nil {
				t.Fatal(err)
			}
			if worker.RollHeartbeatAt.Valid != (tc.uid != "") {
				t.Fatal("heartbeat identity proof mismatch")
			}
			if tc.uid == current.String() && tc.kind == "hosted" {
				if _, err := q.CordonHostedWorkerImageRoll(ctx, worker.ID); err != nil {
					t.Fatal(err)
				}
				poll := httptest.NewRecorder()
				h.ControllerPoll(poll, httptest.NewRequest(http.MethodGet, "/api/controller/poll", nil))
				if poll.Code != http.StatusOK || poll.Header().Get("Cache-Control") != "no-store" {
					t.Fatalf("poll %d: %s", poll.Code, poll.Body.String())
				}
				var result hostedsvc.PollResponse
				if err := json.Unmarshal(poll.Body.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				var found bool
				for _, desired := range result.Workers {
					if desired.ID == worker.ID.String() {
						found = true
						e := desired.RollEvidence
						if e == nil || e.Version != 1 || e.OwnerID != user.String() || !e.Registered || e.PodUID == nil || *e.PodUID != current.String() ||
							e.RegisterNonce == nil || *e.RegisterNonce != worker.SnapshotRegisterNonce.String || e.RegisteredVersion == nil || *e.RegisteredVersion != tc.version ||
							e.HeartbeatAt == nil || !e.HeartbeatAt.Equal(worker.RollHeartbeatAt.Time) || !e.ValidUntil.Equal(e.HeartbeatAt.Add(time.Minute)) ||
							e.OrdinaryCoverage || !e.CheckerCoverage || len(e.SchedulingCapabilities) != 1 || len(e.ProtocolCapabilities) != 2 ||
							desired.DrainingReason == nil || *desired.DrainingReason != "image_roll" || desired.DrainingSince == nil {
							t.Fatalf("incomplete poll evidence: %+v", desired)
						}
					}
				}
				if !found {
					t.Fatal("registered worker absent from real poll handler")
				}
			}
		})
	}
}

func hostedWorkerParams(user uuid.UUID, name string) store.CreateHostedWorkerParams {
	return store.CreateHostedWorkerParams{UserID: user, Name: name, TokenHash: []byte(uuid.NewString()), TemplateDeclared: pgconv.TextOrNull("base"), HostedSize: pgconv.TextOrNull("m"), AnthropicBindMode: "default"}
}

func TestRollEvidencePollOverflowLiveDB(t *testing.T) {
	h, pool, _, box, user := hostedLiveDB(t, "5")
	ctx := context.Background()
	// Isolate the complete-fleet boundary within a rolled-back transaction:
	// do not alter earlier fixtures or leave 513 workers for later packages.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	if _, err := tx.Exec(ctx, "UPDATE workers SET kind='external',hosted_size=NULL WHERE kind='hosted'"); err != nil {
		t.Fatal(err)
	}
	h.hsvc = hostedsvc.New(q, box, time.Now, time.Minute)
	for i := 0; i < 512; i++ {
		if _, err := q.CreateHostedWorker(ctx, hostedWorkerParams(user, fmt.Sprintf("fleet-%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	rec := httptest.NewRecorder()
	h.ControllerPoll(rec, httptest.NewRequest(http.MethodGet, "/api/controller/poll", nil))
	var result hostedsvc.PollResponse
	if rec.Code != http.StatusOK {
		t.Fatalf("512 poll: %d %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil || len(result.Workers) != 512 {
		t.Fatalf("512 snapshot len=%d err=%v", len(result.Workers), err)
	}
	if _, err := q.CreateHostedWorker(ctx, hostedWorkerParams(user, "overflow")); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	h.ControllerPoll(rec, httptest.NewRequest(http.MethodGet, "/api/controller/poll", nil))
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "\"workers\"") {
		t.Fatalf("overflow emitted partial snapshot: %d %s", rec.Code, rec.Body.String())
	}
}
