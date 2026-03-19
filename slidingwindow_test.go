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
	"testing"
	"time"
)

func TestSlidingWindowBasicRateLimiting(t *testing.T) {
	initTime()

	maxEvents := 5
	window := 10 * time.Second
	sw := newSlidingWindowRateLimiter(maxEvents, window)

	// Should allow maxEvents events
	for i := 0; i < maxEvents; i++ {
		if when := sw.When(); when != 0 {
			t.Fatalf("event %d should be allowed, got wait %v", i, when)
		}
	}

	// Next event should be rejected
	if when := sw.When(); when == 0 {
		t.Fatal("should not allow event beyond max_events")
	}
}

func TestSlidingWindowCountAfterExpiry(t *testing.T) {
	initTime()

	maxEvents := 5
	window := 10 * time.Second
	sw := newSlidingWindowRateLimiter(maxEvents, window)

	// Fill up
	for i := 0; i < maxEvents; i++ {
		sw.When()
	}

	count, _ := sw.Count(now())
	if count != maxEvents {
		t.Fatalf("expected count %d, got %d", maxEvents, count)
	}

	// Advance past the window — all events should expire
	advanceTime(21) // 21 seconds > 2 * window (10s)

	count, _ = sw.Count(now())
	if count != 0 {
		t.Fatalf("expected count 0 after full expiry, got %d", count)
	}

	// Should allow events again
	if when := sw.When(); when != 0 {
		t.Fatal("should allow events after window expires")
	}
}

func TestSlidingWindowWindowRotation(t *testing.T) {
	initTime()

	maxEvents := 10
	window := 10 * time.Second
	sw := newSlidingWindowRateLimiter(maxEvents, window)

	// Add 5 events in current window
	for i := 0; i < 5; i++ {
		sw.When()
	}

	// Advance to next window
	advanceTime(10)

	// The 5 events from previous window should be partially weighted.
	// At the start of a new window, prevWeight = 1.0, so all 5 count.
	// We should be able to add 5 more.
	for i := 0; i < 5; i++ {
		if when := sw.When(); when != 0 {
			t.Fatalf("event %d should be allowed with partial prev window, got wait %v", i, when)
		}
	}

	// Now we should be at the limit (5 prev weighted + 5 current = 10)
	if when := sw.When(); when == 0 {
		t.Fatal("should reject events at the limit")
	}
}

func TestSlidingWindowZeroMaxEvents(t *testing.T) {
	initTime()

	sw := newSlidingWindowRateLimiter(0, 10*time.Second)
	if when := sw.When(); when == 0 {
		t.Fatal("zero max_events should reject all events")
	}
}

func TestSlidingWindowZeroWindow(t *testing.T) {
	initTime()

	sw := newSlidingWindowRateLimiter(10, 0)
	// Zero window means all events allowed
	for i := 0; i < 100; i++ {
		if when := sw.When(); when != 0 {
			t.Fatalf("zero window should allow all events, got wait %v on event %d", when, i)
		}
	}
}

func TestSlidingWindowSetMaxEvents(t *testing.T) {
	initTime()

	sw := newSlidingWindowRateLimiter(5, 10*time.Second)

	// Fill up
	for i := 0; i < 5; i++ {
		sw.When()
	}

	// Should be full
	if when := sw.When(); when == 0 {
		t.Fatal("should be full at 5 events")
	}

	// Increase limit
	sw.SetMaxEvents(10)

	// Should allow more events
	if when := sw.When(); when != 0 {
		t.Fatal("should allow events after increasing max_events")
	}
}

func TestSlidingWindowSetWindow(t *testing.T) {
	initTime()

	sw := newSlidingWindowRateLimiter(5, 10*time.Second)

	for i := 0; i < 5; i++ {
		sw.When()
	}

	// Should be full
	if when := sw.When(); when == 0 {
		t.Fatal("should be full")
	}

	// Change window to 0 (allow all)
	sw.SetWindow(0)
	if when := sw.When(); when != 0 {
		t.Fatal("zero window should allow all events")
	}
}

func TestSlidingWindowReserveUnsynced(t *testing.T) {
	initTime()

	sw := newSlidingWindowRateLimiter(10, 10*time.Second)

	mu := sw.getLock()
	mu.Lock()
	sw.reserveUnsynced()
	count, _ := sw.countUnsynced(now())
	mu.Unlock()

	if count != 1 {
		t.Fatalf("expected count 1 after reserveUnsynced, got %d", count)
	}
}
