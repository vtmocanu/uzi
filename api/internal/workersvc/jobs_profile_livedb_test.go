package workersvc

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// jobs_profile_livedb_test.go covers PRD #1976 M1 at the service seams against a live Postgres:
// job create naming a site list (the allowance decision), the isolated_job_v1 rollout gate on the
// claim and sweeper paths, worker registration of that capability, and the claim a profile-bound
// job receives. Skipped unless UZI_TEST_DATABASE_URL points at a throwaway database; run via
// ./e2e/run-store-it.sh.
//
// CALIBRATION (create): delete the ProductEgressProfileAllowed call in CreateJobRun and the
// "not allowed" subtests of TestCreateJobRunSiteListLiveDB go red.

// seedProfile inserts a site list and removes it (and every run bound to it, the FK is RESTRICT)
// when the test ends.
func (e jobEnv) seedProfile(t *testing.T, hosts ...string) (id uuid.UUID, name string) {
	t.Helper()
	id = uuid.New()
	name = "sl-" + strings.ReplaceAll(id.String(), "-", "")[:20]
	e.exec(`INSERT INTO egress_profiles (id, name, hosts) VALUES ($1, $2, $3)`, id, name, hosts)
	t.Cleanup(func() {
		e.exec(`DELETE FROM runs WHERE egress_profile_id = $1`, id)
		e.exec(`DELETE FROM egress_profiles WHERE id = $1`, id)
	})
	return id, name
}

func (e jobEnv) allow(t *testing.T, product, profile uuid.UUID) {
	t.Helper()
	e.exec(`INSERT INTO product_egress_profiles (product_id, egress_profile_id) VALUES ($1, $2)`, product, profile)
}

func profileReq(c JobCaller, profile string) CreateJobParams {
	p := jobReq(c)
	p.EgressProfile = profile
	return p
}

