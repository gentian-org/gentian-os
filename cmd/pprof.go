/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package main

import (
	"fmt"
	"net"
)

// loopbackOnly refuses a profiling address another pod could reach.
//
// Go's profiles show what the operator holds in memory and let a caller keep
// it busy, and the endpoint has no sign-in. Bound to the pod's loopback, only
// a process in the pod and an administrator's kubectl port-forward reach it;
// the NetworkPolicies are not asked to keep it closed. Empty is off.
func loopbackOnly(addr string) error {
	if addr == "" || addr == "0" {
		return nil
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%q is not host:port: %w", addr, err)
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("%q is not a loopback address; use 127.0.0.1:<port>", addr)
}
