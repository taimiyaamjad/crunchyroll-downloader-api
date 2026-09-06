package main

import (
	"testing"
	"time"
)

func TestDownloadBackoffFirstWaitIsNoop(t *testing.T) {
	var slept time.Duration
	b := newDownloadBackoff(5 * time.Second)
	b.sleep = func(d time.Duration) { slept = d }

	b.wait()
	if slept != 0 {
		t.Fatalf("wait() before any download slept %v, want 0", slept)
	}
}

func TestDownloadBackoffWaitsRemainingDelay(t *testing.T) {
	now := time.Unix(0, 0)
	var slept time.Duration
	b := newDownloadBackoff(5 * time.Second)
	b.now = func() time.Time { return now }
	b.sleep = func(d time.Duration) { slept = d; now = now.Add(d) }

	b.done()
	now = now.Add(2 * time.Second) // only 2s of the 5s delay has elapsed
	b.wait()
	if slept != 3*time.Second {
		t.Fatalf("wait() slept %v, want 3s", slept)
	}
}

func TestDownloadBackoffSkipsWaitWhenDelayElapsed(t *testing.T) {
	now := time.Unix(0, 0)
	var slept time.Duration
	b := newDownloadBackoff(5 * time.Second)
	b.now = func() time.Time { return now }
	b.sleep = func(d time.Duration) { slept = d }

	b.done()
	now = now.Add(10 * time.Second) // more than the delay has already passed
	b.wait()
	if slept != 0 {
		t.Fatalf("wait() slept %v, want 0 (delay already elapsed)", slept)
	}
}

func TestDownloadBackoffDisabledIsNoop(t *testing.T) {
	var slept time.Duration
	b := newDownloadBackoff(0)
	b.sleep = func(d time.Duration) { slept = d }

	b.done()
	b.wait()
	if slept != 0 {
		t.Fatalf("wait() slept %v with delay disabled, want 0", slept)
	}
}

func TestDownloadBackoffNilIsNoop(t *testing.T) {
	var b *downloadBackoff
	b.done()
	b.wait() // must not panic
}

func TestRateLimitWaitGrowsAndCaps(t *testing.T) {
	b := newDownloadBackoff(0)

	want := []time.Duration{
		defaultRateLimitWait,
		2 * defaultRateLimitWait,
		4 * defaultRateLimitWait,
	}
	for i, w := range want {
		if got := b.rateLimitWait(); got != w {
			t.Fatalf("hit %d: rateLimitWait() = %v, want %v", i, got, w)
		}
	}

	// Keep hitting it until it should be capped.
	var last time.Duration
	for i := 0; i < 20; i++ {
		last = b.rateLimitWait()
	}
	if last != maxRateLimitWait {
		t.Fatalf("rateLimitWait() after many hits = %v, want cap %v", last, maxRateLimitWait)
	}
}

func TestRateLimitWaitUsesConfiguredDelayAsFloor(t *testing.T) {
	b := newDownloadBackoff(5 * time.Minute)
	if got := b.rateLimitWait(); got != 5*time.Minute {
		t.Fatalf("rateLimitWait() = %v, want configured delay 5m as the floor", got)
	}
}

func TestRateLimitWaitResets(t *testing.T) {
	b := newDownloadBackoff(0)
	b.rateLimitWait()
	b.rateLimitWait()
	b.resetRateLimit()

	if got := b.rateLimitWait(); got != defaultRateLimitWait {
		t.Fatalf("rateLimitWait() after reset = %v, want %v", got, defaultRateLimitWait)
	}
}

func TestRateLimitWaitNilIsSafe(t *testing.T) {
	var b *downloadBackoff
	if got := b.rateLimitWait(); got != defaultRateLimitWait {
		t.Fatalf("nil backoff rateLimitWait() = %v, want %v", got, defaultRateLimitWait)
	}
	b.resetRateLimit() // must not panic
}