func TestCreateJobRunSiteListLiveDB(t *testing.T) {
	e := setupJobLiveDB(t, 0)
	u := e.seedJobUser(t)
	e.ownJobsCleanup(t, u)
	allowedID, allowedName := e.seedProfile(t, "docs.example.com")
	_, otherName := e.seedProfile(t, "other.example.com")
	prod, tok := e.seedProduct(t, u, []string{"research"})
	e.allow(t, prod, allowedID)

	t.Run("an allowed list: the job is created bound to it and stamped for the file protocol", func(t *testing.T) {
		v, err := e.svc.CreateJobRun(e.ctx, profileReq(productCaller(u, prod, tok), allowedName))
		if err != nil {
			t.Fatalf("CreateJobRun: %v", err)
		}
		run := mustRun(t, e.codexTestEnv, v.ID)
		if !run.EgressProfileID.Valid || uuid.UUID(run.EgressProfileID.Bytes) != allowedID {
			t.Fatalf("egress_profile_id = %+v, want %s", run.EgressProfileID, allowedID)
		}
		if !run.JobProtocol.Valid || run.Kind != "job" || run.Status != "queued" {
			t.Fatalf("run = kind %s status %s job_protocol %+v", run.Kind, run.Status, run.JobProtocol)
		}
	})

	t.Run("a list the product is not allowed: refused and no run row", func(t *testing.T) {
		before := e.jobCount(t, u)
		_, err := e.svc.CreateJobRun(e.ctx, profileReq(productCaller(u, prod, tok), otherName))
		if !errors.Is(err, ErrJobProfileNotAllowed) {
			t.Fatalf("err = %v, want ErrJobProfileNotAllowed", err)
		}
		if n := e.jobCount(t, u); n != before {
			t.Fatalf("a refused create left %d job rows", n-before)
		}
	})

	t.Run("a user token may name any existing list", func(t *testing.T) {
		v, err := e.svc.CreateJobRun(e.ctx, profileReq(cliCaller(u), otherName))
		if err != nil {
			t.Fatalf("CreateJobRun: %v", err)
		}
		if run := mustRun(t, e.codexTestEnv, v.ID); !run.EgressProfileID.Valid {
			t.Fatal("a user job naming a list must be bound to it")
		}
	})

	t.Run("an unknown list is refused for both caller kinds, and creates nothing", func(t *testing.T) {
		before := e.jobCount(t, u)
		for _, c := range []JobCaller{cliCaller(u), productCaller(u, prod, tok)} {
			if _, err := e.svc.CreateJobRun(e.ctx, profileReq(c, "no-such-list")); !errors.Is(err, ErrJobProfileNotFound) {
				t.Errorf("caller product=%v: err = %v, want ErrJobProfileNotFound", c.ProductID != nil, err)
			}
		}
		if n := e.jobCount(t, u); n != before {
			t.Fatalf("a refused create left %d job rows", n-before)
		}
	})

	t.Run("a product with no allowance rows is refused every list (fail closed)", func(t *testing.T) {
		bare, bareTok := e.seedProduct(t, u, []string{"research"})
		before := e.jobCount(t, u)
		if _, err := e.svc.CreateJobRun(e.ctx, profileReq(productCaller(u, bare, bareTok), allowedName)); !errors.Is(err, ErrJobProfileNotAllowed) {
			t.Fatalf("err = %v, want ErrJobProfileNotAllowed", err)
		}
		if n := e.jobCount(t, u); n != before {
			t.Fatalf("a refused create left %d job rows", n-before)
		}
	})

	t.Run("a disabled product is refused at the job-type policy, before the list is read", func(t *testing.T) {
		off, offTok := e.seedProduct(t, u, []string{"research"})
		e.allow(t, off, allowedID)
		e.exec(`UPDATE products SET enabled = false WHERE id = $1`, off)
		if _, err := e.svc.CreateJobRun(e.ctx, profileReq(productCaller(u, off, offTok), allowedName)); !errors.Is(err, ErrJobTypeNotAllowed) {
			t.Fatalf("err = %v, want ErrJobTypeNotAllowed (GetProductJobPolicy refuses a disabled product first)", err)
		}
		// The allowance query is itself fail closed for a disabled or deleted product.
		for _, set := range []string{`enabled = false`, `enabled = false, deleted_at = now()`} {
			e.exec(`UPDATE products SET `+set+` WHERE id = $1`, off)
			ok, err := e.q.ProductEgressProfileAllowed(e.ctx, store.ProductEgressProfileAllowedParams{ProductID: off, EgressProfileID: allowedID})
			if err != nil || ok {
				t.Fatalf("ProductEgressProfileAllowed with %s = %v, %v; want false", set, ok, err)
			}
		}
	})

	t.Run("removing the allowance refuses the next create and leaves the existing run bound", func(t *testing.T) {
		p2, t2 := e.seedProduct(t, u, []string{"research"})
		e.allow(t, p2, allowedID)
		v, err := e.svc.CreateJobRun(e.ctx, profileReq(productCaller(u, p2, t2), allowedName))
		if err != nil {
			t.Fatalf("first create: %v", err)
		}
		e.exec(`DELETE FROM product_egress_profiles WHERE product_id = $1`, p2)
		if _, err := e.svc.CreateJobRun(e.ctx, profileReq(productCaller(u, p2, t2), allowedName)); !errors.Is(err, ErrJobProfileNotAllowed) {
			t.Fatalf("create after removal: err = %v, want ErrJobProfileNotAllowed", err)
		}
		run := mustRun(t, e.codexTestEnv, v.ID)
		if !run.EgressProfileID.Valid || uuid.UUID(run.EgressProfileID.Bytes) != allowedID || run.Status != "queued" {
			t.Fatalf("the earlier job changed: status %s egress_profile_id %+v", run.Status, run.EgressProfileID)
		}
	})

	t.Run("an unbound job is unchanged: no profile, still stamped", func(t *testing.T) {
		v, err := e.svc.CreateJobRun(e.ctx, jobReq(productCaller(u, prod, tok)))
		if err != nil {
			t.Fatalf("CreateJobRun: %v", err)
		}
		run := mustRun(t, e.codexTestEnv, v.ID)
		if run.EgressProfileID.Valid || !run.JobProtocol.Valid {
			t.Fatalf("unbound job: egress_profile_id %+v job_protocol %+v", run.EgressProfileID, run.JobProtocol)
		}
	})
}

// laneProtocolCaps is what a lane worker built for PRD #1976 registers with.
var laneProtocolCaps = []string{capability.JobRunnerV1, capability.JobFilesV1, capability.IsolatedFetchV1, capability.IsolatedJobV1}

