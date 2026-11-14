package workersvc

import (
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// fixtures/terminal-preservation/completion-reasons.json is the list the worker decoder
// (agent/src/client.ts decodeCompletedPublicationReason) is pinned to.
func TestTerminalPreservationFixtureCompletionReasons(t *testing.T) {
	b, err := os.ReadFile("../../../fixtures/terminal-preservation/completion-reasons.json")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	want := []string{reasonTerminalPreservationPending, reasonTerminalPreservationRefused}
	if !slices.Equal(got, want) {
		t.Fatalf("completion-reasons.json = %v, want %v", got, want)
	}
}

// TestTerminalPreservationFixtureMatchesServerResult ties each shared response fixture to the
// server: it builds the stored rows the fixture describes, runs terminalResult over them, maps the
// result onto the handler's DTO the way handler.WorkerRunTerminalPreservation does, and requires the
// JSON to equal the fixture. A change inside terminalResult (the disposition, state, reason or ref
// it echoes) reddens this test. It rebuilds the rows and the handler's DTO mapping itself, so a
// change to the handler mapping or to which state/disposition terminalBind stores is not caught
// here (the 00449 CHECKs bound the latter).
func TestTerminalPreservationFixtureMatchesServerResult(t *testing.T) {
	const (
		runID  = "11111111-1111-4111-8111-111111111111"
		presID = "55555555-5555-4555-8555-555555555555"
		conID  = "66666666-6666-4666-8666-666666666666"
		tip    = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	)
	cases := []struct {
		fixture      string
		presState    string
		disposition  string
		conState     string
		refusal      string
		wantReasonIn bool
	}{
		{"published", "live", TerminalDispositionPreserved, "published", "", false},
		{"invoked", "publishing", TerminalDispositionPreserved, "invoked", "", false},
		{"publish_unknown", "publishing", TerminalDispositionPreserved, "publish_unknown", "", false},
		{"prepared", "bound", TerminalDispositionPreserved, "prepared", "", false},
		{"refused", "live", TerminalDispositionPreserved, "refused", apitypes.TerminalReasonSendSuppressed, true},
		{"recorded", "not_published", TerminalDispositionClosedIssue, "recorded", "", false},
		{"recorded-pr", "live", TerminalDispositionClosedPR, "recorded", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			p := store.RunTerminalPreservation{ID: uuid.MustParse(presID), RunID: uuid.MustParse(runID), Ref: pushbroker.TerminalRef(uuid.MustParse(runID)), State: tc.presState}
			c := store.RunTerminalContribution{ID: uuid.MustParse(conID), PreservationID: p.ID, RunID: p.RunID, Tip: tip, Disposition: tc.disposition, State: tc.conState}
			if tc.refusal != "" {
				c.RefusalReason = pgtype.Text{String: tc.refusal, Valid: true}
			}
			res := terminalResult(p, c)
			got, err := json.Marshal(apitypes.TerminalPreservationResponse{
				PreservationID: res.PreservationID.String(), ContributionID: res.ContributionID.String(), Ref: res.Ref, Tip: res.Tip,
				State: res.State, PreservationState: res.PreservationState, Disposition: res.Disposition, Reason: res.Reason,
			})
			if err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile("../../../fixtures/terminal-preservation/response." + tc.fixture + ".json")
			if err != nil {
				t.Fatal(err)
			}
			var gotM, wantM map[string]any
			if err := json.Unmarshal(got, &gotM); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(raw, &wantM); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(gotM, wantM) {
				t.Fatalf("terminalResult wire shape = %v, fixture %s = %v", gotM, tc.fixture, wantM)
			}
			if _, has := wantM["reason"]; has != tc.wantReasonIn {
				t.Fatalf("fixture %s reason presence = %v, want %v", tc.fixture, has, tc.wantReasonIn)
			}
		})
	}
}

// A refusal reason is surfaced only for a refused contribution; a stored reason on any other state
// (which the 00449 refusal_state CHECK forbids) is never echoed.
func TestTerminalResultReasonOnlyWhenRefused(t *testing.T) {
	c := store.RunTerminalContribution{State: "published", RefusalReason: pgtype.Text{String: apitypes.TerminalReasonSendSuppressed, Valid: true}}
	if got := terminalResult(store.RunTerminalPreservation{}, c).Reason; got != "" {
		t.Fatalf("reason on a published contribution = %q", got)
	}
}
