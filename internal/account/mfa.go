package account

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	mfaChallengeTTL         = 5 * time.Minute
	mfaChallengeAttempts    = 5
	mfaChallengeIssueWindow = time.Hour
	mfaChallengeIssueLimit  = 12
	mfaChallengeRetention   = 24 * time.Hour
	mfaActionWindow         = 5 * time.Minute
	mfaActionAttempts       = 5
	mfaActionRetention      = 24 * time.Hour
	totpPeriodSeconds       = int64(30)
	totpDigits              = 6
	recoveryCodeCount       = 10
)

var (
	ErrMFANotConfigured     = errors.New("mfa is not configured")
	ErrMFAInvalid           = errors.New("mfa verification failed")
	ErrMFAChallenge         = errors.New("mfa challenge is invalid or expired")
	ErrMFARateLimited       = errors.New("mfa challenge issuance is rate limited")
	ErrMFAActionRateLimited = errors.New("mfa sensitive action is rate limited")
	ErrMFAReplay            = errors.New("mfa code replay detected")
)

type MFARecord struct {
	EncryptedSecret []byte
	Nonce           []byte
	ConfirmedAt     *time.Time
	LastUsedCounter *int64
}

type MFAChallenge struct {
	User            User
	DeviceName      string
	EncryptedSecret []byte
	Nonce           []byte
	LastUsedCounter *int64
	ExpiresAt       time.Time
	Attempts        int
}

type MFARepository interface {
	MFAUserByID(context.Context, string) (User, error)
	TOTPForUser(context.Context, string) (MFARecord, error)
	SavePendingTOTP(context.Context, string, []byte, []byte) error
	ConfirmTOTP(context.Context, string, int64, [][32]byte) error
	CreateMFAChallenge(context.Context, string, string, [32]byte, time.Time, time.Time, int) error
	MFAChallengeByHash(context.Context, [32]byte, time.Time) (MFAChallenge, error)
	FailMFAChallenge(context.Context, [32]byte, time.Time) (int, error)
	CompleteMFATOTPChallenge(context.Context, [32]byte, time.Time, int64) (User, string, error)
	CompleteMFARecoveryChallenge(context.Context, [32]byte, [32]byte, time.Time) (User, string, error)
	RecordMFAActionAttempt(context.Context, string, string, time.Time, time.Duration, int) (bool, time.Duration, error)
	ClearMFAActionAttempts(context.Context, string, string) error
	RotateRecoveryCodes(context.Context, string, int64, [][32]byte) error
	DisableMFA(context.Context, string, int64) error
}

type mfaCipher struct {
	aead cipher.AEAD
}

func newMFACipher(key []byte) (*mfaCipher, error) {
	if len(key) != 32 {
		return nil, errors.New("MFA encryption key must decode to exactly 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &mfaCipher{aead: aead}, nil
}

func (cipher *mfaCipher) encrypt(userID string, secret []byte) ([]byte, []byte, error) {
	nonce := make([]byte, cipher.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	sealed := cipher.aead.Seal(nil, nonce, secret, []byte(userID))
	return sealed, nonce, nil
}

func (cipher *mfaCipher) decrypt(userID string, encrypted, nonce []byte) ([]byte, error) {
	if len(nonce) != cipher.aead.NonceSize() {
		return nil, ErrMFAInvalid
	}
	secret, err := cipher.aead.Open(nil, nonce, encrypted, []byte(userID))
	if err != nil {
		return nil, ErrMFAInvalid
	}
	return secret, nil
}

func newTOTPSecret() ([]byte, string, error) {
	secret := make([]byte, 20)
	if _, err := rand.Read(secret); err != nil {
		return nil, "", err
	}
	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(secret)
	return secret, encoded, nil
}

func totpURI(email, encodedSecret string) string {
	label := url.PathEscape("Family Photo Cloud:" + email)
	query := url.Values{}
	query.Set("secret", encodedSecret)
	query.Set("issuer", "Family Photo Cloud")
	query.Set("algorithm", "SHA1")
	query.Set("digits", strconv.Itoa(totpDigits))
	query.Set("period", strconv.FormatInt(totpPeriodSeconds, 10))
	return "otpauth://totp/" + label + "?" + query.Encode()
}

func verifyTOTP(secret []byte, code string, now time.Time, lastUsed *int64) (int64, bool) {
	code = strings.TrimSpace(code)
	if len(code) != totpDigits {
		return 0, false
	}
	for _, character := range code {
		if character < '0' || character > '9' {
			return 0, false
		}
	}
	current := now.Unix() / totpPeriodSeconds
	for _, counter := range []int64{current - 1, current, current + 1} {
		if counter < 0 || (lastUsed != nil && counter <= *lastUsed) {
			continue
		}
		candidate := totpCode(secret, counter)
		if subtle.ConstantTimeCompare([]byte(candidate), []byte(code)) == 1 {
			return counter, true
		}
	}
	return 0, false
}

func totpCode(secret []byte, counter int64) string {
	var message [8]byte
	binary.BigEndian.PutUint64(message[:], uint64(counter))
	mac := hmac.New(sha1.New, secret)
	_, _ = mac.Write(message[:])
	digest := mac.Sum(nil)
	offset := digest[len(digest)-1] & 0x0f
	binaryCode := (uint32(digest[offset])&0x7f)<<24 |
		uint32(digest[offset+1])<<16 |
		uint32(digest[offset+2])<<8 |
		uint32(digest[offset+3])
	return fmt.Sprintf("%06d", binaryCode%1_000_000)
}

func newMFAChallengeToken() (string, [32]byte, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", [32]byte{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(raw[:])
	return token, sha256.Sum256([]byte(token)), nil
}

func challengeHash(raw string) ([32]byte, bool) {
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(decoded) != 32 {
		return [32]byte{}, false
	}
	return sha256.Sum256([]byte(raw)), true
}

func newRecoveryCodes() ([]string, [][32]byte, error) {
	codes := make([]string, 0, recoveryCodeCount)
	hashes := make([][32]byte, 0, recoveryCodeCount)
	for range recoveryCodeCount {
		var raw [16]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return nil, nil, err
		}
		encoded := strings.ToUpper(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw[:]))
		code := encoded[:5] + "-" + encoded[5:10] + "-" + encoded[10:15] + "-" + encoded[15:20] + "-" + encoded[20:]
		codes = append(codes, code)
		hashes = append(hashes, recoveryCodeHash(code))
	}
	return codes, hashes, nil
}

func recoveryCodeHash(code string) [32]byte {
	normalized := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(code), "-", ""))
	return sha256.Sum256([]byte(normalized))
}
