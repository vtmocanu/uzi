package handler

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/hostedsvc"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// dockerAllowlistProbe counts pass reads and can return values alongside an error.
type dockerAllowlistProbe struct {
	*settings.Cache
	values []uuid.UUID
	reads  int
	fail   bool
}

func (s *dockerAllowlistProbe) DockerRepoAllowlist(ctx context.Context) ([]uuid.UUID, error) {
	s.reads++
	if s.fail {
		return s.values, errors.New("allowlist unavailable")
	}
	return s.Cache.DockerRepoAllowlist(ctx)
}

func (fx *ephemeralFixture) dockerProvisioner(preference, tier, member bool, max int, fail bool) (*hostedsvc.EphemeralProvisioner, *dockerAllowlistProbe) {
	fx.t.Helper()
	cliMustExec(fx.t, fx.pool, "UPDATE users SET ephemeral_docker_enabled=$2 WHERE id=$1", fx.userID, preference)
	list := ""
	if member {
		list = fx.repoID.String()
	}
	sc := settings.New(&settingsStore{rows: []store.AppSetting{
		{Key: settings.KeyEphemeralWorkersEnabled, Value: "true"},
		{Key: settings.KeyDockerRepoAllowlist, Value: list},
	}}, time.Minute)
	values := []uuid.UUID{}
	if member {
		values = append(values, fx.repoID)
	}
	probe := &dockerAllowlistProbe{Cache: sc, values: values, fail: fail}
	return hostedsvc.NewEphemeralProvisioner(fx.pool, fx.q, fx.box, probe, hostedsvc.EphemeralConfig{
		DockerEnabled: tier, MaxPerUser: max, DefaultSize: "m",
	}), probe
}

func TestEphemeralDockerProvisionPassLiveDB(t *testing.T) {
	for _, trigger := range []string{"gap", "saturation"} {
		for _, tc := range []struct {
			name                           string
			preference, tier, member, fail bool
			caps                           []string
			want                           bool
		}{
			{"preference", true, true, true, false, []string{"jvm"}, true},
			{"preference off", false, true, true, false, []string{"jvm"}, false},
			{"tier off", true, false, true, false, []string{"jvm"}, false},
			{"unallowlisted", true, true, false, false, []string{"jvm"}, false},
			{"empty error", true, true, false, true, []string{"jvm"}, false},
			{"values with error", true, true, true, true, []string{"jvm"}, false},
			{"capability Docker", false, false, false, false, []string{"docker"}, true},
			{"capability Docker read error", true, false, true, true, []string{"docker"}, true},
		} {
			t.Run(trigger+"/"+tc.name, func(t *testing.T) {
				fx := newEphemeralFixture(t, true)
				// JVM is a provisionable capability gap independent of the Docker preference.
				// Saturation uses a capability-free run on a full plain worker, unless Docker is required.
				caps := tc.caps
				if trigger == "saturation" {
					docker := slices.Contains(caps, "docker")
					w := fx.onlineWorkerWithCap("busy", docker, 1)
					fx.activeRunOn(w)
					if !docker {
						caps = []string{}
					}
				}
				run := fx.queuedRun(caps)
				p, failure := fx.dockerProvisioner(tc.preference, tc.tier, tc.member, 2, tc.fail)
				if _, err := p.ProvisionPass(fx.ctx); err != nil {
					t.Fatal(err)
				}
				rows := fx.ephemeralRows()
				if len(rows) != 1 || rows[0].runID != run || rows[0].docker != tc.want || rows[0].size != "m" {
					t.Fatalf("workers=%+v, want one bound worker docker=%v size=m", rows, tc.want)
				}
				if failure != nil && failure.reads != 1 {
					t.Fatalf("allowlist reads=%d, want 1", failure.reads)
				}
			})
		}
	}
}

