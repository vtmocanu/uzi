package forge

// GitHub App keys remain in the API's encrypted connection credential. Only
// downscoped, expiring installation tokens may cross the worker boundary.
import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// GitHubAppCredential is sealed using the existing connection secretbox. It is
// accepted only for GitHub connections and is never returned by a read endpoint.
type GitHubAppCredential struct {
	AuthType       string   `json:"auth_type"`
	AppID          string   `json:"app_id"`
	InstallationID int64    `json:"installation_id"`
	PrivateKey     string   `json:"private_key"`
	Repositories   []string `json:"repositories"`
}

var appRepoName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)

func IsGitHubAppCredential(credential string) bool {
	return strings.HasPrefix(strings.TrimSpace(credential), "{")
}

// ConnectionSecretValues lets API-side evidence scrubbers cover App keys and
// every minted token without returning plaintext credentials to their callers.
func ConnectionSecretValues(credential string) []string {
	values := []string{credential}
	if IsGitHubAppCredential(credential) {
		if provider, err := githubAppProvider(credential, 15*time.Second); err == nil {
			values = append(values, provider.secretValues()...)
			for _, line := range strings.Split(provider.config.PrivateKey, "\n") {
				if len(line) >= 16 {
					values = append(values, line)
				}
			}
		}
	}
	return values
}

func parseGitHubAppCredential(value string) (GitHubAppCredential, *rsa.PrivateKey, error) {
	var c GitHubAppCredential
	d := json.NewDecoder(strings.NewReader(value))
	d.DisallowUnknownFields()
	if len(value) > 64<<10 || d.Decode(&c) != nil || d.Decode(new(any)) != io.EOF {
		return c, nil, errors.New("github app: invalid credential document")
	}
	id, err := strconv.ParseInt(c.AppID, 10, 64)
	if err != nil || id <= 0 || c.AuthType != "github_app" || c.InstallationID <= 0 || len(c.Repositories) == 0 || len(c.Repositories) > 100 {
		return c, nil, errors.New("github app: positive App and installation IDs and explicit repositories are required")
	}
	seen := map[string]bool{}
	for _, name := range c.Repositories {
		if !appRepoName.MatchString(name) || name == "." || name == ".." || seen[strings.ToLower(name)] {
			return c, nil, errors.New("github app: invalid or duplicate repository name")
		}
		seen[strings.ToLower(name)] = true
	}
	block, rest := pem.Decode([]byte(c.PrivateKey))
	if block == nil || len(bytes.TrimSpace(rest)) != 0 {
		return c, nil, errors.New("github app: an RSA PEM private key is required")
	}
	var key *rsa.PrivateKey
	switch block.Type {
	case "RSA PRIVATE KEY":
		key, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	case "PRIVATE KEY":
		var parsed any
		parsed, err = x509.ParsePKCS8PrivateKey(block.Bytes)
		key, _ = parsed.(*rsa.PrivateKey)
	}
	if err != nil || key == nil || key.N.BitLen() < 2048 || key.Validate() != nil {
		return c, nil, errors.New("github app: a valid RSA key of at least 2048 bits is required")
	}
	return c, key, nil
}

// githubAppPermissions is the exact permission map uzi requests when it mints an
// installation token, and the map the minted token must come back holding.
//
// administration:read is what makes a ruleset's bypass_actors readable at all:
// GitHub returns that field only to a caller with write access to the ruleset, and
// an installation token otherwise holds none, so every App credential sees it
// omitted. Without it the branch-protection check can only say "protected, bypass
// unknown" (see github_app_access.go), which UNDER-reports an App that really is a
// bypass actor and makes uzi take the PR route it does not strictly need.
//
// It is read-only and the App is installed per-repository, so the blast radius is
// the repos the installer selected — the same scope the rest of this map already
// covers. Do not widen it to administration:write.
func githubAppPermissions() map[string]string {
	return map[string]string{"contents": "write", "issues": "write", "pull_requests": "write", "metadata": "read", "actions": "read", "checks": "read", "statuses": "read", "administration": "read"}
}

