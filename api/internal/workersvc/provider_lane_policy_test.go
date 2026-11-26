package workersvc

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// This fake exposes only the presence query: credential and vendor reads are unavailable.
type lanePresenceReader struct {
	row   store.GetUserProviderLaneSourcesRow
	err   error
	calls int
	owner uuid.UUID
}

func (f *lanePresenceReader) GetUserProviderLaneSources(_ context.Context, owner uuid.UUID) (store.GetUserProviderLaneSourcesRow, error) {
	f.calls++
	f.owner = owner
	return f.row, f.err
}

func TestProviderLanePresenceMatrix(t *testing.T) {
	id := pgtype.UUID{Bytes: uuid.New(), Valid: true}
	for _, source := range []struct {
		name                        string
		row                         store.GetUserProviderLaneSourcesRow
		claudeBlocked, codexBlocked bool
	}{
		{"neither", store.GetUserProviderLaneSourcesRow{}, false, false},
		{"claude", store.GetUserProviderLaneSourcesRow{ClaudeProviderID: id}, true, true},
		{"codex", store.GetUserProviderLaneSourcesRow{CodexProviderID: id}, false, true},
		{"both", store.GetUserProviderLaneSourcesRow{ClaudeProviderID: id, CodexProviderID: id}, true, true},
	} {
		for _, lane := range []Harness{HarnessClaude, HarnessCodex} {
			t.Run(source.name+"/"+string(lane), func(t *testing.T) {
				f := &lanePresenceReader{row: source.row}
				owner := uuid.New()
				err := (&Service{}).refuseProviderLane(context.Background(), f, owner, lane)
				blocked := source.claudeBlocked
				if lane == HarnessCodex {
					blocked = source.codexBlocked
				}
				if blocked && !errors.Is(err, ErrProviderLaneUnsupported) || !blocked && err != nil {
					t.Fatalf("blocked=%t err=%v", blocked, err)
				}
				if f.calls != 1 || f.owner != owner {
					t.Fatalf("calls=%d owner=%s", f.calls, f.owner)
				}
			})
		}
	}
	for _, lane := range []Harness{"", "unknown"} {
		f := &lanePresenceReader{err: errors.New("must not query")}
		if err := (&Service{}).refuseProviderLane(context.Background(), f, uuid.New(), lane); !errors.Is(err, ErrProviderLaneUnsupported) || f.calls != 0 {
			t.Fatalf("lane=%q calls=%d err=%v", lane, f.calls, err)
		}
	}
	for _, queryErr := range []error{pgx.ErrNoRows, errors.New("query failed")} {
		for _, lane := range []Harness{HarnessClaude, HarnessCodex} {
			f := &lanePresenceReader{err: queryErr}
			if err := (&Service{}).refuseProviderLane(context.Background(), f, uuid.New(), lane); !errors.Is(err, queryErr) || f.calls != 1 {
				t.Fatalf("calls=%d err=%v want=%v", f.calls, err, queryErr)
			}
		}
	}
}

type laneCreationStore struct {
	fakeStore
	lanePresenceReader
}

func TestProviderLaneFakeCreationAdmission(t *testing.T) {
	for _, tc := range []struct {
		name             string
		explicit         *Harness
		row              store.GetUserProviderLaneSourcesRow
		want             error
		queries, inserts int
	}{
		{name: "missing explicit", want: ErrProviderLaneUnsupported},
		{name: "unknown explicit", explicit: laneHarness("unknown"), want: ErrProviderLaneUnsupported},
		{name: "claude selected", explicit: laneHarness(HarnessClaude), row: store.GetUserProviderLaneSourcesRow{ClaudeProviderID: pgtype.UUID{Bytes: uuid.New(), Valid: true}}, want: ErrProviderLaneUnsupported, queries: 1},
		{name: "codex selected claude allowed", explicit: laneHarness(HarnessClaude), row: store.GetUserProviderLaneSourcesRow{CodexProviderID: pgtype.UUID{Bytes: uuid.New(), Valid: true}}, queries: 1, inserts: 1},
		{name: "codex selected codex refused", explicit: laneHarness(HarnessCodex), row: store.GetUserProviderLaneSourcesRow{CodexProviderID: pgtype.UUID{Bytes: uuid.New(), Valid: true}}, want: ErrProviderLaneUnsupported, queries: 1},
		{name: "neither claude allowed", explicit: laneHarness(HarnessClaude), queries: 1, inserts: 1},
		{name: "neither codex fake credential refusal", explicit: laneHarness(HarnessCodex), want: errNoCredentialForHarness, queries: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &laneCreationStore{lanePresenceReader: lanePresenceReader{row: tc.row}}
			inserts := 0
			_, err := (&Service{q: f}).createRunResolved(context.Background(), uuid.New(), tc.explicit, func(Store, resolvedHarness) (store.Run, error) {
				inserts++
				return store.Run{}, nil
			}, harnessCreationOrigin{RefuseProviderOwner: true})
			if !errors.Is(err, tc.want) || f.calls != tc.queries || inserts != tc.inserts {
				t.Fatalf("err=%v want=%v queries=%d inserts=%d", err, tc.want, f.calls, inserts)
			}
		})
	}
}
func laneHarness(h Harness) *Harness { return &h }