func TestEphemeralDockerProvisionCapLiveDB(t *testing.T) {
	fx := newEphemeralFixture(t, true)
	// A batch with more candidates than the cap exercises provisionOne's locked count.
	for i := 0; i < 3; i++ {
		fx.queuedRun([]string{"docker"})
	}
	p, _ := fx.dockerProvisioner(true, true, true, 1, false)
	if _, err := p.ProvisionPass(fx.ctx); err != nil {
		t.Fatal(err)
	}
	rows := fx.ephemeralRows()
	if len(rows) != 1 || !rows[0].docker {
		t.Fatalf("workers=%+v, want one Docker worker at cap", rows)
	}
}

func TestEphemeralDockerLatePlanApprovalLiveDB(t *testing.T) {
	for _, preference := range []bool{false, true} {
		t.Run(fmt.Sprintf("preference=%t", preference), func(t *testing.T) {
			fx := newEphemeralFixture(t, true)
			busy := fx.onlineWorkerWithCap("busy", false, 1)
			fx.activeRunOn(busy)
			run := fx.queuedRun([]string{})
			p, _ := fx.dockerProvisioner(preference, true, true, 2, false)
			if _, err := p.ProvisionPass(fx.ctx); err != nil {
				t.Fatal(err)
			}
			rows := fx.ephemeralRows()
			if len(rows) != 1 || rows[0].runID != run {
				t.Fatalf("workers=%+v", rows)
			}
			worker, err := fx.q.GetWorkerByID(fx.ctx, rows[0].id)
			if err != nil {
				t.Fatal(err)
			}
			effective := capability.EffectiveWorkerCaps(worker.Capabilities, worker.DockerEnabled.Valid && worker.DockerEnabled.Bool)
			if slices.Contains(effective, "docker") != preference {
				t.Fatalf("effective caps=%v preference=%v", effective, preference)
			}
			// The actual provisioned worker owns the gate; Docker was inferred only after provisioning.
			cliMustExec(t, fx.pool, `UPDATE runs SET status='awaiting_approval',worker_id=$2,plan_md='Build and smoke-test the container',required_capabilities=ARRAY['docker'],gate_revision=1 WHERE id=$1`, run, worker.ID)
			svc := workersvc.New(fx.q, fx.box, workersvc.Params{})
			result, err := svc.SubmitInput(fx.ctx, fx.userID, run, "approve_plan", "", nil)
			if preference {
				if err != nil {
					t.Fatalf("Docker plan approval: %v", err)
				}
				var kind string
				if err := fx.pool.QueryRow(fx.ctx, "SELECT kind FROM run_user_inputs WHERE id=$1 AND run_id=$2", result.ID, run).Scan(&kind); err != nil || kind != "approve_plan" {
					t.Fatalf("approval kind=%q err=%v", kind, err)
				}
			} else {
				if !errors.Is(err, workersvc.ErrCapabilityUnmet) {
					t.Fatalf("plain worker approval err=%v", err)
				}
				var n int
				if err := fx.pool.QueryRow(fx.ctx, "SELECT count(*) FROM run_user_inputs WHERE run_id=$1", run).Scan(&n); err != nil || n != 0 {
					t.Fatalf("blocked input count=%d err=%v", n, err)
				}
			}
		})
	}
}

