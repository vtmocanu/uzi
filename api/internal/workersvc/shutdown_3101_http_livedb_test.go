package workersvc_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/codexauth"
	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/handler"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func shutdown3101Server(t *testing.T, f workersvc.Shutdown3101Fixture) *httptest.Server {
	t.Helper()
	h := handler.New(f.Pool, f.Queries, config.Config{}, f.Box, nil, f.Service, nil, nil, nil)
	// Handler.New installs the production config gate. This fixture's declared
	// forge is fixed; the broker below redirects only its test-owned origin.
	f.Service.SetForgeBaseURLAllowed(func(u string) bool { return u == "https://forge.e2e" })
	s := httptest.NewServer(h.WorkerRoutes(mw.NewLimiter(1000, time.Minute, nil)))
	t.Cleanup(s.Close)
	return s
}

func shutdown3101Post(t *testing.T, s *httptest.Server, f workersvc.Shutdown3101Fixture, path string, body []byte, tip string) (int, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL+"/api/worker/runs/"+f.RunID.String()+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+f.Token)
	req.Header.Set("Content-Type", "application/json")
	if tip != "" {
		req.Header.Set("X-Uzi-Checkpoint-Tip", tip)
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	resp, err := s.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close HTTP response: %v", err)
		}
	}()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, data
}

func shutdown3101JSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// #3101: real worker Bearer routing -> production service admission -> production
// broker -> an independent bare origin. Git verifies content as well as the ref;
// a response-only publisher cannot satisfy this proof.
func TestShutdown3101CheckpointHTTPLiveDB(t *testing.T) {
	for _, stage := range []string{"admission", "live recheck"} {
		for _, mutation := range []string{"running", "terminal", "generation", "released", "worker"} {
			t.Run(stage+"/"+mutation, func(t *testing.T) {
				f := workersvc.NewShutdown3101CheckpointFixture(t)
				origin, tip, pack := shutdown3101Pack(t)
				var calls atomic.Int32
				f.Service.SetPublishFn(func(ctx context.Context, o pushbroker.Options) (pushbroker.Result, error) {
					calls.Add(1)
					o.CloneURL = "file://" + origin
					// Local test transport has no HTTP basic auth; all admission/pack/ancestry
					// and receive-pack work remains the production broker's.
					o.Username, o.PAT = "", ""
					return pushbroker.Publish(ctx, o)
				})
				if stage == "admission" {
					f.Mutate(mutation)
				} else {
					f.DuringPublish(mutation)
				}
				s := shutdown3101Server(t, f)
				status, body := shutdown3101Post(t, s, f, fmt.Sprintf("/publish?claim_generation=%d", f.Generation), pack, tip)
				wantStatus := http.StatusOK
				accepted := mutation == "running" || (stage == "admission" && mutation == "terminal")
				if stage == "admission" && !accepted {
					wantStatus = http.StatusConflict
					if mutation == "worker" {
						wantStatus = http.StatusNotFound
					}
				}
				if status != wantStatus {
					t.Fatalf("status=%d body=%s want=%d", status, body, wantStatus)
				}
				var result struct {
					Published            bool
					Ref, Skipped, Reason string
				}
				if err := json.Unmarshal(body, &result); err != nil {
					t.Fatal(err)
				}
				if result.Published != accepted {
					t.Fatalf("publish response=%s accepted=%v", body, accepted)
				}
				if stage == "live recheck" && !accepted && result.Skipped != "not_descendant" {
					t.Fatalf("live refusal=%s", body)
				}
				if stage == "admission" && (mutation == "generation" || mutation == "released") && result.Reason != "stale_claim" {
					t.Fatalf("claim refusal=%s", body)
				}
				wantCalls := int32(0)
				if accepted {
					wantCalls = 1
				}
				if calls.Load() != wantCalls {
					t.Fatalf("broker calls=%d want=%d", calls.Load(), wantCalls)
				}
				refs := shutdown3101Git(t, origin, nil, "for-each-ref", "--format=%(refname) %(objectname)", "refs/uzi-checkpoints/")
				run, err := f.Queries.GetRunByID(context.Background(), f.RunID)
				if err != nil {
					t.Fatal(err)
				}
				if accepted {
					wantRef := fmt.Sprintf("refs/uzi-checkpoints/agent/issue-%d", run.IssueIid.Int64)
					if result.Ref != wantRef || refs != wantRef+" "+tip+"\n" {
						t.Fatalf("origin refs=%q response=%s wantRef=%s", refs, body, wantRef)
					}
					if got := shutdown3101Git(t, origin, nil, "show", wantRef+":shutdown.txt"); got != "shutdown 3101 durable work\n" {
						t.Fatalf("origin content=%q", got)
					}
					wantStatus := "running"
					if mutation == "terminal" {
						wantStatus = "cancelled"
					}
					if run.CheckpointTip.String != tip || run.Status != wantStatus || run.ClaimGeneration != f.Generation || run.ClaimReleasedAt.Valid {
						t.Fatalf("same-claim checkpoint not persisted: run=%+v", run)
					}
				} else if refs != "" || run.CheckpointTip.Valid {
					t.Fatalf("refused publish changed origin or persisted a tip: refs=%s tip=%v", refs, run.CheckpointTip)
				}
			})
		}
	}
}

