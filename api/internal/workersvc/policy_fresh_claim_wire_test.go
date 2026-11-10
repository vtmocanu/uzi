package workersvc

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// policyKeys marshals the claim projection of a run and keeps only its policy_* keys.
func policyKeys(t *testing.T, run store.Run) map[string]any {
	t.Helper()
	b, err := json.Marshal(ClaimPayload{
		PolicyPreserveOnly: run.PolicyPreserveRequired, PolicyExpectedHead: policyExpectedHead(run),
		PolicyFreshSession: policyFreshSession(run), PolicyFreshUnconfirmed: policyFreshUnconfirmed(run),
		SessionID: textPtr(run.SessionID),
	})
	if err != nil {
		t.Fatal(err)
	}
	var all map[string]any
	if err := json.Unmarshal(b, &all); err != nil {
		t.Fatal(err)
	}
	out := map[string]any{}
	for k, v := range all {
		if len(k) > 7 && k[:7] == "policy_" {
			out[k] = v
		}
	}
	return out
}

func readClaimFixture(t *testing.T, name string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("../../../fixtures/claims/" + name) //nolint:gosec // G304: test reads a fixed fixtures/claims file named by the test itself
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]any
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	return want
}

func freshRun(consumed, confirmed int64, claim int64, head string) store.Run {
	run := store.Run{PolicyPreserveRequired: true, PolicyRequiredHead: pgconv.Text(head), ClaimGeneration: claim}
	if consumed != 0 {
		run.PolicyFreshConsumedGeneration = pgtype.Int8{Int64: consumed, Valid: true}
	}
	if confirmed != 0 {
		run.PolicyFreshConfirmedGeneration = pgtype.Int8{Int64: confirmed, Valid: true}
	}
	return run
}

// The agent reads the same files, so the two sides cannot drift on the Fresh claim keys.
func TestPolicyFreshClaimSharedWire(t *testing.T) {
	for _, tc := range []struct {
		fixture string
		run     store.Run
	}{
		{"policy-fresh-session-claim.json", freshRun(2, 0, 2, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")},
		{"policy-fresh-unconfirmed-claim.json", freshRun(2, 0, 3, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")},
	} {
		want := readClaimFixture(t, tc.fixture)
		if got := policyKeys(t, tc.run); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: claim keys %v, fixture %v", tc.fixture, got, want)
		}
	}
}

// Every Fresh claim shape, with the keys it must and must not carry.
func TestPolicyFreshClaimProjection(t *testing.T) {
	head := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	for name, tc := range map[string]struct {
		run                  store.Run
		session, unconfirmed bool
	}{
		"no history":                          {run: freshRun(0, 0, 2, head)},
		"consuming claim":                     {run: freshRun(2, 0, 2, head), session: true},
		"confirmed history on the same claim": {run: freshRun(2, 2, 2, head)},
		"reclaim of an unconfirmed history":   {run: freshRun(2, 0, 3, head), unconfirmed: true},
		"reclaim of a confirmed history":      {run: freshRun(2, 2, 3, head)},
		"not preserving":                      {run: func() store.Run { r := freshRun(2, 0, 2, head); r.PolicyPreserveRequired = false; return r }()},
		"not preserving, later claim":         {run: func() store.Run { r := freshRun(2, 0, 3, head); r.PolicyPreserveRequired = false; return r }()},
	} {
		got := policyKeys(t, tc.run)
		if _, ok := got["policy_fresh_session"]; ok != tc.session {
			t.Fatalf("%s: policy_fresh_session present=%v", name, ok)
		}
		if _, ok := got["policy_fresh_unconfirmed"]; ok != tc.unconfirmed {
			t.Fatalf("%s: policy_fresh_unconfirmed present=%v", name, ok)
		}
	}
}
