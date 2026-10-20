package workersvc_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/codexauth"
	"github.com/vtmocanu/uzi/api/internal/codexusagepoller"
	"github.com/vtmocanu/uzi/api/internal/notifysvc"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Keep global engine discovery scoped to this fixture; all authority and snapshot
// operations still use the embedded real store. No staged aliases need reconciliation.
type earlyResetUserStore struct {
	*store.Queries
	user uuid.UUID
}

func (s earlyResetUserStore) ListLinkedCodexAccountsToPoll(ctx context.Context) ([]store.ListLinkedCodexAccountsToPollRow, error) {
	rows, err := s.Queries.ListLinkedCodexAccountsToPoll(ctx)
	var own []store.ListLinkedCodexAccountsToPollRow
	for _, r := range rows {
		if r.UserID == s.user {
			own = append(own, r)
		}
	}
	return own, err
}
func (s earlyResetUserStore) ListStagedCodexAliases(ctx context.Context) ([]store.ListStagedCodexAliasesRow, error) {
	rows, err := s.Queries.ListStagedCodexAliases(ctx)
	var own []store.ListStagedCodexAliasesRow
	for _, r := range rows {
		if r.UserID == s.user {
			own = append(own, r)
		}
	}
	return own, err
}

type earlyResetUsageHTTP struct {
	mu        sync.Mutex
	body      string
	calls     int
	workspace string
}

func (d *earlyResetUsageHTTP) Do(r *http.Request) (*http.Response, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if r.Method != http.MethodGet || r.URL.Path != "/backend-api/wham/usage" || r.Header.Get("ChatGPT-Account-Id") != d.workspace {
		return nil, fmt.Errorf("unexpected usage request")
	}
	d.calls++
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(d.body))}, nil
}

type earlyResetSlackRecord struct {
	mu      sync.Mutex
	users   []uuid.UUID
	renders []notifysvc.SlackRender
}

func (s *earlyResetSlackRecord) PublishNotification(u uuid.UUID, r notifysvc.SlackRender) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.users = append(s.users, u)
	s.renders = append(s.renders, r)
}

// TestCodexEarlyResetAuxiliaryProvenanceLiveDB composes the real HTTP decoder,
// collector, engine, SQL fences, and notification service without provider egress.
func TestCodexEarlyResetAuxiliaryProvenanceLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	q := store.New(pool)
	box, err := secretbox.New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := box.Seal([]byte(`{"access_token":"synthetic-access","refresh_token":"synthetic-refresh"}`))
	if err != nil {
		t.Fatal(err)
	}
	user, account := uuid.New(), uuid.New()
	providerUser, workspace := "principal-"+uuid.NewString(), "workspace-"+uuid.NewString()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO users (id,email,password_hash,notify_early_limit_reset) VALUES ($1,$2,'x',true)`, user, "early-reset-"+uuid.NewString()+"@example.test")
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		if _, err := pool.Exec(cleanupCtx, "DELETE FROM users WHERE id=$1", user); err != nil {
			t.Error(err)
		}
	})
	exec(`INSERT INTO codex_provider_account (id,user_id,provider_user_id,workspace_account_id,sealed_login,sealed_with)
 VALUES ($1,$2,$3,$4,$5,'master')`, account, user, providerUser, workspace, sealed)
	for i := 0; i < 2; i++ {
		alias := uuid.New()
		exec(`INSERT INTO user_secrets (id,user_id,kind,label,ciphertext,sealed_with) VALUES ($1,$2,'codex_auth',$3,$4,'master')`, alias, user, fmt.Sprintf("alias-%d", i), sealed)
		exec(`INSERT INTO codex_credential_state (user_secret_id,user_id,status,provider_account_id) VALUES ($1,$2,'linked',$3)`, alias, user, account)
	}
	svc := workersvc.New(q, box, workersvc.Params{})
	doer := &earlyResetUsageHTTP{workspace: workspace}
	svc.SetCodexRefresh(codexauth.NewClient(codexauth.WithHTTPDoer(doer)))
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	slack := &earlyResetSlackRecord{}
	notifier := notifysvc.New(q, slack, 0, logger)
	engine := codexusagepoller.New(earlyResetUserStore{Queries: q, user: user}, svc, nil, time.Minute, logger)
	engine.SetNotifier(notifier)
	reset := time.Now().Add(48 * time.Hour).Unix()
	sequence := []struct {
		aux    bool
		pct    int
		alerts int
	}{{true, 20, 0}, {true, 0, 0}, {false, 20, 0}, {false, 0, 1}, {false, 0, 1}}
	for i, step := range sequence {
		rate := fmt.Sprintf(`{"primary_window":{"limit_window_seconds":604800,"reset_at":%d,"used_percent":%d}}`, reset, step.pct)
		field := `"rate_limit":` + rate
		if step.aux {
			field = `"additional_rate_limits":[{"limit_name":"codex","rate_limit":` + rate + `}]`
		}
		doer.mu.Lock()
		doer.body = fmt.Sprintf(`{"user_id":%q,"account_id":%q,%s}`, providerUser, workspace, field)
		doer.mu.Unlock()
		engine.Boot(ctx)
		doer.mu.Lock()
		calls := doer.calls
		doer.mu.Unlock()
		if calls != i+1 {
			t.Fatalf("step %d: %d HTTP calls; two aliases must collect once per account", i, calls)
		}
		rows, err := q.GetCodexAccountRateLimitsForUser(ctx, user)
		if err != nil || len(rows) != 1 {
			t.Fatalf("step %d: snapshot rows=%v err=%v", i, rows, err)
		}
		var buckets []apitypes.CodexRateLimitBucketDTO
		if err := json.Unmarshal(rows[0].Buckets, &buckets); err != nil {
			t.Fatal(err)
		}
		if len(buckets) != 1 || buckets[0].ID != "codex" || buckets[0].Primary == nil || buckets[0].Primary.UsedPercent == nil || *buckets[0].Primary.UsedPercent != float64(step.pct) {
			t.Fatalf("step %d: accepted snapshot=%+v", i, buckets)
		}
		wantDisplay := ""
		if step.aux {
			wantDisplay = "codex"
		}
		if buckets[0].DisplayName != wantDisplay || !rows[0].LastSuccessAt.Valid || !rows[0].AttemptStatus.Valid || rows[0].AttemptStatus.String != "ok" {
			t.Fatalf("step %d: provenance/accepted snapshot=%+v", i, rows[0])
		}
		var notifications int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM notifications WHERE user_id=$1 AND kind=$2", user, notifysvc.KindEarlyLimitReset).Scan(&notifications); err != nil {
			t.Fatal(err)
		}
		slack.mu.Lock()
		renders := append([]notifysvc.SlackRender(nil), slack.renders...)
		users := append([]uuid.UUID(nil), slack.users...)
		slack.mu.Unlock()
		if notifications != step.alerts || len(renders) != step.alerts {
			t.Fatalf("step %d: rows=%d Slack=%d want=%d", i, notifications, len(renders), step.alerts)
		}
		if step.alerts == 1 {
			f := renders[0].CodexAccount
			if f == nil || users[0] != user || f.OwnerID != user || f.AccountID != account || f.Generation != 0 || f.CredentialRevision != 0 || strings.Count(f.EnablementSig, ",") != 1 || renders[0].DeliveryTag != notifysvc.DeliveryTagCodexEarlyReset {
				t.Fatalf("account fence=%+v", f)
			}
			current, err := q.IsCodexEarlyResetCurrent(ctx, f.Params())
			if err != nil || !current {
				t.Fatalf("real notification fence current=%v err=%v", current, err)
			}
		}
	}
}