// registeredLaneWorker provisions a run-bound ephemeral lane worker the way the provisioner does
// (isolated_lane set by the api), registers it through Service.Register with caps, and reloads
// the row, so the capabilities the claim sees are the ones registration stored.
func (e jobEnv) registeredLaneWorker(t *testing.T, u, run uuid.UUID, caps ...string) store.Worker {
	t.Helper()
	hash := make([]byte, 32)
	if _, err := rand.Read(hash); err != nil {
		t.Fatal(err)
	}
	w, err := e.q.CreateEphemeralHostedWorker(e.ctx, store.CreateEphemeralHostedWorkerParams{
		UserID: u, Name: "lane-" + run.String(), TokenHash: hash,
		TemplateDeclared: pgtype.Text{String: "base", Valid: true},
		HostedSize:       pgtype.Text{String: "small", Valid: true},
		DockerEnabled:    pgtype.Bool{Bool: false, Valid: true},
		EphemeralRunID:   run, AnthropicBindMode: BindModeDefault, IsolatedLane: true,
	})
	if err != nil {
		t.Fatalf("CreateEphemeralHostedWorker: %v", err)
	}
	if _, _, err := e.svc.Register(e.ctx, store.Worker(w), "test", "", nil, nil, caps, nil); err != nil {
		t.Fatalf("Register: %v", err)
	}
	loaded, err := e.q.GetWorkerByID(e.ctx, w.ID)
	if err != nil {
		t.Fatalf("reload worker: %v", err)
	}
	return loaded
}

// TestRegisterStoresIsolatedJobV1LiveDB: a lane worker's isolated_job_v1 report survives
// FilterProtocol and lands in workers.protocol_capabilities, and an unknown string beside it does
// not. Reads the stored row, not the filter.
//
// CALIBRATION: remove IsolatedJobV1 from capability.protocolVocabulary; this goes red.
func TestRegisterStoresIsolatedJobV1LiveDB(t *testing.T) {
	e := setupJobLiveDB(t, 0)
	u := e.seedJobUser(t)
	e.ownJobsCleanup(t, u)
	_, name := e.seedProfile(t, "docs.example.com")
	v, err := e.svc.CreateJobRun(e.ctx, profileReq(cliCaller(u), name))
	if err != nil {
		t.Fatal(err)
	}
	w := e.registeredLaneWorker(t, u, v.ID, append([]string{"not_a_capability"}, laneProtocolCaps...)...)
	for _, c := range laneProtocolCaps {
		if !slices.Contains(w.ProtocolCapabilities, c) {
			t.Errorf("stored protocol_capabilities = %v, missing %s", w.ProtocolCapabilities, c)
		}
	}
	if slices.Contains(w.ProtocolCapabilities, "not_a_capability") {
		t.Errorf("stored protocol_capabilities = %v kept an unknown string", w.ProtocolCapabilities)
	}
	if !w.IsolatedLane {
		t.Error("the lane marker is the api's, and registration must not clear it")
	}
}

