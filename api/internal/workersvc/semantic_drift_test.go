package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
)

const driftSHA = "0123456789abcdef0123456789abcdef01234567"

func driftHash(s string) string { return semanticHash([]byte(s)) }

func driftFrozen() []apitypes.SemanticSourceRef {
	return []apitypes.SemanticSourceRef{
		{ID: "issue:7", Kind: "issue", Hash: driftHash("- [ ] one\n- [ ] two\n"), Completeness: "complete"},
		{ID: "prd:prds/7-x.md", Kind: "prd", Hash: driftHash("prd body"), Completeness: "complete"},
		{ID: "prd:prds/8-y.md", Kind: "prd", Hash: driftHash(""), Completeness: "unknown"},
	}
}

func presentation(prds ...apitypes.SemanticObservedSource) apitypes.SemanticObservationRequest {
	gate := int64(3)
	return apitypes.SemanticObservationRequest{Phase: "presentation", CandidateDigest: driftHash("candidate"), GateRevision: &gate, PRDSources: prds}
}

func TestSemanticObservationRequestValidation(t *testing.T) {
	ok := apitypes.SemanticObservedSource{SourceID: "prd:prds/7-x.md", ObservedHash: driftHash("prd body"), Ref: driftSHA, Outcome: "unchanged"}
	unknown := apitypes.SemanticObservedSource{SourceID: "prd:prds/8-y.md", Ref: driftSHA, Outcome: "unknown", Reason: "missing"}
	mutate := func(f func(*apitypes.SemanticObservedSource)) apitypes.SemanticObservedSource {
		c := ok
		f(&c)
		return c
	}
	publication := func(sha string, prds ...apitypes.SemanticObservedSource) apitypes.SemanticObservationRequest {
		return apitypes.SemanticObservationRequest{Phase: "publication", PushedSHA: sha, PRDSources: prds}
	}
	var nine []apitypes.SemanticObservedSource
	for i := range 9 {
		nine = append(nine, apitypes.SemanticObservedSource{SourceID: "prd:prds/" + string(rune('a'+i)) + ".md", Ref: driftSHA, Outcome: "unknown", Reason: "missing"})
	}
	for _, tc := range []struct {
		name string
		req  apitypes.SemanticObservationRequest
		ok   bool
	}{
		{"presentation", presentation(ok, unknown), true},
		{"publication", publication(driftSHA, ok, unknown), true},
		{"no prds", presentation(), true},
		{"unknown carrying a hash", presentation(mutate(func(s *apitypes.SemanticObservedSource) { s.Outcome, s.Reason = "unknown", "missing" })), false},
		{"unknown without a reason", presentation(mutate(func(s *apitypes.SemanticObservedSource) { s.Outcome, s.ObservedHash = "unknown", "" })), false},
		{"unknown with an unlisted reason", presentation(mutate(func(s *apitypes.SemanticObservedSource) {
			s.Outcome, s.ObservedHash, s.Reason = "unknown", "", "withheld"
		})), false},
		{"unchanged without a hash", presentation(mutate(func(s *apitypes.SemanticObservedSource) { s.ObservedHash = "" })), false},
		{"unchanged with a reason", presentation(mutate(func(s *apitypes.SemanticObservedSource) { s.Reason = "missing" })), false},
		{"uppercase hash", presentation(mutate(func(s *apitypes.SemanticObservedSource) { s.ObservedHash = strings.ToUpper(s.ObservedHash) })), false},
		{"unknown outcome word", presentation(mutate(func(s *apitypes.SemanticObservedSource) { s.Outcome = "same" })), false},
		{"duplicate source", presentation(ok, ok), false},
		{"nine prd sources", presentation(nine...), false},
		{"short ref", presentation(mutate(func(s *apitypes.SemanticObservedSource) { s.Ref = "abc123" })), false},
		{"publication needs the pushed sha", publication("", ok), false},
		{"publication uppercase sha", publication(strings.ToUpper(driftSHA), ok), false},
		{"publication ref differs from pushed sha", publication(driftSHA, mutate(func(s *apitypes.SemanticObservedSource) { s.Ref = strings.Repeat("f", 40) })), false},
		{"presentation without digest", apitypes.SemanticObservationRequest{Phase: "presentation"}, false},
		{"unknown phase", apitypes.SemanticObservationRequest{Phase: "later"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSemanticObservationRequest(tc.req)
			if tc.ok != (err == nil) {
				t.Fatalf("err=%v, want ok=%v", err, tc.ok)
			}
			if err != nil && !errors.Is(err, ErrSemanticObservationInvalid) {
				t.Fatalf("a validation refusal must be ErrSemanticObservationInvalid: %v", err)
			}
		})
	}
}

