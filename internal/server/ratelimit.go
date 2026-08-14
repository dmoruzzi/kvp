package server

import (
	"math"
	"sync"
	"time"
)

// rateLimiter is an in-memory token bucket keyed by client IP (spec §7). Each
// bucket refills at rps tokens/second up to a burst cap. Old buckets are pruned
// to keep memory bounded.
type rateLimiter struct {
	mu      sync.Mutex
	rps     float64
	burst   float64
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newRateLimiter(rps float64, burst int) *rateLimiter {
	return &rateLimiter{
		rps:     rps,
		burst:   float64(burst),
		buckets: make(map[string]*bucket),
	}
}

// allow consumes one token if available. On denial it returns the number of
// seconds until a token is expected, for the Retry-After header.
func (rl *rateLimiter) allow(key string, now time.Time) (bool, int) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	b, ok := rl.buckets[key]
	if !ok {
		b = &bucket{tokens: rl.burst, last: now}
		rl.buckets[key] = b
	}
	elapsed := now.Sub(b.last).Seconds()
	b.tokens = math.Min(rl.burst, b.tokens+elapsed*rl.rps)
	b.last = now

	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	wait := (1 - b.tokens) / rl.rps
	return false, int(math.Ceil(wait))
}

// prune removes buckets idle for more than an hour; called opportunistically to
// bound memory when the map grows large.
func (rl *rateLimiter) prune(now time.Time) {
	cutoff := now.Add(-time.Hour)
	for k, b := range rl.buckets {
		if b.last.Before(cutoff) {
			delete(rl.buckets, k)
		}
	}
}
