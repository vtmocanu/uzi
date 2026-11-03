package apitypes

import "time"

// CrossCheckHistoryAvailabilityDTO depends only on retained, owner-scoped attempts.
type CrossCheckHistoryAvailabilityDTO struct {
	Available bool `json:"available"`
}

// CrossCheckHistoryDTO has no global page-derived counters.
type CrossCheckHistoryDTO struct {
	Groups     []CrossCheckHistoryGroupDTO `json:"groups"`
	NextCursor string                      `json:"next_cursor,omitempty"`
}

type CrossCheckHistoryGroupDTO struct {
	ParentID     string    `json:"parent_id"`
	ParentTitle  string    `json:"parent_title"`
	ParentStatus string    `json:"parent_status"`
	LatestAt     time.Time `json:"latest_at"`
	Active       bool      `json:"active"`
	// TotalAttempts counts all retained attempts; MatchingAttempts applies stage/outcome.
	TotalAttempts    int64 `json:"total_attempts"`
	MatchingAttempts int64 `json:"matching_attempts"`
	// ActiveAttempts counts the lifecycle union across all attempts. ActiveHidden
	// counts active attempts absent from this response's slice, including filtered ones.
	ActiveAttempts int64                         `json:"active_attempts"`
	ActiveHidden   int64                         `json:"active_hidden"`
	ActiveContext  string                        `json:"active_context,omitempty"`
	Attempts       []CrossCheckHistoryAttemptDTO `json:"attempts"`
	OlderCursor    string                        `json:"older_cursor,omitempty"`
	Omission       string                        `json:"omission,omitempty"`
	// RefreshNeeded means this continuation's lifecycle no longer matches. It is
	// distinct from an exhausted history, whose attempts are empty without this flag.
	RefreshNeeded bool `json:"refresh_needed"`
}

// CrossCheckHistoryAttemptDTO separates execution evidence from recorded review.
type CrossCheckHistoryAttemptDTO struct {
	Stage         string     `json:"stage"`
	Round         int32      `json:"round"`
	Result        string     `json:"result"`
	Review        string     `json:"review"`
	Execution     string     `json:"execution"`
	Reason        string     `json:"reason"`
	FindingsCount *int32     `json:"findings_count"`
	CreatedAt     time.Time  `json:"created_at"`
	DecidedAt     *time.Time `json:"decided_at"`
	InterruptedAt *time.Time `json:"interrupted_at"`
	DeadlineAt    time.Time  `json:"deadline_at"`
	ChildID       *string    `json:"child_id"`
	ChildStatus   *string    `json:"child_status"`
	ChildContext  string     `json:"child_context"`
	Model         *string    `json:"model"`
	Effort        *string    `json:"effort"`
	Historical    bool       `json:"historical"`
	Active        bool       `json:"active"`
	Effect        string     `json:"effect"`
	NextAction    string     `json:"next_action"`
}
