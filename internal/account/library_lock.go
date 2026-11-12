package account

import (
	"context"
	"errors"
	"net/http"
	"time"

	"family-photo-cloud/internal/auth"
	"github.com/jackc/pgx/v5"
)

type LibraryLockRepository interface {
	auth.LibraryLockReader
	MFAUserByID(context.Context, string) (User, error)
	ChangeLibraryLock(context.Context, string, string, *bool, bool, string, *int64) error
}

const libraryDeviceMatch = `(device.id = $2::uuid OR EXISTS (
 SELECT 1 FROM user_sessions generation WHERE generation.id = $2::uuid AND generation.device_session_id = device.id))`

func (r *PostgresRepository) LibraryLockState(ctx context.Context, userID, sessionID string) (auth.LibraryLockState, error) {
	var state auth.LibraryLockState
	err := r.pool.QueryRow(ctx, `SELECT account.library_lock_enabled, device.library_lock_version,
 CASE WHEN device.library_unlocked_until > now() THEN device.library_unlocked_until END
 FROM users account JOIN device_sessions device ON device.user_id = account.id
 WHERE account.id = $1::uuid AND account.state = 'active' AND account.deleted_at IS NULL
 AND device.revoked_at IS NULL AND device.expires_at > now() AND `+libraryDeviceMatch, userID, sessionID).Scan(&state.Enabled, &state.Version, &state.UnlockedUntil)
	if errors.Is(err, pgx.ErrNoRows) {
		return state, ErrInvalidCredentials
	}
	if !state.Enabled {
		state.UnlockedUntil = nil
	}
	return state, err
}

var errLibraryLockChanged = errors.New("library lock changed")

func (r *PostgresRepository) ChangeLibraryLock(ctx context.Context, userID, sessionID string, enabled *bool, unlock bool, expectedHash string, expectedVersion *int64) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var hash string
	err = tx.QueryRow(ctx, `SELECT password_hash FROM users WHERE id=$1::uuid AND state='active' AND deleted_at IS NULL FOR UPDATE`, userID).Scan(&hash)
	if err != nil {
		return err
	}
	if (unlock || (enabled != nil && !*enabled)) && hash != expectedHash {
		return ErrInvalidCredentials
	}
	var deviceID string
	var version int64
	err = tx.QueryRow(ctx, `SELECT device.id::text, device.library_lock_version FROM device_sessions device WHERE device.user_id=$1::uuid
 AND device.revoked_at IS NULL AND device.expires_at > now() AND `+libraryDeviceMatch+` FOR UPDATE`, userID, sessionID).Scan(&deviceID, &version)
	if err != nil {
		return err
	}
	if unlock && (expectedVersion == nil || *expectedVersion != version) {
		return errLibraryLockChanged
	}
	if enabled != nil {
		_, err = tx.Exec(ctx, `UPDATE users SET library_lock_enabled=$2 WHERE id=$1::uuid`, userID, *enabled)
		if err == nil {
			_, err = tx.Exec(ctx, `UPDATE device_sessions SET library_unlocked_until=NULL, library_lock_version=library_lock_version+1 WHERE user_id=$1::uuid`, userID)
		}
	} else {
		var until *time.Time
		if unlock {
			value := time.Now().UTC().Add(10 * time.Minute)
			until = &value
		}
		_, err = tx.Exec(ctx, `UPDATE device_sessions SET library_unlocked_until=$2, library_lock_version=library_lock_version+1 WHERE id=$1::uuid`, deviceID, until)
	}
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (api *API) libraryLock(w http.ResponseWriter, r *http.Request) {
	principal, ok := api.authenticateAccess(w, r)
	if !ok {
		return
	}
	repo, ok := api.repository.(LibraryLockRepository)
	if !ok {
		accountProblem(w, 503, "authentication_unavailable", "authentication is temporarily unavailable")
		return
	}
	if r.Method != http.MethodGet {
		var request struct {
			Enabled  *bool  `json:"enabled"`
			Password string `json:"password"`
			Version  *int64 `json:"version"`
		}
		if !decodeAccountJSON(w, r, &request) {
			return
		}
		setting := r.URL.Path == "/v1/auth/library-lock"
		unlock := r.URL.Path == "/v1/auth/library-lock/unlock"
		if (setting && request.Enabled == nil) || (!setting && request.Enabled != nil) {
			accountProblem(w, 400, "invalid_request", "invalid lock request")
			return
		}
		var expectedHash string
		if unlock || (request.Enabled != nil && !*request.Enabled) {
			user, err := repo.MFAUserByID(r.Context(), principal.UserID)
			if err != nil {
				accountProblem(w, 503, "authentication_unavailable", "authentication is temporarily unavailable")
				return
			}
			verified, valid := api.verifyLoginPassword(w, r, user.Email, request.Password)
			if !valid {
				return
			}
			expectedHash = verified.PasswordHash
		}
		if err := repo.ChangeLibraryLock(r.Context(), principal.UserID, principal.SessionID, request.Enabled, unlock, expectedHash, request.Version); err != nil {
			if errors.Is(err, errLibraryLockChanged) {
				accountProblem(w, 409, "library_lock_changed", "library lock changed; try again")
			} else if errors.Is(err, ErrInvalidCredentials) {
				accountProblem(w, 401, "invalid_credentials", "email or password is incorrect")
			} else {
				accountProblem(w, 503, "authentication_unavailable", "authentication is temporarily unavailable")
			}
			return
		}
	}
	state, err := repo.LibraryLockState(r.Context(), principal.UserID, principal.SessionID)
	if err != nil {
		accountProblem(w, 503, "authentication_unavailable", "authentication is temporarily unavailable")
		return
	}
	accountJSON(w, http.StatusOK, state)
}
