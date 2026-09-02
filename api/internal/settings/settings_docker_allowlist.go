package settings

// This file holds the docker-allowlist accessors and their write-time validator
// (PRD #1021 M3, split verbatim from settings.go).

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
)

// DockerRepoAllowlist returns the set of repo ids a docker-enabled worker may claim
// runs for (PRD #89 M-allow). Stored as a comma-separated list of repo UUIDs; an
// absent/empty value yields an EMPTY slice, which the claim gate treats as
// fail-closed (a docker worker then claims no repo-bearing run). Unparseable tokens
// in a hand-edited row are skipped rather than erroring — the same junk-tolerance as
// the bool/int accessors, since write-time validation is the real gate. The slice is
// always non-nil so the claim param encodes as a Postgres array, never NULL.
//
// The claim path (workersvc) reads this STRICTLY — a non-nil error is surfaced and
// the run is left unclaimed — because this is a security control: never claim a repo
// run when the allowlist cannot be read (mirrors HostedWorkerQuota's strict caller).
func (c *Cache) DockerRepoAllowlist(ctx context.Context) ([]uuid.UUID, error) {
	v, err := c.get(ctx, KeyDockerRepoAllowlist)
	return parseRepoAllowlist(v), err
}

// parseRepoAllowlist splits a comma-separated repo-id list into canonical UUIDs,
// skipping empty and unparseable tokens. Always returns a non-nil slice (possibly
// empty). Shared by the accessor and reused as the parse half of validation's intent.
func parseRepoAllowlist(v string) []uuid.UUID {
	out := []uuid.UUID{}
	for _, tok := range strings.Split(v, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		id, err := uuid.Parse(tok)
		if err != nil {
			continue
		}
		out = append(out, id)
	}
	return out
}

// validateRepoAllowlist is the write-time gate for the docker repo allowlist (PRD
// #89 M-allow): a comma-separated list of repo UUIDs. Empty is allowed — it is the
// fail-closed "no repos trusted" value, not a rejection. Each non-empty entry must
// be a valid UUID; a malformed entry fails the WRITE, the only moment a human is
// present to be told, so a typo can never silently widen or void the gate.
//
// Like validateHostedWorkerQuota, this MUST have an explicit Validate case: the
// default branch falls through to ValidateLabel, which REJECTS the comma that an
// allowlist of two or more repos requires — so without this case a valid multi-repo
// allowlist could never be saved at all.
func validateRepoAllowlist(value string) error {
	for _, tok := range strings.Split(value, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		if _, err := uuid.Parse(tok); err != nil {
			return errors.New("must be a comma-separated list of repo ids (UUIDs)")
		}
	}
	return nil
}
