package forge

import (
	"context"
	"errors"
)

// Artifact contains bounded descriptive metadata only. No field grants download
// authority; DownloadArtifact addresses the API by repository and artifact ID.
type Artifact struct {
	ID           int64
	Name         string
	RunID        int64
	RepositoryID int64
	SHA          string
	SizeBytes    int64
	Expired      bool
}

var (
	ErrArtifactBudget      = errors.New("artifact: budget exceeded")
	ErrArtifactMissing     = errors.New("artifact: missing")
	ErrArtifactExpired     = errors.New("artifact: expired")
	ErrArtifactDenied      = errors.New("artifact: denied")
	errArtifactResponse    = errors.New("artifact: invalid response")
	errArtifactDestination = errors.New("artifact: invalid destination")
	errArtifactRequest     = errors.New("artifact: request failed")
)

func (*gitLab) ListRunArtifacts(context.Context, int64, int64) ([]Artifact, error) {
	return nil, ErrForgeVersionUnsupported
}
func (*gitLab) DownloadArtifact(context.Context, int64, int64, int64) ([]byte, error) {
	return nil, ErrForgeVersionUnsupported
}
func (*forgejo) ListRunArtifacts(context.Context, int64, int64) ([]Artifact, error) {
	return nil, ErrForgeVersionUnsupported
}
func (*forgejo) DownloadArtifact(context.Context, int64, int64, int64) ([]byte, error) {
	return nil, ErrForgeVersionUnsupported
}
