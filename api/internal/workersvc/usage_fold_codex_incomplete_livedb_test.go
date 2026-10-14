package workersvc

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestUsageFoldCodexIncompleteClaimLiveDB checks the existing persisted fold
// against the shared executor emission fixture, including same-key conflicts and
// the retained partial cost of a distinct model.
func TestUsageFoldCodexIncompleteClaimLiveDB(t *testing.T) {
	raw, err := os.ReadFile("../../../fixtures/run-usage/codex-incomplete-claim.json")
	if err != nil {
		t.Fatalf("read incomplete claim fixture: %v", err)
	}
	var fixtures map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatalf("decode fixture envelope: %v", err)
	}
	for _, key := range []string{"earlierMetered", "incompleteRootDistinctChild", "incompleteRootSameModelChild", "zeroIncomplete"} {
		payload, ok := fixtures[key]
		if !ok {
			t.Fatalf("missing fixture %q", key)
		}
		// Reasoning is included in outputTokens; run_usage has no separate
		// reasoning column. Check this fixture's explicit zero reasoning values
		// without modifying the RawMessage passed to foldUsageFrames.
		var frame struct {
			Event      string `json:"event"`
			ModelUsage map[string]struct {
				Reasoning *int64 `json:"reasoningOutputTokens"`
			} `json:"modelUsage"`
		}
		if err := json.Unmarshal(payload, &frame); err != nil {
			t.Fatalf("read fixture %q reasoning: %v", key, err)
		}
		if frame.Event != "result" || len(frame.ModelUsage) == 0 {
			t.Fatalf("fixture %q is not a populated result frame", key)
		}
		for model, usage := range frame.ModelUsage {
			if usage.Reasoning == nil || *usage.Reasoning != 0 {
				t.Fatalf("fixture %q model %q must explicitly report zero reasoning", key, model)
			}
		}
	}

	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; PostgreSQL fold assertions were not run")
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

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("fold-incomplete-%s@e2e", userID))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)

	type counters struct {
		input, output, cacheRead, cacheCreation int64
	}
	type modelState struct {
		model, status, cost string
		tokens              counters
	}
	type persistedRow struct {
		modelState
		session, harness, basis string
		epoch, lineage          int32
		legacyGeneration        bool
	}
	type totalState struct {
		tokens counters
		status string
		cost   float64
	}
	type snapshot struct {
		rows  []persistedRow
		total totalState
	}
	earlier := []modelState{{"gpt-6-astra", "metered", "0.002000", counters{input: 100, output: 20}}}
	sameModel := []modelState{{"gpt-6-astra", "unreported", "0.000000", counters{input: 200, output: 100}}}
	zero := []modelState{{"gpt-6-astra", "unreported", "0.000000", counters{input: 100, output: 20}}}
	distinct := []modelState{
		{"gpt-6-astra", "unreported", "0.000000", counters{input: 100, output: 20}},
		{"gpt-6.1-sol", "metered", "0.001400", counters{input: 200, output: 100}},
	}
	for i, tc := range []struct {
		name       string
		incomplete string
		want       []modelState
		total      totalState
		increase   bool
	}{
		{"distinct_model_partial_cost", "incompleteRootDistinctChild", distinct, totalState{counters{input: 300, output: 120}, "unreported", 0.0014}, false},
		{"same_model_increased_high_water", "incompleteRootSameModelChild", sameModel, totalState{counters{input: 200, output: 100}, "unreported", 0}, false},
		{"zero_lower_bound_retains_high_water", "zeroIncomplete", zero, totalState{counters{input: 100, output: 20}, "unreported", 0}, false},
		{"increase_after_zero_incomplete", "zeroIncomplete", zero, totalState{counters{input: 100, output: 20}, "unreported", 0}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runID, sessionID := uuid.New(), uuid.NewString()
			exec(`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind, harness, session_id)
			      VALUES ($1, $2, $3, $4, 't', 'd', 'running', 'issue', 'codex', $5)`,
				runID, userID, repoID, int64(i+1), sessionID)
			run, err := q.GetRunByID(ctx, runID)
			if err != nil {
				t.Fatalf("GetRunByID: %v", err)
			}
			fold := func(seq int32, key string) {
				t.Helper()
				if err := foldUsageFrames(ctx, q, run, []IncomingMessage{{
					Seq: seq, Kind: "status", Agent: "lead", Payload: fixtures[key],
				}}); err != nil {
					t.Fatalf("fold fixture %q at seq %d: %v", key, seq, err)
				}
			}
			read := func() snapshot {
				t.Helper()
				// updated_at is refreshed by UpsertRunUsage even on redelivery;
				// compare all persisted usage and key/provenance fields instead.
				rows, err := pool.Query(ctx, `SELECT model, cost_status, cost_usd::text,
					input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens,
					session_id, harness, usage_basis, lineage_epoch, lineage_index,
					claim_generation IS NULL
					FROM run_usage WHERE run_id=$1 ORDER BY model`, runID)
				if err != nil {
					t.Fatalf("query persisted usage: %v", err)
				}
				defer rows.Close()
				var result snapshot
				for rows.Next() {
					var row persistedRow
					if err := rows.Scan(&row.model, &row.status, &row.cost,
						&row.tokens.input, &row.tokens.output, &row.tokens.cacheRead, &row.tokens.cacheCreation,
						&row.session, &row.harness, &row.basis, &row.epoch, &row.lineage, &row.legacyGeneration); err != nil {
						t.Fatalf("scan persisted usage: %v", err)
					}
					result.rows = append(result.rows, row)
				}
				if err := rows.Err(); err != nil {
					t.Fatalf("read persisted usage: %v", err)
				}
				total, err := q.GetRunUsageTotal(ctx, runID)
				if err != nil {
					t.Fatalf("GetRunUsageTotal: %v", err)
				}
				cost, err := total.CostUsd.Float64Value()
				if err != nil || !cost.Valid {
					t.Fatalf("invalid total cost: %+v / %v", total.CostUsd, err)
				}
				result.total = totalState{
					counters{total.InputTokens, total.OutputTokens, total.CacheReadTokens, total.CacheCreationTokens},
					total.CostStatus, cost.Float64,
				}
				return result
			}
			assertState := func(label string, wantModels []modelState, wantTotal totalState) snapshot {
				t.Helper()
				got := read()
				if len(got.rows) != len(wantModels) {
					t.Fatalf("%s row/model count = %d, want %d: %+v", label, len(got.rows), len(wantModels), got.rows)
				}
				byModel := make(map[string]persistedRow, len(got.rows))
				for _, row := range got.rows {
					if _, exists := byModel[row.model]; exists {
						t.Fatalf("%s duplicate model %q: %+v", label, row.model, got.rows)
					}
					byModel[row.model] = row
				}
				// Every row must have the exact same session and lineage key.
				// With no persisted init frames, epoch and lineage index are zero.
				for _, want := range wantModels {
					row, ok := byModel[want.model]
					if !ok {
						t.Fatalf("%s missing model %q: %+v", label, want.model, got.rows)
					}
					delete(byModel, want.model)
					if row.modelState != want || row.session != sessionID ||
						row.harness != "codex" || row.basis != "per_leg" ||
						row.epoch != 0 || row.lineage != 0 || !row.legacyGeneration {
						t.Fatalf("%s model %q = %+v, want model %+v session %q codex/per_leg epoch=0 lineage=0 legacy generation",
							label, want.model, row, want, sessionID)
					}
				}
				if got.total != wantTotal {
					t.Fatalf("%s total = %+v, want %+v", label, got.total, wantTotal)
				}
				return got
			}
			assertRedelivery := func(seq int32, key string, before snapshot) {
				t.Helper()
				fold(seq, key)
				if after := read(); !reflect.DeepEqual(after, before) {
					t.Fatalf("redelivery %q changed persisted usage: before=%+v after=%+v", key, before, after)
				}
			}

			fold(10, "earlierMetered")
			assertState("earlier metered", earlier, totalState{counters{input: 100, output: 20}, "metered", 0.002})
			fold(20, tc.incomplete)
			incomplete := assertState("incomplete", tc.want, tc.total)
			assertRedelivery(20, tc.incomplete, incomplete)
			if tc.increase {
				// Reuse the same fixture to increase high-water AFTER status was
				// already unreported, then replay both older lower-bound frames.
				fold(30, "incompleteRootSameModelChild")
				increased := assertState("later increase", sameModel, totalState{counters{input: 200, output: 100}, "unreported", 0})
				assertRedelivery(30, "incompleteRootSameModelChild", increased)
				assertRedelivery(20, "zeroIncomplete", increased)
				assertRedelivery(10, "earlierMetered", increased)
			}
		})
	}
}
