package workersvc

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type auditReadFake struct {
	*store.Queries
	rows   []store.ListRunMilestoneReconciliationsRow
	params store.ListRunMilestoneReconciliationsParams
}

func (f *auditReadFake) ListRunMilestoneReconciliations(_ context.Context, p store.ListRunMilestoneReconciliationsParams) ([]store.ListRunMilestoneReconciliationsRow, error) {
	f.params = p
	// Intentionally ignore SQL's row limit and owner filter to test service bounds.
	for i, row := range f.rows {
		if p.BeforeGateRevision == 0 || row.GateRevision < p.BeforeGateRevision {
			return f.rows[i:], nil
		}
	}
	return nil, nil
}
func auditReadFixture(n int) (*Service, *auditReadFake, uuid.UUID, uuid.UUID) {
	owner, run := uuid.New(), uuid.New()
	f := &auditReadFake{}
	for revision := n; revision > 0; revision-- {
		f.rows = append(f.rows, store.ListRunMilestoneReconciliationsRow{
			ID: uuid.New(), RunID: run, OwnerID: owner, GateRevision: int64(revision), ClaimGeneration: 1,
			Reason: "owner reset", Consent: []byte(`{"mode":"reset"}`), BeforeState: []byte(`{"milestones_frozen":["old"]}`),
			AfterState: []byte(`{"milestones_frozen":["new"]}`),
		})
	}
	return &Service{q: f}, f, owner, run
}
func TestMilestoneReconciliationBoundedReadPaging(t *testing.T) {
	svc, f, owner, run := auditReadFixture(10002)
	var before int64
	expected := int64(10002)
	pages := 0
	for {
		page, err := svc.MilestoneReconciliationAuditBefore(t.Context(), owner, run, before)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Records) == 0 || len(page.Records) > 50 {
			t.Fatalf("page rows %d", len(page.Records))
		}
		if f.params.OwnerID != owner || f.params.RunID != run || f.params.BeforeGateRevision != before {
			t.Fatal("query lost scope/cursor")
		}
		b, err := json.Marshal(page)
		if err != nil || len(b) > 256*1024 {
			t.Fatalf("encoded page %d: %v", len(b), err)
		}
		for _, raw := range page.Records {
			var row struct {
				GateRevision int64 `json:"gate_revision"`
			}
			if err := json.Unmarshal(raw, &row); err != nil {
				t.Fatal(err)
			}
			if row.GateRevision != expected {
				t.Fatalf("skip/repeat: got %d want %d", row.GateRevision, expected)
			}
			expected--
		}
		pages++
		if page.NextBeforeGateRevision == nil {
			break
		}
		if *page.NextBeforeGateRevision != expected+1 {
			t.Fatal("cursor is not last returned revision")
		}
		before = *page.NextBeforeGateRevision
	}
	if expected != 0 || pages != 201 {
		t.Fatalf("incomplete history: remaining %d pages %d", expected, pages)
	}
}
func TestMilestoneReconciliationBoundedReadBytes(t *testing.T) {
	svc, f, owner, run := auditReadFixture(60)
	// HTML escaping expands each '<' to six encoded bytes. UTF-8 and escaped
	// controls exercise reason encoding independently of snapshot input lengths.
	raw := []byte(`{"body":"` + strings.Repeat("<", 8100) + `"}`)
	for i := range f.rows {
		f.rows[i].Consent = raw
		f.rows[i].BeforeState = raw
		f.rows[i].AfterState = raw
		f.rows[i].Reason = strings.Repeat("😀\n\x01", 1000)
	}
	before := int64(0)
	expected := int64(60)
	for {
		page, err := svc.MilestoneReconciliationAuditBefore(t.Context(), owner, run, before)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(page)
		if err != nil || len(encoded) > 256*1024 {
			t.Fatalf("byte cap: %d %v", len(encoded), err)
		}
		if len(page.Records) == 0 || len(page.Records) >= 50 {
			t.Fatalf("byte cap did not stop page: %d", len(page.Records))
		}
		for _, b := range page.Records {
			var item struct {
				GateRevision    int64           `json:"gate_revision"`
				Reason          string          `json:"reason"`
				ReasonTruncated bool            `json:"reason_truncated"`
				Consent         json.RawMessage `json:"consent"`
			}
			if err := json.Unmarshal(b, &item); err != nil {
				t.Fatal(err)
			}
			if item.GateRevision != expected {
				t.Fatalf("skip/repeat: %d want %d", item.GateRevision, expected)
			}
			if !item.ReasonTruncated || !utf8.ValidString(item.Reason) || item.Reason != strings.Repeat("😀\n\x01", 85)+"😀" {
				t.Fatal("reason preview lost truncation provenance or UTF-8")
			}
			var consent struct {
				Body string `json:"body"`
			}
			if err := json.Unmarshal(item.Consent, &consent); err != nil || consent.Body != strings.Repeat("<", 8100) {
				t.Fatal("normal consent changed")
			}
			expected--
		}
		if page.NextBeforeGateRevision == nil {
			break
		}
		if *page.NextBeforeGateRevision != expected+1 {
			t.Fatal("byte stop skipped record")
		}
		before = *page.NextBeforeGateRevision
	}
	if expected != 0 {
		t.Fatal("history incomplete")
	}
}

