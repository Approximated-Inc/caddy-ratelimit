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
	"fmt"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
)

// RateLimit describes an HTTP rate limit zone.
type RateLimit struct {
	// Request matchers, which defines the class of requests that are in the RL zone.
	MatcherSetsRaw caddyhttp.RawMatcherSets `json:"match,omitempty" caddy:"namespace=http.matchers"`

	// The key which uniquely differentiates rate limits within this zone. It could
	// be a static string (no placeholders), resulting in one and only one rate limiter
	// for the whole zone. Or, placeholders could be used to dynamically allocate
	// rate limiters. For example, a key of "foo" will create exactly one rate limiter
	// for all clients. But a key of "{http.request.remote.host}" will create one rate
	// limiter for each different client IP address.
	Key string `json:"key,omitempty"`

	// Number of events allowed within the window.
	MaxEvents int `json:"max_events,omitempty"`

	// Duration of the sliding window.
	Window caddy.Duration `json:"window,omitempty"`

	// Algorithm selects the rate limiting algorithm for this zone.
	// Valid values: "ring_buffer" (default), "sliding_window", "gcra".
	//
	// "ring_buffer" uses a ring of timestamps (exact, O(max_events) memory per key).
	// "sliding_window" uses two fixed-window counters with interpolation (~56 bytes per key, ~1-2% approximation).
	// "gcra" uses the Generic Cell Rate Algorithm (~32 bytes per key, exact).
	Algorithm string `json:"algorithm,omitempty"`

	// MaxKeys bounds the number of distinct keys (i.e. individual rate
	// limiters) held in memory for this zone. When the zone is full,
	// expired entries are reclaimed first; if none are expired, a small
	// random batch is evicted so the new key is always admitted.
	// Defaults to 100000. This deliberately diverges from upstream's
	// unbounded maps: per-IP or per-host keys are otherwise a memory
	// exhaustion vector (rotating-IP floods, wildcard subdomains).
	MaxKeys int `json:"max_keys,omitempty"`

	matcherSets caddyhttp.MatcherSets

	zoneName string

	limitersMap *rateLimitersMap
}

func (rl *RateLimit) provision(ctx caddy.Context, name string) error {
	if rl.Window <= 0 {
		return fmt.Errorf("window must be greater than zero")
	}
	if rl.MaxEvents < 0 {
		return fmt.Errorf("max_events must be at least zero")
	}
	if rl.MaxKeys <= 0 {
		rl.MaxKeys = 100_000
	}

	switch rl.Algorithm {
	case "", "ring_buffer", "sliding_window", "gcra":
		// valid
	default:
		return fmt.Errorf("unknown algorithm %q; valid values are: ring_buffer, sliding_window, gcra", rl.Algorithm)
	}

	if len(rl.MatcherSetsRaw) > 0 {
		matcherSets, err := ctx.LoadModule(rl, "MatcherSetsRaw")
		if err != nil {
			return err
		}
		err = rl.matcherSets.FromInterface(matcherSets)
		if err != nil {
			return err
		}
	}

	// ensure rate limiter state endures across config changes
	rl.limitersMap = newRateLimiterMap(rl.Algorithm)
	if val, loaded := rateLimits.LoadOrStore(name, rl.limitersMap); loaded {
		rl.limitersMap = val.(*rateLimitersMap)
	}
	rl.limitersMap.updateAll(rl.MaxEvents, time.Duration(rl.Window))
	rl.limitersMap.configure(rl.MaxKeys, name, ctx.Logger())

	return nil
}

func (rl *RateLimit) permissiveness() float64 {
	return float64(rl.MaxEvents) / float64(rl.Window)
}

// newRateLimiter creates a new rate limiter using the specified algorithm.
func newRateLimiter(algorithm string, maxEvents int, window time.Duration) rateLimiter {
	switch algorithm {
	case "sliding_window":
		return newSlidingWindowRateLimiter(maxEvents, window)
	case "gcra":
		return newGCRARateLimiter(maxEvents, window)
	default: // "ring_buffer" or ""
		return newRingBufferRateLimiter(maxEvents, window)
	}
}

type rateLimitersMap struct {
	limiters   map[string]rateLimiter
	limitersMu sync.Mutex
	algorithm  string

	// cap on distinct keys (0 = unbounded; provision always sets it),
	// plus zone identity/logging for the at-cap warning — all guarded
	// by limitersMu
	maxKeys      int
	zoneName     string
	logger       *zap.Logger
	lastCapWarn  time.Time
	lastCapSweep time.Time
}

func newRateLimiterMap(algorithm string) *rateLimitersMap {
	return &rateLimitersMap{
		limiters:  make(map[string]rateLimiter),
		algorithm: algorithm,
	}
}

