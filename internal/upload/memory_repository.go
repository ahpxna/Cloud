package upload

import (
	"context"
	"crypto/subtle"
	"fmt"
	"sort"
	"sync"
	"time"
)

// MemoryRepository is used by protocol and failure-path tests. Production uses
// PostgresRepository so state survives process restarts.
type MemoryRepository struct {
	mu              sync.RWMutex
	sessions        map[string]Session
	byClient        map[string]string
	assets          map[string]Asset
	createThrottles map[string]createThrottle
}

type createThrottle struct {
	windowStart time.Time
	count       int
}

func NewMemoryRepository() *MemoryRepository {
	return &MemoryRepository{
		sessions:        make(map[string]Session),
		byClient:        make(map[string]string),
		assets:          make(map[string]Asset),
		createThrottles: make(map[string]createThrottle),
	}
}

func (r *MemoryRepository) CreateSession(_ context.Context, input CreateSessionInput) (Session, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	clientKey := input.OwnerID + "\x00" + input.ClientAssetID
	var expiredExisting *Session
	if id, ok := r.byClient[clientKey]; ok {
		existing := r.sessions[id]
		if existing.OriginalFilename != input.OriginalFilename ||
			existing.MediaType != input.MediaType ||
			existing.ExpectedSize != input.ExpectedSize ||
			subtle.ConstantTimeCompare(existing.ClientSHA256[:], input.ClientSHA256[:]) != 1 {
			return Session{}, false, ErrConflict
		}
		if existing.State != StateExpired {
			return existing, false, nil
		}
		expiredExisting = &existing
	}

	var pendingThrottle *createThrottle
	if input.MaxCreatesPerWindow > 0 && input.CreateWindow > 0 {
		now := input.Now
		if now.IsZero() {
			now = time.Now().UTC()
		}
		entry := r.createThrottles[input.OwnerID]
		if entry.windowStart.IsZero() || !now.Before(entry.windowStart.Add(input.CreateWindow)) {
			entry = createThrottle{windowStart: now}
		}
		if entry.count >= input.MaxCreatesPerWindow {
			return Session{}, false, ErrCreateRateLimit
		}
		entry.count++
		pendingThrottle = &entry
	}

	if input.MaxActiveSessions > 0 {
		active := 0
		for _, existing := range r.sessions {
			if existing.OwnerID == input.OwnerID && (existing.State == StateCreated || existing.State == StateUploading || existing.State == StateReceived || existing.State == StateVerifying || existing.State == StateVerified || existing.State == StateCommitting || existing.State == StateQuarantining) {
				active++
			}
		}
		if active >= input.MaxActiveSessions {
			return Session{}, false, ErrSessionLimit
		}
	}

	if pendingThrottle != nil {
		r.createThrottles[input.OwnerID] = *pendingThrottle
	}

	if expiredExisting != nil {
		existing := *expiredExisting
		existing.State = StateCreated
		existing.ReceivedSize = 0
		existing.ServerSHA256 = nil
		existing.TransportResource = ""
		existing.FinalStorageKey = ""
		existing.AssetID = ""
		existing.ExpiresAt = input.ExpiresAt
		r.sessions[existing.ID] = existing
		return existing, false, nil
	}

	id, err := NewID()
	if err != nil {
		return Session{}, false, err
	}
	session := Session{
		ID:               id,
		OwnerID:          input.OwnerID,
		ClientAssetID:    input.ClientAssetID,
		OriginalFilename: input.OriginalFilename,
		MediaType:        input.MediaType,
		ExpectedSize:     input.ExpectedSize,
		ClientSHA256:     input.ClientSHA256,
		State:            StateCreated,
		ExpiresAt:        input.ExpiresAt,
	}
	r.sessions[id] = session
	r.byClient[clientKey] = id
	return session, true, nil
}

func (r *MemoryRepository) SessionByID(_ context.Context, id string) (Session, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	session, ok := r.sessions[id]
	if !ok {
		return Session{}, ErrNotFound
	}
	return session, nil
}

func (r *MemoryRepository) ClaimTusCreation(_ context.Context, id, ownerID string, size int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	session, ok := r.sessions[id]
	if !ok {
		return ErrNotFound
	}
	if session.OwnerID != ownerID {
		return ErrOwnerMismatch
	}
	if session.State != StateCreated || session.ExpectedSize != size {
		return ErrInvalidState
	}
	session.State = StateUploading
	session.TransportResource = id
	r.sessions[id] = session
	return nil
}

