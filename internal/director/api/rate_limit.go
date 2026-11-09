/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package api

import (
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/time/rate"

	"github.com/gentian-org/gentian-os/internal/director/authn"
)

// Each caller of the API is held to a number of requests a second, with a
// burst on top: a person by the account their token names, a component that
// commissions partners by its key. The count is in this process, so with more
// than one director replica a caller is allowed the number in each.
//
// The limit is asked once the caller is known and before anything else is
// -- the authorization store, git, the operator -- so a caller over it costs
// a token check and nothing more. A request without a caller is refused as
// unauthenticated before it is counted.
const (
	// DefaultCallerRequestsPerSecond is how many requests one caller may make
	// a second, on average.
	DefaultCallerRequestsPerSecond = 10
	// DefaultCallerRequestBurst is how many one caller may make at once: a
	// console page that asks for everything it shows together.
	DefaultCallerRequestBurst = 100
)

// CallerLimit is the limit each caller is held to. The zero value is no
// limit.
type CallerLimit struct {
	PerSecond int
	Burst     int
}

// CallerLimitFromEnv is the limit in force: the defaults above, or what the
// director's environment names (DIRECTOR_REQUESTS_PER_SECOND,
// DIRECTOR_REQUEST_BURST). DIRECTOR_REQUESTS_PER_SECOND=0 turns it off. A
// burst below the rate is raised to it.
func CallerLimitFromEnv() CallerLimit {
	l := CallerLimit{
		PerSecond: envCount("DIRECTOR_REQUESTS_PER_SECOND", DefaultCallerRequestsPerSecond),
		Burst:     envCount("DIRECTOR_REQUEST_BURST", DefaultCallerRequestBurst),
	}
	if l.PerSecond == 0 {
		return CallerLimit{}
	}
	l.Burst = max(l.Burst, l.PerSecond)
	return l
}

func envCount(key string, def int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 || n > 1000000 {
		return def
	}
	return n
}

// maxIdleCallers is how many callers are remembered before those whose
// allowance is whole again are forgotten: forgetting one of those changes
// nothing, since a new bucket starts whole.
const maxIdleCallers = 10000

// callerLimits holds a bucket for each caller. A nil one admits everybody.
type callerLimits struct {
	limit    CallerLimit
	mu       sync.Mutex
	byCaller map[string]*rate.Limiter
}

func newCallerLimits(l CallerLimit) *callerLimits {
	if l.PerSecond <= 0 {
		return nil
	}
	return &callerLimits{limit: CallerLimit{PerSecond: l.PerSecond, Burst: max(l.Burst, l.PerSecond)}, byCaller: map[string]*rate.Limiter{}}
}

// allow takes one request from caller's allowance, or says there is none.
func (c *callerLimits) allow(caller string) bool {
	if c == nil {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	l, ok := c.byCaller[caller]
	if !ok {
		if len(c.byCaller) >= maxIdleCallers {
			for k, v := range c.byCaller {
				if v.Tokens() >= float64(c.limit.Burst) {
					delete(c.byCaller, k)
				}
			}
		}
		l = rate.NewLimiter(rate.Limit(c.limit.PerSecond), c.limit.Burst)
		c.byCaller[caller] = l
	}
	return l.Allow()
}

// personCaller is the caller a token names: its account, in the realm that
// issued it.
func personCaller(id *authn.Identity) string {
	return "user:" + id.Realm + "/" + id.Subject
}

// refuseOverLimit answers 429 when caller has no allowance left, and says
// whether it did.
func (s *Server) refuseOverLimit(w http.ResponseWriter, r *http.Request, caller string) bool {
	if s.limits.allow(caller) {
		return false
	}
	w.Header().Set("Retry-After", "1")
	s.fail(w, r, http.StatusTooManyRequests, "too many requests")
	return true
}

// limitPerson is the guard's Limit for this director.
func (s *Server) limitPerson(id *authn.Identity) bool {
	return s.limits.allow(personCaller(id))
}
