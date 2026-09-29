package settings

// This file holds the per-user active-job cap accessor and its write-time validator
// (PRD #1908).

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// maxJobMaxActivePerUser bounds the per-user active-job cap. It only catches a typo (an admin
// meaning 10 and typing 10000); the cap's job is fairness, not capacity planning.
const maxJobMaxActivePerUser = 1000

// JobMaxActivePerUser returns how many non-terminal job runs one user may hold at once
// (PRD #1908). The create path reads it STRICTLY: a non-nil error refuses the create rather
// than falling back to a number no admin chose on a cold-cache blip.
func (c *Cache) JobMaxActivePerUser(ctx context.Context) (int, error) {
	return c.intSetting(ctx, KeyJobMaxActivePerUser)
}

// validateJobMaxActivePerUser is the write-time gate for the cap: a base-10 integer in
// [1, maxJobMaxActivePerUser]. 0 is refused (it would silently disable job creation; an admin
// who wants that removes the product allow-lists instead). The explicit Validate case this
// backs is load-bearing for the reason validateHostedWorkerQuota records: without it the key
// falls through to ValidateLabel and a junk value reads back as the default.
func validateJobMaxActivePerUser(value string) error {
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return errors.New("must be a whole number of jobs")
	}
	if n < 1 || n > maxJobMaxActivePerUser {
		return fmt.Errorf("must be between 1 and %d jobs", maxJobMaxActivePerUser)
	}
	return nil
}
