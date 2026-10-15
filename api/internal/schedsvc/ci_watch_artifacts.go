package schedsvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/vtmocanu/uzi/api/internal/ciartifacts"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func parseCIArtifacts(raw []byte) ([]string, error) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return nil, nil
	}
	var selectors []string
	if err := json.Unmarshal(raw, &selectors); err != nil {
		return nil, fmt.Errorf("%w: invalid ci_artifacts", errBadConfig)
	}
	if len(selectors) > 20 {
		return nil, fmt.Errorf("%w: invalid ci_artifacts", errBadConfig)
	}
	unique := make([]string, 0, len(selectors))
	seen := map[string]bool{}
	for _, s := range selectors {
		if !seen[strings.TrimSpace(s)] {
			unique = append(unique, s)
			seen[strings.TrimSpace(s)] = true
		}
	}
	out, err := ciartifacts.NormalizeSelectors(unique)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid ci_artifacts", errBadConfig)
	}
	sort.Strings(out)
	return out, nil
}

// ciRevalidate compares to discovery, never to a subsequently fetched revision.
// An old red candidate cannot reset a now-green episode or consume an attempt.
func ciRevalidate(ctx context.Context, f forge.Forge, repo store.GetRepoForUserRow, k *ciKey) (bool, error) {
	if k.run.ID <= 0 || k.run.WorkflowKey == "" || k.run.SHA == "" || k.run.UpdatedAt.IsZero() {
		return false, nil // equality of missing identity metadata proves no freshness
	}
	r, err := f.GetWorkflowRun(ctx, repo.ForgeProjectID, k.run.ID)
	if err != nil {
		return false, err
	}
	b := k.run
	if r.ID != b.ID || r.WorkflowKey != b.WorkflowKey || r.RepositoryID != b.RepositoryID ||
		r.HeadRepositoryID != b.HeadRepositoryID || r.SHA != b.SHA || r.Branch != b.Branch ||
		r.Branch != repo.DefaultBranch.String || !r.UpdatedAt.Equal(b.UpdatedAt) ||
		!ciSameAttempt(r.RunAttempt, b.RunAttempt) || ciIgnoredEvent(r.Event) ||
		!ciCompleted(r) || ciClassify(r) != ciClassRed {
		return false, nil
	}
	if r.RepositoryID != 0 && r.RepositoryID != repo.ForgeProjectID {
		return false, nil
	}
	if r.HeadRepositoryID != 0 && r.HeadRepositoryID != repo.ForgeProjectID {
		return false, nil
	}
	if repo.ForgeType == "github" && (r.RepositoryID != repo.ForgeProjectID || r.HeadRepositoryID != repo.ForgeProjectID) {
		return false, nil
	}
	return true, nil
}
func ciSameAttempt(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
func ciStartReadFailure(res ciStartResult, k *ciKey, err error) (ciStartResult, error) {
	if reason, ok := ciForgeErrOutcomeBenign(err); ok {
		res.skips = append(res.skips, ciSkip(k, reason))
		return res, nil
	}
	return res, err
}

func artifactRefusal(err error) (reason string, safeContinue bool) {
	switch {
	case errors.Is(err, forge.ErrArtifactMissing):
		return "missing", true
	case errors.Is(err, forge.ErrArtifactExpired):
		return "expired", true
	case errors.Is(err, forge.ErrArtifactDenied):
		return "denied", true
	case errors.Is(err, forge.ErrForgeVersionUnsupported):
		return "unsupported", true
	case errors.Is(err, forge.ErrArtifactBudget):
		return "download_budget", false
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return "deadline", false
	default:
		return "download_unknown", false
	}
}

// ciCollectArtifacts performs no DB writes. One 60-second child context bounds
// the sequential list and at most 100 downloads; only pre-body status refusals
// permit siblings to continue. Unknown partial consumption stops the collection.
func (e *Scheduler) ciCollectArtifacts(ctx context.Context, sched store.RunSchedule, repo store.GetRepoForUserRow, f forge.Forge, cfg CIRepairConfig, k *ciKey) (*ciartifacts.Manifest, []ciartifacts.Input) {
	selectors, err := parseCIArtifacts(sched.CiArtifacts)
	if err != nil || len(selectors) == 0 {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	scrub, err := e.forge.CIArtifactScrubber(repo.TokenCiphertext)
	if err != nil || scrub == nil {
		c := ciartifacts.NewCollector(ciartifacts.Limits{}, workersvc.ScrubKnownTokens)
		c.Unavailable("source", "", "scrubber_unavailable")
		m, inputs := c.Result()
		return &m, inputs
	}
	clean := func(s string) string { return workersvc.ScrubKnownTokens(scrub(s)) }
	c := ciartifacts.NewCollector(ciartifacts.Limits{MaxFiles: cfg.ArtifactMaxFiles, MaxBytes: cfg.ArtifactMaxBytes}, clean)
	c.SetExecution(ciartifacts.Execution{RunID: k.run.ID, RepositoryID: k.run.RepositoryID,
		HeadRepositoryID: k.run.HeadRepositoryID, WorkflowKey: k.run.WorkflowKey, Workflow: k.run.Name,
		SHA: k.run.SHA, Branch: k.run.Branch, UpdatedAt: k.run.UpdatedAt.UTC().Format(time.RFC3339Nano),
		Attempt: k.run.RunAttempt})
	finish := func() (*ciartifacts.Manifest, []ciartifacts.Input) {
		m, inputs := c.Result()
		return &m, inputs
	}
	// The run-scoped API may return artifacts from any attempt. Only a positively
	// identified first attempt proves that none belongs to an older execution.
	if k.run.RunAttempt == nil || *k.run.RunAttempt != 1 {
		c.Unavailable("source", "", "attempt_provenance_unavailable")
		return finish()
	}
	artifacts, err := f.ListRunArtifacts(ctx, repo.ForgeProjectID, k.run.ID)
	if err != nil {
		reason, _ := artifactRefusal(err)
		c.Unavailable("source", "", reason)
		return finish()
	}
	sort.SliceStable(artifacts, func(i, j int) bool { return artifacts[i].ID < artifacts[j].ID })
	if len(artifacts) > 100 {
		c.Unavailable("limit", "", "artifact_limit")
		artifacts = artifacts[:100]
	}
	matched := make([]bool, len(selectors))
	for _, a := range artifacts {
		for i, s := range selectors {
			if ciartifacts.Matches([]string{s}, a.Name) {
				matched[i] = true
			}
		}
	}
	seen := map[int64]bool{}
	remaining := int64(64 << 20)
	for _, a := range artifacts {
		if seen[a.ID] {
			continue
		}
		seen[a.ID] = true
		if !ciartifacts.Matches(selectors, a.Name) {
			continue
		}
		for i, s := range selectors {
			if ciartifacts.Matches([]string{s}, a.Name) {
				matched[i] = true
			}
		}
		id := strconv.FormatInt(a.ID, 10)
		if ctx.Err() != nil {
			c.Unavailable(id, a.Name, "deadline")
			break
		}
		if a.ID <= 0 || a.RunID != k.run.ID || a.RepositoryID != repo.ForgeProjectID || a.SHA == "" || a.SHA != k.run.SHA {
			c.Unavailable(id, a.Name, "association")
			continue
		}
		if a.Expired {
			c.Unavailable(id, a.Name, "expired")
			continue
		}
		if remaining <= 0 {
			c.Unavailable(id, a.Name, "download_budget")
			break
		}
		data, err := f.DownloadArtifact(ctx, repo.ForgeProjectID, a.ID, remaining)
		if err != nil {
			reason, safe := artifactRefusal(err)
			c.Unavailable(id, a.Name, reason)
			if !safe {
				break
			}
			continue
		}
		if int64(len(data)) > remaining {
			c.Unavailable(id, a.Name, "download_budget")
			break
		}
		remaining -= int64(len(data))
		c.Add(ctx, id, a.Name, data)
	}
	for i, hit := range matched {
		if !hit {
			c.Unavailable("selector-"+strconv.Itoa(i), selectors[i], "missing")
		}
	}
	return finish()
}
