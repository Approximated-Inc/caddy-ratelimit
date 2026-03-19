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

func TestGCRABasicRateLimiting(t *testing.T) {
	initTime()

	maxEvents := 5
	window := 10 * time.Second
	g := newGCRARateLimiter(maxEvents, window)

	// Should allow maxEvents events (burst capacity)
	for i := 0; i < maxEvents; i++ {
		if when := g.When(); when != 0 {
			t.Fatalf("event %d should be allowed, got wait %v", i, when)
		}
	}

	// Next event should be rejected
	if when := g.When(); when == 0 {
		t.Fatal("should not allow event beyond max_events")
	}
}

func TestGCRAEventsAfterExpiry(t *testing.T) {
	initTime()

	maxEvents := 5
	window := 10 * time.Second
	g := newGCRARateLimiter(maxEvents, window)

	// Fill up
	for i := 0; i < maxEvents; i++ {
		g.When()
	}

	// Advance past the window — should allow events again
	advanceTime(11)

	for i := 0; i < maxEvents; i++ {
		if when := g.When(); when != 0 {
			t.Fatalf("event %d should be allowed after expiry, got wait %v", i, when)
		}
	}
}

func TestGCRAPartialRecovery(t *testing.T) {
	initTime()

	maxEvents := 10
	window := 10 * time.Second
	g := newGCRARateLimiter(maxEvents, window)

	// Fill up completely
	for i := 0; i < maxEvents; i++ {
		g.When()
	}

	// Should be full
	if when := g.When(); when == 0 {
		t.Fatal("should be full after max_events")
	}

	// Advance by one emission interval (window/maxEvents = 1s)
	advanceTime(1)

	// Should now allow exactly one more event
	if when := g.When(); when != 0 {
		t.Fatal("should allow one event after one emission interval")
	}

	// Should be full again
	if when := g.When(); when == 0 {
		t.Fatal("should be full again after using the recovered slot")
	}
}

func TestGCRACount(t *testing.T) {
	initTime()

	maxEvents := 5
	window := 10 * time.Second
	g := newGCRARateLimiter(maxEvents, window)

	count, _ := g.Count(now())
	if count != 0 {
		t.Fatalf("expected count 0 for new limiter, got %d", count)
	}

	// Add 3 events
	for i := 0; i < 3; i++ {
		g.When()
	}

	count, _ = g.Count(now())
	if count != 3 {
		t.Fatalf("expected count 3, got %d", count)
	}

	// Advance past window
	advanceTime(11)

	count, _ = g.Count(now())
	if count != 0 {
		t.Fatalf("expected count 0 after window expiry, got %d", count)
	}
}

func TestGCRAZeroMaxEvents(t *testing.T) {
	initTime()

	g := newGCRARateLimiter(0, 10*time.Second)
	if when := g.When(); when == 0 {
		t.Fatal("zero max_events should reject all events")
	}
}

func TestGCRAZeroWindow(t *testing.T) {
	initTime()

	g := newGCRARateLimiter(10, 0)
	// Zero window means all events allowed
	for i := 0; i < 100; i++ {
		if when := g.When(); when != 0 {
			t.Fatalf("zero window should allow all events, got wait %v on event %d", when, i)
		}
	}
}

func TestGCRASetMaxEvents(t *testing.T) {
	initTime()

	g := newGCRARateLimiter(5, 10*time.Second)

	// Add 3 events (not full)
	for i := 0; i < 3; i++ {
		g.When()
	}

	// Should still allow events
	if when := g.When(); when != 0 {
		t.Fatal("should allow event 4")
	}

	// Reduce limit to 4 — we've used exactly 4 now, should be full
	g.SetMaxEvents(4)

	// After reducing, burst offset shrinks, so existing TAT may exceed new limit
	// The next event should be rejected
	if when := g.When(); when == 0 {
		t.Fatal("should be full after reducing max_events to match current usage")
	}

	// Increase limit back — should allow again
	g.SetMaxEvents(10)
	if when := g.When(); when != 0 {
		t.Fatal("should allow events after increasing max_events well above usage")
	}
}

func TestGCRARetryAfterValue(t *testing.T) {
	initTime()

	maxEvents := 5
	window := 10 * time.Second
	g := newGCRARateLimiter(maxEvents, window)

	// Fill up
	for i := 0; i < maxEvents; i++ {
		g.When()
	}

	// The wait time should be approximately one emission interval (2s)
	wait := g.When()
	if wait <= 0 {
		t.Fatal("wait should be positive when rate limited")
	}
	// Emission interval = 10s / 5 = 2s
	expectedWait := 2 * time.Second
	if wait != expectedWait {
		t.Fatalf("expected wait %v, got %v", expectedWait, wait)
	}
}

func TestGCRAReserveUnsynced(t *testing.T) {
	initTime()

	g := newGCRARateLimiter(10, 10*time.Second)

	mu := g.getLock()
	mu.Lock()
	g.reserveUnsynced()
	count, _ := g.countUnsynced(now())
	mu.Unlock()

	if count != 1 {
		t.Fatalf("expected count 1 after reserveUnsynced, got %d", count)
	}
}
