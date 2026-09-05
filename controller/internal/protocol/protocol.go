// Package protocol is the controller's side of the controller⇄api wire contract
// (PRD #58 Decision 2).
//
// These types mirror api/internal/hostedsvc/protocol.go. They are re-declared
// rather than imported: the contract between the two components is the JSON on the
// wire, not a Go package, and importing the api module would pull its entire
// dependency graph (pgx, chi, the GitLab client, …) into a controller that needs
// none of it. The same split the TypeScript agent lives with on the worker
// protocol.
//
// Drift is caught, not trusted: protocol_contract_test.go parses the same golden
// file the api's producer test marshals
// (api/internal/hostedsvc/testdata/controller_poll_wire.json), so a field renamed
// on one side fails the other side's build gate.
package protocol

import "time"

// The poll is a GET and carries no request body.
//
// There is deliberately no ack: the controller tells the api nothing about token
// delivery. The api learns it from the worker's own authenticated registration,
// which proves possession of the current token — something this side could never
// assert, since it can see only that A Secret exists, not which token is in it.
// That gap destroyed freshly rotated tokens undelivered.
//
// It also means this controller never reads a Secret back, so Decision 1's RBAC
// ("Secrets create/delete only") holds as written. If you find yourself wanting a
// `get`/`list` on Secrets here, that is the design telling you something is wrong:
// k8s has no existence-only verb, and granting it would let a compromised
// controller harvest every hosted worker's join token for the fleet's lifetime.

// PollResponse is the desired state of the entire hosted fleet — every hosted
// worker, every poll, never a delta. Anything in the worker namespace that is not
// in here is an orphan (Decision 9).
type PollResponse struct {
	Workers []DesiredWorker `json:"workers"`
}

// DesiredWorker is one hosted worker's desired state.
type DesiredWorker struct {
	ID       string `json:"id"`
	Template string `json:"template"`
	// Size is the preset NAME (S/M/L). Resolving it to cpu/memory/PVC quantities is
	// this side's job: the api is not allowed to know what a pod spec looks like.
	Size string `json:"size"`
	// Docker is the rootless-DinD sidecar opt-in (PRD #83 M3). true → render this
	// worker with a privileged DinD native sidecar in the dedicated
	// `enforce: privileged` namespace (UZI_WORKER_DOCKER_NAMESPACE); false → #58's
	// plain single-container worker in the restricted namespace. It is a pod-shape
	// DIMENSION orthogonal to Template (Decision 1: docker is not a template), which
	// is why it rides its own bool rather than a template name. Mirrors the api's
	// hostedsvc.DesiredWorker.Docker; the shared golden pins the two in lockstep.
	Docker bool `json:"docker"`
	// Busy is true when the worker holds at least one non-terminal run (PRD #422 M3).
	// The controller defers a deliberate roll of a busy worker (cordon-then-roll-when-idle,
	// M4). Mirrors the api's hostedsvc.DesiredWorker.Busy; the shared golden pins the two
	// in lockstep.
	Busy bool `json:"busy"`
	// DrainingSince is WHEN the worker was cordoned (PRD #422 Decision 7), or nil when it
	// is not draining (nil == not draining). A cordoned worker keeps heartbeating and
	// finishing its in-flight runs but claims nothing new. This side reads it to enforce
	// the bounded drain deadline — the reconcile loop is stateless, so `now - draining_since`
	// is the only elapsed-time signal it has (M5) — and to avoid re-cordoning a worker
	// already draining (M4). Replaces M3's `draining` bool; mirrors the api's
	// hostedsvc.DesiredWorker.DrainingSince, and the shared golden pins the two in lockstep.
	DrainingSince *time.Time `json:"draining_since"`
	Generation    int64      `json:"generation"`
	// DiskPressure is true when the worker's own disk-space metric crossed the api's
	// pressure threshold (PRD #837 M4). This side reacts by recycling the worker's /nix
	// and /data volumes (teardown + reprovision), unless the worker is Ephemeral or a
	// just-recycled worker is still within the recycle cooldown. Mirrors the api's
	// hostedsvc.DesiredWorker.DiskPressure; the shared golden pins the two in lockstep.
	DiskPressure bool `json:"disk_pressure"`
	// Ephemeral is true for a run-bound worker whose lifetime is a single run (PRD #837
	// M4). Such a worker is EXCLUDED from every disk recycle arm: recycling volumes out
	// from under a run-bound worker would be pointless churn — the worker is torn down
	// when its run ends anyway. Mirrors the api's hostedsvc.DesiredWorker.Ephemeral; the
	// shared golden pins the two in lockstep.
	Ephemeral bool `json:"ephemeral"`
	// JoinToken is the plaintext, present only until a pod proves it holds it (by
	// registering) or the api's buffer expires unread.
	//
	// Null means "write no Secret for this worker" — either one was already
	// delivered (the k8s Secret is then the only copy that exists) or the buffer
	// expired and recovery is a rotation. Never read a null as "this worker has no
	// token", never invent one, never clear the existing Secret on account of it,
	// and never log this field.
	JoinToken *string `json:"join_token"`
}

