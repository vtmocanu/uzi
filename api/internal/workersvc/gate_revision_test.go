package workersvc

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

func mustDigest(t *testing.T, p gatePresentedPayload) []byte {
	t.Helper()
	_, sum, err := p.digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	return sum
}

func basePayload() gatePresentedPayload {
	return gatePresentedPayload{
		PlanMd:               strp("# Plan"),
		Milestones:           json.RawMessage(`[{"id":"M1","title":"First"}]`),
		RequiredCapabilities: []string{"docker"},
		RequiredTools:        []string{"go"},
		SizeClass:            strp("m"),
	}
}

// TestGatePayloadCanonicalForm pins that the digest is over a canonical form: set order,
// duplicates, milestone whitespace and key order, and JSONB's re-formatting of a stored snapshot
// never change it, while an absent and a JSON-null milestone list compare equal.
func TestGatePayloadCanonicalForm(t *testing.T) {
	a := basePayload()
	b := gatePresentedPayload{
		PlanMd:               strp("# Plan"),
		Milestones:           json.RawMessage("[ {\"title\": \"First\",  \"id\": \"M1\"} ]"),
		RequiredCapabilities: []string{"docker", "docker"},
		RequiredTools:        []string{"go"},
		SizeClass:            strp("m"),
	}
	if !bytes.Equal(mustDigest(t, a), mustDigest(t, b)) {
		t.Fatal("equivalent payloads digest differently")
	}
	multi := func(caps ...string) gatePresentedPayload { p := basePayload(); p.RequiredCapabilities = caps; return p }
	if !bytes.Equal(mustDigest(t, multi("jvm", "docker")), mustDigest(t, multi("docker", "jvm"))) {
		t.Fatal("capability order changed the digest")
	}
	// A snapshot round-tripped through the stored form (decode) digests the same.
	canonical, sum, err := a.digest()
	if err != nil {
		t.Fatal(err)
	}
	decoded, ok, err := decodeGatePresentedPayload(canonical)
	if err != nil || !ok {
		t.Fatalf("decode canonical: %v %v", ok, err)
	}
	if !bytes.Equal(mustDigest(t, decoded), sum) {
		t.Fatal("a decoded snapshot digests differently from the one it was stored from")
	}
	nullMs, absentMs := basePayload(), basePayload()
	nullMs.Milestones, absentMs.Milestones = json.RawMessage("null"), nil
	if !bytes.Equal(mustDigest(t, nullMs), mustDigest(t, absentMs)) {
		t.Fatal("null and absent milestones digest differently")
	}
	if _, ok, err := decodeGatePresentedPayload(nil); ok || err != nil {
		t.Fatalf("empty snapshot = %v %v, want none", ok, err)
	}
}

// TestGatePayloadDigestCoversApprovalRelevantFields pins that each approval-relevant field moves
// the digest, and that the payload has no plan_changed_files at all (PRD #1795 A1).
func TestGatePayloadDigestCoversApprovalRelevantFields(t *testing.T) {
	base := mustDigest(t, basePayload())
	for name, mutate := range map[string]func(*gatePresentedPayload){
		"plan_md":               func(p *gatePresentedPayload) { p.PlanMd = strp("# Other") },
		"plan_md null":          func(p *gatePresentedPayload) { p.PlanMd = nil },
		"milestones":            func(p *gatePresentedPayload) { p.Milestones = json.RawMessage(`[{"id":"M2","title":"First"}]`) },
		"milestone title":       func(p *gatePresentedPayload) { p.Milestones = json.RawMessage(`[{"id":"M1","title":"Other"}]`) },
		"required_capabilities": func(p *gatePresentedPayload) { p.RequiredCapabilities = []string{"docker", "jvm"} },
		"required_tools":        func(p *gatePresentedPayload) { p.RequiredTools = nil },
		"size_class":            func(p *gatePresentedPayload) { p.SizeClass = strp("l") },
	} {
		p := basePayload()
		mutate(&p)
		if bytes.Equal(mustDigest(t, p), base) {
			t.Errorf("changing %s did not move the digest", name)
		}
	}
	canonical, _, err := basePayload().digest()
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]any
	if err := json.Unmarshal(canonical, &keys); err != nil {
		t.Fatal(err)
	}
	want := []string{"milestones", "plan_md", "required_capabilities", "required_tools", "size_class"}
	var got []string
	for k := range keys {
		got = append(got, k)
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("canonical keys = %v, want exactly %v (no plan_changed_files)", got, want)
	}
}