// TestProviderLaneFakeCreationAdmissionJobLane is the JobLane twin of the table above (PRD #4322 M1b):
// without a transaction (the fake store) a vendor job proceeds, but a selected Claude source can never
// be frozen, so it fails closed with errProviderCreateRequiresTx before any insert, and a site-list job
// keeps its provider_lane_unsupported refusal (never an insert either way).
func TestProviderLaneFakeCreationAdmissionJobLane(t *testing.T) {
	claudeSet := store.GetUserProviderLaneSourcesRow{ClaudeProviderID: pgtype.UUID{Bytes: uuid.New(), Valid: true}}
	codexSet := store.GetUserProviderLaneSourcesRow{CodexProviderID: pgtype.UUID{Bytes: uuid.New(), Valid: true}}
	for _, tc := range []struct {
		name             string
		explicit         *Harness
		siteList         bool
		row              store.GetUserProviderLaneSourcesRow
		want             error
		queries, inserts int
	}{
		{name: "missing explicit", want: ErrProviderLaneUnsupported},
		{name: "unknown explicit", explicit: laneHarness("unknown"), want: ErrProviderLaneUnsupported},
		{name: "claude selected fails closed without a transaction", explicit: laneHarness(HarnessClaude), row: claudeSet, want: errProviderCreateRequiresTx, queries: 1},
		{name: "claude selected with a site list", explicit: laneHarness(HarnessClaude), siteList: true, row: claudeSet, want: ErrProviderLaneUnsupported, queries: 1},
		{name: "codex selected claude allowed", explicit: laneHarness(HarnessClaude), row: codexSet, queries: 1, inserts: 1},
		{name: "codex selected codex refused", explicit: laneHarness(HarnessCodex), row: codexSet, want: ErrProviderLaneUnsupported, queries: 1},
		{name: "neither claude allowed", explicit: laneHarness(HarnessClaude), queries: 1, inserts: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &laneCreationStore{lanePresenceReader: lanePresenceReader{row: tc.row}}
			inserts := 0
			_, err := (&Service{q: f}).createRunResolved(context.Background(), uuid.New(), tc.explicit, func(Store, resolvedHarness) (store.Run, error) {
				inserts++
				return store.Run{}, nil
			}, harnessCreationOrigin{JobLane: true, JobSiteList: tc.siteList})
			if !errors.Is(err, tc.want) || f.calls != tc.queries || inserts != tc.inserts {
				t.Fatalf("err=%v want=%v queries=%d inserts=%d, want %d/%d", err, tc.want, f.calls, inserts, tc.queries, tc.inserts)
			}
		})
	}
}

func TestProviderLaneSettingsSelectionBeforeCredentials(t *testing.T) {
	for _, row := range []store.GetUserProviderLaneSourcesRow{
		{ClaudeProviderID: pgtype.UUID{Bytes: uuid.New(), Valid: true}},
		{CodexProviderID: pgtype.UUID{Bytes: uuid.New(), Valid: true}},
	} {
		// Vendor availability must not be read for a refused selection.
		f := &laneSettingsStore{lanePresenceReader: lanePresenceReader{row: row}}
		both, err := (&Service{q: f}).BothHarnessesUsable(context.Background(), uuid.New())
		if both || err != nil || f.calls != 1 {
			t.Fatalf("both=%t err=%v calls=%d", both, err, f.calls)
		}
	}
}

type laneSettingsStore struct {
	poolFakeStore
	lanePresenceReader
}

func (f *laneCreationStore) GetUserProviderLaneSources(ctx context.Context, owner uuid.UUID) (store.GetUserProviderLaneSourcesRow, error) {
	return f.lanePresenceReader.GetUserProviderLaneSources(ctx, owner)
}
func (f *laneSettingsStore) GetUserProviderLaneSources(ctx context.Context, owner uuid.UUID) (store.GetUserProviderLaneSourcesRow, error) {
	return f.lanePresenceReader.GetUserProviderLaneSources(ctx, owner)
}
func (f *laneSettingsStore) UserHasEnabledAnthropicToken(context.Context, uuid.UUID) (bool, error) {
	panic("unexpected vendor credential read")
}
func TestProviderLaneSettingsQueryErrors(t *testing.T) {
	for _, queryErr := range []error{pgx.ErrNoRows, errors.New("query failed")} {
		f := &laneSettingsStore{lanePresenceReader: lanePresenceReader{err: queryErr}}
		both, err := (&Service{q: f}).BothHarnessesUsable(context.Background(), uuid.New())
		if both || !errors.Is(err, queryErr) || f.calls != 1 {
			t.Fatalf("both=%t err=%v calls=%d", both, err, f.calls)
		}
	}
}
