package workersvc

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/agenttmpl"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// template_effort_livedb_test.go is the LiveDB half of issue #3258 M1 (the claim projection N4, the
// withheld-pin notice N6, the lead-template tier N3 and its claim/prediction parity). It reuses the
// effFix of effective_settings_livedb_test.go. Skipped unless UZI_TEST_DATABASE_URL points at a
// throwaway Postgres; run via ./e2e/run-store-it.sh.

// templateEffort inserts an enabled user template carrying effort pins (""  is NULL).
func (fx *effFix) templateEffort(name, effort, codexEffort string) {
	fx.t.Helper()
	id := uuid.New()
	fx.env.exec(`INSERT INTO agent_templates(id,name,description,prompt_body,scope,user_id,effort,codex_effort)
		VALUES($1,$2,'d','b','user',$3,NULLIF($4,''),NULLIF($5,''))`, id, name, fx.userID, effort, codexEffort)
	fx.env.exec(`INSERT INTO agent_template_allocations(template_id,user_id,enabled) VALUES($1,$2,true)`, id, fx.userID)
}

// claimWith assembles the claim of run as claim generation gen on a worker advertising caps.
func (fx *effFix) claimWith(run store.Run, gen int64, caps []string) *ClaimPayload {
	fx.t.Helper()
	fx.env.exec(`UPDATE runs SET status = 'claimed', worker_id = $2, claim_generation = $3 WHERE id = $1`, run.ID, fx.workerID, gen)
	w := store.Worker{ID: fx.workerID, UserID: fx.userID, ProtocolCapabilities: caps}
	payload, err := fx.svc.assembleClaim(fx.env.ctx, w, mustRun(fx.t, fx.env, run.ID))
	if err != nil {
		fx.t.Fatalf("assembleClaim: %v", err)
	}
	return payload
}

var (
	oldWorkerCaps = []string{capability.CodexHarnessV1, capability.CodexRuntimeV2, capability.CodexCustomModelV1}
	newWorkerCaps = []string{capability.CodexHarnessV1, capability.CodexRuntimeV2, capability.CodexCustomModelV1, capability.TemplateEffortV1}
)

func agentEfforts(p *ClaimPayload) map[string]string {
	out := map[string]string{}
	for _, a := range p.Agents {
		if a.Effort != nil {
			out[a.Name] = *a.Effort
		}
	}
	return out
}

type effortNoteRow struct {
	Seq             int32
	Agent           string
	ClaimGeneration int64
	Payload         string
}

func (fx *effFix) effortNotes(runID uuid.UUID) []effortNoteRow {
	fx.t.Helper()
	rows, err := fx.env.pool.Query(fx.env.ctx, `SELECT seq, COALESCE(agent,''), COALESCE(claim_generation,0), payload::text
		FROM run_messages WHERE run_id = $1 AND kind = 'status' AND payload->>'event' = 'template_effort_omitted' ORDER BY seq`, runID)
	if err != nil {
		fx.t.Fatalf("query notes: %v", err)
	}
	defer rows.Close()
	var out []effortNoteRow
	for rows.Next() {
		var r effortNoteRow
		if err := rows.Scan(&r.Seq, &r.Agent, &r.ClaimGeneration, &r.Payload); err != nil {
			fx.t.Fatalf("scan note: %v", err)
		}
		out = append(out, r)
	}
	return out
}

