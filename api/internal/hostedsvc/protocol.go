// Package hostedsvc implements the api's half of the controller protocol for
// hosted k8s workers (PRD #58): the desired-state poll and the delivered-once
// join-token handoff.
//
// It is deliberately the ONLY package the controller endpoint reaches, and it
// imports no kubernetes client — nor may it ever (Decision 1: the api gets zero
// kube-apiserver access, `automountServiceAccountToken: false` stays). Everything
// cluster-shaped happens on the far side of the poll, in the controller's own
// module. What crosses is this wire contract and nothing else.
package hostedsvc

import "time"

// The poll is a GET and carries no request body.
//
// It used to be a POST whose body acked the tokens the controller had
// materialized. That is gone, and deliberately: an ack is a report from the very
// component this PRD exists to bound, and it could not name WHICH token it had —
// so a rotation racing an in-flight ack destroyed the fresh plaintext undelivered
// and left the row asserting a delivery that never happened. Delivery is now
// derived from the worker's own registration (see NoteRegistered): a pod
// authenticating with the token proves it holds it, unforgeably and version-exactly,
// and the api decides from its own auth result rather than from anyone's assertion.
//
// The second reason the ack had to go: an ack meant the controller had to READ
// Secret existence, and Decision 1 pins its RBAC to "Secrets create/delete only
// (it writes them, never needs to read them back)". k8s has no existence-only verb,
// so the old contract would have obligated M3 to widen the Role — turning a
// controller compromise from "harvest what flows through the compromise window"
// into "harvest every hosted worker's token for the fleet's life". Nothing here
// reads a Secret now, so Decision 1's Secrets line stands verbatim.

// PollResponse is the desired state of the whole hosted fleet — every hosted
// worker, always, not a delta. The controller reconciles it as a set, which is
// what lets it detect objects that should not exist (Decision 9's orphans); a
// delta or a page could never express "these are all the workers there are".
//
// It carries join-token plaintext, so the handler sets Cache-Control: no-store on
// it — a POST was never cacheable, a GET is.
type PollResponse struct {
	Workers []DesiredWorker `json:"workers"`
}

// DesiredWorker is one hosted worker's desired state.
type DesiredWorker struct {
	ID string `json:"id"`
	// Template is the worker type: the per-template agent image to run
	// (Decision 7). Always set — a hosted row cannot exist without one
	// (ck_workers_hosted_metadata).
	Template string `json:"template"`
	// Size is the S/M/L preset name, resolved to cpu/memory/PVC size by the
	// controller against its own copy of the preset constants. The api sends the
	// NAME, not the resolved quantities: presets are code constants on both
	// sides, and shipping resolved values would make the api the authority on a
	// pod spec it is not allowed to know anything about.
	Size string `json:"size"`
	// Docker is the rootless-DinD sidecar opt-in (PRD #83 M3): true → the controller
	// renders the worker with a privileged DinD native sidecar in the dedicated
	// privileged-tier namespace, false → #58's plain single-container worker in the
	// restricted namespace. A pod-shape DIMENSION orthogonal to Template (Decision 1:
	// docker is not a template), so it rides its own bool. Always present on the wire
	// (no omitempty) so a drop is a visible contract change, not a silent false.
	// COALESCEd from the nullable docker_enabled column, so a NULL (external rows
	// never reach this poll) and an explicit false both mean "no sidecar".
	Docker bool `json:"docker"`
	// Busy is true when the worker holds at least one non-terminal run (PRD #422 M3,
	// Decision 5): it reuses the same active-run predicate the DeleteWorker guard uses
	// (awaiting_approval included). The controller reads it to defer a deliberate roll of
	// a busy worker (cordon-then-roll-when-idle, M4). Always present on the wire (no
	// omitempty) so a drop is a visible contract change, not a silent false.
	Busy bool `json:"busy"`
	// DrainingSince is WHEN the worker was cordoned (draining_since set, PRD #422
	// Decision 7), or nil when it is not draining (nil == not draining). A cordoned
	// worker keeps heartbeating and finishing its in-flight runs but claims nothing
	// new. The controller reads this to enforce the drain deadline — it is stateless,
	// so `now - draining_since` is the only way it can tell how long a worker has been
	// draining (M5) — and to avoid re-cordoning one already draining (M4). It replaces
	// M3's `draining` bool, which could say only y/n and not for-how-long.
	DrainingSince *time.Time `json:"draining_since"`
	// DiskPressure is true when the worker has self-reported a volume at/above the
	// disk-pressure threshold on at least the last two consecutive fresh heartbeats
	// (PRD #837 M4): the api derives it from a debounced streak counter
	// (stats_disk_pressure_streak >= 2) gated on a fresh last_heartbeat_at, so a
	// single spike or a stale gauge never fires it. It is a DISPLAY/LIFECYCLE-only
	// signal — the controller reads it to decide whether to roll a worker whose /nix
	// PVC is filling, and it NEVER feeds claim or scheduling. Always present on the
	// wire (no omitempty) so a drop is a visible contract change, not a silent false,
	// exactly like Busy. Purely "under pressure": it does NOT bake in Ephemeral.
	DiskPressure bool `json:"disk_pressure"`
	// Ephemeral is true for a run-bound throwaway worker (PRD #529): the controller
	// reads it alongside DiskPressure to choose teardown-vs-reprovision for a
	// disk-pressured worker (an ephemeral one is torn down, a persistent one rolled).
	// Always present on the wire (no omitempty) so a drop is a visible contract change,
	// not a silent false.
	Ephemeral bool `json:"ephemeral"`
	// Generation is bumped whenever the desired spec changes; the controller
	// compares it against what it observes to decide whether to roll (Decision 9).
	Generation int64 `json:"generation"`
	// JoinToken is the plaintext join token, present ONLY while it is still
	// undelivered. Once the worker itself registers with it — proving it holds it —
	// the api destroys its sealed copy and this field is null forever after
	// (Decision 3). The TTL sweep also clears it if no pod ever proves it booted.
	//
	// A null therefore means "this worker needs no token written", never "this
	// worker has no token": either a pod already proved it holds one (the k8s Secret
	// is then the only place the plaintext exists, exactly as a hand-run worker's
	// token lives only where its user pasted it), or the buffer expired unread and
	// recovery is a rotation. Both are the controller's cue to reconcile the worker
	// WITHOUT touching its Secret, never to invent or clear one.
	JoinToken *string `json:"join_token"`
}

