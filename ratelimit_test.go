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
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
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

// TestSweepZeroMaxEventsRingBuffer verifies that sweep survives a ring_buffer
// limiter with max_events 0 (zero-length ring). Regression test: countUnsynced
// used to index the empty ring and panic, killing the sweeper goroutine and
// the whole process with it.
func TestSweepZeroMaxEventsRingBuffer(t *testing.T) {
	initTime()

	rlm := newRateLimiterMap("ring_buffer")
	rlm.getOrInsert("empty", 0, 10*time.Second)

	rlm.sweep()

	rlm.limitersMu.Lock()
	defer rlm.limitersMu.Unlock()

	if _, ok := rlm.limiters["empty"]; ok {
		t.Fatal("zero-max-events limiter should have been swept (count 0)")
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

// TestGetOrInsertCapAdmitsNewKey verifies that when a zone is at max_keys,
// a new distinct key is still admitted (never rejected) while the map stays
// at or under the cap.
func TestGetOrInsertCapAdmitsNewKey(t *testing.T) {
	for _, algo := range []string{"ring_buffer", "sliding_window", "gcra"} {
		t.Run("algorithm_"+algo, func(t *testing.T) {
			initTime()

			const maxKeys = 20
			rlm := newRateLimiterMap(algo)
			rlm.configure(maxKeys, "test_zone", zap.NewNop())

			// Fill to cap with live keys (each has an event in the window).
			for i := 0; i < maxKeys; i++ {
				rlm.getOrInsert(fmt.Sprintf("key%d", i), 10, 10*time.Second).When()
			}
			// One more distinct key pushes the zone over the cap.
			rlm.getOrInsert("newest", 10, 10*time.Second).When()

			rlm.limitersMu.Lock()
			defer rlm.limitersMu.Unlock()
			if len(rlm.limiters) > maxKeys {
				t.Fatalf("limiter map exceeded max_keys: %d > %d", len(rlm.limiters), maxKeys)
			}
			if _, ok := rlm.limiters["newest"]; !ok {
				t.Fatal("newest key should have been admitted, not rejected")
			}
		})
	}
}

// TestGetOrInsertCapSweepsExpiredFirst verifies that hitting the cap first
// reclaims expired keys before evicting any live ones.
func TestGetOrInsertCapSweepsExpiredFirst(t *testing.T) {
	initTime()

	const maxKeys = 10
	rlm := newRateLimiterMap("sliding_window")
	rlm.configure(maxKeys, "test_zone", zap.NewNop())

	// Two keys whose events will fully expire...
	rlm.getOrInsert("expired1", 10, 10*time.Second).When()
	rlm.getOrInsert("expired2", 10, 10*time.Second).When()

	// ...by moving past the window (2x covers sliding window approximation)
	advanceTime(21)

	// Fill the rest of the cap with live keys.
	for i := 0; i < maxKeys-2; i++ {
		rlm.getOrInsert(fmt.Sprintf("live%d", i), 10, 10*time.Second).When()
	}

	rlm.getOrInsert("newest", 10, 10*time.Second).When()

	rlm.limitersMu.Lock()
	defer rlm.limitersMu.Unlock()
	for _, key := range []string{"expired1", "expired2"} {
		if _, ok := rlm.limiters[key]; ok {
			t.Fatalf("%s should have been reclaimed by the at-cap sweep", key)
		}
	}
	for i := 0; i < maxKeys-2; i++ {
		key := fmt.Sprintf("live%d", i)
		if _, ok := rlm.limiters[key]; !ok {
			t.Fatalf("live key %s should not have been evicted while expired keys existed", key)
		}
	}
	if _, ok := rlm.limiters["newest"]; !ok {
		t.Fatal("newest key should have been admitted")
	}
}

// TestGetOrInsertAtCapExistingKeyNoEviction verifies that looking up an
// already-present key while the zone is at the cap evicts nothing.
func TestGetOrInsertAtCapExistingKeyNoEviction(t *testing.T) {
	initTime()

	const maxKeys = 5
	rlm := newRateLimiterMap("sliding_window")
	rlm.configure(maxKeys, "test_zone", zap.NewNop())

	for i := 0; i < maxKeys; i++ {
		rlm.getOrInsert(fmt.Sprintf("key%d", i), 10, 10*time.Second).When()
	}

	limiter := rlm.getOrInsert("key0", 10, 10*time.Second)

	count, _ := limiter.Count(now())
	if count != 1 {
		t.Fatalf("expected the existing limiter with 1 event, got count %d", count)
	}
	rlm.limitersMu.Lock()
	defer rlm.limitersMu.Unlock()
	if len(rlm.limiters) != maxKeys {
		t.Fatalf("existing-key lookup must not evict: want %d keys, got %d", maxKeys, len(rlm.limiters))
	}
	for i := 0; i < maxKeys; i++ {
		if _, ok := rlm.limiters[fmt.Sprintf("key%d", i)]; !ok {
			t.Fatalf("key%d went missing after existing-key lookup", i)
		}
	}
}

// TestCapConcurrentWithSweepAndGetOrInsert mirrors
// TestSweepConcurrentWithGetOrInsert with the max_keys cap engaged, so the
// at-cap sweep/evict path runs concurrently with sweep() and updateAll().
func TestCapConcurrentWithSweepAndGetOrInsert(t *testing.T) {
	initTime()

	const maxKeys = 10
	rlm := newRateLimiterMap("sliding_window")
	rlm.configure(maxKeys, "test_zone", zap.NewNop())

	// Pre-populate past the cap so eviction engages immediately.
	for i := 0; i < 20; i++ {
		rlm.getOrInsert("key_"+string(rune('a'+i)), 10, 10*time.Second).When()
	}

	var wg sync.WaitGroup
	done := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			rlm.sweep()
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			rlm.getOrInsert(fmt.Sprintf("churn%d", i), 10, 10*time.Second).When()
		}
	}()

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
		t.Fatal("concurrent sweep/getOrInsert/updateAll deadlocked with cap engaged")
	}

	rlm.limitersMu.Lock()
	defer rlm.limitersMu.Unlock()
	if len(rlm.limiters) > maxKeys {
		t.Fatalf("cap not enforced under concurrency: %d keys > %d", len(rlm.limiters), maxKeys)
	}
}