// ---------------------------------------------------------------------------
// Status report (controller -> api), PRD #113 M3.
// ---------------------------------------------------------------------------

// StatusReport is per-worker roll health, POSTed to /api/controller/status.
//
// DISPLAY-ONLY, and that is a structural property rather than a promise (Decision
// 10). It exists so the Workers UI can tell "rolling" from "rolled and wedged" — a
// distinction no worker can self-report, because a worker whose new pod never
// becomes Ready is offline and says nothing at all. Everything the api does with it
// ends at rendering: it lands in its own table, no query on the claim / heartbeat /
// register path reads it, and the response carries no body for this side to read
// state back out of. PRD #58's "the controller asserts nothing" still holds for
// everything else, and the poll stays a pure read.
//
// This is the FIRST thing this controller has ever asserted to the api, so the
// bound on it matters more than the feature: a lying controller must be able to
// make the UI wrong and nothing else. It may not touch a token, a worker's
// online/offline status, its generation, or the run lane, and it may never create a
// worker row.
//
// Whole fleet every time, never a delta — the same shape decision PollResponse
// records. A delta cannot express "this worker no longer has a signal", and the api
// needs exactly that to let a stale report decay back to version comparison.
type StatusReport struct {
	// ReportedAt is this controller's clock at send. DISPLAY-ONLY: the api stamps its
	// own observed_at at receipt and computes freshness from that. Freshness driven by
	// a timestamp the reporting party controls is freshness the reporting party can
	// extend indefinitely, which is the one thing the TTL exists to prevent.
	ReportedAt time.Time `json:"reported_at"`
	// PollIntervalSeconds lets the api derive its staleness window from this
	// controller's actual cadence rather than assuming the default.
	PollIntervalSeconds int `json:"poll_interval_seconds"`
	// WorkerImageTag is the tag this controller actually rolls workers to
	// (UZI_WORKER_IMAGE_TAG). Decision 9: for hosted workers THIS is the upgrade
	// target, not the api's own release, because values.yaml allows pinning the two
	// independently.
	WorkerImageTag string         `json:"worker_image_tag"`
	Workers        []WorkerStatus `json:"workers"`
}

// The closed phase enum, and it is the WIRE contract: the api matches on these
// exact strings and drops an entry carrying anything else rather than rejecting the
// whole report. Declared here rather than in reconcile because protocol is the leaf
// package both the derivation and the client depend on, and because a phase name is
// a wire value before it is an internal state.
const (
	// PhaseRolling: the new pod is on its way and nothing says it is wedged. Also the
	// Recreate gap, where the old pod is gone and the new one does not exist yet.
	PhaseRolling = "rolling"
	// PhaseStuck: the pod carries a blocking reason, has restarted past the threshold,
	// or has been not-Ready long enough that no healthy roll explains it.
	PhaseStuck = "stuck"
	// PhaseSettled: the pod matching the deployment's spec hash is Ready.
	PhaseSettled = "settled"
)

// WorkerStatus is one hosted worker's roll health.
//
// The pointer fields are pointers because their zero values are meaningful: a
// RestartCount of 0 is a real observation, so it stays a value, while LastExitCode
// must distinguish "exited 0" from "never terminated" and BlockingReason must
// distinguish "no reason" from an empty one. A pointer/value collapse here is
// exactly what the golden's settled worker exists to catch.
type WorkerStatus struct {
	ID string `json:"id"`
	// Phase is the closed enum rolling|stuck|settled. The api validates rather than
	// sanitizes it, and DROPS an entry carrying anything else rather than rejecting
	// the whole report — one garbage row must not blind the fleet.
	Phase string `json:"phase"`
	// PhaseSince is when the phase began, from pod fields (Ready transition, or the
	// pod's creation). Null during the Recreate gap, where no pod exists to date it
	// and inventing now() would restart the clock every tick.
	PhaseSince *time.Time `json:"phase_since"`
	// TargetImage is the full image reference this worker is being rolled to, e.g.
	// harbor.example.com/uzi/agent-jvm:0.11.7 — the UI renders the image name, not a
	// bare tag.
	TargetImage       string  `json:"target_image"`
	PodPhase          string  `json:"pod_phase"`
	BlockingContainer *string `json:"blocking_container"`
	BlockingReason    *string `json:"blocking_reason"`
	RestartCount      int32   `json:"restart_count"`
	LastExitCode      *int32  `json:"last_exit_code"`
}
