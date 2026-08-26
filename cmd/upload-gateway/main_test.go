package main

import (
	"strconv"
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

func TestEnvIntRejectsArchitectureOverflow(t *testing.T) {
	if strconv.IntSize != 32 {
		t.Skip("int64 to int overflow is only representable on 32-bit targets")
	}
	t.Setenv("PHOTO_TEST_INT", "2147483648")
	if _, err := envInt("PHOTO_TEST_INT", 1); err == nil {
		t.Fatal("envInt accepted a value larger than math.MaxInt")
	}
}
