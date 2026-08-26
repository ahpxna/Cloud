package account

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type mfaVerifyRequest struct {
	Challenge    string `json:"challenge"`
	TOTPCode     string `json:"totp_code,omitempty"`
	RecoveryCode string `json:"recovery_code,omitempty"`
}

type mfaCodeRequest struct {
	TOTPCode string `json:"totp_code"`
}

type mfaEnrollRequest struct {
	Password string `json:"password"`
}

func (api *API) issueMFAChallenge(w http.ResponseWriter, ctx context.Context, user User, deviceName string) {
	if api.mfaRepo == nil {
		accountProblem(w, http.StatusServiceUnavailable, "authentication_unavailable", "authentication is temporarily unavailable")
		return
	}
	raw, hash, err := newMFAChallengeToken()
	if err != nil {
		accountProblem(w, http.StatusInternalServerError, "mfa_challenge_failed", "could not create MFA challenge")
		return
	}
	now := api.now().UTC()
	if err := api.mfaRepo.CreateMFAChallenge(ctx, user.ID, deviceName, hash, now, now.Add(mfaChallengeTTL), mfaChallengeAttempts); err != nil {
		if errors.Is(err, ErrMFARateLimited) {
			w.Header().Set("Retry-After", "300")
			accountProblem(w, http.StatusTooManyRequests, "mfa_rate_limited", "too many MFA challenges; try again later")
			return
		}
		api.logger.Error("create MFA challenge", "user_id", user.ID, "error", err)
		accountProblem(w, http.StatusServiceUnavailable, "authentication_unavailable", "authentication is temporarily unavailable")
		return
	}
	accountJSON(w, http.StatusAccepted, map[string]any{
		"mfa_required": true,
		"challenge":    raw,
		"expires_in":   int64(mfaChallengeTTL.Seconds()),
	})
}

func (api *API) mfaEnroll(w http.ResponseWriter, r *http.Request) {
	principal, ok := api.authenticateAccess(w, r)
	if !ok {
		accountProblem(w, http.StatusUnauthorized, "unauthorized", "valid access token required")
		return
	}
	if api.mfaRepo == nil || api.mfaCipher == nil {
		accountProblem(w, http.StatusServiceUnavailable, "mfa_unavailable", "MFA is unavailable")
		return
	}
	var request mfaEnrollRequest
	if !decodeAccountJSON(w, r, &request) {
		return
	}
	if len(request.Password) == 0 || len(request.Password) > 1024 {
		accountProblem(w, http.StatusUnauthorized, "recent_auth_required", "password re-authentication is required to enroll MFA")
		return
	}
	if record, err := api.mfaRepo.TOTPForUser(r.Context(), principal.UserID); err == nil && record.ConfirmedAt != nil {
		accountProblem(w, http.StatusConflict, "mfa_already_enabled", "disable existing MFA before enrolling a new authenticator")
		return
	} else if err != nil && !errors.Is(err, ErrMFANotConfigured) {
		accountProblem(w, http.StatusServiceUnavailable, "mfa_unavailable", "MFA is temporarily unavailable")
		return
	}
	user, err := api.mfaRepo.MFAUserByID(r.Context(), principal.UserID)
	if err != nil {
		accountProblem(w, http.StatusUnauthorized, "unauthorized", "valid access token required")
		return
	}
	if !api.allowSensitiveMFAAction(w, r, principal.UserID, "enroll") {
		return
	}
	select {
	case api.passwordGate <- struct{}{}:
		defer func() { <-api.passwordGate }()
	default:
		w.Header().Set("Retry-After", "1")
		accountProblem(w, http.StatusTooManyRequests, "mfa_enroll_busy", "try again shortly")
		return
	}
	valid, err := VerifyPassword(user.PasswordHash, request.Password)
	if err != nil || !valid {
		accountProblem(w, http.StatusUnauthorized, "recent_auth_required", "password re-authentication is required to enroll MFA")
		return
	}
	secret, encoded, err := newTOTPSecret()
	if err != nil {
		accountProblem(w, http.StatusServiceUnavailable, "mfa_unavailable", "MFA is temporarily unavailable")
		return
	}
	encrypted, nonce, err := api.mfaCipher.encrypt(user.ID, secret)
	for index := range secret {
		secret[index] = 0
	}
	if err != nil {
		accountProblem(w, http.StatusServiceUnavailable, "mfa_unavailable", "MFA is temporarily unavailable")
		return
	}
	if err := api.mfaRepo.SavePendingTOTP(r.Context(), user.ID, encrypted, nonce); err != nil {
		if errors.Is(err, ErrMFAInvalid) {
			accountProblem(w, http.StatusConflict, "mfa_already_enabled", "disable existing MFA before enrolling a new authenticator")
			return
		}
		api.logger.Error("save pending MFA", "user_id", user.ID, "error", err)
		accountProblem(w, http.StatusServiceUnavailable, "mfa_unavailable", "MFA is temporarily unavailable")
		return
	}
	api.clearSensitiveMFAAction(r.Context(), principal.UserID, "enroll")
	accountJSON(w, http.StatusOK, map[string]any{
		"secret":      encoded,
		"otpauth_uri": totpURI(user.Email, encoded),
	})
}