// configure sets the zone-level map settings. It runs at provision time,
// including on reloads when the map is reused from the usage pool.
func (rlm *rateLimitersMap) configure(maxKeys int, zoneName string, logger *zap.Logger) {
	rlm.limitersMu.Lock()
	defer rlm.limitersMu.Unlock()

	rlm.maxKeys = maxKeys
	rlm.zoneName = zoneName
	rlm.logger = logger
}

// getOrInsert returns an existing rate limiter from the map, or inserts a new
// one with the desired settings and returns it. If the zone is at maxKeys,
// room is made for the new key first.
func (rlm *rateLimitersMap) getOrInsert(key string, maxEvents int, window time.Duration) rateLimiter {
	rlm.limitersMu.Lock()
	defer rlm.limitersMu.Unlock()

	limiter, ok := rlm.limiters[key]
	if ok {
		return limiter
	}

	if rlm.maxKeys > 0 && len(rlm.limiters) >= rlm.maxKeys {
		rlm.makeRoom()
	}

	limiter = newRateLimiter(rlm.algorithm, maxEvents, window)
	rlm.limiters[key] = limiter
	return limiter
}

// capEvictBatch is how many entries makeRoom evicts when the sweep reclaims
// nothing, keeping collateral damage to live limiters small.
const capEvictBatch = 10

// capSweepInterval gates the full-zone expired-entry sweep in makeRoom: at
// most one O(n) sweep per interval per zone. Without the gate, a rotating-key
// flood at the cap triggers back-to-back full sweeps under limitersMu and
// every request in the zone serializes behind them.
const capSweepInterval = time.Second

// makeRoom frees space for a new key when the zone is at maxKeys: it sweeps
// this zone's expired entries (at most once per capSweepInterval), and if the
// map is still full it evicts a small random batch (map iteration order is
// effectively random). We never fail closed here — rejecting new keys would
// turn a memory-exhaustion attack into "deny all new visitors", and random
// eviction is about as good as LRU against rotating-key attackers, who get
// fresh buckets either way. The caller must hold limitersMu.
func (rlm *rateLimitersMap) makeRoom() {
	if t := now(); t.Sub(rlm.lastCapSweep) >= capSweepInterval {
		rlm.lastCapSweep = t
		rlm.sweepUnsynced()
	}

	if len(rlm.limiters) >= rlm.maxKeys {
		evicted := 0
		for key := range rlm.limiters {
			delete(rlm.limiters, key)
			evicted++
			if evicted >= capEvictBatch {
				break
			}
		}
	}

	if rlm.logger != nil && now().Sub(rlm.lastCapWarn) >= time.Minute {
		rlm.lastCapWarn = now()
		rlm.logger.Warn("rate limit zone hit max_keys; making room for new keys",
			zap.String("zone", rlm.zoneName),
			zap.Int("max_keys", rlm.maxKeys))
	}
}

// updateAll updates existing rate limiters with new settings.
func (rlm *rateLimitersMap) updateAll(maxEvents int, window time.Duration) {
	rlm.limitersMu.Lock()
	defer rlm.limitersMu.Unlock()

	for _, limiter := range rlm.limiters {
		limiter.SetMaxEvents(maxEvents)
		limiter.SetWindow(time.Duration(window))
	}
}

// sweep cleans up expired rate limit states.
func (rlm *rateLimitersMap) sweep() {
	rlm.limitersMu.Lock()
	defer rlm.limitersMu.Unlock()
	rlm.sweepUnsynced()
}

// sweepUnsynced removes expired rate limit states from this zone. The caller
// must hold limitersMu (makeRoom runs at the cap with the mutex already held,
// so calling the locking sweep() there would self-deadlock).
func (rlm *rateLimitersMap) sweepUnsynced() {
	for key, rl := range rlm.limiters {
		mu := rl.getLock()
		mu.Lock()

		// Use countUnsynced to check if any events are still in the window.
		// If count is 0, the limiter has expired and can be removed.
		// NOTE: we must NOT call MaxEvents()/Window() here as they
		// acquire the same mutex we already hold via getLock().
		count, _ := rl.countUnsynced(now())

		if count == 0 {
			delete(rlm.limiters, key)
		}

		mu.Unlock()
	}
}

// rlStateForZone returns the state of all rate limiters in the map.
func (rlm *rateLimitersMap) rlStateForZone(timestamp time.Time) map[string]rlStateValue {
	state := make(map[string]rlStateValue)

	rlm.limitersMu.Lock()
	defer rlm.limitersMu.Unlock()
	for key, rl := range rlm.limiters {
		count, oldestEvent := rl.Count(timestamp)
		state[key] = rlStateValue{
			Count:       count,
			OldestEvent: oldestEvent,
		}
	}

	return state
}
