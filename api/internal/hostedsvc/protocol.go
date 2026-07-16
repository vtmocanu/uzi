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

// PollRequest is what the controller POSTs on every cycle. It is a poll and an
// acknowledgement in one round trip: there is exactly one controller-facing
// endpoint, and folding the ack into the next poll keeps it that way.
type PollRequest struct {
	// Materialized lists the hosted worker ids whose join-token Secret the
	// controller OBSERVES in the cluster right now — read fresh from the
	// apiserver each cycle, never remembered across restarts (Decision 2: the
	// controller is stateless, desired state in the DB, observed state in the
	// cluster). That is precisely what makes this ack safe to act on: it asserts
	// a durable fact about the world, not a claim about what the controller did.
	Materialized []string `json:"materialized"`
}

// PollResponse is the desired state of the whole hosted fleet — every hosted
// worker, always, not a delta. The controller reconciles it as a set, which is
// what lets it detect objects that should not exist (Decision 9's orphans); a
// delta or a page could never express "these are all the workers there are".
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
	// JoinToken is the plaintext join token, present ONLY while its delivery is
	// still unacknowledged. Once the controller reports the worker in
	// PollRequest.Materialized, the api destroys its sealed copy and this field is
	// null forever after (Decision 3).
	//
	// A null here therefore means "already delivered", never "no token" — the
	// worker's k8s Secret is by then the only place the plaintext exists, exactly
	// as a hand-run worker's token lives only where its user pasted it.
	JoinToken *string `json:"join_token"`
}
