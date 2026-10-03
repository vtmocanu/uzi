package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/codexauth"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
	"github.com/vtmocanu/uzi/api/internal/jointoken"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// The only store override delays retention; all authority and transactions remain SQL-backed.
type lostReplyStore struct {
	*store.Queries
	mu         sync.Mutex
	delay      bool
	background []func()
}

func (q *lostReplyStore) SetCodexRecoverySlot(ctx context.Context, p store.SetCodexRecoverySlotParams) (int64, error) {
	q.mu.Lock()
	delay := q.delay
	q.mu.Unlock()
	if delay {
		// Check the generation and operation before quarantining the leased account.
		account, err := q.GetCodexProviderAccountByID(ctx, store.GetCodexProviderAccountByIDParams{ID: p.ID, UserID: p.UserID})
		if err != nil {
			return 0, err
		}
		if account.Generation != p.Gen || !account.CoordOperationID.Valid || account.CoordOperationID.Bytes != p.Op {
			return 0, errors.New("fixture retention fence lost")
		}
		if _, err := q.QuarantineCodexAccount(ctx, store.QuarantineCodexAccountParams{ID: p.ID, UserID: p.UserID, Op: p.Op}); err != nil {
			return 0, err
		}
		return 0, errors.New("fixture retention not released")
	}
	return q.Queries.SetCodexRecoverySlot(ctx, p)
}

func (q *lostReplyStore) dispatch(fn func()) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.background = append(q.background, fn)
}

func (q *lostReplyStore) release() {
	q.mu.Lock()
	q.delay = false
	work := q.background
	q.background = nil
	q.mu.Unlock()
	// Finite snapshot, synchronously joined. Each callback has the service's bounded retries.
	for _, fn := range work {
		fn()
	}
}

type lostReplyProvider struct {
	mu       sync.Mutex
	vlt      *vault.Vault
	owner    uuid.UUID
	identity codexauth.Identity
	inputs   []string
	discover int
}

func lostReplyAccess(n int) string  { return "access-canary-" + fmt.Sprint(n) }
func lostReplyRefresh(n int) string { return "refresh-canary-" + fmt.Sprint(n) }

func (f *lostReplyProvider) Refresh(_ context.Context, input string) (codexauth.RefreshResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := len(f.inputs) + 1
	// Record lineage labels, never credential material.
	label := "unexpected"
	for i := 0; i <= n; i++ {
		if input == lostReplyRefresh(i) {
			label = fmt.Sprint(i)
		}
	}
	f.inputs = append(f.inputs, label)
	if n == 1 {
		f.vlt.Lock(f.owner)
	}
	next := lostReplyRefresh(n)
	return codexauth.RefreshResult{
		AccessToken: lostReplyAccess(n), RefreshToken: &next,
		IdentityClaims: codexauth.FreshAccessTokenIdentityClaims{
			ChatGPTAccountID: f.identity.WorkspaceAccountID,
			ChatGPTUserID:    f.identity.ProviderUserID, AuthUserID: f.identity.ProviderUserID,
		},
	}, nil
}

func (f *lostReplyProvider) DiscoverIdentity(_ context.Context, access string) (codexauth.Identity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if access != lostReplyAccess(1) {
		return codexauth.Identity{}, errors.New("unexpected discovery lineage")
	}
	f.discover++
	return f.identity, nil
}

// This substitutes only the forge transport; custody proofs read actual Git objects.
type lostReplyForge struct {
	forgetest.BaseFake
	origin string
}

func (f *lostReplyForge) BranchHead(ctx context.Context, _ int64, branch string) (string, error) {
	return f.RefHead(ctx, 0, "refs/heads/"+branch)
}

func (f *lostReplyForge) RefHead(ctx context.Context, _ int64, ref string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", f.origin, "rev-parse", "--verify", ref+"^{commit}").Output() //nolint:gosec // G204: fixed Git binary, owned fixture directory and server-derived ref; no shell.
	if err != nil {
		return "", forge.ErrRefNotFound
	}
	return strings.TrimSpace(string(out)), nil
}

func (f *lostReplyForge) CompareAncestry(ctx context.Context, _ int64, head, candidate string) (forge.Ancestry, error) {
	err := exec.CommandContext(ctx, "git", "-C", f.origin, "merge-base", "--is-ancestor", candidate, head).Run() //nolint:gosec // G204: fixed Git binary and validated fixture commit SHAs; no shell.
	if err == nil {
		return forge.AncestryAncestor, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return forge.AncestryNotAncestor, nil
	}
	return forge.AncestryUnknown, errors.New("fixture ancestry could not be proved")
}

