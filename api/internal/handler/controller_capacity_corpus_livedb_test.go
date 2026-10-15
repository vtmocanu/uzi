package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/hostedsvc"
)

// The controller's TestSchedulerCapacityTickCorpus records real ObserveLoop.Tick
// output. Keep workers as RawMessage: remarshal only the ID-bearing object, never
// reconstruct diagnostics from the corpus expectations. Run with -count=1 because
// the producer input is outside the API module.
const controllerCapacityCorpusPath = "../../../controller/internal/kube/testdata/scheduler_capacity_tick_corpus.json"

type controllerCapacityCorpusCase struct {
	Name                 string          `json:"name"`
	ExpectedPhase        string          `json:"expected_phase"`
	ExpectedBlockingKind string          `json:"expected_blocking_kind,omitempty"`
	ExpectedFixedReason  string          `json:"expected_fixed_reason,omitempty"`
	Worker               json.RawMessage `json:"worker"`
}

func loadControllerCapacityCorpus(t *testing.T) []controllerCapacityCorpusCase {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(controllerCapacityCorpusPath))
	if err != nil {
		t.Fatal(err)
	}
	var cases []controllerCapacityCorpusCase
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 || len(cases) > 512 {
		t.Fatalf("unbounded or empty controller corpus: %d cases", len(cases))
	}
	names := make(map[string]bool)
	positive, negative := 0, 0
	for _, tc := range cases {
		if tc.Name == "" || names[tc.Name] {
			t.Fatalf("empty or duplicate corpus name: %q", tc.Name)
		}
		names[tc.Name] = true
		var worker hostedsvc.WorkerStatus
		dec := json.NewDecoder(bytes.NewReader(tc.Worker))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&worker); err != nil {
			t.Fatalf("%s: API cannot decode producer worker: %v", tc.Name, err)
		}
		if worker.Phase != tc.ExpectedPhase {
			t.Fatalf("%s: producer phase=%q metadata=%q", tc.Name, worker.Phase, tc.ExpectedPhase)
		}
		switch tc.ExpectedPhase {
		case "stuck", "rolling", "settled":
		default:
			t.Fatalf("%s: unexpected producer phase %q", tc.Name, tc.ExpectedPhase)
		}
		if tc.ExpectedBlockingKind == "" {
			negative++
			if len(worker.BlockingKind) != 0 {
				t.Fatalf("%s: negative producer must omit cause: %s", tc.Name, worker.BlockingKind)
			}
		} else {
			positive++
			if tc.ExpectedBlockingKind != "scheduler_capacity" ||
				!bytes.Equal(worker.BlockingKind, []byte(`"scheduler_capacity"`)) ||
				worker.Phase != "stuck" || worker.PodPhase != "Pending" ||
				worker.BlockingReason == nil || *worker.BlockingReason != tc.ExpectedFixedReason {
				t.Fatalf("%s: capacity metadata disagrees with producer: %s", tc.Name, tc.Worker)
			}
			switch tc.ExpectedFixedReason {
			case "scheduler capacity: insufficient cpu", "scheduler capacity: insufficient memory",
				"scheduler capacity: insufficient cpu and memory":
			default:
				t.Fatalf("%s: unexpected fixed reason %q", tc.Name, tc.ExpectedFixedReason)
			}
		}
	}
	if positive == 0 || negative == 0 {
		t.Fatal("corpus must exercise capacity and ordinary reports")
	}
	t.Logf("producer corpus: %d cases, %d capacity positives, %d negatives", len(cases), positive, negative)
	return cases
}

func TestControllerCapacityCorpusConsumerContract(t *testing.T) {
	loadControllerCapacityCorpus(t)
}

func controllerCapacityWorkerID(t *testing.T, raw json.RawMessage, id uuid.UUID) map[string]json.RawMessage {
	t.Helper()
	var row map[string]json.RawMessage
	if err := json.Unmarshal(raw, &row); err != nil {
		t.Fatal(err)
	}
	value, err := json.Marshal(id.String())
	if err != nil {
		t.Fatal(err)
	}
	row["id"] = value
	return row
}

