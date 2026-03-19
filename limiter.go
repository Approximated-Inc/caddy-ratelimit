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
	"sync"
	"time"
)

// rateLimiter is the interface that all rate limiting algorithm
// implementations must satisfy.
type rateLimiter interface {
	// When returns 0 if the event is allowed (and makes a reservation),
	// or the duration to wait before the next allowable event.
	When() time.Duration

	// MaxEvents returns the configured maximum number of events.
	MaxEvents() int

	// SetMaxEvents changes the maximum number of events allowed in the window.
	SetMaxEvents(n int)

	// Window returns the sliding window duration.
	Window() time.Duration

	// SetWindow changes the sliding window duration.
	SetWindow(d time.Duration)

	// Count returns the number of events in the window from the reference
	// time, and the oldest event timestamp (zero value if no events).
	Count(ref time.Time) (int, time.Time)

	// countUnsynced is the lock-free version of Count, called when the
	// caller already holds the mutex (used by distributed rate limiting).
	countUnsynced(ref time.Time) (int, time.Time)

	// reserveUnsynced makes a reservation without acquiring the lock.
	// The caller must already hold the lock via getLock().
	reserveUnsynced()

	// getLock returns the mutex for external locking (used by distributed.go).
	getLock() *sync.Mutex
}
