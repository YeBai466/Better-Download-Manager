package downloader

import (
	"context"
	"sync"
	"time"
)

const (
	// defaultRetries is per chunk and only counts attempts that made no
	// forward progress (see downloadChunkWithRetry), so it can stay small
	// without giving up on flaky-but-alive connections.
	defaultRetries = 4
	// maxChunkAttempts bounds a single chunk's total attempts. Forward
	// progress resets the retry budget, which is what lets a flaky-but-alive
	// connection finish; this cap is the backstop against a server that
	// dribbles a few bytes per request forever.
	maxChunkAttempts    = 200
	defaultStallTimeout = 30 * time.Second
	defaultMetaInterval = 2 * time.Second
	smallChunkSize      = int64(2 << 20)   // 2 MiB
	minTailChunk        = int64(1 << 20)   // 1 MiB, floor for end-of-file chunks
	defaultChunkSize    = int64(8 << 20)   // 8 MiB
	largeFileChunkSize  = int64(16 << 20)  // 16 MiB
	smallFileThreshold  = int64(8 << 20)   // 8 MiB
	mediumFileThreshold = int64(64 << 20)  // 64 MiB
	largeFileThreshold  = int64(512 << 20) // 512 MiB
)

type RuntimeConfig struct {
	MaxConcurrent  int
	MaxConnections int
	SpeedLimit     int64
	Retries        int
	StallTimeout   time.Duration
	MetaInterval   time.Duration
}

func normalizeRuntimeConfig(c RuntimeConfig) RuntimeConfig {
	if c.MaxConcurrent < 1 {
		c.MaxConcurrent = 5
	}
	if c.Retries < 0 {
		c.Retries = defaultRetries
	}
	if c.Retries == 0 {
		c.Retries = defaultRetries
	}
	if c.StallTimeout <= 0 {
		c.StallTimeout = defaultStallTimeout
	}
	if c.MetaInterval <= 0 {
		c.MetaInterval = defaultMetaInterval
	}
	if c.MaxConnections < 1 {
		c.MaxConnections = c.MaxConcurrent * DefaultConnections
	}
	return c
}

// smartConnections scales the worker count with file size. Tiny files finish
// before extra connections would ramp up; anything bigger gets enough
// connections to fill a high-bandwidth-delay-product link (IDM uses 8 across
// the board — we only hold back where setup cost would dominate).
func smartConnections(total int64, requested int) int {
	if requested < 1 {
		requested = DefaultConnections
	}
	if total <= 0 {
		return 1
	}
	switch {
	case total < smallFileThreshold:
		return 1
	case total < mediumFileThreshold:
		return minInt(requested, 4)
	case total < largeFileThreshold:
		return minInt(requested, 8)
	default:
		return requested
	}
}

func smartChunkSize(total int64) int64 {
	switch {
	case total <= 0:
		return defaultChunkSize
	case total < mediumFileThreshold:
		return smallChunkSize
	case total < largeFileThreshold:
		return defaultChunkSize
	default:
		return largeFileChunkSize
	}
}

type speedLimiter struct {
	mu        sync.Mutex
	rate      int64
	updated   time.Time
	allowance float64
}

func newSpeedLimiter(rate int64) *speedLimiter {
	return &speedLimiter{rate: rate, updated: time.Now()}
}

func (l *speedLimiter) SetRate(rate int64) {
	l.mu.Lock()
	l.rate = rate
	l.updated = time.Now()
	if rate <= 0 {
		l.allowance = 0
	} else if l.allowance > float64(rate) {
		l.allowance = float64(rate)
	}
	l.mu.Unlock()
}

func (l *speedLimiter) Wait(ctx context.Context, n int) error {
	if l == nil || n <= 0 {
		return ctx.Err()
	}
	// Consume the debt in pieces: the allowance is capped at one second of
	// rate, so a single read larger than that cap could otherwise never be
	// satisfied and this would spin forever (e.g. a 128 KiB read against a
	// 64 KiB/s limit set mid-transfer).
	remaining := float64(n)
	for {
		l.mu.Lock()
		rate := l.rate
		if rate <= 0 {
			l.mu.Unlock()
			return ctx.Err()
		}
		now := time.Now()
		elapsed := now.Sub(l.updated).Seconds()
		l.updated = now
		l.allowance += elapsed * float64(rate)
		if cap := float64(rate); l.allowance > cap {
			l.allowance = cap
		}
		take := remaining
		if take > l.allowance {
			take = l.allowance
		}
		l.allowance -= take
		remaining -= take
		if remaining <= 0 {
			l.mu.Unlock()
			return ctx.Err()
		}
		need := remaining
		if cap := float64(rate); need > cap {
			need = cap
		}
		wait := time.Duration(need / float64(rate) * float64(time.Second))
		if wait < 10*time.Millisecond {
			wait = 10 * time.Millisecond
		}
		l.mu.Unlock()

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

type connectionLimiter struct {
	ch chan struct{}
}

func newConnectionLimiter(n int) *connectionLimiter {
	if n < 1 {
		n = DefaultConnections
	}
	return &connectionLimiter{ch: make(chan struct{}, n)}
}

// Acquire takes a connection slot. On error no slot is held: winning the send
// race against an already-cancelled context must not leak the slot, since the
// caller skips its deferred Release when Acquire fails.
func (l *connectionLimiter) Acquire(ctx context.Context) error {
	if l == nil {
		return ctx.Err()
	}
	select {
	case l.ch <- struct{}{}:
		if err := ctx.Err(); err != nil {
			l.Release()
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l *connectionLimiter) Release() {
	if l == nil {
		return
	}
	select {
	case <-l.ch:
	default:
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
