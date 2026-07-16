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
