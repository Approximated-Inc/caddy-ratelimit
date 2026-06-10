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
	"bytes"
	"fmt"
	"testing"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddytest"
)

func TestCaddyfileRateLimits(t *testing.T) {
	window := 60
	maxEvents := 10
	// Admin API must be exposed on port 2999 to match what caddytest.Tester does
	config := fmt.Sprintf(`
	{
		skip_install_trust
		admin localhost:2999
		http_port 8080
	}

	localhost:8080
	
	rate_limit {
		zone zone1 {
			match {
				method GET
			}
			key static
			window %ds
			events %d
		}
	}

	respond 200
	`, window, maxEvents)

	initTime()

	tester := caddytest.NewTester(t)
	tester.InitServer(config, "caddyfile")

	for i := 0; i < maxEvents; i++ {
		tester.AssertGetResponse("http://localhost:8080", 200, "")
	}

	assert429Response(t, tester, int64(window))
	tester.AssertPostResponseBody("http://localhost:8080", nil, &bytes.Buffer{}, 200, "")

	// After advancing time by half the window, the retry-after value should
	// change accordingly
	advanceTime(window / 2)

	assert429Response(t, tester, int64(window/2))

	// Advance time beyond the window where the events occurred. We should now
	// be able to make requests again.
	advanceTime(window)

	tester.AssertGetResponse("http://localhost:8080", 200, "")
}

// TestCaddyfileMaxKeysParsing verifies the max_keys zone subdirective parses
// and that it stays 0 (unset) at parse time when absent; the 100k default is
// applied later, at provision.
func TestCaddyfileMaxKeysParsing(t *testing.T) {
	d := caddyfile.NewTestDispenser(`rate_limit {
		zone capped {
			key static
			window 10s
			events 5
			max_keys 5000
		}
		zone uncapped {
			key static
			window 10s
			events 5
		}
	}`)

	var h Handler
	if err := h.UnmarshalCaddyfile(d); err != nil {
		t.Fatalf("unmarshaling caddyfile: %v", err)
	}
	if got := h.RateLimits["capped"].MaxKeys; got != 5000 {
		t.Fatalf("expected max_keys 5000, got %d", got)
	}
	if got := h.RateLimits["uncapped"].MaxKeys; got != 0 {
		t.Fatalf("expected max_keys 0 (unset) at parse time, got %d", got)
	}
}

// TestCaddyfileMaxKeysRejectsInvalid verifies non-integer and duplicate
// max_keys values are rejected.
func TestCaddyfileMaxKeysRejectsInvalid(t *testing.T) {
	for name, config := range map[string]string{
		"non_integer": `rate_limit {
			zone bad {
				key static
				window 10s
				events 5
				max_keys lots
			}
		}`,
		"missing_arg": `rate_limit {
			zone bad {
				key static
				window 10s
				events 5
				max_keys
			}
		}`,
		"duplicate": `rate_limit {
			zone bad {
				key static
				window 10s
				events 5
				max_keys 5000
				max_keys 6000
			}
		}`,
	} {
		t.Run(name, func(t *testing.T) {
			var h Handler
			if err := h.UnmarshalCaddyfile(caddyfile.NewTestDispenser(config)); err == nil {
				t.Fatal("expected an error, got none")
			}
		})
	}
}