func postControllerCapacityCorpus(t *testing.T, f *capacityLiveHarness, rows ...map[string]json.RawMessage) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"kind": "fleet", "reported_at": f.now.Add(365 * 24 * time.Hour),
		"worker_image_tag": "2.0.0", "poll_interval_seconds": 10, "workers": rows,
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	f.h.ControllerStatus(rec, httptest.NewRequest(http.MethodPost, "/api/controller/status", bytes.NewReader(raw)))
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("ControllerStatus=%d body=%s", rec.Code, rec.Body)
	}
}

func assertControllerCapacityCorpusReceipt(t *testing.T, f *capacityLiveHarness, id uuid.UUID, wantPhase string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var phase string
	var observed time.Time
	var anchor pgtype.Timestamptz
	if err := f.pool.QueryRow(ctx,
		"SELECT phase, observed_at, upgrading_since FROM worker_upgrade_reports WHERE worker_id=$1",
		id).Scan(&phase, &observed, &anchor); err != nil {
		t.Fatal(err)
	}
	if phase != wantPhase || !observed.Equal(f.now) {
		t.Fatalf("persisted phase/receipt=%q/%s want=%q/%s", phase, observed, wantPhase, f.now)
	}
	// UpsertWorkerRollHealth anchors non-terminal reports at API receipt, so the
	// corpus's old phase_since must not consume R2's upgrading window.
	if wantPhase == "rolling" || wantPhase == "stuck" {
		if !anchor.Valid || !anchor.Time.Equal(f.now) {
			t.Fatalf("non-terminal anchor=%+v want API receipt %s", anchor, f.now)
		}
	}
}

func assertControllerCapacityOrdinaryDetail(t *testing.T, f *capacityLiveHarness, id uuid.UUID, worker hostedsvc.WorkerStatus) {
	t.Helper()
	for _, admin := range []bool{false, true} {
		dto := f.dto(t, id, admin)
		if dto.UpgradeDetail == nil || *dto.UpgradeDetail == "" ||
			dto.UpgradeBlockingKind != nil || dto.UpgradeStatus == "capacity_blocked" {
			t.Fatalf("admin=%v lost ordinary detail or exposed capacity: %+v", admin, dto)
		}
		if worker.BlockingReason != nil && *worker.BlockingReason != "" &&
			!strings.Contains(*dto.UpgradeDetail, *worker.BlockingReason) {
			t.Fatalf("admin=%v lost producer reason %q: %+v", admin, *worker.BlockingReason, dto)
		}
		if worker.BlockingContainer != nil && *worker.BlockingContainer != "" &&
			(dto.UpgradeBlockingContainer == nil || *dto.UpgradeBlockingContainer != *worker.BlockingContainer) {
			t.Fatalf("admin=%v lost producer container %q: %+v", admin, *worker.BlockingContainer, dto)
		}
	}
}

