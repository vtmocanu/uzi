package hostedsvc

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// wireContractFixture is the single golden file both sides of the controller
// protocol validate against, so the api producer and the controller consumer can
// never drift into two lenient fakes. This test pins that the api MARSHALS exactly
// this shape; the controller side
// (controller/internal/protocol/protocol_contract_test.go) pins that it PARSES the
// same file. Same mechanism as the worker protocol's claim contract
// (claim_wire_contract_test.go vs. agent/test/claim-skills-contract.test.ts), which
// is what makes re-declaring the types across the module boundary safe rather than
// hopeful. Regenerate with `UPDATE_GOLDEN=1 go test ./internal/hostedsvc/...`.
const wireContractFixture = "testdata/controller_poll_wire.json"

// samplePollResponse is a representative desired-state response covering both
// token states the wire can carry: one still awaiting delivery, one where no token
// is to be written (already delivered, or expired unread — indistinguishable to the
// controller, and deliberately so: both mean "reconcile this worker, touch no
// Secret"). Values are fixed (no random uuids) so the golden file is stable, and the
// token is an obvious non-credential so secret scanners never flag the fixture.
func samplePollResponse() PollResponse {
	token := "uzw_EXAMPLE-NOT-A-REAL-TOKEN" //nolint:gosec // G101: a deliberate non-credential fixture value (see the doc comment above), not a hardcoded secret.
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	owner := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	nonce := "registration-nonce"
	version := "0.11.7"
	pod := "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	reason := "image_roll"
	never, previously := true, false
	livenessUntil := at.Add(60 * time.Second)
	return PollResponse{Workers: []DesiredWorker{
		{
			RollEvidence: &RollEvidence{
				NeverRegistered: &previously, Version: 1, OwnerID: owner, Registered: true,
				SchedulingCapabilities: []string{"docker"}, ProtocolCapabilities: []string{"cross_check_v1", "cross_check_lane_v1"},
				OrdinaryCoverage: false, CheckerCoverage: true, RegisteredVersion: &version,
				RegisterNonce: &nonce, PodUID: &pod, HeartbeatAt: &at, EvaluatedAt: at, ValidUntil: at.Add(45 * time.Second),
				LivenessValidUntil: &livenessUntil,
			},
			ID:         "11111111-1111-1111-1111-111111111111",
			Template:   "base",
			Size:       "s",
			Generation: 1,
			// A docker-capable worker: the controller renders it with the privileged
			// DinD native sidecar in the docker namespace (PRD #83 M3).
			Docker: true,
			// Isolated false here, true on the second worker (PRD #1906 M5): both states of the
			// lane dimension ride one wire so a drop of the field on either side reddens.
			Isolated: false,
			// Busy true, DrainingSince nil on this worker; the second is Busy false,
			// DrainingSince set — so BOTH states of BOTH fields appear on one wire and a drop
			// of either field on either side reddens (PRD #422 M3/M5), the same both-states
			// rationale as Docker above.
			Busy:          true,
			DrainingSince: nil,
			// DiskPressure true, Ephemeral false here; the second worker is the mirror
			// (false/true), so BOTH states of BOTH new M4 fields ride one wire and a drop of
			// either on either side reddens (PRD #837 M4), the same both-states rationale as
			// Docker/Busy above.
			DiskPressure:        true,
			DiskPressureVolumes: []string{"nix", "dind"},
			Ephemeral:           false,
			// CustodyHeld true here, false on the second worker: both states of the new
			// PRD #1296 M1 field ride one wire so a drop on either side reddens, the same
			// both-states rationale as Docker/Busy/DiskPressure above.
			CustodyHeld: true,
			JoinToken:   &token,
		},
		{
			RollEvidence:   &RollEvidence{NeverRegistered: &never, Version: 1, OwnerID: owner, SchedulingCapabilities: []string{}, ProtocolCapabilities: []string{}, EvaluatedAt: at, ValidUntil: at},
			DrainingReason: &reason,
			ID:             "22222222-2222-2222-2222-222222222222",
			Template:       "jvm",
			Size:           "l",
			Generation:     4,
			// docker false: #58's plain single-container worker in the restricted
			// namespace — both docker states on one wire so a drop of the field is a
			// red build on both sides.
			Docker: false,
			// Isolated true: the mirror of the first worker (PRD #1906 M5). A lane worker is
			// ephemeral and never docker, which this worker also is, so the sample stays a shape
			// the api can actually produce.
			Isolated: true,
			// Busy false, DrainingSince set: this worker is cordoned but idle — the mirror of
			// the first worker, so both states of both fields ride one wire (PRD #422 M3/M5).
			// A fixed time so the golden is stable.
			Busy:          false,
			DrainingSince: func() *time.Time { t := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC); return &t }(),
			// DiskPressure false, Ephemeral true: the mirror of the first worker, so both
			// states of both M4 fields appear on one wire (PRD #837 M4).
			DiskPressure:        false,
			DiskPressureVolumes: []string{},
			Ephemeral:           true,
			// CustodyHeld false: the mirror of the first worker, so both states of the
			// PRD #1296 M1 field appear on one wire.
			CustodyHeld: false,
			// No token to write: a pod already proved it holds one (its plaintext
			// lives only in the cluster Secret now), or the buffer expired unread.
			JoinToken: nil,
		},
		{
			ID:       "33333333-3333-3333-3333-333333333333",
			Template: "base", Size: "m", Generation: 1, Docker: true,
			DiskPressureVolumes: []string{"dind"},
			DindMaintenance: &workersvc.DindMaintenance{
				ID: "44444444-4444-4444-4444-444444444444", Nonce: "maintenance-operation",
				Phase: "requested", DeploymentUID: "deployment-uid", PVCUID: "pvc-uid",
				RegisterNonce: "registration-nonce",
			},
		},
	}}
}

