package main

import (
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func TestWorkerListDrainDetails(t *testing.T) {
	at := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	detail := "run 11111111-1111-4111-8111-111111111111 (chat, pool_wait): a pooled token becomes spendable"
	worker := apitypes.WorkerDTO{ID: "22222222-2222-4222-8222-222222222222", Name: "draining", Kind: "hosted", DrainingSince: &at, UpgradeStatus: "up_to_date", UpgradeDetail: &detail}
	fc := &uzicli.FakeClient{Workers: []apitypes.WorkerDTO{worker}, AdminWorkers: []apitypes.AdminWorkerDTO{{WorkerDTO: worker, OwnerEmail: "owner@uzi.test"}}}
	for _, args := range [][]string{{"worker", "list"}, {"admin", "workers"}} {
		out, _, code := runCLI(t, fakeEnv(fc), args...)
		if code != 0 || !strings.Contains(out, detail) {
			t.Fatalf("%v missing drain detail (exit %d): %s", args, code, out)
		}
	}
}
