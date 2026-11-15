package apitypes

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The shared fixtures under fixtures/terminal-preservation are the contract the worker (agent/test/
// client-terminal-preservation.test.ts) is also pinned to.
const terminalFixtureDir = "../../../fixtures/terminal-preservation"

func terminalFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(terminalFixtureDir, name)) //nolint:gosec // G304: fixed repo-relative test fixture path
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

func strictTerminalDecode(b []byte, into any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	return dec.Decode(into)
}

var terminalFixtureReasons = []string{
	TerminalReasonSendSuppressed, TerminalReasonCreateRefused, TerminalReasonUpdateRefused, TerminalReasonUnsupported, TerminalReasonHoldMissing,
	TerminalReasonIdentityChanged, TerminalReasonExcludedOrigin, TerminalReasonManifestInvalid, TerminalReasonManifestMismatch,
	TerminalReasonGenerationBound, TerminalReasonPriorTipMismatch, TerminalReasonPreservationRetired, TerminalReasonPublicationInFlight,
	TerminalReasonForgeUnavailable, TerminalReasonPackInvalid, TerminalReasonPackTooLarge, TerminalReasonTipMissing,
	TerminalReasonCIConfigGuard, TerminalReasonNotDescendant, TerminalReasonRootUnreachable, TerminalReasonTreeMismatch,
	TerminalReasonWalkTooLarge, TerminalReasonForeignTip, TerminalReasonAuthorityChanged,
}

var terminalFixtureStates = []string{"published", "invoked", "publish_unknown", "refused", "recorded", "prepared"}

var terminalSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// terminalResponseDispositions is the stored run_terminal_contributions.disposition vocabulary
// (migration 00449); the response echoes it, never the request's "preserve". The server side is
// pinned to the same values in workersvc's TestTerminalPreservationFixtureMatchesServerResult.
var terminalResponseDispositions = []string{"preserved", "closed_issue_abandoned", "closed_pr_abandoned"}

// validateTerminalResponse is the test's stand-in for a worker-side semantic check: the disposition
// is a stored one and pairs with the state (recorded <=> an abandonment), and a reason appears only on
// a refused state.
func validateTerminalResponse(r TerminalPreservationResponse) bool {
	return slices.Contains(terminalFixtureStates, r.State) && terminalSHA.MatchString(r.Tip) &&
		slices.Contains(terminalResponseDispositions, r.Disposition) &&
		(r.State == "recorded") == (r.Disposition != "preserved") &&
		(r.Reason == "" || (r.State == "refused" && slices.Contains(terminalFixtureReasons, r.Reason)))
}

func validateTerminalRequest(r TerminalPreservationRequest) bool {
	if !terminalSHA.MatchString(r.Tip) || r.Disposition != TerminalDispositionPreserve {
		return false
	}
	for _, a := range r.Manifest.Abandoned {
		if a.Class != TerminalAbandonedIgnored && a.Class != TerminalAbandonedColdDirty {
			return false
		}
	}
	return true
}

func TestTerminalFixtureRequestAndResponsesDecodeStrictly(t *testing.T) {
	var req TerminalPreservationRequest
	if err := strictTerminalDecode(terminalFixture(t, "request.json"), &req); err != nil {
		t.Fatalf("request.json: %v", err)
	}
	if !validateTerminalRequest(req) || len(req.Manifest.Roots) != 2 || len(req.Manifest.Abandoned) != 1 {
		t.Fatalf("request.json is not the documented valid request: %+v", req)
	}
	for _, state := range append(slices.Clone(terminalFixtureStates), "recorded-pr") {
		var resp TerminalPreservationResponse
		if err := strictTerminalDecode(terminalFixture(t, "response."+state+".json"), &resp); err != nil {
			t.Fatalf("response.%s.json: %v", state, err)
		}
		if strings.TrimSuffix(state, "-pr") != resp.State || !validateTerminalResponse(resp) {
			t.Errorf("response.%s.json = %+v", state, resp)
		}
	}
}

func TestTerminalFixtureVocabulary(t *testing.T) {
	var reasons map[string]struct {
		StatusClass string `json:"status_class"`
	}
	if err := json.Unmarshal(terminalFixture(t, "reasons.json"), &reasons); err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(reasons))
	for k, v := range reasons {
		got = append(got, k)
		if !slices.Contains([]string{"409", "422", "413", "503"}, v.StatusClass) {
			t.Errorf("reason %s has status_class %q", k, v.StatusClass)
		}
	}
	want := slices.Clone(terminalFixtureReasons)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("reasons.json keys = %v, want %v", got, want)
	}
	var classes []string
	if err := json.Unmarshal(terminalFixture(t, "abandoned.json"), &classes); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(classes, []string{TerminalAbandonedIgnored, TerminalAbandonedColdDirty}) {
		t.Fatalf("abandoned.json = %v", classes)
	}
}

func TestTerminalFixtureInvalidControlsAreRejected(t *testing.T) {
	var req TerminalPreservationRequest
	if err := strictTerminalDecode(terminalFixture(t, "invalid/request.extra-field.json"), &req); err == nil {
		t.Error("invalid/request.extra-field.json passed strict decode")
	}
	var resp TerminalPreservationResponse
	if err := strictTerminalDecode(terminalFixture(t, "invalid/response.extra-field.json"), &resp); err == nil {
		t.Error("invalid/response.extra-field.json passed strict decode")
	}
	for _, name := range []string{"request.unknown-abandoned-class", "request.bad-sha"} {
		var r TerminalPreservationRequest
		if err := strictTerminalDecode(terminalFixture(t, "invalid/"+name+".json"), &r); err != nil {
			t.Fatalf("%s must decode structurally: %v", name, err)
		}
		if validateTerminalRequest(r) {
			t.Errorf("%s passed validation", name)
		}
	}
	for _, name := range []string{"response.unknown-state", "response.unknown-reason", "response.bad-sha",
		"response.request-disposition", "response.recorded-preserved", "response.preserved-closed"} {
		var r TerminalPreservationResponse
		if err := strictTerminalDecode(terminalFixture(t, "invalid/"+name+".json"), &r); err != nil {
			t.Fatalf("%s must decode structurally: %v", name, err)
		}
		if validateTerminalResponse(r) {
			t.Errorf("%s passed validation", name)
		}
	}
}
