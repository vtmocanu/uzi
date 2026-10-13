package healthsvc

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/dbdiskfull"
	"github.com/vtmocanu/uzi/api/internal/notifysvc"
)

// emergencyNoticeCooldown is how long one admin is not re-sent the in-memory disk-full
// notice within the same dbdiskfull generation.
const emergencyNoticeCooldown = 30 * time.Minute

// emergencyDiskFullLine is the fixed server-authored line every emergency notice carries,
// whether or not the evaluated document still lists the db disk-full danger check. It is
// the single source of the db check's disk-full summary (checkDB uses this const), and
// withDiskFullLine dedups against it.
const emergencyDiskFullLine = "Database writes are failing: disk full (53100)."

// emergencyKey identifies one admin's cooldown slot within one disk-full incident.
type emergencyKey struct {
	gen  uint64
	user uuid.UUID
}

// emergencyState is the reconciler's in-memory emergency-notice state: the consecutive
// open-failed-with-53100 streak and the per-(generation, admin) last-sent times.
type emergencyState struct {
	slack notifysvc.Slacker
	sig   *dbdiskfull.Signal

	mu     sync.Mutex
	streak int
	sent   map[emergencyKey]time.Time
}

// WithEmergencySlack enables the in-memory disk-full path: when the database refuses
// writes with SQLSTATE 53100 the normal claim-and-persist notice cannot be recorded, so
// the admin DM is published straight to slack without touching the database for the write.
// A nil slack or nil sig leaves the path disabled. It returns r for chaining.
func (r *EpisodeReconciler) WithEmergencySlack(slack notifysvc.Slacker, sig *dbdiskfull.Signal) *EpisodeReconciler {
	if slack == nil || sig == nil {
		r.emergency = nil
		return r
	}
	r.emergency = &emergencyState{slack: slack, sig: sig, sent: map[emergencyKey]time.Time{}}
	return r
}

// openFailedDiskFull records one tick whose OpenHealthEpisode failed with 53100 and reports
// whether it is the second (or later) consecutive one, i.e. the debounce is satisfied.
func (r *EpisodeReconciler) openFailedDiskFull(err error) bool {
	e := r.emergency
	if e == nil || !dbdiskfull.Is(err) {
		return false
	}
	e.sig.Observe(err)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.streak++
	return e.streak >= 2
}

// resetEmergencyStreak ends the consecutive-53100 run.
func (r *EpisodeReconciler) resetEmergencyStreak() {
	if e := r.emergency; e != nil {
		e.mu.Lock()
		e.streak = 0
		e.mu.Unlock()
	}
}

// emergencyOpenNotice fans the emergency notice out when the open itself keeps failing with
// 53100. No episode exists to claim against, so it reads the enablement gate and the admin
// list itself.
func (r *EpisodeReconciler) emergencyOpenNotice(ctx context.Context, doc Doc) {
	enabled, err := r.settings.HealthEnabled(ctx)
	if err != nil {
		r.logger.Error("health episode: read health_enabled", "error", err)
		return
	}
	if !enabled {
		return
	}
	admins, err := r.store.ListAdmins(ctx)
	if err != nil {
		r.logger.Error("health episode: list admins", "error", err)
		return
	}
	if len(admins) == 0 {
		return
	}
	base, _ := r.settings.PublicBaseURL(ctx)
	danger := dangerChecks(doc)
	for _, uid := range admins {
		r.emergencySend(uid, base, danger)
	}
}

// emergencyAfterErr is the claim/notify trigger: when err is a 53100 it sends the
// emergency notice to uid. Callers have already passed the HealthEnabled and ListAdmins gates.
func (r *EpisodeReconciler) emergencyAfterErr(err error, uid uuid.UUID, base string, danger []dangerCheck) {
	e := r.emergency
	if e == nil || !dbdiskfull.Is(err) {
		return
	}
	e.sig.Observe(err)
	r.emergencySend(uid, base, danger)
}

// emergencySend publishes the emergency DM to uid unless one was sent for the same
// (generation, admin) within emergencyNoticeCooldown. The cooldown is armed before the
// publish, so a dropped enqueue or an admin who has not opted in to Slack DMs is not
// retried within the window. Entries of older generations are pruned.
func (r *EpisodeReconciler) emergencySend(uid uuid.UUID, base string, danger []dangerCheck) {
	e := r.emergency
	if e == nil {
		return
	}
	gen := e.sig.Generation()
	now := r.now()
	e.mu.Lock()
	for k := range e.sent {
		if k.gen < gen {
			delete(e.sent, k)
		}
	}
	key := emergencyKey{gen: gen, user: uid}
	if last, ok := e.sent[key]; ok && now.Sub(last) < emergencyNoticeCooldown {
		e.mu.Unlock()
		return
	}
	e.sent[key] = now
	e.mu.Unlock()

	e.slack.PublishNotification(uid, *buildHealthEpisodeNotification(base, uid, uuid.Nil, withDiskFullLine(danger)).Slack)
	r.logger.Warn("health episode: emergency disk-full notice sent from memory", "user", uid.String(), "generation", gen)
}

// withDiskFullLine returns danger with the fixed disk-full check first, unless a check
// already carries exactly that summary.
func withDiskFullLine(danger []dangerCheck) []dangerCheck {
	for _, c := range danger {
		if c.Summary == emergencyDiskFullLine {
			return danger
		}
	}
	out := make([]dangerCheck, 0, len(danger)+1)
	out = append(out, dangerCheck{ID: "db", Title: "Database", Summary: emergencyDiskFullLine})
	return append(out, danger...)
}
