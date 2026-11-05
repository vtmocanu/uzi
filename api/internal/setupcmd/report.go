package setupcmd

import (
	"encoding/json"
	"io"
)

// ReportSchemaVersion is the version of the JSON document on stdout. Bump it
// when a field is removed or changes meaning; adding a field does not.
const ReportSchemaVersion = 1

// Exit codes. Exit 3 (a worker already exists) arrives with the worker step.
const (
	ExitOK     = 0 // every requested step done or already present
	ExitFailed = 1 // a step failed or was refused; the report says which
	ExitUsage  = 2 // usage, configuration, schema or owner precondition; nothing written
)

// Step statuses. A script branches on these strings, so they are a contract.
const (
	StatusConnected         = "connected"
	StatusPresent           = "present"
	StatusDone              = "done"
	StatusEnabled           = "enabled"
	StatusRefused           = "refused"
	StatusNotFound          = "not_found"
	StatusFailed            = "failed"
	StatusSkippedDependency = "skipped_dependency"
	StatusNotRequested      = "not_requested"
)

// Report overall statuses.
const (
	ReportOK     = "ok"
	ReportFailed = "failed"
	// ReportRejected: a precondition failed before any write (exit 2).
	ReportRejected = "rejected"
)

// Step is one entry of the report.
type Step struct {
	Status       string   `json:"status"`
	Message      string   `json:"message,omitempty"`
	ConnectionID string   `json:"connection_id,omitempty"`
	BotUsername  string   `json:"bot_username,omitempty"`
	Findings     []string `json:"findings,omitempty"`
}

// RepoStep is the entry for one requested repo.
type RepoStep struct {
	Path     string   `json:"path"`
	Status   string   `json:"status"`
	RepoID   string   `json:"repo_id,omitempty"`
	Message  string   `json:"message,omitempty"`
	Findings []string `json:"findings,omitempty"`
}

// Report is the one JSON document setup writes to stdout.
type Report struct {
	SchemaVersion int    `json:"schema_version"`
	Status        string `json:"status"`
	ExitCode      int    `json:"exit_code"`
	// Error is set only on a rejected run (exit 2).
	Error     string     `json:"error,omitempty"`
	Forge     *Step      `json:"forge,omitempty"`
	Discovery *Step      `json:"discovery,omitempty"`
	Repos     []RepoStep `json:"repos,omitempty"`
	Worker    *Step      `json:"worker,omitempty"`
	// IssueSync is "pending" once a repo is enabled: setup cannot reach the
	// API's in-process sync, so the poller imports issues on its next pass.
	IssueSync string `json:"issue_sync,omitempty"`
}

// failed reports whether any step ended in a state exit 1 covers.
func (r *Report) failed() bool {
	bad := func(s *Step) bool {
		return s != nil && (s.Status == StatusFailed || s.Status == StatusRefused)
	}
	if bad(r.Forge) || bad(r.Discovery) || bad(r.Worker) {
		return true
	}
	for _, rp := range r.Repos {
		if rp.Status == StatusFailed || rp.Status == StatusRefused || rp.Status == StatusNotFound {
			return true
		}
	}
	return false
}

func writeReport(w io.Writer, r *Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}
