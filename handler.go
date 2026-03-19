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
	"context"
	"encoding/json"
	"fmt"
	weakrand "math/rand"
	"net"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyevents"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/caddyserver/certmagic"
	"go.uber.org/zap"
)

func init() {
	caddy.RegisterModule(Handler{})
}

// Handler implements rate limiting functionality.
//
// If a rate limit is exceeded, an HTTP error with status 429 will be
// returned. This error can be handled using the conventional error
// handling routes in your config. An additional placeholder is made
// available, called `{http.rate_limit.exceeded.name}`, which you can
// use for logging or handling; it contains the name of the rate limit
// zone which limit was exceeded.
type Handler struct {
	// RateLimits contains the definitions of the rate limit zones, keyed by name.
	// The name **MUST** be globally unique across all other instances of this handler.
	RateLimits map[string]*RateLimit `json:"rate_limits,omitempty"`

	// Percentage jitter on expiration times (example: 0.2 means 20% jitter)
	Jitter float64 `json:"jitter,omitempty"`

	// How often to scan for expired rate limit states. Default: 1m.
	SweepInterval caddy.Duration `json:"sweep_interval,omitempty"`

	// Enables distributed rate limiting. For this to work properly, rate limit
	// zones must have the same configuration for all instances in the cluster
	// because an instance's own configuration is used to calculate whether a
	// rate limit is exceeded. As usual, a cluster is defined to be all instances
	// sharing the same storage configuration.
	Distributed *DistributedRateLimiting `json:"distributed,omitempty"`

	// Storage backend through which rate limit state is synced. If not set,
	// the global or default storage configuration will be used.
	StorageRaw json.RawMessage `json:"storage,omitempty" caddy:"namespace=caddy.storage inline_key=module"`

	// LogKey, if true, will log the key used for rate limiting.
	// Defaults to `false` because keys can contain sensitive information.
	LogKey bool `json:"log_key,omitempty"`

	rateLimits []*RateLimit
	storage    certmagic.Storage
	random     *weakrand.Rand
	logger     *zap.Logger
	ctx        caddy.Context
	events     *caddyevents.App
}

// CaddyModule returns the Caddy module information.
func (Handler) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.rate_limit",
		New: func() caddy.Module { return new(Handler) },
	}
}

// Provision sets up the handler.
func (h *Handler) Provision(ctx caddy.Context) error {
	h.ctx = ctx
	h.logger = ctx.Logger(h)

	eventsAppIface, err := ctx.App("events")
	if err != nil {
		return fmt.Errorf("getting events app: %v", err)
	}
	h.events = eventsAppIface.(*caddyevents.App)

	// Only initialize storage when distributed rate limiting is configured,
	// since storage is only used for cross-instance state sync.
	if h.Distributed != nil {
		if len(h.StorageRaw) > 0 {
			val, err := ctx.LoadModule(h, "StorageRaw")
			if err != nil {
				return fmt.Errorf("loading storage module: %v", err)
			}
			stor, err := val.(caddy.StorageConverter).CertMagicStorage()
			if err != nil {
				return fmt.Errorf("creating storage value: %v", err)
			}
			h.storage = stor
		} else {
			h.storage = ctx.Storage()
		}

		// TODO: maybe choose defaults intelligently based on window durations?
		if h.Distributed.ReadInterval == 0 {
			h.Distributed.ReadInterval = caddy.Duration(5 * time.Second)
		}
		if h.Distributed.WriteInterval == 0 {
			h.Distributed.WriteInterval = caddy.Duration(5 * time.Second)
		}

		iid, err := caddy.InstanceID()
		if err != nil {
			return err
		}
		h.Distributed.instanceID = iid.String()

		// gather distributed RL states right away so we can properly adjust
		// our rate limiting decisions to account for other instances
		err = h.syncDistributedRead(ctx)
		if err != nil {
			h.logger.Error("gathering initial rate limiter states", zap.Error(err))
		}

		// keep RL state synced
		go h.syncDistributed(ctx)
	}

	// provision each rate limit and put them in a slice so we can sort them
	for name, rl := range h.RateLimits {
		rl.zoneName = name
		err := rl.provision(ctx, name)
		if err != nil {
			return fmt.Errorf("setting up rate limit %s: %v", name, err)
		}
		h.rateLimits = append(h.rateLimits, rl)
	}

	// sort by tightest rate limit to most permissive (issue #10)
	sort.Slice(h.rateLimits, func(i, j int) bool {
		return h.rateLimits[i].permissiveness() > h.rateLimits[j].permissiveness()
	})

	if h.Jitter < 0 {
		return fmt.Errorf("jitter must be at least zero")
	} else if h.Jitter > 0 {
		h.random = weakrand.New(weakrand.NewSource(now().UnixNano()))
	}

	// Use a single global sweep goroutine shared across all handler instances,
	// since sweep iterates the global rateLimits pool anyway.
	if h.SweepInterval == 0 {
		h.SweepInterval = caddy.Duration(1 * time.Minute)
	}
	startGlobalSweep(ctx, time.Duration(h.SweepInterval))

	return nil
}