// TestCapWarnRateLimited verifies the at-cap warning logs at most once per
// minute per zone, no matter how many cap hits occur in that minute.
func TestCapWarnRateLimited(t *testing.T) {
	initTime()

	core, logs := observer.New(zap.WarnLevel)
	const maxKeys = 3
	rlm := newRateLimiterMap("sliding_window")
	rlm.configure(maxKeys, "warn_zone", zap.New(core))

	fill := func(prefix string) {
		for i := 0; i < maxKeys; i++ {
			rlm.getOrInsert(fmt.Sprintf("%s%d", prefix, i), 10, 60*time.Second).When()
		}
	}

	// Multiple cap hits within the same minute: only the first should warn.
	fill("a")
	rlm.getOrInsert("overA", 10, 60*time.Second).When() // cap hit (warns)
	fill("b")                                           // refilling past the cap hits it again (suppressed)
	if got := logs.FilterMessageSnippet("max_keys").Len(); got != 1 {
		t.Fatalf("expected exactly 1 warn within the same minute, got %d", got)
	}

	// After a minute passes, the next cap hit warns again.
	advanceTime(61)
	fill("c")
	if got := logs.FilterMessageSnippet("max_keys").Len(); got != 2 {
		t.Fatalf("expected a second warn after a minute elapsed, got %d", got)
	}
}