// ---------------------------------------------------------------------------
// Status report (controller -> api), PRD #113 M4. The api's half of the wire.
// ---------------------------------------------------------------------------

// StatusReport mirrors the controller's protocol.StatusReport.
//
// Re-declared rather than imported, for the same reason PollResponse is: the contract
// is the JSON on the wire, not a Go package, and importing the controller module would
// pull its kube client graph into an api that must have none. Drift is caught rather
// than trusted — status_wire_contract_test.go parses the golden the CONTROLLER's test
// marshals, so a field renamed on either side reddens the other side's build gate.
//
// Note the golden lives under controller/, the opposite direction from the poll's.
// The producer owns the golden, and for /status the producer is the controller.
//
// DISPLAY-ONLY (Decision 10). Nothing decoded here may reach a token, a worker's
// online/offline status, its generation, or the run lane; see the query comments in
// queries/worker_roll_health.sql, where that is enforced by the SQL rather than
// promised by prose.
type StatusReport struct {
	// ReportedAt is the controller's clock at send. DISPLAY ONLY — the api stamps its
	// own observed_at at receipt and computes freshness from that. A field the
	// untrusted party controls must never drive an expiry the same party benefits from
	// extending.
	ReportedAt          time.Time      `json:"reported_at"`
	PollIntervalSeconds int            `json:"poll_interval_seconds"`
	WorkerImageTag      string         `json:"worker_image_tag"`
	Workers             []WorkerStatus `json:"workers"`
}

// WorkerStatus is one hosted worker's roll health as it arrives on the wire.
//
// Every string here is controller-supplied and UNTRUSTED: it is sanitized before
// persistence and the phase is validated against a closed enum. RestartCount is a
// value because 0 is a real observation; LastExitCode is a pointer because "exited 0"
// and "never terminated" are different facts.
type WorkerStatus struct {
	ID                string     `json:"id"`
	Phase             string     `json:"phase"`
	PhaseSince        *time.Time `json:"phase_since"`
	TargetImage       string     `json:"target_image"`
	PodPhase          string     `json:"pod_phase"`
	BlockingContainer *string    `json:"blocking_container"`
	BlockingReason    *string    `json:"blocking_reason"`
	RestartCount      int32      `json:"restart_count"`
	LastExitCode      *int32     `json:"last_exit_code"`
}