func (h Handler) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	repl := r.Context().Value(caddy.ReplacerCtxKey).(*caddy.Replacer)

	// iterate the slice, not the map, so the order is deterministic
	for _, rl := range h.rateLimits {
		// ignore rate limit if request doesn't qualify
		if !rl.matcherSets.AnyMatch(r) {
			continue
		}

		// make key for the individual rate limiter in this zone
		key := repl.ReplaceAll(rl.Key, "")
		limiter := rl.limitersMap.getOrInsert(key, rl.MaxEvents, time.Duration(rl.Window))

		if h.Distributed == nil {
			// internal rate limiter only
			if dur := limiter.When(); dur > 0 {
				return h.rateLimitExceeded(w, r, repl, rl.zoneName, key, dur)
			}
		} else {
			// distributed rate limiting; add last known state of other instances
			if err := h.distributedRateLimiting(w, r, repl, limiter, key, rl.zoneName); err != nil {
				return err
			}
		}
	}

	return next.ServeHTTP(w, r)
}

func (h *Handler) rateLimitExceeded(w http.ResponseWriter, r *http.Request, repl *caddy.Replacer, zoneName string, key string, wait time.Duration) error {
	// add jitter, if configured
	if h.random != nil {
		jitter := h.randomFloatInRange(0, float64(wait)*h.Jitter)
		wait += time.Duration(jitter)
	}

	// add 0.5 to ceil() instead of round() which FormatFloat() does automatically
	w.Header().Set("Retry-After", strconv.FormatFloat(wait.Seconds()+0.5, 'f', 0, 64))

	// emit log about exceeding rate limit (see #37)
	remoteIP, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		remoteIP = r.RemoteAddr // assume there was no port, I guess
	}

	// Create logger with common fields
	logger := h.logger.With(
		zap.String("zone", zoneName),
		zap.Duration("wait", wait),
		zap.String("remote_ip", remoteIP),
	)

	// Conditionally add the key field
	if h.LogKey {
		logger = logger.With(zap.String("key", key))
	}

	// Log the rate limit exceeded message
	logger.Info("rate limit exceeded")

	// also emit event so user can configure custom responses to rate limit violations
	h.events.Emit(h.ctx, "rate_limit_exceeded", map[string]any{
		"zone":      zoneName,
		"wait":      wait,
		"remote_ip": remoteIP,
	})

	// make some information about this rate limit available
	repl.Set("http.rate_limit.exceeded.name", zoneName)

	return caddyhttp.Error(http.StatusTooManyRequests, nil)
}

// Cleanup cleans up the handler.
func (h *Handler) Cleanup() error {
	// remove unused rate limit zones
	for name := range h.RateLimits {
		rateLimits.Delete(name)
	}
	return nil
}

func (h Handler) randomFloatInRange(min, max float64) float64 {
	if h.random == nil {
		return 0
	}
	return min + h.random.Float64()*(max-min)
}

// startGlobalSweep ensures a single sweep goroutine runs for the given context,
// shared across all handler instances. This avoids spawning redundant goroutines
// when many rate_limit handlers exist (e.g., one per virtual host).
func startGlobalSweep(ctx context.Context, interval time.Duration) {
	globalSweepMu.Lock()
	defer globalSweepMu.Unlock()

	// If a sweep is already running for this context, don't start another
	if globalSweepCancel != nil {
		return
	}

	sweepCtx, cancel := context.WithCancel(ctx)
	globalSweepCancel = cancel

	go func() {
		cleanerTicker := time.NewTicker(interval)
		defer cleanerTicker.Stop()
		defer func() {
			globalSweepMu.Lock()
			globalSweepCancel = nil
			globalSweepMu.Unlock()
		}()

		for {
			select {
			case <-cleanerTicker.C:
				rateLimits.Range(func(key, value interface{}) bool {
					value.(*rateLimitersMap).sweep()
					return true
				})

			case <-sweepCtx.Done():
				return
			}
		}
	}()
}

var (
	globalSweepMu     sync.Mutex
	globalSweepCancel context.CancelFunc
)

// rateLimits persists RL zones through config changes.
var rateLimits = caddy.NewUsagePool()

// Interface guards
var (
	_ caddy.Provisioner           = (*Handler)(nil)
	_ caddy.CleanerUpper          = (*Handler)(nil)
	_ caddyhttp.MiddlewareHandler = (*Handler)(nil)
)
