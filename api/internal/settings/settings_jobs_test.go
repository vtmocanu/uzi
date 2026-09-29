package settings

import (
	"context"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestValidateJobMaxActivePerUser(t *testing.T) {
	for _, v := range []string{"1", "10", "1000", " 5 "} {
		if err := Validate(KeyJobMaxActivePerUser, v); err != nil {
			t.Errorf("Validate(job_max_active_per_user, %q) = %v, want nil", v, err)
		}
	}
	for _, v := range []string{"0", "-1", "1001", "2.5", "", "abc", "PRD"} {
		if err := Validate(KeyJobMaxActivePerUser, v); err == nil {
			t.Errorf("Validate(job_max_active_per_user, %q) = nil, want a rejection", v)
		}
	}
}

func TestJobMaxActivePerUserAccessor(t *testing.T) {
	ctx := context.Background()
	c := New(&fakeStore{rows: []store.AppSetting{row(KeyJobMaxActivePerUser, "3")}}, time.Minute)
	if got, _ := c.JobMaxActivePerUser(ctx); got != 3 {
		t.Errorf("JobMaxActivePerUser = %d, want 3", got)
	}
	c = New(&fakeStore{}, time.Minute)
	if got, _ := c.JobMaxActivePerUser(ctx); got != 10 {
		t.Errorf("default JobMaxActivePerUser = %d, want 10", got)
	}
	c = New(&fakeStore{rows: []store.AppSetting{row(KeyJobMaxActivePerUser, "junk")}}, time.Minute)
	if got, _ := c.JobMaxActivePerUser(ctx); got != 10 {
		t.Errorf("junk JobMaxActivePerUser = %d, want the default 10", got)
	}
}
