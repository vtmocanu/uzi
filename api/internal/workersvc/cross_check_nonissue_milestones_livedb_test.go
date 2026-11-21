package workersvc

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4555 live-DB coverage: a non-issue lead (mr_rework, task) has no milestones to check, so
// its plan cross-check candidate is keyed on canonical [] whatever the worker sent; an issue
// lead keeps today's identity and refusals. A cross_checks row written before this rule (keyed
// on the received milestones) stays replayable.

// Independent goldens: sha256 of the digest payload spelled out by hand for the fixture
// candidate (plan "plan", base commit 40 x "a", diff "diff", size s, no capabilities or tools),
// not recomputed with the service's own code.
const (
	ccNIGoldenEmpty = "ba85eacf89947b347ffc8bc1d678fbd7b027f5730b17401d60d1e5a87aa9e209" // milestones []
	ccNIGoldenFirst = "9b49aa3a7c45d5a5e67c030f019ea8de4afdaafc171553e04a44092861a22871" // [{"id":"m1","title":"First"}]
)

var ccNINonIssueKinds = []string{"mr_rework", "task"}

func ccNISetKind(f gatedFixture, kind string) {
	switch kind {
	case "mr_rework":
		// runs_kind_shape: an mr_rework row needs a pipeline_ref, mr_iid and a source run.
		f.env.exec(`UPDATE runs SET kind='mr_rework', pipeline_ref='agent/issue-2150', mr_iid=7, target_run_id=id WHERE id=$1`, f.lead)
	case "task":
		f.env.exec(`UPDATE runs SET kind='task', issue_iid=NULL, branch='uzi/task/ni' WHERE id=$1`, f.lead)
	case "prompt":
		f.env.exec(`UPDATE runs SET kind='prompt', issue_iid=NULL WHERE id=$1`, f.lead)
	case "ci_fix":
		f.env.exec(`UPDATE runs SET kind='ci_fix', pipeline_id=11, pipeline_ref='agent/issue-2150' WHERE id=$1`, f.lead)
	case "issue":
	default:
		panic("unknown kind " + kind)
	}
}

// ccNIFixture is an automatic (or gated) lead of the given kind and harness.
func ccNIFixture(t *testing.T, kind, harness string, gated bool) gatedFixture {
	t.Helper()
	var f gatedFixture
	if gated {
		f = newGatedFixture(t, 2)
	} else {
		f = ccAutomaticFixture(t, 2)
	}
	ccNISetKind(f, kind)
	if harness == "codex" {
		f.env.exec(`UPDATE runs SET harness='codex' WHERE id=$1`, f.lead)
		seedDefaultAnthropicToken(t, f.env, f.w.UserID)
		caps := "'cross_check_v1','cross_check_rounds_v1','cross_check_codex_lead_v1'"
		if gated {
			caps += ",'cross_check_gated_v1'"
		}
		f.env.exec("UPDATE workers SET protocol_capabilities=ARRAY["+caps+"] WHERE id=$1", f.w.ID)
	}
	return f
}

// ccNIDigest is the digest the service stores for c after `times` normalisations of its
// milestones (1 = one service pass; 2 = the pre-change handler-then-service path), with the
// lead's inferred-capability policy applied as the service does. It also returns the
// normalised milestone bytes.
func ccNIDigest(t *testing.T, f gatedFixture, c PlanCrossCheckCandidate, raw string, times int) ([]byte, []byte) {
	t.Helper()
	c.RequiredCapabilities = append([]string(nil), c.RequiredCapabilities...)
	c.RequiredTools = append([]string(nil), c.RequiredTools...)
	c.Milestones = json.RawMessage(raw)
	for i := 0; i < times; i++ {
		var err error
		if c, err = NormalizePlanCrossCheckCandidate(c); err != nil {
			t.Fatalf("normalize %q: %v", raw, err)
		}
	}
	c.RequiredCapabilities = applyInferredCapabilityPolicy(mustRun(t, f.env, f.lead), sortedSet(c.RequiredCapabilities))
	c.RequiredTools = sortedSet(c.RequiredTools)
	d, err := c.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return d, c.Milestones
}

// ccNIRunningFromCC is the running report a worker builds from the server's canonical
// candidate: every field comes from the cross_checks row.
func ccNIRunningFromCC(cc store.CrossCheck) StateRequest {
	ms := []Milestone{}
	if len(cc.Milestones) > 0 {
		if err := json.Unmarshal(cc.Milestones, &ms); err != nil {
			panic(err)
		}
	}
	caps := append([]string{}, cc.RequiredCapabilities...)
	tools := append([]string{}, cc.RequiredTools...)
	plan, size, gen := cc.PlanMd.String, cc.SizeClass.String, cc.LeadClaimGeneration
	return StateRequest{State: "running", ClaimGeneration: &gen, PlanMd: &plan, Milestones: &ms,
		RequiredCapabilities: &caps, RequiredTools: &tools, SizeClass: &size, CandidateDigest: hex.EncodeToString(cc.CandidateDigest)}
}