func TestDriftPRDSourcesRecomputesOutcome(t *testing.T) {
	frozen := driftFrozen()
	same := apitypes.SemanticObservedSource{SourceID: "prd:prds/7-x.md", ObservedHash: driftHash("prd body"), Ref: driftSHA, Outcome: "unchanged"}
	missing := apitypes.SemanticObservedSource{SourceID: "prd:prds/8-y.md", Ref: driftSHA, Outcome: "unknown", Reason: "missing"}
	t.Run("accepts matching outcomes and keeps hashes", func(t *testing.T) {
		out, err := driftPRDSources(frozen, []apitypes.SemanticObservedSource{same, missing})
		if err != nil || len(out) != 2 || out[0].FrozenHash != driftHash("prd body") || out[0].Outcome != "unchanged" || out[1].Outcome != "unknown" {
			t.Fatalf("out=%+v err=%v", out, err)
		}
	})
	t.Run("changed with a different hash", func(t *testing.T) {
		c := same
		c.Outcome, c.ObservedHash = "changed", driftHash("edited")
		if out, err := driftPRDSources(frozen, []apitypes.SemanticObservedSource{c, missing}); err != nil || out[0].Outcome != "changed" {
			t.Fatalf("out=%+v err=%v", out, err)
		}
	})
	for name, reported := range map[string][]apitypes.SemanticObservedSource{
		"foreign source id":                        {same, {SourceID: "prd:prds/99-z.md", Ref: driftSHA, Outcome: "unknown", Reason: "missing"}},
		"issue id is not a prd source":             {same, {SourceID: "issue:7", Ref: driftSHA, Outcome: "unknown", Reason: "missing"}},
		"unchanged contradicting the hashes":       {{SourceID: "prd:prds/7-x.md", ObservedHash: driftHash("edited"), Ref: driftSHA, Outcome: "unchanged"}, missing},
		"changed contradicting the hashes":         {{SourceID: "prd:prds/7-x.md", ObservedHash: driftHash("prd body"), Ref: driftSHA, Outcome: "changed"}, missing},
		"omitted frozen source":                    {same},
		"baseline-less source cannot be unchanged": {same, {SourceID: "prd:prds/8-y.md", ObservedHash: driftHash(""), Ref: driftSHA, Outcome: "unchanged"}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := driftPRDSources(frozen, reported); !errors.Is(err, ErrSemanticObservationInvalid) {
				t.Fatalf("err=%v, want ErrSemanticObservationInvalid", err)
			}
		})
	}
}

func TestDriftIssueSource(t *testing.T) {
	const frozenText = "- [ ] one\n- [ ] two\n"
	ref := driftFrozen()[0]
	for _, tc := range []struct {
		name         string
		frozen       string
		cur          currentIssue
		outcome      string
		reason       string
		checkboxOnly bool
		hashed       bool
	}{
		{"unchanged", frozenText, currentIssue{Text: frozenText}, "unchanged", "", false, true},
		{"changed", frozenText, currentIssue{Text: frozenText + "- [ ] three\n"}, "changed", "", false, true},
		{"checkbox flips only", frozenText, currentIssue{Text: "- [x] one\n- [X] two\n"}, "changed", "", true, true},
		{"checkbox flip plus an edit is not checkbox only", frozenText, currentIssue{Text: "- [x] one\n- [ ] 2\n"}, "changed", "", false, true},
		{"brackets inside prose are not a checkbox", "see [ ] here", currentIssue{Text: "see [x] here"}, "changed", "", false, true},
		{"forge error", frozenText, currentIssue{Reason: "unreadable"}, "unknown", "unreadable", false, false},
		{"forge timeout", frozenText, currentIssue{Reason: "timeout"}, "unknown", "timeout", false, false},
		{"oversize", frozenText, currentIssue{Reason: "truncated"}, "unknown", "truncated", false, false},
		{"denied", frozenText, currentIssue{Reason: "denied"}, "unknown", "denied", false, false},
		{"snapshot is not the frozen bytes", "something else", currentIssue{Text: frozenText}, "unknown", "invalid", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := ref
			if tc.name == "brackets inside prose are not a checkbox" {
				r.Hash = driftHash(tc.frozen)
			}
			got := driftIssueSource(r, tc.frozen, tc.cur)
			if got.Outcome != tc.outcome || got.Reason != tc.reason || got.CheckboxOnly != tc.checkboxOnly || (got.ObservedHash != "") != tc.hashed || got.FrozenHash != r.Hash || got.SourceID != "issue:7" {
				t.Fatalf("got %+v", got)
			}
			if tc.hashed && got.ObservedHash != driftHash(tc.cur.Text) {
				t.Fatalf("observed hash is not sha256 of the raw current bytes: %+v", got)
			}
		})
	}
}

type driftFakeForge struct {
	forgetest.BaseFake
	issue forge.Issue
	err   error
	block bool
}

func (f *driftFakeForge) GetIssue(ctx context.Context, _, _ int64) (forge.Issue, error) {
	if f.block {
		<-ctx.Done()
		return forge.Issue{}, ctx.Err()
	}
	return f.issue, f.err
}

