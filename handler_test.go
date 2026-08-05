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
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddytest"
)

const referenceTime = 1000000

func initTime() {
	now = func() time.Time {
		return time.Unix(referenceTime, 0)
	}
}

func advanceTime(seconds int) {
	now = func() time.Time {
		return time.Unix(referenceTime+int64(seconds), 0)
	}
}

func assert429Response(t *testing.T, tester *caddytest.Tester, expectedRetryAfter int64) {
	response, _ := tester.AssertGetResponse("http://localhost:8080", 429, "")

	retry_after := response.Header.Get("retry-after")
	if retry_after == "" {
		t.Fatal("429 response should have retry-after header")
	}

	retry_after_int, err := strconv.ParseInt(retry_after, 10, 64)
	if err != nil {
		t.Fatalf("could not parse retry-after header as integer: %+v", retry_after)
	}

	if retry_after_int != expectedRetryAfter {
		t.Fatalf("unexpected retry-after header value: %+v (wanted %d)", retry_after, expectedRetryAfter)
	}
}

func TestRateLimits(t *testing.T) {
	window := 60
	maxEvents := 10
	// Admin API must be exposed on port 2999 to match what caddytest.Tester does
	config := fmt.Sprintf(`{
	"admin": {"listen": "localhost:2999"},
	"apps": {
		"http": {
			"servers": {
				"demo": {
					"listen": [":8080"],
					"routes": [{
						"handle": [
							{
								"handler": "rate_limit",
								"rate_limits": {
									"zone1": {
										"match": [{"method": ["GET"]}],
										"key": "static",
										"window": "%ds",
										"max_events": %d
									}
								}
							},
							{
								"handler": "static_response",
								"status_code": 200
							}
						]
					}]
				}
			}
		}
	}
}`, window, maxEvents)

	initTime()

	tester := caddytest.NewTester(t)
	tester.InitServer(config, "json")

	for i := 0; i < maxEvents; i++ {
		tester.AssertGetResponse("http://localhost:8080", 200, "")
	}

	assert429Response(t, tester, int64(window))

	// After advancing time by half the window, the retry-after value should
	// change accordingly
	advanceTime(window / 2)

	assert429Response(t, tester, int64(window/2))

	// Advance time beyond the window where the events occurred. We should now
	// be able to make requests again.
	advanceTime(window)

	tester.AssertGetResponse("http://localhost:8080", 200, "")
}

func TestDistinctZonesAndKeys(t *testing.T) {
	maxEvents := 10
	// Admin API must be exposed on port 2999 to match what caddytest.Tester does
	config := fmt.Sprintf(`{
	"admin": {"listen": "localhost:2999"},
	"apps": {
		"http": {
			"servers": {
				"demo": {
					"listen": [":8080"],
					"routes": [{
						"handle": [
							{
								"handler": "rate_limit",
								"rate_limits": {
									"zone1": {
										"match": [{"method": ["GET"]}],
										"key": "{http.request.orig_uri.path}",
										"window": "60s",
										"max_events": %d
									},
									"zone2": {
										"match": [{"method": ["DELETE"]}],
										"key": "{http.request.orig_uri.path}",
										"window": "60s",
										"max_events": %d
									}
								}
							},
							{
								"handler": "static_response",
								"status_code": 200
							}
						]
					}]
				}
			}
		}
	}
}`, maxEvents, maxEvents)

	initTime()

	tester := caddytest.NewTester(t)
	tester.InitServer(config, "json")

	// Rate limits for different zones (by method) and keys (by request path)
	// should be accounted independently
	for i := 0; i < maxEvents; i++ {
		tester.AssertGetResponse("http://localhost:8080/path1", 200, "")
	}
	tester.AssertGetResponse("http://localhost:8080/path1", 429, "")

	for i := 0; i < maxEvents; i++ {
		tester.AssertGetResponse("http://localhost:8080/path2", 200, "")
	}
	tester.AssertGetResponse("http://localhost:8080/path2", 429, "")

	for i := 0; i < maxEvents; i++ {
		tester.AssertDeleteResponse("http://localhost:8080/path1", 200, "")
	}
	tester.AssertDeleteResponse("http://localhost:8080/path1", 429, "")

	for i := 0; i < maxEvents; i++ {
		tester.AssertDeleteResponse("http://localhost:8080/path2", 200, "")
	}
	tester.AssertDeleteResponse("http://localhost:8080/path2", 429, "")
}

