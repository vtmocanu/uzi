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
	Size       string `json:"size"`
	Generation int64  `json:"generation"`
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
