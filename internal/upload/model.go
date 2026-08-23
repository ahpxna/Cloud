package upload

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

type State string

const (
	StateCreated     State = "created"
	StateUploading   State = "uploading"
	StateReceived    State = "received"
	StateVerifying   State = "verifying"
	StateVerified    State = "verified"
	StateCommitting  State = "committing"
	StateAvailable   State = "available"
	StateFailed      State = "failed"
	StateExpired     State = "expired"
	StateQuarantined State = "quarantined"
)

var (
	ErrNotFound            = errors.New("upload session not found")
	ErrConflict            = errors.New("upload session conflict")
	ErrInvalidState        = errors.New("invalid upload state transition")
	ErrOwnerMismatch       = errors.New("upload owner mismatch")
	ErrChecksumMismatch    = errors.New("uploaded content checksum mismatch")
	ErrInsufficientStorage = errors.New("insufficient storage capacity")
)

type Session struct {
	ID                string
	OwnerID           string
	ClientAssetID     string
	OriginalFilename  string
	MediaType         string
	ExpectedSize      int64
	ReceivedSize      int64
	ClientSHA256      [32]byte
	ServerSHA256      *[32]byte
	State             State
	TransportResource string
	FinalStorageKey   string
	ExpiresAt         time.Time
	AssetID           string
}

type CreateSessionInput struct {
	OwnerID          string
	ClientAssetID    string
	OriginalFilename string
	MediaType        string
	ExpectedSize     int64
	ClientSHA256     [32]byte
	ExpiresAt        time.Time
	AvailableBytes   int64
	MinimumFreeBytes int64
}

type Repository interface {
	CreateSession(context.Context, CreateSessionInput) (Session, bool, error)
	SessionByID(context.Context, string) (Session, error)
	ClaimTusCreation(context.Context, string, string, int64) error
	RecordProgress(context.Context, string, int64) error
	MarkReceived(context.Context, string, int64) error
	BeginVerification(context.Context, string) (Session, error)
	MarkVerified(context.Context, string, [32]byte) error
	MarkCommitting(context.Context, string, string) error
	MarkAvailable(context.Context, string, string, [32]byte) error
	MarkQuarantined(context.Context, string, [32]byte, string) error
	MarkFailed(context.Context, string, string) error
	PendingVerification(context.Context, int) ([]Session, error)
	ExpiredSessions(context.Context, time.Time, int) ([]Session, error)
	MarkExpired(context.Context, string) error
}

// AssetRepository is deliberately read-only. Asset visibility is granted only
// after Repository.MarkAvailable completes the verified commit state machine.
type AssetRepository interface {
	ListAssets(context.Context, string, *AssetCursor, int) ([]Asset, error)
	AssetByID(context.Context, string, string) (Asset, error)
}

type AssetCursor struct {
	CreatedAt time.Time
	ID        string
}

type Asset struct {
	ID               string
	OwnerID          string
	StorageKey       string
	OriginalFilename string
	MediaType        string
	ByteSize         int64
	ContentSHA256    [32]byte
	CreatedAt        time.Time
}

func NewID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate ID: %w", err)
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(raw[:])
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32], nil
}
