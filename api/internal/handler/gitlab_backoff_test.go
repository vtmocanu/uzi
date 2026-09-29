package handler

import (
	"net/http"
	"time"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forgesvc"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
)

// newZeroWaitForgeService keeps GitLab's retry behavior while avoiding retry sleeps
// in handlers backed by an httptest GitLab server. The policy belongs to this
// service instance; it does not change other forge clients.
func newZeroWaitForgeService(q forgesvc.IssueStore, box *secretbox.Box) *forgesvc.Service {
	return forgesvc.NewWithForgeBuilder(q, box, 5*time.Second, nil,
		func(kind forge.Type, baseURL, token string, timeout time.Duration) (forge.Forge, error) {
			return forge.NewWithGitLabBackoff(kind, baseURL, token, timeout,
				func(_, _ time.Duration, _ int, _ *http.Response) time.Duration { return 0 })
		})
}