func ccNIStoredRow(t *testing.T, f gatedFixture, round int32) store.CrossCheck {
	t.Helper()
	cc, err := f.env.q.GetExactPlanCrossCheck(f.env.ctx, store.GetExactPlanCrossCheckParams{LeadRunID: f.lead, Round: round})
	if err != nil {
		t.Fatal(err)
	}
	return cc
}

// ccNIRunningWrite applies the running report and asserts the plan and milestones were stored.
func ccNIRunningWrite(t *testing.T, f gatedFixture, req StateRequest, wantMilestones string) {
	t.Helper()
	if _, applied, err := f.svc.SetState(f.env.ctx, f.w, f.lead, req); err != nil || !applied {
		t.Fatalf("running write: applied=%v err=%v", applied, err)
	}
	run := mustRun(t, f.env, f.lead)
	if run.Status != "running" || run.PlanMd.String != *req.PlanMd {
		t.Fatalf("after running write: status=%s plan=%q", run.Status, run.PlanMd.String)
	}
	var frozen string
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT milestones_frozen::text FROM runs WHERE id=$1`, f.lead).Scan(&frozen); err != nil {
		t.Fatal(err)
	}
	if wantMilestones == "[]" {
		if frozen != "[]" {
			t.Fatalf("milestones_frozen = %s, want []", frozen)
		}
		return
	}
	var got, want []Milestone
	if err := json.Unmarshal([]byte(frozen), &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(wantMilestones), &want); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) || len(got) == 0 || got[0] != want[0] {
		t.Fatalf("milestones_frozen = %s, want %s", frozen, wantMilestones)
	}
}

func TestCrossCheckNonIssueMilestonesNewIdentityLiveDB(t *testing.T) {
	variants := []struct {
		name string
		raw  json.RawMessage
	}{
		{"valid stray list", json.RawMessage(`[{"id":"m1","title":"Stray"}]`)},
		{"absent", nil},
		{"null", json.RawMessage(`null`)},
		{"extra key", json.RawMessage(`[{"id":"m1","title":"t","extra":1}]`)},
		{"blank title", json.RawMessage(`[{"id":"m1","title":""}]`)},
	}
	for _, kind := range ccNINonIssueKinds {
		for _, harness := range []string{"claude", "codex"} {
			for _, v := range variants {
				t.Run(kind+"/"+harness+"/"+v.name, func(t *testing.T) {
					f := ccNIFixture(t, kind, harness, false)
					c := f.c
					c.Milestones = v.raw
					cc, err := f.svc.SubmitPlanCrossCheck(f.env.ctx, f.w, f.lead, 1, c, 1)
					if err != nil {
						t.Fatalf("non-issue submit refused: %v", err)
					}
					if rows, children := f.counts(t); rows != 1 || children != 1 {
						t.Fatalf("rows=%d children=%d", rows, children)
					}
					snap := ccAdmissionSnapshot(t, f, 1)
					if snap.Milestones != "[]" {
						t.Fatalf("stored cc.milestones = %s, want []", snap.Milestones)
					}
					want, _ := ccNIDigest(t, f, f.c, `[]`, 1)
					if snap.Digest != hex.EncodeToString(want) || snap.Digest != ccNIGoldenEmpty {
						t.Fatalf("cc.candidate_digest = %s, want %x (golden %s)", snap.Digest, want, ccNIGoldenEmpty)
					}
					f.decide(t, cc, "approve")
					ccNIRunningWrite(t, f, ccNIRunningFromCC(ccNIStoredRow(t, f, 1)), "[]")
				})
			}
		}
	}
}

func TestCrossCheckNonIssueMilestonesIssueControlLiveDB(t *testing.T) {
	for _, tc := range invalidMilestoneLists {
		t.Run("issue malformed "+tc.name, func(t *testing.T) {
			f := ccAutomaticFixture(t, 2)
			c := f.c
			c.Milestones = json.RawMessage(tc.raw)
			if _, err := f.svc.SubmitPlanCrossCheck(f.env.ctx, f.w, f.lead, 1, c, 1); !errors.Is(err, ErrCrossCheckRefused) {
				t.Fatalf("issue lead admitted malformed milestones: %v", err)
			}
			if rows, children := f.counts(t); rows != 0 || children != 0 {
				t.Fatalf("refused submit created rows=%d children=%d", rows, children)
			}
		})
	}
	t.Run("issue valid list keeps its identity", func(t *testing.T) {
		f := ccAutomaticFixture(t, 2)
		c := f.c
		c.Milestones = json.RawMessage(`[{"id":"m1","title":"First"}]`)
		cc, err := f.svc.SubmitPlanCrossCheck(f.env.ctx, f.w, f.lead, 1, c, 1)
		if err != nil {
			t.Fatal(err)
		}
		snap := ccAdmissionSnapshot(t, f, 1)
		if snap.Digest != ccNIGoldenFirst {
			t.Fatalf("issue digest = %s, want golden %s", snap.Digest, ccNIGoldenFirst)
		}
		var got []Milestone
		if err := json.Unmarshal(cc.Milestones, &got); err != nil || len(got) != 1 || got[0] != (Milestone{ID: "m1", Title: "First"}) {
			t.Fatalf("stored list %s err=%v", cc.Milestones, err)
		}
		f.decide(t, cc, "approve")
		ccNIRunningWrite(t, f, ccNIRunningFromCC(ccNIStoredRow(t, f, 1)), `[{"id":"m1","title":"First"}]`)
	})
}

func TestCrossCheckNonIssueMilestonesEmptyListDigestLiveDB(t *testing.T) {
	for _, kind := range []string{"issue", "mr_rework", "task"} {
		t.Run(kind, func(t *testing.T) {
			f := ccNIFixture(t, kind, "claude", false)
			c := f.c
			c.Milestones = json.RawMessage(`[]`)
			if _, err := f.svc.SubmitPlanCrossCheck(f.env.ctx, f.w, f.lead, 1, c, 1); err != nil {
				t.Fatal(err)
			}
			snap := ccAdmissionSnapshot(t, f, 1)
			if snap.Milestones != "[]" || snap.Digest != ccNIGoldenEmpty {
				t.Fatalf("milestones=%s digest=%s, want [] / %s", snap.Milestones, snap.Digest, ccNIGoldenEmpty)
			}
		})
	}
}

// ccNISeedLegacyAttempt submits f.c once (a real row and checker child) and rewrites the row to
// what the pre-change path stored for the received milestones: the twice-normalised list and the
// digest of that candidate.
func ccNISeedLegacyAttempt(t *testing.T, f gatedFixture, c PlanCrossCheckCandidate, raw string) (store.CrossCheck, []byte, []byte) {
	t.Helper()
	cc, err := f.svc.SubmitPlanCrossCheck(f.env.ctx, f.w, f.lead, 1, f.c, 1)
	if err != nil {
		t.Fatal(err)
	}
	digest, ms := ccNIDigest(t, f, c, raw, 2)
	f.env.exec(`UPDATE cross_checks SET milestones=$2::jsonb, candidate_digest=$3 WHERE id=$1`, cc.ID, string(ms), digest)
	return cc, ms, digest
}

func TestCrossCheckNonIssueMilestonesSeededLegacyAttemptLiveDB(t *testing.T) {
	const raw = `[{"id":"m1","title":"  Title  "}]`
	for _, kind := range ccNINonIssueKinds {
		for _, policy := range []bool{false, true} {
			name := kind + "/default caps"
			if policy {
				name = kind + "/policy-applied caps"
			}
			t.Run(name, func(t *testing.T) {
				f := ccNIFixture(t, kind, "claude", false)
				if policy {
					f.env.exec(`UPDATE runs SET required_capabilities='{docker}', ignored_inferred_capabilities='{jvm}' WHERE id=$1`, f.lead)
					f.c.RequiredCapabilities = []string{"jvm", "docker"}
				}
				c := f.c
				c.Milestones = json.RawMessage(raw)
				cc, ms, digest := ccNISeedLegacyAttempt(t, f, c, raw)
				if string(ms) == raw {
					t.Fatalf("fixture does not exercise normalisation: %s", ms)
				}
				if policy {
					if got := mustRun(t, f.env, f.lead); len(got.IgnoredInferredCapabilities) != 1 {
						t.Fatal("policy variant not applied")
					}
					applied := applyInferredCapabilityPolicy(mustRun(t, f.env, f.lead), sortedSet(c.RequiredCapabilities))
					if len(applied) != 1 || applied[0] != "docker" {
						t.Fatalf("policy variant is inert: applied caps %v", applied)
					}
				}
				before := ccAdmissionSnapshot(t, f, 1)

				once, err := NormalizePlanCrossCheckCandidate(cloneForNI(c))
				if err != nil {
					t.Fatal(err)
				}
				// The raw received candidate, then the handler-shaped (already normalised once) one.
				for name, replay := range map[string]PlanCrossCheckCandidate{"raw": c, "handler-normalised": once} {
					got, err := f.svc.SubmitPlanCrossCheck(f.env.ctx, f.w, f.lead, 1, replay, 1)
					if err != nil {
						t.Fatalf("%s replay: %v", name, err)
					}
					if got.ID != cc.ID || got.CheckerRunID != cc.CheckerRunID {
						t.Fatalf("%s replay returned a different row: %v vs %v", name, got.ID, cc.ID)
					}
					if rows, children := f.counts(t); rows != 1 || children != 1 {
						t.Fatalf("%s replay created rows=%d children=%d", name, rows, children)
					}
					if after := ccAdmissionSnapshot(t, f, 1); after != before {
						t.Fatalf("%s replay changed the row: %+v -> %+v", name, before, after)
					}
				}

				f.decide(t, cc, "approve")
				stored := ccNIStoredRow(t, f, 1)
				if hex.EncodeToString(stored.CandidateDigest) != hex.EncodeToString(digest) {
					t.Fatal("stored digest drifted")
				}
				ccNIRunningWrite(t, f, ccNIRunningFromCC(stored), `[{"id":"m1","title":"Title"}]`)
			})
		}
	}
}

// A legacy row (valid non-empty list, keyed on the received milestones) written at lead
// generation 1 must NOT satisfy a replay at generation 2: the generation check is part of the
// match, so the legacy digest alone never admits a cross-generation replay.
func TestCrossCheckNonIssueMilestonesLegacyCrossGenerationLiveDB(t *testing.T) {
	const raw = `[{"id":"m1","title":"  Title  "}]`
	for _, kind := range ccNINonIssueKinds {
		t.Run(kind, func(t *testing.T) {
			f := ccNIFixture(t, kind, "claude", false)
			c := f.c
			c.Milestones = json.RawMessage(raw)
			ccNISeedLegacyAttempt(t, f, c, raw)
			before := ccAdmissionSnapshot(t, f, 1)
			f.setGeneration(2)
			_, err := f.svc.SubmitPlanCrossCheck(f.env.ctx, f.w, f.lead, 2, c, 1)
			if !errors.Is(err, ErrCrossCheckInterrupted) {
				t.Fatalf("cross-generation legacy replay: want ErrCrossCheckInterrupted, got %v", err)
			}
			if rows, children := f.counts(t); rows != 1 || children != 1 {
				t.Fatalf("rows=%d children=%d, want 1/1", rows, children)
			}
			if after := ccAdmissionSnapshot(t, f, 1); after != before {
				t.Fatalf("row changed: %+v -> %+v", before, after)
			}
		})
	}
}

func cloneForNI(c PlanCrossCheckCandidate) PlanCrossCheckCandidate {
	c.RequiredCapabilities = append([]string(nil), c.RequiredCapabilities...)
	c.RequiredTools = append([]string(nil), c.RequiredTools...)
	return c
}

func TestCrossCheckNonIssueMilestonesMixedRollLiveDB(t *testing.T) {
	for _, kind := range ccNINonIssueKinds {
		t.Run(kind, func(t *testing.T) {
			f := ccNIFixture(t, kind, "claude", false)
			// An old worker sends the milestones it planned with.
			c := f.c
			c.Milestones = json.RawMessage(`[{"id":"m1","title":"Old worker"},{"id":"m2","title":"Plan"}]`)
			cc, err := f.svc.SubmitPlanCrossCheck(f.env.ctx, f.w, f.lead, 1, c, 1)
			if err != nil {
				t.Fatalf("old-worker-shaped submit refused: %v", err)
			}
			if string(cc.Milestones) != "[]" {
				t.Fatalf("server response milestones = %s, want []", cc.Milestones)
			}
			if snap := ccAdmissionSnapshot(t, f, 1); snap.Milestones != "[]" {
				t.Fatalf("stored cc.milestones = %s, want []", snap.Milestones)
			}
			f.decide(t, cc, "approve")
			// The old worker reports the candidate it was handed back, not the one it sent.
			ccNIRunningWrite(t, f, ccNIRunningFromCC(cc), "[]")
		})
	}
}

func TestCrossCheckNonIssueMilestonesSeededMalformedAttemptLiveDB(t *testing.T) {
	const raw = `[{"id":"","title":""}]`
	canonical := []byte(raw)
	seed := func(t *testing.T, f gatedFixture, verdict string) store.CrossCheck {
		t.Helper()
		cc, err := f.svc.SubmitPlanCrossCheck(f.env.ctx, f.w, f.lead, 1, f.c, 1)
		if err != nil {
			t.Fatal(err)
		}
		if verdict != "" {
			f.decide(t, cc, verdict)
		}
		d := ccPreFixDigest(t, f, cc, canonical)
		f.env.exec(`UPDATE cross_checks SET milestones=$2::jsonb, candidate_digest=$3 WHERE id=$1`, cc.ID, string(canonical), d)
		cc.CandidateDigest = d
		return cc
	}
	for _, kind := range ccNINonIssueKinds {
		for _, bump := range []bool{false, true} {
			name := kind + "/same generation"
			if bump {
				name = kind + "/lead generation bumped"
			}
			t.Run(name, func(t *testing.T) {
				f := ccNIFixture(t, kind, "claude", false)
				seed(t, f, "revise")
				before := ccAdmissionSnapshot(t, f, 1)
				generation := int64(1)
				if bump {
					f.setGeneration(2)
					generation = 2
				}
				c := f.c
				c.Milestones = json.RawMessage(raw)
				_, err := f.svc.SubmitPlanCrossCheck(f.env.ctx, f.w, f.lead, generation, c, 1)
				if !errors.Is(err, ErrCrossCheckRefused) || errors.Is(err, ErrCrossCheckInterrupted) {
					t.Fatalf("malformed replay: want ErrCrossCheckRefused and not Interrupted, got %v", err)
				}
				if rows, children := f.counts(t); rows != 1 || children != 1 {
					t.Fatalf("rows=%d children=%d", rows, children)
				}
				if after := ccAdmissionSnapshot(t, f, 1); after != before {
					t.Fatalf("row changed: %+v -> %+v", before, after)
				}
			})
		}
		t.Run(kind+"/running write refuses", func(t *testing.T) {
			f := ccNIFixture(t, kind, "claude", false)
			cc := seed(t, f, "approve")
			_, applied, err := f.svc.SetState(f.env.ctx, f.w, f.lead, ccRunningRequest(cc, "plan", []Milestone{{ID: "", Title: ""}}))
			if applied || !errors.Is(err, ErrInvalidState) {
				t.Fatalf("running write with malformed milestones: applied=%v err=%v", applied, err)
			}
			if run := mustRun(t, f.env, f.lead); run.PlanMd.Valid || len(run.MilestonesFrozen) != 0 && string(run.MilestonesFrozen) != "null" {
				t.Fatalf("refused write persisted: plan=%v milestones=%s", run.PlanMd, run.MilestonesFrozen)
			}
		})
	}
}

func TestCrossCheckNonIssueMilestonesGatedLiveDB(t *testing.T) {
	// runs_plan_cross_check_mode_check admits gated mode only on issue, prompt and ci_fix leads.
	for _, kind := range []string{"prompt", "ci_fix"} {
		t.Run(kind, func(t *testing.T) {
			f := ccNIFixture(t, kind, "claude", true)
			c := f.c
			c.Milestones = json.RawMessage(`[{"id":"m1","title":"Stray"}]`)
			cc, err := f.svc.SubmitPlanCrossCheck(f.env.ctx, f.w, f.lead, 1, c, 1)
			if err != nil {
				t.Fatalf("gated non-issue submit refused: %v", err)
			}
			if snap := ccAdmissionSnapshot(t, f, 1); snap.Milestones != "[]" {
				t.Fatalf("stored cc.milestones = %s, want []", snap.Milestones)
			}
			f.decide(t, cc, "approve")
			stored := ccNIStoredRow(t, f, 1)
			plan, gen, size := stored.PlanMd.String, int64(1), stored.SizeClass.String
			caps, tools, ms := []string{}, []string{}, []Milestone{}
			pid := uuid.New()
			req := StateRequest{State: "awaiting_approval", ClaimGeneration: &gen, PlanMd: &plan, Milestones: &ms,
				RequiredCapabilities: &caps, RequiredTools: &tools, SizeClass: &size, PresentationID: &pid, CandidateDigest: hex.EncodeToString(stored.CandidateDigest)}
			if _, applied, err := f.svc.SetState(f.env.ctx, f.w, f.lead, req); err != nil || !applied {
				t.Fatalf("gate report: applied=%v err=%v", applied, err)
			}
			run := mustRun(t, f.env, f.lead)
			if run.Status != "awaiting_approval" || run.AutoApprove {
				t.Fatalf("status=%s auto=%v", run.Status, run.AutoApprove)
			}
			if run.PlanCrossCheckGateReason.Valid {
				t.Fatalf("gate reason %q, want NULL (passed)", run.PlanCrossCheckGateReason.String)
			}
		})
	}
}
