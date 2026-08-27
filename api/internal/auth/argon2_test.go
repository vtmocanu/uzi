package auth

import "testing"

func TestHashAndVerify(t *testing.T) {
	hash, err := HashPassword("correct-horse-battery-staple")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if hash == "correct-horse-battery-staple" {
		t.Fatal("password stored in plaintext")
	}

	ok, err := VerifyPassword("correct-horse-battery-staple", hash)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !ok {
		t.Fatal("correct password did not verify")
	}

	ok, err = VerifyPassword("wrong-password", hash)
	if err != nil {
		t.Fatalf("verify wrong: %v", err)
	}
	if ok {
		t.Fatal("wrong password verified")
	}
}

func TestHashIsSalted(t *testing.T) {
	a, _ := HashPassword("same-password-value")
	b, _ := HashPassword("same-password-value")
	if a == b {
		t.Fatal("identical hashes for the same password: salt not applied")
	}
}

func TestVerifyRejectsMalformed(t *testing.T) {
	for _, bad := range []string{"", "not-a-hash", "$argon2id$v=19$m=1$x$y", "$bcrypt$abc"} {
		if _, err := VerifyPassword("pw", bad); err == nil {
			t.Errorf("expected error for malformed hash %q", bad)
		}
	}
}
