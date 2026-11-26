package auth

import (
	"sync"
	"time"
)

// RateLimiter implements a simple sliding-window rate limiter per key.
type RateLimiter struct {
	mu      sync.Mutex
	buckets map[string][]time.Time
}

func NewRateLimiter() *RateLimiter {
	return &RateLimiter{
		buckets: make(map[string][]time.Time),
	}
}

// Allow records an attempt for key if fewer than maxEvents occurred within window.
func (rl *RateLimiter) Allow(key string, maxEvents int, window time.Duration, now time.Time) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	cutoff := now.Add(-window)
	prev := rl.buckets[key]
	kept := prev[:0]
	for _, t := range prev {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= maxEvents {
		rl.buckets[key] = kept
		return false
	}
	kept = append(kept, now)
	rl.buckets[key] = kept
	return true
}
