package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type workerBlockerReadCapture struct {
	*pgxpool.Pool
	batches [][]uuid.UUID
	failure error
}

func (c *workerBlockerReadCapture) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if strings.Contains(sql, "-- name: ListWorkerUpgradeBlockers :many") {
		ids := args[0].([]uuid.UUID)
		c.batches = append(c.batches, append([]uuid.UUID(nil), ids...))
		if c.failure != nil {
			return nil, c.failure
		}
	}
	return c.Pool.Query(ctx, sql, args...)
}

func TestWorkerUpgradeBlockersOwnerAdminLiveDB(t *testing.T) {
	h, router, pool := cliLiveDB(t)
	capture := &workerBlockerReadCapture{Pool: pool}
	h.q = store.New(capture)
	owner := cliSeedUser(t, pool, false)
	other := cliSeedUser(t, pool, false)
	admin := cliSeedUser(t, pool, true)
	ownerToken := cliMintToken(t, pool, owner, clitoken.ScopeUser)
	adminToken := cliMintToken(t, pool, admin, clitoken.ScopeAdminRO)
	worker := func(u uuid.UUID, kind string, drain bool) uuid.UUID {
		id := uuid.New()
		var size any
		if kind == "hosted" {
			size = "m"
		}
		cliMustExec(t, pool, `INSERT INTO workers (id,user_id,name,token_hash,status,kind,hosted_size,template_declared,draining_since,version,last_heartbeat_at)
			VALUES ($1,$2,'blocker-http',$3,'online',$4,$5,'base',CASE WHEN $6 THEN now() ELSE NULL END,'0.83.0',now())`, id, u, id[:], kind, size, drain)
		return id
	}
	draining, foreignWorker := worker(owner, "hosted", true), worker(other, "hosted", true)
	external, ready, foreignOnly := worker(owner, "external", true), worker(owner, "hosted", false), worker(owner, "hosted", true)
	seed := func(u, w uuid.UUID, status string) uuid.UUID {
		id := uuid.New()
		cliMustExec(t, pool, `INSERT INTO runs (id,user_id,worker_id,kind,status,issue_title,issue_description,claim_released_at)
			VALUES ($1,$2,$3,'chat',$4,'private title sentinel','private prompt sentinel',now())`, id, u, w, status)
		return id
	}
	run := seed(owner, draining, "awaiting_approval")
	foreign := seed(other, draining, "awaiting_input")
	foreignOnlyRun := seed(other, foreignOnly, "running")
	otherRun := seed(other, foreignWorker, "paused")
	externalRun := seed(owner, external, "running")
	readyRun := seed(owner, ready, "queued")
	seed(owner, draining, "completed")
	// Existing failure content must survive the append.
	cliMustExec(t, pool, `INSERT INTO worker_upgrade_reports (worker_id,phase,controller_reported_at,observed_at,blocking_container,blocking_reason,restart_count)
		VALUES ($1,'stuck',now(),now(),'seed-nix','ImagePullBackOff',2)`, draining)

	read := func(path, token string) map[string]apitypes.WorkerDTO {
		t.Helper()
		rec := bearerReq(router, http.MethodGet, path, token)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
		}
		var body struct {
			Workers []apitypes.WorkerDTO `json:"workers"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		out := map[string]apitypes.WorkerDTO{}
		for _, dto := range body.Workers {
			out[dto.ID] = dto
		}
		for _, hidden := range []string{foreign.String(), foreignOnlyRun.String(), "private title sentinel", "private prompt sentinel"} {
			if strings.Contains(rec.Body.String(), hidden) {
				t.Fatalf("unsafe response contains %q", hidden)
			}
		}
		return out
	}
	listed, err := h.wsvc.ListWorkers(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}
	var prior string
	for _, row := range listed {
		if row.ID == draining {
			dto := workerDTOFromRow(row, h.version, h.cfg.HostedWorkerVersion, h.clock(), h.startedAt)
			if dto.UpgradeDetail != nil {
				prior = *dto.UpgradeDetail
			}
		}
	}
	if prior == "" {
		t.Fatal("missing existing upgrade detail")
	}
	for _, tc := range []struct {
		path, token string
		admin       bool
	}{{"/api/workers", ownerToken, false}, {"/api/admin/workers", adminToken, true}} {
		capture.batches = nil
		out := read(tc.path, tc.token)
		var expectedIDs []uuid.UUID
		for _, d := range out {
			if d.Kind == "hosted" && d.DrainingSince != nil {
				expectedIDs = append(expectedIDs, uuid.MustParse(d.ID))
			}
		}
		if len(capture.batches) != 1 {
			t.Fatalf("wanted one batch, got %+v", capture.batches)
		}
		gotIDs := capture.batches[0]
		sort.Slice(gotIDs, func(i, j int) bool { return gotIDs[i].String() < gotIDs[j].String() })
		sort.Slice(expectedIDs, func(i, j int) bool { return expectedIDs[i].String() < expectedIDs[j].String() })
		if !reflect.DeepEqual(gotIDs, expectedIDs) {
			t.Fatalf("queried %+v, authorized draining hosted IDs %+v", gotIDs, expectedIDs)
		}
		dto, ok := out[draining.String()]
		if !ok || dto.UpgradeDetail == nil {
			t.Fatal("missing detail")
		}
		for _, want := range []string{run.String(), "owner approves or reviews", "unavailable"} {
			if !strings.Contains(*dto.UpgradeDetail, want) {
				t.Fatalf("%s missing %s", *dto.UpgradeDetail, want)
			}
		}
		if dto.UpgradeBlockingReason == nil || *dto.UpgradeBlockingReason != "ImagePullBackOff" {
			t.Fatalf("failure fields lost: %+v", dto)
		}
		if !strings.HasPrefix(*dto.UpgradeDetail, prior+"; ") {
			t.Fatalf("existing failure detail lost: %s", *dto.UpgradeDetail)
		}
		if d := out[foreignOnly.String()].UpgradeDetail; d == nil || !strings.Contains(*d, "unavailable") || strings.Contains(*d, "No assigned") {
			t.Fatalf("foreign-only detail: %v", d)
		}
		for id, hidden := range map[uuid.UUID]uuid.UUID{external: externalRun, ready: readyRun} {
			if d := out[id.String()].UpgradeDetail; d != nil && strings.Contains(*d, hidden.String()) {
				t.Fatalf("non-draining/hosted overlay: %s", *d)
			}
		}
		_, hasOther := out[foreignWorker.String()]
		if hasOther != tc.admin {
			t.Fatalf("owner authorization: other present=%v admin=%v", hasOther, tc.admin)
		}
		if tc.admin {
			d := out[foreignWorker.String()].UpgradeDetail
			if d == nil || !strings.Contains(*d, otherRun.String()) {
				t.Fatal("admin missing other owner's safe blocker")
			}
		}
	}
	capture.failure = errors.New("injected blocker projection failure")
	failed := read("/api/workers", ownerToken)[draining.String()]
	if failed.UpgradeDetail == nil || !strings.HasPrefix(*failed.UpgradeDetail, prior+"; ") || !strings.Contains(*failed.UpgradeDetail, "unavailable") || strings.Contains(*failed.UpgradeDetail, "No assigned") {
		t.Fatalf("projection failure: %+v", failed)
	}
	capture.failure = nil
	// The batch helper can prove it did not fetch another authorized population:
	// response IDs alone produce these entries, with no worker_active_runs fixture.
	batch := h.upgradeBlockersByWorker(context.Background(), []uuid.UUID{draining})
	if len(batch) != 1 || !strings.Contains(batch[draining].Summary(), run.String()) {
		t.Fatal(batch)
	}
}
