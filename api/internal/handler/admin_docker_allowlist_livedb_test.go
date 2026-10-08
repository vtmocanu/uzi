package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/config"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

const dockerAllowlistReposPath = "/api/admin/docker-allowlist-repos"

func TestAdminDockerAllowlistReposAuthAndSelectionLiveDB(t *testing.T) {
	ctx := context.Background()
	h, router, pool := cliLiveDB(t)
	admin := cliSeedUser(t, pool, true)
	member := cliSeedUser(t, pool, false)
	otherAdmin := cliSeedUser(t, pool, true)
	f := enableGuardFixture{h: h, pool: pool}
	connA := f.addConn(ctx, t, member, "https://docker-a.example")
	connB := f.addConn(ctx, t, member, "https://docker-b.example")
	connC := f.addConn(ctx, t, otherAdmin, "https://docker-c.example")
	connD := f.addConn(ctx, t, admin, "https://docker-d.example")
	a := f.addRepoRow(ctx, t, connA, 1, "g/same", "", uuid.Nil)
	b := f.addRepoRow(ctx, t, connB, 1, "g/same", "", uuid.Nil)
	c := f.addRepoRow(ctx, t, connC, 1, "g/disabled-trusted", "", uuid.Nil)
	d := f.addRepoRow(ctx, t, connD, 1, "g/admin", "", uuid.Nil)
	excluded := f.addRepoRow(ctx, t, connA, 2, "g/disabled-untrusted", "", uuid.Nil)
	deleted := f.addRepoRow(ctx, t, connA, 3, "g/deleted", "", uuid.Nil)
	cliMustExec(t, pool, "UPDATE repos SET enabled=false WHERE id=ANY($1::uuid[])", []uuid.UUID{c, excluded})
	cliMustExec(t, pool, "DELETE FROM repos WHERE id=$1", deleted)
	// The effective env value differs from the DB row, including a disabled repo.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // restore the instance setting after the fixture
	if _, err := tx.Exec(ctx, "INSERT INTO app_settings(key,value) VALUES($1,$2) ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value",
		settings.KeyDockerRepoAllowlist, excluded.String()); err != nil {
		t.Fatal(err)
	}
	h.settings = settings.New(h.q.WithTx(tx), time.Minute)
	h.settings.ConfigureSecrets(h.box, map[string]string{settings.KeyDockerRepoAllowlist: c.String() + "," + deleted.String()})
	adminJWT := cliMintJWT(t, pool, admin)
	rec := cookieReq(t, router, http.MethodGet, dockerAllowlistReposPath, adminJWT, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("admin GET = %d: %s", rec.Code, rec.Body.String())
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope) != 1 || envelope["repos"] == nil {
		t.Fatalf("envelope keys: %s", rec.Body.String())
	}
	var rawRows []map[string]json.RawMessage
	if err := json.Unmarshal(envelope["repos"], &rawRows); err != nil {
		t.Fatal(err)
	}
	wantKeys := []string{"base_url", "connection_id", "enabled", "forge_type", "id", "owner_email", "path_with_namespace"}
	for _, row := range rawRows {
		keys := make([]string, 0, len(row))
		for key := range row {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		if !reflect.DeepEqual(keys, wantKeys) {
			t.Fatalf("repo keys = %v", keys)
		}
	}
	var body apitypes.AdminDockerAllowlistReposDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	byID := map[string]apitypes.AdminDockerAllowlistRepoDTO{}
	// Other packages leave repositories in this shared database. Restrict the
	// bytewise order assertion to our fixed-format fixture values; unrelated text
	// can have a different order under the database collation.
	fixtureIDs := map[string]bool{a.String(): true, b.String(): true, c.String(): true, d.String(): true}
	order := make([]string, 0, len(fixtureIDs))
	for _, row := range body.Repos {
		byID[row.ID] = row
		if fixtureIDs[row.ID] {
			order = append(order, strings.Join([]string{row.OwnerEmail, row.PathWithNamespace, row.ConnectionID, row.ID}, "\n"))
		}
	}
	if !sort.StringsAreSorted(order) {
		t.Fatalf("nondeterministic order: %v", order)
	}
	for _, tc := range []struct {
		id, conn, owner uuid.UUID
		path, base      string
		enabled         bool
	}{
		{a, connA, member, "g/same", "https://docker-a.example", true},
		{b, connB, member, "g/same", "https://docker-b.example", true},
		{c, connC, otherAdmin, "g/disabled-trusted", "https://docker-c.example", false},
		{d, connD, admin, "g/admin", "https://docker-d.example", true},
	} {
		row, ok := byID[tc.id.String()]
		if !ok || row.ConnectionID != tc.conn.String() || row.OwnerEmail != fmt.Sprintf("cli-%s@e2e", tc.owner) ||
			row.PathWithNamespace != tc.path || row.Enabled != tc.enabled || row.BaseURL != tc.base || row.ForgeType != "gitlab" {
			t.Fatalf("repo %s = %+v (present %v), want %+v", tc.id, row, ok, tc)
		}
	}
	for _, id := range []uuid.UUID{excluded, deleted} {
		if _, ok := byID[id.String()]; ok {
			t.Fatalf("excluded repo %s returned", id)
		}
	}
	for _, tc := range []struct {
		name, token string
		status      int
	}{
		{"unauthenticated", "", http.StatusUnauthorized},
		{"member CLI", cliMintToken(t, pool, member, clitoken.ScopeUser), http.StatusForbidden},
		{"admin read CLI", cliMintToken(t, pool, admin, clitoken.ScopeAdminRO), http.StatusOK},
		{"admin user CLI", cliMintToken(t, pool, admin, clitoken.ScopeUser), http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := bearerReq(router, http.MethodGet, dockerAllowlistReposPath, tc.token)
			if got.Code != tc.status {
				t.Fatalf("status=%d want=%d body=%s", got.Code, tc.status, got.Body.String())
			}
		})
	}
	if got := cookieReq(t, router, http.MethodGet, dockerAllowlistReposPath, cliMintJWT(t, pool, member), ""); got.Code != http.StatusForbidden {
		t.Fatalf("member session = %d: %s", got.Code, got.Body.String())
	}
}

