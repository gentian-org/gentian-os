/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package addresses

// PerimeterRate is what one client address is held to on a published entry:
// requests a second, how many more at once before the publishing proxy
// answers 429, and how many it works on at a time.
type PerimeterRate struct {
	PerSecond, Burst, Concurrent int
}

// PerimeterRateFor is the rate the platform ships with. The cluster's
// administrator may set another in the operator's environment, and never a
// looser one for an entry that passes a credential than for every other.
//
// passCredential asks for an entry that passes the caller's Authorization
// header to the app (authMode app). Every request to one may be a guess at a
// password that lands on the app's own sign-in, so it is held to a quarter
// of the rate of an entry that passes nothing on.
//
// Here, and not with the proxy's configuration, because two programs read
// it: the operator renders the proxy from it, and the director tells an
// approver the limit that will apply.
func PerimeterRateFor(passCredential bool) PerimeterRate {
	if passCredential {
		return PerimeterRate{PerSecond: 5, Burst: 50, Concurrent: 20}
	}
	return PerimeterRate{PerSecond: 20, Burst: 200, Concurrent: 100}
}