// TestTemplateEffortClaimProjectionLiveDB: the claim projects each template's effort for the run's
// harness only, only to a worker advertising template_effort_v1; a worker without it gets no effort
// key and exactly one run-feed notice per claim generation, and a claim that withholds nothing
// writes none.
func TestTemplateEffortClaimProjectionLiveDB(t *testing.T) {
	for _, h := range []Harness{HarnessClaude, HarnessCodex} {
		wantCoder := map[Harness]string{HarnessClaude: "high", HarnessCodex: "low"}[h]
		t.Run(string(h)+"/capable worker gets the family effort and no notice", func(t *testing.T) {
			fx := newEffFix(t)
			fx.anthropic()
			fx.codexDefault()
			fx.templateEffort(effLeadName, "max", "xhigh")
			fx.templateEffort("eff-coder", "high", "low")
			fx.templateEffort("eff-plain", "", "")
			run := fx.mustCreate(&h)
			p := fx.claimWith(run, 1, newWorkerCaps)
			got := agentEfforts(p)
			// The lead template's effort travels only via default_effort, never as an agent pin.
			if len(got) != 1 || got["eff-coder"] != wantCoder {
				t.Fatalf("agent efforts = %v, want eff-coder=%s only (the lead carries no effort)", got, wantCoder)
			}
			if n := fx.effortNotes(run.ID); len(n) != 0 {
				t.Fatalf("capable worker got notices: %+v", n)
			}
		})
		t.Run(string(h)+"/old worker gets no effort and one notice per generation", func(t *testing.T) {
			fx := newEffFix(t)
			fx.anthropic()
			fx.codexDefault()
			fx.templateEffort(effLeadName, "max", "xhigh")
			fx.templateEffort("eff-coder", "high", "low")
			run := fx.mustCreate(&h)
			before := mustRun(t, fx.env, run.ID).LastSeq
			p := fx.claimWith(run, 1, oldWorkerCaps)
			if got := agentEfforts(p); len(got) != 0 {
				t.Fatalf("old worker received efforts %v", got)
			}
			raw, _ := json.Marshal(p.Agents)
			if json.Valid(raw) && containsKey(raw, "effort") {
				t.Fatalf("old worker wire carries an effort key: %s", raw)
			}
			notes := fx.effortNotes(run.ID)
			if len(notes) != 1 {
				t.Fatalf("notices after generation 1 = %+v, want exactly one", notes)
			}
			n := notes[0]
			if n.Agent != "worker" || n.ClaimGeneration != 1 || n.Seq != before+1 {
				t.Errorf("note = %+v, want agent worker, generation 1, seq %d", n, before+1)
			}
			if p.LastSeq != n.Seq || mustRun(t, fx.env, run.ID).LastSeq != n.Seq {
				t.Errorf("claim last_seq = %d, run last_seq = %d, want the note's seq %d", p.LastSeq, mustRun(t, fx.env, run.ID).LastSeq, n.Seq)
			}
			var body struct{ Event, Text string }
			if err := json.Unmarshal([]byte(n.Payload), &body); err != nil {
				t.Fatal(err)
			}
			if body.Event != "template_effort_omitted" || body.Text != "stored subagent effort pins omitted: this worker predates template effort; those subagents use the run's effort" {
				t.Errorf("note payload = %+v", body)
			}

			fx.claimWith(run, 2, oldWorkerCaps)
			notes = fx.effortNotes(run.ID)
			if len(notes) != 2 || notes[0].ClaimGeneration != 1 || notes[1].ClaimGeneration != 2 {
				t.Fatalf("notices after generation 2 = %+v, want one per generation", notes)
			}
		})
		t.Run(string(h)+"/nothing withheld writes no notice", func(t *testing.T) {
			fx := newEffFix(t)
			fx.anthropic()
			fx.codexDefault()
			// A lead-only pin is not a subagent pin, and the other family's pin never applies.
			fx.templateEffort(effLeadName, "max", "xhigh")
			other := map[Harness][2]string{HarnessClaude: {"", "low"}, HarnessCodex: {"high", ""}}[h]
			fx.templateEffort("eff-other-family", other[0], other[1])
			run := fx.mustCreate(&h)
			p := fx.claimWith(run, 1, oldWorkerCaps)
			if n := fx.effortNotes(run.ID); len(n) != 0 {
				t.Fatalf("notices = %+v, want none", n)
			}
			if p.LastSeq != mustRun(t, fx.env, run.ID).LastSeq {
				t.Errorf("claim last_seq %d != run last_seq %d", p.LastSeq, mustRun(t, fx.env, run.ID).LastSeq)
			}
		})
	}
}

// TestTemplateEffortOmittedNoteStaleGenerationLiveDB: a re-assembly holding a stale run row (the claim
// generation has since moved) persists no notice and returns the seq it was handed, so the claim
// payload never adopts a seq that no row carries.
//
// MUTATION: delete the `if !res.Inserted { return startLastSeq }` guard in
// recordTemplateEffortOmittedNote; the stale call then returns startLastSeq+1 and this test fails.
func TestTemplateEffortOmittedNoteStaleGenerationLiveDB(t *testing.T) {
	fx := newEffFix(t)
	fx.anthropic()
	fx.templateEffort("eff-coder", "high", "low")
	h := HarnessClaude
	run := fx.mustCreate(&h)
	fx.claimWith(run, 1, newWorkerCaps) // capable worker: no notice written
	stale := mustRun(t, fx.env, run.ID)
	fx.env.exec(`UPDATE runs SET claim_generation = 2 WHERE id = $1`, run.ID)
	lastSeq := mustRun(t, fx.env, run.ID).LastSeq

	got := fx.svc.recordTemplateEffortOmittedNote(fx.env.ctx, stale, lastSeq)

	if got != lastSeq {
		t.Errorf("stale call returned seq %d, want the passed %d", got, lastSeq)
	}
	if n := fx.effortNotes(run.ID); len(n) != 0 {
		t.Errorf("stale call persisted notices: %+v", n)
	}
	if after := mustRun(t, fx.env, run.ID).LastSeq; after != lastSeq {
		t.Errorf("runs.last_seq = %d, want unchanged %d", after, lastSeq)
	}
}