// Query failure is injected only into Query; authentication's QueryRow remains live.
type dockerAllowlistQueryFailure struct{ store.DBTX }

func (dockerAllowlistQueryFailure) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("isolated repository query failure")
}

func TestAdminDockerAllowlistReposEmptyAndErrorsLiveDB(t *testing.T) {
	ctx := context.Background()
	h, _, pool := cliLiveDB(t)
	admin := cliSeedUser(t, pool, true)
	jwt := cliMintJWT(t, pool, admin)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // fixture transaction is always rolled back
	if _, err := tx.Exec(ctx, "UPDATE repos SET enabled=false"); err != nil {
		t.Fatal(err)
	}
	h.q = store.New(tx)
	h.settings = settings.New(&settingsStore{}, time.Minute)
	lim := mw.NewLimiter(100000, time.Minute, nil)
	router := h.Routes(lim, lim, lim, lim, lim, lim, lim, lim, lim, lim, lim)
	got := cookieReq(t, router, http.MethodGet, dockerAllowlistReposPath, jwt, "")
	if got.Code != http.StatusOK || strings.TrimSpace(got.Body.String()) != `{"repos":[]}` {
		t.Fatalf("empty = %d %s", got.Code, got.Body.String())
	}
	// A cold settings failure with a healthy repository query.
	h.settings = settings.New(&settingsStore{err: errors.New("isolated settings failure")}, time.Minute)
	got = cookieReq(t, router, http.MethodGet, dockerAllowlistReposPath, jwt, "")
	if got.Code != http.StatusInternalServerError || strings.Contains(got.Body.String(), "repos") {
		t.Fatalf("settings failure = %d %s", got.Code, got.Body.String())
	}
	h.settings = settings.New(&settingsStore{}, time.Minute)
	h.q = store.New(dockerAllowlistQueryFailure{DBTX: tx})
	router = h.Routes(lim, lim, lim, lim, lim, lim, lim, lim, lim, lim, lim)
	got = cookieReq(t, router, http.MethodGet, dockerAllowlistReposPath, jwt, "")
	if got.Code != http.StatusInternalServerError {
		t.Fatalf("query failure = %d %s", got.Code, got.Body.String())
	}
	if strings.Contains(got.Body.String(), "repos") {
		t.Fatalf("failure returned partial repos: %s", got.Body.String())
	}
}

