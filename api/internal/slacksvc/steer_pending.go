package slacksvc

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	// steerPendingTTL bounds how long an armed pending-steer waits for its in-thread
	// reply before it is pruned. A user who presses Steer and then wanders off must not
	// leave a live pending that silently swallows an unrelated later reply as a follow_up.
	steerPendingTTL = 15 * time.Minute
	// steerPendingsCap is a hard bound on the in-memory registry. Arm prunes expired
	// entries first; if a NEW key would still push the map past the cap it evicts the
	// soonest-to-expire live entry, so the map size can never exceed the cap regardless of
	// arm rate. In practice the TTL bound dominates (each Arm is one human confirm-gated,
	// spend-gated press, processed serially by the single Socket Mode goroutine); the cap
	// is the belt-and-braces upper bound behind that.
	steerPendingsCap = 4096
)

// SteerPendings is the in-memory registry of armed "pending steers" for Slack (PRD #322
// M4). Pressing the Steer button on a chat card ARMS a one-shot pending keyed by the
// chat conversation's thread; the requesting user's next in-thread reply is then
// consumed and submitted as a follow_up to the card's TARGET issue run.
//
// It is deliberately an in-memory map with NO DB table and NO migration, consistent
// with the poller/sweeper/notifier in-memory state: the api runs single-replica by
// design (deploy/chart/values.yaml replicaCount: 1 — one Socket Mode connection), so a
// pending armed by a button press and consumed by the next reply is always handled by
// the same process. A restart drops armed pendings, which is acceptable — the user
// simply presses Steer again.
//
// The key is the thread ROOT ts, not the card's own message ts: a Slack reply to any
// message in a thread carries thread_ts = the thread root (= the chat conversation's
// rootTS), which the card builder encodes into the button value so Arm and Take agree
// on the same key. All methods are safe for concurrent use, though the single Socket
// Mode goroutine processes interactions serially in practice.
type SteerPendings struct {
	mu  sync.Mutex
	m   map[steerKey]steerEntry
	now func() time.Time // injectable clock (default time.Now) for deterministic tests
}

type steerKey struct {
	channel  string
	threadTS string
}

type steerEntry struct {
	runID  uuid.UUID
	userID uuid.UUID
	expiry time.Time
}

// NewSteerPendings builds an empty registry with the real clock.
func NewSteerPendings() *SteerPendings {
	return &SteerPendings{m: make(map[steerKey]steerEntry), now: time.Now}
}

// Arm registers a pending steer for (channel, threadTS): the requesting user's next
// in-thread reply will be submitted as a follow_up to runID. It prunes expired entries
// first, then sets the entry (last-write-wins on a collision — a second Steer press in
// the same thread re-targets the pending, which is the intuitive behavior). If inserting
// a NEW key would still exceed the cap after pruning, it first evicts the soonest-to-
// expire live entry, so the map size is hard-bounded at steerPendingsCap.
func (s *SteerPendings) Arm(channel, threadTS string, runID, userID uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	k := steerKey{channel, threadTS}
	if _, exists := s.m[k]; !exists && len(s.m) >= steerPendingsCap {
		// A full map of LIVE entries: pruning freed nothing, so evict the entry closest to
		// expiry to make room. This makes the cap a real bound, not just a TTL bound.
		s.evictSoonestLocked()
	}
	s.m[k] = steerEntry{
		runID:  runID,
		userID: userID,
		expiry: s.now().Add(steerPendingTTL),
	}
}

// Take is the one-shot consume: it prunes expired entries, then returns and DELETES the
// pending for (channel, threadTS) only if one is present AND it was armed by userID.
// Otherwise it returns (uuid.Nil, false) and leaves the map untouched — a wrong-user
// reply does not consume the pending, so the real requester's reply still lands.
func (s *SteerPendings) Take(channel, threadTS string, userID uuid.UUID) (uuid.UUID, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	k := steerKey{channel, threadTS}
	e, ok := s.m[k]
	if !ok || e.userID != userID {
		return uuid.Nil, false
	}
	delete(s.m, k)
	return e.runID, true
}

// pruneLocked drops every entry whose expiry has passed, using the injected clock.
// Caller holds mu.
func (s *SteerPendings) pruneLocked() {
	now := s.now()
	for k, e := range s.m {
		if !e.expiry.After(now) {
			delete(s.m, k)
		}
	}
}

// evictSoonestLocked deletes the single entry with the earliest expiry — the pending
// that would time out next, so evicting it under cap pressure loses the least remaining
// life. Called only when the map is full of live (unexpired) entries. Caller holds mu.
func (s *SteerPendings) evictSoonestLocked() {
	var (
		soonestKey steerKey
		soonest    time.Time
		found      bool
	)
	for k, e := range s.m {
		if !found || e.expiry.Before(soonest) {
			soonestKey, soonest, found = k, e.expiry, true
		}
	}
	if found {
		delete(s.m, soonestKey)
	}
}
