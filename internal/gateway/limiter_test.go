package gateway

import "testing"

func TestPatchLimiterEnforcesGlobalAndPerUserLimits(t *testing.T) {
	t.Parallel()
	limiter := newPatchLimiter(3, 2)
	if !limiter.Acquire("user-a") || !limiter.Acquire("user-a") {
		t.Fatal("user-a should receive two slots")
	}
	if limiter.Acquire("user-a") {
		t.Fatal("user-a exceeded per-user limit")
	}
	if !limiter.Acquire("user-b") {
		t.Fatal("user-b should receive remaining global slot")
	}
	if limiter.Acquire("user-c") {
		t.Fatal("global limit was exceeded")
	}
	limiter.Release("user-a")
	if !limiter.Acquire("user-c") {
		t.Fatal("released slot was not reusable")
	}
	limiter.Release("user-a")
	limiter.Release("user-b")
	limiter.Release("user-c")
}