func containsKey(raw []byte, key string) bool {
	var list []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &list); err != nil {
		return false
	}
	for _, m := range list {
		if _, ok := m[key]; ok {
			return true
		}
	}
	return false
}

// TestTemplateEffortLeadTierParityLiveDB: the lead tier on the real claim path against the effective
// settings prediction, so claim and prediction cannot drift. The lead template's effort fills only an
// unset preference; the lead tier travels in default_effort regardless of the worker's capability.
func TestTemplateEffortLeadTierParityLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name       string
		h          Harness
		userEffort string // "" = NULL
		column     string
		wantEffort string
		wantSource string
	}{
		{"claude NULL preference takes the template", HarnessClaude, "", "default_effort", "high", "template"},
		{"codex NULL preference takes the template", HarnessCodex, "", "default_codex_effort", "high", "template"},
		{"claude user level wins", HarnessClaude, "low", "default_effort", "low", "user_default"},
		{"codex user level wins", HarnessCodex, "low", "default_codex_effort", "low", "user_default"},
		{"claude runtime wins and sends nothing", HarnessClaude, "runtime", "default_effort", "", "user_default"},
		{"codex runtime wins and sends nothing", HarnessCodex, "runtime", "default_codex_effort", "", "user_default"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newEffFix(t)
			fx.anthropic()
			fx.codexDefault()
			fx.templateEffort(effLeadName, "high", "high")
			fx.lane(tc.column, tc.userEffort)
			h := tc.h
			run := fx.mustCreate(&h)
			lead := fx.predictLead(h, false)
			for _, caps := range [][]string{oldWorkerCaps, newWorkerCaps} {
				p := fx.claimWith(run, 1, caps)
				if got := derefOr(p.Config.DefaultEffort, ""); got != tc.wantEffort {
					t.Errorf("claim default_effort = %q, want %q", got, tc.wantEffort)
				}
				if got := derefOr(p.Config.DefaultEffortSource, ""); got != tc.wantSource {
					t.Errorf("claim default_effort_source = %q, want %q", got, tc.wantSource)
				}
			}
			if got := derefOr(lead.Effort, ""); got != tc.wantEffort || lead.EffortSource != tc.wantSource {
				t.Errorf("predicted effort = %q (%s), want %q (%s)", got, lead.EffortSource, tc.wantEffort, tc.wantSource)
			}
		})
	}
}

// TestTemplateEffortLeadSelectionParityLiveDB: the builtin lead is switched off for the user by an
// overlay while a global orchestrator is delivered. The claim (agenttmpl.SelectLead over the
// delivered roster) and the prediction (the lateral over lead and orchestrator) must both read the
// orchestrator's effort, never the disabled builtin lead's.
func TestTemplateEffortLeadSelectionParityLiveDB(t *testing.T) {
	for _, h := range []Harness{HarnessClaude, HarnessCodex} {
		t.Run(string(h), func(t *testing.T) {
			fx := newEffFix(t)
			fx.anthropic()
			fx.codexDefault()
			// newEffFix reconciled the builtins and disabled every shared template for the user. Give
			// the disabled builtin lead an effort that must not be read, then deliver a global
			// orchestrator with a different one.
			// The database is shared with every other test and package, so both seeds are undone.
			res, err := fx.env.pool.Exec(fx.env.ctx, `UPDATE agent_templates SET effort = 'max', codex_effort = 'max' WHERE scope = 'builtin' AND lower(name) = 'lead'`)
			if err != nil || res.RowsAffected() != 1 {
				t.Fatalf("builtin lead effort seed: rows=%d err=%v", res.RowsAffected(), err)
			}
			id := uuid.New()
			t.Cleanup(func() {
				fx.env.exec(`UPDATE agent_templates SET effort = NULL, codex_effort = NULL WHERE scope = 'builtin' AND lower(name) = 'lead'`)
				fx.env.exec(`DELETE FROM agent_template_allocations WHERE template_id = $1`, id)
				fx.env.exec(`DELETE FROM agent_templates WHERE id = $1`, id)
			})
			fx.env.exec(`INSERT INTO agent_templates(id,name,description,prompt_body,scope,effort,codex_effort)
				VALUES($1,'orchestrator','d','b','global','low','xhigh')`, id)
			fx.env.exec(`INSERT INTO agent_template_allocations(template_id,user_id,enabled) VALUES($1,NULL,true)`, id)
			want := map[Harness]string{HarnessClaude: "low", HarnessCodex: "xhigh"}[h]

			hh := h
			run := fx.mustCreate(&hh)
			lead := fx.predictLead(h, false)
			p := fx.claimWith(run, 1, newWorkerCaps)
			if got := derefOr(p.Config.DefaultEffort, ""); got != want || derefOr(p.Config.DefaultEffortSource, "") != "template" {
				t.Errorf("claim effort = %q (%s), want %q (template)", got, derefOr(p.Config.DefaultEffortSource, ""), want)
			}
			if got := derefOr(lead.Effort, ""); got != want || lead.EffortSource != "template" {
				t.Errorf("predicted effort = %q (%s), want %q (template)", got, lead.EffortSource, want)
			}
			// The lead-shaped orchestrator's effort travels only via default_effort (N3), never as an
			// agent pin that would bypass the owner's preference.
			if got, ok := agentEfforts(p)["orchestrator"]; ok {
				t.Errorf("delivered orchestrator carries agent effort %q, want none", got)
			}
		})
	}
}