func (r *MemoryRepository) RecordProgress(_ context.Context, id string, offset int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	session, ok := r.sessions[id]
	if !ok {
		return ErrNotFound
	}
	if session.State != StateUploading || offset < session.ReceivedSize || offset > session.ExpectedSize {
		return ErrInvalidState
	}
	session.ReceivedSize = offset
	r.sessions[id] = session
	return nil
}

func (r *MemoryRepository) MarkReceived(_ context.Context, id string, size int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	session, ok := r.sessions[id]
	if !ok {
		return ErrNotFound
	}
	if session.State != StateUploading || size != session.ExpectedSize {
		return ErrInvalidState
	}
	session.ReceivedSize = size
	session.State = StateReceived
	r.sessions[id] = session
	return nil
}

// MemoryRepository executes verification synchronously in tests, so a durable
// lease is not required. Keep the same claiming contract as PostgreSQL to make
// failure-path tests exercise the production scheduler shape.
func (r *MemoryRepository) ClaimVerification(_ context.Context, _ string, _ time.Duration, limit int) ([]Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]Session, 0, limit)
	for id, session := range r.sessions {
		switch session.State {
		case StateReceived:
			claim, err := NewID()
			if err != nil {
				return nil, err
			}
			session.State = StateVerifying
			session.VerificationClaim = claim
			r.sessions[id] = session
			result = append(result, session)
		case StateVerifying, StateVerified, StateCommitting, StateQuarantining:
			// Memory tests do not model time-based expiry; never hand an active
			// claim to a second worker.
		}
		if len(result) == limit {
			break
		}
	}
	return result, nil
}

func (r *MemoryRepository) RenewVerificationLease(_ context.Context, _ string, _ string, _ string, _ time.Duration) error {
	return nil
}

func (r *MemoryRepository) BeginVerification(ctx context.Context, id string) (Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	session, ok := r.sessions[id]
	if !ok {
		return Session{}, ErrNotFound
	}
	if !matchesFence(ctx, session) {
		return Session{}, ErrInvalidState
	}
	switch session.State {
	case StateReceived:
		session.State = StateVerifying
		r.sessions[id] = session
	case StateVerifying, StateVerified, StateCommitting, StateQuarantining, StateAvailable:
	default:
		return Session{}, ErrInvalidState
	}
	return session, nil
}

func (r *MemoryRepository) MarkVerified(ctx context.Context, id string, hash [32]byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	session, ok := r.sessions[id]
	if !ok {
		return ErrNotFound
	}
	if session.State != StateVerifying || !matchesFence(ctx, session) {
		return ErrInvalidState
	}
	session.ServerSHA256 = &hash
	session.State = StateVerified
	r.sessions[id] = session
	return nil
}

func (r *MemoryRepository) MarkCommitting(ctx context.Context, id, storageKey string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	session, ok := r.sessions[id]
	if !ok {
		return ErrNotFound
	}
	if (session.State != StateVerified && session.State != StateCommitting) || !matchesFence(ctx, session) {
		return ErrInvalidState
	}
	session.State = StateCommitting
	session.FinalStorageKey = storageKey
	r.sessions[id] = session
	return nil
}

func (r *MemoryRepository) MarkAvailable(ctx context.Context, id, storageKey string, hash [32]byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	session, ok := r.sessions[id]
	if !ok {
		return ErrNotFound
	}
	if session.State == StateAvailable {
		return nil
	}
	if session.State != StateCommitting || !matchesFence(ctx, session) || subtle.ConstantTimeCompare(session.ClientSHA256[:], hash[:]) != 1 {
		return ErrInvalidState
	}
	session.State = StateAvailable
	session.ServerSHA256 = &hash
	session.FinalStorageKey = storageKey
	session.VerificationClaim = ""
	for assetID, asset := range r.assets {
		if asset.OwnerID == session.OwnerID && subtle.ConstantTimeCompare(asset.ContentSHA256[:], hash[:]) == 1 {
			if asset.StorageKey != storageKey {
				return fmt.Errorf("duplicate digest has inconsistent storage key: %w", ErrInvalidState)
			}
			session.AssetID = assetID
			r.sessions[id] = session
			return nil
		}
	}
	assetID, err := NewID()
	if err != nil {
		return err
	}
	r.assets[assetID] = Asset{
		ID:               assetID,
		OwnerID:          session.OwnerID,
		StorageKey:       storageKey,
		OriginalFilename: session.OriginalFilename,
		MediaType:        session.MediaType,
		ByteSize:         session.ExpectedSize,
		ContentSHA256:    hash,
		CreatedAt:        time.Now().UTC(),
	}
	session.AssetID = assetID
	r.sessions[id] = session
	return nil
}

