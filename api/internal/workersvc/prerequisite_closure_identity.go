package workersvc

import (
	"context"
	"errors"
	"net/url"
	"strings"

	"github.com/vtmocanu/uzi/api/internal/forge"
)

// Only one terminal slash is ignored. Escapes, dot segments, case changes and
// alternate paths are never equivalent and never authorize automatic rebinding.
func prerequisiteURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") ||
		u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
		strings.ContainsAny(raw, "#%\\") || strings.IndexFunc(raw, func(r rune) bool { return r <= ' ' || r == 127 }) >= 0 {
		return nil, errors.New("prerequisite: invalid forge URL")
	}
	for _, part := range strings.Split(u.Path, "/") {
		if part == "." || part == ".." {
			return nil, errors.New("prerequisite: invalid forge path")
		}
	}
	return u, nil
}

func (s *Service) prerequisiteDriver(b prerequisiteBinding) (forge.Forge, error) {
	if s.forges == nil {
		return nil, errors.New("prerequisite: forge unavailable")
	}
	return s.forges.ForgeForConnection(b.connection.ForgeType, b.connection.BaseUrl, b.connection.TokenCiphertext)
}

// validate observes the stable ID using the same driver as ancestry. Callers
// share their existing external deadline with Git and the second observation.
func (b *prerequisiteBinding) validate(ctx context.Context, driver forge.Forge) error {
	p, err := driver.GetProject(ctx, b.repo.ForgeProjectID)
	if err != nil {
		return err
	}
	u, err := prerequisiteURL(p.WebURL)
	path := strings.TrimSuffix(p.PathWithNamespace, "/")
	if err != nil || ctx.Err() != nil || p.ForgeProjectID != b.repo.ForgeProjectID ||
		path == "" || path != p.PathWithNamespace || strings.TrimSuffix(u.Path, "/") != "/"+path ||
		strings.TrimSuffix(p.WebURL, "/") != strings.TrimSuffix(b.repo.WebUrl, "/") {
		return errors.New("prerequisite: project identity changed")
	}
	for _, part := range strings.Split(path, "/") {
		if part == "" || part == "." || part == ".." {
			return errors.New("prerequisite: invalid project path")
		}
	}
	b.clone = strings.TrimSuffix(p.WebURL, "/") + ".git"
	return nil
}