// TestMilestoneReconciliationBoundedReadLiveDB is the lead's database proof:
// SQL bounds transfer, while the full oversized audit remains stored.
func TestMilestoneReconciliationBoundedReadLiveDB(t *testing.T) {
	f := newGateFix(t, 8)
	run := f.newRun("issue")
	var input int64
	if err := f.pool.QueryRow(f.ctx, "INSERT INTO run_user_inputs(run_id,kind,body) VALUES($1,'approve_plan','{}') RETURNING id", run).Scan(&input); err != nil {
		t.Fatal(err)
	}
	huge := `{"body":"` + strings.Repeat("😀<", 10000) + `"}`
	f.exec(`INSERT INTO run_milestone_reconciliations
 (id,run_id,owner_id,gate_revision,presentation_id,gate_digest,reset_state_digest,worker_id,claim_generation,
 reason,consent,before_state,after_state,applied_input_id,stop_cutoff)
 SELECT gen_random_uuid(),$1,$2,revision,gen_random_uuid(),'digest'::bytea,'reset',$3,1,
 CASE WHEN revision=10002 THEN repeat('😀',1000) ELSE 'reset' END,
 CASE WHEN revision=10002 THEN $5::jsonb ELSE '{"mode":"reset"}'::jsonb END,
 CASE WHEN revision=10002 THEN $5::jsonb ELSE '{"before":true}'::jsonb END,
 CASE WHEN revision=10002 THEN $5::jsonb ELSE '{"after":true}'::jsonb END,$4,0
 FROM generate_series(1,10002) AS revision`, run, f.userID, f.wkr.ID, input, huge)
	var storedBefore string
	if err := f.pool.QueryRow(f.ctx, "SELECT before_state::text FROM run_milestone_reconciliations WHERE run_id=$1 AND gate_revision=10002", run).Scan(&storedBefore); err != nil {
		t.Fatal(err)
	}
	rows, err := f.q.ListRunMilestoneReconciliations(f.ctx, store.ListRunMilestoneReconciliationsParams{RunID: run, OwnerID: f.userID})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 51 || rows[0].GateRevision != 10002 || !rows[0].DetailsOmitted || !rows[0].ReasonTruncated ||
		string(rows[0].Consent) != "null" || string(rows[0].BeforeState) != "null" || string(rows[0].AfterState) != "null" ||
		rows[0].Reason != strings.Repeat("😀", 256) {
		t.Fatalf("SQL projection unbounded or missing provenance: %+v", rows[0])
	}
	var before int64
	expected := int64(10002)
	for {
		page, err := f.svc.MilestoneReconciliationAuditBefore(f.ctx, f.userID, run, before)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(page)
		if err != nil || len(encoded) > 256*1024 || len(page.Records) > 50 || len(page.Records) == 0 {
			t.Fatal("invalid SQL-backed page")
		}
		for _, raw := range page.Records {
			var item struct {
				GateRevision int64 `json:"gate_revision"`
			}
			if err := json.Unmarshal(raw, &item); err != nil {
				t.Fatal(err)
			}
			if item.GateRevision != expected {
				t.Fatalf("SQL skip/repeat: got %d want %d", item.GateRevision, expected)
			}
			expected--
		}
		if page.NextBeforeGateRevision == nil {
			break
		}
		before = *page.NextBeforeGateRevision
	}
	if expected != 0 {
		t.Fatal("SQL history incomplete")
	}
	foreign, err := f.svc.MilestoneReconciliationAuditBefore(f.ctx, uuid.New(), run, 0)
	if err != nil || len(foreign.Records) != 0 || foreign.NextBeforeGateRevision != nil {
		t.Fatal("SQL owner scope leaked")
	}
	var storedAfter string
	if err := f.pool.QueryRow(f.ctx, "SELECT before_state::text FROM run_milestone_reconciliations WHERE run_id=$1 AND gate_revision=10002", run).Scan(&storedAfter); err != nil {
		t.Fatal(err)
	}
	if storedAfter != storedBefore || !strings.Contains(storedAfter, strings.Repeat("😀<", 10000)) {
		t.Fatal("durable oversized history changed")
	}
}

func TestMilestoneReconciliationBoundedReadOmissionAndScope(t *testing.T) {
	svc, f, owner, run := auditReadFixture(1)
	huge := []byte(`{"body":"` + strings.Repeat("x", 1000000) + `"}`)
	f.rows[0].BeforeState = huge
	stored, err := json.Marshal(f.rows)
	if err != nil {
		t.Fatal(err)
	}
	page, err := svc.MilestoneReconciliationAuditBefore(t.Context(), owner, run, 0)
	if err != nil || len(page.Records) != 1 {
		t.Fatalf("oversized record discarded: %v", err)
	}
	var item struct {
		DetailsOmitted bool            `json:"details_omitted"`
		Before         json.RawMessage `json:"before_state"`
		After          json.RawMessage `json:"after_state"`
		Consent        json.RawMessage `json:"consent"`
	}
	if err := json.Unmarshal(page.Records[0], &item); err != nil {
		t.Fatal(err)
	}
	if !item.DetailsOmitted || string(item.Before) != "null" || string(item.After) != `{"milestones_frozen":["new"]}` || string(item.Consent) != `{"mode":"reset"}` {
		t.Fatalf("bad omission projection: %s", page.Records[0])
	}
	after, _ := json.Marshal(f.rows)
	if !bytes.Equal(stored, after) {
		t.Fatal("read mutated stored audit")
	}
	for _, ids := range [][2]uuid.UUID{{uuid.New(), run}, {owner, uuid.New()}} {
		hidden, err := svc.MilestoneReconciliationAuditBefore(t.Context(), ids[0], ids[1], 0)
		if err != nil || len(hidden.Records) != 0 || hidden.NextBeforeGateRevision != nil {
			t.Fatalf("outsider audit leaked: %+v %v", hidden, err)
		}
	}
}