type githubAppAuth struct {
	config        GitHubAppCredential
	key           *rsa.PrivateKey
	apiURL        string
	client        *http.Client
	mu            sync.Mutex
	token         string
	expires       time.Time
	slug          string
	permissions   map[string]string
	repositoryIDs map[int64]bool
	secrets       []string
	now           func() time.Time
}

var githubAppCache = struct {
	sync.Mutex
	entries map[string]*githubAppAuth
}{entries: map[string]*githubAppAuth{}}

func githubAppProvider(value string, timeout time.Duration) (*githubAppAuth, error) {
	hash := sha256.Sum256([]byte(value))
	cacheKey := hex.EncodeToString(hash[:])
	githubAppCache.Lock()
	defer githubAppCache.Unlock()
	if p := githubAppCache.entries[cacheKey]; p != nil {
		return p, nil
	}
	p, err := newGitHubAppAuth(value, "https://api.github.com", timeout)
	if err != nil {
		return nil, err
	}
	// Cache is bounded independently of the number of reconnect attempts.
	if len(githubAppCache.entries) >= 128 {
		clear(githubAppCache.entries)
	}
	githubAppCache.entries[cacheKey] = p
	return p, nil
}

func newGitHubAppAuth(value, apiURL string, timeout time.Duration) (*githubAppAuth, error) {
	c, key, err := parseGitHubAppCredential(value)
	if err != nil {
		return nil, err
	}
	client := timeoutClient(timeout)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &githubAppAuth{config: c, key: key, apiURL: strings.TrimRight(apiURL, "/"), client: client, now: time.Now, secrets: []string{value, c.PrivateKey}}, nil
}

func (a *githubAppAuth) secretValues() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.secrets)
}