func TestAdminDockerAllowlistTrustUnlocksMemberClaimLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newSeededPlanFixture(ctx, t)
	q := store.New(f.pool)
	// Preserve the instance policy after this test; this suite shares a database.
	rows, err := q.ListAppSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var prior *store.AppSetting
	for i := range rows {
		if rows[i].Key == settings.KeyDockerRepoAllowlist {
			prior = &rows[i]
		}
	}
	t.Cleanup(func() {
		if prior == nil {
			cliMustExec(t, f.pool, "DELETE FROM app_settings WHERE key=$1", settings.KeyDockerRepoAllowlist)
		} else {
			cliMustExec(t, f.pool, `INSERT INTO app_settings(key,value,updated_at,updated_by) VALUES($1,$2,$3,$4)
				ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value, updated_at=EXCLUDED.updated_at, updated_by=EXCLUDED.updated_by`,
				prior.Key, prior.Value, prior.UpdatedAt, prior.UpdatedBy)
		}
	})
	cliMustExec(t, f.pool, "DELETE FROM app_settings WHERE key=$1", settings.KeyDockerRepoAllowlist)
	cliMustExec(t, f.pool, "UPDATE repos SET required_capabilities=ARRAY['docker']::text[] WHERE id=$1", f.repoID)
	cliMustExec(t, f.pool, "UPDATE workers SET docker_enabled=true, capabilities=ARRAY['docker']::text[], max_concurrent_runs=1, last_heartbeat_at=now() WHERE id=$1", f.wkr.ID)
	worker, err := q.GetWorkerByID(ctx, f.wkr.ID)
	if err != nil {
		t.Fatal(err)
	}
	run, err := q.CreateRun(ctx, store.CreateRunParams{
		Harness: "claude", UserID: f.owner, RepoID: f.repoID,
		IssueIid: pgconv.Int8Ptr(&f.iid), IssueTitle: "Docker trust", IssueDescription: "requires Docker",
		PlanSource: "agent", TriggerSource: "manual",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(run.RequiredCapabilities, []string{"docker"}) {
		t.Fatalf("run requirements = %v", run.RequiredCapabilities)
	}
	cache := settings.New(q, time.Minute)
	svc := workersvc.New(q, f.box, workersvc.Params{WorkerHeartbeatStale: time.Minute})
	svc.SetTxBeginner(f.pool)
	svc.SetDockerAllowlist(cache)
	svc.SetCapabilitySettings(cache)
	svc.SetEffectiveDockerTier(true)
	svc.SetBackground(func(func()) {})
	payload, err := svc.Claim(ctx, worker, nil)
	if err != nil || payload != nil {
		t.Fatalf("before trust: payload=%+v err=%v", payload, err)
	}
	stored, err := q.GetRunByID(ctx, run.ID)
	if err != nil || stored.Status != "queued" || stored.WorkerID.Valid {
		t.Fatalf("before trust run=%+v err=%v", stored, err)
	}
	admin := cliSeedUser(t, f.pool, true)
	h := &Handler{pool: f.pool, q: q, box: f.box, cfg: config.Config{JWTSecret: cliTestSecret, AuthTokenTTL: time.Hour}, settings: cache, wsvc: svc}
	lim := mw.NewLimiter(100000, time.Minute, nil)
	router := h.Routes(lim, lim, lim, lim, lim, lim, lim, lim, lim, lim, lim)
	body, err := json.Marshal(map[string]any{"settings": map[string]string{settings.KeyDockerRepoAllowlist: f.repoID.String()}})
	if err != nil {
		t.Fatal(err)
	}
	got := cookieReq(t, router, http.MethodPut, "/api/admin/settings", cliMintJWT(t, f.pool, admin), string(body))
	if got.Code != http.StatusOK {
		t.Fatalf("save trust=%d %s", got.Code, got.Body.String())
	}
	var value string
	if err := f.pool.QueryRow(ctx, "SELECT value FROM app_settings WHERE key=$1", settings.KeyDockerRepoAllowlist).Scan(&value); err != nil || value != f.repoID.String() {
		t.Fatalf("stored allowlist=%q err=%v", value, err)
	}
	got = cookieReq(t, router, http.MethodGet, "/api/repos", cliMintJWT(t, f.pool, f.owner), "")
	if got.Code != http.StatusOK {
		t.Fatalf("member repos=%d %s", got.Code, got.Body.String())
	}
	var repos struct {
		Repos []apitypes.RepoDTO `json:"repos"`
	}
	if err := json.Unmarshal(got.Body.Bytes(), &repos); err != nil {
		t.Fatal(err)
	}
	if len(repos.Repos) != 1 || repos.Repos[0].ID != f.repoID.String() || !repos.Repos[0].DockerAllowlisted {
		t.Fatalf("member repos=%+v", repos.Repos)
	}
	payload, err = svc.Claim(ctx, worker, nil)
	if err != nil || payload == nil || payload.RunID != run.ID.String() {
		t.Fatalf("after trust: payload=%+v err=%v", payload, err)
	}
	stored, err = q.GetRunByID(ctx, run.ID)
	if err != nil || stored.Status != "claimed" || !stored.WorkerID.Valid || uuid.UUID(stored.WorkerID.Bytes) != worker.ID {
		t.Fatalf("after trust run=%+v err=%v", stored, err)
	}
}
