package issuetracker

import (
	"context"
	"testing"
)

func TestStatesAndProjects(t *testing.T) {
	y, _ := newFakeDriver(t)
	ctx := context.Background()
	acct, err := y.VerifyToken(ctx)
	if err != nil || acct.Login != youtracktestBot {
		t.Fatalf("VerifyToken = %+v, %v", acct, err)
	}
	projects, err := y.ListProjects(ctx)
	if err != nil || len(projects) != 2 {
		t.Fatalf("ListProjects = %+v, %v", projects, err)
	}
	states, err := y.ListStates(ctx, "0-1")
	if err != nil || len(states) != 3 || states[0].Resolved || !states[1].Resolved || states[2].Name != "Won't fix" {
		t.Fatalf("ListStates = %+v, %v", states, err)
	}
	if _, err := y.ListStates(ctx, "../x"); err == nil {
		t.Fatal("ListStates accepted a malformed project id")
	}
}