func TestEphemeralDockerPreferenceGoSQLParityLiveDB(t *testing.T) {
	fx := newEphemeralFixture(t, true)
	repo := pgtype.UUID{Bytes: fx.repoID, Valid: true}
	kind := pgtype.Text{String: "issue", Valid: true}
	yes := pgtype.Bool{Bool: true, Valid: true}
	for _, tc := range []struct {
		name       string
		pref, tier pgtype.Bool
		repo       pgtype.UUID
		kind       pgtype.Text
		profile    pgtype.UUID
		list       []pgtype.UUID
		want       bool
	}{
		{"ordinary", yes, yes, repo, kind, pgtype.UUID{}, []pgtype.UUID{repo}, true},
		{"null preference", pgtype.Bool{}, yes, repo, kind, pgtype.UUID{}, []pgtype.UUID{repo}, false},
		{"null tier", yes, pgtype.Bool{}, repo, kind, pgtype.UUID{}, []pgtype.UUID{repo}, false},
		{"preference off", pgtype.Bool{Valid: true}, yes, repo, kind, pgtype.UUID{}, []pgtype.UUID{repo}, false},
		{"tier off", yes, pgtype.Bool{Valid: true}, repo, kind, pgtype.UUID{}, []pgtype.UUID{repo}, false},
		{"repo-less judge", yes, yes, pgtype.UUID{}, pgtype.Text{String: "judge", Valid: true}, pgtype.UUID{}, []pgtype.UUID{repo}, false},
		{"null kind", yes, yes, repo, pgtype.Text{}, pgtype.UUID{}, []pgtype.UUID{repo}, false},
		{"empty kind", yes, yes, repo, pgtype.Text{Valid: true}, pgtype.UUID{}, []pgtype.UUID{repo}, true},
		{"job", yes, yes, repo, pgtype.Text{String: "job", Valid: true}, pgtype.UUID{}, []pgtype.UUID{repo}, false},
		{"lane", yes, yes, repo, kind, repo, []pgtype.UUID{repo}, false},
		{"null list", yes, yes, repo, kind, pgtype.UUID{}, nil, false},
		{"empty list", yes, yes, repo, kind, pgtype.UUID{}, []pgtype.UUID{}, false},
		{"null element", yes, yes, repo, kind, pgtype.UUID{}, []pgtype.UUID{{}}, false},
		{"null plus match", yes, yes, repo, kind, pgtype.UUID{}, []pgtype.UUID{{}, repo}, true},
		{"unallowlisted", yes, yes, repo, kind, pgtype.UUID{}, []pgtype.UUID{{Bytes: uuid.New(), Valid: true}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sql bool
			if err := fx.pool.QueryRow(fx.ctx, `SELECT fn_ephemeral_docker_preference_applies($1::boolean,$2::boolean,$3::uuid,$4::text,$5::uuid,$6::uuid[])`, tc.pref, tc.tier, tc.repo, tc.kind, tc.profile, tc.list).Scan(&sql); err != nil {
				t.Fatal(err)
			}
			// NULL array members carry no membership; retain the valid members for the Go mirror.
			var list []uuid.UUID
			for _, id := range tc.list {
				if id.Valid {
					list = append(list, uuid.UUID(id.Bytes))
				}
			}
			goResult := hostedsvc.EphemeralDockerPreferenceApplies(tc.pref.Valid && tc.pref.Bool, tc.tier.Valid && tc.tier.Bool, tc.repo, tc.kind, tc.profile, list)
			if sql != tc.want || goResult != tc.want {
				t.Fatalf("SQL=%v Go=%v want=%v", sql, goResult, tc.want)
			}
		})
	}
}