// TestTemplateEffortNonLeadLaneIgnoresTemplateLiveDB is the lane control for the lead-template tier:
// a Claude task-review run (not a lead lane) of an owner whose lead template pins an effort keeps the
// builtin default_effort and carries no source, never the template's value.
//
// MUTATION: in claim_assembly.go change the non-lead leadEffortFor line to leadEffortWithTemplate
// with leadTemplateEffort(templates, harness); this test then reads "high" and fails.
func TestTemplateEffortNonLeadLaneIgnoresTemplateLiveDB(t *testing.T) {
	fx := newEffFix(t)
	fx.anthropic()
	fx.templateEffort(effLeadName, "high", "high")

	targetID := uuid.New()
	fx.env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, branch, base_branch, issue_title, issue_description, status, harness)
	          VALUES ($1, $2, $3, 'task', $4, 'main', 't', 'd', 'completed', 'claude')`,
		targetID, fx.userID, fx.repoID, "uzi/task/"+targetID.String())
	reviewID := uuid.New()
	fx.env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, branch, base_branch, review_target_run_id, dispatched_at, issue_title, issue_description, status, worker_id, harness)
	          VALUES ($1, $2, $3, 'task', $4, 'main', $5, now(), 't', 'd', 'claimed', $6, 'claude')`,
		reviewID, fx.userID, fx.repoID, "uzi/task/"+reviewID.String(), targetID, fx.workerID)
	run := mustRun(t, fx.env, reviewID)
	if !run.ReviewTargetRunID.Valid {
		t.Fatal("fixture must be a review run")
	}

	p := fx.claimWith(run, 1, newWorkerCaps)
	if got := derefOr(p.Config.DefaultEffort, ""); got != "medium" {
		t.Errorf("non-lead claim default_effort = %q, want the builtin medium, not the lead template's high", got)
	}
	if p.Config.DefaultEffortSource != nil {
		t.Errorf("non-lead claim default_effort_source = %q, want none", *p.Config.DefaultEffortSource)
	}
}