func shutdown3101Git(t *testing.T, dir string, input []byte, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // Fixed git operations on test-owned repositories.
	cmd.Dir = dir
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GIT_") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0")
	cmd.Stdin = bytes.NewReader(input)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, stderr.String())
	}
	return string(out)
}

func shutdown3101Pack(t *testing.T) (origin, tip string, pack []byte) {
	t.Helper()
	root := t.TempDir()
	origin, work := filepath.Join(root, "origin.git"), filepath.Join(root, "work")
	shutdown3101Git(t, root, nil, "init", "--bare", "--initial-branch=main", origin)
	shutdown3101Git(t, root, nil, "init", "--initial-branch=main", work)
	shutdown3101Git(t, work, nil, "config", "user.name", "shutdown test")
	shutdown3101Git(t, work, nil, "config", "user.email", "shutdown@example.test")
	shutdown3101Git(t, work, nil, "config", "commit.gpgsign", "false")
	write := func(content string) {
		if err := os.WriteFile(filepath.Join(work, "shutdown.txt"), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		shutdown3101Git(t, work, nil, "add", "shutdown.txt")
		shutdown3101Git(t, work, nil, "commit", "-m", "shutdown fixture")
	}
	write("base\n")
	base := strings.TrimSpace(shutdown3101Git(t, work, nil, "rev-parse", "HEAD"))
	shutdown3101Git(t, work, nil, "push", origin, "HEAD:refs/heads/main")
	write("shutdown 3101 durable work\n")
	tip = strings.TrimSpace(shutdown3101Git(t, work, nil, "rev-parse", "HEAD"))
	objects := shutdown3101Git(t, work, nil, "rev-list", "--objects", tip, "^"+base)
	pack = []byte(shutdown3101Git(t, work, []byte(objects), "pack-objects", "--stdout"))
	return
}

// #3101: stop_kind is an owner intent while running; only terminal state or
// actual authority loss refuses credentials. No input/poll endpoint is called.
func TestShutdown3101CodexReleaseHTTPLiveDB(t *testing.T) {
	for _, apiKey := range []bool{false, true} {
		for _, mutation := range []string{"queued", "terminal", "epoch", "released", "worker"} {
			for _, during := range []bool{false, true} {
				t.Run(fmt.Sprintf("api_key=%v/during=%v/%s", apiKey, during, mutation), func(t *testing.T) {
					f := workersvc.NewShutdown3101CodexFixture(t, apiKey)
					if during {
						f.DuringRelease(mutation)
					} else {
						f.Mutate(mutation)
					}
					s := shutdown3101Server(t, f)
					status, body := shutdown3101Post(t, s, f, "/codex/release", shutdown3101JSON(t, map[string]any{"capability": f.Capability}), "")
					want := http.StatusConflict
					switch mutation {
					case "queued":
						want = http.StatusOK
					case "epoch":
						want = http.StatusForbidden
					case "worker":
						want = http.StatusNotFound
					}
					if status != want {
						t.Fatalf("status=%d body=%s want=%d", status, body, want)
					}
					var result struct {
						AccessToken string `json:"access_token"`
					}
					if err := json.Unmarshal(body, &result); err != nil {
						t.Fatal(err)
					}
					if mutation == "queued" {
						if result.AccessToken != f.AccessToken {
							t.Fatal("queued cancel refused the running run's credential")
						}
					} else if result.AccessToken != "" || bytes.Contains(body, []byte(f.AccessToken)) || (f.RefreshToken != "" && bytes.Contains(body, []byte(f.RefreshToken))) {
						t.Fatalf("refusal disclosed credential: status=%d", status)
					}
				})
			}
		}
	}
}

type shutdown3101Transport struct{ target *url.URL }

func (rt shutdown3101Transport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.URL.Scheme, r.URL.Host = rt.target.Scheme, rt.target.Host
	r.Host = rt.target.Host
	return http.DefaultTransport.RoundTrip(r)
}

// #3101: authority changes inside the real HTTP OAuth exchange, before the
// provider returns fresh material. The worker makes no local observation first.
func TestShutdown3101CodexRefreshHTTPLiveDB(t *testing.T) {
	for _, during := range []bool{false, true} {
		for _, mutation := range []string{"queued", "terminal", "epoch", "released", "worker"} {
			t.Run(fmt.Sprintf("during=%v/%s", during, mutation), func(t *testing.T) {
				f := workersvc.NewShutdown3101CodexFixture(t, false)
				payload := shutdown3101JSON(t, map[string]any{"https://api.openai.com/auth": map[string]string{
					"chatgpt_account_id": f.AccountID, "chatgpt_user_id": f.ProviderUserID, "user_id": f.ProviderUserID,
				}})
				fresh := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`)) + "." + base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString([]byte("fixture-signature"))
				var hits atomic.Int32
				provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					hits.Add(1)
					var request struct {
						RefreshToken string `json:"refresh_token"`
					}
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.RefreshToken != f.RefreshToken {
						t.Error("provider did not receive the committed refresh token")
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					if during {
						f.Mutate(mutation)
					}
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]any{"access_token": fresh, "refresh_token": "rotated-" + uuid.NewString()})
				}))
				defer provider.Close()
				target, err := url.Parse(provider.URL)
				if err != nil {
					t.Fatal(err)
				}
				f.Service.SetCodexRefresh(codexauth.NewClient(codexauth.WithHTTPDoer(&http.Client{
					Transport:     shutdown3101Transport{target: target},
					CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
				})))
				if !during {
					f.Mutate(mutation)
				}
				s := shutdown3101Server(t, f)
				status, body := shutdown3101Post(t, s, f, "/codex/refresh", shutdown3101JSON(t, map[string]any{
					"capability": f.Capability, "operation_id": uuid.NewString(), "observed_generation": 0,
				}), "")
				want := http.StatusConflict
				switch mutation {
				case "queued":
					want = http.StatusOK
				case "epoch":
					want = http.StatusForbidden
				case "worker":
					want = http.StatusNotFound
				}
				if status != want {
					t.Fatalf("status=%d body=%s want=%d", status, body, want)
				}
				var result struct {
					AccessToken string `json:"access_token"`
					Outcome     string
				}
				if err := json.Unmarshal(body, &result); err != nil {
					t.Fatal(err)
				}
				if mutation == "queued" {
					if result.AccessToken != fresh || result.Outcome != "advanced" {
						t.Fatal("queued cancel blocked successful refresh")
					}
				} else if result.AccessToken != "" || bytes.Contains(body, []byte(fresh)) || bytes.Contains(body, []byte(f.RefreshToken)) {
					t.Fatal("lost authority disclosed refreshed material")
				}
				wantHits := int32(0)
				if during || mutation == "queued" {
					wantHits = 1
				}
				if hits.Load() != wantHits {
					t.Fatalf("provider calls=%d want=%d", hits.Load(), wantHits)
				}
				if wantHits == 1 {
					f.AssertCommitted(fresh)
				}
			})
		}
	}
}
