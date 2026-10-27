package main

import (
	"context"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/config"
)

func TestSeedAdminEnrollmentDisabledNeedsNoPool(t *testing.T) {
	if err := seedAdmin(context.Background(), nil, config.Config{}); err != nil {
		t.Fatalf("disabled seed: %v", err)
	}
}
