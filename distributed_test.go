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
	"context"
	"fmt"
	"os"
	"path"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddytest"
	"github.com/caddyserver/certmagic"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

func ensureAppDataDir(t *testing.T) {
	// Make sure AppDataDir exists, because otherwise the caddytest.Tester won't
	// be able to generate an instance ID
	if err := os.MkdirAll(caddy.AppDataDir(), 0700); err != nil {
		t.Fatalf("failed to create app data dir %s: %s", caddy.AppDataDir(), err)
	}
}

func TestDistributed(t *testing.T) {
	initTime()
	window := 60
	maxEvents := 10

	ensureAppDataDir(t)

	testCases := []struct {
		name               string
		peerRequests       int
		peerStateTimeStamp time.Time
		localRequests      int
		rateLimited        bool
	}{
		// Request should be refused because a peer used up the rate limit
		{
			name:               "peer-usage-in-window",
			peerRequests:       maxEvents,
			peerStateTimeStamp: now(),
			localRequests:      0,
			rateLimited:        true,
		},
		// Request should be allowed because while lots of requests are in the
		// peer state, the timestamp is outside the window
		{
			name:               "peer-usage-before-window",
			peerStateTimeStamp: now().Add(-time.Duration(window + 1)),
			localRequests:      0,
			rateLimited:        false,
		},
		// Request should be refused because local usage exceeds rate limit
		{
			name:               "local-usage",
			peerRequests:       0,
			peerStateTimeStamp: now(),
			localRequests:      maxEvents,
			rateLimited:        true,
		},
		// Request should be refused because usage in peer and locally sum up to
		// exceed rate limit
		{
			name:               "both-usage",
			peerRequests:       maxEvents / 2,
			peerStateTimeStamp: now(),
			localRequests:      maxEvents / 2,
			rateLimited:        true,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			storageDir := t.TempDir()
			// Use a random UUID as the zone so that rate limits from multiple test runs
			// don't collide with each other
			zone := uuid.New().String()

			// To simulate a peer in a rate limiting cluster, constuct a
			// ringBufferRateLimiter, record a bunch of events in it, and then sync that
			// state to storage.
			parsedDuration, err := time.ParseDuration(fmt.Sprintf("%ds", window))
			if err != nil {
				t.Fatal("failed to parse duration")
			}
			simulatedPeer := newRingBufferRateLimiter(maxEvents, parsedDuration)

			for i := 0; i < testCase.peerRequests; i++ {
				if when := simulatedPeer.When(); when != 0 {
					t.Fatalf("event should be allowed")
				}
			}

			zoneLimiters := newRateLimiterMap("")
			zoneLimiters.limiters["static"] = simulatedPeer

			rlState := rlState{
				Timestamp: testCase.peerStateTimeStamp,
				Zones: map[string]map[string]rlStateValue{
					zone: zoneLimiters.rlStateForZone(now()),
				},
			}

			storage := certmagic.FileStorage{
				Path: storageDir,
			}

			if err := writeRateLimitState(context.Background(), rlState, "f92a00f1-050c-4353-83b1-8ccc2337c25b", &storage); err != nil {
				t.Fatalf("failed to write state to storage: %s", err)
			}

			// For Windows, escape \ in storage path.
			storageDir = strings.ReplaceAll(storageDir, `\`, `\\`)

			// Run a caddytest.Tester that uses the same storage we just wrote to, so it
			// will treat the generated state as a peer to sync from.
			configString := `{
	"admin": {"listen": "localhost:2999"},
	"storage": {
		"module": "file_system",
		"root": "%s"
	},
	"logging": {
		"logs": {
			"default": {
				"level": "DEBUG"
			}
		}
	},
	"apps": {
		"http": {
			"servers": {
				"one": {
					"listen": [":8080"],
					"routes": [{
						"handle": [
							{
								"handler": "rate_limit",
								"rate_limits": {
									"%s": {
										"match": [{"method": ["GET"]}],
										"key": "static",
										"window": "%ds",
										"max_events": %d
									}
								},
								"distributed": {
									"write_interval": "3600s",
									"read_interval": "3600s",
									"purge_age": "7200s"
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
}`

			testerConfig := fmt.Sprintf(configString, storageDir, zone, window, maxEvents)
			tester := caddytest.NewTester(t)
			tester.InitServer(testerConfig, "json")

			for i := 0; i < testCase.localRequests; i++ {
				tester.AssertGetResponse("http://localhost:8080", 200, "")
			}

			if testCase.rateLimited {
				assert429Response(t, tester, int64(window))
			} else {
				tester.AssertGetResponse("http://localhost:8080", 200, "")
			}
		})
	}
}

func TestPurgeDistributedState(t *testing.T) {
	initTime()
	ensureAppDataDir(t)
	logger, err := zap.NewDevelopment()
	if err != nil {
		t.Fatalf("failed to create logger: %s", err)
	}

	storageDir := t.TempDir()
	storage := certmagic.FileStorage{
		Path: storageDir,
	}

	// Seed the storage directory with a rate limit state file from another instance.
	otherRlState := rlState{
		Timestamp: now(),
		Zones:     make(map[string]map[string]rlStateValue, 0),
	}
	if err := writeRateLimitState(context.Background(), otherRlState, "12345678-1234-1234-1234-123456789abc", &storage); err != nil {
		t.Fatalf("failed to write state to storage: %s", err)
	}

	handler := Handler{
		Distributed: &DistributedRateLimiting{
			instanceID: "99999999-9999-9999-9999-999999999999",
			PurgeAge:   caddy.Duration(time.Hour),
		},
		storage: &storage,
		logger:  logger,
	}

	// Perform initial read, and confirm it picks up the existing state file.
	err = handler.syncDistributedRead(context.Background())
	if err != nil {
		t.Fatalf("reading distributed state failed: %s", err)
	}
	if len(handler.Distributed.otherStates) != 1 {
		t.Fatalf("did not read other states correctly: %v", handler.Distributed.otherStates)
	}
	dirEntries, err := os.ReadDir(path.Join(storageDir, "rate_limit", "instances"))
	if err != nil {
		t.Fatalf("couldn't list directory: %s", err)
	}
	if len(dirEntries) != 1 {
		t.Fatalf("wrong number of files present in storage directory: %v", dirEntries)
	}

	// Advance time and sync again. The old state file should be deleted now.
	advanceTime(2 * 60 * 60)
	err = handler.syncDistributedRead(context.Background())
	if err != nil {
		t.Fatalf("reading distributed state failed: %s", err)
	}
	if len(handler.Distributed.otherStates) != 0 {
		t.Fatalf("expected other state to be deleted: %v", handler.Distributed.otherStates)
	}
	dirEntries, err = os.ReadDir(path.Join(storageDir, "rate_limit", "instances"))
	if err != nil {
		t.Fatalf("couldn't list directory: %s", err)
	}
	if len(dirEntries) != 0 {
		t.Fatalf("storage directory was not empty: %v", dirEntries)
	}
}

// orphanIndexStorage wraps a Storage and reports one extra key from List that
// Load will never find. This reproduces the production condition seen with the
// Redis storage backend, where List enumerates a sorted-set directory index and
// the value key can disappear (eviction, or a Del that bypassed Delete) while
// its index entry survives.
type orphanIndexStorage struct {
	certmagic.Storage

	mu      sync.Mutex
	orphan  string
	deleted []string
}

func (s *orphanIndexStorage) List(ctx context.Context, dir string, recursive bool) ([]string, error) {
	keys, err := s.Storage.List(ctx, dir, recursive)
	if err != nil {
		return keys, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.orphan != "" {
		keys = append(keys, s.orphan)
	}
	return keys, nil
}

func (s *orphanIndexStorage) Delete(ctx context.Context, key string) error {
	s.mu.Lock()
	s.deleted = append(s.deleted, key)
	isOrphan := key == s.orphan
	if isOrphan {
		// Deleting the key drops the stale index entry, so List stops
		// reporting it — the behaviour a real backend gives us.
		s.orphan = ""
	}
	s.mu.Unlock()

	if isOrphan {
		return nil
	}
	return s.Storage.Delete(ctx, key)
}

func (s *orphanIndexStorage) deletedKeys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.deleted...)
}

// An index entry whose value is missing can never be reclaimed by the purge
// path (that requires a state that loaded AND decoded), so before the fix it
// was re-listed and re-logged at ERROR on every read interval, forever. It
// should be deleted on the first read instead, and must not disturb the states
// that do load.
func TestOrphanedDistributedStateEntryIsDeleted(t *testing.T) {
	initTime()
	ensureAppDataDir(t)
	logger, err := zap.NewDevelopment()
	if err != nil {
		t.Fatalf("failed to create logger: %s", err)
	}

	fileStorage := &certmagic.FileStorage{Path: t.TempDir()}

	// A healthy peer that must survive untouched.
	healthy := rlState{Timestamp: now(), Zones: make(map[string]map[string]rlStateValue, 0)}
	if err := writeRateLimitState(context.Background(), healthy, "12345678-1234-1234-1234-123456789abc", fileStorage); err != nil {
		t.Fatalf("failed to write state to storage: %s", err)
	}

	orphanKey := path.Join(storagePrefix, "b137b85f-b981-44a9-b2ad-d8f672322182.rlstate")
	storage := &orphanIndexStorage{Storage: fileStorage, orphan: orphanKey}

	handler := Handler{
		Distributed: &DistributedRateLimiting{
			instanceID: "99999999-9999-9999-9999-999999999999",
			PurgeAge:   caddy.Duration(time.Hour),
		},
		storage: storage,
		logger:  logger,
	}

	if err := handler.syncDistributedRead(context.Background()); err != nil {
		t.Fatalf("reading distributed state failed: %s", err)
	}

	// The loadable peer is still aggregated; the orphan is not.
	if len(handler.Distributed.otherStates) != 1 {
		t.Fatalf("expected exactly the healthy peer state, got: %v", handler.Distributed.otherStates)
	}

	deleted := storage.deletedKeys()
	if len(deleted) != 1 || deleted[0] != orphanKey {
		t.Fatalf("expected the orphaned entry %q to be deleted, deletes were: %v", orphanKey, deleted)
	}

	// Second pass: the orphan is gone from List, so nothing further is deleted
	// and the healthy peer is still read.
	if err := handler.syncDistributedRead(context.Background()); err != nil {
		t.Fatalf("second read failed: %s", err)
	}
	if len(handler.Distributed.otherStates) != 1 {
		t.Fatalf("healthy peer state lost on second read: %v", handler.Distributed.otherStates)
	}
	if got := storage.deletedKeys(); len(got) != 1 {
		t.Fatalf("orphan should only be deleted once, deletes were: %v", got)
	}
}
