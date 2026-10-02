package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/anthropic"
	"github.com/vtmocanu/uzi/api/internal/codexauth"
)

type providerAnthropicFake struct {
	usage func(context.Context, []byte) (anthropic.Reading, error)
	probe func(context.Context, []byte) (anthropic.Reading, bool, error)
}

func (f providerAnthropicFake) Usage(ctx context.Context, token []byte) (anthropic.Reading, error) {
	return f.usage(ctx, token)
}
func (f providerAnthropicFake) ProbeTest(ctx context.Context, token []byte) (anthropic.Reading, bool, error) {
	return f.probe(ctx, token)
}

func anthropicHTTPFailure(status int) error {
	return &anthropic.Error{Kind: anthropic.KindHTTP, Status: status}
}

func installAnthropicTestClient(h *Handler, fake providerAnthropicFake) {
	h.secretTestOnce.Do(func() {})
	h.secretTestClients = &secretTestClients{anthropic: fake, openai: &http.Client{Timeout: 10 * time.Second}}
}

func anthropicTestState(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) (bool, int64, int, int, string, bool) {
	t.Helper()
	var marker sql.NullTime
	var generation int64
	var five, seven sql.NullInt64
	var source sql.NullString
	err := pool.QueryRow(context.Background(), `SELECT s.anthropic_rejected_at, s.anthropic_success_generation,
		r.five_hour_pct, r.seven_day_pct, r.source FROM user_secrets s
		LEFT JOIN anthropic_rate_limits r ON r.user_secret_id = s.id WHERE s.id = $1`, id).
		Scan(&marker, &generation, &five, &seven, &source)
	if err != nil {
		t.Fatal(err)
	}
	return marker.Valid, generation, int(five.Int64), int(seven.Int64), source.String, source.Valid
}

func TestSecretAnthropicProviderLiveDB(t *testing.T) {
	h, router, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	session := cliMintJWT(t, pool, owner)
	h.cfg.UsageProbe = true
	reading := anthropic.Reading{
		FiveHour: anthropic.Window{Pct: 23}, SevenDay: anthropic.Window{Pct: 67},
		Source: anthropic.SourceUsageEndpoint,
	}
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			id := seedTestConnection(t, h, pool, owner, "anthropic_token", "fixture-anthropic-"+uuid.NewString())
			path := testConnectionPath("anthropic_token", id)
			var probes atomic.Int32
			installAnthropicTestClient(h, providerAnthropicFake{
				usage: func(context.Context, []byte) (anthropic.Reading, error) {
					return anthropic.Reading{}, anthropicHTTPFailure(http.StatusUnauthorized)
				},
				probe: func(context.Context, []byte) (anthropic.Reading, bool, error) {
					probes.Add(1)
					return anthropic.Reading{}, false, anthropicHTTPFailure(status)
				},
			})
			requireTestResult(t, cookieReq(t, router, http.MethodPost, path, session, ""), http.StatusOK, "rejected", "")
			marker, generation, _, _, _, gauge := anthropicTestState(t, pool, id)
			if !marker || generation != 0 || gauge || probes.Load() != 1 {
				t.Fatalf("rejected state: marker=%v generation=%d gauge=%v probes=%d", marker, generation, gauge, probes.Load())
			}

			installAnthropicTestClient(h, providerAnthropicFake{
				usage: func(context.Context, []byte) (anthropic.Reading, error) { return reading, nil },
				probe: func(context.Context, []byte) (anthropic.Reading, bool, error) {
					t.Fatal("successful Usage must not call Messages")
					return anthropic.Reading{}, false, nil
				},
			})
			requireTestResult(t, cookieReq(t, router, http.MethodPost, path, session, ""), http.StatusOK, "ok", "")
			marker, generation, five, seven, source, gauge := anthropicTestState(t, pool, id)
			if marker || generation != 1 || !gauge || five != 23 || seven != 67 || source != anthropic.SourceUsageEndpoint {
				t.Fatalf("usage recovery: marker=%v generation=%d gauge=%v five=%d seven=%d source=%q",
					marker, generation, gauge, five, seven, source)
			}
		})
	}

	t.Run("Messages success without meter", func(t *testing.T) {
		id := seedTestConnection(t, h, pool, owner, "anthropic_token", "fixture-anthropic-"+uuid.NewString())
		cliMustExec(t, pool, `UPDATE user_secrets SET anthropic_rejected_at = now() WHERE id = $1`, id)
		cliMustExec(t, pool, `INSERT INTO anthropic_rate_limits
			(user_secret_id, user_id, five_hour_pct, seven_day_pct, source, synced_at)
			VALUES ($1, $2, 41, 53, 'usage_endpoint', now())`, id, owner)
		installAnthropicTestClient(h, providerAnthropicFake{
			usage: func(context.Context, []byte) (anthropic.Reading, error) {
				return anthropic.Reading{}, anthropicHTTPFailure(http.StatusUnauthorized)
			},
			probe: func(context.Context, []byte) (anthropic.Reading, bool, error) {
				return anthropic.Reading{}, false, nil
			},
		})
		requireTestResult(t, cookieReq(t, router, http.MethodPost, testConnectionPath("anthropic_token", id), session, ""), http.StatusOK, "ok", "")
		marker, generation, five, seven, source, gauge := anthropicTestState(t, pool, id)
		if marker || generation != 1 || !gauge || five != 41 || seven != 53 || source != anthropic.SourceUsageEndpoint {
			t.Fatalf("status-only success: marker=%v generation=%d gauge=%v five=%d seven=%d source=%q",
				marker, generation, gauge, five, seven, source)
		}
	})

	t.Run("probe disabled", func(t *testing.T) {
		h.cfg.UsageProbe = false
		id := seedTestConnection(t, h, pool, owner, "anthropic_token", "fixture-anthropic-"+uuid.NewString())
		installAnthropicTestClient(h, providerAnthropicFake{
			usage: func(context.Context, []byte) (anthropic.Reading, error) {
				return anthropic.Reading{}, anthropicHTTPFailure(http.StatusUnauthorized)
			},
			probe: func(context.Context, []byte) (anthropic.Reading, bool, error) {
				t.Fatal("Messages called with UsageProbe=false")
				return anthropic.Reading{}, false, nil
			},
		})
		requireTestResult(t, cookieReq(t, router, http.MethodPost, testConnectionPath("anthropic_token", id), session, ""),
			http.StatusOK, "inconclusive", "generic")
		marker, generation, _, _, _, gauge := anthropicTestState(t, pool, id)
		if marker || generation != 0 || gauge {
			t.Fatalf("disabled probe: marker=%v generation=%d gauge=%v", marker, generation, gauge)
		}
	})
}