func TestReadCurrentSemanticIssue(t *testing.T) {
	ctx := context.Background()
	if got := readCurrentSemanticIssue(ctx, &driftFakeForge{issue: forge.Issue{Description: "text"}}, 1, 7); got.Reason != "" || got.Text != "text" {
		t.Fatalf("read: %+v", got)
	}
	if got := readCurrentSemanticIssue(ctx, &driftFakeForge{err: errors.New("boom https://forge/projects/1")}, 1, 7); got.Reason != "unreadable" || got.Text != "" {
		t.Fatalf("forge error must become unknown/unreadable, got %+v", got)
	}
	atLimit := strings.Repeat("a", 64*1024)
	if got := readCurrentSemanticIssue(ctx, &driftFakeForge{issue: forge.Issue{Description: atLimit}}, 1, 7); got.Reason != "" {
		t.Fatalf("64 KiB is within the frozen-source limit: %+v", got)
	}
	if got := readCurrentSemanticIssue(ctx, &driftFakeForge{issue: forge.Issue{Description: atLimit + "a"}}, 1, 7); got.Reason != "truncated" || got.Text != "" {
		t.Fatalf("oversize must be unknown/truncated: %+v", got)
	}
	expired, cancel := context.WithTimeout(ctx, time.Millisecond)
	defer cancel()
	if got := readCurrentSemanticIssue(expired, &driftFakeForge{block: true}, 1, 7); got.Reason != "timeout" {
		t.Fatalf("deadline must be unknown/timeout: %+v", got)
	}
}

func TestDriftLinks(t *testing.T) {
	frozen := driftFrozen()
	added, removed, truncated := driftLinks(frozen, "see prds/7-x.md and prds/9-new.md")
	if strings.Join(added, ",") != "prds/9-new.md" || strings.Join(removed, ",") != "prds/8-y.md" || truncated {
		t.Fatalf("added=%q removed=%q truncated=%v", added, removed, truncated)
	}
	added, removed, _ = driftLinks(frozen, "prds/7-x.md prds/8-y.md")
	if len(added) != 0 || len(removed) != 0 || added == nil || removed == nil {
		t.Fatalf("an unchanged link set must report empty non-nil lists: %q %q", added, removed)
	}
	text := ""
	for i := range 10 {
		text += " prds/" + string(rune('a'+i)) + ".md"
	}
	if added, _, truncated = driftLinks(nil, text); len(added) != 8 || !truncated {
		t.Fatalf("only the compiler's eight-path window is compared and flagged: %q %v", added, truncated)
	}
}

func TestSemanticDriftDTO(t *testing.T) {
	obs := func(issue, prd string) apitypes.SemanticDriftObservation {
		return apitypes.SemanticDriftObservation{Phase: "publication", Issue: apitypes.SemanticDriftSource{Outcome: issue}, PRDs: []apitypes.SemanticDriftSource{{Outcome: prd}}}
	}
	marshal := func(dropped int64, items ...apitypes.SemanticDriftObservation) []byte {
		raw, err := json.Marshal(storedSemanticObservations{Dropped: dropped, Items: items})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	if dto, err := semanticDriftDTO(marshal(0)); err != nil || dto != nil {
		t.Fatalf("no observations must project to nothing: %+v %v", dto, err)
	}
	for _, tc := range []struct {
		name          string
		raw           []byte
		contradiction bool
	}{
		{"all unchanged", marshal(0, obs("unchanged", "unchanged")), false},
		{"issue changed", marshal(0, obs("changed", "unchanged")), true},
		{"prd unknown", marshal(0, obs("unchanged", "unknown")), true},
		{"an older contradiction is not silenced by a later clean one", marshal(0, obs("changed", "unchanged"), obs("unchanged", "unchanged")), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dto, err := semanticDriftDTO(tc.raw)
			if err != nil || dto == nil || dto.Contradiction != tc.contradiction {
				t.Fatalf("dto=%+v err=%v", dto, err)
			}
		})
	}
	if dto, _ := semanticDriftDTO(marshal(5)); dto == nil || dto.Dropped != 5 || dto.Observations == nil {
		t.Fatalf("a dropped count alone is still reported with a non-nil list: %+v", dto)
	}
}

func TestDriftRecordCarriesNoSourceText(t *testing.T) {
	const secret = "TOP-SECRET-ISSUE-BODY"
	frozen := "frozen " + secret
	ref := apitypes.SemanticSourceRef{ID: "issue:7", Kind: "issue", Hash: driftHash(frozen), Completeness: "complete"}
	rec := apitypes.SemanticDriftObservation{Phase: "publication", PushedSHA: driftSHA, ObservedAt: time.Unix(0, 0).UTC(),
		Issue: driftIssueSource(ref, frozen, currentIssue{Text: "current " + secret}), PRDs: []apitypes.SemanticDriftSource{}, LinksAdded: []string{}, LinksRemoved: []string{}}
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) || strings.Contains(string(raw), "current") || strings.Contains(string(raw), "frozen ") {
		t.Fatalf("record leaks issue text: %s", raw)
	}
}
