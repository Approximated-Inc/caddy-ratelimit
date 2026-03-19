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

// slidingWindowRateLimiter uses two fixed-window counters with linear
// interpolation to approximate a sliding window. Memory usage is O(1)
// per key regardless of maxEvents (~56 bytes vs O(maxEvents)*24 bytes
// for the ring buffer approach).
//
// The trade-off is ~1-2% less precision compared to exact per-event
// tracking, which is acceptable for virtually all production use cases.
// This is the same algorithm used by Cloudflare and Redis.
type slidingWindowRateLimiter struct {
	mu        sync.Mutex
	window    time.Duration
	maxEvents int
	prevCount int
	currCount int
	currStart time.Time
}

// newSlidingWindowRateLimiter creates a new sliding window rate limiter
// allowing maxEvents in a sliding window of the given duration. It panics
// if maxEvents or window are less than zero.
func newSlidingWindowRateLimiter(maxEvents int, window time.Duration) *slidingWindowRateLimiter {
	if maxEvents < 0 {
		panic("maxEvents cannot be less than zero")
	}
	if window < 0 {
		panic("window cannot be less than zero")
	}
	return &slidingWindowRateLimiter{
		window:    window,
		maxEvents: maxEvents,
		currStart: now(),
	}
}

// When returns the duration before the next allowable event; it does not block.
// If zero, the event is allowed and a reservation is immediately made.
// If non-zero, the event is NOT allowed and a reservation is not made.
func (s *slidingWindowRateLimiter) When() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.maxEvents == 0 {
		return s.window // no events allowed
	}
	if s.window == 0 {
		return 0 // all events allowed
	}

	s.advanceWindow()

	count := s.weightedCount()
	if count < float64(s.maxEvents) {
		s.currCount++
		return 0
	}

	// Estimate when the weighted count will drop below maxEvents.
	// The previous window's contribution decreases linearly over time.
	// We need: prevCount * (1 - elapsed/window) + currCount < maxEvents
	// Solving for elapsed: elapsed > window * (1 - (maxEvents - currCount) / prevCount)
	if s.prevCount > 0 {
		needed := float64(s.maxEvents) - float64(s.currCount)
		if needed <= 0 {
			// Current window alone exceeds limit; must wait for full window rotation
			return s.currStart.Add(s.window).Sub(now())
		}
		fraction := 1.0 - needed/float64(s.prevCount)
		waitUntilElapsed := time.Duration(float64(s.window) * fraction)
		elapsed := now().Sub(s.currStart)
		wait := waitUntilElapsed - elapsed
		if wait > 0 {
			return wait
		}
	}

	// Current window alone is at capacity
	return s.currStart.Add(s.window).Sub(now())
}

// advanceWindow rotates windows if the current one has expired.
// NOT safe for concurrent use; must be called under s.mu.
func (s *slidingWindowRateLimiter) advanceWindow() {
	elapsed := now().Sub(s.currStart)
	if elapsed >= s.window {
		// How many full windows have passed?
		windowsPassed := int(elapsed / s.window)
		if windowsPassed == 1 {
			s.prevCount = s.currCount
		} else {
			// More than one window has passed; previous data is stale
			s.prevCount = 0
		}
		s.currCount = 0
		s.currStart = s.currStart.Add(time.Duration(windowsPassed) * s.window)
	}
}

// weightedCount returns the interpolated event count across the sliding window.
// NOT safe for concurrent use; must be called under s.mu.
func (s *slidingWindowRateLimiter) weightedCount() float64 {
	elapsed := now().Sub(s.currStart)
	if elapsed < 0 {
		elapsed = 0
	}
	prevWeight := 1.0 - float64(elapsed)/float64(s.window)
	if prevWeight < 0 {
		prevWeight = 0
	}
	return float64(s.prevCount)*prevWeight + float64(s.currCount)
}

// MaxEvents returns the maximum number of events allowed in the window.
func (s *slidingWindowRateLimiter) MaxEvents() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maxEvents
}

// SetMaxEvents changes the maximum number of events allowed in the window.
func (s *slidingWindowRateLimiter) SetMaxEvents(n int) {
	if n < 0 {
		panic("maxEvents cannot be less than zero")
	}
	s.mu.Lock()
	s.maxEvents = n
	s.mu.Unlock()
}

// Window returns the size of the sliding window.
func (s *slidingWindowRateLimiter) Window() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.window
}

// SetWindow changes the sliding window duration.
func (s *slidingWindowRateLimiter) SetWindow(d time.Duration) {
	if d < 0 {
		panic("window cannot be less than zero")
	}
	s.mu.Lock()
	s.window = d
	s.mu.Unlock()
}

// Count returns the approximate number of events in the window from the
// reference time, and the estimated oldest event timestamp.
func (s *slidingWindowRateLimiter) Count(ref time.Time) (int, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.countUnsynced(ref)
}

// countUnsynced is the lock-free version of Count.
func (s *slidingWindowRateLimiter) countUnsynced(ref time.Time) (int, time.Time) {
	var zeroTime time.Time
	s.advanceWindow()

	count := int(math.Ceil(s.weightedCount()))
	if count == 0 {
		return 0, zeroTime
	}

	// Estimate oldest event: the beginning of the weighted portion
	// of the previous window that contributes to the count.
	elapsed := ref.Sub(s.currStart)
	if elapsed < 0 {
		elapsed = 0
	}
	prevWeight := 1.0 - float64(elapsed)/float64(s.window)
	if prevWeight < 0 {
		prevWeight = 0
	}

	var oldestEvent time.Time
	if s.prevCount > 0 && prevWeight > 0 {
		// Oldest event is estimated at the start of the contributing
		// portion of the previous window
		oldestEvent = s.currStart.Add(-time.Duration(prevWeight * float64(s.window)))
	} else {
		oldestEvent = s.currStart
	}

	return count, oldestEvent
}

// reserveUnsynced makes a reservation without acquiring the lock.
func (s *slidingWindowRateLimiter) reserveUnsynced() {
	s.advanceWindow()
	s.currCount++
}

// getLock returns the mutex for external locking.
func (s *slidingWindowRateLimiter) getLock() *sync.Mutex {
	return &s.mu
}

// Interface guard
var _ rateLimiter = (*slidingWindowRateLimiter)(nil)
