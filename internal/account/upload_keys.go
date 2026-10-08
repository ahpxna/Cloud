package account

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

const (
	uploadKeyPrefix         = "fpcu_"
	maxActiveUploadKeys     = 10
	uploadKeyTouchInterval  = time.Minute
	uploadKeyNameMaxRunes   = 100
	uploadKeyRawRandomBytes = 32
)

var (
	ErrUploadKeyInvalid = errors.New("upload key is invalid or revoked")
	ErrUploadKeyLimit   = errors.New("too many active upload keys")
)

// UploadKey is the listing view of a device upload key. The raw key is shown
// only once, when it is created.
type UploadKey struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

// UploadKeyRepository stores upload-only keys for iOS Shortcuts.
type UploadKeyRepository interface {
	CreateUploadKey(ctx context.Context, userID, name string, keyHash [32]byte) (UploadKey, error)
	ListUploadKeys(ctx context.Context, userID string) ([]UploadKey, error)
	RevokeUploadKey(ctx context.Context, userID, keyID string) error
	// UploadKeyOwner returns the owner and key ID of an active key whose owner
	// account is active, and records its use.
	UploadKeyOwner(ctx context.Context, keyHash [32]byte, now time.Time) (string, string, error)
}

var _ UploadKeyRepository = (*PostgresRepository)(nil)

func newUploadKey() (string, [32]byte, error) {
	var raw [uploadKeyRawRandomBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", [32]byte{}, err
	}
	key := uploadKeyPrefix + base64.RawURLEncoding.EncodeToString(raw[:])
	return key, sha256.Sum256([]byte(key)), nil
}

