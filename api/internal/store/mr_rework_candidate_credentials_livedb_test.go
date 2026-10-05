package store_test

import (
	"context"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestMRReworkCandidateCredentialsLiveDB pins #2084 M1's source-harness gate
// after newest-per-branch selection. Each case has its own owner and repo.
func TestMRReworkCandidateCredentialsLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()
	q := store.New(pool)

	for _, tc := range []struct {
		name         string
		olderHarness string
		harness      string
		anthropic    string
		codexKey     bool
		admitted     bool
	}{
		{name: "Codex-only owner with default static API key", harness: "codex", codexKey: true, admitted: true},
		{name: "Claude disabled token only", harness: "claude", anthropic: "disabled"},
		{name: "Claude enabled token", harness: "claude", anthropic: "enabled", admitted: true},
		{name: "Claude no token", harness: "claude"},
		{name: "older Claude newer Codex without Anthropic", olderHarness: "claude", harness: "codex", codexKey: true, admitted: true},
		{name: "older Codex newer Claude without Anthropic", olderHarness: "codex", harness: "claude", codexKey: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exec := func(sql string, args ...any) {
				t.Helper()
				if _, err := pool.Exec(ctx, sql, args...); err != nil {
					t.Fatalf("exec %q: %v", sql, err)
				}
			}
			userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
			exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'fixture')`,
				userID, "mr-credentials-"+userID.String()+"@example.test")
			exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
			      VALUES ($1, $2, 'gitlab', 'https://forge.example.test', 'bot', 777, $3)`,
				connID, userID, []byte("fixture-forge"))
			exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
			      VALUES ($1, $2, 1, $3, $4, 'main', true)`,
				repoID, connID, "fixtures/"+repoID.String(), "https://forge.example.test/"+repoID.String())

			if tc.codexKey {
				secretID := uuid.New()
				exec(`INSERT INTO user_secrets (id, user_id, kind, label, is_default, ciphertext, sealed_with)
				      VALUES ($1, $2, 'openai_api_key', $3, true, $4, 'master')`,
					secretID, userID, "static-"+secretID.String(), []byte("fixture-openai"))
				if _, err := q.InsertCodexCredentialState(ctx, store.InsertCodexCredentialStateParams{
					UserSecretID: secretID, UserID: userID, Status: "static",
				}); err != nil {
					t.Fatalf("insert static Codex state: %v", err)
				}
			}
			if tc.anthropic != "" {
				var disabledAt *time.Time
				if tc.anthropic == "disabled" {
					when := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
					disabledAt = &when
				}
				exec(`INSERT INTO user_secrets (id, user_id, kind, label, is_default, ciphertext, sealed_with, disabled_at)
				      VALUES ($1, $2, 'anthropic_token', $3, $4, $5, 'master', $6)`,
					uuid.New(), userID, "anthropic-"+uuid.NewString(), tc.anthropic == "enabled", []byte("fixture-anthropic"), disabledAt)
			}

			ref := "agent/credentials-" + uuid.NewString()
			seedSource := func(harness string, createdAt time.Time) uuid.UUID {
				t.Helper()
				id := uuid.New()
				exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description,
				                        branch, mr_iid, mr_state, status, harness, created_at)
				      VALUES ($1, $2, $3, 'issue', 9, 'fixture', 'fixture', $4, 90, 'opened', 'completed', $5, $6)`,
					id, userID, repoID, ref, harness, createdAt)
				return id
			}
			older := time.Date(2026, time.January, 2, 0, 0, 0, 0, time.UTC)
			if tc.olderHarness != "" {
				seedSource(tc.olderHarness, older)
			}
			newestID := seedSource(tc.harness, older.Add(time.Hour))
			candidates, err := q.ListMRReworkCandidates(ctx, repoID)
			if err != nil {
				t.Fatalf("ListMRReworkCandidates: %v", err)
			}
			got := map[string]uuid.UUID{}
			for _, candidate := range candidates {
				if !candidate.Ref.Valid {
					t.Fatalf("candidate has null ref: %+v", candidate)
				}
				if _, duplicate := got[candidate.Ref.String]; duplicate {
					t.Fatalf("duplicate candidate for ref %q", candidate.Ref.String)
				}
				got[candidate.Ref.String] = candidate.SourceRunID
			}
			want := map[string]uuid.UUID{}
			if tc.admitted {
				want[ref] = newestID
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("candidate ref/source set = %v, want %v", got, want)
			}
		})
	}
}