type lostReplyAttempt struct {
	Capability string `json:"capability"`
	Operation  string `json:"operation_id"`
	Generation int64  `json:"observed_generation"`
}

// TestCodexRefreshLostReplyE2E is deliberately outside the ordinary LiveDB sweep: Node is
// required only for the explicit combined proof, never silently skipped once opted in.
func TestCodexRefreshLostReplyE2E(t *testing.T) {
	if os.Getenv("UZI_CODEX_REFRESH_LOSTREPLY_E2E") == "" {
		t.Skip("opt in with task test:codex-refresh-lostreply-e2e")
	}
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate fixture root")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(source), "../../.."))
	if _, err := exec.LookPath("node"); err != nil {
		t.Fatal("combined proof requires Node")
	}
	if _, err := os.Stat(filepath.Join(root, "agent/node_modules/tsx/package.json")); err != nil {
		t.Fatal("combined proof requires installed agent tsx")
	}
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Fatal("combined proof requires UZI_TEST_DATABASE_URL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	// LiveDB leaves unresolved accounts sealed by other test boxes. The real global
	// survivor must see only this proof's fixtures, including on pooled transactions.
	admin, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal("cannot open fixture schema administration pool")
	}
	defer admin.Close()
	schema := "lostreply_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer func() {
		// Preserve failed custody proofs until the dedicated target destroys its DB.
		if t.Failed() {
			return
		}
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if _, err := admin.Exec(cleanup, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("cleanup fixture schema: %v", err)
		}
	}()
	// Set the startup parameter on the DSN used by BOTH goose and every pool
	// connection; a one-off SET would not cover new sessions or transactions.
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatal("cannot parse fixture database URL")
		}
		params := u.Query()
		params.Set("search_path", schema)
		u.RawQuery = params.Encode()
		dsn = u.String()
	} else {
		dsn += " search_path=" + schema
	}
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	for _, variant := range []string{"drop-first", "drop-both", "pending-retention"} {
		t.Run(variant, func(t *testing.T) {
			dir, err := os.MkdirTemp(filepath.Join(root, ".uzi/scratch"), "lostreply-")
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := os.RemoveAll(dir); err != nil {
					t.Errorf("cleanup fixture directory: %v", err)
				}
			}()
			origin := filepath.Join(dir, "origin.git")
			q := store.New(pool)
			delayed := &lostReplyStore{Queries: q, delay: variant == "pending-retention"}
			box := newHandlerTestBox(t)
			vlt := vault.New(box, q)
			svc := workersvc.New(delayed, box, workersvc.Params{RunRecoveryParkBase: time.Second, RunRecoveryMaxPark: time.Second})
			svc.SetTxBeginner(pool)
			svc.SetVault(vlt)
			svc.SetForges(settleForgeBuilder{f: &lostReplyForge{origin: origin}})
			svc.SetBackground(delayed.dispatch)
			svc.SetForgeBaseURLAllowed(func(string) bool { return true })
			// Only the forge transport is replaced: packs, ancestry, CAS and fetch-back use Git.
			svc.SetPublishFn(func(ctx context.Context, o pushbroker.Options) (pushbroker.Result, error) {
				git := func(input []byte, args ...string) (string, error) {
					cmd := exec.CommandContext(ctx, "git", append([]string{"-C", origin}, args...)...) //nolint:gosec // G204: only the fixed Git subcommands below, server-validated refs/SHAs and owned paths; no shell.
					cmd.Stdin = bytes.NewReader(input)
					out, err := cmd.CombinedOutput()
					if err != nil {
						return "", fmt.Errorf("fixture Git %s failed: %w", args[0], err)
					}
					return strings.TrimSpace(string(out)), nil
				}
				ref := "refs/uzi-checkpoints/" + o.Branch
				if _, err := git(nil, "check-ref-format", ref); err != nil {
					return pushbroker.Result{}, err
				}
				if _, err := git(o.Pack, "index-pack", "--stdin"); err != nil {
					return pushbroker.Result{}, err
				}
				if _, err := git(nil, "cat-file", "-e", o.DeclaredTip+"^{commit}"); err != nil {
					return pushbroker.Result{}, pushbroker.ErrTipMissing
				}
				old, err := git(nil, "rev-parse", "--verify", ref)
				if err != nil {
					old = strings.Repeat("0", 40)
				}
				if old == o.DeclaredTip {
					return pushbroker.Result{Ref: ref, AlreadyCurrent: true}, nil
				}
				base := old
				if base == strings.Repeat("0", 40) {
					base, err = git(nil, "rev-parse", "refs/heads/"+o.DefaultBranch)
					if err != nil {
						return pushbroker.Result{}, err
					}
				}
				if _, err := git(nil, "merge-base", "--is-ancestor", base, o.DeclaredTip); err != nil {
					return pushbroker.Result{}, pushbroker.ErrNotDescendant
				}
				if _, err := git(nil, "update-ref", ref, o.DeclaredTip, old); err != nil {
					return pushbroker.Result{}, err
				}
				verify, err := os.MkdirTemp(dir, "fetch-back-")
				if err != nil {
					return pushbroker.Result{}, err
				}
				defer func() {
					if err := os.RemoveAll(verify); err != nil {
						t.Errorf("cleanup fetch-back directory: %v", err)
					}
				}()
				for _, args := range [][]string{{"init", "--bare", verify}, {"-C", verify, "fetch", "--no-tags", origin, ref}, {"-C", verify, "cat-file", "-e", o.DeclaredTip + "^{commit}"}} {
					if err := exec.CommandContext(ctx, "git", args...).Run(); err != nil { //nolint:gosec // G204: literal fetch-back commands and owned fixture paths below; no shell.
						return pushbroker.Result{}, fmt.Errorf("fetch-back proof: %w", err)
					}
				}
				return pushbroker.Result{Ref: ref}, nil
			})
			svc.SetDeleteCheckpointFn(func(ctx context.Context, o pushbroker.DeleteOptions) error {
				return exec.CommandContext(ctx, "git", "-C", origin, "update-ref", "-d", "refs/uzi-checkpoints/"+o.Branch).Run() //nolint:gosec // G204: fixed Git binary, owned directory and server-derived branch; no shell.
			})
			owner, conn, repo, worker, run := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
			defer func() {
				delayed.release()
				// A failed proof can retain live custody. Preserve that fixture until the
				// dedicated target tears down its throwaway DB; do not bypass deletion guards.
				if t.Failed() {
					return
				}
				c, stop := context.WithTimeout(context.Background(), 5*time.Second)
				defer stop()
				if _, err := pool.Exec(c, "DELETE FROM users WHERE id=$1", owner); err != nil {
					t.Error(err)
				}
			}()
			mustExecT(ctx, t, pool, `INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')`, owner, owner.String()+"@example.test")
			pat, err := box.Seal([]byte("fixture-forge-pat"))
			if err != nil {
				t.Fatal(err)
			}
			mustExecT(ctx, t, pool, `INSERT INTO forge_connections(id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext)
				VALUES($1,$2,'gitlab','https://forge.example','bot',1,$3)`, conn, owner, pat)
			mustExecT(ctx, t, pool, `INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch,enabled)
				VALUES($1,$2,1,$3,$4,'main',true)`, repo, conn, "fixture/"+repo.String(), "https://forge.example/fixture")
			token, hash, err := jointoken.Generate()
			if err != nil {
				t.Fatal(err)
			}
			mustExecT(ctx, t, pool, `INSERT INTO workers(id,user_id,name,token_hash,status,last_heartbeat_at,protocol_capabilities)
				VALUES($1,$2,$3,$4,'online',now(),$5)`, worker, owner, worker.String(), hash,
				[]string{"codex_harness_v1", "codex_runtime_v2", "codex_completion_interlock_v1", "completion_interlock_v1", "recovery_archive_v1", "recovery_archive_v2", "credential_switch_v1", "codex_refresh_recovery_v1"})
			password := "fixture-password"
			if err := vlt.Unlock(ctx, owner, password); err != nil {
				t.Fatal(err)
			}
			login, _ := json.Marshal(map[string]string{"access_token": lostReplyAccess(0), "refresh_token": lostReplyRefresh(0)})
			sealed, err := vlt.Seal(owner, store.KindCodexAuth, login)
			if err != nil {
				t.Fatal(err)
			}
			secret, err := q.InsertCodexSecret(ctx, store.InsertCodexSecretParams{UserID: owner, Kind: store.KindCodexAuth, Label: "subscription", WantDefault: true, Ciphertext: sealed, SealedWith: store.SealedWithDEK})
			if err != nil {
				t.Fatal(err)
			}
			identity := codexauth.Identity{ProviderUserID: "provider-" + owner.String(), WorkspaceAccountID: "account-" + owner.String()}
			account, err := q.InsertCodexProviderAccount(ctx, store.InsertCodexProviderAccountParams{UserID: owner, ProviderUserID: identity.ProviderUserID, WorkspaceAccountID: identity.WorkspaceAccountID, SealedLogin: sealed, SealedWith: store.SealedWithDEK})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := q.InsertCodexCredentialState(ctx, store.InsertCodexCredentialStateParams{UserSecretID: secret.ID, UserID: owner, Status: "staging"}); err != nil {
				t.Fatal(err)
			}
			if n, err := q.LinkCodexCredentialState(ctx, store.LinkCodexCredentialStateParams{ProviderAccountID: pgconv.UUID(account.ID), UserSecretID: secret.ID, UserID: owner}); err != nil || n != 1 {
				t.Fatalf("link: %d %v", n, err)
			}
			mustExecT(ctx, t, pool, `INSERT INTO runs(id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,status,harness,auto_approve,plan_source,plan_md,completion_contract_version,milestones_frozen,completion_contract,contract_revision)
				VALUES($1,$2,$3,'issue',1770,'lost refresh reply','Implement the approved fixture change','queued','codex',true,'seeded','# Approved plan',1,'[{"id":"M3","title":"Recover lost refresh reply"}]','{"profile":"structural","revision":1,"criteria":[{"id":"M3.c1","milestone_id":"M3","text":"Recover lost refresh reply","audit":null,"finding_ids":[]}]}',1)`, run, owner, repo)
			if err := svc.FreezeCodexBinding(ctx, owner, run, secret.ID, "subscription"); err != nil {
				t.Fatal(err)
			}
			provider := &lostReplyProvider{vlt: vlt, owner: owner, identity: identity}
			svc.SetCodexRefresh(provider)
			h := &Handler{q: q, pool: pool, box: box, wsvc: svc}
			router := h.WorkerRoutes(mw.NewLimiter(10000, time.Minute, nil))
			var mu sync.Mutex
			var attempts []lostReplyAttempt
			var quarantineReplies []string
			var quarantineCodes []int
			var requestBodies []string
			permits := 0

			inspect := func() (map[string]any, error) {
				var status string
				var cause, session *string
				var slot bool
				var coord, sealing string
				var gen int64
				err := pool.QueryRow(ctx, `SELECT r.status,r.recovery_wait_cause,r.session_id,a.recovery_sealed IS NOT NULL,a.coord_state,a.sealed_with,a.generation
					FROM runs r,codex_provider_account a WHERE r.id=$1 AND a.id=$2`, run, account.ID).Scan(&status, &cause, &session, &slot, &coord, &sealing, &gen)
				if err != nil {
					return nil, err
				}
				var holds, captures, issued, consumed int
				for _, query := range []struct {
					sql string
					out *int
				}{
					{`SELECT count(*) FROM recovery_custody_holds WHERE run_id=$1 AND state='open'`, &holds},
					{`SELECT count(*) FROM recovery_captures WHERE run_id=$1 AND state='available'`, &captures},
					{`SELECT count(*) FROM run_completion_permits WHERE run_id=$1`, &issued},
					{`SELECT count(*) FROM run_completion_permits WHERE run_id=$1 AND consumed_at IS NOT NULL`, &consumed},
				} {
					if err := pool.QueryRow(ctx, query.sql, run).Scan(query.out); err != nil {
						return nil, err
					}
				}
				provider.mu.Lock()
				inputs := append([]string(nil), provider.inputs...)
				discover := provider.discover
				provider.mu.Unlock()
				mu.Lock()
				requests := permits
				mu.Unlock()
				return map[string]any{"status": status, "cause": cause, "session": session, "slot": slot, "coord": coord, "sealing": sealing, "generation": gen, "holds": holds, "captures": captures, "issued": issued, "consumed": consumed, "permit_requests": requests, "inputs": inputs, "discover": discover}, nil
			}
			control := func(action string) (map[string]any, error) {
				before, err := inspect()
				if err != nil {
					return nil, err
				}
				if action == "inspect" {
					return before, nil
				}
				if action != "recover" {
					return nil, errors.New("unknown fixture control")
				}
				if before["status"] != "recovery_wait" || before["issued"] != 0 || before["permit_requests"] != 0 || before["holds"] == 0 || vlt.Unlocked(owner) {
					return nil, errors.New("locked pre-sweep park/custody/authority invariant failed")
				}
				mu.Lock()
				equal := len(attempts) == 2 && reflect.DeepEqual(attempts[0], attempts[1])
				if len(quarantineReplies) != 2 || len(quarantineCodes) != 2 || quarantineCodes[0] != 409 || quarantineCodes[1] != 409 {
					equal = false
				}
				if equal {
					var first, second map[string]string
					if json.Unmarshal([]byte(quarantineReplies[0]), &first) != nil || json.Unmarshal([]byte(quarantineReplies[1]), &second) != nil {
						equal = false
					}
					if variant == "pending-retention" {
						equal = equal && reflect.DeepEqual(first, map[string]string{"error": codexErrRefreshContended}) && reflect.DeepEqual(second, map[string]string{"error": codexErrCredUnavailable})
					} else {
						want := map[string]string{"error": codexErrVaultLocked, "reason": codexReasonVaultLocked}
						equal = equal && reflect.DeepEqual(first, want) && reflect.DeepEqual(second, want)
					}
				}
				mu.Unlock()
				if !equal {
					return nil, errors.New("refresh retry tuple was not identical")
				}
				if variant == "pending-retention" {
					if before["slot"] != false || before["coord"] != "quarantined" {
						return nil, errors.New("pending retention was already durable")
					}
					delayed.release()
				}
				durable, err := q.GetCodexProviderAccountByID(ctx, store.GetCodexProviderAccountByIDParams{ID: account.ID, UserID: owner})
				if err != nil {
					return nil, err
				}
				if !durable.RecoveryCause.Valid || durable.RecoveryCause.String != "vault_locked" {
					return nil, errors.New("durable recovery cause missing after retention")
				}
				if err := vlt.Unlock(ctx, owner, password); err != nil {
					return nil, err
				}
				unlocked, err := inspect()
				if err != nil {
					return nil, err
				}
				if unlocked["status"] != "recovery_wait" || unlocked["slot"] != true || unlocked["generation"] != int64(0) || unlocked["issued"] != 0 || unlocked["holds"] != before["holds"] || unlocked["captures"] != before["captures"] || unlocked["permit_requests"] != 0 {
					return nil, errors.New("unlocked pre-sweep account/run advanced prematurely")
				}
				if n, err := svc.SweepUnresolvedCodexRefresh(ctx); err != nil || n != 1 {
					return nil, fmt.Errorf("actual sweep: %d %w", n, err)
				}
				after, err := inspect()
				if err != nil {
					return nil, err
				}
				if after["generation"] != int64(1) || after["sealing"] != store.SealedWithDEK || after["discover"] != 1 || after["slot"] != false {
					return nil, errors.New("identity discovery/DEK reseal/promotion invariant failed")
				}
				if _, err := pool.Exec(ctx, `UPDATE runs SET recovery_retry_not_before=now()-interval '1 minute',updated_at=now()-interval '3 hours' WHERE id=$1`, run); err != nil {
					return nil, err
				}
				if _, err := q.PromoteRecoveryWaitRuns(ctx, pgconv.Time(time.Now())); err != nil {
					return nil, err
				}
				return inspect()
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.URL.Path, "/fixture/") {
					if r.Header.Get("Authorization") != "Bearer "+token {
						http.Error(w, "unauthorized", http.StatusForbidden)
						return
					}
					state, err := control(strings.TrimPrefix(r.URL.Path, "/fixture/"))
					if err != nil {
						http.Error(w, err.Error(), 500)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(state)
					return
				}
				body, err := io.ReadAll(io.LimitReader(r.Body, 2<<20))
				if err != nil {
					http.Error(w, "read", 500)
					return
				}
				r.Body = io.NopCloser(bytes.NewReader(body))
				mu.Lock()
				if strings.HasSuffix(r.URL.Path, "/completion/permit") {
					permits++
				}
				if !strings.Contains(r.URL.Path, "/archives/") {
					requestBodies = append(requestBodies, string(body))
				}
				index := 0
				if strings.HasSuffix(r.URL.Path, "/codex/refresh") {
					var a lostReplyAttempt
					if err := json.Unmarshal(body, &a); err == nil {
						attempts = append(attempts, a)
						index = len(attempts)
					}
				}
				mu.Unlock()
				if index > 0 && index <= 2 {
					rec := httptest.NewRecorder()
					router.ServeHTTP(rec, r)
					mu.Lock()
					quarantineReplies = append(quarantineReplies, rec.Body.String())
					quarantineCodes = append(quarantineCodes, rec.Code)
					mu.Unlock()
					// Only after the real handler has returned, including its durable write/quarantine.
					if index == 1 || variant == "drop-both" {
						c, _, err := w.(http.Hijacker).Hijack()
						if err == nil {
							_ = c.Close()
						}
						return
					}
					for key, values := range rec.Header() {
						w.Header()[key] = values
					}
					w.WriteHeader(rec.Code)
					_, _ = w.Write(rec.Body.Bytes())
					return
				}
				router.ServeHTTP(w, r)
			}))
			defer server.Close()
			cfg, _ := json.Marshal(map[string]any{"url": server.URL, "token": token, "run": run.String(), "variant": variant, "origin": origin, "scratch": dir})
			cmd := exec.CommandContext(ctx, "node", "--import", "tsx", "test/fixtures/codex-refresh-lostreply-e2e.ts")
			cmd.Dir = filepath.Join(root, "agent")
			cmd.Env = append(os.Environ(), "UZI_LOSTREPLY_FIXTURE="+string(cfg), "TMPDIR="+dir)
			out, err := cmd.CombinedOutput()
			// Keep raw output for leak assertions, but never print fixture credentials,
			// including on a failing assertion or subprocess error.
			printable := strings.ReplaceAll(string(out), token, "[redacted]")
			mu.Lock()
			for _, attempt := range attempts {
				if attempt.Capability != "" {
					printable = strings.ReplaceAll(printable, attempt.Capability, "[redacted]")
				}
			}
			mu.Unlock()
			provider.mu.Lock()
			for i := 0; i <= len(provider.inputs); i++ {
				printable = strings.ReplaceAll(printable, lostReplyAccess(i), "[redacted]")
				printable = strings.ReplaceAll(printable, lostReplyRefresh(i), "[redacted]")
			}
			provider.mu.Unlock()
			t.Logf("worker fixture: %s", printable)
			if err != nil {
				t.Fatalf("worker fixture exit: %v", err)
			}
			state, err := inspect()
			if err != nil {
				t.Fatal(err)
			}
			if state["status"] != "completed" || state["consumed"] != 1 || state["holds"] != 0 {
				t.Fatalf("completion/custody result: %v", state)
			}
			mu.Lock()
			seen := map[string]bool{attempts[0].Operation: true}
			later := append([]lostReplyAttempt(nil), attempts[2:]...)
			mu.Unlock()
			for i, attempt := range later {
				if attempt.Operation == "" || seen[attempt.Operation] || attempt.Generation != int64(i+1) {
					t.Error("later boundary reused an operation or wrong generation")
				}
				seen[attempt.Operation] = true
			}
			provider.mu.Lock()
			if len(provider.inputs) != len(later)+1 {
				t.Error("refresh requests and provider lineage diverged")
			}
			for i, label := range provider.inputs {
				if label != fmt.Sprint(i) {
					t.Errorf("refresh lineage %d used %s", i, label)
				}
			}
			provider.mu.Unlock()
			var feed string
			if err := pool.QueryRow(ctx, `SELECT coalesce(string_agg(payload::text,' '),'') FROM run_messages WHERE run_id=$1`, run).Scan(&feed); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(feed, `"status": "failed"`) || strings.Contains(feed, `"status":"failed"`) {
				t.Error("terminal FAILED appeared in state feed")
			}
			mu.Lock()
			for _, attempt := range attempts {
				if attempt.Capability != "" && strings.Contains(feed+string(out), attempt.Capability) {
					t.Error("capability escaped to feed or output")
				}
			}
			if strings.Contains(feed+string(out), token) {
				t.Error("worker token escaped to feed or output")
			}
			surfaces := strings.Join(quarantineReplies, " ") + strings.Join(requestBodies, " ") + feed + string(out)
			mu.Unlock()
			for i := 0; i < 10; i++ {
				for _, secret := range []string{lostReplyAccess(i), lostReplyRefresh(i)} {
					if strings.Contains(surfaces, secret) {
						t.Error("credential canary escaped to non-credential surface")
					}
				}
			}
		})
	}
}
