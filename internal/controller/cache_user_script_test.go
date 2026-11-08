/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"strings"
	"testing"
)

// An app's cache user reaches its own prefix and nothing else: the key pattern
// does not hold the commands that take no key, so they are refused by name.
func TestCacheUserIsHeldToItsPrefix(t *testing.T) {
	script := redisSetUserScript("demo-wiki", redisKeyPrefix("demo", "wiki"))
	for _, want := range []string{
		`reset on ">$USER_PW"`,
		`"~demo:wiki:*"`,
		`"&demo:wiki:*"`,
		`"&demo:wiki"`,
		"-@dangerous",
		"-randomkey",
		"-scan",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the script does not say %q:\n%s", want, script)
		}
	}
	for _, refused := range []string{"allkeys", "allchannels", "+@all"} {
		if strings.Contains(script, refused) {
			t.Errorf("the script grants %q:\n%s", refused, script)
		}
	}
}

// A user that is not saved is gone at the next restart of the server, and the
// script fails where the server cannot save, before it has changed anything.
func TestCacheUserIsSavedBeforeAndAfter(t *testing.T) {
	script := redisSetUserScript("demo-wiki", redisKeyPrefix("demo", "wiki"))
	set := strings.Index(script, "expect_ok ACL SETUSER")
	if first := strings.Index(script, "expect_ok ACL SAVE"); first < 0 || first > set {
		t.Errorf("no ACL SAVE before the user is changed:\n%s", script)
	}
	if last := strings.LastIndex(script, "expect_ok ACL SAVE"); last < set {
		t.Errorf("no ACL SAVE after the user is changed:\n%s", script)
	}
}
