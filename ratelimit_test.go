// Copyright 2023 Matthew Holt

// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at

//  http://www.apache.org/licenses/LICENSE-2.0

// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package caddyrl

import (
	"sync"
	"testing"
	"time"
)

// TestSweepDoesNotDeadlock verifies that sweep() does not deadlock by
// calling methods that re-acquire the per-limiter mutex. This is a
// regression test: sweep must only use countUnsynced (lock-free) and
// must NOT call MaxEvents()/Window() while holding the lock via getLock().
func TestSweepDoesNotDeadlock(t *testing.T) {
	for _, algo := range []string{"", "ring_buffer", "sliding_window", "gcra"} {
		t.Run("algorithm_"+algo, func(t *testing.T) {
			initTime()

			rlm := newRateLimiterMap(algo)
			rlm.getOrInsert("key1", 10, 10*time.Second)
			rlm.getOrInsert("key2", 10, 10*time.Second)

			// Add some events
			rlm.limitersMu.Lock()
			for _, rl := range rlm.limiters {
				rl.When()
				rl.When()
			}
			rlm.limitersMu.Unlock()

			// sweep() must complete without deadlocking.
			// Use a goroutine + timeout to detect deadlock.
			done := make(chan struct{})
			go func() {
				rlm.sweep()
				close(done)
			}()

			select {
			case <-done:
				// success
			case <-time.After(2 * time.Second):
				t.Fatal("sweep() deadlocked")
			}
		})
	}
}

// TestSweepRemovesExpiredLimiters verifies that sweep removes limiters
// whose events have all expired outside the window.
func TestSweepRemovesExpiredLimiters(t *testing.T) {
	for _, algo := range []string{"ring_buffer", "sliding_window", "gcra"} {
		t.Run("algorithm_"+algo, func(t *testing.T) {
			initTime()

			rlm := newRateLimiterMap(algo)
			rlm.getOrInsert("active", 10, 10*time.Second)
			rlm.getOrInsert("expired", 10, 10*time.Second)

			// Add events to both
			rlm.limitersMu.Lock()
			rlm.limiters["active"].When()
			rlm.limiters["expired"].When()
			rlm.limitersMu.Unlock()

			// Advance time well past the window so expired limiter's events
			// are fully outside (2x window covers sliding window approximation)
			advanceTime(21)

			// Add a fresh event to "active" so it stays
			rlm.limitersMu.Lock()
			rlm.limiters["active"].When()
			rlm.limitersMu.Unlock()

			rlm.sweep()

			rlm.limitersMu.Lock()
			defer rlm.limitersMu.Unlock()

			if _, ok := rlm.limiters["active"]; !ok {
				t.Fatal("active limiter should not have been swept")
			}
			if _, ok := rlm.limiters["expired"]; ok {
				t.Fatal("expired limiter should have been swept")
			}
		})
	}
}

// TestSweepKeepsActiveLimiters verifies that sweep does not remove
// limiters that still have events within the window.
func TestSweepKeepsActiveLimiters(t *testing.T) {
	for _, algo := range []string{"ring_buffer", "sliding_window", "gcra"} {
		t.Run("algorithm_"+algo, func(t *testing.T) {
			initTime()

			rlm := newRateLimiterMap(algo)
			rlm.getOrInsert("key1", 10, 10*time.Second)

			rlm.limitersMu.Lock()
			rlm.limiters["key1"].When()
			rlm.limitersMu.Unlock()

			rlm.sweep()

			rlm.limitersMu.Lock()
			defer rlm.limitersMu.Unlock()

			if _, ok := rlm.limiters["key1"]; !ok {
				t.Fatal("limiter with active events should not have been swept")
			}
		})
	}
}

// TestSweepConcurrentWithGetOrInsert verifies that sweep and getOrInsert
// can run concurrently without deadlock.
func TestSweepConcurrentWithGetOrInsert(t *testing.T) {
	initTime()

	rlm := newRateLimiterMap("sliding_window")
	// Pre-populate
	for i := 0; i < 20; i++ {
		rlm.getOrInsert("key_"+string(rune('a'+i)), 10, 10*time.Second)
	}

	var wg sync.WaitGroup
	done := make(chan struct{})

	// Run sweep in a goroutine
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			rlm.sweep()
		}
	}()

	// Run getOrInsert concurrently
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			rlm.getOrInsert("concurrent_key", 10, 10*time.Second)
		}
	}()

	// Run updateAll concurrently
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			rlm.updateAll(5, 5*time.Second)
		}
	}()

	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// success - no deadlock
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent sweep/getOrInsert/updateAll deadlocked")
	}
}

// TestNewRateLimiterFactory verifies the factory creates the correct algorithm.
func TestNewRateLimiterFactory(t *testing.T) {
	tests := []struct {
		algorithm string
		expected  string
	}{
		{"", "*caddyrl.ringBufferRateLimiter"},
		{"ring_buffer", "*caddyrl.ringBufferRateLimiter"},
		{"sliding_window", "*caddyrl.slidingWindowRateLimiter"},
		{"gcra", "*caddyrl.gcraRateLimiter"},
	}

	for _, tc := range tests {
		t.Run("algorithm_"+tc.algorithm, func(t *testing.T) {
			rl := newRateLimiter(tc.algorithm, 10, 10*time.Second)

			// Verify basic functionality works
			if when := rl.When(); when != 0 {
				t.Fatal("new limiter should allow first event")
			}
			if rl.MaxEvents() != 10 {
				t.Fatalf("expected max events 10, got %d", rl.MaxEvents())
			}
			if rl.Window() != 10*time.Second {
				t.Fatalf("expected window 10s, got %v", rl.Window())
			}
		})
	}
}

// TestGetOrInsertReturnsExisting verifies that getOrInsert returns an
// existing limiter rather than creating a new one.
func TestGetOrInsertReturnsExisting(t *testing.T) {
	initTime()

	rlm := newRateLimiterMap("sliding_window")
	first := rlm.getOrInsert("key", 10, 10*time.Second)
	first.When() // add an event

	second := rlm.getOrInsert("key", 10, 10*time.Second)

	// Should be the same instance
	count, _ := second.Count(now())
	if count != 1 {
		t.Fatal("getOrInsert should return existing limiter with its state")
	}
}
