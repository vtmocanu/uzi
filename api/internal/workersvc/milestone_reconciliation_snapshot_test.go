package workersvc

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// populatedRun sets every field of store.Run to a non-zero value; []byte fields
// hold valid JSON because the snapshot inlines several of them.
func populatedRun(t *testing.T) store.Run {
	t.Helper()
	var r store.Run
	v := reflect.ValueOf(&r).Elem()
	for i := 0; i < v.NumField(); i++ {
		populate(t, v.Field(i), v.Type().Field(i).Name)
	}
	return r
}

func populate(t *testing.T, f reflect.Value, name string) {
	t.Helper()
	switch x := f.Addr().Interface().(type) {
	case *uuid.UUID:
		*x = uuid.New()
		return
	case *pgtype.UUID:
		*x = pgtype.UUID{Bytes: uuid.New(), Valid: true}
		return
	case *pgtype.Text:
		*x = pgtype.Text{String: "txt-" + name, Valid: true}
		return
	case *pgtype.Int2:
		*x = pgtype.Int2{Int16: 3, Valid: true}
		return
	case *pgtype.Int4:
		*x = pgtype.Int4{Int32: 3, Valid: true}
		return
	case *pgtype.Int8:
		*x = pgtype.Int8{Int64: 3, Valid: true}
		return
	case *pgtype.Float8:
		*x = pgtype.Float8{Float64: 1.5, Valid: true}
		return
	case *pgtype.Bool:
		*x = pgtype.Bool{Bool: true, Valid: true}
		return
	case *pgtype.Timestamptz:
		*x = pgtype.Timestamptz{Time: time.Unix(1700000000, 0).UTC(), Valid: true}
		return
	case *[]byte:
		*x = []byte(`["x"]`)
		return
	case *[]string:
		*x = []string{"a"}
		return
	}
	switch f.Kind() {
	case reflect.String:
		f.SetString("s-" + name)
	case reflect.Bool:
		f.SetBool(true)
	case reflect.Int32, reflect.Int64:
		f.SetInt(7)
	default:
		t.Fatalf("populatedRun: unhandled field %s of type %s", name, f.Type())
	}
}

func snapshotKeys(t *testing.T, b []byte) (map[string]json.RawMessage, []string) {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("snapshot is not a JSON object: %v", err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return m, keys
}

func TestReconciliationSnapshotKeySet(t *testing.T) {
	b, err := reconciliationSnapshot(populatedRun(t))
	if err != nil {
		t.Fatal(err)
	}
	m, got := snapshotKeys(t, b)
	want := []string{
		"agent_exclusions", "agent_source", "budget_paused_seconds", "checkpoint_tip",
		"claim_generation", "claim_released_at", "codex_claim_epoch", "completion_contract",
		"completion_contract_version", "contract_revision", "credential_switch_requested_at",
		"gate_payload_digest", "gate_presentation_id", "gate_revision", "health",
		"health_reason", "health_since", "id", "kind", "milestones_agents",
		"milestones_candidate", "milestones_completed", "milestones_frozen",
		"milestones_in_progress", "pause_requested_at", "plan_source", "required_capabilities",
		"revise_count", "scope_ceiling", "session_id", "status", "status_since",
		"stop_kind", "stop_reason", "worker_id",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot keys = %v, want %v", got, want)
	}
	for _, k := range []string{"latest_completion_attempt", "gate_presented_payload", "plan_md",
		"issue_title", "issue_description", "issue_comments", "codex_cap_hash", "updated_at"} {
		if _, ok := m[k]; ok {
			t.Errorf("excluded key %q present", k)
		}
	}
	// JSONB values are inlined, not base64 strings.
	if string(m["milestones_frozen"]) != `["x"]` || string(m["agent_exclusions"]) != `["x"]` {
		t.Errorf("JSONB not inlined: frozen=%s exclusions=%s", m["milestones_frozen"], m["agent_exclusions"])
	}
}

func TestReconciliationSnapshotIgnoresLargeUnrelatedFields(t *testing.T) {
	big := func(marker string) string { return marker + strings.Repeat("z", 20*1024) }
	r := store.Run{
		ID:                  uuid.New(),
		Status:              "awaiting_approval",
		MilestonesFrozen:    []byte(`[{"id":"M1","title":"Old"}]`),
		MilestonesCandidate: []byte(`[{"id":"M1","title":"New"}]`),
		MilestonesCompleted: []byte(`["M1"]`),
		CompletionContract:  []byte(`{"revision":1}`),
		PlanMd:              pgtype.Text{String: big("PLANMARK"), Valid: true},
		IssueDescription:    big("DESCMARK"),
		IssueComments:       []byte(`[{"body":"` + big("COMMENTMARK") + `"}]`),
		GatePresentedPayload: []byte(`{"plan_md":"` + big("PAYLOADMARK") +
			`","milestones":[{"id":"M1","title":"New"}]}`),
		LatestCompletionAttempt: []byte(`{"unmet":["` + big("ATTEMPTMARK") + `"]}`),
	}
	b, err := reconciliationSnapshot(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"PLANMARK", "DESCMARK", "COMMENTMARK", "PAYLOADMARK", "ATTEMPTMARK"} {
		if strings.Contains(string(b), marker) {
			t.Errorf("snapshot contains unrelated large field %s", marker)
		}
	}
	m, _ := snapshotKeys(t, b)
	for key, want := range map[string]string{
		"milestones_frozen":    string(r.MilestonesFrozen),
		"milestones_candidate": string(r.MilestonesCandidate),
		"milestones_completed": string(r.MilestonesCompleted),
		"completion_contract":  string(r.CompletionContract),
	} {
		if !jsonEqual(t, m[key], want) {
			t.Errorf("%s = %s, want %s", key, m[key], want)
		}
	}
	// Headroom: Go marshal length differs from the Postgres jsonb text length the
	// 8 KiB cap measures; the LiveDB octet_length test is authoritative.
	if len(b) > 6*1024 {
		t.Fatalf("snapshot is %d bytes, want <= %d", len(b), 6*1024)
	}
}

func TestReconciliationSnapshotSessionID(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   pgtype.Text
		want string
	}{
		{"valid", pgtype.Text{String: "sess-1", Valid: true}, `"sess-1"`},
		{"invalid", pgtype.Text{}, `null`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := reconciliationSnapshot(store.Run{SessionID: tc.in})
			if err != nil {
				t.Fatal(err)
			}
			m, _ := snapshotKeys(t, b)
			got, ok := m["session_id"]
			if !ok || string(got) != tc.want {
				t.Fatalf("session_id = %s (present=%v), want %s", got, ok, tc.want)
			}
		})
	}
}