func (a *githubAppAuth) appRequest(ctx context.Context, method, path string, body any, out any, bearer string) error {
	var data []byte
	if body != nil {
		var err error
		data, err = json.Marshal(body)
		if err != nil {
			return errors.New("github app: invalid request")
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, a.apiURL+path, bytes.NewReader(data))
	if err != nil {
		return errors.New("github app: invalid endpoint")
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := a.client.Do(req)
	if err != nil {
		return errors.New("github app: authentication request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("github app: authentication refused (HTTP %d); verify installation, repository selection and App permissions", resp.StatusCode)
	}
	data, err = io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 || json.Unmarshal(data, out) != nil {
		return errors.New("github app: invalid authentication response")
	}
	return nil
}

func (a *githubAppAuth) credential(ctx context.Context) (string, time.Time, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	if a.token != "" && a.expires.After(now.Add(5*time.Minute)) {
		return a.token, a.expires, nil
	}
	claims := jwt.RegisteredClaims{Issuer: a.config.AppID, IssuedAt: jwt.NewNumericDate(now.Add(-time.Minute)), ExpiresAt: jwt.NewNumericDate(now.Add(8 * time.Minute))}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(a.key)
	if err != nil {
		return "", time.Time{}, errors.New("github app: could not sign authentication")
	}
	a.secrets = append(a.secrets, signed)
	var app struct {
		ID   int64  `json:"id"`
		Slug string `json:"slug"`
	}
	if err = a.appRequest(ctx, http.MethodGet, "/app", nil, &app, signed); err != nil {
		return "", time.Time{}, err
	}
	id, _ := strconv.ParseInt(a.config.AppID, 10, 64)
	if app.ID != id || app.Slug == "" {
		return "", time.Time{}, errors.New("github app: App identity does not match configured ID")
	}
	var installation struct {
		AppID       int64      `json:"app_id"`
		ID          int64      `json:"id"`
		SuspendedAt *time.Time `json:"suspended_at"`
	}
	path := "/app/installations/" + strconv.FormatInt(a.config.InstallationID, 10)
	if err = a.appRequest(ctx, http.MethodGet, path, nil, &installation, signed); err != nil {
		return "", time.Time{}, err
	}
	if installation.AppID != id || installation.ID != a.config.InstallationID || installation.SuspendedAt != nil {
		return "", time.Time{}, errors.New("github app: installation is suspended or belongs to another App")
	}
	var result struct {
		Token        string            `json:"token"`
		ExpiresAt    time.Time         `json:"expires_at"`
		Permissions  map[string]string `json:"permissions"`
		Repositories []struct {
			Name string `json:"name"`
			ID   int64  `json:"id"`
		} `json:"repositories"`
	}
	request := struct {
		Repositories []string          `json:"repositories"`
		Permissions  map[string]string `json:"permissions"`
	}{a.config.Repositories, githubAppPermissions()}
	if err = a.appRequest(ctx, http.MethodPost, path+"/access_tokens", request, &result, signed); err != nil {
		return "", time.Time{}, err
	}
	if result.Token != "" {
		a.secrets = append(a.secrets, result.Token)
	}
	if !strings.HasPrefix(result.Token, "ghs_") || !result.ExpiresAt.After(now.Add(5*time.Minute)) || result.ExpiresAt.After(now.Add(2*time.Hour)) {
		return "", time.Time{}, errors.New("github app: invalid installation token lifetime or type")
	}
	expected := githubAppPermissions()
	if len(result.Permissions) != len(expected) {
		return "", time.Time{}, errors.New("github app: installation token permissions do not match requested minimum")
	}
	for permission, level := range expected {
		if result.Permissions[permission] != level {
			return "", time.Time{}, errors.New("github app: installation token is missing required permissions")
		}
	}
	actual := map[string]bool{}
	ids := map[int64]bool{}
	for _, repo := range result.Repositories {
		if repo.ID <= 0 || ids[repo.ID] {
			return "", time.Time{}, errors.New("github app: invalid repository identity in installation scope")
		}
		ids[repo.ID] = true
		actual[strings.ToLower(repo.Name)] = true
	}
	if len(result.Repositories) != len(a.config.Repositories) || len(actual) != len(a.config.Repositories) {
		return "", time.Time{}, errors.New("github app: installation token repository scope does not match selection")
	}
	for _, name := range a.config.Repositories {
		if !actual[strings.ToLower(name)] {
			return "", time.Time{}, errors.New("github app: installation token includes an unexpected repository")
		}
	}
	a.repositoryIDs = ids
	a.token, a.expires, a.slug, a.permissions = result.Token, result.ExpiresAt, app.Slug, result.Permissions
	return a.token, a.expires, nil
}

type githubAppTransport struct {
	base http.RoundTripper
	auth *githubAppAuth
}

func (t githubAppTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	origin, err := url.Parse(t.auth.apiURL)
	if err != nil || req.URL.Scheme != origin.Scheme || req.URL.Host != origin.Host || req.URL.User != nil {
		return nil, errors.New("github app: refusing credential transmission outside API origin")
	}
	token, _, err := t.auth.credential(req.Context())
	if err != nil {
		return nil, err
	}
	clone := req.Clone(req.Context())
	clone.Header = req.Header.Clone()
	clone.Header.Set("Authorization", "Bearer "+token)
	return t.base.RoundTrip(clone)
}

// ResolveGitCredentialWithExpiry resolves a sealed connection's decrypted value
// at the last API-side handoff. App private keys must never become forge_pat.
func ResolveGitCredentialWithExpiry(ctx context.Context, forgeType Type, baseURL, credential string, timeout time.Duration) (string, time.Time, error) {
	if !IsGitHubAppCredential(credential) {
		return credential, time.Time{}, nil
	}
	if forgeType != TypeGitHub || !isDefaultGitHubBase(baseURL) || strings.HasPrefix(baseURL, "http:") {
		return "", time.Time{}, errors.New("github app: only github.com is supported")
	}
	provider, err := githubAppProvider(credential, timeout)
	if err != nil {
		return "", time.Time{}, err
	}
	return provider.credential(ctx)
}