func (r *MemoryRepository) ListAssets(_ context.Context, ownerID string, before *AssetCursor, limit int) ([]Asset, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	assets := make([]Asset, 0, limit)
	for _, asset := range r.assets {
		if asset.OwnerID != ownerID || (before != nil && !assetBefore(asset, *before)) {
			continue
		}
		assets = append(assets, asset)
	}
	sort.Slice(assets, func(left, right int) bool {
		if assets[left].CreatedAt.Equal(assets[right].CreatedAt) {
			return assets[left].ID > assets[right].ID
		}
		return assets[left].CreatedAt.After(assets[right].CreatedAt)
	})
	if len(assets) > limit {
		assets = assets[:limit]
	}
	return assets, nil
}

func (r *MemoryRepository) AssetByID(_ context.Context, ownerID, assetID string) (Asset, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	asset, ok := r.assets[assetID]
	if !ok || asset.OwnerID != ownerID {
		return Asset{}, ErrNotFound
	}
	return asset, nil
}

func assetBefore(asset Asset, cursor AssetCursor) bool {
	if asset.CreatedAt.Before(cursor.CreatedAt) {
		return true
	}
	return asset.CreatedAt.Equal(cursor.CreatedAt) && asset.ID < cursor.ID
}

func (r *MemoryRepository) MarkQuarantineIntent(ctx context.Context, id string, hash [32]byte, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	session, ok := r.sessions[id]
	if !ok {
		return ErrNotFound
	}
	if session.State != StateVerifying || !matchesFence(ctx, session) {
		return ErrInvalidState
	}
	session.State = StateQuarantining
	session.ServerSHA256 = &hash
	r.sessions[id] = session
	return nil
}

func (r *MemoryRepository) MarkQuarantined(ctx context.Context, id string, hash [32]byte, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	session, ok := r.sessions[id]
	if !ok {
		return ErrNotFound
	}
	if session.State != StateQuarantining || !matchesFence(ctx, session) {
		return ErrInvalidState
	}
	session.State = StateQuarantined
	session.ServerSHA256 = &hash
	session.VerificationClaim = ""
	r.sessions[id] = session
	return nil
}

func (r *MemoryRepository) MarkFailed(ctx context.Context, id, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	session, ok := r.sessions[id]
	if !ok {
		return ErrNotFound
	}
	if session.State == StateAvailable || session.State == StateQuarantining || session.State == StateQuarantined || !matchesFence(ctx, session) {
		return fmt.Errorf("terminal session: %w", ErrInvalidState)
	}
	session.State = StateFailed
	session.VerificationClaim = ""
	r.sessions[id] = session
	return nil
}

func matchesFence(ctx context.Context, session Session) bool {
	claim := VerificationFence(ctx)
	return claim == "" || (session.VerificationClaim != "" && claim == session.VerificationClaim)
}

func (r *MemoryRepository) PendingVerification(_ context.Context, limit int) ([]Session, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]Session, 0, limit)
	for _, session := range r.sessions {
		switch session.State {
		case StateReceived, StateVerifying, StateVerified, StateCommitting, StateQuarantining:
			result = append(result, session)
			if len(result) == limit {
				return result, nil
			}
		}
	}
	return result, nil
}

func (r *MemoryRepository) ExpiredSessions(_ context.Context, now time.Time, limit int) ([]Session, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]Session, 0, limit)
	for _, session := range r.sessions {
		if !session.ExpiresAt.After(now) && (session.State == StateCreated || session.State == StateUploading || session.State == StateFailed) {
			result = append(result, session)
			if len(result) == limit {
				break
			}
		}
	}
	return result, nil
}

func (r *MemoryRepository) MarkExpired(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	session, ok := r.sessions[id]
	if !ok {
		return ErrNotFound
	}
	if session.State != StateCreated && session.State != StateUploading && session.State != StateFailed && session.State != StateExpired {
		return ErrInvalidState
	}
	session.State = StateExpired
	r.sessions[id] = session
	return nil
}

func (r *MemoryRepository) ResetForRetry(_ context.Context, id, ownerID string) (Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	session, ok := r.sessions[id]
	if !ok {
		return Session{}, ErrNotFound
	}
	if session.OwnerID != ownerID || (session.State != StateCreated && session.State != StateUploading) || session.ReceivedSize >= session.ExpectedSize {
		return Session{}, ErrInvalidState
	}
	session.State = StateCreated
	session.ReceivedSize = 0
	session.TransportResource = ""
	r.sessions[id] = session
	return session, nil
}