func TestSecretAnthropicReplacementDuringProviderCallLiveDB(t *testing.T) {
	h, router, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	id := seedTestConnection(t, h, pool, owner, "anthropic_token", "fixture-before")
	h.cfg.UsageProbe = true
	entered, release := make(chan struct{}), make(chan struct{})
	bearer := cliMintToken(t, pool, owner, "user")
	installAnthropicTestClient(h, providerAnthropicFake{
		usage: func(context.Context, []byte) (anthropic.Reading, error) {
			close(entered)
			<-release
			return anthropic.Reading{}, anthropicHTTPFailure(http.StatusUnauthorized)
		},
		probe: func(context.Context, []byte) (anthropic.Reading, bool, error) {
			return anthropic.Reading{}, false, anthropicHTTPFailure(http.StatusUnauthorized)
		},
	})
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- bearerReqBody(router, http.MethodPost, testConnectionPath("anthropic_token", id), bearer, "")
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("Usage call did not start")
	}
	sealed, err := h.box.Seal([]byte("fixture-after"))
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	cliMustExec(t, pool, `UPDATE user_secrets SET ciphertext=$1, updated_at=updated_at + interval '1 second',
		enablement_rev=enablement_rev + 1 WHERE id=$2`, sealed, id)
	close(release)
	select {
	case rec := <-done:
		requireTestResult(t, rec, http.StatusOK, "inconclusive", "superseded")
	case <-time.After(5 * time.Second):
		t.Fatal("provider call did not return")
	}
	marker, generation, _, _, _, gauge := anthropicTestState(t, pool, id)
	if marker || generation != 0 || gauge {
		t.Fatalf("replacement changed state: marker=%v generation=%d gauge=%v", marker, generation, gauge)
	}
}

type providerCodexFake struct {
	discover func(context.Context, string) (codexauth.Identity, error)
}

func (f providerCodexFake) DiscoverIdentity(ctx context.Context, token string) (codexauth.Identity, error) {
	return f.discover(ctx, token)
}
func (providerCodexFake) ReadUsage(context.Context, string, string) (codexauth.UsageReading, error) {
	return codexauth.UsageReading{}, nil
}

