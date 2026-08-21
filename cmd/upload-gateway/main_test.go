package main

import (
	"testing"
	"time"
)

func TestEnvDuration(t *testing.T) {
	const fallback = 2 * time.Minute
	t.Setenv("PHOTO_TEST_DURATION", "45s")
	if got := envDuration("PHOTO_TEST_DURATION", fallback); got != 45*time.Second {
		t.Fatalf("envDuration valid value = %s, want 45s", got)
	}

	t.Setenv("PHOTO_TEST_DURATION", "not-a-duration")
	if got := envDuration("PHOTO_TEST_DURATION", fallback); got != fallback {
		t.Fatalf("envDuration invalid value = %s, want fallback %s", got, fallback)
	}

	t.Setenv("PHOTO_TEST_DURATION", "0s")
	if got := envDuration("PHOTO_TEST_DURATION", fallback); got != fallback {
		t.Fatalf("envDuration zero value = %s, want fallback %s", got, fallback)
	}
}
