package auth

import (
	"context"
	"time"
)

type LibraryLockState struct {
	Enabled       bool       `json:"enabled"`
	Version       int64      `json:"version"`
	UnlockedUntil *time.Time `json:"unlocked_until,omitempty"`
}

func (s LibraryLockState) Locked() bool {
	return s.Enabled && (s.UnlockedUntil == nil || !s.UnlockedUntil.After(time.Now()))
}

type LibraryLockReader interface {
	LibraryLockState(context.Context, string, string) (LibraryLockState, error)
}