func (api *API) mfaConfirm(w http.ResponseWriter, r *http.Request) {
	principal, ok := api.authenticateAccess(w, r)
	if !ok {
		accountProblem(w, http.StatusUnauthorized, "unauthorized", "valid access token required")
		return
	}
	if api.mfaRepo == nil || api.mfaCipher == nil {
		accountProblem(w, http.StatusServiceUnavailable, "mfa_unavailable", "MFA is unavailable")
		return
	}
	var request mfaCodeRequest
	if !decodeAccountJSON(w, r, &request) {
		return
	}
	record, err := api.mfaRepo.TOTPForUser(r.Context(), principal.UserID)
	if err != nil || record.ConfirmedAt != nil {
		accountProblem(w, http.StatusConflict, "mfa_enrollment_missing", "start a new MFA enrollment before confirming")
		return
	}
	if !api.allowSensitiveMFAAction(w, r, principal.UserID, "confirm") {
		return
	}
	secret, err := api.mfaCipher.decrypt(principal.UserID, record.EncryptedSecret, record.Nonce)
	if err != nil {
		accountProblem(w, http.StatusInternalServerError, "mfa_confirm_failed", "could not confirm MFA")
		return
	}
	counter, valid := verifyTOTP(secret, request.TOTPCode, api.now().UTC(), nil)
	for index := range secret {
		secret[index] = 0
	}
	if !valid {
		accountProblem(w, http.StatusUnauthorized, "invalid_mfa_code", "MFA code is invalid")
		return
	}
	codes, hashes, err := newRecoveryCodes()
	if err != nil {
		accountProblem(w, http.StatusInternalServerError, "mfa_confirm_failed", "could not confirm MFA")
		return
	}
	if err := api.mfaRepo.ConfirmTOTP(r.Context(), principal.UserID, counter, hashes, record.Nonce); err != nil {
		api.logger.Error("confirm MFA", "user_id", principal.UserID, "error", err)
		accountProblem(w, http.StatusServiceUnavailable, "mfa_unavailable", "MFA is temporarily unavailable")
		return
	}
	api.clearSensitiveMFAAction(r.Context(), principal.UserID, "confirm")
	accountJSON(w, http.StatusOK, map[string]any{"recovery_codes": codes})
}

