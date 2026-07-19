package apitypes

import "time"

// AgentMemoryWriteRequest is the worker's save-memory body (PRD #90). Only the
// content shape crosses the wire — the (user_id, repo_id) identity is DERIVED
// SERVER-SIDE from the run claim and is NEVER accepted from the body (a compromised
// worker's join token is not user-scoped). The server enforces the title/body size
// caps; this struct just carries the two fields.
type AgentMemoryWriteRequest struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

// AgentMemoryDTO is the browser/CLI/worker view of one memory entry. repo_id +
// repo_name are omitempty because they are surfaced only on the cross-repo user
// list (`/api/me/memory`); the worker's per-(user,repo) read already knows the repo
// and omits them. run_id is omitempty: it is provenance and goes NULL when the
// writing run is pruned (the FK is ON DELETE SET NULL).
type AgentMemoryDTO struct {
	ID        string    `json:"id"`
	RepoID    string    `json:"repo_id,omitempty"`
	RepoName  string    `json:"repo_name,omitempty"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	RunID     string    `json:"run_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}
