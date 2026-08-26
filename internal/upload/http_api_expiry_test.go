package upload

import (
	"testing"
	"time"
)

func TestUploadTransferExpiredOnlyExpiresTransferStates(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 26, 1, 0, 0, 0, time.UTC)
	expiredAt := now.Add(-time.Minute)
	future := now.Add(time.Hour)

	for _, state := range []State{StateCreated, StateUploading} {
		session := Session{State: state, ExpiresAt: expiredAt}
		if !uploadTransferExpired(session, now) {
			t.Fatalf("state %s with expired transfer deadline was accepted", state)
		}
		session.ExpiresAt = future
		if uploadTransferExpired(session, now) {
			t.Fatalf("state %s with live transfer deadline was expired", state)
		}
	}

	for _, state := range []State{
		StateReceived,
		StateVerifying,
		StateVerified,
		StateCommitting,
		StateAvailable,
		StateQuarantining,
		StateQuarantined,
	} {
		if uploadTransferExpired(Session{State: state, ExpiresAt: expiredAt}, now) {
			t.Fatalf("durable state %s was incorrectly expired by transfer deadline", state)
		}
	}

	if !uploadTransferExpired(Session{State: StateExpired, ExpiresAt: future}, now) {
		t.Fatal("explicit expired state must always be gone")
	}
}

func TestUploadCapabilityOnlyIssuedForTransferStates(t *testing.T) {
	t.Parallel()
	for _, state := range []State{StateCreated, StateUploading} {
		if !uploadStateAcceptsCapability(state) {
			t.Fatalf("state %s should accept upload capability", state)
		}
	}
	for _, state := range []State{
		StateReceived,
		StateVerifying,
		StateVerified,
		StateCommitting,
		StateAvailable,
		StateFailed,
		StateExpired,
		StateQuarantining,
		StateQuarantined,
	} {
		if uploadStateAcceptsCapability(state) {
			t.Fatalf("state %s must not receive upload capability", state)
		}
	}
}
