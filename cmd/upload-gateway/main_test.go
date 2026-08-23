package main

import (
	"testing"
	"time"
)

func TestEnvDurationStrict(t *testing.T) {
	const fallback = 2 * time.Minute
	t.Setenv("PHOTO_TEST_DURATION", "45s")
	got, err := envDurationStrict("PHOTO_TEST_DURATION", fallback)
	if err != nil || got != 45*time.Second {
		t.Fatalf("envDurationStrict valid value = %s, err=%v", got, err)
	}

	t.Setenv("PHOTO_TEST_DURATION", "not-a-duration")
	if _, err := envDurationStrict("PHOTO_TEST_DURATION", fallback); err == nil {
		t.Fatal("invalid duration silently used fallback")
	}

	t.Setenv("PHOTO_TEST_DURATION", "0s")
	if _, err := envDurationStrict("PHOTO_TEST_DURATION", fallback); err == nil {
		t.Fatal("zero duration silently used fallback")
	}
}