func (api *API) mfaVerify(w http.ResponseWriter, r *http.Request) {
	if api.mfaRepo == nil || api.mfaCipher == nil {
		accountProblem(w, http.StatusServiceUnavailable, "mfa_unavailable", "MFA is unavailable")
		return
	}
	var request mfaVerifyRequest
	if !decodeAccountJSON(w, r, &request) {
		return
	}
	hash, ok := challengeHash(strings.TrimSpace(request.Challenge))
	if !ok || (request.TOTPCode == "") == (request.RecoveryCode == "") {
		accountProblem(w, http.StatusUnauthorized, "invalid_mfa_challenge", "MFA challenge is invalid or expired")
		return
	}
	now := api.now().UTC()
	challenge, err := api.mfaRepo.MFAChallengeByHash(r.Context(), hash, now)
	if err != nil {
		if errors.Is(err, ErrMFAChallenge) {
			accountProblem(w, http.StatusUnauthorized, "invalid_mfa_challenge", "MFA challenge is invalid or expired")
		} else {
			api.logger.Error("load MFA challenge", "error", err)
			accountProblem(w, http.StatusServiceUnavailable, "authentication_unavailable", "authentication is temporarily unavailable")
		}
		return
	}

	var user User
	var deviceName string
	if request.TOTPCode != "" {
		secret, decryptErr := api.mfaCipher.decrypt(challenge.User.ID, challenge.EncryptedSecret, challenge.Nonce)
		if decryptErr != nil {
			accountProblem(w, http.StatusServiceUnavailable, "authentication_unavailable", "authentication is temporarily unavailable")
			return
		}
		counter, valid := verifyTOTP(secret, request.TOTPCode, now, challenge.LastUsedCounter)
		for index := range secret {
			secret[index] = 0
		}
		if !valid {
			api.rejectMFAAttempt(w, r, hash, now)
			return
		}
		user, deviceName, err = api.mfaRepo.CompleteMFATOTPChallenge(r.Context(), hash, now, counter)
	} else {
		user, deviceName, err = api.mfaRepo.CompleteMFARecoveryChallenge(r.Context(), hash, recoveryCodeHash(request.RecoveryCode), now)
	}
	if err != nil {
		if errors.Is(err, ErrMFAInvalid) || errors.Is(err, ErrMFAReplay) {
			api.rejectMFAAttempt(w, r, hash, now)
			return
		}
		if errors.Is(err, ErrMFAChallenge) {
			accountProblem(w, http.StatusUnauthorized, "invalid_mfa_challenge", "MFA challenge is invalid or expired")
		} else {
			api.logger.Error("complete MFA challenge", "error", err)
			accountProblem(w, http.StatusServiceUnavailable, "authentication_unavailable", "authentication is temporarily unavailable")
		}
		return
	}
	api.createTokenPair(w, r.Context(), user, deviceName, true)
}

func (api *API) rejectMFAAttempt(w http.ResponseWriter, r *http.Request, hash [32]byte, now time.Time) {
	remaining, err := api.mfaRepo.FailMFAChallenge(r.Context(), hash, now)
	if err != nil {
		accountProblem(w, http.StatusUnauthorized, "invalid_mfa_challenge", "MFA challenge is invalid or expired")
		return
	}
	accountJSON(w, http.StatusUnauthorized, map[string]any{
		"status":             http.StatusUnauthorized,
		"code":               "invalid_mfa_code",
		"detail":             "MFA code is invalid",
		"attempts_remaining": remaining,
	})
}

func (api *API) mfaRecovery(w http.ResponseWriter, r *http.Request) {
	principal, ok := api.authenticateAccess(w, r)
	if !ok {
		accountProblem(w, http.StatusUnauthorized, "unauthorized", "valid access token required")
		return
	}
	if api.mfaRepo == nil || api.mfaCipher == nil {
		accountProblem(w, http.StatusServiceUnavailable, "mfa_unavailable", "MFA is unavailable")
		return
	}
	var request mfaCodeRequest
	if !decodeAccountJSON(w, r, &request) {
		return
	}
	if !api.allowSensitiveMFAAction(w, r, principal.UserID, "recovery") {
		return
	}
	counter, valid, err := api.verifyCurrentTOTP(r.Context(), principal.UserID, request.TOTPCode)
	if err != nil || !valid {
		accountProblem(w, http.StatusUnauthorized, "invalid_mfa_code", "MFA code is invalid")
		return
	}
	codes, hashes, err := newRecoveryCodes()
	if err != nil {
		accountProblem(w, http.StatusServiceUnavailable, "mfa_unavailable", "MFA is temporarily unavailable")
		return
	}
	if err := api.mfaRepo.RotateRecoveryCodes(r.Context(), principal.UserID, counter, hashes); err != nil {
		if errors.Is(err, ErrMFAReplay) || errors.Is(err, ErrMFANotConfigured) {
			accountProblem(w, http.StatusUnauthorized, "invalid_mfa_code", "MFA code is invalid")
			return
		}
		api.logger.Error("rotate MFA recovery codes", "user_id", principal.UserID, "error", err)
		accountProblem(w, http.StatusServiceUnavailable, "mfa_unavailable", "MFA is temporarily unavailable")
		return
	}
	api.clearSensitiveMFAAction(r.Context(), principal.UserID, "recovery")
	accountJSON(w, http.StatusOK, map[string]any{"recovery_codes": codes})
}