// TestTemplateEffortJudgeLaneIgnoresTemplateLiveDB is the judge half of the lane control (PRD #3258
// test (e)): an owner with no Claude effort preference whose lead template pins `high` still gets a
// judge claim at the builtin medium with no source. The judge assembler has its own effort read and
// loads no templates, so this pins the observable outcome against a future shared resolver.
func TestTemplateEffortJudgeLaneIgnoresTemplateLiveDB(t *testing.T) {
	fx := newEffFix(t)
	fx.anthropic()
	fx.templateEffort(effLeadName, "high", "high")

	target := uuid.New()
	fx.env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status)
		VALUES ($1, $2, $3, 'issue', 950, 't', 'd', 'completed')`, target, fx.userID, fx.repoID)
	id := uuid.New()
	fx.env.exec(`INSERT INTO runs (id, user_id, kind, issue_title, issue_description, status, target_run_id, harness)
		VALUES ($1, $2, 'judge', 't', 'd', 'queued', $3, 'claude')`, id, fx.userID, target)
	fx.env.exec(`UPDATE runs SET status = 'claimed', worker_id = $2, claim_generation = 1 WHERE id = $1`, id, fx.workerID)
	p, err := fx.svc.assembleJudgeClaim(fx.env.ctx, store.Worker{ID: fx.workerID, UserID: fx.userID, ProtocolCapabilities: newWorkerCaps}, mustRun(t, fx.env, id))
	if err != nil {
		t.Fatalf("assembleJudgeClaim: %v", err)
	}
	if got := derefOr(p.Config.DefaultEffort, ""); got != "medium" {
		t.Errorf("judge claim default_effort = %q, want the builtin medium, not the lead template's high", got)
	}
	if p.Config.DefaultEffortSource != nil {
		t.Errorf("judge claim default_effort_source = %q, want none", *p.Config.DefaultEffortSource)
	}
}

// TestTemplateEffortCheckerLaneIgnoresTemplateLiveDB is the checker half of the lane control (PRD
// #3258 test (e)): a Codex cross-check child of an owner with no Codex effort preference whose lead
// template pins codex_effort `low` keeps its frozen cross-check default (high), never the template's.
//
// MUTATION: in claim_assembly.go set the checker arm's defaultEffort to
// leadTemplateEffort(templates, harness) instead of the frozen checkerResolution.Effort, or prefer
// that template effort only when it is set; either way the claim reads the template's low and this
// test fails.
func TestTemplateEffortCheckerLaneIgnoresTemplateLiveDB(t *testing.T) {
	env, svc, w, lead, c := roundsPinsFixture(t)
	env.exec("UPDATE users SET default_codex_effort = NULL, default_effort = NULL WHERE id = $1", w.UserID)
	// Make the fixture template the delivered lead in every test order, as newEffFix does: seed the
	// builtin roster, switch all of it off for this user, and refuse a shared-name collision (a user
	// template sharing a shared name is excluded from the claim roster).
	if _, err := store.ReconcileBuiltinTemplates(env.ctx, env.q); err != nil {
		t.Fatalf("reconcile builtins: %v", err)
	}
	env.exec(`INSERT INTO agent_template_allocations(template_id,user_id,enabled)
		SELECT id,$1,false FROM agent_templates
		ON CONFLICT (template_id,(COALESCE(user_id,'00000000-0000-0000-0000-000000000000'::uuid)))
		DO UPDATE SET enabled=false`, w.UserID)
	var shared int
	if err := env.pool.QueryRow(env.ctx, `SELECT count(*) FROM agent_templates WHERE scope <> 'user' AND name = $1`, effLeadName).Scan(&shared); err != nil || shared != 0 {
		t.Fatalf("shared name probe for %q: count=%d err=%v", effLeadName, shared, err)
	}
	tid := uuid.New()
	env.exec(`INSERT INTO agent_templates(id,name,description,prompt_body,scope,user_id,effort,codex_effort)
		VALUES($1,$2,'d','b','user',$3,'low','low')`, tid, effLeadName, w.UserID)
	env.exec(`INSERT INTO agent_template_allocations(template_id,user_id,enabled) VALUES($1,$2,true)`, tid, w.UserID)
	// Precondition: the claim roster's lead is this template and it pins codex_effort low, so the
	// control below compares against a template the checker claim really sees.
	rows, err := env.q.ListClaimAgentTemplates(env.ctx, pgconv.UUID(w.UserID))
	if err != nil {
		t.Fatalf("list claim templates: %v", err)
	}
	if te := leadTemplateEffort(rows, HarnessCodex); !te.Valid || te.String != "low" {
		t.Fatalf("fixture lead template effort = %+v, want codex low on the claim roster", te)
	}

	cc := roundsPinsSubmit(t, env, svc, w, lead, c, 1)
	p := roundsPinsClaim(t, env, svc, w, cc)
	if got := derefOr(p.Config.DefaultEffort, ""); got != agenttmpl.CrossCheckDefaultEffort {
		t.Errorf("checker claim default_effort = %q, want the cross-check default %q, not the lead template's low", got, agenttmpl.CrossCheckDefaultEffort)
	}
	if p.CrossCheck.EffortSource == nil || *p.CrossCheck.EffortSource != "cross-check default" {
		t.Errorf("checker effort source = %v, want cross-check default", p.CrossCheck.EffortSource)
	}
	if p.Config.DefaultEffortSource != nil {
		t.Errorf("checker claim default_effort_source = %q, want none", *p.Config.DefaultEffortSource)
	}
}
