package settings

import "testing"

func TestVaultReunlockDaysBounds(t *testing.T) {
	if !Known(KeyVaultReunlockDays) || IsSecret(KeyVaultReunlockDays) || Defaults[KeyVaultReunlockDays] != "7" {
		t.Fatal("slot setting registry")
	}
	for _, value := range []string{"1", "7", "30"} {
		if err := Validate(KeyVaultReunlockDays, value); err != nil {
			t.Fatalf("%q: %v", value, err)
		}
	}
	for _, value := range []string{"", "0", "31", "-1", "1.5", "junk", " 7", "07", "+7", "99999999999999999999999"} {
		if err := Validate(KeyVaultReunlockDays, value); err == nil {
			t.Fatalf("accepted %q", value)
		}
	}
}