// TestEffectiveGatePayload pins decision 3's field rules: report fields win, absent-safe fields
// fall back to the base, required_capabilities union-merges, plan_md is always the report's.
func TestEffectiveGatePayload(t *testing.T) {
	base := basePayload()

	// A report carrying nothing but the plan keeps every absent-safe field from the base.
	got := effectiveGatePayload(base, gateReportFields{planMd: pgtype.Text{String: "# Plan", Valid: true}})
	if !bytes.Equal(mustDigest(t, got), mustDigest(t, base)) {
		t.Fatalf("plan-only report = %+v, want the base unchanged", got)
	}

	// A subset of the presented capabilities (a resent narrower inference) unions back to the base.
	got = effectiveGatePayload(base, gateReportFields{planMd: pgtype.Text{String: "# Plan", Valid: true}, caps: []string{}})
	if !slices.Equal(got.RequiredCapabilities, []string{"docker"}) {
		t.Fatalf("subset caps = %v, want the union [docker]", got.RequiredCapabilities)
	}
	got = effectiveGatePayload(base, gateReportFields{planMd: pgtype.Text{String: "# Plan", Valid: true}, caps: []string{"jvm"}})
	if !slices.Equal(got.RequiredCapabilities, []string{"docker", "jvm"}) {
		t.Fatalf("added cap = %v, want [docker jvm]", got.RequiredCapabilities)
	}

	// Reported tools and size replace; a reported milestone list replaces, an absent one keeps.
	got = effectiveGatePayload(base, gateReportFields{
		planMd:             pgtype.Text{String: "# Plan", Valid: true},
		tools:              []string{"node"},
		sizeClass:          pgtype.Text{String: "s", Valid: true},
		milestones:         []byte(`[{"id":"M9","title":"Nine"}]`),
		milestonesReported: true,
	})
	if !slices.Equal(got.RequiredTools, []string{"node"}) || *got.SizeClass != "s" || string(got.Milestones) != `[{"id":"M9","title":"Nine"}]` {
		t.Fatalf("replacing report = %+v", got)
	}
	// A reported-but-rejected milestone list (resolved to NULL) is a change, not a fallback.
	got = effectiveGatePayload(base, gateReportFields{planMd: pgtype.Text{String: "# Plan", Valid: true}, milestonesReported: true})
	if bytes.Equal(mustDigest(t, got), mustDigest(t, base)) {
		t.Fatal("a rejected milestone list compared equal to the presented one")
	}
	// plan_md is a direct assignment: an absent plan is NULL, never the base's.
	got = effectiveGatePayload(base, gateReportFields{})
	if got.PlanMd != nil {
		t.Fatalf("absent plan_md = %v, want nil", *got.PlanMd)
	}
	// The base is never mutated through the union.
	if !slices.Equal(base.RequiredCapabilities, []string{"docker"}) {
		t.Fatalf("base mutated: %v", base.RequiredCapabilities)
	}
}

// TestGatePayloadFromRow maps the row's persisted columns, treating the empty size_class as unset.
func TestGatePayloadFromRow(t *testing.T) {
	p := gatePayloadFromRow(store.Run{
		PlanMd:               pgtype.Text{String: "# Plan", Valid: true},
		MilestonesCandidate:  []byte(`[{"id":"M1","title":"First"}]`),
		RequiredCapabilities: []string{"docker"},
		RequiredTools:        []string{"go"},
		SizeClass:            "",
	})
	if p.PlanMd == nil || *p.PlanMd != "# Plan" || p.SizeClass != nil || !slices.Equal(p.RequiredTools, []string{"go"}) {
		t.Fatalf("row payload = %+v", p)
	}
	if got := retainedMilestonesCandidate(gatePresentedPayload{Milestones: json.RawMessage("null")}); got != nil {
		t.Fatalf("retained null candidate = %s, want SQL NULL", got)
	}
	if got := retainedMilestonesCandidate(p); string(got) != `[{"id":"M1","title":"First"}]` {
		t.Fatalf("retained candidate = %s", got)
	}
}

// TestGatePresentationRefusalReason pins the 409 reason tokens.
func TestGatePresentationRefusalReason(t *testing.T) {
	for err, want := range map[error]string{
		ErrGatePresentationHistorical: "gate_presentation_historical",
		ErrGatePresentationConflict:   "gate_presentation_conflict",
		ErrGateAdoptionStale:          "gate_adoption_stale",
	} {
		if got, ok := GatePresentationRefusalReason(err); !ok || got != want {
			t.Errorf("reason(%v) = %q %v, want %q", err, got, ok, want)
		}
	}
	if _, ok := GatePresentationRefusalReason(errors.New("other")); ok {
		t.Fatal("an unrelated error mapped to a refusal reason")
	}
	if _, ok := GatePresentationRefusalReason(ErrClaimGenerationRequired); ok {
		t.Fatal("claim_generation_required is a 400, not a refusal")
	}
}