func TestEphemeralDockerExcludedCandidatesLiveDB(t *testing.T) {
	for _, trigger := range []string{"gap", "saturation"} {
		for _, kind := range []string{"job", "judge"} {
			t.Run(trigger+"/"+kind, func(t *testing.T) {
				fx := newEphemeralFixture(t, true)
				if trigger == "saturation" {
					w := fx.onlineWorkerWithCap("busy", false, 1)
					cliMustExec(t, fx.pool, "UPDATE workers SET protocol_capabilities=ARRAY['job_runner_v1'] WHERE id=$1", w)
					fx.activeRunOn(w)
				}
				run := uuid.New()
				if kind == "job" {
					cliMustExec(t, fx.pool, `INSERT INTO runs(id,user_id,kind,job_type,issue_title,issue_description,status,required_capabilities) VALUES($1,$2,'job','research','job','prompt','queued','{}')`, run, fx.userID)
				} else {
					target := fx.queuedRun([]string{})
					cliMustExec(t, fx.pool, "UPDATE runs SET status='completed' WHERE id=$1", target)
					cliMustExec(t, fx.pool, `INSERT INTO runs(id,user_id,kind,target_run_id,issue_title,issue_description,status,required_capabilities) VALUES($1,$2,'judge',$4,'judge','prompt','queued',$3)`, run, fx.userID, []string{"jvm"}, target)
					if trigger == "saturation" {
						cliMustExec(t, fx.pool, "UPDATE runs SET required_capabilities='{}' WHERE id=$1", run)
					}
				}
				p, _ := fx.dockerProvisioner(true, true, true, 2, false)
				if _, err := p.ProvisionPass(fx.ctx); err != nil {
					t.Fatal(err)
				}
				rows := fx.ephemeralRows()
				if len(rows) != 1 || rows[0].runID != run || rows[0].docker {
					t.Fatalf("excluded candidate workers=%+v", rows)
				}
			})
		}
	}
	t.Run("isolated lane", func(t *testing.T) {
		fx := newEphemeralFixture(t, true)
		profile := fx.boundProfile()
		run := fx.boundQueuedRun(profile, nil)
		// Use the real settings cache, with both the preference and admin scope on.
		cliMustExec(t, fx.pool, "UPDATE users SET ephemeral_docker_enabled=true WHERE id=$1", fx.userID)
		sc := settings.New(&settingsStore{rows: []store.AppSetting{
			{Key: settings.KeyEphemeralWorkersEnabled, Value: "true"},
			{Key: settings.KeyDockerRepoAllowlist, Value: fx.repoID.String()},
		}}, time.Minute)
		p := hostedsvc.NewEphemeralProvisioner(fx.pool, fx.q, fx.box, sc, hostedsvc.EphemeralConfig{
			DockerEnabled: true, IsolatedLaneEnabled: true, MaxPerUser: 2, DefaultSize: "m",
		})
		if _, err := p.ProvisionPass(fx.ctx); err != nil {
			t.Fatal(err)
		}
		isolated, docker, found := fx.laneOf(run)
		if !found || !isolated || docker {
			t.Fatalf("lane found=%v isolated=%v docker=%v", found, isolated, docker)
		}
	})
}

func TestEphemeralDockerFinalPlacementLiveDB(t *testing.T) {
	t.Run("preference-only gap", func(t *testing.T) {
		fx := newEphemeralFixture(t, true)
		run := fx.queuedRun([]string{})
		p, _ := fx.dockerProvisioner(true, true, true, 2, false)
		if _, err := p.ProvisionPass(fx.ctx); err != nil {
			t.Fatal(err)
		}
		rows := fx.ephemeralRows()
		if len(rows) != 1 || rows[0].runID != run || !rows[0].docker {
			t.Fatalf("preference gap workers=%+v", rows)
		}
	})
	for _, tc := range []struct {
		name                     string
		pref, tier, member, fail bool
	}{
		{"effective", true, true, true, false},
		{"preference off", false, true, true, false},
		{"tier off", true, false, true, false},
		{"empty list", true, true, false, false},
		{"values with error", true, true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newEphemeralFixture(t, true)
			warm := fx.leasedWorker(time.Minute)
			cliMustExec(t, fx.pool, "UPDATE workers SET docker_enabled=false WHERE id=$1", warm.id)
			run := fx.followUp(warm)
			_, probe := fx.dockerProvisioner(tc.pref, tc.tier, tc.member, 2, tc.fail)
			// Use the same settings snapshot with a live lease.
			p := hostedsvc.NewEphemeralProvisioner(fx.pool, fx.q, fx.box, probe, hostedsvc.EphemeralConfig{DockerEnabled: tc.tier, MaxPerUser: 2, DefaultSize: "m", Lease: 2 * time.Hour})
			if _, err := p.ProvisionPass(fx.ctx); err != nil {
				t.Fatal(err)
			}
			rows := fx.ephemeralRows()
			if tc.name == "effective" {
				if len(rows) != 2 {
					t.Fatalf("warm step-aside workers=%+v", rows)
				}
				found := false
				for _, row := range rows {
					if row.runID == run && row.docker {
						found = true
					}
				}
				if !found {
					t.Fatalf("missing Docker follow-up worker: %+v", rows)
				}
			} else if len(rows) != 1 || rows[0].id != warm.id || rows[0].docker {
				t.Fatalf("ineffective preference changed reuse: %+v", rows)
			}
			if probe.reads != 1 {
				t.Fatalf("allowlist reads=%d", probe.reads)
			}
		})
	}
}

