package poller

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forgesvc"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
)

func TestCrossRunDiscardETagRevalidationLiveDB(t *testing.T) {
	for _, revalidate := range []bool{true, false} {
		t.Run(map[bool]string{true: "live_304", false: "unreadable_cached_merged"}[revalidate], func(t *testing.T) {
			e, _ := newCrossRunDiscardEnv(t)
			scratch := filepath.Join("..", "..", "..", ".uzi", "scratch")
			if err := os.MkdirAll(scratch, 0750); err != nil {
				t.Fatal(err)
			}
			requests, err := os.CreateTemp(scratch, fmt.Sprintf("m3-cross-run-etag-%d-", os.Getpid()))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := requests.Close(); err != nil {
					t.Error(err)
				}
				if err := os.Remove(requests.Name()); err != nil {
					t.Error(err)
				}
			})
			role := fmt.Sprintf("m3-cross-run-etag pid=%d", os.Getpid())
			t.Logf("owned HTTP instrument request log=%s", requests.Name())
			var pulls, confirmations atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if _, err := fmt.Fprintf(requests, "%s %s %s\n", role, r.Method, r.URL.Path); err != nil {
					t.Error(err)
				}
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v3/repositories/42":
					_ = json.NewEncoder(w).Encode(map[string]any{"id": 42, "name": "r", "owner": map[string]string{"login": "o"}})
				case "/api/v3/repos/o/r/pulls/7":
					n := pulls.Add(1)
					if n == 1 {
						w.Header().Set("ETag", `"cross-run-v1"`)
						_, _ = w.Write([]byte(`{"number":7,"state":"closed","merged":true}`))
					} else if revalidate {
						if r.Header.Get("If-None-Match") != `"cross-run-v1"` {
							t.Error("fresh GET omitted cached ETag")
							w.WriteHeader(400)
							return
						}
						confirmations.Add(1)
						w.WriteHeader(http.StatusNotModified)
					} else {
						w.WriteHeader(http.StatusServiceUnavailable)
					}
				default:
					// Ordinary issue polling is independent of the fresh witness GET.
					if strings.HasSuffix(r.URL.Path, "/issues") {
						_, _ = w.Write([]byte("[]"))
						return
					}
					t.Errorf("unexpected driver request %s", r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			box, err := secretbox.New([]byte("0123456789abcdef0123456789abcdef"))
			if err != nil {
				t.Fatal(err)
			}
			e.svc = forgesvc.NewWithForgeBuilder(e.q, box, time.Second, nil, forge.New)
			sealed, err := e.svc.EncryptToken("etag-fixture-" + uuid.NewString())
			if err != nil {
				t.Fatal(err)
			}
			e.exec("UPDATE forge_connections SET forge_type='github',base_url=$2,token_ciphertext=$3 WHERE id=$1", e.conn, server.URL, sealed)
			client, err := e.svc.ForgeForConnection("github", server.URL, sealed)
			if err != nil {
				t.Fatal(err)
			}
			primed, err := client.GetMergeRequest(e.ctx, 42, 7)
			if err != nil || primed.State != forge.MRStateMerged || pulls.Load() != 1 {
				t.Fatalf("prime real driver: state=%s GETs=%d err=%v", primed.State, pulls.Load(), err)
			}
			e.svc.SetMergedPredecessorRecovery(e.q, e.recovery)
			e.engine = New(e.svc, mergedRepoStore{e.q, e.repo}, time.Minute, 10)
			e.engine.PollOnce(e.ctx)
			recorded, err := os.ReadFile(requests.Name())
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(string(recorded), role+" GET /api/v3/repos/o/r/pulls/7\n") != 2 {
				t.Fatalf("owned instrument did not record both PR GETs: %s", recorded)
			}
			want := "open"
			if revalidate {
				want = "discarded"
			}
			if e.state(e.hold) != want || pulls.Load() != 2 || (confirmations.Load() == 1) != revalidate {
				t.Fatalf("revalidation state=%s GETs=%d live304=%d", e.state(e.hold), pulls.Load(), confirmations.Load())
			}
		})
	}
}
