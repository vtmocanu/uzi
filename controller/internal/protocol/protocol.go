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

// PollRequest is what the controller POSTs to /api/controller/poll each cycle.
type PollRequest struct {
	// Materialized lists the hosted worker ids whose join-token Secret this
	// controller can see in the cluster right now. It is re-derived from the
	// apiserver on every cycle and never remembered across restarts — the api
	// destroys its only sealed copy of a token on the strength of this field, so
	// it must assert what the cluster durably holds, never what this process
	// believes it did earlier.
	Materialized []string `json:"materialized"`
}

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
	// JoinToken is the plaintext, present only until this controller reports the
	// worker in PollRequest.Materialized. Null means already delivered — the k8s
	// Secret is by then the only copy that exists, so never treat a null as "this
	// worker has no token" and never log this field.
	JoinToken *string `json:"join_token"`
}
