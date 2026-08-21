package account

import (
	"strings"
	"testing"
)

func TestPasswordHashRoundTrip(t *testing.T) {
	encoded, err := HashPassword("a correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(encoded, "$argon2id$v=19$m=65536,t=3,p=1$") {
		t.Fatalf("unexpected password record: %s", encoded)
	}
	ok, err := VerifyPassword(encoded, "a correct horse battery staple")
	if err != nil || !ok {
		t.Fatalf("valid password rejected: ok=%v err=%v", ok, err)
	}
	ok, err = VerifyPassword(encoded, "definitely the wrong password")
	if err != nil || ok {
		t.Fatalf("wrong password result: ok=%v err=%v", ok, err)
	}
}

func TestPasswordRejectsWeakAndMalformedInputs(t *testing.T) {
	if _, err := HashPassword("too short"); err == nil {
		t.Fatal("expected weak password rejection")
	}
	if _, err := VerifyPassword("not-a-phc-record", "anything at all"); err == nil {
		t.Fatal("expected malformed record rejection")
	}
}