func TestPollResponseWireContract(t *testing.T) {
	got, err := json.MarshalIndent(samplePollResponse(), "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got = append(got, '\n')

	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(wireContractFixture), 0o755); err != nil { //nolint:gosec // G301: a checked-in test golden directory, not sensitive data; 0755 matches the repo's other testdata dirs.
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(wireContractFixture, got, 0o644); err != nil { //nolint:gosec // G306: a committed golden fixture that must be world-readable for git/CI; 0644 is intentional.
			t.Fatalf("write golden: %v", err)
		}
		t.Log("golden file updated")
		return
	}

	want, err := os.ReadFile(wireContractFixture)
	if err != nil {
		t.Fatalf("read golden (regenerate with UPDATE_GOLDEN=1 go test ./internal/hostedsvc/...): %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("poll response wire shape drifted from the golden file.\n got:\n%s\nwant:\n%s", got, want)
	}
}

func TestPollLivenessIndependentOfPromotionProof(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	fresh := pgtype.Timestamptz{Time: at.Add(-15 * time.Second), Valid: true}
	stale := pgtype.Timestamptz{Time: at.Add(-60 * time.Second), Valid: true}
	for _, tc := range []struct {
		name      string
		status    string
		heartbeat pgtype.Timestamptz
		proof     pgtype.Timestamptz
		wantLife  bool
	}{
		{"legacy_online", "online", fresh, pgtype.Timestamptz{}, true},
		{"online_stale", "online", stale, pgtype.Timestamptz{}, false},
		{"online_at_cutoff", "online", pgtype.Timestamptz{Time: at.Add(-45 * time.Second), Valid: true}, pgtype.Timestamptz{}, false},
		{"online_without_heartbeat", "online", pgtype.Timestamptz{}, pgtype.Timestamptz{}, false},
		{"offline_fresh", "offline", fresh, pgtype.Timestamptz{}, false},
		{"fresh_liveness_stale_proof", "online", fresh, stale, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeStore()
			f.workers = []store.ListHostedWorkersForControllerRow{{
				Status: tc.status, LastHeartbeatAt: tc.heartbeat, RollHeartbeatAt: tc.proof,
				SnapshotRegisterNonce: pgtype.Text{String: "current-registration", Valid: true},
			}}
			got, err := New(f, nil, func() time.Time { return at }, 45*time.Second).Poll(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			e := got.Workers[0].RollEvidence
			if !e.Registered || e.PodUID != nil || !e.EvaluatedAt.Equal(at) {
				t.Fatalf("legacy registration changed: %+v", e)
			}
			if tc.wantLife {
				if e.LivenessValidUntil == nil || !e.LivenessValidUntil.Equal(at.Add(30*time.Second)) {
					t.Fatalf("fresh API liveness missing: %+v", e)
				}
			} else if e.LivenessValidUntil != nil {
				t.Fatalf("unsupported/expired API liveness: %+v", e)
			}
			if tc.proof.Valid {
				if e.HeartbeatAt == nil || !e.HeartbeatAt.Equal(at.Add(-60*time.Second)) ||
					!e.ValidUntil.Equal(at.Add(-15*time.Second)) {
					t.Fatalf("ordinary heartbeat refreshed promotion proof: %+v", e)
				}
			} else if e.HeartbeatAt != nil || !e.ValidUntil.Equal(at) {
				t.Fatalf("ordinary heartbeat manufactured promotion proof: %+v", e)
			}
			raw, err := json.Marshal(e)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatal(err)
			}
			life, present := fields["liveness_valid_until"]
			if !present || (!tc.wantLife && string(life) != "null") {
				t.Fatalf("liveness must be explicitly nullable on the wire: %s", raw)
			}
		})
	}
}

// There is deliberately no request-side contract to pin: the poll is a GET with no
// body. The ack that used to travel in one is gone — delivery is proved by the
// worker's own registration, not asserted by the controller.