// Serial, one POST per case, no retries or sleeps. Subtest cleanup removes only
// its seeded workers; all report writes go through the actual handler.
func TestControllerCapacityCorpusLiveDB(t *testing.T) {
	cases := loadControllerCapacityCorpus(t)
	f := newCapacityLiveHarness(t)
	version := "1.0.0" // Previously registered; ordinary stuck reports are upgrade failures.
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			id := f.seed(t, &version)
			postControllerCapacityCorpus(t, f, controllerCapacityWorkerID(t, tc.Worker, id))
			f.rawKind(t, id, tc.ExpectedBlockingKind)
			assertControllerCapacityCorpusReceipt(t, f, id, tc.ExpectedPhase)
			if tc.ExpectedBlockingKind != "" {
				f.assertDTO(t, id, "capacity_blocked", tc.ExpectedBlockingKind, tc.ExpectedFixedReason)
				f.attention(t, 1)
				return
			}
			var worker hostedsvc.WorkerStatus
			if err := json.Unmarshal(tc.Worker, &worker); err != nil {
				t.Fatal(err)
			}
			switch tc.ExpectedPhase {
			case "stuck":
				reason := ""
				if worker.BlockingReason != nil {
					reason = *worker.BlockingReason
				}
				f.assertDTO(t, id, "upgrade_failed", "", reason)
				assertControllerCapacityOrdinaryDetail(t, f, id, worker)
				f.attention(t, 1)
			case "rolling":
				f.assertDTO(t, id, "upgrading", "", "")
				assertControllerCapacityOrdinaryDetail(t, f, id, worker)
			case "settled":
				// Old Ready timestamps are outside registration convergence grace.
				// Pin absence of capacity without inventing a health classification.
				for _, admin := range []bool{false, true} {
					dto := f.dto(t, id, admin)
					if dto.UpgradeStatus == "capacity_blocked" || dto.UpgradeBlockingKind != nil {
						t.Fatalf("admin=%v settled report exposed capacity: %+v", admin, dto)
					}
				}
			}
		})
	}
}

func TestControllerCapacityCorpusMalformedCausesLiveDB(t *testing.T) {
	cases := loadControllerCapacityCorpus(t)
	var cpu json.RawMessage
	for _, tc := range cases {
		if tc.Name == "cpu" && tc.ExpectedBlockingKind == "scheduler_capacity" &&
			tc.ExpectedFixedReason == "scheduler capacity: insufficient cpu" {
			cpu = tc.Worker
		}
	}
	if len(cpu) == 0 {
		t.Fatal("producer CPU positive absent")
	}
	f := newCapacityLiveHarness(t)
	version := "1.0.0"
	for _, tc := range []struct {
		name, cause, reason string
	}{
		{name: "unknown", cause: `"future_cause"`},
		{name: "number", cause: `42`},
		{name: "object", cause: `{}`},
		{name: "array", cause: `[]`},
		{name: "null", cause: `null`},
		{name: "older_controller_omission"},
		{name: "cause_bidi", cause: `"scheduler_\u202ecapacity"`},
		{name: "typed_invalid_message", cause: `"scheduler_capacity"`,
			reason: `"scheduler capacity: insufficient cpu\u202e\u001b\n"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, sibling := f.seed(t, &version), f.seed(t, &version)
			row := controllerCapacityWorkerID(t, cpu, id)
			if tc.cause == "" {
				delete(row, "blocking_kind")
			} else {
				row["blocking_kind"] = json.RawMessage(tc.cause)
			}
			if tc.reason != "" {
				row["blocking_reason"] = json.RawMessage(tc.reason)
			}
			postControllerCapacityCorpus(t, f, row, controllerCapacityWorkerID(t, cpu, sibling))
			f.rawKind(t, id, "")
			assertControllerCapacityCorpusReceipt(t, f, id, "stuck")
			// Sanitization restores the fixed CPU text but cannot authorize the
			// invalid raw cause or message as scheduler capacity.
			f.assertDTO(t, id, "upgrade_failed", "", "scheduler capacity: insufficient cpu")
			for _, admin := range []bool{false, true} {
				dto := f.dto(t, id, admin)
				if dto.UpgradeBlockingReason == nil || *dto.UpgradeBlockingReason != "scheduler capacity: insufficient cpu" ||
					dto.UpgradeDetail == nil || strings.ContainsAny(*dto.UpgradeDetail, "\u202e\u001b\n") {
					t.Fatalf("admin=%v unsafe or lost ordinary diagnostics: %+v", admin, dto)
				}
			}
			f.rawKind(t, sibling, "scheduler_capacity")
			assertControllerCapacityCorpusReceipt(t, f, sibling, "stuck")
			f.assertDTO(t, sibling, "capacity_blocked", "scheduler_capacity", "scheduler capacity: insufficient cpu")
			f.attention(t, 2)
		})
	}
}
