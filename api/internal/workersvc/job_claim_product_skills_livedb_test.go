package workersvc

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1909 M6 (D9): claim-time delivery of product skills. A job started through a registered
// product gets exactly that product's APPLIED skills; a uzc_ job and every non-job run get none
// of them; and a product job gets no user, global or builtin skill. Skipped unless
// UZI_TEST_DATABASE_URL points at a throwaway Postgres (./e2e/run-store-it.sh).

func (e jobEnv) seedSkill(t *testing.T, name, scope string, owner, product *uuid.UUID, body string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	e.exec(`INSERT INTO skills (id, name, description, body, scope, user_id, product_id) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		id, name, name+" description.", body, scope, owner, product)
	return id
}

// claimNextJob claims the next job for a fresh capable worker of u and returns its payload.
func (e jobEnv) claimNextJob(t *testing.T, u uuid.UUID) *ClaimPayload {
	t.Helper()
	id := e.seedWorkerRow(t, u, false, nil, jobCap, jobFilesCap)
	w := store.Worker{ID: id, UserID: u, Name: "w-" + id.String()[:8], Status: "online", ProtocolCapabilities: []string{jobCap, jobFilesCap}}
	pl, err := e.svc.Claim(e.ctx, w, nil)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if pl == nil || pl.Job == nil {
		t.Fatalf("Claim = %+v, want a job", pl)
	}
	return pl
}

// ownJobsCleanup removes the job runs a subtest created once it ends. The LiveDB packages share one
// database, and sweeps such as TestCancelRevokedProductJobsLiveDB assert on instance-wide job
// state: a job left behind for a product the subtest disabled would fail them.
func (e jobEnv) ownJobsCleanup(t *testing.T, u uuid.UUID) {
	t.Helper()
	t.Cleanup(func() { e.exec(`DELETE FROM runs WHERE user_id = $1 AND kind = 'job'`, u) })
}

func skillNames(s []ClaimSkill) []string {
	out := make([]string, 0, len(s))
	for _, c := range s {
		out = append(out, c.Name)
	}
	sort.Strings(out)
	return out
}

// TestProductJobClaimSkillsLiveDB: the four delivery rules on the real claim path.
func TestProductJobClaimSkillsLiveDB(t *testing.T) {
	e := setupJobLiveDB(t, 0)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]

	t.Run("a product job gets exactly its product's applied skills and no user, global or builtin skill", func(t *testing.T) {
		u := e.seedJobUser(t)
		e.ownJobsCleanup(t, u)
		e.makeTokenDefault(t, u)
		prod, tok := e.seedProduct(t, u, []string{"research"})
		other, _ := e.seedProduct(t, u, []string{"research"})
		e.seedSkill(t, "voice-"+suffix, "product", nil, &prod, "product body")
		e.seedSkill(t, "tone-"+suffix, "product", nil, &prod, "tone body")
		e.seedSkill(t, "foreign-"+suffix, "product", nil, &other, "another product's body")
		// Every non-product scope, allocated to a template for this owner (shared and overlay):
		// the assembly a normal run uses would deliver all three.
		g := e.seedSkill(t, "glob-"+suffix, "global", nil, nil, "global body")
		b := e.seedSkill(t, "built-"+suffix, "builtin", nil, nil, "builtin body")
		usr := e.seedSkill(t, "mine-"+suffix, "user", &u, nil, "user body")
		tmpl := uuid.New()
		e.exec(`INSERT INTO agent_templates (id, name, description, prompt_body) VALUES ($1, $2, 'd', 'b')`, tmpl, "t-"+suffix)
		e.exec(`INSERT INTO agent_skill_allocations (template_id, skill_id, user_id) VALUES ($1, $2, NULL), ($1, $3, NULL)`, tmpl, g, b)
		e.exec(`INSERT INTO agent_skill_allocations (template_id, skill_id, user_id) VALUES ($1, $2, $3)`, tmpl, usr, u)

		if _, err := e.svc.CreateJobRun(e.ctx, jobReq(productCaller(u, prod, tok))); err != nil {
			t.Fatalf("CreateJobRun: %v", err)
		}
		pl := e.claimNextJob(t, u)
		want := []string{"tone-" + suffix, "voice-" + suffix}
		if got := skillNames(pl.Skills); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("product job skills = %v, want exactly %v", got, want)
		}
		for _, s := range pl.Skills {
			if s.Description == "" || s.Body == "" {
				t.Errorf("skill %q lost its description or body: %+v", s.Name, s)
			}
		}
		if pl.SkillsDropped == nil || len(pl.SkillsDropped) != 0 {
			t.Errorf("skills_dropped = %#v, want a non-nil empty slice", pl.SkillsDropped)
		}
		raw, err := json.Marshal(pl)
		if err != nil {
			t.Fatal(err)
		}
		for _, leaked := range []string{"glob-" + suffix, "built-" + suffix, "mine-" + suffix, "foreign-" + suffix, "global body", "user body", "another product"} {
			if strings.Contains(string(raw), leaked) {
				t.Errorf("the wire claim of a product job carries %q", leaked)
			}
		}
	})

	t.Run("a uzc_ job (no origin product) gets no product skill, even with product skills in the database", func(t *testing.T) {
		u := e.seedJobUser(t)
		e.ownJobsCleanup(t, u)
		e.makeTokenDefault(t, u)
		prod, _ := e.seedProduct(t, u, []string{"research"})
		e.seedSkill(t, "someones-"+suffix, "product", nil, &prod, "body")
		if _, err := e.svc.CreateJobRun(e.ctx, jobReq(cliCaller(u))); err != nil {
			t.Fatalf("CreateJobRun: %v", err)
		}
		pl := e.claimNextJob(t, u)
		if pl.Skills == nil || len(pl.Skills) != 0 {
			t.Fatalf("a uzc_ job's skills = %#v, want a non-nil empty slice", pl.Skills)
		}
	})

	t.Run("an unapproved (staged) set never reaches a claim", func(t *testing.T) {
		u := e.seedJobUser(t)
		e.ownJobsCleanup(t, u)
		e.makeTokenDefault(t, u)
		prod, tok := e.seedProduct(t, u, []string{"research"})
		// A staged snapshot exists (product_skill_staged) but nothing is applied (no skills row).
		e.exec(`INSERT INTO product_skill_staged (product_id, source_sha, skills) VALUES ($1, $2, $3::jsonb)`,
			prod, strings.Repeat("a", 40), `[{"name":"staged-only","description":"d","body":"staged body"}]`)
		if _, err := e.svc.CreateJobRun(e.ctx, jobReq(productCaller(u, prod, tok))); err != nil {
			t.Fatalf("CreateJobRun: %v", err)
		}
		pl := e.claimNextJob(t, u)
		if len(pl.Skills) != 0 {
			t.Fatalf("a staged, unapproved set reached a claim: %+v", pl.Skills)
		}
		raw, _ := json.Marshal(pl)
		if strings.Contains(string(raw), "staged-only") || strings.Contains(string(raw), "staged body") {
			t.Fatalf("the staged set is in the wire claim: %s", raw)
		}
	})

	t.Run("a disabled product's job gets none", func(t *testing.T) {
		u := e.seedJobUser(t)
		e.ownJobsCleanup(t, u)
		e.makeTokenDefault(t, u)
		prod, tok := e.seedProduct(t, u, []string{"research"})
		e.seedSkill(t, "off-"+suffix, "product", nil, &prod, "body")
		if _, err := e.svc.CreateJobRun(e.ctx, jobReq(productCaller(u, prod, tok))); err != nil {
			t.Fatalf("CreateJobRun: %v", err)
		}
		// Disabled after the job was created (the job row and its origin remain).
		e.exec(`UPDATE products SET enabled = false WHERE id = $1`, prod)
		pl := e.claimNextJob(t, u)
		if len(pl.Skills) != 0 {
			t.Fatalf("a disabled product's job received %+v", pl.Skills)
		}
	})

	t.Run("the caps apply: an oversized body is dropped too_large, the rest is cut to the per-run cap in name order", func(t *testing.T) {
		u := e.seedJobUser(t)
		e.ownJobsCleanup(t, u)
		e.makeTokenDefault(t, u)
		prod, tok := e.seedProduct(t, u, []string{"research"})
		e.seedSkill(t, "a-"+suffix, "product", nil, &prod, "ok")
		e.seedSkill(t, "b-"+suffix, "product", nil, &prod, "ok")
		e.seedSkill(t, "c-"+suffix, "product", nil, &prod, "ok")
		e.seedSkill(t, "huge-"+suffix, "product", nil, &prod, strings.Repeat("x", 50))
		old := e.svc.p
		e.svc.p.SkillMaxBytes, e.svc.p.SkillsMaxPerRun = 10, 2
		t.Cleanup(func() { e.svc.p = old })
		if _, err := e.svc.CreateJobRun(e.ctx, jobReq(productCaller(u, prod, tok))); err != nil {
			t.Fatalf("CreateJobRun: %v", err)
		}
		pl := e.claimNextJob(t, u)
		if got, want := strings.Join(skillNames(pl.Skills), ","), "a-"+suffix+",b-"+suffix; got != want {
			t.Fatalf("delivered = %s, want %s", got, want)
		}
		drops := map[string]string{}
		for _, d := range pl.SkillsDropped {
			drops[d.Name] = d.Reason
		}
		if drops["huge-"+suffix] != DropTooLarge || drops["c-"+suffix] != DropOverLimit || len(drops) != 2 {
			t.Fatalf("drops = %v, want huge too_large and c over_limit", drops)
		}
		if pl.Config.SkillMaxBytes != 10 || pl.Config.SkillsMaxPerRun != 2 {
			t.Errorf("config caps = %d / %d, want the job claim to carry them (10 / 2)", pl.Config.SkillMaxBytes, pl.Config.SkillsMaxPerRun)
		}
	})
}

// TestNonJobRunNeverReceivesProductSkillsLiveDB: an ordinary issue run is claimed through the same
// assembleClaim a long-standing run kind uses. A product skill exists AND corrupted allocation rows
// (inserted directly, as a handler bug would) point at it; the run's claim carries the legitimate
// global skill (positive control) and no product skill.
func TestNonJobRunNeverReceivesProductSkillsLiveDB(t *testing.T) {
	f := newIsoFix(t)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	env := f.env

	prod := uuid.New()
	env.exec(`INSERT INTO products (id, name) VALUES ($1, $2)`, prod, "prod-"+prod.String())
	psID, gID := uuid.New(), uuid.New()
	env.exec(`INSERT INTO skills (id, name, description, body, scope, product_id) VALUES ($1, $2, 'd', 'PRODUCT-ONLY-BODY', 'product', $3)`, psID, "p-"+suffix, prod)
	env.exec(`INSERT INTO skills (id, name, description, body, scope) VALUES ($1, $2, 'd', 'global body', 'global')`, gID, "g-"+suffix)
	tmpl := uuid.New()
	env.exec(`INSERT INTO agent_templates (id, name, description, prompt_body) VALUES ($1, $2, 'd', 'b')`, tmpl, "t-"+suffix)
	env.exec(`INSERT INTO agent_skill_allocations (template_id, skill_id, user_id) VALUES ($1, $2, NULL), ($1, $3, NULL)`, tmpl, gID, psID)
	env.exec(`INSERT INTO agent_skill_allocations (template_id, skill_id, user_id) VALUES ($1, $2, $3)`, tmpl, psID, f.userID)

	run := f.queuedRun(t, 4091, false)
	pl := f.claim(t, run)
	var sawGlobal bool
	for _, s := range pl.Skills {
		if s.Name == "p-"+suffix || s.Body == "PRODUCT-ONLY-BODY" {
			t.Fatalf("an issue run's claim carries a product skill: %+v", s)
		}
		if s.Name == "g-"+suffix {
			sawGlobal = true
		}
	}
	if !sawGlobal {
		t.Fatalf("the positive control failed: the allocated global skill is not in the claim (%v), so this test proves nothing", skillNames(pl.Skills))
	}
	raw, _ := json.Marshal(pl)
	if strings.Contains(string(raw), "PRODUCT-ONLY-BODY") {
		t.Fatal("a product skill body is in an issue run's wire claim")
	}
}