// nonRateLimitConfig has no rate_limit handlers: loading it drops the last
// live handler reference on the shared sweeper.
const nonRateLimitConfig = `{
	"admin": {"listen": "localhost:2999"},
	"apps": {
		"http": {
			"servers": {
				"demo": {
					"listen": [":8080"],
					"routes": [{
						"handle": [{
							"handler": "static_response",
							"status_code": 200
						}]
					}]
				}
			}
		}
	}
}`

func sweepReloadConfig(maxEvents int) string {
	return fmt.Sprintf(`{
	"admin": {"listen": "localhost:2999"},
	"apps": {
		"http": {
			"servers": {
				"demo": {
					"listen": [":8080"],
					"routes": [{
						"handle": [
							{
								"handler": "rate_limit",
								"sweep_interval": "10ms",
								"rate_limits": {
									"reload_sweep_zone": {
										"match": [{"method": ["GET"]}],
										"key": "static",
										"window": "10s",
										"max_events": %d
									}
								}
							},
							{
								"handler": "static_response",
								"status_code": 200
							}
						]
					}]
				}
			}
		}
	}
}`, maxEvents)
}

func findPooledZone(t *testing.T, name string) *rateLimitersMap {
	t.Helper()
	var zone *rateLimitersMap
	rateLimits.Range(func(key, value any) bool {
		if key == name {
			zone = value.(*rateLimitersMap)
			return false
		}
		return true
	})
	if zone == nil {
		t.Fatalf("zone %s not found in the rateLimits pool", name)
	}
	return zone
}

// insertExpiredLimiter plants a limiter holding one event, then moves the time
// stub past the window — all under the zone lock the sweeper takes, so the
// time-stub write is synchronized with the sweeper's reads and the next sweep
// must evict the limiter.
func insertExpiredLimiter(zone *rateLimitersMap, key string, advanceToSeconds int) {
	zone.limitersMu.Lock()
	defer zone.limitersMu.Unlock()
	limiter := newRateLimiter("", 10, 10*time.Second)
	limiter.When() // record one event at the current stubbed time
	zone.limiters[key] = limiter
	advanceTime(advanceToSeconds)
}

