package config

import (
	"encoding/base64"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/secretbox"
)

func TestLoadDBStorageCapacityBytes(t *testing.T) {
	cases := []struct {
		name string
		set  bool
		raw  string
		want int64
	}{
		{"unset", false, "", 0},
		{"empty", true, "", 0},
		{"valid", true, "8589934592", 8589934592},
		{"garbage", true, "8Gi", 0},
		{"zero", true, "0", 0},
		{"negative", true, "-5", 0},
		{"one EiB is valid", true, "1152921504606846976", 1 << 60},
		{"above one EiB", true, "1152921504606846977", 0},
		{"overflows int64", true, "99999999999999999999", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://uzi:pw@db:5432/uzi?sslmode=disable")
			t.Setenv("JWT_SECRET", "unit-test-jwt-signing-key-not-a-real-secret")
			varied := make([]byte, secretbox.KeySize)
			for i := range varied {
				varied[i] = byte(i + 1)
			}
			t.Setenv("UZI_SECRET_KEY", base64.StdEncoding.EncodeToString(varied))
			t.Setenv("DB_STORAGE_CAPACITY_BYTES", "")
			if tc.set {
				t.Setenv("DB_STORAGE_CAPACITY_BYTES", tc.raw)
			}
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load(): %v", err)
			}
			if cfg.DBStorageCapacityBytes != tc.want {
				t.Errorf("DBStorageCapacityBytes = %d, want %d", cfg.DBStorageCapacityBytes, tc.want)
			}
		})
	}
}
