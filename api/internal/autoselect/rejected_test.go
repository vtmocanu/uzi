package autoselect

import (
	"github.com/google/uuid"
	"testing"
	"time"
)

func TestRejectedCredentialIsNeverSelectedOrFloored(t *testing.T) {
	now := time.Now()
	rejected := Candidate{SecretID: uuid.New(), AutoEligible: true, Rejected: true}
	healthy := Candidate{SecretID: uuid.New(), AutoEligible: true, HasReading: true, SyncedAt: &now}
	p := Policy{MaxStaleness: time.Hour}
	if got := Classify(rejected, p, now).Status; got != StatusRejected {
		t.Fatalf("status = %s", got)
	}
	if got := Select([]Candidate{rejected}, uuid.Nil, p, now); got.Picked {
		t.Fatalf("selected rejected token: %+v", got)
	}
	if _, ok := Floor([]Candidate{rejected}, uuid.Nil, now); ok {
		t.Fatal("floored rejected token")
	}
	if id, ok := Floor([]Candidate{rejected, healthy}, uuid.Nil, now); !ok || id != healthy.SecretID {
		t.Fatalf("floor = %v, %v", id, ok)
	}
	rejected.AutoEligible = false
	if got := Classify(rejected, p, now).Status; got != StatusNotPooled {
		t.Fatalf("unpooled status = %s", got)
	}
}