// TestCapSweepTimeGated verifies the at-cap sweep runs at most once per
// capSweepInterval per zone: a second cap hit inside the gate window skips
// the O(n) sweep (expired entries survive; the random-evict batch makes room
// instead), and a cap hit after the gate elapses sweeps them out. This keeps
// rotating-key floods at the cap from serializing the whole zone behind
// back-to-back full sweeps.
func TestCapSweepTimeGated(t *testing.T) {
	initTime()
	// Sub-second clock control: the gate is 1s, so expired entries that
	// appear between two in-gate cap hits need sub-second windows.
	setClock := func(offset time.Duration) {
		now = func() time.Time { return time.Unix(referenceTime, 0).Add(offset) }
	}

	const maxKeys = 30
	rlm := newRateLimiterMap("sliding_window")
	rlm.configure(maxKeys, "gate_zone", zap.NewNop())

	// Fill to cap: 15 keys that will expire quickly, 15 long-lived ones.
	for i := 0; i < 15; i++ {
		rlm.getOrInsert(fmt.Sprintf("preexp%d", i), 10, 100*time.Millisecond).When()
	}
	for i := 0; i < 15; i++ {
		rlm.getOrInsert(fmt.Sprintf("live%d", i), 10, time.Hour).When()
	}

	// Cap hit #1: zero-value lastCapSweep permits the sweep, which reclaims
	// the 15 expired keys, so no eviction is needed.
	setClock(300 * time.Millisecond)
	rlm.getOrInsert("over1", 10, time.Hour).When()
	rlm.limitersMu.Lock()
	if got := len(rlm.limiters); got != 16 {
		t.Fatalf("after first cap hit, expected sweep to reclaim 15 expired keys (16 left), got %d", got)
	}
	rlm.limitersMu.Unlock()

	// Refill to cap with 14 keys that expire within the gate window.
	for i := 0; i < 14; i++ {
		rlm.getOrInsert(fmt.Sprintf("exp%d", i), 10, 100*time.Millisecond).When()
	}

	// Cap hit #2, 300ms after the last sweep (inside the gate): the sweep
	// must be skipped, so the expired keys survive (eviction removes at most
	// capEvictBatch of the 30) and the random-evict batch fires instead.
	setClock(600 * time.Millisecond)
	rlm.getOrInsert("over2", 10, time.Hour).When()

	rlm.limitersMu.Lock()
	if got, want := len(rlm.limiters), maxKeys-capEvictBatch+1; got != want {
		t.Fatalf("in-gate cap hit should evict a batch, not sweep: want %d keys, got %d", want, got)
	}
	survivors := 0
	for i := 0; i < 14; i++ {
		if _, ok := rlm.limiters[fmt.Sprintf("exp%d", i)]; ok {
			survivors++
		}
	}
	rlm.limitersMu.Unlock()
	if survivors == 0 {
		t.Fatal("expired keys should survive an in-gate cap hit (sweep must be skipped)")
	}

	// Refill to cap, then advance past the gate: the next cap hit sweeps
	// again and reclaims the surviving expired keys.
	rlm.limitersMu.Lock()
	missing := maxKeys - len(rlm.limiters)
	rlm.limitersMu.Unlock()
	for i := 0; i < missing; i++ {
		rlm.getOrInsert(fmt.Sprintf("fill%d", i), 10, time.Hour).When()
	}

	setClock(2 * time.Second)
	rlm.getOrInsert("over3", 10, time.Hour).When()

	rlm.limitersMu.Lock()
	defer rlm.limitersMu.Unlock()
	for i := 0; i < 14; i++ {
		key := fmt.Sprintf("exp%d", i)
		if _, ok := rlm.limiters[key]; ok {
			t.Fatalf("%s should have been reclaimed once the gate elapsed", key)
		}
	}
	if _, ok := rlm.limiters["over3"]; !ok {
		t.Fatal("over3 should have been admitted")
	}
	if got, want := len(rlm.limiters), maxKeys-survivors+1; got != want {
		t.Fatalf("post-gate cap hit should sweep (no eviction): want %d keys, got %d", want, got)
	}
}

// TestProvisionDefaultsMaxKeys verifies that provision applies the 100k
// default when max_keys is unset, and threads the cap into the zone's
// limiter map (also when an explicit value is configured).
func TestProvisionDefaultsMaxKeys(t *testing.T) {
	tests := []struct {
		name     string
		maxKeys  int
		expected int
	}{
		{"default_when_unset", 0, 100_000},
		{"default_when_negative", -5, 100_000},
		{"explicit_value_kept", 42, 42},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rl := &RateLimit{
				Window:    caddy.Duration(10 * time.Second),
				MaxEvents: 5,
				MaxKeys:   tc.maxKeys,
			}
			zoneName := "provision_max_keys_" + tc.name
			if err := rl.provision(caddy.Context{}, zoneName); err != nil {
				t.Fatalf("provision: %v", err)
			}
			defer rateLimits.Delete(zoneName)

			if rl.MaxKeys != tc.expected {
				t.Fatalf("expected MaxKeys %d after provision, got %d", tc.expected, rl.MaxKeys)
			}
			rl.limitersMap.limitersMu.Lock()
			defer rl.limitersMap.limitersMu.Unlock()
			if rl.limitersMap.maxKeys != tc.expected {
				t.Fatalf("expected limiter map cap %d, got %d", tc.expected, rl.limitersMap.maxKeys)
			}
		})
	}
}

// BenchmarkGetOrInsertAtCap measures inserting new keys into a zone pinned at
// the cap with 100k live keys, where every makeRoom sweep is a full O(n) scan
// that reclaims nothing (worst case), amortized over the evicted batch.
func BenchmarkGetOrInsertAtCap(b *testing.B) {
	initTime()

	const maxKeys = 100_000
	rlm := newRateLimiterMap("sliding_window")
	rlm.configure(maxKeys, "bench_zone", zap.NewNop())

	// 1h window keeps every seeded key live so the sweep reclaims nothing.
	for i := 0; i < maxKeys; i++ {
		rlm.getOrInsert(strconv.Itoa(i), 10, time.Hour).When()
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rlm.getOrInsert("new"+strconv.Itoa(i), 10, time.Hour).When()
	}
}
