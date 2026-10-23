package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// wireFixtureDir is fixtures/completion-wire relative to this package.
const wireFixtureDir = "../../../fixtures/completion-wire"

// wireRecordEnv, when "1", rewrites the fixtures from the handler output instead of comparing.
const wireRecordEnv = "UZI_RECORD_COMPLETION_WIRE"

var (
	wireUUIDRe = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	wireTimeRe = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})`)
)

const (
	wirePlaceholderUUID = "00000000-0000-4000-8000-000000000000"
	wirePlaceholderTime = "2026-01-01T00:00:00Z"
)

// wireStore stubs the two writes the attempt and permit endpoints make.
type wireStore struct{ protocolStore }

func (p *wireStore) RecordCompletionAttempt(context.Context, store.RecordCompletionAttemptParams) (int32, error) {
	return 1, nil
}

func (p *wireStore) UpsertCompletionPermit(_ context.Context, arg store.UpsertCompletionPermitParams) (store.RunCompletionPermit, error) {
	return store.RunCompletionPermit{
		ID:               uuid.MustParse("5f0c1c2e-7d3a-4b8e-9a61-2c4d6e8f0a1b"),
		RunID:            arg.RunID,
		ContractRevision: arg.ContractRevision,
		Branch:           arg.Branch,
		Head:             arg.Head,
		IssuedAt:         pgtype.Timestamptz{Time: time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC), Valid: true},
	}, nil
}

// wireRun is a structural run frozen with m1..m5 (one criterion each) and the given completed set.
func wireRun(id uuid.UUID, completed string) store.Run {
	return store.Run{
		ID: id, Kind: "issue", Status: "running",
		CompletionContractVersion: pgtype.Int4{Int32: 1, Valid: true},
		ContractRevision:          pgtype.Int4{Int32: 1, Valid: true},
		MilestonesFrozen:          []byte(`[{"id":"m1","title":"m1"},{"id":"m2","title":"m2"},{"id":"m3","title":"m3"},{"id":"m4","title":"m4"},{"id":"m5","title":"m5"}]`),
		MilestonesCompleted:       []byte(completed),
		CompletionContract:        []byte(`{"profile":"structural","revision":1,"criteria":[{"id":"m1.c1","milestone_id":"m1","text":"m1","audit":null,"finding_ids":[]},{"id":"m2.c1","milestone_id":"m2","text":"m2","audit":null,"finding_ids":[]},{"id":"m3.c1","milestone_id":"m3","text":"m3","audit":null,"finding_ids":[]},{"id":"m4.c1","milestone_id":"m4","text":"m4","audit":null,"finding_ids":[]},{"id":"m5.c1","milestone_id":"m5","text":"m5","audit":null,"finding_ids":[]}]}`),
	}
}

// normalizeWire replaces UUIDs and RFC 3339 timestamps with fixed placeholders and pretty-prints
// with two-space indent (key order is the handler's own) plus a trailing newline.
func normalizeWire(t *testing.T, raw []byte) []byte {
	t.Helper()
	s := wireUUIDRe.ReplaceAll(raw, []byte(wirePlaceholderUUID))
	s = wireTimeRe.ReplaceAll(s, []byte(wirePlaceholderTime))
	var out bytes.Buffer
	if err := json.Indent(&out, bytes.TrimSpace(s), "", "  "); err != nil {
		t.Fatalf("indent: %v body=%s", err, raw)
	}
	out.WriteByte('\n')
	return out.Bytes()
}

func TestCompletionWireFixtures(t *testing.T) {
	cases := []struct {
		file    string
		handler func(h *Handler, rec *httptest.ResponseRecorder, id uuid.UUID)
		run     func(id uuid.UUID) store.Run
	}{
		{
			file: "attempt-unmet.json",
			run:  func(id uuid.UUID) store.Run { return wireRun(id, `[]`) },
			handler: func(h *Handler, rec *httptest.ResponseRecorder, id uuid.UUID) {
				h.WorkerRunCompletionAttempt(rec, workerReq(http.MethodPost,
					`{"milestones_completed":["m1","m2","m3","m4"],"head":"H"}`, id))
			},
		},
		{
			file: "permit-denied.json",
			run:  func(id uuid.UUID) store.Run { return wireRun(id, `["m1","m2","m3","m4"]`) },
			handler: func(h *Handler, rec *httptest.ResponseRecorder, id uuid.UUID) {
				h.WorkerRunCompletionPermit(rec, workerReq(http.MethodPost,
					`{"contract_revision":1,"branch":"agent/issue-1","head":"H"}`, id))
			},
		},
		{
			file: "permit-granted.json",
			run:  func(id uuid.UUID) store.Run { return wireRun(id, `["m1","m2","m3","m4","m5"]`) },
			handler: func(h *Handler, rec *httptest.ResponseRecorder, id uuid.UUID) {
				h.WorkerRunCompletionPermit(rec, workerReq(http.MethodPost,
					`{"contract_revision":1,"branch":"agent/issue-1","head":"H"}`, id))
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			id := uuid.New()
			h := newProtocolHandler(t, &wireStore{protocolStore: protocolStore{ownedRun: tc.run(id)}})
			rec := httptest.NewRecorder()
			tc.handler(h, rec, id)
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
			}
			got := normalizeWire(t, rec.Body.Bytes())
			// os.Root confines reads and writes to the fixture directory: a name cannot climb out.
			root, err := os.OpenRoot(wireFixtureDir)
			if err != nil {
				t.Fatalf("open fixture dir %s: %v", wireFixtureDir, err)
			}
			defer func() { _ = root.Close() }()
			if os.Getenv(wireRecordEnv) == "1" {
				if err := root.WriteFile(tc.file, got, 0o600); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := root.ReadFile(tc.file)
			if err != nil {
				t.Fatalf("read fixture (record with %s=1): %v", wireRecordEnv, err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("%s differs from the handler output.\n--- got ---\n%s\n--- want ---\n%s", tc.file, got, want)
			}
		})
	}
}