// TestProfileBoundJobClaimGateLiveDB: the isolated_job_v1 rollout gate. A registered lane worker
// with the capability claims a profile-bound job; the same lane worker registered without it never
// does and the unservable-ephemeral sweeper fails its job; a non-lane job-capable worker never
// claims a profile-bound job, whatever it advertises.
//
// CALIBRATION: remove the `r.egress_profile_id IS NULL OR 'isolated_job_v1' = ANY(...)` arm from
// ClaimRun's job clause (store/runtime.sql.go, or queries/runtime.sql then regenerate): the
// "without isolated_job_v1" subtest then sees an old-image lane worker claim the job. Remove the
// same arm from LockUnservableEphemeralJobWorkers: the sweeper subtest finds nothing to fail.
func TestProfileBoundJobClaimGateLiveDB(t *testing.T) {
	t.Run("a lane worker registered with isolated_job_v1 claims it", func(t *testing.T) {
		e := setupJobLiveDB(t, 0)
		u := e.seedJobUser(t)
		e.ownJobsCleanup(t, u)
		e.makeTokenDefault(t, u)
		_, name := e.seedProfile(t, "docs.example.com")
		v, err := e.svc.CreateJobRun(e.ctx, profileReq(cliCaller(u), name))
		if err != nil {
			t.Fatal(err)
		}
		w := e.registeredLaneWorker(t, u, v.ID, laneProtocolCaps...)
		pl, err := e.svc.Claim(e.ctx, w, nil)
		if err != nil {
			t.Fatalf("Claim: %v", err)
		}
		if pl == nil || pl.RunID != v.ID.String() || pl.Job == nil {
			t.Fatalf("Claim = %+v, want the profile-bound job", pl)
		}
	})

	t.Run("a lane worker registered without isolated_job_v1 never claims it and the sweeper fails the job", func(t *testing.T) {
		e := setupJobLiveDB(t, 0)
		u := e.seedJobUser(t)
		e.ownJobsCleanup(t, u)
		e.makeTokenDefault(t, u)
		_, name := e.seedProfile(t, "docs.example.com")
		v, err := e.svc.CreateJobRun(e.ctx, profileReq(cliCaller(u), name))
		if err != nil {
			t.Fatal(err)
		}
		old := []string{capability.JobRunnerV1, capability.JobFilesV1, capability.IsolatedFetchV1}
		w := e.registeredLaneWorker(t, u, v.ID, old...)
		if slices.Contains(w.ProtocolCapabilities, capability.IsolatedJobV1) {
			t.Fatalf("setup: worker stored %v", w.ProtocolCapabilities)
		}
		pl, err := e.svc.Claim(e.ctx, w, nil)
		if err != nil || pl != nil {
			t.Fatalf("Claim = %+v, %v; want idle", pl, err)
		}
		if s := e.status(t, v.ID); s != "queued" {
			t.Fatalf("status = %s, want queued", s)
		}
		n, err := e.svc.FailJobsWithUnservableEphemeral(e.ctx, 10*time.Minute)
		if err != nil || n != 1 {
			t.Fatalf("sweeper = %d, %v; want 1 failed job", n, err)
		}
		run := mustRun(t, e.codexTestEnv, v.ID)
		if run.Status != "failed" || run.FailOrigin.String != "no_job_capable_worker" {
			t.Fatalf("job = %s / %q, want failed / no_job_capable_worker", run.Status, run.FailOrigin.String)
		}
		if e.workerExists(t, w.ID) {
			t.Fatal("the unservable ephemeral worker must be removed")
		}
	})

	t.Run("a non-lane job-capable worker never claims it, whatever it advertises", func(t *testing.T) {
		e := setupJobLiveDB(t, 0)
		u := e.seedJobUser(t)
		e.ownJobsCleanup(t, u)
		e.makeTokenDefault(t, u)
		_, name := e.seedProfile(t, "docs.example.com")
		v, err := e.svc.CreateJobRun(e.ctx, profileReq(cliCaller(u), name))
		if err != nil {
			t.Fatal(err)
		}
		id := e.seedWorkerRow(t, u, false, nil, laneProtocolCaps...)
		w := store.Worker{ID: id, UserID: u, Name: "plain", Status: "online", ProtocolCapabilities: laneProtocolCaps}
		pl, err := e.svc.Claim(e.ctx, w, nil)
		if err != nil || pl != nil {
			t.Fatalf("Claim = %+v, %v; want idle", pl, err)
		}
		if _, err := e.q.ClaimRun(e.ctx, e.claimParams(u, id, false, laneProtocolCaps, false)); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("ClaimRun by a non-lane worker = %v, want no rows", err)
		}
		if s := e.status(t, v.ID); s != "queued" {
			t.Fatalf("status = %s, want queued", s)
		}
	})
}

