package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestRunMessageInstanceLiveDB proves the PRD #99 columns against a REAL Postgres:
// the 00075 migration applies, InsertRunMessage persists agent_instance +
// agent_label, and ListRunMessagesAfter reads them back on the same row. The load-
// bearing half is the NULL contract — a message emitted with neither field (the
// lead, an infra frame, or any pre-migration row) must come back with
// AgentInstance.Valid == false, NOT an empty string. A fake store cannot show that:
// pgText("") → NULL is a database round-trip, and every consumer's role-name
// fallback keys off Valid.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (the store IT
// runner provides one); mirrors the other *_integration_test.go here.
func TestRunMessageInstanceLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via the store integration runner for live-DB coverage")
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

	userID, connID, repoID, runID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("msginstance-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)
	mustExec(ctx, t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status)
		 VALUES ($1, $2, $3, 1, 't', 'd', 'running')`, runID, userID, repoID)

	// pgText mirrors workersvc.pgText: "" is absence, i.e. SQL NULL.
	pgText := func(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }
	insert := func(seq int32, agent, instance, label string) {
		t.Helper()
		rows, err := q.InsertRunMessage(ctx, store.InsertRunMessageParams{
			RunID: runID, Seq: seq, Kind: "text", Agent: pgText(agent),
			AgentInstance: pgText(instance), AgentLabel: pgText(label),
			Payload: []byte(`{"text":"x"}`),
		})
		if err != nil {
			t.Fatalf("InsertRunMessage(seq=%d): %v", seq, err)
		}
		if rows != 1 {
			t.Fatalf("InsertRunMessage(seq=%d) inserted %d rows, want 1", seq, rows)
		}
	}
	// The lead: no instance, no label. Then two PARALLEL coder invocations — the
	// case the whole PRD exists for: same role, distinct instance ids, distinct
	// labels, interleaved seqs.
	insert(1, "lead", "", "")
	insert(2, "coder", "toolu_A", "web gate UX")
	insert(3, "coder", "toolu_B", "API wiring")
	insert(4, "coder", "toolu_A", "web gate UX")
	// A subagent frame whose task_description is absent: instance set, label NULL.
	insert(5, "reviewer", "toolu_C", "")

	msgs, err := q.ListRunMessagesAfter(ctx, store.ListRunMessagesAfterParams{RunID: runID, AfterSeq: 0})
	if err != nil {
		t.Fatalf("ListRunMessagesAfter: %v", err)
	}
	if len(msgs) != 5 {
		t.Fatalf("ListRunMessagesAfter returned %d messages, want 5", len(msgs))
	}
	bySeq := map[int32]store.RunMessage{}
	for _, m := range msgs {
		bySeq[m.Seq] = m
	}

	// NULL stays NULL — never coerced to "".
	if lead := bySeq[1]; lead.AgentInstance.Valid || lead.AgentLabel.Valid {
		t.Fatalf("lead message must read back with both columns NULL, got instance=%+v label=%+v",
			lead.AgentInstance, lead.AgentLabel)
	}
	if lead := bySeq[1]; lead.AgentInstance.String != "" || lead.AgentLabel.String != "" {
		t.Fatalf("NULL columns must scan to the zero string, got instance=%q label=%q",
			lead.AgentInstance.String, lead.AgentLabel.String)
	}

	// Two same-role instances stay distinguishable through the round trip.
	a, b := bySeq[2], bySeq[3]
	if a.Agent.String != "coder" || b.Agent.String != "coder" {
		t.Fatalf("both parallel messages should carry agent=coder, got %q / %q", a.Agent.String, b.Agent.String)
	}
	if !a.AgentInstance.Valid || !b.AgentInstance.Valid || a.AgentInstance.String == b.AgentInstance.String {
		t.Fatalf("parallel coders must round-trip DISTINCT instance ids, got %+v / %+v",
			a.AgentInstance, b.AgentInstance)
	}
	if a.AgentLabel.String != "web gate UX" || b.AgentLabel.String != "API wiring" {
		t.Fatalf("labels must round-trip verbatim, got %q / %q", a.AgentLabel.String, b.AgentLabel.String)
	}
	// A non-adjacent later turn of the SAME instance keeps the same id (what lets
	// the web coalesce it into one lane instead of a fresh bar).
	if again := bySeq[4]; again.AgentInstance.String != a.AgentInstance.String {
		t.Fatalf("seq 4 should re-use instance %q, got %q", a.AgentInstance.String, again.AgentInstance.String)
	}

	// Instance without a label: id present, label NULL (the role-only lane title).
	if m := bySeq[5]; !m.AgentInstance.Valid || m.AgentLabel.Valid {
		t.Fatalf("labelless subagent frame should be instance-valid/label-NULL, got instance=%+v label=%+v",
			m.AgentInstance, m.AgentLabel)
	}

	// The worker read-tool page reads the same columns off the same rows.
	page, err := q.ListRunMessagesForWorkerPage(ctx, store.ListRunMessagesForWorkerPageParams{
		RunID: runID, AfterSeq: 1, Lim: 10,
	})
	if err != nil {
		t.Fatalf("ListRunMessagesForWorkerPage: %v", err)
	}
	if len(page) == 0 || page[0].AgentInstance.String != "toolu_A" || page[0].AgentLabel.String != "web gate UX" {
		t.Fatalf("worker page should carry the instance columns, got %+v", page)
	}
}