// This seam uses real claim transactions, provisioning and registration on the same run.
func TestEphemeralDockerWarmStepAsideLiveDB(t *testing.T) {
	fx := newDockerPlacementFixture(t)
	warm := fx.leasedWorker(time.Minute)
	cliMustExec(t, fx.pool, "UPDATE workers SET docker_enabled=false WHERE id=$1", warm.id)
	run := fx.followUp(warm)
	_, probe := fx.dockerProvisioner(true, true, true, 2, false)
	p := hostedsvc.NewEphemeralProvisioner(fx.pool, fx.q, fx.box, probe, hostedsvc.EphemeralConfig{DockerEnabled: true, MaxPerUser: 2, DefaultSize: "m", Lease: 2 * time.Hour})
	svc := workersvc.New(fx.q, fx.box, workersvc.Params{WorkerHeartbeatStale: time.Minute, WorkerAffinityCeiling: time.Minute})
	svc.SetTxBeginner(fx.pool)
	svc.SetEphemeralLease(2 * time.Hour)
	svc.SetDockerAllowlist(probe)
	svc.SetEffectiveDockerTier(true)
	svc.SetBackground(func(func()) {})
	fx.sealDockerClaimCredentials()
	cliMustExec(t, fx.pool, "UPDATE users SET ephemeral_docker_enabled=false WHERE id=$1", fx.userID)
	plain, err := fx.q.GetWorkerByID(fx.ctx, warm.id)
	if err != nil {
		t.Fatal(err)
	}
	// The owner preference changes after the worker was loaded. ClaimRun must read it fresh.
	cliMustExec(t, fx.pool, "UPDATE users SET ephemeral_docker_enabled=true WHERE id=$1", fx.userID)
	payload, err := svc.Claim(fx.ctx, plain, nil)
	if err != nil {
		t.Fatal(err)
	}
	if payload != nil {
		t.Fatal("plain warm worker claims when it should refuse")
	}
	gaps, err := fx.q.ListUnplaceableQueuedRunsForEphemeral(fx.ctx, store.ListUnplaceableQueuedRunsForEphemeralParams{MaxRows: 10, MaxPerUser: 2, EphemeralLease: workersvc.LeaseInterval(2 * time.Hour), WorkerDockerEnabled: true, DockerRepoAllowlist: []uuid.UUID{fx.repoID}, CodexCuratedModels: workersvc.CodexCuratedModels()})
	if err != nil || len(gaps) != 1 || gaps[0].ID != run {
		t.Fatalf("gap=%+v err=%v, want same follow-up", gaps, err)
	}
	created, err := p.ProvisionPass(fx.ctx)
	if err != nil || created != 1 {
		t.Fatalf("ProvisionPass=(%d,%v)", created, err)
	}
	var dockerID uuid.UUID
	for _, row := range fx.ephemeralRows() {
		if row.runID == run && row.docker {
			dockerID = row.id
		}
	}
	if dockerID == uuid.Nil {
		t.Fatal("missing newly created Docker worker")
	}
	docker, err := fx.q.GetWorkerByID(fx.ctx, dockerID)
	if err != nil {
		t.Fatal(err)
	}
	max := 1
	docker, _, err = svc.Register(fx.ctx, docker, "test", "base", &max, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if docker.Status != "online" {
		t.Fatalf("registered status=%q, want online", docker.Status)
	}
	if !slices.Contains(capability.EffectiveWorkerCaps(docker.Capabilities, docker.DockerEnabled.Valid && docker.DockerEnabled.Bool), "docker") {
		t.Fatal("registered worker lacks effective Docker capability")
	}
	payload, err = svc.Claim(fx.ctx, docker, nil)
	if err != nil || payload == nil {
		t.Fatalf("Docker claim payload present=%v err=%v", payload != nil, err)
	}
	if payload.RunID != run.String() {
		t.Fatalf("claimed=%s want=%s", payload.RunID, run)
	}
	var owner uuid.UUID
	err = fx.pool.QueryRow(fx.ctx, "SELECT worker_id FROM runs WHERE id=$1", run).Scan(&owner)
	if err != nil || owner != dockerID {
		t.Fatalf("created worker does not own same run: %s err=%v", owner, err)
	}
}