// TestProfileBoundJobClaimAssemblyLiveDB: the claim a profile-bound job receives carries the job
// block and the fetch grant, none of the repo or forge material, and exactly its product's
// approved skills; an unbound job's claim carries no grant and is what assembleJobClaim builds.
//
// CALIBRATION: drop the isolateClaimKeeping call from assembleClaim's job branch; the grant
// assertions go red. Make isolateClaimKeeping skip re-attaching the skills; the skills assertion
// goes red.
func TestProfileBoundJobClaimAssemblyLiveDB(t *testing.T) {
	e := setupJobLiveDB(t, 0)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	u := e.seedJobUser(t)
	e.ownJobsCleanup(t, u)
	e.makeTokenDefault(t, u)
	profID, name := e.seedProfile(t, "docs.example.com", "*.vendor.example")
	prod, tok := e.seedProduct(t, u, []string{"research"})
	other, _ := e.seedProduct(t, u, []string{"research"})
	e.allow(t, prod, profID)
	e.seedSkill(t, "voice-"+suffix, "product", nil, &prod, "product body")
	e.seedSkill(t, "foreign-"+suffix, "product", nil, &other, "another product's body")
	g := e.seedSkill(t, "glob-"+suffix, "global", nil, nil, "global body")
	usr := e.seedSkill(t, "mine-"+suffix, "user", &u, nil, "user body")
	tmpl := uuid.New()
	e.exec(`INSERT INTO agent_templates (id, name, description, prompt_body) VALUES ($1, $2, 'd', 'b')`, tmpl, "t-"+suffix)
	e.exec(`INSERT INTO agent_skill_allocations (template_id, skill_id, user_id) VALUES ($1, $2, NULL)`, tmpl, g)
	e.exec(`INSERT INTO agent_skill_allocations (template_id, skill_id, user_id) VALUES ($1, $2, $3)`, tmpl, usr, u)

	v, err := e.svc.CreateJobRun(e.ctx, profileReq(productCaller(u, prod, tok), name))
	if err != nil {
		t.Fatalf("CreateJobRun: %v", err)
	}
	w := e.registeredLaneWorker(t, u, v.ID, laneProtocolCaps...)
	pl, err := e.svc.Claim(e.ctx, w, nil)
	if err != nil || pl == nil || pl.RunID != v.ID.String() {
		t.Fatalf("Claim = %+v, %v", pl, err)
	}
	if pl.Job == nil || pl.Job.Prompt == "" || pl.Job.Type != "research" {
		t.Fatalf("job block = %+v", pl.Job)
	}
	if pl.Secrets.AnthropicOAuthToken == "" {
		t.Error("the job claim must carry the model credential")
	}
	f := pl.IsolatedFetch
	if f == nil || !strings.HasPrefix(f.Credential, "uzf_") || f.Profile != name || !slices.Equal(f.Hosts, []string{"docs.example.com", "*.vendor.example"}) {
		t.Fatalf("isolated_fetch = %+v, want a uzf_ credential, profile %q and the two hosts", f, name)
	}
	if pl.Secrets.ForgePAT != "" || pl.Secrets.ForgeUsername != "" || pl.Repo.URL != "" || len(pl.Agents) != 0 || pl.IssueComments != nil {
		t.Errorf("a profile-bound job claim carries repo or forge material: %+v", pl)
	}
	if got := skillNames(pl.Skills); strings.Join(got, ",") != "voice-"+suffix {
		t.Fatalf("skills = %v, want exactly the product's [voice-%s]", got, suffix)
	}
	if pl.SkillsDropped == nil {
		t.Error("skills_dropped must stay a non-nil slice")
	}
	raw, _ := json.Marshal(pl)
	for _, leaked := range []string{"glob-" + suffix, "mine-" + suffix, "foreign-" + suffix, "global body", "user body", "another product"} {
		if strings.Contains(string(raw), leaked) {
			t.Errorf("the wire claim carries %q", leaked)
		}
	}

	t.Run("an unbound job's claim has no grant and is exactly what assembleJobClaim builds", func(t *testing.T) {
		uv := e.seedJobUser(t)
		e.ownJobsCleanup(t, uv)
		e.makeTokenDefault(t, uv)
		jv, err := e.svc.CreateJobRun(e.ctx, jobReq(cliCaller(uv)))
		if err != nil {
			t.Fatal(err)
		}
		pl := e.claimNextJob(t, uv)
		if pl.RunID != jv.ID.String() || pl.IsolatedFetch != nil {
			t.Fatalf("unbound claim = run %s isolated_fetch %+v", pl.RunID, pl.IsolatedFetch)
		}
		run := mustRun(t, e.codexTestEnv, jv.ID)
		again, err := e.svc.assembleJobClaim(e.ctx, store.Worker{ID: run.WorkerID.Bytes, UserID: uv}, run)
		if err != nil {
			t.Fatalf("assembleJobClaim: %v", err)
		}
		a, _ := json.Marshal(pl)
		b, _ := json.Marshal(again)
		if string(a) != string(b) {
			t.Fatalf("the unbound claim differs from the bare job assembly:\n%s\n%s", a, b)
		}
		if strings.Contains(string(a), "isolated_fetch") {
			t.Error("an unbound claim must not carry an isolated_fetch key")
		}
	})
}
