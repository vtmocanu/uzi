package workersvc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type diagramDiagnosticStore struct {
	Store
	tx *diagramDiagnosticTx
}

func (s diagramDiagnosticStore) BeginPrDescTx(context.Context) (PrDescTx, error) { return s.tx, nil }

type diagramDiagnosticTx struct {
	PrDescQueries
	run        store.Run
	id         uuid.UUID
	commits    int
	failCommit int
	insertErr  error
	logs       *bytes.Buffer
	t          *testing.T
}

func (tx *diagramDiagnosticTx) GetRunOwnedByWorkerForUpdate(context.Context, store.GetRunOwnedByWorkerForUpdateParams) (store.Run, error) {
	return tx.run, nil
}
func (tx *diagramDiagnosticTx) AbandonStalePrDescriptionVersionsForRun(context.Context, store.AbandonStalePrDescriptionVersionsForRunParams) (int64, error) {
	return 0, nil
}
func (tx *diagramDiagnosticTx) CountPendingPrDescriptionVersionsForRunGeneration(context.Context, store.CountPendingPrDescriptionVersionsForRunGenerationParams) (int64, error) {
	return 0, nil
}
func (tx *diagramDiagnosticTx) CountPrDescriptionVersionsForRun(context.Context, uuid.UUID) (int64, error) {
	return 0, nil
}
func (tx *diagramDiagnosticTx) InsertPrDescriptionVersion(_ context.Context, p store.InsertPrDescriptionVersionParams) (store.PrDescriptionVersion, error) {
	return store.PrDescriptionVersion{ID: tx.id, RunID: p.RunID, ClaimGeneration: p.ClaimGeneration, Fields: p.Fields}, tx.insertErr
}
func (tx *diagramDiagnosticTx) Commit(context.Context) error {
	tx.commits++
	if tx.logs.Len() != 0 {
		tx.t.Fatal("diagram warning emitted before commit returned")
	}
	if tx.commits == tx.failCommit {
		return errors.New("private commit detail")
	}
	return nil
}
func (tx *diagramDiagnosticTx) Rollback(context.Context) error { return nil }

