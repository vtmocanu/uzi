package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/jointoken"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Explicitly outside LiveDB$: this composition requires installed agent dependencies.
// UZI_STALE_WORKER_CANCEL_DISABLE_CONTROL=1 is an opt-in mutation: the empty poll
// case MUST fail, rather than accepting another terminal path as evidence.
func TestStaleWorkerCancelE2E(t *testing.T) {
	if os.Getenv("UZI_STALE_WORKER_CANCEL_E2E") != "1" {
		t.Skip("opt in with task test:stale-worker-cancel-e2e")
	}
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate fixture root")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(source), "../../.."))
	if _, err := exec.LookPath("node"); err != nil {
		t.Fatal("Node required")
	}
	if _, err := os.Stat(filepath.Join(root, "agent/node_modules/tsx/package.json")); err != nil {
		t.Fatal("installed agent tsx required")
	}
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Fatal("UZI_TEST_DATABASE_URL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	admin, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal("cannot open schema administration pool")
	}
	defer admin.Close()
	schema := "stale_cancel_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if t.Failed() {
			return
		} // Preserve failed custody evidence for the database owner.
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if _, err := admin.Exec(cleanup, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Error(err)
		}
	}()
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatal("invalid database URL")
		}
		p := u.Query()
		p.Set("search_path", schema)
		u.RawQuery = p.Encode()
		dsn = u.String()
	} else {
		dsn += " search_path=" + schema
	}
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal("cannot open isolated pool")
	}
	defer pool.Close()
	for _, variant := range []string{"plan-empty", "question-empty", "stale-generation", "ordinary-outage"} {
		t.Run(variant, func(t *testing.T) {
			dir, err := os.MkdirTemp(filepath.Join(root, ".uzi/scratch"), "stale-cancel-")
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if !t.Failed() {
					if err := os.RemoveAll(dir); err != nil {
						t.Error(err)
					}
				}
			}()
			q := store.New(pool)
			box := newHandlerTestBox(t)
			svc := workersvc.New(q, box, workersvc.Params{WorkerHeartbeatStale: time.Minute, WorkerHeartbeatInterval: 10 * time.Millisecond, ActiveSnapshotMaxEntries: 256, WorkerOutboxMaxPending: 32, TerminalPendingLease: time.Hour})
			svc.SetTxBeginner(pool)
			svc.SetForgeBaseURLAllowed(func(string) bool { return true })
			owner, conn, repo, worker, first, second := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
			sql := func(query string, args ...any) error { _, err := pool.Exec(ctx, query, args...); return err }
			must := func(query string, args ...any) {
				t.Helper()
				if err := sql(query, args...); err != nil {
					t.Fatal(err)
				}
			}
			must("INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')", owner, owner.String()+"@example.test")
			pat, err := box.Seal([]byte("fixture-forge-secret"))
			if err != nil {
				t.Fatal(err)
			}
			model, err := box.Seal([]byte("fixture-model-secret"))
			if err != nil {
				t.Fatal(err)
			}
			must("INSERT INTO forge_connections(id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext) VALUES($1,$2,'gitlab','https://forge.example','bot',1,$3)", conn, owner, pat)
			must("INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch,enabled) VALUES($1,$2,1,$3,'https://forge.example/fixture','main',true)", repo, conn, "fixture")
			must("INSERT INTO user_secrets(id,user_id,kind,label,is_default,ciphertext,sealed_with) VALUES($1,$2,'anthropic_token','fixture',true,$3,'master')", uuid.New(), owner, model)
			token, hash, err := jointoken.Generate()
			if err != nil {
				t.Fatal(err)
			}
			must("INSERT INTO workers(id,user_id,name,token_hash) VALUES($1,$2,$3,$4)", worker, owner, worker.String(), hash)
			for i, id := range []uuid.UUID{first, second} {
				must("INSERT INTO runs(id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,status,claim_generation,created_at) VALUES($1,$2,$3,'issue',$4,'fixture','fixture','queued',6,now()+$5*interval '1 second')", id, owner, repo, 290710+i, i)
			}
			h := &Handler{q: q, pool: pool, box: box, wsvc: svc}
			routes := h.WorkerRoutes(mw.NewLimiter(100000, time.Minute, nil))
			ownerRoutes := chi.NewRouter()
			ownerRoutes.Use(func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					next.ServeHTTP(w, r.WithContext(mw.ContextWithUser(r.Context(), store.User{ID: owner})))
				})
			})
			ownerRoutes.Post("/api/runs/{id}/inputs", h.CreateRunInput)
			var mu sync.Mutex
			outage, stale := false, variant == "stale-generation"
			polls, inputPolls, beats, blocked, receiptCalls, forbidden := 0, 0, 0, 0, 0, 0
			custodyBefore := ""
			custody := func() (string, error) {
				var s string
				err := pool.QueryRow(ctx, `SELECT jsonb_build_object(
      'worker',worker_id,'generation',claim_generation,'tip',checkpoint_tip,'tip_at',checkpoint_tip_at,
      'holds',(SELECT jsonb_agg(to_jsonb(h) ORDER BY h.id) FROM recovery_custody_holds h WHERE run_id=$1),
      'captures',(SELECT jsonb_agg(to_jsonb(c) ORDER BY c.id) FROM recovery_captures c WHERE run_id=$1),
      'retentions',(SELECT jsonb_agg(to_jsonb(c)) FROM checkpoint_retentions c WHERE run_id=$1),
      'inputs',(SELECT jsonb_agg(to_jsonb(i) ORDER BY i.id) FROM run_user_inputs i WHERE run_id=$1))::text FROM runs WHERE id=$1`, first).Scan(&s)
				return s, err
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Serialize outage transitions with real heartbeat writes, so no in-flight
				// heartbeat can freshen the row after the fixture ages it.
				mu.Lock()
				defer mu.Unlock()
				if r.Header.Get("Authorization") != "Bearer "+token {
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
				p := r.URL.Path
				if strings.HasPrefix(p, "/fixture/") {
					action := strings.TrimPrefix(p, "/fixture/")
					var err error
					switch action {
					case "outage":
						outage = true
						err = sql("UPDATE workers SET last_heartbeat_at=now()-interval '1 hour' WHERE id=$1", worker)
						if err == nil {
							custodyBefore, err = custody()
						}
					case "reconnect":
						outage = false
					case "exact":
						stale = false
						polls = 0
					case "inspect":
					default:
						http.Error(w, "unknown control", http.StatusNotFound)
						return
					}
					if err != nil {
						http.Error(w, "fixture transition failed", http.StatusInternalServerError)
						return
					}
					var status, workerID string
					var generation int64
					var inputs, holds int
					err = pool.QueryRow(ctx, "SELECT status,worker_id::text,claim_generation,(SELECT count(*) FROM run_user_inputs WHERE run_id=$1),(SELECT count(*) FROM recovery_custody_holds WHERE run_id=$1 AND state='open') FROM runs WHERE id=$1", first).Scan(&status, &workerID, &generation, &inputs, &holds)
					nowCustody, e := custody()
					if err != nil || e != nil {
						http.Error(w, "fixture inspection failed", http.StatusInternalServerError)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]any{"status": status, "worker": workerID, "generation": generation, "inputs": inputs, "holds": holds, "custody_unchanged": custodyBefore == "" || nowCustody == custodyBefore, "polls": polls, "input_polls": inputPolls, "blocked": blocked, "beats": beats, "receipts": receiptCalls, "forbidden": forbidden})
					return
				}
				if strings.HasPrefix(p, "/api/runs/") {
					ownerRoutes.ServeHTTP(w, r)
					return
				}
				inputGet := p == "/api/worker/runs/"+first.String()+"/inputs" && r.Method == http.MethodGet
				heartbeat := p == "/api/worker/heartbeat"
				if strings.Contains(p, "/inputs/") && r.Method != http.MethodGet {
					receiptCalls++
				}
				if r.Method == http.MethodDelete || strings.Contains(p, "/publish") || strings.Contains(p, "publication") || strings.Contains(p, "/report-retir") {
					forbidden++
				}
				if outage && strings.HasPrefix(p, "/api/worker/") {
					blocked++
					http.Error(w, "fixture outage", http.StatusServiceUnavailable)
					return
				}
				if inputGet {
					rec := httptest.NewRecorder()
					routes.ServeHTTP(rec, r)
					if rec.Code == http.StatusOK {
						inputPolls++
						var body map[string]json.RawMessage
						if json.Unmarshal(rec.Body.Bytes(), &body) == nil {
							if raw, ok := body["terminal_cancelled"]; ok {
								polls++
								if os.Getenv("UZI_STALE_WORKER_CANCEL_DISABLE_CONTROL") == "1" {
									delete(body, "terminal_cancelled")
								} else if stale {
									var control struct {
										Run        string `json:"run_id"`
										Generation int64  `json:"generation"`
									}
									if json.Unmarshal(raw, &control) == nil {
										control.Generation--
										body["terminal_cancelled"], _ = json.Marshal(control)
									}
								}
								for k, v := range rec.Header() {
									w.Header()[k] = v
								}
								w.WriteHeader(rec.Code)
								_ = json.NewEncoder(w).Encode(body)
								return
							}
						}
					}
					for k, v := range rec.Header() {
						w.Header()[k] = v
					}
					w.WriteHeader(rec.Code)
					_, _ = w.Write(rec.Body.Bytes())
					return
				}
				if heartbeat {
					rec := httptest.NewRecorder()
					routes.ServeHTTP(rec, r)
					if rec.Code == http.StatusOK {
						beats++
					}
					for k, v := range rec.Header() {
						w.Header()[k] = v
					}
					w.WriteHeader(rec.Code)
					_, _ = w.Write(rec.Body.Bytes())
					return
				}
				routes.ServeHTTP(w, r)
			}))
			defer server.Close()
			cfg, _ := json.Marshal(map[string]any{"url": server.URL, "token": token, "first": first, "second": second, "worker": worker, "variant": variant, "scratch": dir})
			cmd := exec.CommandContext(ctx, "node", "--import", "tsx", "--test", "--test-timeout=120000", "test/fixtures/stale-worker-cancel-e2e.ts")
			cmd.Dir = filepath.Join(root, "agent")
			cmd.Env = append(os.Environ(), "UZI_STALE_CANCEL_FIXTURE="+string(cfg), "TMPDIR="+dir)
			out, err := cmd.CombinedOutput()
			safe := strings.ReplaceAll(string(out), token, "[redacted]")
			safe = strings.ReplaceAll(safe, "fixture-forge-secret", "[redacted]")
			safe = strings.ReplaceAll(safe, "fixture-model-secret", "[redacted]")
			t.Log(safe)
			if err != nil {
				t.Fatalf("combined fixture failed: %v", err)
			}
		})
	}
}
