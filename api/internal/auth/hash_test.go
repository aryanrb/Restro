package auth_test

import (
	"testing"

	"github.com/oneaccord/restro/api/internal/auth"
)

func TestHashRoundTrip(t *testing.T) {
	h, err := auth.HashSecret("s3cret-pass")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	ok, err := auth.VerifySecret("s3cret-pass", h)
	if err != nil || !ok {
		t.Fatalf("verify correct: ok=%v err=%v", ok, err)
	}
	bad, err := auth.VerifySecret("wrong-pass", h)
	if err != nil || bad {
		t.Fatalf("verify wrong: ok=%v err=%v (want false,nil)", bad, err)
	}
}

func TestHashesAreSalted(t *testing.T) {
	a, _ := auth.HashSecret("same")
	b, _ := auth.HashSecret("same")
	if a == b {
		t.Fatal("two hashes of the same secret should differ (per-hash salt)")
	}
}

func TestVerifyBadFormat(t *testing.T) {
	if _, err := auth.VerifySecret("x", "not-a-valid-hash"); err == nil {
		t.Fatal("want error on malformed hash")
	}
}