func TestStagePrDescriptionDiagramDiagnostic(t *testing.T) {
	for _, tc := range []struct {
		name        string
		failCommit  int
		insertErr   error
		wantWarning bool
		wantVersion bool
	}{
		{name: "committed", wantWarning: true, wantVersion: true},
		{name: "preflight commit failure", failCommit: 1},
		{name: "insert failure", insertErr: errors.New("private insert detail"), wantWarning: true},
		{name: "second commit failure", failCommit: 2, wantWarning: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			old := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			defer slog.SetDefault(old)
			wkr := worker()
			tx := &diagramDiagnosticTx{run: store.Run{ID: uuid.New(), UserID: wkr.UserID, RepoID: pgtype.UUID{Bytes: uuid.New(), Valid: true}, ClaimGeneration: 3, Status: "running"}, id: uuid.New(), failCommit: tc.failCommit, insertErr: tc.insertErr, logs: &logs, t: t}
			generation := int64(3)
			req := apitypes.PrDescriptionStageRequest{ClaimGeneration: &generation, Source: "generated", BaseSha: strings.Repeat("a", 40), HeadSha: strings.Repeat("b", 40), TargetBranch: "main", Fields: apitypes.PrDescriptionFields{Summary: "Retained prose", Diagram: &apitypes.PrDescriptionDiagram{Kind: "private graph detail"}}}
			out, err := New(diagramDiagnosticStore{tx: tx}, newBox(t), testParams()).StagePrDescription(context.Background(), wkr, tx.run.ID, req)
			if (err == nil) != tc.wantVersion {
				t.Fatalf("stage error = %v", err)
			}
			if tc.wantVersion && (out.ID != tx.id.String() || out.Fields.Diagram != nil || out.Fields.Summary != "Retained prose") {
				t.Fatalf("stage result = %+v", out)
			}
			if !tc.wantWarning {
				if logs.Len() != 0 {
					t.Fatalf("unexpected log: %s", &logs)
				}
				return
			}
			var record map[string]any
			if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &record); err != nil {
				t.Fatalf("expected exactly one warning: %s: %v", &logs, err)
			}
			if record["msg"] != "workersvc: dropped pr description diagram" || record["reason"] != "kind" || record["run_id"] != tx.run.ID.String() || record["claim_generation"] != float64(3) {
				t.Fatalf("warning = %+v", record)
			}
			_, hasVersion := record["version_id"]
			if hasVersion != tc.wantVersion || (hasVersion && record["version_id"] != tx.id.String()) {
				t.Fatalf("version correlation = %+v", record)
			}
			if strings.Contains(logs.String(), "private") {
				t.Fatalf("private details leaked: %s", &logs)
			}
		})
	}
}
func TestSanitizePrDescriptionFieldsDiagramReporting(t *testing.T) {
	in := apitypes.PrDescriptionFields{Summary: "Retained", Diagram: &apitypes.PrDescriptionDiagram{Kind: "invalid"}}
	var reasons []string
	out, err := sanitizePrDescriptionFields(context.Background(), in, func(reason string) { reasons = append(reasons, reason) })
	public, publicErr := SanitizePrDescriptionFields(context.Background(), in)
	if err != nil || publicErr != nil || out.Diagram != nil || public.Diagram != nil || out.Summary != public.Summary || len(reasons) != 1 || reasons[0] != "kind" {
		t.Fatalf("report = %+v, %+v, %v, %v", out, public, reasons, err)
	}
}
func TestStagePrDescriptionDiagramCorrelationLiveDB(t *testing.T) {
	p := setupPrDescLive(t)
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	out, err := p.svc.StagePrDescription(p.env.ctx, p.wkr, p.runID, apitypes.PrDescriptionStageRequest{
		ClaimGeneration: gen(1), Source: "generated",
		BaseSha: strings.Repeat("a", 40), HeadSha: strings.Repeat("b", 40), TargetBranch: "main",
		Fields: apitypes.PrDescriptionFields{Summary: "Retained prose", Diagram: &apitypes.PrDescriptionDiagram{Kind: "invalid"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &record); err != nil {
		t.Fatalf("expected one warning: %v", err)
	}
	if record["run_id"] != p.runID.String() || record["claim_generation"] != float64(1) || record["version_id"] != out.ID || record["reason"] != "kind" {
		t.Fatalf("correlation = %+v", record)
	}
	var storedRun uuid.UUID
	var generation int64
	var fields []byte
	if err := p.env.pool.QueryRow(p.env.ctx, "SELECT run_id, claim_generation, fields FROM pr_description_versions WHERE id = $1", out.ID).Scan(&storedRun, &generation, &fields); err != nil {
		t.Fatalf("committed version: %v", err)
	}
	var stored apitypes.PrDescriptionFields
	if err := json.Unmarshal(fields, &stored); err != nil {
		t.Fatal(err)
	}
	if storedRun != p.runID || generation != 1 || stored.Diagram != nil || stored.Summary != "Retained prose" {
		t.Fatal("committed row does not match correlated sanitized version")
	}
}

func TestDiagramSanitationFixedReasons(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*apitypes.PrDescriptionDiagram)
		reason string
	}{
		{"kind", func(d *apitypes.PrDescriptionDiagram) { d.Kind = "private graph" }, "kind"},
		{"entries", func(d *apitypes.PrDescriptionDiagram) { d.Nodes = d.Nodes[:2] }, "entries"},
		{"title", func(d *apitypes.PrDescriptionDiagram) { d.Title = "Closes #7" }, "title"},
		{"node key", func(d *apitypes.PrDescriptionDiagram) { d.Nodes[1].Key = d.Nodes[0].Key }, "node key"},
		{"node label", func(d *apitypes.PrDescriptionDiagram) { d.Nodes[0].Label = "Closes #7" }, "node label"},
		{"edge endpoint", func(d *apitypes.PrDescriptionDiagram) { d.Edges[0].To = "missing" }, "edge endpoint"},
		{"edge label", func(d *apitypes.PrDescriptionDiagram) { d.Edges[0].Label = "Closes #7" }, "edge label"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &apitypes.PrDescriptionDiagram{
				Kind:  "flow",
				Nodes: []apitypes.PrDescriptionDiagramNode{{Key: "a", Label: "A"}, {Key: "b", Label: "B"}, {Key: "c", Label: "C"}},
				Edges: []apitypes.PrDescriptionDiagramEdge{{From: "a", To: "b"}, {From: "b", To: "c"}},
			}
			tc.mutate(d)
			var reasons []string
			fields, err := sanitizePrDescriptionFields(context.Background(), apitypes.PrDescriptionFields{Summary: "Retained", Diagram: d}, func(reason string) {
				reasons = append(reasons, reason)
			})
			if err != nil || fields.Diagram != nil || fields.Summary != "Retained" || len(reasons) != 1 || reasons[0] != tc.reason {
				t.Fatalf("whole-graph rejection or fixed reason changed: fields=%+v reasons=%v err=%v", fields, reasons, err)
			}
		})
	}
}
