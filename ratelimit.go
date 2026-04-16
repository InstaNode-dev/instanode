package main

import (
	"net/http"
	"sync"
	"time"
)

// ipRateLimiter enforces a per-IP requests-per-second limit using a simple
// token bucket. Protects against HTTP-level flooding (distinct from the
// per-fingerprint daily provision limit).
type ipRateLimiter struct {
	mu       sync.Mutex
	visitors map[string]*bucket
	rate     float64
	burst    int
}

type bucket struct {
	tokens   float64
	lastSeen time.Time
}

func newIPRateLimiter(requestsPerSecond float64, burst int) *ipRateLimiter {
	rl := &ipRateLimiter{
		visitors: make(map[string]*bucket),
		rate:     requestsPerSecond,
		burst:    burst,
	}
	go rl.cleanup()
	return rl
}

func (rl *ipRateLimiter) allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	b, exists := rl.visitors[ip]
	now := time.Now()

	if !exists {
		rl.visitors[ip] = &bucket{tokens: float64(rl.burst) - 1, lastSeen: now}
		return true
	}

	elapsed := now.Sub(b.lastSeen).Seconds()
	b.tokens += elapsed * rl.rate
	if b.tokens > float64(rl.burst) {
		b.tokens = float64(rl.burst)
	}
	b.lastSeen = now

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func (rl *ipRateLimiter) cleanup() {
	for {
		time.Sleep(5 * time.Minute)
		rl.mu.Lock()
		cutoff := time.Now().Add(-10 * time.Minute)
		for ip, b := range rl.visitors {
			if b.lastSeen.Before(cutoff) {
				delete(rl.visitors, ip)
			}
		}
		rl.mu.Unlock()
	}
}

func rateLimitMiddleware(rl *ipRateLimiter, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		if !rl.allow(ip) {
			writeJSON(w, http.StatusTooManyRequests, map[string]any{
				"ok": false, "error": "too_many_requests", "message": "Rate limit exceeded. Try again shortly.",
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}