func seedCodexTestAlias(t *testing.T, h *Handler, pool *pgxpool.Pool, owner uuid.UUID, status string) (uuid.UUID, string) {
	t.Helper()
	token := "fixture-access-" + uuid.NewString()
	blob, err := json.Marshal(map[string]string{"access_token": token, "refresh_token": "fixture-refresh-" + uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	id := seedTestConnection(t, h, pool, owner, "codex_auth", string(blob))
	cliMustExec(t, pool, `INSERT INTO codex_credential_state (user_secret_id, user_id, status)
		VALUES ($1,$2,$3)`, id, owner, status)
	return id, token
}

func codexTestState(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) (string, bool, int64) {
	t.Helper()
	var status string
	var account sql.NullString
	var revision int64
	if err := pool.QueryRow(context.Background(), `SELECT status, provider_account_id::text, material_revision
		FROM codex_credential_state WHERE user_secret_id=$1`, id).Scan(&status, &account, &revision); err != nil {
		t.Fatal(err)
	}
	return status, account.Valid, revision
}

func TestSecretCodexReconcileLiveDB(t *testing.T) {
	for _, initial := range []string{"staging", "failed"} {
		t.Run(initial, func(t *testing.T) {
			h, router, pool := cliLiveDB(t)
			owner := cliSeedUser(t, pool, false)
			session := cliMintJWT(t, pool, owner)
			id, token := seedCodexTestAlias(t, h, pool, owner, initial)
			identity := codexauth.Identity{
				ProviderUserID:     "provider-" + uuid.NewString(),
				WorkspaceAccountID: "workspace-" + uuid.NewString(),
			}
			var calls atomic.Int32
			h.secretTestOnce.Do(func() {})
			h.secretTestClients = &secretTestClients{codex: providerCodexFake{
				discover: func(_ context.Context, got string) (codexauth.Identity, error) {
					calls.Add(1)
					if got != token {
						t.Errorf("identity received wrong token")
					}
					return identity, nil
				},
			}, openai: &http.Client{Timeout: 10 * time.Second}}
			requireTestResult(t, cookieReq(t, router, http.MethodPost, testConnectionPath("codex_auth", id), session, ""),
				http.StatusOK, "ok", "")
			status, account, revision := codexTestState(t, pool, id)
			if status != "linked" || !account || revision != 0 || calls.Load() != 1 {
				t.Fatalf("reconcile: status=%q account=%v revision=%d calls=%d", status, account, revision, calls.Load())
			}
			var providerID, workspaceID string
			err := pool.QueryRow(context.Background(), `SELECT a.provider_user_id, a.workspace_account_id
				FROM codex_provider_account a JOIN codex_credential_state c
				ON c.provider_account_id = a.id WHERE c.user_secret_id = $1`, id).
				Scan(&providerID, &workspaceID)
			if err != nil || providerID != identity.ProviderUserID || workspaceID != identity.WorkspaceAccountID {
				t.Fatalf("linked identity = %q/%q, want %q/%q, err=%v",
					providerID, workspaceID, identity.ProviderUserID, identity.WorkspaceAccountID, err)
			}
		})
	}
}

func TestSecretCodexReplacementDuringIdentityLiveDB(t *testing.T) {
	h, router, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	token := cliMintToken(t, pool, owner, "user")
	id, _ := seedCodexTestAlias(t, h, pool, owner, "staging")
	entered, release := make(chan struct{}), make(chan struct{})
	h.secretTestOnce.Do(func() {})
	h.secretTestClients = &secretTestClients{codex: providerCodexFake{
		discover: func(context.Context, string) (codexauth.Identity, error) {
			close(entered)
			<-release
			return codexauth.Identity{
				ProviderUserID:     "provider-" + uuid.NewString(),
				WorkspaceAccountID: "workspace-" + uuid.NewString(),
			}, nil
		},
	}, openai: &http.Client{Timeout: 10 * time.Second}}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- bearerReqBody(router, http.MethodPost, testConnectionPath("codex_auth", id), token, "")
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("identity call did not start")
	}
	cliMustExec(t, pool, `UPDATE codex_credential_state SET material_revision=material_revision+1 WHERE user_secret_id=$1`, id)
	replacementBlob, err := json.Marshal(map[string]string{
		"access_token":  "replacement-access-" + uuid.NewString(),
		"refresh_token": "replacement-refresh-" + uuid.NewString(),
	})
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	sealed, err := h.box.Seal(replacementBlob)
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	cliMustExec(t, pool, `UPDATE user_secrets SET ciphertext=$1, updated_at=updated_at+interval '1 second',
		enablement_rev=enablement_rev+1 WHERE id=$2`, sealed, id)
	close(release)
	select {
	case rec := <-done:
		requireTestResult(t, rec, http.StatusOK, "inconclusive", "superseded")
	case <-time.After(5 * time.Second):
		t.Fatal("identity call did not return")
	}
	status, account, revision := codexTestState(t, pool, id)
	if status != "staging" || account || revision != 1 {
		t.Fatalf("replacement: status=%q account=%v revision=%d", status, account, revision)
	}
	var accounts int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM codex_provider_account WHERE user_id=$1`, owner).Scan(&accounts); err != nil || accounts != 0 {
		t.Fatalf("stale reconciliation left %d account rows, err=%v", accounts, err)
	}
	var persisted []byte
	if err := pool.QueryRow(context.Background(), `SELECT ciphertext FROM user_secrets WHERE id=$1`, id).Scan(&persisted); err != nil || string(persisted) != string(sealed) {
		t.Fatalf("replacement ciphertext did not persist, err=%v", err)
	}
}