// UploadKeyHash validates the key format before hashing, so arbitrary bearer
// strings (for example JWT access tokens) never reach the key lookup.
func UploadKeyHash(raw string) ([32]byte, bool) {
	if !strings.HasPrefix(raw, uploadKeyPrefix) {
		return [32]byte{}, false
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(strings.TrimPrefix(raw, uploadKeyPrefix))
	if err != nil || len(decoded) != uploadKeyRawRandomBytes {
		return [32]byte{}, false
	}
	return sha256.Sum256([]byte(raw)), true
}

func validUploadKeyName(name string) bool {
	if !utf8.ValidString(name) {
		return false
	}
	count := utf8.RuneCountInString(name)
	if count < 1 || count > uploadKeyNameMaxRunes {
		return false
	}
	for _, character := range name {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func (r *PostgresRepository) CreateUploadKey(ctx context.Context, userID, name string, keyHash [32]byte) (UploadKey, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return UploadKey{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockUserTransaction(ctx, tx, userID); err != nil {
		return UploadKey{}, err
	}
	var active int
	if err := tx.QueryRow(ctx, `
        SELECT count(*) FROM device_upload_keys WHERE user_id = $1::uuid AND revoked_at IS NULL`, userID,
	).Scan(&active); err != nil {
		return UploadKey{}, err
	}
	if active >= maxActiveUploadKeys {
		return UploadKey{}, ErrUploadKeyLimit
	}
	var key UploadKey
	if err := tx.QueryRow(ctx, `
        INSERT INTO device_upload_keys (user_id, name, key_sha256)
        VALUES ($1::uuid, $2, $3)
        RETURNING id::text, name, created_at`, userID, name, keyHash[:],
	).Scan(&key.ID, &key.Name, &key.CreatedAt); err != nil {
		return UploadKey{}, err
	}
	return key, tx.Commit(ctx)
}

func (r *PostgresRepository) ListUploadKeys(ctx context.Context, userID string) ([]UploadKey, error) {
	rows, err := r.pool.Query(ctx, `
        SELECT id::text, name, created_at, last_used_at
        FROM device_upload_keys
        WHERE user_id = $1::uuid AND revoked_at IS NULL
        ORDER BY created_at DESC, id DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keys := []UploadKey{}
	for rows.Next() {
		var key UploadKey
		if err := rows.Scan(&key.ID, &key.Name, &key.CreatedAt, &key.LastUsedAt); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

func (r *PostgresRepository) RevokeUploadKey(ctx context.Context, userID, keyID string) error {
	command, err := r.pool.Exec(ctx, `
        UPDATE device_upload_keys SET revoked_at = now()
        WHERE id = $1::uuid AND user_id = $2::uuid AND revoked_at IS NULL`, keyID, userID)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrUploadKeyInvalid
	}
	return nil
}

func (r *PostgresRepository) UploadKeyOwner(ctx context.Context, keyHash [32]byte, now time.Time) (string, string, error) {
	var userID, keyID string
	var lastUsed *time.Time
	err := r.pool.QueryRow(ctx, `
        SELECT key.user_id::text, key.id::text, key.last_used_at
        FROM device_upload_keys AS key
        JOIN users AS account ON account.id = key.user_id
        WHERE key.key_sha256 = $1 AND key.revoked_at IS NULL
          AND account.state = 'active' AND account.deleted_at IS NULL`, keyHash[:],
	).Scan(&userID, &keyID, &lastUsed)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrUploadKeyInvalid
	}
	if err != nil {
		return "", "", err
	}
	// Coarse last-use tracking avoids a write per uploaded photo.
	if lastUsed == nil || now.Sub(*lastUsed) >= uploadKeyTouchInterval {
		if _, err := r.pool.Exec(ctx, `UPDATE device_upload_keys SET last_used_at = $2 WHERE id = $1::uuid`, keyID, now); err != nil {
			return "", "", err
		}
	}
	return userID, keyID, nil
}

type uploadKeyCreateRequest struct {
	Name string `json:"name"`
}

func (api *API) listUploadKeys(w http.ResponseWriter, r *http.Request) {
	principal, ok := api.authenticateAccess(w, r)
	if !ok {
		return
	}
	if api.uploadKeys == nil {
		accountProblem(w, http.StatusServiceUnavailable, "upload_keys_unavailable", "upload keys are unavailable")
		return
	}
	keys, err := api.uploadKeys.ListUploadKeys(r.Context(), principal.UserID)
	if err != nil {
		api.logger.Error("list upload keys", "user_id", principal.UserID, "error", err)
		accountProblem(w, http.StatusServiceUnavailable, "upload_keys_unavailable", "upload keys are temporarily unavailable")
		return
	}
	accountJSON(w, http.StatusOK, map[string]any{"upload_keys": keys})
}

func (api *API) createUploadKey(w http.ResponseWriter, r *http.Request) {
	principal, ok := api.authenticateAccess(w, r)
	if !ok {
		return
	}
	if api.uploadKeys == nil {
		accountProblem(w, http.StatusServiceUnavailable, "upload_keys_unavailable", "upload keys are unavailable")
		return
	}
	var request uploadKeyCreateRequest
	if !decodeAccountJSON(w, r, &request) {
		return
	}
	name := strings.TrimSpace(request.Name)
	if !validUploadKeyName(name) {
		accountProblem(w, http.StatusUnprocessableEntity, "invalid_upload_key_name", "name must be 1 to 100 printable characters")
		return
	}
	raw, hash, err := newUploadKey()
	if err != nil {
		accountProblem(w, http.StatusInternalServerError, "upload_key_failed", "could not create upload key")
		return
	}
	key, err := api.uploadKeys.CreateUploadKey(r.Context(), principal.UserID, name, hash)
	if errors.Is(err, ErrUploadKeyLimit) {
		accountProblem(w, http.StatusConflict, "upload_key_limit", "revoke an unused upload key first")
		return
	}
	if err != nil {
		api.logger.Error("create upload key", "user_id", principal.UserID, "error", err)
		accountProblem(w, http.StatusServiceUnavailable, "upload_keys_unavailable", "upload keys are temporarily unavailable")
		return
	}
	accountJSON(w, http.StatusCreated, map[string]any{
		"id":         key.ID,
		"name":       key.Name,
		"created_at": key.CreatedAt,
		"upload_key": raw,
	})
}

func (api *API) revokeUploadKey(w http.ResponseWriter, r *http.Request, keyID string) {
	if !validRotationRequestID(keyID) {
		accountProblem(w, http.StatusNotFound, "not_found", "upload key not found")
		return
	}
	principal, ok := api.authenticateAccess(w, r)
	if !ok {
		return
	}
	if api.uploadKeys == nil {
		accountProblem(w, http.StatusServiceUnavailable, "upload_keys_unavailable", "upload keys are unavailable")
		return
	}
	if err := api.uploadKeys.RevokeUploadKey(r.Context(), principal.UserID, keyID); err != nil {
		if errors.Is(err, ErrUploadKeyInvalid) {
			accountProblem(w, http.StatusNotFound, "not_found", "upload key not found")
			return
		}
		api.logger.Error("revoke upload key", "user_id", principal.UserID, "error", err)
		accountProblem(w, http.StatusServiceUnavailable, "upload_keys_unavailable", "upload keys are temporarily unavailable")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
