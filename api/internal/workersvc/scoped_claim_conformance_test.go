package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func scopedRawSnapshot(t *testing.T, id uuid.UUID, pending, group string) *ActiveSnapshot {
	t.Helper()
	var snap ActiveSnapshot
	raw := fmt.Sprintf(`{"snapshot_epoch":1,"register_nonce":"scoped","active":[],"pending_overflow":true,"pending_all":%s,"pending_policy_protection":%s}`, pending, group)
	if err := json.Unmarshal([]byte(raw), &snap); err != nil {
		t.Fatal(err)
	}
	return &snap
}
func scopedTuple(id uuid.UUID, gen int64) string {
	return fmt.Sprintf(`{"run_id":%q,"claim_generation":%d}`, id.String(), gen)
}
func scopedGroup(id uuid.UUID, gen int64) string {
	return fmt.Sprintf(`[{"run_id":%q,"owner_generation":%d}]`, id.String(), gen)
}

func TestScopedClaimAccounting(t *testing.T) {
	for _, name := range []string{"history", "three grouped plus ordinary", "three grouped plus two ordinary", "three ordinary", "ordinary duplicate run generations", "ordinary foreign", "ordinary wrong generation", "ordinary released", "ordinary terminal", "ordinary zero upsert", "group unknown", "group foreign", "group wrong user", "group frozen target excluded", "ordinary frozen target excluded", "group alias duplicate", "group released", "group queued", "group terminal", "group wrong generation", "group zero generation", "group duplicate", "group missing source", "group malformed", "group absent", "pending unavailable", "pending malformed", "pending duplicate", "pending truncated", "pending wire ceiling", "zero capacity", "no locked capability", "input capability stale", "wrong nonce", "equal epoch", "stale epoch", "postlock release", "postlock status", "postlock generation", "postlock owner", "postlock user", "contradictory active coverage"} {
		t.Run(name, func(t *testing.T) {
			svc, tx, id := scopedClaimFixture(t)
			ordinary := uuid.New()
			tx.runs[ordinary] = store.Run{ID: ordinary, Kind: "issue", WorkerID: pgconv.UUID(tx.worker.ID), ClaimGeneration: 2, Status: "running"}
			pending := "[" + scopedTuple(id, 7) + "]"
			group := scopedGroup(id, 9)
			allow := name == "history" || name == "three grouped plus ordinary"
			invalid := false
			switch name {
			case "three grouped plus ordinary", "three grouped plus two ordinary":
				pending = "[" + scopedTuple(id, 1) + "," + scopedTuple(id, 2) + "," + scopedTuple(id, 3) + "," + scopedTuple(ordinary, 2)
				if name == "three grouped plus two ordinary" {
					pending += "," + scopedTuple(ordinary, 1)
				}
				pending += "]"
			case "three ordinary":
				pending = "[" + scopedTuple(ordinary, 0) + "," + scopedTuple(ordinary, 1) + "," + scopedTuple(ordinary, 2) + "]"
			case "ordinary duplicate run generations":
				pending = "[" + scopedTuple(id, 7) + "," + scopedTuple(ordinary, 1) + "," + scopedTuple(ordinary, 2) + "]"
			case "ordinary foreign", "ordinary wrong generation", "ordinary released", "ordinary terminal", "ordinary zero upsert":
				pending = "[" + scopedTuple(id, 7) + "," + scopedTuple(ordinary, 2) + "]"
				r := tx.runs[ordinary]
				switch name {
				case "ordinary foreign":
					r.WorkerID = pgconv.UUID(uuid.New())
					tx.zeroOrdinary = true
				case "ordinary wrong generation":
					r.ClaimGeneration = 3
				case "ordinary released":
					r.ClaimReleasedAt = pgconv.Time(svc.now())
					tx.zeroOrdinary = true
				case "ordinary terminal":
					r.Status = "completed"
					tx.zeroOrdinary = true
				case "ordinary zero upsert":
					tx.zeroOrdinary = true
				}
				tx.runs[ordinary] = r
			case "ordinary frozen target excluded":
				pending = "[" + scopedTuple(id, 7) + "," + scopedTuple(ordinary, 2) + "]"
				tx.excludedTarget = ordinary
			case "group frozen target excluded":
				tx.excludedTarget = id
			case "group wrong user":
				r := tx.runs[id]
				r.UserID = uuid.New()
				tx.runs[id] = r
			case "group alias duplicate":
				group = fmt.Sprintf(`[{"run_id":%q,"owner_generation":9},{"run_id":%q,"owner_generation":9}]`, id.String(), "urn:uuid:"+id.String())
			case "group unknown":
				delete(tx.runs, id)
			case "group foreign":
				r := tx.runs[id]
				r.WorkerID = pgconv.UUID(uuid.New())
				tx.runs[id] = r
			case "group released":
				r := tx.runs[id]
				r.ClaimReleasedAt = pgconv.Time(svc.now())
				tx.runs[id] = r
			case "group queued":
				r := tx.runs[id]
				r.Status = "queued"
				tx.runs[id] = r
			case "group terminal":
				r := tx.runs[id]
				r.Status = "completed"
				tx.runs[id] = r
			case "group wrong generation":
				group = scopedGroup(id, 8)
			case "group zero generation":
				group = scopedGroup(id, 0)
			case "group duplicate":
				group = fmt.Sprintf(`[{"run_id":%q,"owner_generation":9},{"run_id":%q,"owner_generation":9}]`, id.String(), id.String())
			case "group missing source":
				pending = "[]"
			case "group malformed":
				group = `[{"run_id":5,"owner_generation":9}]`
			case "group absent":
				group = "null"
			case "pending unavailable":
				pending = "null"
			case "pending malformed":
				pending = `[{"run_id":2}]`
			case "pending duplicate":
				pending = "[" + scopedTuple(id, 7) + "," + scopedTuple(id, 7) + "]"
			case "pending truncated":
				pending = "[]"
			case "pending wire ceiling":
				svc.p.ActiveSnapshotMaxEntries = 1
				pending = "[" + scopedTuple(id, 7) + "," + scopedTuple(id, 8) + "]"
			case "zero capacity":
				svc.p.WorkerOutboxMaxPending = 0
			case "no locked capability", "input capability stale":
				tx.worker.ProtocolCapabilities = nil
			}
			snap := scopedRawSnapshot(t, id, pending, group)
			switch name {
			case "wrong nonce":
				snap.RegisterNonce = "old"
				invalid = true
			case "equal epoch":
				tx.worker.SnapshotEpoch = 1
				invalid = true
			case "stale epoch":
				tx.worker.SnapshotEpoch = 2
				invalid = true
			case "contradictory active coverage":
				snap.Active = []ActiveRunEntry{entry(ordinary, 2, "running", true)}
			case "postlock release", "postlock status", "postlock generation", "postlock owner", "postlock user":
				tx.afterTargets = func() {
					r := tx.runs[id]
					switch name {
					case "postlock release":
						r.ClaimReleasedAt = pgconv.Time(svc.now())
					case "postlock status":
						r.Status = "queued"
					case "postlock generation":
						r.ClaimGeneration = 10
					case "postlock user":
						r.UserID = uuid.New()
					case "postlock owner":
						r.WorkerID = pgconv.UUID(uuid.New())
					}
					tx.runs[id] = r
				}
			}
			input := tx.worker
			if name == "input capability stale" {
				input.ProtocolCapabilities = []string{"pending_outcome_scoped_claim_v1"}
			}
			payload, err := svc.Claim(context.Background(), input, snap)
			if invalid {
				if !errors.Is(err, ErrActiveSnapshotInvalid) || tx.claims != 0 {
					t.Fatalf("stale evidence err=%v claims=%d", err, tx.claims)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if (payload != nil) != allow || tx.claims != boolCount(allow) {
				t.Fatalf("payload=%v claims=%d allow=%v", payload != nil, tx.claims, allow)
			}
			if name == "history" {
				if rows, found := tx.identityWrites[pendingOutcomeTuple{id, 7}]; !found || rows != 0 {
					t.Fatal("historical source must exercise a zero-row identity write")
				}
			}
			if name == "three grouped plus ordinary" && tx.identityWrites[pendingOutcomeTuple{ordinary, 2}] != 1 {
				t.Fatal("ordinary tuple was not independently attributed")
			}
			if !tx.worker.PendingOverflowUntil.Valid {
				t.Fatal("conservative overflow not persisted")
			}
		})
	}
}

func TestScopedClaimKnownPolicyOwnerLease(t *testing.T) {
	for _, mode := range []string{"known historical", "known grouped plus ordinary", "wrong owner lease", "independent source lease", "unmatched ordinary lease", "unmatched zero generation lease"} {
		t.Run(mode, func(t *testing.T) {
			svc, tx, id := scopedClaimFixture(t)
			pending := "[" + scopedTuple(id, 7) + "]"
			if mode == "known grouped plus ordinary" {
				ordinary := uuid.New()
				tx.runs[ordinary] = store.Run{ID: ordinary, UserID: tx.worker.UserID, WorkerID: pgconv.UUID(tx.worker.ID), Kind: "issue", Status: "running", ClaimGeneration: 2}
				pending = "[" + scopedTuple(id, 1) + "," + scopedTuple(id, 2) + "," + scopedTuple(id, 3) + "," + scopedTuple(ordinary, 2) + "]"
			}
			gen := int64(9)
			if mode == "wrong owner lease" || mode == "independent source lease" {
				gen = 8
			}
			if mode == "independent source lease" {
				pending = "[" + scopedTuple(id, 7) + "," + scopedTuple(id, 8) + "]"
			}
			snap := scopedRawSnapshot(t, id, pending, scopedGroup(id, 9))
			snap.Active = []ActiveRunEntry{entry(id, gen, "running", true)}
			if mode == "unmatched zero generation lease" {
				snap.Active = append(snap.Active, entry(uuid.New(), 0, "running", true))
			}
			if mode == "unmatched ordinary lease" {
				snap.Active = append(snap.Active, entry(uuid.New(), 2, "running", true))
			}
			payload, err := svc.Claim(context.Background(), tx.worker, snap)
			allow := mode != "wrong owner lease" && mode != "unmatched ordinary lease" && mode != "unmatched zero generation lease"
			if err != nil || (payload != nil) != allow {
				t.Fatalf("known-policy owner/source split: payload=%v want=%v err=%v", payload != nil, allow, err)
			}
			if allow && payload.RunID != tx.candidate.ID.String() {
				t.Fatalf("claimed %s want unrelated %s", payload.RunID, tx.candidate.ID)
			}
		})
	}
}

func boolCount(b bool) int {
	if b {
		return 1
	}
	return 0
}

func TestScopedClaimAbsoluteInventoryBoundary(t *testing.T) {
	for _, count := range []int{256, 257} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			svc, tx, id := scopedClaimFixture(t)
			svc.p.ActiveSnapshotMaxEntries = 256
			tuples := make([]string, count)
			for i := range tuples {
				tuples[i] = scopedTuple(id, int64(i+1))
			}
			snap := scopedRawSnapshot(t, id, "["+strings.Join(tuples, ",")+"]", scopedGroup(id, 9))
			payload, err := svc.Claim(context.Background(), tx.worker, snap)
			if err != nil || (payload != nil) != (count == 256) {
				t.Fatalf("inventory=%d payload=%v err=%v", count, payload != nil, err)
			}
		})
	}
}

func TestScopedClaimIndependentDecode(t *testing.T) {
	id := uuid.New()
	for _, group := range []string{`{}`, `"bad"`, `[null]`, `[{"run_id":"x"}]`, `[{"run_id":"x","owner_generation":null}]`, `[{"run_id":"x","owner_generation":1.5}]`} {
		snap := scopedRawSnapshot(t, id, "["+scopedTuple(id, 7)+"]", group)
		if !snap.pendingPolicyProtectionMalformed || len(snap.PendingAll) != 1 || !snap.PendingOverflow {
			t.Fatalf("optional decode lost independent evidence: %+v", snap)
		}
	}
}
