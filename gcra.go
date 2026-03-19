// Copyright 2021 Matthew Holt

// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at

// 	http://www.apache.org/licenses/LICENSE-2.0

// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package caddyrl

import (
	"math"
	"sync"
	"time"
)

// gcraRateLimiter implements the Generic Cell Rate Algorithm (GCRA),
// also known as "leaky bucket as a meter". It uses only a single
// timestamp per key (~32 bytes), making it extremely memory efficient.
//
// GCRA provides exact rate limiting (no approximation) and is used
// at scale by Shopify and GitLab.
//
// The algorithm works by tracking a "theoretical arrival time" (TAT)
// which represents when the next cell/event is theoretically expected.
// If a request arrives too early (before TAT - window), it is rejected.
type gcraRateLimiter struct {
	mu        sync.Mutex
	window    time.Duration
	maxEvents int
	tat       time.Time // theoretical arrival time
}

// newGCRARateLimiter creates a new GCRA rate limiter allowing maxEvents
// in a sliding window of the given duration. It panics if maxEvents or
// window are less than zero.
func newGCRARateLimiter(maxEvents int, window time.Duration) *gcraRateLimiter {
	if maxEvents < 0 {
		panic("maxEvents cannot be less than zero")
	}
	if window < 0 {
		panic("window cannot be less than zero")
	}
	return &gcraRateLimiter{
		window:    window,
		maxEvents: maxEvents,
	}
}

// emissionInterval returns the time between allowed events.
func (g *gcraRateLimiter) emissionInterval() time.Duration {
	if g.maxEvents == 0 {
		return g.window
	}
	return time.Duration(float64(g.window) / float64(g.maxEvents))
}

// When returns the duration before the next allowable event; it does not block.
// If zero, the event is allowed and a reservation is immediately made.
// If non-zero, the event is NOT allowed and a reservation is not made.
func (g *gcraRateLimiter) When() time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.maxEvents == 0 {
		return g.window // no events allowed
	}
	if g.window == 0 {
		return 0 // all events allowed
	}

	t := now()
	interval := g.emissionInterval()

	// If TAT is zero (first request), or TAT is in the past, set it to now
	newTAT := g.tat
	if newTAT.Before(t) {
		newTAT = t
	}

	// The burst offset is (maxEvents - 1) * interval, which allows exactly
	// maxEvents events in a burst. This differs from using the full window
	// (maxEvents * interval) which would allow maxEvents + 1.
	burstOffset := time.Duration(g.maxEvents-1) * interval
	allowAt := newTAT.Add(-burstOffset)

	if t.Before(allowAt) {
		// Too early — request would exceed the rate limit
		return allowAt.Sub(t)
	}

	// Allow the event and advance TAT by one emission interval
	g.tat = newTAT.Add(interval)
	return 0
}

// MaxEvents returns the maximum number of events allowed in the window.
func (g *gcraRateLimiter) MaxEvents() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.maxEvents
}

// SetMaxEvents changes the maximum number of events allowed in the window.
func (g *gcraRateLimiter) SetMaxEvents(n int) {
	if n < 0 {
		panic("maxEvents cannot be less than zero")
	}
	g.mu.Lock()
	g.maxEvents = n
	g.mu.Unlock()
}

// Window returns the size of the sliding window.
func (g *gcraRateLimiter) Window() time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.window
}

// SetWindow changes the sliding window duration.
func (g *gcraRateLimiter) SetWindow(d time.Duration) {
	if d < 0 {
		panic("window cannot be less than zero")
	}
	g.mu.Lock()
	g.window = d
	g.mu.Unlock()
}

// Count returns the estimated number of events in the window from the
// reference time, and the estimated oldest event timestamp.
func (g *gcraRateLimiter) Count(ref time.Time) (int, time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.countUnsynced(ref)
}

// countUnsynced is the lock-free version of Count.
func (g *gcraRateLimiter) countUnsynced(ref time.Time) (int, time.Time) {
	var zeroTime time.Time

	if g.maxEvents == 0 || g.window == 0 {
		return 0, zeroTime
	}

	// If TAT is zero or entirely in the past beyond the window, no events
	if g.tat.IsZero() || g.tat.Before(ref.Add(-g.window)) {
		return 0, zeroTime
	}

	interval := g.emissionInterval()

	// The TAT represents the cumulative "debt" from past events.
	// Count is how many intervals fit between now and TAT.
	// If TAT is in the future, that means events have been reserved.
	var count int
	if g.tat.After(ref) {
		count = int(math.Ceil(float64(g.tat.Sub(ref)) / float64(interval)))
	} else {
		count = 0
	}

	if count > g.maxEvents {
		count = g.maxEvents
	}
	if count == 0 {
		return 0, zeroTime
	}

	// Estimate oldest event: TAT minus count intervals
	oldestEvent := g.tat.Add(-time.Duration(count) * interval)
	if oldestEvent.Before(ref.Add(-g.window)) {
		oldestEvent = ref.Add(-g.window)
	}

	return count, oldestEvent
}

// reserveUnsynced makes a reservation without acquiring the lock.
func (g *gcraRateLimiter) reserveUnsynced() {
	t := now()
	interval := g.emissionInterval()
	if g.tat.Before(t) {
		g.tat = t
	}
	g.tat = g.tat.Add(interval)
}

// getLock returns the mutex for external locking.
func (g *gcraRateLimiter) getLock() *sync.Mutex {
	return &g.mu
}

// Interface guard
var _ rateLimiter = (*gcraRateLimiter)(nil)