func waitForSweep(t *testing.T, zone *rateLimitersMap, key, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		zone.limitersMu.Lock()
		_, present := zone.limiters[key]
		zone.limitersMu.Unlock()
		if !present {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(msg)
}

// TestSweeperSurvivesReload reproduces the production reload sequence: Caddy
// provisions a new config BEFORE cleaning up the old one. A sweeper bound to
// the first config's context dies on reload, leaving config epochs with no
// eviction at all. After a reload, an expired limiter must still get swept.
func TestSweeperSurvivesReload(t *testing.T) {
	initTime()

	tester := caddytest.NewTester(t)

	// Start from a config with no rate_limit handler so any sweeper held by
	// previously loaded test configs is torn down first.
	tester.InitServer(nonRateLimitConfig, "json")
	// Drop the sweeper again when this test finishes so its 10ms ticker
	// cannot race later tests that rewrite the global time stub.
	t.Cleanup(func() { tester.InitServer(nonRateLimitConfig, "json") })

	tester.InitServer(sweepReloadConfig(10), "json")

	// Reload twice (admin /load provisions new-then-stops-old) and verify
	// sweeping after each: the old ctx-bound sweeper died on alternating
	// epochs, so checking two consecutive reloads catches it regardless of
	// which epoch parity this test starts on.
	tester.InitServer(sweepReloadConfig(11), "json")
	zone := findPooledZone(t, "reload_sweep_zone")
	insertExpiredLimiter(zone, "stale-1", 60)
	waitForSweep(t, zone, "stale-1",
		"expired limiter not swept after first reload: sweeper did not survive the config swap")

	tester.InitServer(sweepReloadConfig(12), "json")
	zone = findPooledZone(t, "reload_sweep_zone")
	insertExpiredLimiter(zone, "stale-2", 120)
	waitForSweep(t, zone, "stale-2",
		"expired limiter not swept after second reload: sweeper did not survive the config swap")

	// exactly one live handler should hold the sweeper after the reloads
	if refs, _ := sweepers.References(sweeperPoolKey); refs != 1 {
		t.Fatalf("expected 1 sweeper reference after reloads, got %d", refs)
	}
}

func currentSweeper(t *testing.T) *globalSweeper {
	t.Helper()
	var s *globalSweeper
	sweepers.Range(func(key, value any) bool {
		if key == sweeperPoolKey {
			s = value.(*globalSweeper)
			return false
		}
		return true
	})
	if s == nil {
		t.Fatal("no sweeper found in the sweepers pool")
	}
	return s
}

// TestSweeperRefBalance verifies that Provision/Cleanup keep the UsagePool
// refcount balanced: a handler that never took a reference (its Provision
// failed before startSweeper) must not release one, and double Cleanup must
// release exactly once. Unbalanced Deletes panic inside caddy.UsagePool.
func TestSweeperRefBalance(t *testing.T) {
	baseRefs, _ := sweepers.References(sweeperPoolKey)

	// a handler whose Provision failed before startSweeper never set
	// sweeperRef, so Cleanup must not decrement anything
	failed := &Handler{}
	if err := failed.Cleanup(); err != nil {
		t.Fatalf("cleanup of unprovisioned handler: %v", err)
	}
	if refs, _ := sweepers.References(sweeperPoolKey); refs != baseRefs {
		t.Fatalf("cleanup of unprovisioned handler changed refs: %d -> %d", baseRefs, refs)
	}

	h := &Handler{SweepInterval: caddy.Duration(10 * time.Millisecond)}
	if err := h.startSweeper(); err != nil {
		t.Fatalf("startSweeper: %v", err)
	}
	if refs, _ := sweepers.References(sweeperPoolKey); refs != baseRefs+1 {
		t.Fatalf("expected %d refs after startSweeper, got %d", baseRefs+1, refs)
	}

	// double Cleanup releases the reference exactly once
	if err := h.Cleanup(); err != nil {
		t.Fatalf("first cleanup: %v", err)
	}
	if err := h.Cleanup(); err != nil {
		t.Fatalf("second cleanup: %v", err)
	}
	if refs, _ := sweepers.References(sweeperPoolKey); refs != baseRefs {
		t.Fatalf("expected %d refs after double cleanup, got %d", baseRefs, refs)
	}
}

// TestSweeperTeardownAndRecreate verifies that the sweep goroutine is stopped
// when the last handler releases its reference, and that a fresh sweeper is
// constructed on the next acquisition.
func TestSweeperTeardownAndRecreate(t *testing.T) {
	if refs, _ := sweepers.References(sweeperPoolKey); refs != 0 {
		t.Fatalf("test requires no live sweeper refs, found %d (another loaded config holds the sweeper)", refs)
	}

	h1 := &Handler{SweepInterval: caddy.Duration(10 * time.Millisecond)}
	if err := h1.startSweeper(); err != nil {
		t.Fatalf("startSweeper: %v", err)
	}
	s1 := currentSweeper(t)

	if err := h1.Cleanup(); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if refs, _ := sweepers.References(sweeperPoolKey); refs != 0 {
		t.Fatalf("expected 0 refs after last cleanup, got %d", refs)
	}
	select {
	case <-s1.stop:
		// closed: the run loop has been told to exit
	default:
		t.Fatal("sweeper stop channel not closed after last handler cleanup")
	}

	// the next acquisition must construct a NEW sweeper, not reuse the dead one
	h2 := &Handler{SweepInterval: caddy.Duration(10 * time.Millisecond)}
	if err := h2.startSweeper(); err != nil {
		t.Fatalf("startSweeper after teardown: %v", err)
	}
	defer func() { _ = h2.Cleanup() }()
	if s2 := currentSweeper(t); s2 == s1 {
		t.Fatal("expected a new sweeper after refs hit zero, got the destructed one")
	}
}
