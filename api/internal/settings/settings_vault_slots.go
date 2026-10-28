package settings

import (
	"errors"
	"strconv"
)

// ParseVaultReunlockDays validates the authoritative DB value during mint.
// This setting has no cache accessor: each mint must read its transaction.
func ParseVaultReunlockDays(value string) (int, error) {
	n, err := strconv.Atoi(value)
	if err != nil || n < 1 || n > 30 || strconv.Itoa(n) != value {
		return 0, errors.New("vault_reunlock_days must be an integer from 1 to 30")
	}
	return n, nil
}
