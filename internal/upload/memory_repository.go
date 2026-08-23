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
	mu       sync.RWMutex
	sessions map[string]Session
	byClient map[string]string
	assets   map[string]Asset
}

func NewMemoryRepository() *MemoryRepository {
	return &MemoryRepository{
		sessions: make(map[string]Session),
		byClient: make(map[string]string),
		assets:   make(map[string]Asset),
	}
}

func (r *MemoryRepository) CreateSession(_ context.Context, input CreateSessionInput) (Session, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	clientKey := input.OwnerID + "\x00" + input.ClientAssetID
	if id, ok := r.byClient[clientKey]; ok {
		existing := r.sessions[id]
		if existing.OriginalFilename != input.OriginalFilename ||
			existing.MediaType != input.MediaType ||
			existing.ExpectedSize != input.ExpectedSize ||
			subtle.ConstantTimeCompare(existing.ClientSHA256[:], input.ClientSHA256[:]) != 1 {
			return Session{}, false, ErrConflict
		}
		if existing.State == StateExpired {
			existing.State = StateCreated
			existing.ReceivedSize = 0
			existing.ServerSHA256 = nil
			existing.TransportResource = ""
			existing.FinalStorageKey = ""
			existing.AssetID = ""
			existing.ExpiresAt = input.ExpiresAt
			r.sessions[id] = existing
		}
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

func (r *MemoryRepository) BeginVerification(_ context.Context, id string) (Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	session, ok := r.sessions[id]
	if !ok {
		return Session{}, ErrNotFound
	}
	switch session.State {
	case StateReceived:
		session.State = StateVerifying
		r.sessions[id] = session
	case StateVerifying, StateVerified, StateCommitting, StateAvailable:
	default:
		return Session{}, ErrInvalidState
	}
	return session, nil
}

func (r *MemoryRepository) MarkVerified(_ context.Context, id string, hash [32]byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	session, ok := r.sessions[id]
	if !ok {
		return ErrNotFound
	}
	if session.State != StateVerifying {
		return ErrInvalidState
	}
	session.ServerSHA256 = &hash
	session.State = StateVerified
	r.sessions[id] = session
	return nil
}

func (r *MemoryRepository) MarkCommitting(_ context.Context, id, storageKey string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	session, ok := r.sessions[id]
	if !ok {
		return ErrNotFound
	}
	if session.State != StateVerified && session.State != StateCommitting {
		return ErrInvalidState
	}
	session.State = StateCommitting
	session.FinalStorageKey = storageKey
	r.sessions[id] = session
	return nil
}

func (r *MemoryRepository) MarkAvailable(_ context.Context, id, storageKey string, hash [32]byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	session, ok := r.sessions[id]
	if !ok {
		return ErrNotFound
	}
	if session.State == StateAvailable {
		return nil
	}
	if session.State != StateCommitting || subtle.ConstantTimeCompare(session.ClientSHA256[:], hash[:]) != 1 {
		return ErrInvalidState
	}
	session.State = StateAvailable
	session.ServerSHA256 = &hash
	session.FinalStorageKey = storageKey
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

func (r *MemoryRepository) MarkQuarantined(_ context.Context, id string, hash [32]byte, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	session, ok := r.sessions[id]
	if !ok {
		return ErrNotFound
	}
	session.State = StateQuarantined
	session.ServerSHA256 = &hash
	r.sessions[id] = session
	return nil
}

func (r *MemoryRepository) MarkFailed(_ context.Context, id, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	session, ok := r.sessions[id]
	if !ok {
		return ErrNotFound
	}
	if session.State == StateAvailable || session.State == StateQuarantined {
		return fmt.Errorf("terminal session: %w", ErrInvalidState)
	}
	session.State = StateFailed
	r.sessions[id] = session
	return nil
}

func (r *MemoryRepository) PendingVerification(_ context.Context, limit int) ([]Session, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]Session, 0, limit)
	for _, session := range r.sessions {
		switch session.State {
		case StateReceived, StateVerifying, StateVerified, StateCommitting:
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
