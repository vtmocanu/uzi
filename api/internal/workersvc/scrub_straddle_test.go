package workersvc

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// GHSA-2722 item 4: a recognized credential that STRADDLES a field's byte cap must be
// redacted in the value that is actually stored. Capping first truncates the token
// below secretscrub's match length (a ghp_ body needs 16+ chars), so the unmatched
// prefix is persisted and can later reach a filed forge issue.

// straddleTokenChunk repeats into the credential body at runtime, so no complete
// provider-token literal (nor a token-shaped constant) sits in this file.
const straddleTokenChunk = "Zq9x"

// straddleLead is how many bytes of the token fall inside the cap: the "ghp_" prefix
// plus 4 body chars, well under the 16-char body the scrubber needs to match.
const straddleLead = 8

// straddlingSecret returns a value whose first max bytes end inside a GitHub classic
// PAT (assembled at runtime), and the filler prefix a correct scrub-then-bound keeps.
func straddlingSecret(max int) (value, keep string) {
	token := "gh" + "p_" + strings.Repeat(straddleTokenChunk, 9)
	keep = strings.Repeat("a", max-straddleLead)
	return keep + token + " tail", keep
}

// assertStraddleRedacted fails when got carries any fragment of the straddling token,
// and requires the positive observation that the filler survived (so an empty or
// unrelated value cannot pass vacuously).
func assertStraddleRedacted(t *testing.T, field, got, keep string, max int) {
	t.Helper()
	if !strings.HasPrefix(got, keep) {
		t.Fatalf("%s: stored value lost its leading text (len %d), want the %d-byte filler kept", field, len(got), len(keep))
	}
	if strings.Contains(got, "ghp_") || strings.Contains(got, straddleTokenChunk) {
		t.Errorf("%s: stored value leaks a truncated credential: tail %q", field, got[len(keep):])
	}
	if len(got) > max {
		t.Errorf("%s: stored value is %d bytes, want at most the %d-byte cap", field, len(got), max)
	}
}

func TestCreateFindingScrubsCredentialStraddlingCap(t *testing.T) {
	run := baseFindingRun()
	f := &findingsFakeStore{run: run}
	svc := New(f, nil, Params{})
	wkr := store.Worker{ID: uuid.New(), UserID: run.UserID}

	title, titleKeep := straddlingSecret(MaxFindingTitleBytes)
	desc, descKeep := straddlingSecret(MaxIssueDescriptionBytes)
	label, labelKeep := straddlingSecret(MaxFindingLabelBytes)

	_, _, err := svc.CreateFinding(context.Background(), wkr, run.ID, CreateFindingRequest{
		Title:       title,
		Description: desc,
		Location:    "api/internal/x.go#Fn",
		Labels:      []string{label},
		Confidence:  "high",
	})
	if err != nil {
		t.Fatalf("CreateFinding: %v", err)
	}
	if f.inserted == nil {
		t.Fatal("no evidence row inserted")
	}
	assertStraddleRedacted(t, "title", f.inserted.Title, titleKeep, MaxFindingTitleBytes)
	assertStraddleRedacted(t, "description", f.inserted.DescriptionMd, descKeep, MaxIssueDescriptionBytes)

	var labels []string
	if err := json.Unmarshal(f.inserted.Labels, &labels); err != nil {
		t.Fatalf("labels not valid JSON: %v", err)
	}
	if len(labels) != 1 {
		t.Fatalf("stored labels = %q, want exactly one", labels)
	}
	assertStraddleRedacted(t, "label", labels[0], labelKeep, MaxFindingLabelBytes)
}

func TestSetIntentSummaryScrubsCredentialStraddlingCap(t *testing.T) {
	f := &summariesFakeStore{run: baseSummaryRun(), intentRows: 1}
	svc := New(f, nil, Params{})
	wkr := store.Worker{ID: uuid.New(), UserID: f.run.UserID}

	summary, keep := straddlingSecret(MaxSummaryBytes)
	if _, _, err := svc.SetIntentSummary(context.Background(), wkr, f.run.ID, summary); err != nil {
		t.Fatalf("SetIntentSummary: %v", err)
	}
	if f.intentParams == nil {
		t.Fatal("SetRunIntentSummary was not called")
	}
	assertStraddleRedacted(t, "summary_intent", f.intentParams.SummaryIntent.String, keep, MaxSummaryBytes)
}

func TestSetPlanSummaryScrubsCredentialStraddlingCap(t *testing.T) {
	f := &summariesFakeStore{run: baseSummaryRun(), planRows: 1}
	svc := New(f, nil, Params{})
	wkr := store.Worker{ID: uuid.New(), UserID: f.run.UserID}

	summary, keep := straddlingSecret(MaxSummaryBytes)
	if _, err := svc.SetPlanSummary(context.Background(), wkr, f.run.ID, summary, nil, f.run.PlanMd.String); err != nil {
		t.Fatalf("SetPlanSummary: %v", err)
	}
	if f.planParams == nil {
		t.Fatal("SetRunPlanSummary was not called")
	}
	assertStraddleRedacted(t, "summary_plan", f.planParams.SummaryPlan.String, keep, MaxSummaryBytes)
}

// TestCreateFindingInvalidUTF8DoesNotCutCredential pins the "normalize WITHOUT
// truncation" step against input that GROWS under normalization: each invalid UTF-8 byte
// becomes a 3-byte U+FFFD, so a "no-cut" bound of len(s)+1 still cuts. 15 invalid bytes
// ahead of a 40-byte token end that bound 11 bytes into the token, below the scrubber's
// match length. (JSON decoding already replaces invalid UTF-8, so this guards the helper
// rather than a live HTTP path.)
func TestCreateFindingInvalidUTF8DoesNotCutCredential(t *testing.T) {
	run := baseFindingRun()
	f := &findingsFakeStore{run: run}
	svc := New(f, nil, Params{})
	wkr := store.Worker{ID: uuid.New(), UserID: run.UserID}

	token := "gh" + "p_" + strings.Repeat(straddleTokenChunk, 9)
	_, _, err := svc.CreateFinding(context.Background(), wkr, run.ID, CreateFindingRequest{
		Title:       strings.Repeat("\xff", 15) + token,
		Description: "d",
		Location:    "api/internal/x.go#Fn",
	})
	if err != nil {
		t.Fatalf("CreateFinding: %v", err)
	}
	if f.inserted == nil {
		t.Fatal("no evidence row inserted")
	}
	got := f.inserted.Title
	if strings.Contains(got, "ghp_") || strings.Contains(got, straddleTokenChunk) {
		t.Errorf("title leaks a cut credential: %q", got)
	}
	if !strings.Contains(got, "[redacted]") {
		t.Errorf("title %q, want the credential replaced by [redacted]", got)
	}
}
