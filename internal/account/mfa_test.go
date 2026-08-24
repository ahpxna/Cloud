package account

import (
	"bytes"
	"testing"
	"time"
)

func TestTOTPCodeMatchesRFC4226CounterVector(t *testing.T) {
	secret := []byte("12345678901234567890")
	if got := totpCode(secret, 1); got != "287082" {
		t.Fatalf("counter 1 code = %q, want RFC4226 value 287082", got)
	}
}

func TestVerifyTOTPAcceptsOneWindowAndRejectsReplay(t *testing.T) {
	secret := []byte("12345678901234567890")
	now := time.Unix(30*100, 0).UTC()
	previous := totpCode(secret, 99)
	counter, ok := verifyTOTP(secret, previous, now, nil)
	if !ok || counter != 99 {
		t.Fatalf("previous window result counter=%d ok=%v", counter, ok)
	}
	lastUsed := int64(99)
	if _, ok := verifyTOTP(secret, previous, now, &lastUsed); ok {
		t.Fatal("replayed TOTP was accepted")
	}
	future := totpCode(secret, 101)
	counter, ok = verifyTOTP(secret, future, now, &lastUsed)
	if !ok || counter != 101 {
		t.Fatalf("next window result counter=%d ok=%v", counter, ok)
	}
}

func TestMFACipherBindsSecretToUser(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	cipher, err := newMFACipher(key)
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("totp-secret")
	encrypted, nonce, err := cipher.encrypt("user-a", secret)
	if err != nil {
		t.Fatal(err)
	}
	decrypted, err := cipher.decrypt("user-a", encrypted, nonce)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decrypted, secret) {
		t.Fatalf("decrypted secret = %q, want %q", decrypted, secret)
	}
	if _, err := cipher.decrypt("user-b", encrypted, nonce); err == nil {
		t.Fatal("ciphertext decrypted for the wrong user AAD")
	}
}

func TestMFAChallengeTokenRoundTripAndRecoveryNormalization(t *testing.T) {
	raw, expectedHash, err := newMFAChallengeToken()
	if err != nil {
		t.Fatal(err)
	}
	actualHash, ok := challengeHash(raw)
	if !ok || actualHash != expectedHash {
		t.Fatal("challenge token did not round-trip to its persisted hash")
	}
	if _, ok := challengeHash(raw + "!"); ok {
		t.Fatal("malformed challenge token was accepted")
	}

	if recoveryCodeHash("abcde-fghij-klmno") != recoveryCodeHash(" ABCDEFGHIJKLMNO ") {
		t.Fatal("recovery-code hashing is not case/dash/space normalized")
	}
}