func (api *API) mfaDisable(w http.ResponseWriter, r *http.Request) {
	principal, ok := api.authenticateAccess(w, r)
	if !ok {
		accountProblem(w, http.StatusUnauthorized, "unauthorized", "valid access token required")
		return
	}
	if api.mfaRepo == nil || api.mfaCipher == nil {
		accountProblem(w, http.StatusServiceUnavailable, "mfa_unavailable", "MFA is unavailable")
		return
	}
	var request mfaCodeRequest
	if !decodeAccountJSON(w, r, &request) {
		return
	}
	if !api.allowSensitiveMFAAction(w, r, principal.UserID, "disable") {
		return
	}
	counter, valid, err := api.verifyCurrentTOTP(r.Context(), principal.UserID, request.TOTPCode)
	if err != nil || !valid {
		accountProblem(w, http.StatusUnauthorized, "invalid_mfa_code", "MFA code is invalid")
		return
	}
	if err := api.mfaRepo.DisableMFA(r.Context(), principal.UserID, counter); err != nil {
		if errors.Is(err, ErrMFAReplay) || errors.Is(err, ErrMFANotConfigured) {
			accountProblem(w, http.StatusUnauthorized, "invalid_mfa_code", "MFA code is invalid")
			return
		}
		accountProblem(w, http.StatusServiceUnavailable, "mfa_unavailable", "MFA is temporarily unavailable")
		return
	}
	api.clearSensitiveMFAAction(r.Context(), principal.UserID, "disable")
	w.WriteHeader(http.StatusNoContent)
}

func (api *API) allowSensitiveMFAAction(w http.ResponseWriter, r *http.Request, userID, action string) bool {
	now := api.now().UTC()
	allowed, retryAfter, err := api.mfaRepo.RecordMFAActionAttempt(
		r.Context(), userID, action, now, mfaActionWindow, mfaActionAttempts,
	)
	if err != nil {
		api.logger.Error("record MFA action throttle", "user_id", userID, "action", action, "error", err)
		accountProblem(w, http.StatusServiceUnavailable, "mfa_unavailable", "MFA is temporarily unavailable")
		return false
	}
	if !allowed {
		seconds := int64(retryAfter.Round(time.Second).Seconds())
		if seconds < 1 {
			seconds = 1
		}
		w.Header().Set("Retry-After", fmt.Sprint(seconds))
		accountProblem(w, http.StatusTooManyRequests, "mfa_rate_limited", "too many MFA attempts; try again later")
		return false
	}
	return true
}

func (api *API) clearSensitiveMFAAction(ctx context.Context, userID, action string) {
	if err := api.mfaRepo.ClearMFAActionAttempts(ctx, userID, action); err != nil {
		// The protected action already committed. Do not turn a successful MFA
		// mutation into a false client failure merely because limiter cleanup
		// failed; retain the row fail-closed and surface the operational error.
		api.logger.Error("clear MFA action throttle", "user_id", userID, "action", action, "error", err)
	}
}

func (api *API) verifyCurrentTOTP(ctx context.Context, userID, code string) (int64, bool, error) {
	record, err := api.mfaRepo.TOTPForUser(ctx, userID)
	if err != nil || record.ConfirmedAt == nil {
		return 0, false, err
	}
	secret, err := api.mfaCipher.decrypt(userID, record.EncryptedSecret, record.Nonce)
	if err != nil {
		return 0, false, err
	}
	counter, valid := verifyTOTP(secret, code, api.now().UTC(), record.LastUsedCounter)
	for index := range secret {
		secret[index] = 0
	}
	return counter, valid, nil
}
